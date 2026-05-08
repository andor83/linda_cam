// Package aiquality wraps an OpenAI-compatible /v1/chat/completions
// endpoint with vision support, sending up to N candidate JPEGs of a
// single sighting and parsing back a per-image quality score (0–100).
//
// Used by the detector at session close to pick the keeper frame and
// optionally discard the picture entirely if even the best frame
// scores below the user's threshold.
package aiquality

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/linda/linda_cam/internal/config"
)

// resultsSchema builds the strict json_schema response_format for a
// {"results":[{<key>:<bool>,<numKey>:<int 0-100>}, ...]} shape.
func resultsSchema(name, boolKey, numKey string) map[string]any {
	return map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   name,
			"strict": true,
			"schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"results": map[string]any{
						"type": "array",
						"items": map[string]any{
							"type": "object",
							"properties": map[string]any{
								boolKey: map[string]any{"type": "boolean"},
								numKey: map[string]any{
									"type":    "integer",
									"minimum": 0,
									"maximum": 100,
								},
							},
							"required":             []string{boolKey, numKey},
							"additionalProperties": false,
						},
					},
				},
				"required":             []string{"results"},
				"additionalProperties": false,
			},
		},
	}
}

var (
	scoresResponseFormat      = resultsSchema("scores", "bird_present", "score")
	validationsResponseFormat = resultsSchema("validations", "bird_present", "confidence")
)

// Score is one model verdict for one input image.
type Score struct {
	Index int `json:"index"`
	Value int `json:"value"` // 0–100, clamped
}

// Validation is the model's binary "is there a bird here?" judgment
// for one cropped image, plus a confidence (0–100).
type Validation struct {
	Index       int  `json:"index"`
	BirdPresent bool `json:"bird_present"`
	Confidence  int  `json:"confidence"`
}

// Service holds an HTTP client and a snapshot of the AI config.
type Service struct {
	client *http.Client
	cfg    config.AIQualityConfig
}

func New(cfg config.AIQualityConfig) *Service {
	return &Service{
		// Generous client-side ceiling. llama.cpp can take several
		// minutes for batched multi-image scoring, especially on cold
		// start (model loading, KV cache warm-up); per-call context
		// timeouts at the caller still apply.
		client: &http.Client{Timeout: 5 * time.Minute},
		cfg:    cfg,
	}
}

// ScoreResult bundles the parsed scores with the raw response payload
// so callers can persist what the model actually returned (handy for
// the Debug tab in the gallery modal).
type ScoreResult struct {
	Scores      []Score
	RawResponse string // the full content string from choices[0].message.content
}

// Score POSTs the request and returns one Score per input image. The
// returned slice always has len(images) entries; for any image the
// model failed to score, Value is 0 and Index matches input order.
//
// Kept for backward compatibility with callers that don't care about
// the raw response. New callers should prefer ScoreWithRaw.
func (s *Service) Score(
	ctx context.Context,
	species string,
	confidence float64,
	images [][]byte,
) ([]Score, error) {
	r, err := s.ScoreWithRaw(ctx, species, confidence, images)
	if err != nil {
		return nil, err
	}
	return r.Scores, nil
}

