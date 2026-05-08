// Package birdinfo proxies the public Ornithophile API
// (https://ornithophile.vercel.app) for bird-species lookups, normalizing
// the response (HTTPS image URLs, filtered "other_images" array) and
// caching results in memory so we don't hammer the upstream.
package birdinfo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const baseURL = "https://ornithophile.vercel.app/api/birds"

// Image is one named photo from the upstream response.
type Image struct {
	Name   string `json:"name,omitempty"`
	Source string `json:"source"`
}

// Info is the slimmed-down view of one bird record we hand to the UI.
type Info struct {
	CommonName         string  `json:"common_name"`
	ScientificName     string  `json:"scientific_name,omitempty"`
	Description        string  `json:"description,omitempty"`
	ConservationStatus string  `json:"conservation_status,omitempty"`
	Family             string  `json:"family,omitempty"`
	Genus              string  `json:"genus,omitempty"`
	Sound              string  `json:"sound,omitempty"`
	MaleImage          string  `json:"male_image,omitempty"`
	FemaleImage        string  `json:"female_image,omitempty"`
	OtherImages        []Image `json:"other_images,omitempty"`
	Source             string  `json:"source,omitempty"`
}

// Service caches Ornithophile lookups by lowercased common name.
type Service struct {
	client *http.Client

	mu    sync.RWMutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	at   time.Time
	info *Info // nil means "not found"
}

// New creates a service with a sensible 5-second HTTP timeout.
func New() *Service {
	return &Service{
		client: &http.Client{Timeout: 5 * time.Second},
		cache:  make(map[string]cacheEntry),
	}
}

const cacheTTL = 24 * time.Hour

// Lookup returns the species record matching the given common name (case-
// insensitive). Returns (nil, nil) when no match. Cached for 24 h.
func (s *Service) Lookup(ctx context.Context, name string) (*Info, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return nil, nil
	}
	s.mu.RLock()
	if e, ok := s.cache[key]; ok && time.Since(e.at) < cacheTTL {
		s.mu.RUnlock()
		return e.info, nil
	}
	s.mu.RUnlock()

	q := url.Values{}
	q.Set("common_name", name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("ornithophile %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var raw []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	var info *Info
	if len(raw) > 0 {
		info = normalize(raw[0])
	}
	s.mu.Lock()
	s.cache[key] = cacheEntry{at: time.Now(), info: info}
	s.mu.Unlock()
	return info, nil
}

// normalize converts the raw upstream JSON into our Info, filtering junk
// images and forcing https:// on protocol-relative URLs.
func normalize(raw map[string]any) *Info {
	getStr := func(k string) string {
		if v, ok := raw[k].(string); ok {
			return v
		}
		return ""
	}
	out := &Info{
		CommonName:         getStr("common_name"),
		ScientificName:     getStr("scientific_name"),
		Description:        getStr("description"),
		ConservationStatus: getStr("conservation_status"),
		Family:             getStr("family"),
		Genus:              getStr("genus"),
		Sound:              getStr("sound"),
		MaleImage:          fixURL(getStr("male_image")),
		FemaleImage:        fixURL(getStr("female_image")),
		Source:             getStr("sources"),
	}
	// other_images is a heterogeneous list; filter to plausible photos.
	if oi, ok := raw["other_images"].([]any); ok {
		for _, e := range oi {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			src, _ := m["source"].(string)
			if strings.HasPrefix(src, "//") {
				src = "https:" + src
			}
			// Apply the same junk filter to the upstream URL before we
			// rewrite — this catches SVGs and badge/icon images regardless
			// of host (and avoids spending fixURL cycles on them).
			if !looksLikePhoto(src) {
				continue
			}
			fixed := fixURL(src)
			if fixed == "" {
				continue
			}
			name, _ := m["name"].(string)
			// "Unnamed" is the upstream's stand-in for no caption.
			if strings.EqualFold(name, "Unnamed") {
				name = ""
			}
			out.OtherImages = append(out.OtherImages, Image{Name: name, Source: fixed})
		}
	}
	return out
}

// fixURL upgrades protocol-relative URLs and rewrites Wikimedia thumbnail
// hotlinks (which Wikimedia now rejects with a 403) to point through our
// own /api/bird-image proxy, which fetches the canonical full-resolution
// image, downscales it locally, and caches the result on disk.
//
// Returns an empty string when the URL points at something we can't
// reliably proxy (non-Wikimedia host, video/PDF original, etc.) so the
// caller can drop that entry rather than render a broken image.
func fixURL(u string) string {
	if u == "" {
		return ""
	}
	if strings.HasPrefix(u, "//") {
		u = "https:" + u
	}
	if !isWikimediaUpload(u) {
		// Anything not on upload.wikimedia.org we can't safely proxy
		// (auth, hotlink, or a wiki-page URL that's not even an image).
		return ""
	}
	canonical, ok := stripWikimediaThumb(u)
	if !ok {
		return ""
	}
	return "/api/bird-image?url=" + url.QueryEscape(canonical)
}

// isWikimediaUpload reports whether u points at upload.wikimedia.org (where
// all media files live).
func isWikimediaUpload(u string) bool {
	pu, err := url.Parse(u)
	if err != nil {
		return false
	}
	return strings.EqualFold(pu.Host, "upload.wikimedia.org")
}

// stripWikimediaThumb rewrites /wikipedia/<lang>/thumb/A/B/file.ext/Npx-file.ext
// back to /wikipedia/<lang>/A/B/file.ext (the canonical original-resolution
// path, which is just a static file and isn't subject to the thumbnailer's
// hotlink restrictions).
//
// Returns ok=false when the original-extension target isn't something we
// can decode + downscale — e.g. PDFs (`page1-Npx-File.pdf.jpg` thumbs of a
// .pdf original), animated GIFs, video stills (.webm.jpg of a .webm), etc.
// The caller should drop these.
func stripWikimediaThumb(u string) (string, bool) {
	const tag = "/thumb/"
	idx := strings.Index(u, tag)
	if idx < 0 {
		// Already a canonical (non-thumb) URL — accept as-is if its
		// extension looks like a raster image we can decode.
		return u, isRasterPath(u)
	}
	rest := u[idx+len(tag):]
	lastSlash := strings.LastIndex(rest, "/")
	if lastSlash < 0 {
		return u, false
	}
	originalRel := rest[:lastSlash] // A/B/Filename.ext (the actual source file)
	if !isRasterPath(originalRel) {
		return "", false
	}
	return u[:idx] + "/" + originalRel, true
}

// isRasterPath reports whether the path's extension is one our Go decoder
// can read (jpeg, png — gif/webp are intentionally excluded; gif decodes
// to a single frame and webp isn't in stdlib).
func isRasterPath(p string) bool {
	low := strings.ToLower(p)
	for _, ext := range []string{".jpg", ".jpeg", ".png"} {
		if strings.HasSuffix(low, ext) {
			return true
		}
	}
	return false
}

// looksLikePhoto rejects the icon/SVG/badge/map noise in other_images.
func looksLikePhoto(u string) bool {
	if u == "" {
		return false
	}
	low := strings.ToLower(u)
	if strings.HasSuffix(low, ".svg") || strings.Contains(low, ".svg/") {
		return false
	}
	for _, junk := range []string{
		"symbol_", "status_iucn", "/20px-", "/60px-",
		"_map.", "gnome-mime",
	} {
		if strings.Contains(low, junk) {
			return false
		}
	}
	return true
}
