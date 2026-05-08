package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	imgjpeg "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/draw"

	"github.com/go-chi/chi/v5"

	"github.com/linda/linda_cam/internal/aiquality"
	"github.com/linda/linda_cam/internal/auth"
	"github.com/linda/linda_cam/internal/birdinfo"
	"github.com/linda/linda_cam/internal/capture"
	"github.com/linda/linda_cam/internal/config"
	"github.com/linda/linda_cam/internal/correction"
	"github.com/linda/linda_cam/internal/detector"
	"github.com/linda/linda_cam/internal/detlog"
	"github.com/linda/linda_cam/internal/ebird"
	"github.com/linda/linda_cam/internal/jpeg"
	"github.com/linda/linda_cam/internal/rtsp"
	"github.com/linda/linda_cam/internal/sightings"
	"github.com/linda/linda_cam/internal/stats"
	"github.com/linda/linda_cam/internal/stream"
)

type Deps struct {
	Auth       *auth.Manager
	Config     *config.Store
	Captures   *capture.Store
	Sightings  *sightings.Store
	Extractor  *jpeg.Extractor
	RTSP       *rtsp.Client
	Streamer   *stream.Streamer
	Detector   *detector.Detector
	DetLog     *detlog.Logger
	BirdInfo   *birdinfo.Service
	FFmpegPath string
	HLSDir     string

	// OnConfigChange is invoked whenever the RTSP URL changes so main can
	// restart the RTSP client, JPEG extractor, and HLS streamer.
	OnConfigChange func(cfg config.Config)
}

type API struct {
	d Deps

	// Statistics tab cache. Compute() walks every metadata sidecar so
	// we don't want to do it on every refresh; 60 s TTL with eager
	// invalidation from any handler that mutates picture metadata.
	statsMu      sync.Mutex
	statsCache   stats.Bundle
	statsCacheAt time.Time
}

func New(d Deps) *API { return &API{d: d} }

func (a *API) Mount(r chi.Router) {
	// Public endpoints
	r.Get("/session", a.d.Auth.HandleSession)
	r.Post("/login", a.d.Auth.HandleLogin)
	r.Post("/first-run", a.d.Auth.HandleFirstRun)

	// Protected endpoints
	r.Group(func(pr chi.Router) {
		pr.Use(a.d.Auth.Middleware)
		pr.Post("/logout", a.d.Auth.HandleLogout)
		pr.Post("/change-password", a.d.Auth.HandleChangePassword)

		pr.Get("/config", a.getConfig)
		pr.Put("/config", a.putConfig)
		pr.Post("/apply-corrections", a.applyCorrections)
		pr.Post("/ai-quality/test", a.testAIQuality)
		pr.Post("/ebird/test", a.testEBird)
		pr.Post("/test-rtsp", a.testRTSP)

		pr.Post("/capture", a.capture)
		pr.Get("/pictures", a.listPictures)
		pr.Get("/pictures/{name}", a.getPicture)
		pr.Delete("/pictures/{name}", a.deletePicture)
		pr.Post("/pictures/{name}/reclassify", a.reclassifyPicture)
		pr.Post("/pictures/{name}/quality", a.scorePictureQuality)
		pr.Post("/pictures/{name}/heart", a.heartPicture)
		pr.Get("/pictures/{name}/metadata", a.getMetadata)
		pr.Put("/pictures/{name}/metadata", a.putMetadata)
		pr.Get("/pictures/{name}/crop", a.getPictureCrop)
		pr.Get("/pictures/{name}/crops/{index}", a.getPictureCropIndexed)

		pr.Get("/classes", a.listClasses)
		pr.Get("/detections", a.listDetections)
		pr.Get("/stats", a.getStats)
		pr.Get("/detect-debug", a.detectDebug)
		pr.Get("/bird-info", a.birdInfo)
		pr.Get("/bird-image", a.birdImage)

		pr.Get("/snapshot.jpg", a.snapshot)
		pr.Get("/status", a.status)

		pr.Get("/live/stream.m3u8", a.serveHLSPlaylist)
		pr.Get("/live/{segment:seg_\\d+\\.ts}", a.serveHLSSegment)
	})
}