// ScoreWithRaw is Score plus the raw model response string.
func (s *Service) ScoreWithRaw(
	ctx context.Context,
	species string,
	confidence float64,
	images [][]byte,
) (ScoreResult, error) {
	if !s.cfg.Enabled {
		return ScoreResult{}, errors.New("ai quality scoring disabled")
	}
	if s.cfg.URL == "" || s.cfg.Model == "" {
		return ScoreResult{}, errors.New("ai quality URL or model missing")
	}
	if len(images) == 0 {
		return ScoreResult{}, nil
	}

	prompt := buildPrompt(species, confidence, len(images))
	content := []map[string]any{
		{"type": "text", "text": prompt},
	}
	for _, raw := range images {
		dataURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(raw)
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": dataURL},
		})
	}

	body := map[string]any{
		"model": s.cfg.Model,
		"messages": []map[string]any{
			{
				"role":    "system",
				"content": "You are an image-quality judge. Reply with JSON only, no commentary.",
			},
			{
				"role":    "user",
				"content": content,
			},
		},
		"temperature": 0,
		// 4096 leaves headroom in case the model's chat template
		// inserts a thinking phase before the JSON answer. With
		// thinking suppressed the JSON itself is well under 512.
		"max_tokens": 4096,
		// Strict JSON-schema constraint. LM Studio rejects the older
		// json_object form; recent llama.cpp accepts json_schema too.
		"response_format": scoresResponseFormat,
		// Suppress reasoning/thinking tokens for templates that
		// honor the flag (Qwen3, Gemma 4, etc.). Without this a
		// thinking model can burn the entire token budget on a
		// <think>…</think> block and return empty content.
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return ScoreResult{}, fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.URL, bytes.NewReader(buf))
	if err != nil {
		return ScoreResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.BearerToken)
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return ScoreResult{}, err
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode != http.StatusOK {
		return ScoreResult{}, fmt.Errorf("ai quality %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}

	var envelope struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBytes, &envelope); err != nil {
		return ScoreResult{}, fmt.Errorf("decode envelope: %w", err)
	}
	if len(envelope.Choices) == 0 {
		return ScoreResult{}, errors.New("no choices in response")
	}

	choice := envelope.Choices[0]
	rawContent := choice.Message.Content
	if rawContent == "" {
		// Empty content with finish_reason=length is the classic
		// thinking-model truncation: all 4096 tokens were spent on a
		// <think>…</think> block that llama.cpp split into
		// reasoning_content. Surface enough detail to diagnose.
		return ScoreResult{RawResponse: choice.Message.ReasoningContent},
			fmt.Errorf("empty content (finish_reason=%q, reasoning_content=%d bytes); raise llama-server -n / --n-predict, or disable thinking on the chat template",
				choice.FinishReason, len(choice.Message.ReasoningContent))
	}
	scores, err := parseScores(rawContent, len(images))
	if err != nil {
		return ScoreResult{RawResponse: rawContent}, err
	}
	return ScoreResult{Scores: scores, RawResponse: rawContent}, nil
}

// Validate runs a binary "does this image contain a bird?" judgment
// on each input. Used at session-finalize to re-pick the bird crop
// when the live YOLO+classifier path picked a leaf or branch as the
// highest-confidence "bird" detection.
//
// Always returns len(images) entries; on parse failure for any one,
// BirdPresent is false (conservative — we won't promote a crop the
// model couldn't classify).
func (s *Service) Validate(ctx context.Context, images [][]byte) ([]Validation, error) {
	if !s.cfg.Enabled {
		return nil, errors.New("ai quality scoring disabled")
	}
	if s.cfg.URL == "" || s.cfg.Model == "" {
		return nil, errors.New("ai quality URL or model missing")
	}
	if len(images) == 0 {
		return nil, nil
	}
	prompt := buildValidatePrompt(len(images))
	content := []map[string]any{{"type": "text", "text": prompt}}
	for _, raw := range images {
		dataURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(raw)
		content = append(content, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": dataURL},
		})
	}
	body := map[string]any{
		"model": s.cfg.Model,
		"messages": []map[string]any{
			{"role": "system", "content": "You identify birds in images. Reply with JSON only."},
			{"role": "user", "content": content},
		},
		"temperature":          0,
		"max_tokens":           4096,
		"response_format":      validationsResponseFormat,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.URL, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.BearerToken)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ai validate %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}
	var envelope struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBytes, &envelope); err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	if len(envelope.Choices) == 0 {
		return nil, errors.New("no choices in response")
	}
	choice := envelope.Choices[0]
	if choice.Message.Content == "" {
		return nil, fmt.Errorf("empty content (finish_reason=%q, reasoning_content=%d bytes); raise llama-server -n / --n-predict, or disable thinking on the chat template",
			choice.FinishReason, len(choice.Message.ReasoningContent))
	}
	return parseValidations(choice.Message.Content, len(images))
}

