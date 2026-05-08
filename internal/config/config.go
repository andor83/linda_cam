package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// WatchedAnimal pairs a class name with its own detection threshold so each
// species can be tuned independently.
type WatchedAnimal struct {
	Name      string  `json:"name"`
	Threshold float64 `json:"threshold"`
}

// WatchedAnimalsList accepts either the current object form
// ([{name, threshold}, ...]) or the legacy string-array form (["deer", ...])
// so configs from older versions migrate transparently on first load.
type WatchedAnimalsList []WatchedAnimal

func (w *WatchedAnimalsList) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		*w = nil
		return nil
	}
	var list []WatchedAnimal
	if err := json.Unmarshal(b, &list); err == nil {
		*w = list
		return nil
	}
	var names []string
	if err := json.Unmarshal(b, &names); err != nil {
		return err
	}
	out := make(WatchedAnimalsList, len(names))
	for i, n := range names {
		out[i] = WatchedAnimal{Name: n, Threshold: 0.35}
	}
	*w = out
	return nil
}

// CorrectionRule rewrites a classifier output name to a canonical one so
// the rest of the system (Ornithophile lookup, gallery display, metadata)
// uses a consistent label. Detected may be a literal string (matched
// case-insensitively) or a Go regex when Regex is true.
type CorrectionRule struct {
	Detected   string `json:"detected"`
	Correction string `json:"correction"`
	Regex      bool   `json:"regex,omitempty"`
}

// EBirdConfig configures the optional location-aware filter that drops
// species the classifier suggests but which haven't actually been
// observed near the user lately, per the eBird API.
type EBirdConfig struct {
	Enabled  bool    `json:"enabled,omitempty"`
	APIKey   string  `json:"api_key,omitempty"`   // eBird API token
	Region   string  `json:"region,omitempty"`    // e.g. "US-PA-101" (used when Lat/Lng are 0)
	Lat      float64 `json:"lat,omitempty"`
	Lng      float64 `json:"lng,omitempty"`
	DistKm   int     `json:"dist_km,omitempty"`   // geo radius; default 25, max 50
	BackDays int     `json:"back_days,omitempty"` // observation window; default 30, max 30
}

// AIQualityConfig configures the optional multimodal-AI image-quality
// scoring used to pick the winning frame at session close.
type AIQualityConfig struct {
	Enabled          bool   `json:"enabled,omitempty"`
	URL              string `json:"url,omitempty"`               // OpenAI-compatible /v1/chat/completions endpoint
	Model            string `json:"model,omitempty"`             // e.g. gpt-4o-mini, llava:latest
	BearerToken      string `json:"bearer_token,omitempty"`
	DiscardThreshold int    `json:"discard_threshold,omitempty"` // 0–100 — picture deleted when best score is below this
	NormalizeWidth   int    `json:"normalize_width,omitempty"`   // resize width before sending; default 1024
	MaxCandidates    int    `json:"max_candidates,omitempty"`    // top-N frames buffered per session; default 5
}

type Config struct {
	RTSPURL                 string             `json:"rtsp_url"`
	HTTPAddr                string             `json:"http_addr"`
	PasswordHash            string             `json:"password_hash"`
	SessionKey              string             `json:"session_key"`
	DetectionCooldownS      int                `json:"detection_cooldown_s"`
	SessionTimeoutS         int                `json:"session_timeout_s"`
	AutoCaptureEnabled      bool               `json:"auto_capture_enabled"`
	WatchedAnimals          WatchedAnimalsList `json:"watched_animals"`
	ClassifierCorrections   []CorrectionRule   `json:"classifier_corrections,omitempty"`
	AIQuality               AIQualityConfig    `json:"ai_quality,omitempty"`
	EBird                   EBirdConfig        `json:"ebird,omitempty"`
	BirdConfidenceThreshold float64            `json:"bird_confidence_threshold,omitempty"`
	BirdMaxCrops            int                `json:"bird_max_crops,omitempty"`
}

func defaults() Config {
	return Config{
		RTSPURL:            "",
		HTTPAddr:           ":8001",
		DetectionCooldownS: 5,
		SessionTimeoutS:    60,
		AutoCaptureEnabled: true,
		WatchedAnimals: WatchedAnimalsList{
			// "bird" intentionally absent — handled by the dedicated
			// bird pipeline (see BirdConfidenceThreshold below).
			{Name: "cat", Threshold: 0.35},
			{Name: "dog", Threshold: 0.35},
			{Name: "deer", Threshold: 0.35},
			{Name: "fox", Threshold: 0.35},
		},
		BirdConfidenceThreshold: 0.30,
		BirdMaxCrops:            3,
		AIQuality: AIQualityConfig{
			Enabled:          false,
			NormalizeWidth:   1024,
			MaxCandidates:    5,
			DiscardThreshold: 50,
		},
		EBird: EBirdConfig{
			Enabled:  false,
			DistKm:   25,
			BackDays: 30,
		},
	}
}

type Store struct {
	path string
	mu   sync.RWMutex
	cfg  Config
}

func New(path string) (*Store, error) {
	s := &Store{path: path, cfg: defaults()}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	c := defaults()
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	s.cfg = c
	return s, nil
}

func (s *Store) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *Store) FirstRun() bool {
	return s.Get().PasswordHash == ""
}

func (s *Store) Update(fn func(c *Config)) (Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.cfg)
	if err := s.writeLocked(); err != nil {
		return s.cfg, err
	}
	return s.cfg, nil
}

func (s *Store) writeLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