func (a *API) serveHLSPlaylist(w http.ResponseWriter, r *http.Request) {
	p := filepath.Join(a.d.HLSDir, "stream.m3u8")
	if _, err := os.Stat(p); err != nil {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Retry-After", "2")
		http.Error(w, "stream not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, p)
}

func (a *API) serveHLSSegment(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "segment")
	p := filepath.Join(a.d.HLSDir, name)
	if filepath.Dir(p) != filepath.Clean(a.d.HLSDir) {
		http.Error(w, "invalid segment", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "public, max-age=10")
	http.ServeFile(w, r, p)
}

type configOut struct {
	RTSPURL                 string                  `json:"rtsp_url"`
	HTTPAddr                string                  `json:"http_addr"`
	DetectionCooldownS      int                     `json:"detection_cooldown_s"`
	SessionTimeoutS         int                     `json:"session_timeout_s"`
	AutoCaptureEnabled      bool                    `json:"auto_capture_enabled"`
	WatchedAnimals          []config.WatchedAnimal  `json:"watched_animals"`
	ClassifierCorrections   []config.CorrectionRule `json:"classifier_corrections"`
	AIQuality               config.AIQualityConfig  `json:"ai_quality"`
	EBird                   config.EBirdConfig      `json:"ebird"`
	BirdConfidenceThreshold float64                 `json:"bird_confidence_threshold"`
	BirdMaxCrops            int                     `json:"bird_max_crops"`
}

func toOut(c config.Config) configOut {
	animals := []config.WatchedAnimal(c.WatchedAnimals)
	if animals == nil {
		animals = []config.WatchedAnimal{}
	}
	corrections := c.ClassifierCorrections
	if corrections == nil {
		corrections = []config.CorrectionRule{}
	}
	return configOut{
		RTSPURL:                 c.RTSPURL,
		HTTPAddr:                c.HTTPAddr,
		DetectionCooldownS:      c.DetectionCooldownS,
		SessionTimeoutS:         c.SessionTimeoutS,
		AutoCaptureEnabled:      c.AutoCaptureEnabled,
		WatchedAnimals:          animals,
		ClassifierCorrections:   corrections,
		AIQuality:               c.AIQuality,
		EBird:                   c.EBird,
		BirdConfidenceThreshold: c.BirdConfidenceThreshold,
		BirdMaxCrops:            c.BirdMaxCrops,
	}
}

func (a *API) getConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, toOut(a.d.Config.Get()))
}

