// Package classifier wraps a 526-class ViT bird-species ONNX model so the
// detector can classify cropped bird boxes. The companion JSON sidecar
// (models/bird_classifier_classes.json) carries class names and the input
// size + ImageNet-style normalization stats used at training time.
//
// Concurrency: callers must not invoke Classify concurrently. The detector
// already serializes ONNX runs across all sessions via its ortMu, so this
// package's session shares that lock by being called from inside it.
package classifier

import (
	"encoding/json"
	"fmt"
	"image"
	"log"
	"math"
	"os"
	"sort"
	"strings"

	"golang.org/x/image/draw"

	ort "github.com/yalue/onnxruntime_go"
)

// Guess is one (species, confidence) prediction.
type Guess struct {
	Name       string  `json:"name"`
	Confidence float64 `json:"confidence"`
}

type meta struct {
	Model     string    `json:"model"`
	InputSize int       `json:"input_size"`
	Mean      []float32 `json:"mean"`
	Std       []float32 `json:"std"`
	Classes   []string  `json:"classes"`
}

type Classifier struct {
	session   *ort.AdvancedSession
	input     *ort.Tensor[float32]
	output    *ort.Tensor[float32]
	inputSize int
	mean      [3]float32
	std       [3]float32
	classes   []string
	teardown  func()
}

// New loads the ONNX model + JSON sidecar. ORT environment must already be
// initialized by the caller (the detector does this).
func New(modelPath, classesPath string) (*Classifier, error) {
	if _, err := os.Stat(modelPath); err != nil {
		return nil, fmt.Errorf("model file: %w", err)
	}
	raw, err := os.ReadFile(classesPath)
	if err != nil {
		return nil, fmt.Errorf("classes file: %w", err)
	}
	var m meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("classes json: %w", err)
	}
	if m.InputSize <= 0 || len(m.Classes) == 0 || len(m.Mean) != 3 || len(m.Std) != 3 {
		return nil, fmt.Errorf("classes json missing fields (input_size=%d classes=%d mean=%d std=%d)",
			m.InputSize, len(m.Classes), len(m.Mean), len(m.Std))
	}

	inputs, outputs, err := ort.GetInputOutputInfo(modelPath)
	if err != nil {
		return nil, fmt.Errorf("read model io: %w", err)
	}
	if len(inputs) != 1 || len(outputs) != 1 {
		return nil, fmt.Errorf("expected 1 input/1 output, got %d/%d", len(inputs), len(outputs))
	}
	inputName, outputName := inputs[0].Name, outputs[0].Name

	sz := int64(m.InputSize)
	inT, err := ort.NewEmptyTensor[float32](ort.NewShape(1, 3, sz, sz))
	if err != nil {
		return nil, fmt.Errorf("input tensor: %w", err)
	}
	outT, err := ort.NewEmptyTensor[float32](ort.NewShape(1, int64(len(m.Classes))))
	if err != nil {
		inT.Destroy()
		return nil, fmt.Errorf("output tensor: %w", err)
	}

	opts, err := ort.NewSessionOptions()
	if err != nil {
		inT.Destroy()
		outT.Destroy()
		return nil, fmt.Errorf("session options: %w", err)
	}
	defer opts.Destroy()

	if strings.ToLower(os.Getenv("LINDA_DETECTOR_DEVICE")) != "cpu" {
		if cudaOpts, err := ort.NewCUDAProviderOptions(); err == nil {
			if appendErr := opts.AppendExecutionProviderCUDA(cudaOpts); appendErr != nil {
				log.Printf("classifier: CUDA EP registration failed (%v); CPU fallback", appendErr)
			} else {
				log.Printf("classifier: CUDA execution provider enabled")
			}
			_ = cudaOpts.Destroy()
		}
	}

	sess, err := ort.NewAdvancedSession(modelPath,
		[]string{inputName}, []string{outputName},
		[]ort.Value{inT}, []ort.Value{outT}, opts)
	if err != nil {
		inT.Destroy()
		outT.Destroy()
		return nil, fmt.Errorf("ort session: %w", err)
	}

	c := &Classifier{
		session:   sess,
		input:     inT,
		output:    outT,
		inputSize: m.InputSize,
		mean:      [3]float32{m.Mean[0], m.Mean[1], m.Mean[2]},
		std:       [3]float32{m.Std[0], m.Std[1], m.Std[2]},
		classes:   m.Classes,
	}
	c.teardown = func() {
		sess.Destroy()
		inT.Destroy()
		outT.Destroy()
	}
	return c, nil
}

