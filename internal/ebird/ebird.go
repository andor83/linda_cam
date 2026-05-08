// Package ebird wraps the public eBird API
// (https://documenter.getpostman.com/view/664302/S1ENwy59) for the
// location-aware species filter: we ask "what's been observed near here
// recently?" and use the resulting set to drop classifier guesses for
// species that aren't on the local list.
//
// One Service handles fetching + caching. Has() does case-insensitive,
// punctuation-insensitive name matching so minor spelling differences
// between the eBird taxonomy and the bird-species classifier
// (e.g. hyphens, parentheticals) don't cause false negatives.
package ebird

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/linda/linda_cam/internal/config"
)

// Observation is one entry from the API. Most fields are unused here;
// we only need the common name to build the membership set.
type Observation struct {
	SpeciesCode string `json:"speciesCode"`
	ComName     string `json:"comName"`
	SciName     string `json:"sciName"`
}

type Service struct {
	client *http.Client
	cfg    config.EBirdConfig

	mu         sync.RWMutex
	species    map[string]struct{} // normalized common names
	commonOrig []string            // original-case names for diagnostics
	fetchedAt  time.Time
}

func New(cfg config.EBirdConfig) *Service {
	return &Service{
		client: &http.Client{Timeout: 15 * time.Second},
		cfg:    cfg,
		species: make(map[string]struct{}),
	}
}

// Active reports whether the filter should be applied to classifier
// output. Returns false when the user disabled it OR when we don't
// have a populated cache (e.g. last refresh failed).
func (s *Service) Active() bool {
	if !s.cfg.Enabled {
		return false
	}
	s.mu.RLock()
	n := len(s.species)
	s.mu.RUnlock()
	return n > 0
}

// Has reports whether the named species is in the recently-observed set.
// Both sides are normalized (lowercase, alphanumerics only).
func (s *Service) Has(name string) bool {
	key := normalize(name)
	if key == "" {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.species[key]
	return ok
}

// Stats is what the Settings UI's Test button gets back: number of
// species cached, when the cache was filled, and a sample of names.
type Stats struct {
	Count     int       `json:"count"`
	FetchedAt time.Time `json:"fetched_at,omitempty"`
	Sample    []string  `json:"sample,omitempty"`
}

func (s *Service) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sample := s.commonOrig
	if len(sample) > 12 {
		sample = sample[:12]
	}
	return Stats{
		Count:     len(s.species),
		FetchedAt: s.fetchedAt,
		Sample:    append([]string(nil), sample...),
	}
}

// Refresh hits the eBird API with the current configuration and
// rebuilds the cached set. Safe to call concurrently with reads.
func (s *Service) Refresh(ctx context.Context) error {
	endpoint, err := s.buildURL()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if s.cfg.APIKey == "" {
		return errors.New("eBird API key is empty")
	}
	req.Header.Set("X-eBirdApiToken", s.cfg.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("eBird %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var obs []Observation
	if err := json.Unmarshal(body, &obs); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	set := make(map[string]struct{}, len(obs))
	originals := make([]string, 0, len(obs))
	for _, o := range obs {
		key := normalize(o.ComName)
		if key == "" {
			continue
		}
		if _, dup := set[key]; dup {
			continue
		}
		set[key] = struct{}{}
		originals = append(originals, o.ComName)
	}
	s.mu.Lock()
	s.species = set
	s.commonOrig = originals
	s.fetchedAt = time.Now()
	s.mu.Unlock()
	return nil
}

// buildURL returns the appropriate eBird endpoint for the configured
// inputs: the geo-recent endpoint when Lat/Lng are populated, otherwise
// the region-recent endpoint when Region is set.
func (s *Service) buildURL() (string, error) {
	back := s.cfg.BackDays
	if back <= 0 {
		back = 30
	}
	if back > 30 {
		back = 30
	}
	useGeo := s.cfg.Lat != 0 || s.cfg.Lng != 0
	if useGeo {
		dist := s.cfg.DistKm
		if dist <= 0 {
			dist = 25
		}
		if dist > 50 {
			dist = 50
		}
		q := url.Values{}
		q.Set("lat", strconv.FormatFloat(s.cfg.Lat, 'f', 6, 64))
		q.Set("lng", strconv.FormatFloat(s.cfg.Lng, 'f', 6, 64))
		q.Set("dist", strconv.Itoa(dist))
		q.Set("back", strconv.Itoa(back))
		q.Set("fmt", "json")
		return "https://api.ebird.org/v2/data/obs/geo/recent?" + q.Encode(), nil
	}
	region := strings.TrimSpace(s.cfg.Region)
	if region == "" {
		return "", errors.New("eBird needs lat/lng or a region code")
	}
	q := url.Values{}
	q.Set("back", strconv.Itoa(back))
	q.Set("fmt", "json")
	return "https://api.ebird.org/v2/data/obs/" + url.PathEscape(region) + "/recent?" + q.Encode(), nil
}

// normalize folds case and punctuation so the eBird "Black-capped
// Chickadee" matches a classifier "Black Capped Chickadee" matches a
// user-typed "blackcapped chickadee".
func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			out = append(out, c)
		}
	}
	return string(out)
}