func (a *API) putConfig(w http.ResponseWriter, r *http.Request) {
	var in configOut
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if in.DetectionCooldownS < 1 {
		in.DetectionCooldownS = 1
	}
	if in.SessionTimeoutS < 5 {
		in.SessionTimeoutS = 5
	}
	if in.SessionTimeoutS > 600 {
		in.SessionTimeoutS = 600
	}
	for i := range in.WatchedAnimals {
		if in.WatchedAnimals[i].Threshold < 0.05 {
			in.WatchedAnimals[i].Threshold = 0.05
		}
		if in.WatchedAnimals[i].Threshold > 0.99 {
			in.WatchedAnimals[i].Threshold = 0.99
		}
	}
	if in.WatchedAnimals == nil {
		in.WatchedAnimals = []config.WatchedAnimal{}
	}
	// Trim + drop empty correction rows; validate any regex compiles.
	cleanedRules := make([]config.CorrectionRule, 0, len(in.ClassifierCorrections))
	for i, rule := range in.ClassifierCorrections {
		d := strings.TrimSpace(rule.Detected)
		c := strings.TrimSpace(rule.Correction)
		if d == "" || c == "" {
			continue
		}
		if rule.Regex {
			if _, err := regexp.Compile("(?i)" + d); err != nil {
				http.Error(w, fmt.Sprintf("rule %d: invalid regex %q: %v", i+1, d, err), http.StatusBadRequest)
				return
			}
		}
		cleanedRules = append(cleanedRules, config.CorrectionRule{
			Detected: d, Correction: c, Regex: rule.Regex,
		})
	}

	// Clamp + sanitize AI-quality knobs.
	ai := in.AIQuality
	ai.URL = strings.TrimSpace(ai.URL)
	ai.Model = strings.TrimSpace(ai.Model)
	ai.BearerToken = strings.TrimSpace(ai.BearerToken)
	if ai.NormalizeWidth < 64 {
		ai.NormalizeWidth = 64
	}
	if ai.NormalizeWidth > 4096 {
		ai.NormalizeWidth = 4096
	}
	if ai.MaxCandidates < 1 {
		ai.MaxCandidates = 1
	}
	if ai.MaxCandidates > 10 {
		ai.MaxCandidates = 10
	}
	if ai.DiscardThreshold < 0 {
		ai.DiscardThreshold = 0
	}
	if ai.DiscardThreshold > 100 {
		ai.DiscardThreshold = 100
	}

	// Clamp bird-pipeline knobs.
	birdConf := in.BirdConfidenceThreshold
	if birdConf < 0.05 {
		birdConf = 0.05
	}
	if birdConf > 0.95 {
		birdConf = 0.95
	}
	birdMax := in.BirdMaxCrops
	if birdMax < 1 {
		birdMax = 1
	}
	if birdMax > 10 {
		birdMax = 10
	}

	// Clamp + sanitize eBird knobs.
	eb := in.EBird
	eb.APIKey = strings.TrimSpace(eb.APIKey)
	eb.Region = strings.TrimSpace(eb.Region)
	if eb.DistKm < 1 {
		eb.DistKm = 1
	}
	if eb.DistKm > 50 {
		eb.DistKm = 50
	}
	if eb.BackDays < 1 {
		eb.BackDays = 1
	}
	if eb.BackDays > 30 {
		eb.BackDays = 30
	}

	newCfg, err := a.d.Config.Update(func(c *config.Config) {
		c.RTSPURL = in.RTSPURL
		c.DetectionCooldownS = in.DetectionCooldownS
		c.SessionTimeoutS = in.SessionTimeoutS
		c.AutoCaptureEnabled = in.AutoCaptureEnabled
		c.WatchedAnimals = config.WatchedAnimalsList(in.WatchedAnimals)
		c.ClassifierCorrections = cleanedRules
		c.AIQuality = ai
		c.EBird = eb
		c.BirdConfidenceThreshold = birdConf
		c.BirdMaxCrops = birdMax
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if a.d.OnConfigChange != nil {
		a.d.OnConfigChange(newCfg)
	}
	writeJSON(w, toOut(newCfg))
}

// applyCorrections walks every metadata row and rewrites its
// bird_species names through the configured correction rules — same
// logic the live classifier uses, but applied retroactively. When
// two pre-correction names collapse into one post-correction name,
// their confidences are summed and the merged list is renormalized.
//
// user_species (the user's per-picture override) is intentionally
// left alone — rules are about classifier output, not user-typed
// text.
func (a *API) applyCorrections(w http.ResponseWriter, r *http.Request) {
	rules := a.d.Config.Get().ClassifierCorrections
	regexes := correction.Compile(rules)

	names, err := a.d.Sightings.AllNames()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var processed, modified int
	var firstErr string
	for _, picture := range names {
		md, ok, err := a.d.Sightings.Get(picture)
		if err != nil || !ok {
			continue
		}
		processed++
		if len(md.BirdSpecies) == 0 || len(rules) == 0 {
			continue
		}

		// Apply + merge. Order of first occurrence is preserved before the
		// final descending-confidence sort so ties are stable.
		sums := make(map[string]float64, len(md.BirdSpecies))
		seen := []string{}
		for _, g := range md.BirdSpecies {
			cn := correction.Apply(g.Name, rules, regexes)
			if _, ok := sums[cn]; !ok {
				seen = append(seen, cn)
			}
			sums[cn] += g.Confidence
		}
		merged := make([]capture.SpeciesGuess, 0, len(seen))
		for _, n := range seen {
			merged = append(merged, capture.SpeciesGuess{Name: n, Confidence: sums[n]})
		}
		sort.Slice(merged, func(i, j int) bool { return merged[i].Confidence > merged[j].Confidence })
		// Renormalize to avoid totals >1 when collisions stack confidences.
		var total float64
		for _, g := range merged {
			total += g.Confidence
		}
		if total > 1 && total > 0 {
			for i := range merged {
				merged[i].Confidence /= total
			}
		}

		if !speciesEqual(md.BirdSpecies, merged) {
			md.BirdSpecies = merged
			if err := a.d.Captures.WriteMetadata(picture, md); err != nil {
				if firstErr == "" {
					firstErr = picture + ": " + err.Error()
				}
				continue
			}
			modified++
		}
	}

	resp := map[string]any{
		"processed": processed,
		"modified":  modified,
	}
	if firstErr != "" {
		resp["first_error"] = firstErr
	}
	if modified > 0 {
		a.invalidateStats()
	}
	writeJSON(w, resp)
}

// testAIQuality lets the Settings UI exercise the AI image-quality
// endpoint without saving config first. It accepts the form values in
// the body, fires a synthetic test image at the endpoint, and reports
// HTTP / JSON / response-shape failures with descriptive messages.
func (a *API) testAIQuality(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL         string `json:"url"`
		Model       string `json:"model"`
		BearerToken string `json:"bearer_token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cfg := config.AIQualityConfig{
		Enabled:        true,
		URL:            strings.TrimSpace(in.URL),
		Model:          strings.TrimSpace(in.Model),
		BearerToken:    strings.TrimSpace(in.BearerToken),
		NormalizeWidth: 256,
		MaxCandidates:  1,
	}
	if cfg.URL == "" || cfg.Model == "" {
		writeJSON(w, map[string]any{
			"ok":    false,
			"error": "URL and model are required",
		})
		return
	}
	svc := aiquality.New(cfg)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	result, err := svc.Test(ctx)
	resp := map[string]any{
		"http_status": result.HTTPStatus,
		"latency_ms":  result.LatencyMS,
	}
	if result.RawResponse != "" {
		resp["raw_response"] = result.RawResponse
	}
	if err != nil {
		resp["ok"] = false
		resp["error"] = err.Error()
	} else {
		resp["ok"] = true
		resp["score"] = result.Score
	}
	writeJSON(w, resp)
}

// testEBird lets the Settings UI exercise an eBird configuration without
// saving it first. It builds a Service from the form values, refreshes
// once, and reports a sample of the species set so the user can sanity-
// check the location/region they entered.
func (a *API) testEBird(w http.ResponseWriter, r *http.Request) {
	var in struct {
		APIKey   string  `json:"api_key"`
		Region   string  `json:"region"`
		Lat      float64 `json:"lat"`
		Lng      float64 `json:"lng"`
		DistKm   int     `json:"dist_km"`
		BackDays int     `json:"back_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cfg := config.EBirdConfig{
		Enabled:  true,
		APIKey:   strings.TrimSpace(in.APIKey),
		Region:   strings.TrimSpace(in.Region),
		Lat:      in.Lat,
		Lng:      in.Lng,
		DistKm:   in.DistKm,
		BackDays: in.BackDays,
	}
	if cfg.APIKey == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "API key is required"})
		return
	}
	if cfg.Lat == 0 && cfg.Lng == 0 && cfg.Region == "" {
		writeJSON(w, map[string]any{"ok": false, "error": "lat/lng or region required"})
		return
	}
	svc := ebird.New(cfg)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	start := time.Now()
	if err := svc.Refresh(ctx); err != nil {
		writeJSON(w, map[string]any{
			"ok":         false,
			"error":      err.Error(),
			"latency_ms": time.Since(start).Milliseconds(),
		})
		return
	}
	stats := svc.Stats()
	writeJSON(w, map[string]any{
		"ok":         true,
		"count":      stats.Count,
		"sample":     stats.Sample,
		"latency_ms": time.Since(start).Milliseconds(),
	})
}