// buildValidatePrompt asks for binary bird-presence per image. Tighter
// than the quality prompt — no rubric, no quality scoring, just "is
// there a bird? how sure?".
func buildValidatePrompt(n int) string {
	return fmt.Sprintf(
		"You are looking at %d cropped images from a wildlife camera. For "+
			"EACH image, decide whether it actually contains a bird.\n"+
			"\n"+
			"  • bird_present = true ONLY if you can clearly see a real "+
			"bird (body, head, wing, tail, beak, or recognizable feathers). "+
			"Even a partial bird counts.\n"+
			"  • bird_present = false if the crop shows ONLY non-bird "+
			"content: leaves, branches, foliage, sky, an empty feeder or "+
			"perch, a fence, ground, a building, or a non-bird animal "+
			"(squirrel, cat, deer, etc.). The detector frequently false-"+
			"positives leaves and branches as birds — your job is to "+
			"catch those. Do not guess; if you don't actually see a bird, "+
			"return false.\n"+
			"  • confidence is an integer 0–100 representing how sure you "+
			"are about your bird_present judgment.\n"+
			"\n"+
			"Example output for [bird, leaf, bird]:\n"+
			"{\"results\":[{\"bird_present\":true,\"confidence\":95},"+
			"{\"bird_present\":false,\"confidence\":92},"+
			"{\"bird_present\":true,\"confidence\":80}]}\n"+
			"\n"+
			"Respond with JSON ONLY in this exact shape with %d entries "+
			"in input order: {\"results\":[{\"bird_present\":<bool>,"+
			"\"confidence\":<int>}, ...]}. No prose, no markdown fences.",
		n, n,
	)
}

