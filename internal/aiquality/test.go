package aiquality

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"time"
)

// TestResult is a rich diagnostic record from Service.Test, surfacing
// every layer at which the test could fail (network → HTTP → response
// shape → JSON parse → model output) so the Settings UI can describe
// exactly what's wrong.
type TestResult struct {
	HTTPStatus  int    `json:"http_status,omitempty"`
	LatencyMS   int64  `json:"latency_ms,omitempty"`
	Score       int    `json:"score,omitempty"`
	RawResponse string `json:"raw_response,omitempty"` // model's reply text (or full body on HTTP/JSON error)
}

// Test fires a single small synthetic image at the configured endpoint
// and reports back what came of it. The returned error (when non-nil)
// is the user-facing reason the test failed; partial diagnostic data on
// the result (HTTPStatus, RawResponse, LatencyMS) is filled where
// available even on failure.
func (s *Service) Test(ctx context.Context) (TestResult, error) {
	var result TestResult
	if s.cfg.URL == "" || s.cfg.Model == "" {
		return result, errors.New("URL and model are required")
	}

	img := makeSyntheticTestJPEG()
	prompt := buildPrompt("test image", 0.5, 1)
	dataURL := "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(img)

	body := map[string]any{
		"model": s.cfg.Model,
		"messages": []map[string]any{
			{"role": "system", "content": "You are an image-quality judge. Reply with JSON only, no commentary."},
			{
				"role": "user",
				"content": []map[string]any{
					{"type": "text", "text": prompt},
					{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
				},
			},
		},
		"temperature":          0,
		"max_tokens":           4096,
		"response_format":      scoresResponseFormat,
		"chat_template_kwargs": map[string]any{"enable_thinking": false},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return result, fmt.Errorf("marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.URL, bytes.NewReader(buf))
	if err != nil {
		return result, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.cfg.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.cfg.BearerToken)
	}

	start := time.Now()
	resp, err := s.client.Do(req)
	latency := time.Since(start)
	result.LatencyMS = latency.Milliseconds()
	if err != nil {
		// Network-layer failure: dial timeout, refused, DNS, TLS, etc.
		return result, fmt.Errorf("network: %w", err)
	}
	defer resp.Body.Close()
	result.HTTPStatus = resp.StatusCode
	rawBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 256*1024))

	if resp.StatusCode != http.StatusOK {
		result.RawResponse = string(rawBytes)
		// Surface the most common cases with a clearer label.
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return result, fmt.Errorf("HTTP 401 — bearer token missing or invalid")
		case http.StatusForbidden:
			return result, fmt.Errorf("HTTP 403 — token rejected for this model/URL")
		case http.StatusNotFound:
			return result, fmt.Errorf("HTTP 404 — endpoint URL is wrong (no /v1/chat/completions there)")
		case http.StatusTooManyRequests:
			return result, fmt.Errorf("HTTP 429 — rate limited")
		default:
			return result, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
	}

	// Try to parse the chat-completions envelope.
	var envelope struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rawBytes, &envelope); err != nil {
		result.RawResponse = string(rawBytes)
		return result, fmt.Errorf("server returned non-JSON body (not an OpenAI-compatible endpoint?): %w", err)
	}
	if len(envelope.Choices) == 0 {
		result.RawResponse = string(rawBytes)
		return result, errors.New("response has no choices array (model didn't generate output)")
	}
	content := envelope.Choices[0].Message.Content
	result.RawResponse = content

	// Parse the model's content for the score JSON.
	scores, err := parseScores(content, 1)
	if err != nil {
		return result, fmt.Errorf("model didn't return parseable JSON: %w", err)
	}
	if len(scores) == 0 {
		return result, errors.New("model returned an empty scores array")
	}
	result.Score = scores[0].Value
	return result, nil
}

// makeSyntheticTestJPEG produces a 256×256 JPEG with a simple RGB
// gradient — enough actual pixel data that any vision model will
// accept it (some reject 1×1 inputs), small enough to be cheap.
func makeSyntheticTestJPEG() []byte {
	const sz = 256
	img := image.NewRGBA(image.Rect(0, 0, sz, sz))
	for y := 0; y < sz; y++ {
		for x := 0; x < sz; x++ {
			img.Set(x, y, color.RGBA{
				R: uint8(x),
				G: uint8(y),
				B: 128,
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	_ = jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80})
	return buf.Bytes()
}