func speciesEqual(a, b []capture.SpeciesGuess) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Confidence != b[i].Confidence {
			return false
		}
	}
	return true
}

type testReq struct {
	RTSPURL string `json:"rtsp_url"`
}

type testResp struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (a *API) testRTSP(w http.ResponseWriter, r *http.Request) {
	var req testReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	resp := testResp{OK: true}
	if err := jpeg.TestURL(a.d.FFmpegPath, req.RTSPURL, 8*time.Second); err != nil {
		resp.OK = false
		resp.Error = err.Error()
	}
	writeJSON(w, resp)
}

func (a *API) capture(w http.ResponseWriter, r *http.Request) {
	frame, _ := a.d.Extractor.Latest()
	name, err := a.d.Captures.Save(frame, capture.Reason{Manual: true})
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	// Best-effort: also analyze + attach detections / crop / species guesses
	// so the gallery modal has data to review. Failure here doesn't prevent
	// the capture from being reported successful.
	if _, err := a.d.Detector.ReanalyzeAndAttach(name, frame); err != nil {
		log.Printf("capture: analyze %s: %v", name, err)
	}
	a.invalidateStats()
	writeJSON(w, map[string]string{"name": name})
}

func (a *API) reclassifyPicture(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if _, err := a.d.Captures.Path(name); err != nil {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	mdPre, _, _ := a.d.Captures.ReadMetadata(name)
	if isBirdPicture(name, mdPre) {
		a.reclassifyBirdPicture(w, r, name)
		return
	}
	a.reclassifyNonBirdPicture(w, r, name)
}

// isBirdPicture decides which reclassify path a picture takes. A
// picture with any bird metadata (multi-crop, legacy crop, or species)
// goes through the new bird pipeline; everything else uses the old
// AnalyzeFrame path so deer/fox/cat/dog reclassifies still work.
func isBirdPicture(name string, md capture.Metadata) bool {
	if len(md.BirdCrops) > 0 {
		return true
	}
	if md.BirdCrop != "" || len(md.BirdSpecies) > 0 {
		return true
	}
	// Filename heuristic for unanalyzed pictures: the live tick names
	// non-bird captures with their species token; anything else is
	// either a manual capture or a bird from the new pipeline.
	for _, nonBird := range []string{"_cat_", "_dog_", "_deer_", "_fox_"} {
		if strings.Contains(name, nonBird) {
			return false
		}
	}
	if strings.Contains(name, "_bird_") {
		return true
	}
	return false
}

// reclassifyBirdPicture runs the new bird pipeline against an already-
// saved sighting: re-extract crops, classify, AI-quality-score, and
// replace the BirdCrops + crop files in place. Surfaces a
// quality_score=0 when no crop survives so the frontend can offer the
// usual "delete or keep" prompt.
//
// `?destructive=true` (used by the bulk "start fresh" loop) makes a
// no-bird result wipe existing bird metadata. The default per-picture
// invocation preserves the existing data on a model miss.
func (a *API) reclassifyBirdPicture(w http.ResponseWriter, r *http.Request, name string) {
	destructive := r.URL.Query().Get("destructive") == "true"
	res, err := a.d.Detector.ReclassifyBird(r.Context(), name, destructive)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	md, _, _ := a.d.Captures.ReadMetadata(name)
	resp := map[string]any{
		"detections":      md.Detections,
		"bird_species":    flatSpeciesFromCrops(md.BirdCrops),
		"reclassified_at": md.ReclassifiedAt,
		"quality_enabled": true,
		"has_crop":        len(md.BirdCrops) > 0,
		"crop_count":      len(md.BirdCrops),
		"quality_threshold": res.Threshold,
	}
	// Always emit a score: best surviving crop, or 0 when none survived.
	// Frontend's existing prompt logic treats `score < threshold` as
	// "offer to delete this".
	resp["quality_score"] = res.BestAIScore
	resp["all_scores"] = res.AllScores
	a.invalidateStats()
	writeJSON(w, resp)
}

// reclassifyNonBirdPicture handles fox/deer/cat/dog/person captures
// (and any other non-bird sighting). YOLO is re-run to refresh the
// detection bboxes so the modal overlay stays accurate, but the bird
// classifier and AI quality scorer are NOT invoked — both are bird-
// specific and produce noise on these frames (the AI quality prompt
// asks "is there a bird?" and would always return 0). Captures are
// kept for security / interest; we just record what's in the frame.
func (a *API) reclassifyNonBirdPicture(w http.ResponseWriter, r *http.Request, name string) {
	p, err := a.d.Captures.Path(name)
	if err != nil {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	body, err := os.ReadFile(p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	dets, err := a.d.Detector.ReanalyzeNonBird(name, body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	md, _, _ := a.d.Captures.ReadMetadata(name)
	resp := map[string]any{
		"detections":      dets,
		"reclassified_at": md.ReclassifiedAt,
		"quality_enabled": false,
		"has_crop":        false,
		"crop_count":      0,
		"non_bird":        true,
	}
	a.invalidateStats()
	writeJSON(w, resp)
}

// flatSpeciesFromCrops produces a single species list for the
// reclassify response from a multi-crop sighting — picks the highest-
// classifier-confidence species across all crops. Used for backwards
// compatibility with the response shape; the gallery's modal pulls
// per-crop species from the BirdCrops array directly.
func flatSpeciesFromCrops(crops []capture.BirdCropInfo) []capture.SpeciesGuess {
	if len(crops) == 0 {
		return nil
	}
	if len(crops[0].Species) == 0 {
		return nil
	}
	return crops[0].Species
}

// scorePictureQuality runs the AI image-quality service against a single
// already-saved picture. Persists the score to the metadata sidecar
// (so the gallery badge updates) and optionally auto-deletes the picture
// when below the configured discard threshold (controlled by the
// ?delete_below_threshold=true query param).
//
// Only pictures that have a bird-classifier crop (i.e. a bird was actually
// detected) are scored — the crop sub-image is what's sent to the AI so
// the model is judging the bird itself, not the whole 4K frame. Pictures
// without a crop respond with a clean "no bird crop" error so the batch
// UI can simply skip them.
func (a *API) scorePictureQuality(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	md, _, _ := a.d.Captures.ReadMetadata(name)

	// New multi-crop bird sighting → re-score every existing crop and
	// surface the best score.
	if len(md.BirdCrops) > 0 {
		scores, best, err := a.d.Detector.RescoreBirdCrops(r.Context(), name)
		resp := map[string]any{"enabled": true}
		cfg := a.d.Config.Get()
		resp["threshold"] = cfg.AIQuality.DiscardThreshold
		if err != nil {
			resp["error"] = err.Error()
			writeJSON(w, resp)
			return
		}
		resp["score"] = best
		resp["all_scores"] = scores
		if r.URL.Query().Get("delete_below_threshold") == "true" && best < cfg.AIQuality.DiscardThreshold {
			if err := a.d.Captures.Delete(name); err != nil {
				resp["delete_error"] = err.Error()
			} else {
				resp["deleted"] = true
			}
		}
		a.invalidateStats()
		writeJSON(w, resp)
		return
	}

	cropPath, cropErr := a.d.Captures.CropPath(name)
	if cropErr != nil {
		writeJSON(w, map[string]any{
			"enabled": true,
			"error":   "no bird crop available for this picture",
		})
		return
	}
	body, err := os.ReadFile(cropPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	species, conf := topSpeciesForQuality(nil, md)

	quality, qErr := a.d.Detector.QualityScore(r.Context(), body, species, conf)

	resp := map[string]any{"enabled": quality.Enabled}
	if !quality.Enabled {
		if qErr != nil {
			resp["error"] = qErr.Error()
		} else {
			resp["error"] = "AI quality scoring is disabled in settings"
		}
		writeJSON(w, resp)
		return
	}
	resp["threshold"] = quality.Threshold
	if qErr != nil {
		resp["error"] = qErr.Error()
		// Persist the failure so the gallery badge surfaces it.
		md.AIQualityError = qErr.Error()
		md.AIQualityRaw = quality.RawResponse
		if err := a.d.Captures.WriteMetadata(name, md); err != nil {
			log.Printf("scorePictureQuality: write ai error %s: %v", name, err)
		}
		writeJSON(w, resp)
		return
	}
	resp["score"] = quality.Score

	// Persist the score onto the picture's metadata row, clearing any
	// prior failure.
	now := time.Now()
	score := quality.Score
	md.AIQualityScore = &score
	md.AIQualityAt = &now
	md.AIQualityError = ""
	md.AIQualityRaw = quality.RawResponse
	if err := a.d.Captures.WriteMetadata(name, md); err != nil {
		log.Printf("scorePictureQuality: write metadata %s: %v", name, err)
	}

	// Optional auto-delete when the score is below the configured threshold.
	if r.URL.Query().Get("delete_below_threshold") == "true" && quality.Score < quality.Threshold {
		if err := a.d.Captures.Delete(name); err != nil {
			resp["delete_error"] = err.Error()
		} else {
			resp["deleted"] = true
		}
	}
	a.invalidateStats()
	writeJSON(w, resp)
}

// topSpeciesForQuality picks the species name + confidence to feed into
// the AI prompt. Prefers the (newly-saved) bird-classifier guess when
// present, falls back to the top YOLO detection, then to "animal".
func topSpeciesForQuality(dets []detector.Detection, md capture.Metadata) (string, float64) {
	if len(md.BirdSpecies) > 0 {
		return md.BirdSpecies[0].Name, md.BirdSpecies[0].Confidence
	}
	if len(dets) > 0 {
		return dets[0].Name, dets[0].Confidence
	}
	return "", 0
}

func (a *API) getMetadata(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if _, err := a.d.Captures.Path(name); err != nil {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	md, ok, err := a.d.Captures.ReadMetadata(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		// Empty metadata is a 200 with an empty object, not a 404 — the
		// modal handles "no analysis yet" by rendering the form blank.
		writeJSON(w, capture.Metadata{})
		return
	}
	writeJSON(w, md)
}

type metadataPatch struct {
	UserSpecies *string                    `json:"user_species,omitempty"`
	UserNotes   *string                    `json:"user_notes,omitempty"`
	BirdSpecies *[]capture.SpeciesGuess    `json:"bird_species,omitempty"`
	Detections  *[]capture.DetectionRecord `json:"detections,omitempty"`
	// CropUserSpecies sets per-crop human-confirmed species in a
	// multi-bird sighting. Keyed by crop index (string-keyed for JSON
	// portability). Each value replaces BirdCrops[index].UserSpecies;
	// other crop fields are untouched. Out-of-range indexes are
	// silently ignored.
	CropUserSpecies map[string]string `json:"crop_user_species,omitempty"`
}

func (a *API) putMetadata(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if _, err := a.d.Captures.Path(name); err != nil {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	var patch metadataPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	md, _, _ := a.d.Captures.ReadMetadata(name)
	if patch.UserSpecies != nil {
		md.UserSpecies = strings.TrimSpace(*patch.UserSpecies)
	}
	if patch.UserNotes != nil {
		md.UserNotes = *patch.UserNotes
	}
	if patch.BirdSpecies != nil {
		// Trim names; drop fully-empty entries so a deleted row doesn't leave junk.
		cleaned := make([]capture.SpeciesGuess, 0, len(*patch.BirdSpecies))
		for _, g := range *patch.BirdSpecies {
			n := strings.TrimSpace(g.Name)
			if n == "" {
				continue
			}
			cleaned = append(cleaned, capture.SpeciesGuess{Name: n, Confidence: g.Confidence})
		}
		md.BirdSpecies = cleaned
	}
	if patch.Detections != nil {
		cleaned := make([]capture.DetectionRecord, 0, len(*patch.Detections))
		for _, d := range *patch.Detections {
			n := strings.TrimSpace(d.Name)
			if n == "" {
				continue
			}
			cleaned = append(cleaned, capture.DetectionRecord{
				Name: n, Confidence: d.Confidence, Box: d.Box,
			})
		}
		md.Detections = cleaned
	}
	if patch.CropUserSpecies != nil {
		for k, v := range patch.CropUserSpecies {
			idx, err := strconv.Atoi(k)
			if err != nil {
				continue
			}
			if idx < 0 || idx >= len(md.BirdCrops) {
				continue
			}
			md.BirdCrops[idx].UserSpecies = strings.TrimSpace(v)
		}
	}
	if err := a.d.Captures.WriteMetadata(name, md); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.invalidateStats()
	writeJSON(w, md)
}

func (a *API) getPictureCrop(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	// Prefer indexed crop 0 when present (new bird pipeline). Fall
	// back to the legacy single-crop file. This keeps old client URLs
	// (`/pictures/<name>/crop`) working for both schema vintages.
	if cp, err := a.d.Captures.IndexedCropPath(name, 0); err == nil {
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		w.Header().Set("Content-Type", "image/jpeg")
		http.ServeFile(w, r, cp)
		return
	}
	cp, err := a.d.Captures.CropPath(name)
	if err != nil {
		http.Error(w, "no crop", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeFile(w, r, cp)
}

// getPictureCropIndexed serves the i-th crop sidecar for a sighting.
// Resolves the file by reading the metadata's BirdCrops[idx].Filename
// — that handles both the new `<name>.crop.<idx>.jpg` naming AND
// legacy single-crop rows whose synthesized BirdCrops entry points
// at `<name>.crop.jpg`. Falls back to the indexed name on disk if
// metadata is missing (defense in depth).
func (a *API) getPictureCropIndexed(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	idx, err := strconv.Atoi(chi.URLParam(r, "index"))
	if err != nil || idx < 0 || idx > 99 {
		http.Error(w, "bad index", http.StatusBadRequest)
		return
	}
	if md, ok, _ := a.d.Captures.ReadMetadata(name); ok {
		if idx < len(md.BirdCrops) && md.BirdCrops[idx].Filename != "" {
			cp := filepath.Join(a.d.Captures.Dir(), md.BirdCrops[idx].Filename)
			if _, err := os.Stat(cp); err == nil {
				w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
				w.Header().Set("Content-Type", "image/jpeg")
				http.ServeFile(w, r, cp)
				return
			}
		}
	}
	cp, err := a.d.Captures.IndexedCropPath(name, idx)
	if err != nil {
		http.Error(w, "no crop", http.StatusNotFound)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeFile(w, r, cp)
}

func (a *API) listPictures(w http.ResponseWriter, r *http.Request) {
	pics, err := a.d.Captures.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, pics)
}

func (a *API) getPicture(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	p, err := a.d.Captures.Path(name)
	if err != nil {
		http.Error(w, "invalid name", http.StatusBadRequest)
		return
	}
	if r.URL.Query().Get("thumb") == "1" {
		tp, err := a.d.Captures.ThumbnailPath(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		w.Header().Set("Content-Type", "image/jpeg")
		http.ServeFile(w, r, tp)
		return
	}
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", "attachment; filename=\""+filepath.Base(p)+"\"")
	}
	http.ServeFile(w, r, p)
}

func (a *API) deletePicture(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := a.d.Captures.Delete(name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.invalidateStats()
	w.WriteHeader(http.StatusNoContent)
}

// heartPicture toggles or sets the hearted flag on a picture.
// Hearted pictures are exempt from the 30-day retention sweep.
//
// Body: {"hearted": true|false}. Returns the new state.
func (a *API) heartPicture(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var in struct {
		Hearted *bool `json:"hearted"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var newState bool
	if in.Hearted == nil {
		// Toggle when not specified.
		cur, err := a.d.Captures.Hearted(name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		newState = !cur
	} else {
		newState = *in.Hearted
	}
	if err := a.d.Captures.SetHeart(name, newState); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"hearted": newState})
}

func (a *API) snapshot(w http.ResponseWriter, r *http.Request) {
	frame, ts := a.d.Extractor.Latest()
	if len(frame) == 0 {
		http.Error(w, "no frame available", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	if !ts.IsZero() {
		w.Header().Set("Last-Modified", ts.UTC().Format(http.TimeFormat))
	}
	_, _ = w.Write(frame)
}

type statusOut struct {
	RTSPConnected  bool                 `json:"rtsp_connected"`
	AnimalsPresent []detector.Detection `json:"animals_present"`
	LastCaptureAt  time.Time            `json:"last_capture_at"`
	DetectorReady  bool                 `json:"detector_ready"`
}

func (a *API) status(w http.ResponseWriter, r *http.Request) {
	st := a.d.Detector.Status()
	out := statusOut{
		RTSPConnected:  a.d.Streamer.Connected() || a.d.Extractor.Connected() || a.d.RTSP.Connected(),
		AnimalsPresent: st.Present,
		LastCaptureAt:  st.LastCaptureAt,
		DetectorReady:  st.DetectorReady,
	}
	if out.AnimalsPresent == nil {
		out.AnimalsPresent = []detector.Detection{}
	}
	writeJSON(w, out)
}

// birdImageUA identifies us per Wikimedia's user-agent policy; even though
// originals don't enforce it as strictly as the thumbnailer, a descriptive
// string is the polite thing to do.
const birdImageUA = "linda-cam/1.0 (https://github.com/linda/linda_cam; homelab bird gallery)"

// Per-request cap on what we'll fetch from Wikimedia. Originals can be
// 10–20 MB; we only need the data to downscale and toss.
const birdImageMaxBytes = 25 * 1024 * 1024

// Width to downscale fetched bird images to before caching. 640 px is
// plenty for the popover (~360px) and the modal gallery (~360–500px).
const birdImageThumbWidth = 640

var birdImageClient = &http.Client{Timeout: 30 * time.Second}

// birdImage fetches a Wikimedia upload URL, downscales it to a moderate
// width, caches the result on disk under pictures/.bird-cache/, and serves
// it. The proxy exists because Wikimedia's thumbnailer now rejects
// hotlinks with HTTP 403; the canonical /commons/A/B/Filename.ext path
// still works and we generate our own thumbnails locally.
func (a *API) birdImage(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("url")
	if raw == "" {
		http.Error(w, "missing url", http.StatusBadRequest)
		return
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}
	if !strings.EqualFold(parsed.Host, "upload.wikimedia.org") {
		http.Error(w, "host not allowed", http.StatusForbidden)
		return
	}

	hash := sha256.Sum256([]byte(raw))
	cacheName := hex.EncodeToString(hash[:]) + ".jpg"
	cacheDir := filepath.Join(a.d.Captures.Dir(), ".bird-cache")
	cachePath := filepath.Join(cacheDir, cacheName)

	if fi, err := os.Stat(cachePath); err == nil && fi.Size() > 0 {
		w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
		w.Header().Set("Content-Type", "image/jpeg")
		http.ServeFile(w, r, cachePath)
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, raw, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	req.Header.Set("User-Agent", birdImageUA)
	req.Header.Set("Accept", "image/*")

	resp, err := birdImageClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "upstream "+resp.Status, http.StatusBadGateway)
		return
	}

	src, _, err := image.Decode(io.LimitReader(resp.Body, birdImageMaxBytes))
	if err != nil {
		http.Error(w, "decode: "+err.Error(), http.StatusBadGateway)
		return
	}
	sb := src.Bounds()
	if sb.Dx() == 0 || sb.Dy() == 0 {
		http.Error(w, "empty image", http.StatusBadGateway)
		return
	}
	tw := birdImageThumbWidth
	if tw > sb.Dx() {
		tw = sb.Dx()
	}
	th := sb.Dy() * tw / sb.Dx()
	if th < 1 {
		th = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, tw, th))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, sb, draw.Over, nil)

	if err := os.MkdirAll(cacheDir, 0o755); err == nil {
		tmp := cachePath + ".tmp"
		if f, err := os.Create(tmp); err == nil {
			if encErr := imgjpeg.Encode(f, dst, &imgjpeg.Options{Quality: 85}); encErr == nil {
				_ = f.Close()
				_ = os.Rename(tmp, cachePath)
			} else {
				_ = f.Close()
				_ = os.Remove(tmp)
				log.Printf("bird-image: encode cache: %v", encErr)
			}
		}
	}

	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	w.Header().Set("Content-Type", "image/jpeg")
	if _, err := os.Stat(cachePath); err == nil {
		http.ServeFile(w, r, cachePath)
		return
	}
	// Cache write failed — encode straight to the response.
	_ = imgjpeg.Encode(w, dst, &imgjpeg.Options{Quality: 85})
}

func (a *API) birdInfo(w http.ResponseWriter, r *http.Request) {
	if a.d.BirdInfo == nil {
		http.Error(w, "bird info disabled", http.StatusServiceUnavailable)
		return
	}
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		http.Error(w, "missing name", http.StatusBadRequest)
		return
	}
	info, err := a.d.BirdInfo.Lookup(r.Context(), name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if info == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Server already caches upstream lookups in-memory for 24 h; tell the
	// browser not to keep its own JSON copy so URL-rewrite changes (or
	// future format tweaks) don't get pinned.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, info)
}

func (a *API) detectDebug(w http.ResponseWriter, r *http.Request) {
	threshold := float32(0.1)
	if v := r.URL.Query().Get("threshold"); v != "" {
		if f, err := strconv.ParseFloat(v, 32); err == nil {
			threshold = float32(f)
		}
	}
	topK := 15
	if v := r.URL.Query().Get("top"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			topK = n
		}
	}
	dets, err := a.d.Detector.DebugDetect(threshold, topK)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if dets == nil {
		dets = []detector.Detection{}
	}
	writeJSON(w, dets)
}

func (a *API) listClasses(w http.ResponseWriter, r *http.Request) {
	names := a.d.Detector.ClassNames()
	if names == nil {
		names = []string{}
	}
	writeJSON(w, names)
}

func (a *API) listDetections(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	var before int64
	if v := r.URL.Query().Get("before"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			before = n
		}
	}
	entries, err := a.d.DetLog.List(limit, before)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if entries == nil {
		entries = []detlog.Entry{}
	}
	writeJSON(w, entries)
}

// getStats returns the bundled metrics for the Statistics tab.
// 60 s TTL cache; mutating handlers call invalidateStats() to drop it.
func (a *API) getStats(w http.ResponseWriter, r *http.Request) {
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	if !a.statsCacheAt.IsZero() && time.Since(a.statsCacheAt) < 60*time.Second {
		writeJSON(w, a.statsCache)
		return
	}
	b, err := stats.Compute(a.d.Sightings, a.d.Captures.Dir())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	a.statsCache = b
	a.statsCacheAt = time.Now()
	writeJSON(w, b)
}

// invalidateStats drops the cached stats bundle so the next /stats
// request recomputes from disk. Safe to call from any goroutine.
func (a *API) invalidateStats() {
	a.statsMu.Lock()
	a.statsCacheAt = time.Time{}
	a.statsMu.Unlock()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