// parseValidations parses the {"results":[...]} shape into one
// Validation per requested input.
func parseValidations(raw string, want int) ([]Validation, error) {
	s := strings.TrimSpace(raw)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSpace(s)
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
	}
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	if j := strings.LastIndex(s, "}"); j >= 0 && j < len(s)-1 {
		s = s[:j+1]
	}
	var parsed struct {
		Results []struct {
			BirdPresent *bool `json:"bird_present"`
			Confidence  int   `json:"confidence"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return nil, fmt.Errorf("parse validations from %q: %w", truncate(raw, 200), err)
	}
	out := make([]Validation, want)
	for i := 0; i < want; i++ {
		v := Validation{Index: i, BirdPresent: false, Confidence: 0}
		if i < len(parsed.Results) {
			r := parsed.Results[i]
			if r.BirdPresent != nil {
				v.BirdPresent = *r.BirdPresent
			}
			c := r.Confidence
			if c < 0 {
				c = 0
			}
			if c > 100 {
				c = 100
			}
			v.Confidence = c
		}
		out[i] = v
	}
	return out, nil
}

// buildPrompt produces the text block that goes alongside the images.
// The format is locked deliberately so parseScores can find the JSON.
//
// Strategy: hard caps, not ranges. Small VLMs (qwen2.5-vl-7b et al.)
// gravitate to 60–80 for every shot when given a fuzzy rubric, and
// silently ignore "MUST NOT exceed 40"-style sentences when they're
// buried in prose. Listing each failure mode with a single explicit
// numeric ceiling, in priority order, gets the model to actually
// apply them.
func buildPrompt(species string, confidence float64, n int) string {
	subject := "an animal"
	if species != "" {
		subject = species
	}
	return fmt.Sprintf(
		"You are scoring %d wildlife-camera photos. The intended subject "+
			"is %s (detector confidence %.2f). The detector frequently "+
			"false-positives leaves, branches, foliage, empty feeders, and "+
			"fence posts as birds — your job is to catch those, AND to "+
			"penalize technically flawed shots even when a real bird is "+
			"present. Be strict.\n"+
			"\n"+
			"For EACH photo output two fields:\n"+
			"\n"+
			"  • \"bird_present\": true ONLY if you can clearly see a real "+
			"bird (body, head, wing, tail, beak, or recognizable feathers). "+
			"Empty feeders, leaves, branches, sky, fences, ground, "+
			"buildings, non-bird animals → false. Do not guess.\n"+
			"\n"+
			"  • \"score\": integer 0–100. If bird_present is false, score "+
			"MUST be 0. If true, walk through these HARD CAPS in order and "+
			"apply the LOWEST cap whose condition is met:\n"+
			"\n"+
			"      bird's head is not clearly visible\n"+
			"        (cropped off, hidden behind something, facing fully\n"+
			"        away with no eye/beak visible)        →  score ≤ 25\n"+
			"\n"+
			"      image is blurry, motion-blurred, or the bird itself is\n"+
			"        out of focus (even slightly soft)     →  score ≤ 30\n"+
			"\n"+
			"      bird's body is more than ~25%% obscured\n"+
			"        by a branch, leaf, feeder, or other object\n"+
			"                                              →  score ≤ 30\n"+
			"\n"+
			"      any part of the bird is clipped by the image edge\n"+
			"        (wing, tail, foot — anything)         →  score ≤ 35\n"+
			"\n"+
			"      bird occupies less than ~10%% of the frame\n"+
			"        or is in a far corner                 →  score ≤ 55\n"+
			"\n"+
			"      busy/distracting background, awkward pose, harsh\n"+
			"        lighting, but bird is whole and sharp →  score ≤ 70\n"+
			"\n"+
			"      none of the above apply (whole bird in frame, sharp,\n"+
			"        well-lit, clean composition)          →  score 75–100\n"+
			"        (reserve 90+ for tack-sharp, perfectly composed,\n"+
			"        unobstructed portrait shots)\n"+
			"\n"+
			"Multiple flaws stack only by lowering — never raise above the "+
			"lowest cap that fires. Default-biasing toward 60–80 is wrong; "+
			"most wildlife-camera frames have at least one flaw above and "+
			"belong in the 20–40 range.\n"+
			"\n"+
			"Examples for a 5-image batch [clear cardinal, head hidden by "+
			"leaf, motion-blurred robin, tail clipped at right edge, just "+
			"branches]:\n"+
			"{\"results\":[{\"bird_present\":true,\"score\":85},"+
			"{\"bird_present\":true,\"score\":22},"+
			"{\"bird_present\":true,\"score\":27},"+
			"{\"bird_present\":true,\"score\":33},"+
			"{\"bird_present\":false,\"score\":0}]}\n"+
			"\n"+
			"Respond with JSON ONLY in this exact shape with %d entries in "+
			"input order: {\"results\":[{\"bird_present\":<bool>,\"score\":"+
			"<int>}, ...]}. No prose, no markdown fences, no explanation.",
		n, subject, confidence, n,
	)
}

// parseScores tolerates a model that wraps the JSON in markdown fences,
// adds a leading "Here are the scores:" preamble, etc. It looks for the
// first '{' and the matching last '}'.
//
// Two response shapes are supported:
//
//  1. Current: {"results":[{"bird_present":bool,"score":int}, ...]}
//     score is FORCED to 0 server-side when bird_present is false, so the
//     model can't accidentally hedge a non-bird as "40 — leaf-shaped".
//  2. Legacy: {"scores":[<n>, <n>, ...]}
//     Kept as a fallback for models that ignore the schema instruction.
func parseScores(raw string, want int) ([]Score, error) {
	s := strings.TrimSpace(raw)
	// Strip ``` fences if present
	if strings.HasPrefix(s, "```") {
		s = strings.TrimPrefix(s, "```json")
		s = strings.TrimPrefix(s, "```")
		s = strings.TrimSpace(s)
		if i := strings.LastIndex(s, "```"); i >= 0 {
			s = strings.TrimSpace(s[:i])
		}
	}
	// Find the JSON object even if there's extra prose
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	if j := strings.LastIndex(s, "}"); j >= 0 && j < len(s)-1 {
		s = s[:j+1]
	}

	// Try the current bird_present + score shape first.
	var current struct {
		Results []struct {
			BirdPresent *bool `json:"bird_present"`
			Score       int   `json:"score"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(s), &current); err == nil && len(current.Results) > 0 {
		out := make([]Score, want)
		for i := 0; i < want; i++ {
			v := 0
			if i < len(current.Results) {
				r := current.Results[i]
				// Bird absent → force 0 regardless of what the model
				// stuffed into "score". This is the whole point of the
				// two-field shape: the binary judgment overrides the
				// numeric one.
				if r.BirdPresent != nil && !*r.BirdPresent {
					v = 0
				} else {
					v = r.Score
				}
			}
			if v < 0 {
				v = 0
			}
			if v > 100 {
				v = 100
			}
			out[i] = Score{Index: i, Value: v}
		}
		return out, nil
	}

	// Legacy fallback: {"scores":[...]}
	var parsed struct {
		Scores []int `json:"scores"`
	}
	if err := json.Unmarshal([]byte(s), &parsed); err != nil {
		return nil, fmt.Errorf("parse scores from %q: %w", truncate(raw, 200), err)
	}
	out := make([]Score, want)
	for i := 0; i < want; i++ {
		v := 0
		if i < len(parsed.Scores) {
			v = parsed.Scores[i]
		}
		if v < 0 {
			v = 0
		}
		if v > 100 {
			v = 100
		}
		out[i] = Score{Index: i, Value: v}
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