func (c *Classifier) Close() {
	if c == nil || c.teardown == nil {
		return
	}
	c.teardown()
	c.teardown = nil
}

// Classes returns the model's full species list (read-only).
func (c *Classifier) Classes() []string { return c.classes }

// Classify runs the model on the given crop and returns up to topK guesses
// sorted by descending confidence. Caller is responsible for serializing
// against any other ORT Run calls (the detector holds its ortMu around this).
func (c *Classifier) Classify(crop image.Image, topK int) ([]Guess, error) {
	if c == nil {
		return nil, fmt.Errorf("classifier not initialized")
	}
	if err := c.writeTensor(crop); err != nil {
		return nil, fmt.Errorf("prep: %w", err)
	}
	if err := c.session.Run(); err != nil {
		return nil, fmt.Errorf("run: %w", err)
	}
	logits := c.output.GetData()
	if len(logits) != len(c.classes) {
		return nil, fmt.Errorf("output size %d != classes %d", len(logits), len(c.classes))
	}
	return softmaxTopK(logits, c.classes, topK), nil
}

// writeTensor letterbox-resizes the crop to inputSize×inputSize, RGB-normalizes
// with the model's mean/std, and lays it out as planar CHW float32. Letterbox
// (rather than stretch) preserves bird body proportions which matter for ID.
func (c *Classifier) writeTensor(src image.Image) error {
	data := c.input.GetData()
	channelSize := c.inputSize * c.inputSize
	if len(data) < channelSize*3 {
		return fmt.Errorf("tensor too small")
	}

	// Letterbox: fit into a square canvas, center, pad with the per-channel
	// mean (which becomes 0 after normalization — i.e. neutral input).
	canvas := image.NewRGBA(image.Rect(0, 0, c.inputSize, c.inputSize))
	padR := uint8(c.mean[0] * 255)
	padG := uint8(c.mean[1] * 255)
	padB := uint8(c.mean[2] * 255)
	for i := 0; i < len(canvas.Pix); i += 4 {
		canvas.Pix[i+0] = padR
		canvas.Pix[i+1] = padG
		canvas.Pix[i+2] = padB
		canvas.Pix[i+3] = 255
	}
	srcW := src.Bounds().Dx()
	srcH := src.Bounds().Dy()
	scale := float64(c.inputSize) / float64(maxInt(srcW, srcH))
	dstW := int(float64(srcW) * scale)
	dstH := int(float64(srcH) * scale)
	if dstW < 1 {
		dstW = 1
	}
	if dstH < 1 {
		dstH = 1
	}
	offX := (c.inputSize - dstW) / 2
	offY := (c.inputSize - dstH) / 2
	dstRect := image.Rect(offX, offY, offX+dstW, offY+dstH)
	draw.CatmullRom.Scale(canvas, dstRect, src, src.Bounds(), draw.Over, nil)

	r := data[:channelSize]
	g := data[channelSize : channelSize*2]
	b := data[channelSize*2 : channelSize*3]
	pix := canvas.Pix
	stride := canvas.Stride
	mr, mg, mb := c.mean[0], c.mean[1], c.mean[2]
	sr, sg, sb := c.std[0], c.std[1], c.std[2]
	idx := 0
	for y := 0; y < c.inputSize; y++ {
		row := pix[y*stride : y*stride+c.inputSize*4]
		for x := 0; x < c.inputSize; x++ {
			px := row[x*4 : x*4+4]
			r[idx] = (float32(px[0])/255.0 - mr) / sr
			g[idx] = (float32(px[1])/255.0 - mg) / sg
			b[idx] = (float32(px[2])/255.0 - mb) / sb
			idx++
		}
	}
	return nil
}

// softmaxTopK returns the top-K (name, prob) pairs from a logit vector.
func softmaxTopK(logits []float32, names []string, topK int) []Guess {
	// numerically stable softmax
	maxL := logits[0]
	for _, v := range logits[1:] {
		if v > maxL {
			maxL = v
		}
	}
	probs := make([]float64, len(logits))
	var sum float64
	for i, v := range logits {
		p := math.Exp(float64(v - maxL))
		probs[i] = p
		sum += p
	}
	for i := range probs {
		probs[i] /= sum
	}
	idx := make([]int, len(probs))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return probs[idx[a]] > probs[idx[b]] })
	if topK <= 0 || topK > len(idx) {
		topK = len(idx)
	}
	out := make([]Guess, topK)
	for i := 0; i < topK; i++ {
		out[i] = Guess{Name: names[idx[i]], Confidence: probs[idx[i]]}
	}
	return out
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
