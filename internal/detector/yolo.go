package detector

import (
	"bytes"
	"context"
	"fmt"
	"image"
	imgjpeg "image/jpeg"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/image/draw"

	ort "github.com/yalue/onnxruntime_go"

	"github.com/linda/linda_cam/internal/aiquality"
	"github.com/linda/linda_cam/internal/capture"
	"github.com/linda/linda_cam/internal/classifier"
	"github.com/linda/linda_cam/internal/config"
	"github.com/linda/linda_cam/internal/detlog"
	"github.com/linda/linda_cam/internal/ebird"
	"github.com/linda/linda_cam/internal/jpeg"
)

const pollInterval = 1 * time.Second

// BBox is a normalized bounding box in the range [0,1] of the original
// frame's width and height. (x1,y1) is top-left, (x2,y2) is bottom-right.
type BBox struct {
	X1 float64 `json:"x1"`
	Y1 float64 `json:"y1"`
	X2 float64 `json:"x2"`
	Y2 float64 `json:"y2"`
}

// Detection describes one species present in the current frame.
type Detection struct {
	ClassID    int                `json:"class_id"`
	Name       string             `json:"name"`
	Confidence float64            `json:"confidence"`
	Box        *BBox              `json:"box,omitempty"`
	Species    []classifier.Guess `json:"species,omitempty"`
}

// yoloSession is one loaded YOLOv8 ONNX model plus everything needed to
// prep input and parse output. Multiple sessions share a single ORT env.
type yoloSession struct {
	path       string
	session    *ort.AdvancedSession
	input      *ort.Tensor[float32]
	output     *ort.Tensor[float32]
	inputSize  int
	numClasses int
	numPreds   int
	classNames []string // raw class names from model metadata
	// canonical[i] is the name that session's class i should report as.
	// For the primary model it equals classNames[i]. For the optional
	// secondary bird model every entry is forced to "Bird" so detections
	// merge into the existing "bird" watchlist entry.
	canonical []string
	teardown  func()
}

// Detector runs YOLOv8 inference on periodic frames pulled from the JPEG
// extractor. Supports an optional secondary model (e.g. a bird specialist)
// whose detections merge into the primary's canonical class space.
type Detector struct {
	extractor *jpeg.Extractor
	captures  *capture.Store
	cfgStore  *config.Store
	logger    *detlog.Logger

	enabled atomic.Bool
	ready   atomic.Bool

	stateMu         sync.RWMutex
	present         []Detection
	lastSeen        map[string]time.Time
	lastAutoCapture time.Time
	lastCaptureAt   time.Time
	// captureSessions tracks the "best" picture saved for each currently-
	// active sighting (keyed by canonical species name). While a sighting is
	// active, additional detections of the same species replace the existing
	// JPEG only if they score better than the running best. See tick().
	captureSessions map[string]*captureSession

	// birdSess is the active bird-pipeline session, or nil when no bird
	// is currently sighted. The bird pipeline runs in parallel with
	// captureSessions: per-tick crops are buffered here, and at session
	// close the top-N crops by classifier confidence are AI-quality-
	// scored and the survivors are persisted as a single multi-crop
	// gallery entry.
	birdSess     *birdSession
	birdParentID int

	// correctMu protects the classifier-name correction rule cache. The
	// cached slice mirrors cfg.ClassifierCorrections and is rebuilt only
	// when the config changes (cheap pointer-equality skip on the hot path).
	correctMu     sync.RWMutex
	cachedRules   []config.CorrectionRule
	cachedRegexes []*regexp.Regexp

	// ortMu serializes Run() across every session sharing the ORT env.
	ortMu        sync.Mutex
	primary      *yoloSession
	secondary    *yoloSession
	birdClassif  *classifier.Classifier
	envTeardown  func()

	// aiQuality is set via SetAIQuality after Start. nil → AI-pick disabled
	// and the session-end finalize is a no-op (heuristic-best stays).
	aiQualityMu sync.RWMutex
	aiQuality   *aiquality.Service

	// ebird is the optional location-aware species filter. nil → filter
	// disabled and classifier output passes through untouched.
	ebirdMu sync.RWMutex
	ebird   *ebird.Service

	cancel context.CancelFunc
}

func New(extractor *jpeg.Extractor, captures *capture.Store, cfgStore *config.Store, logger *detlog.Logger) *Detector {
	return &Detector{
		extractor:       extractor,
		captures:        captures,
		cfgStore:        cfgStore,
		logger:          logger,
		lastSeen:        make(map[string]time.Time),
		captureSessions: make(map[string]*captureSession),
	}
}

// captureSession tracks the "best" picture saved for an in-progress
// sighting of one species, keyed by canonical name.
type captureSession struct {
	pictureName string
	score       float64
	startedAt   time.Time
	lastSeen    time.Time
	reason      capture.Reason

	// For AI-quality scoring at session close: a small ring of the
	// highest-scoring frames seen during the sighting. Each entry holds
	// the JPEG bytes captured at that moment so the AI can compare them.
	candidates    []candidateFrame
	species       string  // canonical name shown to the AI prompt
	speciesConf   float64 // top YOLO confidence reported during the session
}

// candidateFrame is one buffered JPEG that *could* end up the kept frame
// for a sighting if it scores best in the AI batch.
type candidateFrame struct {
	jpeg       []byte
	score      float64 // session heuristic score at the moment of capture
	capturedAt time.Time
}

// birdCandidate is one extracted bird crop accumulated during an
// active bird sighting. At session close the candidates are sorted
// by classifierTopConf, the top N go to AI quality scoring, and the
// survivors are persisted as the sighting's multi-crop gallery entry.
type birdCandidate struct {
	cropImg           image.Image
	parentJpeg        []byte // full-frame JPEG bytes of the source frame
	parentID          int    // shared by all candidates from the same tick
	parentBirdCount   int    // number of bird detections in the parent frame
	classifierGuesses []classifier.Guess
	classifierTopConf float64
	yoloConf          float64
	box               capture.BBox
	capturedAt        time.Time
}

// birdSession is the in-flight bird sighting buffer. There is at most
// one active session at a time. lastSeen drives the timeout-based
// finalize.
type birdSession struct {
	startedAt  time.Time
	lastSeen   time.Time
	candidates []birdCandidate
}

// ClassNames returns the primary model's full class-name list. Used by the
// Settings page to autocomplete watchlist entries; the secondary model's
// classes aren't exposed here because they all map to "Bird" which is
// already present in the primary (OIV7) namespace.
func (d *Detector) ClassNames() []string {
	if d.primary == nil {
		return nil
	}
	out := make([]string, len(d.primary.classNames))
	copy(out, d.primary.classNames)
	return out
}

// Start initializes ORT, loads the primary model, and (if the file exists)
// the secondary model. Launches the inference goroutine. Missing library or
// primary model just disables detection without returning an error; missing
// secondary is silently ignored (primary-only mode).
func (d *Detector) Start(ctx context.Context, libraryPath, primaryPath, secondaryPath string) error {
	if _, err := os.Stat(libraryPath); err != nil {
		log.Printf("detector: onnxruntime library %q not found; detection disabled", libraryPath)
		return nil
	}
	if _, err := os.Stat(primaryPath); err != nil {
		log.Printf("detector: primary model %q not found; detection disabled", primaryPath)
		return nil
	}

	ort.SetSharedLibraryPath(libraryPath)
	if err := ort.InitializeEnvironment(); err != nil {
		return fmt.Errorf("ort init: %w", err)
	}
	d.envTeardown = func() { _ = ort.DestroyEnvironment() }

	primary, err := loadSession(primaryPath, "")
	if err != nil {
		d.envTeardown()
		d.envTeardown = nil
		return fmt.Errorf("primary: %w", err)
	}
	d.primary = primary
	log.Printf("detector: primary model %q classes=%d anchors=%d input=%dx%d",
		primaryPath, primary.numClasses, primary.numPreds, primary.inputSize, primary.inputSize)

	if secondaryPath != "" {
		if _, err := os.Stat(secondaryPath); err == nil {
			sec, err := loadSession(secondaryPath, "Bird")
			if err != nil {
				log.Printf("detector: secondary model %q failed to load: %v (continuing with primary only)", secondaryPath, err)
			} else {
				d.secondary = sec
				log.Printf("detector: secondary model %q classes=%d anchors=%d input=%dx%d → forcing canonical name=%q",
					secondaryPath, sec.numClasses, sec.numPreds, sec.inputSize, sec.inputSize, "Bird")
			}
		} else {
			log.Printf("detector: secondary model %q not present; primary-only", secondaryPath)
		}
	}

	d.ready.Store(true)
	d.enabled.Store(true)

	ctx, cancel := context.WithCancel(ctx)
	d.cancel = cancel
	go d.loop(ctx)
	log.Printf("detector: started")
	return nil
}

// loadSession loads one YOLOv8 ONNX model, sizing its input/output tensors
// from the model's declared shapes. If canonicalOverride is non-empty, every
// class in the session will be reported under that name.
func loadSession(modelPath, canonicalOverride string) (*yoloSession, error) {
	inputs, outputs, err := ort.GetInputOutputInfo(modelPath)
	if err != nil {
		return nil, fmt.Errorf("read model io: %w", err)
	}
	if len(inputs) != 1 || len(outputs) != 1 {
		return nil, fmt.Errorf("expected 1 input and 1 output, got %d in / %d out", len(inputs), len(outputs))
	}
	inputName, outputName := inputs[0].Name, outputs[0].Name
	inShape := inputs[0].Dimensions
	outShape := outputs[0].Dimensions
	if len(inShape) != 4 {
		return nil, fmt.Errorf("unexpected input shape %v (want [1,3,H,W])", inShape)
	}
	if len(outShape) != 3 || outShape[1] < 5 {
		return nil, fmt.Errorf("unexpected output shape %v", outShape)
	}
	if inShape[2] != inShape[3] {
		return nil, fmt.Errorf("non-square input %dx%d not supported", inShape[2], inShape[3])
	}
	inputSize := int(inShape[2])
	numClasses := int(outShape[1]) - 4
	numPreds := int(outShape[2])

	names, err := readClassNames(modelPath, numClasses)
	if err != nil {
		log.Printf("detector: class names unavailable for %q (%v); using numeric IDs", modelPath, err)
		names = make([]string, numClasses)
		for i := range names {
			names[i] = fmt.Sprintf("class_%d", i)
		}
	}
	canonical := make([]string, numClasses)
	if canonicalOverride != "" {
		for i := range canonical {
			canonical[i] = canonicalOverride
		}
	} else {
		copy(canonical, names)
	}

	inputTensor, err := ort.NewEmptyTensor[float32](ort.NewShape(1, 3, int64(inputSize), int64(inputSize)))
	if err != nil {
		return nil, fmt.Errorf("input tensor: %w", err)
	}
	outputTensor, err := ort.NewEmptyTensor[float32](ort.NewShape(1, int64(4+numClasses), int64(numPreds)))
	if err != nil {
		inputTensor.Destroy()
		return nil, fmt.Errorf("output tensor: %w", err)
	}
	opts, err := ort.NewSessionOptions()
	if err != nil {
		inputTensor.Destroy()
		outputTensor.Destroy()
		return nil, fmt.Errorf("session options: %w", err)
	}
	defer opts.Destroy()

	// Try to enable the CUDA execution provider. Skipped when LINDA_DETECTOR_DEVICE=cpu,
	// or when the CUDA provider .so / GPU / driver aren't available — the
	// session falls back to the default CPU EP silently.
	device := "cuda"
	switch strings.ToLower(os.Getenv("LINDA_DETECTOR_DEVICE")) {
	case "cpu":
		device = "cpu"
	}
	if device == "cuda" {
		cudaOpts, err := ort.NewCUDAProviderOptions()
		if err != nil {
			log.Printf("detector: CUDA provider options unavailable (%v); falling back to CPU", err)
		} else {
			if appendErr := opts.AppendExecutionProviderCUDA(cudaOpts); appendErr != nil {
				log.Printf("detector: CUDA EP registration failed (%v); falling back to CPU", appendErr)
			} else {
				log.Printf("detector: CUDA execution provider enabled for %q", modelPath)
			}
			_ = cudaOpts.Destroy()
		}
	}

	session, err := ort.NewAdvancedSession(modelPath,
		[]string{inputName}, []string{outputName},
		[]ort.Value{inputTensor}, []ort.Value{outputTensor}, opts)
	if err != nil {
		inputTensor.Destroy()
		outputTensor.Destroy()
		return nil, fmt.Errorf("ort session: %w", err)
	}

	s := &yoloSession{
		path:       modelPath,
		session:    session,
		input:      inputTensor,
		output:     outputTensor,
		inputSize:  inputSize,
		numClasses: numClasses,
		numPreds:   numPreds,
		classNames: names,
		canonical:  canonical,
	}
	s.teardown = func() {
		session.Destroy()
		inputTensor.Destroy()
		outputTensor.Destroy()
	}
	return s, nil
}

// SetBirdClassifier attaches an optional fine-grained bird-species classifier
// to be invoked on bird crops during each detector tick. Call before Stop.
// ORT environment must already be initialized (i.e. call after Start).
func (d *Detector) SetBirdClassifier(c *classifier.Classifier) {
	d.ortMu.Lock()
	defer d.ortMu.Unlock()
	d.birdClassif = c
}

// SetAIQuality plugs in (or unsets, with nil) the multimodal-AI service
// that scores candidate frames at session close. Safe to call any time.
func (d *Detector) SetAIQuality(s *aiquality.Service) {
	d.aiQualityMu.Lock()
	defer d.aiQualityMu.Unlock()
	d.aiQuality = s
}

func (d *Detector) currentAIQuality() *aiquality.Service {
	d.aiQualityMu.RLock()
	defer d.aiQualityMu.RUnlock()
	return d.aiQuality
}

// SetEBird wires up the location-aware species filter. nil disables it.
func (d *Detector) SetEBird(s *ebird.Service) {
	d.ebirdMu.Lock()
	defer d.ebirdMu.Unlock()
	d.ebird = s
}

func (d *Detector) currentEBird() *ebird.Service {
	d.ebirdMu.RLock()
	defer d.ebirdMu.RUnlock()
	return d.ebird
}

func (d *Detector) Stop() {
	if d.cancel != nil {
		d.cancel()
	}
	d.ortMu.Lock()
	defer d.ortMu.Unlock()
	if d.primary != nil && d.primary.teardown != nil {
		d.primary.teardown()
		d.primary.teardown = nil
	}
	if d.secondary != nil && d.secondary.teardown != nil {
		d.secondary.teardown()
		d.secondary.teardown = nil
	}
	if d.birdClassif != nil {
		d.birdClassif.Close()
		d.birdClassif = nil
	}
	if d.envTeardown != nil {
		d.envTeardown()
		d.envTeardown = nil
	}
	d.ready.Store(false)
}

func (d *Detector) Ready() bool       { return d.ready.Load() }
func (d *Detector) Enabled() bool     { return d.enabled.Load() }
func (d *Detector) SetEnabled(v bool) { d.enabled.Store(v) }

type Status struct {
	Present       []Detection
	LastCaptureAt time.Time
	DetectorReady bool
}

func (d *Detector) Status() Status {
	d.stateMu.RLock()
	defer d.stateMu.RUnlock()
	pres := make([]Detection, len(d.present))
	copy(pres, d.present)
	return Status{
		Present:       pres,
		LastCaptureAt: d.lastCaptureAt,
		DetectorReady: d.ready.Load(),
	}
}

func (d *Detector) loop(ctx context.Context) {
	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if !d.enabled.Load() {
				continue
			}
			d.tick()
		}
	}
}

func (d *Detector) tick() {
	frame, _ := d.extractor.Latest()
	if frame == nil {
		return
	}
	img, _, err := image.Decode(bytes.NewReader(frame))
	if err != nil {
		return
	}

	cfg := d.cfgStore.Get()
	now := time.Now()
	sessionTimeout := time.Duration(cfg.SessionTimeoutS) * time.Second
	if sessionTimeout <= 0 {
		sessionTimeout = 60 * time.Second
	}

	d.ortMu.Lock()

	watched := cfg.WatchedAnimals

	// Generic pipeline: primary + secondary YOLO inference. Both runs
	// always happen (inference is unconditional inside
	// runSessionForWatchlist); only the post-processing is gated on
	// the watched-animals set. Inference results from secondary are
	// only used by the bird pipeline below — runSessionForWatchlist
	// returns no detections from secondary because bird isn't in the
	// generic watch set anymore.
	var detections []Detection
	if d.primary != nil {
		if r, err := d.runSessionForWatchlist(d.primary, img, d.watchSet(d.primary, watched)); err != nil {
			log.Printf("detector: primary tick: %v", err)
		} else {
			detections = append(detections, r...)
		}
	}
	if d.secondary != nil {
		if _, err := d.runSessionForWatchlist(d.secondary, img, d.watchSet(d.secondary, watched)); err != nil {
			log.Printf("detector: secondary tick: %v", err)
		}
	}
	detections = mergeByName(detections)

	// Bird pipeline: extract crops + classify under the same lock so
	// session.output is fresh from the inference call above.
	birdCands := d.collectBirdCandidatesLocked(img, frame, cfg.BirdConfidenceThreshold, now)

	d.ortMu.Unlock()

	// Inject a synthetic "Bird" detection so /status and the live
	// overlay reflect that a bird is being tracked, even though bird
	// isn't in WatchedAnimals.
	if len(birdCands) > 0 {
		detections = append(detections, birdCandidatesToDetection(birdCands))
	}
	sort.Slice(detections, func(i, j int) bool { return detections[i].Confidence > detections[j].Confidence })

	d.stateMu.Lock()
	d.present = detections
	for _, det := range detections {
		d.lastSeen[det.Name] = now
	}
	// Append candidates to the active bird session (creating it if
	// this is the first tick of a new sighting). lastSeen advances on
	// every tick that has bird candidates above threshold.
	if len(birdCands) > 0 {
		if d.birdSess == nil {
			d.birdSess = &birdSession{startedAt: now}
		}
		d.birdSess.lastSeen = now
		d.birdSess.candidates = append(d.birdSess.candidates, birdCands...)
	}
	// Stale-check: bird session times out after the same window as
	// the generic ones. Finalize off the tick goroutine.
	var staleBird *birdSession
	if d.birdSess != nil && now.Sub(d.birdSess.lastSeen) > sessionTimeout {
		staleBird = d.birdSess
		d.birdSess = nil
	}
	d.stateMu.Unlock()
	if staleBird != nil {
		go d.finalizeBirdSession(*staleBird)
	}

	if len(detections) == 0 {
		return
	}

	top := detections[0]
	classNames := make([]string, len(detections))
	for i, det := range detections {
		classNames[i] = det.Name
	}

	logID, err := d.logger.Append(detlog.Entry{
		Timestamp:     now,
		Classes:       classNames,
		TopClass:      top.Name,
		TopConfidence: top.Confidence,
	})
	if err != nil {
		log.Printf("detector: log append: %v", err)
	}

	if !cfg.AutoCaptureEnabled {
		return
	}
	cooldown := time.Duration(cfg.DetectionCooldownS) * time.Second
	if cooldown <= 0 {
		cooldown = 5 * time.Second
	}

	// runCaptureSessions handles only non-bird species — the bird
	// pipeline above owns that path. The synthetic Bird detection
	// stays in `detections` for live status and detlog, but must be
	// excluded here or runCaptureSessions would *also* save a
	// captureSession-style picture for it (heuristic-best, no crop
	// sidecar) and we'd end up with two gallery entries per sighting.
	genericDets := detections
	if len(birdCands) > 0 {
		genericDets = make([]Detection, 0, len(detections))
		for _, det := range detections {
			if normalizeClass(det.Name) == "bird" {
				continue
			}
			genericDets = append(genericDets, det)
		}
	}
	d.runCaptureSessions(genericDets, frame, nil, now, cooldown, sessionTimeout, logID)
}

// collectBirdCandidatesLocked extracts bird crops from the most-recent
// primary + secondary YOLO outputs (the caller has just run inference
// under ortMu and that lock is still held). Returns a candidate per
// crop with classifier guesses and bounding boxes already populated.
//
// Returns nil when no bird crosses the configured confidence threshold,
// or when the bird classifier isn't loaded (no point classifying).
//
// Caller MUST hold d.ortMu.
func (d *Detector) collectBirdCandidatesLocked(img image.Image, frameJpeg []byte, threshold float64, now time.Time) []birdCandidate {
	if threshold <= 0 {
		threshold = 0.30
	}

	// Pool bird boxes from both sessions at the requested threshold.
	// Up to 8 per session — multi-bird scenes shouldn't blow past
	// that, and the pool is downsampled at finalize anyway.
	type sessBoxes struct {
		s   *yoloSession
		bxs []box
	}
	var pooled []sessBoxes
	for _, candidate := range []*yoloSession{d.primary, d.secondary} {
		if candidate == nil {
			continue
		}
		bxs := d.birdBoxes(candidate, float32(threshold), 8)
		if len(bxs) == 0 {
			continue
		}
		pooled = append(pooled, sessBoxes{s: candidate, bxs: bxs})
	}
	if len(pooled) == 0 {
		return nil
	}

	srcW := img.Bounds().Dx()
	srcH := img.Bounds().Dy()
	const padFrac = 0.15
	const minCropPx = 64

	d.birdParentID++
	parentID := d.birdParentID

	out := make([]birdCandidate, 0, 4)
	parentBirdCount := 0
	for _, sb := range pooled {
		parentBirdCount += len(sb.bxs)
	}
	for _, sb := range pooled {
		scaleX := float64(srcW) / float64(sb.s.inputSize)
		scaleY := float64(srcH) / float64(sb.s.inputSize)
		for _, bx := range sb.bxs {
			w := float64(bx.x2-bx.x1) * (1 + 2*padFrac)
			h := float64(bx.y2-bx.y1) * (1 + 2*padFrac)
			cx := float64(bx.x1+bx.x2) / 2
			cy := float64(bx.y1+bx.y2) / 2
			x1 := int((cx - w/2) * scaleX)
			y1 := int((cy - h/2) * scaleY)
			x2 := int((cx + w/2) * scaleX)
			y2 := int((cy + h/2) * scaleY)
			if x1 < 0 {
				x1 = 0
			}
			if y1 < 0 {
				y1 = 0
			}
			if x2 > srcW {
				x2 = srcW
			}
			if y2 > srcH {
				y2 = srcH
			}
			if (x2-x1) < minCropPx || (y2-y1) < minCropPx {
				continue
			}
			crop := image.NewRGBA(image.Rect(0, 0, x2-x1, y2-y1))
			draw.Draw(crop, crop.Bounds(), img, image.Point{X: x1, Y: y1}, draw.Src)

			// Classify under the existing ortMu (we already hold it).
			var guesses []classifier.Guess
			if d.birdClassif != nil {
				gs, err := d.birdClassif.Classify(crop, 5)
				if err == nil {
					gs = d.filterByEBird(gs)
					for i := range gs {
						gs[i].Name = d.correctSpeciesName(gs[i].Name)
					}
					guesses = gs
				}
			}
			topConf := 0.0
			if len(guesses) > 0 {
				topConf = guesses[0].Confidence
			}
			// Normalize bbox to [0,1] of the source frame.
			normBox := capture.BBox{
				X1: float64(x1) / float64(srcW),
				Y1: float64(y1) / float64(srcH),
				X2: float64(x2) / float64(srcW),
				Y2: float64(y2) / float64(srcH),
			}
			frameCopy := make([]byte, len(frameJpeg))
			copy(frameCopy, frameJpeg)
			out = append(out, birdCandidate{
				cropImg:           crop,
				parentJpeg:        frameCopy,
				parentID:          parentID,
				parentBirdCount:   parentBirdCount,
				classifierGuesses: guesses,
				classifierTopConf: topConf,
				yoloConf:          float64(bx.confidence),
				box:               normBox,
				capturedAt:        now,
			})
		}
	}
	return out
}

// birdCandidatesToDetection summarizes the per-tick bird candidates
// into a single synthetic "Bird" Detection for live status / overlays.
// The aggregated species list pools every crop's guesses weighted by
// classifier top-conf so the rendered name reflects the most likely
// species in the scene (ties between multiple birds break naturally
// toward the higher-confidence guess).
func birdCandidatesToDetection(cands []birdCandidate) Detection {
	if len(cands) == 0 {
		return Detection{}
	}
	bestYolo := cands[0].yoloConf
	for _, c := range cands[1:] {
		if c.yoloConf > bestYolo {
			bestYolo = c.yoloConf
		}
	}
	speciesSum := map[string]float64{}
	totalWeight := 0.0
	for _, c := range cands {
		w := c.classifierTopConf
		if w <= 0 {
			continue
		}
		totalWeight += w
		for i, g := range c.classifierGuesses {
			if i >= 5 {
				break
			}
			speciesSum[g.Name] += g.Confidence * w
		}
	}
	var species []classifier.Guess
	if totalWeight > 0 {
		names := make([]string, 0, len(speciesSum))
		for n := range speciesSum {
			names = append(names, n)
		}
		sort.Slice(names, func(i, j int) bool { return speciesSum[names[i]] > speciesSum[names[j]] })
		k := 3
		if k > len(names) {
			k = len(names)
		}
		var sum float64
		for _, v := range speciesSum {
			sum += v
		}
		species = make([]classifier.Guess, k)
		for i := 0; i < k; i++ {
			species[i] = classifier.Guess{Name: names[i], Confidence: speciesSum[names[i]] / sum}
		}
	}
	return Detection{
		Name:       "Bird",
		Confidence: float64(bestYolo),
		Species:    species,
	}
}

// captureScore combines YOLO confidence with bbox area: a closer/bigger
// detection wins over a farther/smaller one even at slightly lower
// confidence. Returns 0 when the detection has no bbox.
func captureScore(d Detection) float64 {
	if d.Box == nil {
		return d.Confidence
	}
	w := d.Box.X2 - d.Box.X1
	h := d.Box.Y2 - d.Box.Y1
	if w <= 0 || h <= 0 {
		return d.Confidence
	}
	area := w * h
	if area > 1 {
		area = 1
	}
	return d.Confidence * (1 + 2*area)
}

// runCaptureSessions implements the session-based "keep best" auto-capture:
// for each detected species, it either starts a new session (saves a fresh
// picture), replaces the current best (overwrites the JPEG and re-renders
// sidecars), or skips this tick. Old sessions whose lastSeen has aged out
// are dropped from the map; the picture file for that session stays as the
// keeper.
func (d *Detector) runCaptureSessions(
	detections []Detection,
	frame []byte,
	birdCrop image.Image,
	now time.Time,
	cooldown, sessionTimeout time.Duration,
	logID int64,
) {
	const replaceHysteresis = 1.05 // require >5% better to overwrite

	d.stateMu.Lock()
	// Drop stale sessions before processing this tick's detections; once
	// pulled out of the map they're handed to a goroutine that runs the
	// optional AI-quality finalization pass off the tick path.
	var finalizing []captureSession
	for name, sess := range d.captureSessions {
		if now.Sub(sess.lastSeen) > sessionTimeout {
			finalizing = append(finalizing, *sess)
			delete(d.captureSessions, name)
		}
	}
	d.stateMu.Unlock()
	for _, sess := range finalizing {
		go d.finalizeSession(sess)
	}

	for _, det := range detections {
		key := normalizeClass(det.Name)
		score := captureScore(det)
		reason := capture.Reason{Species: det.Name, Confidence: det.Confidence}

		d.stateMu.Lock()
		sess, active := d.captureSessions[key]
		if !active {
			// Don't start a brand-new session more often than the cooldown.
			if now.Sub(d.lastAutoCapture) < cooldown {
				d.stateMu.Unlock()
				continue
			}
		}
		d.stateMu.Unlock()

		if !active {
			// New session — save a fresh picture.
			name, err := d.captures.Save(frame, reason)
			if err != nil {
				log.Printf("detector: save: %v", err)
				continue
			}
			d.stateMu.Lock()
			d.lastAutoCapture = now
			d.lastCaptureAt = now
			newSess := &captureSession{
				pictureName: name,
				score:       score,
				startedAt:   now,
				lastSeen:    now,
				reason:      reason,
				species:     det.Name,
				speciesConf: det.Confidence,
			}
			d.pushCandidate(newSess, frame, score, now)
			d.captureSessions[key] = newSess
			d.stateMu.Unlock()
			log.Printf("detector: %s (%.2f, score=%.2f) → %s", det.Name, det.Confidence, score, name)
			if logID > 0 && key == normalizeClass(detections[0].Name) {
				if err := d.logger.AttachPicture(logID, name); err != nil {
					log.Printf("detector: log attach: %v", err)
				}
			}
			if err := d.persistAnalysis(name, detections, birdCrop, now); err != nil {
				log.Printf("detector: persist analysis: %v", err)
			}
			continue
		}

		// Existing session — bump lastSeen and decide whether to replace.
		if score > sess.score*replaceHysteresis {
			if err := d.captures.Replace(sess.pictureName, frame); err != nil {
				log.Printf("detector: replace %s: %v", sess.pictureName, err)
				d.stateMu.Lock()
				sess.lastSeen = now
				d.stateMu.Unlock()
				continue
			}
			d.stateMu.Lock()
			oldScore := sess.score
			sess.score = score
			sess.lastSeen = now
			sess.reason = reason
			if det.Confidence > sess.speciesConf {
				sess.species = det.Name
				sess.speciesConf = det.Confidence
			}
			d.pushCandidate(sess, frame, score, now)
			d.lastCaptureAt = now
			d.stateMu.Unlock()
			log.Printf("detector: replace %s — %s %.2f → %.2f", sess.pictureName, det.Name, oldScore, score)
			if err := d.persistAnalysis(sess.pictureName, detections, birdCrop, now); err != nil {
				log.Printf("detector: persist analysis: %v", err)
			}
			continue
		}

		// Same session, this detection isn't better — just keep the session
		// warm AND consider buffering this frame: a slightly-lower-scoring
		// frame might still be the winner once the AI judges sharpness etc.
		d.stateMu.Lock()
		sess.lastSeen = now
		d.pushCandidate(sess, frame, score, now)
		d.stateMu.Unlock()
	}
}

// pushCandidate keeps a top-N (by heuristic score) ring of session frames
// that the AI-quality service will judge at session close. Caller holds
// d.stateMu — we mutate sess.candidates in place. When AI scoring is
// disabled we still keep just one entry so the in-flight metadata is
// consistent; the goroutine fast-paths the single-candidate case.
func (d *Detector) pushCandidate(sess *captureSession, frame []byte, score float64, now time.Time) {
	max := d.aiCandidateCap()
	// We need our own copy of `frame`; the JPEG extractor reuses its
	// buffer between ticks.
	clone := make([]byte, len(frame))
	copy(clone, frame)
	cf := candidateFrame{jpeg: clone, score: score, capturedAt: now}

	// Insert keeping descending score order; cap at `max`.
	inserted := false
	for i, existing := range sess.candidates {
		if cf.score > existing.score {
			sess.candidates = append(sess.candidates[:i+1], sess.candidates[i:]...)
			sess.candidates[i] = cf
			inserted = true
			break
		}
	}
	if !inserted {
		sess.candidates = append(sess.candidates, cf)
	}
	if len(sess.candidates) > max {
		sess.candidates = sess.candidates[:max]
	}
}

// aiCandidateCap returns the configured candidate buffer size, or 1 when
// AI scoring is disabled / misconfigured (so memory doesn't grow).
func (d *Detector) aiCandidateCap() int {
	svc := d.currentAIQuality()
	if svc == nil {
		return 1
	}
	cfg := d.cfgStore.Get().AIQuality
	n := cfg.MaxCandidates
	if n < 1 {
		return 1
	}
	if n > 10 {
		return 10
	}
	return n
}

// finalizeSession runs at session close in its own goroutine. It pulls
// the buffered candidates, sends them to the AI-quality service, picks
// the highest-scored one, and either replaces the saved JPEG with that
// candidate (then re-runs analysis) or deletes the picture entirely
// when even the best score falls below the discard threshold.
//
// Anything goes wrong → the heuristic-best file already on disk stays.
// We never delete on failure.
func (d *Detector) finalizeSession(sess captureSession) {
	svc := d.currentAIQuality()
	if svc == nil || len(sess.candidates) == 0 {
		return
	}
	cfg := d.cfgStore.Get().AIQuality
	if !cfg.Enabled {
		return
	}

	// One-candidate fast-path: still scored so the discard threshold
	// applies; no need to consider replacement.
	candidates := sess.candidates
	width := cfg.NormalizeWidth
	if width <= 0 {
		width = 1024
	}
	resized := make([][]byte, 0, len(candidates))
	keepIdx := make([]int, 0, len(candidates))
	for i, cf := range candidates {
		r, err := aiquality.Resize(cf.jpeg, width)
		if err != nil {
			log.Printf("detector: aiquality resize: %v (skipping candidate)", err)
			continue
		}
		resized = append(resized, r)
		keepIdx = append(keepIdx, i)
	}
	if len(resized) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	result, err := svc.ScoreWithRaw(ctx, sess.species, sess.speciesConf, resized)
	if err != nil && len(resized) > 1 {
		// Batched call failed (commonly: small-context vision models that
		// return empty `choices` when the prompt+images exhaust the
		// window). Retry once with just the heuristic-best — same model,
		// ~1/N the payload — before giving up.
		log.Printf("detector: aiquality score: %v (retrying with 1 candidate for %s)", err, sess.pictureName)
		resized = resized[:1]
		keepIdx = keepIdx[:1]
		result, err = svc.ScoreWithRaw(ctx, sess.species, sess.speciesConf, resized)
	}
	if err != nil {
		log.Printf("detector: aiquality score: %v (keeping heuristic-best %s)", err, sess.pictureName)
		d.recordAIQualityError(sess.pictureName, err.Error())
		return
	}
	scores := result.Scores

	bestScore := -1
	bestPos := 0
	for i, s := range scores {
		if s.Value > bestScore {
			bestScore = s.Value
			bestPos = i
		}
	}
	log.Printf("detector: aiquality scored %d candidates for %q (species=%s): best=%d/100",
		len(scores), sess.pictureName, sess.species, bestScore)

	if bestScore < cfg.DiscardThreshold {
		log.Printf("detector: aiquality best %d < threshold %d; deleting %s",
			bestScore, cfg.DiscardThreshold, sess.pictureName)
		if err := d.captures.Delete(sess.pictureName); err != nil {
			log.Printf("detector: aiquality delete %s: %v", sess.pictureName, err)
		}
		return
	}

	// Map back from resized index → candidate index (we may have skipped
	// some during resize). The candidate at keepIdx[bestPos] is the winner.
	winner := candidates[keepIdx[bestPos]]
	// If the winner is the heuristic-best (first in the descending-score
	// buffer) we don't need to overwrite the JPEG — just record the score.
	heuristicBest := keepIdx[bestPos] == 0
	if !heuristicBest {
		if err := d.captures.Replace(sess.pictureName, winner.jpeg); err != nil {
			log.Printf("detector: aiquality replace %s: %v", sess.pictureName, err)
			return
		}
		dets, crop, err := d.AnalyzeFrame(winner.jpeg)
		if err != nil {
			log.Printf("detector: aiquality reanalyze %s: %v", sess.pictureName, err)
			return
		}
		if err := d.persistAnalysis(sess.pictureName, dets, crop, time.Now()); err != nil {
			log.Printf("detector: aiquality persist %s: %v", sess.pictureName, err)
			return
		}
	}
	// Stamp the score onto the (possibly-just-rewritten) metadata sidecar.
	// Bird sessions don't reach this code path — they have their own
	// finalizeBirdSession with multi-crop persistence — so this is the
	// non-bird (deer/fox/cat/dog) flow only.
	d.recordAIQualityScore(sess.pictureName, bestScore, result.RawResponse)
}

// recordAIQualityScore reads the picture's existing metadata, sets the
// AI-quality fields (score + raw model response), clears any prior
// error, and writes it back. Failures are logged and ignored — the
// score is informational, not load-bearing.
func (d *Detector) recordAIQualityScore(name string, score int, rawResponse string) {
	md, _, err := d.captures.ReadMetadata(name)
	if err != nil {
		log.Printf("detector: read metadata for ai score %s: %v", name, err)
		return
	}
	now := time.Now()
	md.AIQualityScore = &score
	md.AIQualityAt = &now
	md.AIQualityError = ""
	md.AIQualityRaw = rawResponse
	if err := d.captures.WriteMetadata(name, md); err != nil {
		log.Printf("detector: write ai score %s: %v", name, err)
	}
}

// finalizeBirdSession is the bird-pipeline equivalent of
// finalizeSession (which handles non-bird species). At session close
// the session's accumulated candidates are sorted by classifier
// top-species confidence DESC, the top BirdMaxCrops go to AI quality
// scoring as one batched call, and surviving crops (score ≥ discard
// threshold) are persisted as a single multi-crop gallery entry.
// The parent frame is the candidate frame with the most birds in it
// (tiebreak: average classifier conf across that frame's crops).
//
// On infrastructure failure (AI errors, file write errors), the
// session is dropped without writing anything — the user keeps a
// clean gallery with only vetted sightings.
func (d *Detector) finalizeBirdSession(sess birdSession) {
	if len(sess.candidates) == 0 {
		return
	}
	cfg := d.cfgStore.Get()
	maxCrops := cfg.BirdMaxCrops
	if maxCrops <= 0 {
		maxCrops = 3
	}
	if maxCrops > 10 {
		maxCrops = 10
	}

	// Rank candidates by classifier top-species confidence DESC,
	// take top N.
	sort.Slice(sess.candidates, func(i, j int) bool {
		return sess.candidates[i].classifierTopConf > sess.candidates[j].classifierTopConf
	})
	pool := sess.candidates
	if len(pool) > maxCrops {
		pool = pool[:maxCrops]
	}

	// Encode each crop and call AI quality scoring. Without an AI
	// service we treat all crops as score=0 (which fails any non-zero
	// threshold → no entry written). The rationale: AI is the only
	// gate against leaves the classifier weakly hallucinated species
	// onto.
	svc := d.currentAIQuality()
	if svc == nil {
		log.Printf("detector: bird-finalize — pooled %d candidates but AI quality not configured; dropping sighting", len(sess.candidates))
		return
	}
	threshold := cfg.AIQuality.DiscardThreshold

	encoded := make([][]byte, 0, len(pool))
	keepIdx := make([]int, 0, len(pool))
	for i, c := range pool {
		var buf bytes.Buffer
		if err := imgjpeg.Encode(&buf, c.cropImg, &imgjpeg.Options{Quality: 85}); err != nil {
			log.Printf("detector: bird-finalize encode crop %d: %v", i, err)
			continue
		}
		encoded = append(encoded, buf.Bytes())
		keepIdx = append(keepIdx, i)
	}
	if len(encoded) == 0 {
		log.Printf("detector: bird-finalize — pooled %d candidates, no encodes succeeded", len(sess.candidates))
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	scoreResult, err := svc.ScoreWithRaw(ctx, "Bird", 0.5, encoded)
	if err != nil {
		log.Printf("detector: bird-finalize ai score: %v (dropping sighting)", err)
		return
	}

	// Build survivors: crops at or above threshold.
	type survivor struct {
		cand    birdCandidate
		aiScore int
	}
	survivors := make([]survivor, 0, len(pool))
	scores := make([]int, len(pool))
	for i, idx := range keepIdx {
		s := scoreResult.Scores[i].Value
		scores[idx] = s
		if s >= threshold {
			survivors = append(survivors, survivor{cand: pool[idx], aiScore: s})
		}
	}
	if len(survivors) == 0 {
		log.Printf("detector: bird-finalize — %d candidates, none passed threshold %d (scores: %v); dropping sighting",
			len(pool), threshold, scores)
		return
	}

	// Sort survivors by AI score DESC for stable index ordering.
	sort.Slice(survivors, func(i, j int) bool {
		if survivors[i].aiScore != survivors[j].aiScore {
			return survivors[i].aiScore > survivors[j].aiScore
		}
		return survivors[i].cand.classifierTopConf > survivors[j].cand.classifierTopConf
	})

	// Pick parent frame: among ALL session candidates (not just
	// survivors), the parentID with the highest parentBirdCount.
	// Tiebreak: highest mean classifier top-conf among that frame's
	// candidates. This rewards the frame that most fully showed the
	// flock at its peak, even if that frame's individual crops didn't
	// happen to make it through AI quality.
	parentSummary := map[int]struct {
		count int
		sumCC float64
		n     int
		jpeg  []byte
	}{}
	for _, c := range sess.candidates {
		ps := parentSummary[c.parentID]
		ps.count = c.parentBirdCount
		ps.sumCC += c.classifierTopConf
		ps.n++
		if ps.jpeg == nil {
			ps.jpeg = c.parentJpeg
		}
		parentSummary[c.parentID] = ps
	}
	type parentRank struct {
		id    int
		count int
		mean  float64
		jpeg  []byte
	}
	var parents []parentRank
	for id, ps := range parentSummary {
		mean := 0.0
		if ps.n > 0 {
			mean = ps.sumCC / float64(ps.n)
		}
		parents = append(parents, parentRank{id: id, count: ps.count, mean: mean, jpeg: ps.jpeg})
	}
	sort.Slice(parents, func(i, j int) bool {
		if parents[i].count != parents[j].count {
			return parents[i].count > parents[j].count
		}
		return parents[i].mean > parents[j].mean
	})
	parent := parents[0]

	// Persist the parent frame as the picture .jpg.
	topSpecies := ""
	topConf := 0.0
	for _, s := range survivors {
		if len(s.cand.classifierGuesses) > 0 && s.cand.classifierGuesses[0].Confidence > topConf {
			topSpecies = s.cand.classifierGuesses[0].Name
			topConf = s.cand.classifierGuesses[0].Confidence
		}
	}
	pictureName, err := d.captures.Save(parent.jpeg, capture.Reason{Species: topSpecies, Confidence: topConf})
	if err != nil {
		log.Printf("detector: bird-finalize save: %v", err)
		return
	}

	// Save each survivor's crop as an indexed sidecar; build the
	// BirdCrops slice for the metadata row.
	birdCrops := make([]capture.BirdCropInfo, 0, len(survivors))
	for i, s := range survivors {
		fname, err := d.captures.WriteCropImageIndexed(pictureName, i, s.cand.cropImg)
		if err != nil {
			log.Printf("detector: bird-finalize write crop %d: %v", i, err)
			continue
		}
		species := make([]capture.SpeciesGuess, 0, len(s.cand.classifierGuesses))
		for _, g := range s.cand.classifierGuesses {
			species = append(species, capture.SpeciesGuess{Name: g.Name, Confidence: g.Confidence})
		}
		birdCrops = append(birdCrops, capture.BirdCropInfo{
			Filename: fname,
			Species:  species,
			AIScore:  s.aiScore,
			YOLOConf: s.cand.yoloConf,
			Box:      s.cand.box,
		})
	}
	if len(birdCrops) == 0 {
		log.Printf("detector: bird-finalize — all crop writes failed for %s", pictureName)
		return
	}

	// Use the highest AI score across surviving crops as the picture-
	// level summary score, so the gallery's KPI reflects the best
	// crop's quality.
	bestAI := birdCrops[0].AIScore
	now := time.Now()
	md := capture.Metadata{
		AnalyzedAt:     &now,
		BirdCrops:      birdCrops,
		AIQualityScore: &bestAI,
		AIQualityAt:    &now,
		AIQualityRaw:   scoreResult.RawResponse,
	}
	if err := d.captures.WriteMetadata(pictureName, md); err != nil {
		log.Printf("detector: bird-finalize write metadata %s: %v", pictureName, err)
		return
	}

	log.Printf("detector: bird-finalize %s — pooled %d, kept %d/%d (scores: %v, parent_id=%d, parent_birds=%d, top species=%q@%.0f%%)",
		pictureName, len(sess.candidates), len(birdCrops), len(pool), scores, parent.id, parent.count, topSpecies, topConf*100)
}

// BirdReclassifyResult is what the manual-reclassify HTTP handler
// surfaces back to the frontend: the new BirdCrops set, every crop's
// AI score, and the discard threshold so the UI can decide whether
// to prompt the user to delete a sighting that no longer has any
// surviving birds.
type BirdReclassifyResult struct {
	BirdCrops   []capture.BirdCropInfo
	AllScores   []int
	Threshold   int
	BestAIScore int
}

// ReclassifyBird re-runs the bird pipeline against an existing saved
// picture, replacing its crop sidecars + BirdCrops metadata in place.
// Used by the manual reclassify button (per-picture and bulk).
//
// Behavior:
//   - YOLO + classifier extract every bird crop in the saved frame.
//   - Top BirdMaxCrops by classifier confidence go to AI quality.
//   - Crops at or above DiscardThreshold survive; the rest are dropped.
//   - On any survivors, BirdCrops + crop sidecars are replaced fresh.
//   - On zero survivors:
//       destructive=false → existing metadata is preserved (safer for
//         the per-picture button — a model miss doesn't trash valid
//         legacy data).
//       destructive=true  → bird-related fields and crop files are
//         wiped (for the bulk "start fresh" workflow where the user
//         explicitly accepts the destructive semantics).
//   - The picture file itself is not renamed or removed in either case.
func (d *Detector) ReclassifyBird(ctx context.Context, name string, destructive bool) (BirdReclassifyResult, error) {
	result := BirdReclassifyResult{}
	p, err := d.captures.Path(name)
	if err != nil {
		return result, err
	}
	body, err := os.ReadFile(p)
	if err != nil {
		return result, fmt.Errorf("read picture: %w", err)
	}
	img, _, err := image.Decode(bytes.NewReader(body))
	if err != nil {
		return result, fmt.Errorf("decode: %w", err)
	}

	cfg := d.cfgStore.Get()
	threshold := cfg.BirdConfidenceThreshold
	if threshold <= 0 {
		threshold = 0.30
	}
	maxCrops := cfg.BirdMaxCrops
	if maxCrops <= 0 {
		maxCrops = 3
	}
	aiThreshold := cfg.AIQuality.DiscardThreshold
	result.Threshold = aiThreshold

	d.ortMu.Lock()
	// Run YOLO inference on the saved frame for every loaded session
	// before reading their output buffers in collectBirdCandidatesLocked.
	// Without this, birdBoxes() reads stale output from whatever the
	// live tick last ran on — usually empty for a paused detector,
	// always wrong for a different image — and every reclassify
	// silently returns "no birds."
	for _, s := range []*yoloSession{d.primary, d.secondary} {
		if s == nil {
			continue
		}
		if err := writeImageToTensor(img, s.input, s.inputSize); err != nil {
			log.Printf("detector: reclassify-bird prep %s: %v", s.path, err)
			continue
		}
		if err := s.session.Run(); err != nil {
			log.Printf("detector: reclassify-bird run %s: %v", s.path, err)
			continue
		}
	}
	cands := d.collectBirdCandidatesLocked(img, body, threshold, time.Now())
	d.ortMu.Unlock()

	if len(cands) == 0 {
		if destructive {
			if err := d.wipeBirdMetadata(name); err != nil {
				return result, err
			}
			log.Printf("detector: reclassify-bird %s — no birds found at conf >= %.2f (destructive wipe)",
				name, threshold)
		} else {
			log.Printf("detector: reclassify-bird %s — no birds found at conf >= %.2f (preserved)",
				name, threshold)
		}
		return result, nil
	}

	sort.Slice(cands, func(i, j int) bool {
		return cands[i].classifierTopConf > cands[j].classifierTopConf
	})
	if len(cands) > maxCrops {
		cands = cands[:maxCrops]
	}

	svc := d.currentAIQuality()
	if svc == nil {
		return result, fmt.Errorf("AI quality not configured — bird reclassify requires it")
	}

	encoded := make([][]byte, 0, len(cands))
	keepIdx := make([]int, 0, len(cands))
	for i, c := range cands {
		var buf bytes.Buffer
		if err := imgjpeg.Encode(&buf, c.cropImg, &imgjpeg.Options{Quality: 85}); err != nil {
			log.Printf("detector: reclassify-bird encode crop %d: %v", i, err)
			continue
		}
		encoded = append(encoded, buf.Bytes())
		keepIdx = append(keepIdx, i)
	}
	if len(encoded) == 0 {
		return result, fmt.Errorf("no encodable crops")
	}

	scoreResult, err := svc.ScoreWithRaw(ctx, "Bird", 0.5, encoded)
	if err != nil {
		return result, fmt.Errorf("score crops: %w", err)
	}

	scores := make([]int, len(cands))
	type survivor struct {
		cand    birdCandidate
		aiScore int
	}
	var survivors []survivor
	for i, idx := range keepIdx {
		s := scoreResult.Scores[i].Value
		scores[idx] = s
		if s >= aiThreshold {
			survivors = append(survivors, survivor{cand: cands[idx], aiScore: s})
		}
	}
	result.AllScores = scores

	if len(survivors) == 0 {
		if destructive {
			if err := d.wipeBirdMetadata(name); err != nil {
				return result, err
			}
			log.Printf("detector: reclassify-bird %s — no crop passed threshold %d (scores: %v, destructive wipe)",
				name, aiThreshold, scores)
		} else {
			log.Printf("detector: reclassify-bird %s — no crop passed threshold %d (scores: %v, preserved)",
				name, aiThreshold, scores)
		}
		return result, nil
	}

	sort.Slice(survivors, func(i, j int) bool {
		if survivors[i].aiScore != survivors[j].aiScore {
			return survivors[i].aiScore > survivors[j].aiScore
		}
		return survivors[i].cand.classifierTopConf > survivors[j].cand.classifierTopConf
	})

	// Wipe all existing crop files before writing fresh ones — avoids
	// leftover indexed files from a previous reclassify when this one
	// produces fewer survivors.
	d.captures.RemoveAllCrops(name)

	birdCrops := make([]capture.BirdCropInfo, 0, len(survivors))
	for i, s := range survivors {
		fname, err := d.captures.WriteCropImageIndexed(name, i, s.cand.cropImg)
		if err != nil {
			log.Printf("detector: reclassify-bird write crop %d: %v", i, err)
			continue
		}
		species := make([]capture.SpeciesGuess, 0, len(s.cand.classifierGuesses))
		for _, g := range s.cand.classifierGuesses {
			species = append(species, capture.SpeciesGuess{Name: g.Name, Confidence: g.Confidence})
		}
		birdCrops = append(birdCrops, capture.BirdCropInfo{
			Filename: fname,
			Species:  species,
			AIScore:  s.aiScore,
			YOLOConf: s.cand.yoloConf,
			Box:      s.cand.box,
		})
	}
	if len(birdCrops) == 0 {
		return result, fmt.Errorf("all crop writes failed")
	}

	bestAI := birdCrops[0].AIScore
	now := time.Now()
	md, _, _ := d.captures.ReadMetadata(name)
	md.BirdCrops = birdCrops
	md.BirdCrop = ""    // legacy
	md.BirdSpecies = nil // legacy
	md.AIQualityScore = &bestAI
	md.AIQualityAt = &now
	md.AIQualityError = ""
	md.AIQualityRaw = scoreResult.RawResponse
	md.ReclassifiedAt = &now
	if err := d.captures.WriteMetadata(name, md); err != nil {
		return result, err
	}

	result.BirdCrops = birdCrops
	result.BestAIScore = bestAI
	topSpecies := ""
	topConf := 0.0
	if len(birdCrops[0].Species) > 0 {
		topSpecies = birdCrops[0].Species[0].Name
		topConf = birdCrops[0].Species[0].Confidence
	}
	log.Printf("detector: reclassify-bird %s — kept %d/%d (scores: %v, top species=%q@%.0f%%)",
		name, len(birdCrops), len(cands), scores, topSpecies, topConf*100)
	return result, nil
}

// RescoreBirdCrops re-runs the AI quality service against the existing
// crop files for a multi-crop bird sighting, updating each crop's
// AIScore in place. Crop files are not re-extracted — this is a
// cheaper "what does the AI think now?" pass that preserves the
// current crop selection.
//
// Returns the new per-crop scores in BirdCrops order plus the highest
// score (used as the picture-level AIQualityScore).
func (d *Detector) RescoreBirdCrops(ctx context.Context, name string) ([]int, int, error) {
	md, ok, _ := d.captures.ReadMetadata(name)
	if !ok || len(md.BirdCrops) == 0 {
		return nil, 0, fmt.Errorf("no bird crops on this picture")
	}
	svc := d.currentAIQuality()
	if svc == nil {
		return nil, 0, fmt.Errorf("AI quality not configured")
	}

	encoded := make([][]byte, 0, len(md.BirdCrops))
	keepIdx := make([]int, 0, len(md.BirdCrops))
	for i, c := range md.BirdCrops {
		if c.Filename == "" {
			continue
		}
		path := filepath.Join(d.captures.Dir(), c.Filename)
		body, err := os.ReadFile(path)
		if err != nil {
			log.Printf("detector: rescore read crop %d (%s): %v", i, c.Filename, err)
			continue
		}
		encoded = append(encoded, body)
		keepIdx = append(keepIdx, i)
	}
	if len(encoded) == 0 {
		return nil, 0, fmt.Errorf("no readable crop files on disk")
	}

	scoreResult, err := svc.ScoreWithRaw(ctx, "Bird", 0.5, encoded)
	if err != nil {
		return nil, 0, fmt.Errorf("score crops: %w", err)
	}

	scores := make([]int, len(md.BirdCrops))
	bestAI := 0
	for i, idx := range keepIdx {
		s := scoreResult.Scores[i].Value
		scores[idx] = s
		md.BirdCrops[idx].AIScore = s
		if s > bestAI {
			bestAI = s
		}
	}

	now := time.Now()
	md.AIQualityScore = &bestAI
	md.AIQualityAt = &now
	md.AIQualityError = ""
	md.AIQualityRaw = scoreResult.RawResponse
	if err := d.captures.WriteMetadata(name, md); err != nil {
		return scores, bestAI, err
	}
	log.Printf("detector: rescore %s — scores=%v best=%d", name, scores, bestAI)
	return scores, bestAI, nil
}

// wipeBirdMetadata clears every bird-related field on a picture's
// metadata row and removes its crop sidecars from disk. Called by the
// destructive reclassify path when the bulk "start fresh" workflow is
// running and a picture didn't yield any surviving birds.
func (d *Detector) wipeBirdMetadata(name string) error {
	d.captures.RemoveAllCrops(name)
	md, _, _ := d.captures.ReadMetadata(name)
	md.BirdCrops = nil
	md.BirdCrop = ""
	md.BirdSpecies = nil
	md.AIQualityScore = nil
	md.AIQualityAt = nil
	md.AIQualityError = ""
	md.AIQualityRaw = ""
	now := time.Now()
	md.ReclassifiedAt = &now
	return d.captures.WriteMetadata(name, md)
}

// recordAIQualityError persists the last scoring failure on the
// picture's metadata so the gallery can flag it. The picture stays
// on disk; only the badge is added.
func (d *Detector) recordAIQualityError(name, errStr string) {
	md, _, err := d.captures.ReadMetadata(name)
	if err != nil {
		log.Printf("detector: read metadata for ai error %s: %v", name, err)
		return
	}
	md.AIQualityError = errStr
	if err := d.captures.WriteMetadata(name, md); err != nil {
		log.Printf("detector: write ai error %s: %v", name, err)
	}
}

// persistAnalysis writes the detector + classifier output to the
// picture's metadata row and saves the JPEG-encoded crop image that
// fed the bird classifier (when present). Preserves user-edited
// fields (UserSpecies, UserNotes) and the AI quality score; only the
// analysis-derived fields are overwritten.
func (d *Detector) persistAnalysis(name string, detections []Detection, birdCrop image.Image, now time.Time) error {
	md, _, _ := d.captures.ReadMetadata(name)
	md.AnalyzedAt = &now
	md.Detections = md.Detections[:0]
	for _, det := range detections {
		if det.Box == nil {
			continue
		}
		md.Detections = append(md.Detections, capture.DetectionRecord{
			Name:       det.Name,
			Confidence: det.Confidence,
			Box: capture.BBox{
				X1: det.Box.X1, Y1: det.Box.Y1, X2: det.Box.X2, Y2: det.Box.Y2,
			},
		})
	}
	md.BirdSpecies = nil
	for _, det := range detections {
		if normalizeClass(det.Name) == "bird" && len(det.Species) > 0 {
			md.BirdSpecies = make([]capture.SpeciesGuess, len(det.Species))
			for i, g := range det.Species {
				md.BirdSpecies[i] = capture.SpeciesGuess{Name: g.Name, Confidence: g.Confidence}
			}
			break
		}
	}
	md.BirdCrop = ""
	if birdCrop != nil {
		if cn, err := d.captures.WriteCropImage(name, birdCrop); err == nil {
			md.BirdCrop = cn
		} else {
			log.Printf("detector: write crop: %v", err)
		}
	}
	return d.captures.WriteMetadata(name, md)
}

// AnalyzeFrame runs the full YOLO + classifier pipeline against the given
// JPEG bytes and returns the detections (with bboxes) plus the highest-
// confidence bird crop image (or nil). Used to (re)attach analysis to a
// saved picture from outside the live tick loop.
func (d *Detector) AnalyzeFrame(jpegBytes []byte) ([]Detection, image.Image, error) {
	if !d.ready.Load() {
		return nil, nil, fmt.Errorf("detector not ready")
	}
	img, _, err := image.Decode(bytes.NewReader(jpegBytes))
	if err != nil {
		return nil, nil, fmt.Errorf("decode: %w", err)
	}

	d.ortMu.Lock()
	defer d.ortMu.Unlock()

	cfg := d.cfgStore.Get()
	var detections []Detection
	for _, s := range []*yoloSession{d.primary, d.secondary} {
		if s == nil {
			continue
		}
		ws := d.watchSet(s, cfg.WatchedAnimals)
		if len(ws) == 0 {
			// Reclassify-style fallback: if the watchlist doesn't include
			// "bird" but a bird-only session exists, still scan for birds
			// so callers like /reclassify get useful output.
			ws = d.birdOnlyWatchSet(s)
		}
		if len(ws) == 0 {
			continue
		}
		r, err := d.runSessionForWatchlist(s, img, ws)
		if err != nil {
			log.Printf("detector: AnalyzeFrame %q: %v", s.path, err)
			continue
		}
		detections = append(detections, r...)
	}
	detections = mergeByName(detections)
	birdCrop, birdBox := d.classifyBirds(img, detections)
	if birdBox != nil {
		setBirdBox(detections, birdBox)
	}
	sort.Slice(detections, func(i, j int) bool { return detections[i].Confidence > detections[j].Confidence })
	return detections, birdCrop, nil
}


// watchEntry is a resolved watched-class entry: the model's canonical class
// name plus the per-animal threshold.
type watchEntry struct {
	name      string
	threshold float32
}

// watchSet resolves the user-specified watched animals to the session-local
// class IDs whose canonical names match. Same animal may appear with
// different IDs in primary vs secondary sessions; runSessionForWatchlist
// uses each per-session set.
func (d *Detector) watchSet(s *yoloSession, watched []config.WatchedAnimal) map[int]watchEntry {
	if len(watched) == 0 || s == nil {
		return nil
	}
	want := make(map[string]float32, len(watched))
	for _, w := range watched {
		t := float32(w.Threshold)
		if t < 0.05 {
			t = 0.35
		}
		want[normalizeClass(w.Name)] = t
	}
	out := make(map[int]watchEntry, len(watched))
	for id, name := range s.canonical {
		if t, ok := want[normalizeClass(name)]; ok {
			out[id] = watchEntry{name: name, threshold: t}
		}
	}
	return out
}

// runSessionForWatchlist does a per-class max scan for the given watchlist
// and also fills in the best-anchor bbox for each detection. Caller holds
// ortMu. No NMS — production path just needs the dominant detection per
// watched species.
func (d *Detector) runSessionForWatchlist(s *yoloSession, img image.Image, watch map[int]watchEntry) ([]Detection, error) {
	if err := writeImageToTensor(img, s.input, s.inputSize); err != nil {
		return nil, fmt.Errorf("prep: %w", err)
	}
	if err := s.session.Run(); err != nil {
		return nil, fmt.Errorf("run: %w", err)
	}
	if len(watch) == 0 {
		return nil, nil
	}
	out := s.output.GetData()
	if len(out) < (4+s.numClasses)*s.numPreds {
		return nil, fmt.Errorf("output size mismatch")
	}
	sz := float32(s.inputSize)
	res := make([]Detection, 0, len(watch))
	for id, entry := range watch {
		row := out[s.numPreds*(id+4) : s.numPreds*(id+5)]
		var best float32
		bestIdx := -1
		for i, v := range row {
			if v > best {
				best = v
				bestIdx = i
			}
		}
		if best < entry.threshold || bestIdx < 0 {
			continue
		}
		cx := out[s.numPreds*0+bestIdx]
		cy := out[s.numPreds*1+bestIdx]
		w := out[s.numPreds*2+bestIdx]
		h := out[s.numPreds*3+bestIdx]
		res = append(res, Detection{
			ClassID:    id,
			Name:       entry.name,
			Confidence: float64(best),
			Box: &BBox{
				X1: clamp01(float64((cx - w/2) / sz)),
				Y1: clamp01(float64((cy - h/2) / sz)),
				X2: clamp01(float64((cx + w/2) / sz)),
				Y2: clamp01(float64((cy + h/2) / sz)),
			},
		})
	}
	return res, nil
}

// ReanalyzeAndAttach runs the full analysis pipeline against an existing
// saved picture's JPEG bytes and persists the results (detections, bird-
// species guesses, the crop image) as the picture's sidecar metadata.
// Replaces what the older ReclassifyImage helper used to do, but also writes
// boxes and the crop sub-image so the gallery modal can review them.
func (d *Detector) ReanalyzeAndAttach(name string, jpegBytes []byte) ([]Detection, error) {
	dets, crop, err := d.AnalyzeFrame(jpegBytes)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if err := d.persistAnalysis(name, dets, crop, now); err != nil {
		return dets, err
	}
	// Track that this happened explicitly (vs. the implicit AnalyzedAt set
	// by the live capture path).
	if md, ok, _ := d.captures.ReadMetadata(name); ok {
		md.ReclassifiedAt = &now
		_ = d.captures.WriteMetadata(name, md)
	}
	return dets, nil
}

// ReanalyzeNonBird runs YOLO on a saved non-bird picture (fox, deer,
// cat, dog, person, etc.) and persists only the detections and
// timestamps. The bird classifier and AI quality service are NOT
// called — those are bird-specific and produce nonsense output on a
// frame the user already knows isn't a bird (the classifier
// hallucinates a species onto whatever's most bird-shaped in the
// frame; the AI quality scorer's prompt asks "is there a bird?" and
// returns 0 for everything else).
//
// Existing bird-related metadata fields (BirdCrops, BirdSpecies,
// BirdCrop, AIQualityScore) are explicitly cleared — this picture
// being processed as non-bird means any prior misclassification can
// go.
func (d *Detector) ReanalyzeNonBird(name string, jpegBytes []byte) ([]Detection, error) {
	if !d.ready.Load() {
		return nil, fmt.Errorf("detector not ready")
	}
	img, _, err := image.Decode(bytes.NewReader(jpegBytes))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	d.ortMu.Lock()
	defer d.ortMu.Unlock()
	cfg := d.cfgStore.Get()
	var detections []Detection
	for _, s := range []*yoloSession{d.primary, d.secondary} {
		if s == nil {
			continue
		}
		ws := d.watchSet(s, cfg.WatchedAnimals)
		if len(ws) == 0 {
			continue
		}
		r, err := d.runSessionForWatchlist(s, img, ws)
		if err != nil {
			log.Printf("detector: ReanalyzeNonBird %q: %v", s.path, err)
			continue
		}
		detections = append(detections, r...)
	}
	detections = mergeByName(detections)
	sort.Slice(detections, func(i, j int) bool { return detections[i].Confidence > detections[j].Confidence })

	md, _, _ := d.captures.ReadMetadata(name)
	now := time.Now()
	md.AnalyzedAt = &now
	md.ReclassifiedAt = &now
	md.Detections = md.Detections[:0]
	for _, det := range detections {
		if det.Box == nil {
			continue
		}
		md.Detections = append(md.Detections, capture.DetectionRecord{
			Name:       det.Name,
			Confidence: det.Confidence,
			Box: capture.BBox{
				X1: det.Box.X1, Y1: det.Box.Y1, X2: det.Box.X2, Y2: det.Box.Y2,
			},
		})
	}
	// Clear bird-specific fields so a previous misfire doesn't linger.
	md.BirdCrops = nil
	md.BirdCrop = ""
	md.BirdSpecies = nil
	md.AIQualityScore = nil
	md.AIQualityAt = nil
	md.AIQualityError = ""
	md.AIQualityRaw = ""
	d.captures.RemoveAllCrops(name)
	if err := d.captures.WriteMetadata(name, md); err != nil {
		return detections, err
	}
	return detections, nil
}

// QualityResult is the outcome of running the AI image-quality service on
// a single saved picture (manual reclassify path). When Enabled is false
// the score and threshold are zero — caller should skip the prompt.
type QualityResult struct {
	Enabled     bool
	Score       int
	Threshold   int
	RawResponse string // raw model content; surfaced to the Debug tab
}

// QualityScore evaluates one JPEG against the configured AI quality
// service. Used by the manual reclassify flow so the UI can prompt the
// user to delete a low-scoring picture.
//
// Result.Enabled means "AI scoring is configured AND was attempted" —
// independent of whether the call succeeded. The caller pairs that with
// the returned error to distinguish three states:
//   - Enabled=false, err=nil  — scoring not configured; do nothing
//   - Enabled=true,  err!=nil — scoring tried, upstream failed; report
//   - Enabled=true,  err=nil  — Result.Score / Threshold are usable
func (d *Detector) QualityScore(ctx context.Context, jpegBytes []byte, species string, confidence float64) (QualityResult, error) {
	svc := d.currentAIQuality()
	cfg := d.cfgStore.Get().AIQuality
	if svc == nil || !cfg.Enabled {
		return QualityResult{}, nil
	}
	// Configured — Enabled=true on every return path past this point.
	result := QualityResult{Enabled: true, Threshold: cfg.DiscardThreshold}
	width := cfg.NormalizeWidth
	if width <= 0 {
		width = 1024
	}
	resized, err := aiquality.Resize(jpegBytes, width)
	if err != nil {
		return result, fmt.Errorf("resize: %w", err)
	}
	r, err := svc.ScoreWithRaw(ctx, species, confidence, [][]byte{resized})
	result.RawResponse = r.RawResponse
	if err != nil {
		return result, err
	}
	if len(r.Scores) == 0 {
		return result, fmt.Errorf("no scores returned")
	}
	result.Score = r.Scores[0].Value
	return result, nil
}

// birdOnlyWatchSet builds a watchSet for one session containing only its
// bird-canonical class IDs at a low default threshold. Used by ReclassifyImage.
func (d *Detector) birdOnlyWatchSet(s *yoloSession) map[int]watchEntry {
	out := make(map[int]watchEntry)
	for id, name := range s.canonical {
		if normalizeClass(name) == "bird" {
			out[id] = watchEntry{name: name, threshold: 0.25}
		}
	}
	return out
}

// DebugDetect runs inference on the latest frame across every loaded session
// and returns the top-K anchor-first detections with bboxes — regardless of
// the watchlist. Used by the debug panel / overlay on the Live view.
func (d *Detector) DebugDetect(threshold float32, topK int) ([]Detection, error) {
	if !d.ready.Load() {
		return nil, fmt.Errorf("detector not ready")
	}
	frame, _ := d.extractor.Latest()
	if frame == nil {
		return nil, fmt.Errorf("no frame available yet")
	}
	img, _, err := image.Decode(bytes.NewReader(frame))
	if err != nil {
		return nil, fmt.Errorf("decode frame: %w", err)
	}

	d.ortMu.Lock()
	defer d.ortMu.Unlock()

	var all []Detection
	if d.primary != nil {
		r, err := d.runSessionForDebug(d.primary, img, threshold)
		if err != nil {
			return nil, fmt.Errorf("primary: %w", err)
		}
		all = append(all, r...)
	}
	if d.secondary != nil {
		r, err := d.runSessionForDebug(d.secondary, img, threshold)
		if err != nil {
			log.Printf("detector: secondary debug: %v (continuing)", err)
		} else {
			all = append(all, r...)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Confidence > all[j].Confidence })
	if topK > 0 && len(all) > topK {
		all = all[:topK]
	}
	return all, nil
}

// runSessionForDebug does an anchor-first scan with NMS, returning every
// detection above threshold with its normalized bbox. Caller holds ortMu.
func (d *Detector) runSessionForDebug(s *yoloSession, img image.Image, threshold float32) ([]Detection, error) {
	if err := writeImageToTensor(img, s.input, s.inputSize); err != nil {
		return nil, fmt.Errorf("prep: %w", err)
	}
	if err := s.session.Run(); err != nil {
		return nil, fmt.Errorf("run: %w", err)
	}
	out := s.output.GetData()
	if len(out) < (4+s.numClasses)*s.numPreds {
		return nil, fmt.Errorf("output size mismatch")
	}
	boxes := make([]box, 0, 64)
	for a := 0; a < s.numPreds; a++ {
		var bestCls int
		var bestConf float32
		for c := 0; c < s.numClasses; c++ {
			v := out[s.numPreds*(c+4)+a]
			if v > bestConf {
				bestConf = v
				bestCls = c
			}
		}
		if bestConf < threshold {
			continue
		}
		cx := out[s.numPreds*0+a]
		cy := out[s.numPreds*1+a]
		w := out[s.numPreds*2+a]
		h := out[s.numPreds*3+a]
		boxes = append(boxes, box{
			x1:         cx - w/2,
			y1:         cy - h/2,
			x2:         cx + w/2,
			y2:         cy + h/2,
			confidence: bestConf,
			classID:    bestCls,
		})
	}
	kept := nms(boxes, 0.5)
	sz := float64(s.inputSize)
	res := make([]Detection, 0, len(kept))
	for _, b := range kept {
		res = append(res, Detection{
			ClassID:    b.classID,
			Name:       s.canonical[b.classID],
			Confidence: float64(b.confidence),
			Box: &BBox{
				X1: clamp01(float64(b.x1) / sz),
				Y1: clamp01(float64(b.y1) / sz),
				X2: clamp01(float64(b.x2) / sz),
				Y2: clamp01(float64(b.y2) / sz),
			},
		})
	}
	return res, nil
}

// classifyBirds finds bird boxes in the most recent YOLO output, crops them
// from the original full-resolution frame, optionally runs the species
// classifier, and aggregates the top guesses onto the existing "Bird"
// detection. Returns the highest-confidence bird crop image AND its
// normalized [0,1] bbox so callers can keep the modal overlay aligned
// with the saved crop. Returns (nil, nil) when no bird crop is found.
// Caller holds ortMu.
func (d *Detector) classifyBirds(img image.Image, detections []Detection) (image.Image, *capture.BBox) {
	if len(detections) == 0 {
		return nil, nil
	}
	birdIdx := -1
	for i := range detections {
		if normalizeClass(detections[i].Name) == "bird" {
			birdIdx = i
			break
		}
	}
	if birdIdx < 0 {
		return nil, nil
	}

	// Pick whichever session detected the bird most confidently. The crop-
	// selection threshold mirrors the user's bird-watchlist threshold (with
	// a safety floor of 0.15) so we don't *fail to classify* a bird that
	// already cleared the watchlist gate.
	cropThreshold := float32(0.15)
	cfg := d.cfgStore.Get()
	for _, w := range cfg.WatchedAnimals {
		if normalizeClass(w.Name) == "bird" {
			t := float32(w.Threshold)
			if t > 0 && t < cropThreshold {
				cropThreshold = t
			}
			break
		}
	}
	if cropThreshold > 0.4 {
		cropThreshold = 0.4
	}
	var sess *yoloSession
	var birdBxs []box
	for _, candidate := range []*yoloSession{d.primary, d.secondary} {
		if candidate == nil {
			continue
		}
		bxs := d.birdBoxes(candidate, cropThreshold, 3)
		if len(bxs) == 0 {
			continue
		}
		if len(birdBxs) == 0 || bxs[0].confidence > birdBxs[0].confidence {
			birdBxs = bxs
			sess = candidate
		}
	}
	if sess == nil || len(birdBxs) == 0 {
		return nil, nil
	}

	srcW := img.Bounds().Dx()
	srcH := img.Bounds().Dy()
	scaleX := float64(srcW) / float64(sess.inputSize)
	scaleY := float64(srcH) / float64(sess.inputSize)
	const padFrac = 0.15
	const minCropPx = 64

	// Aggregate: weight each crop's species probabilities by the box's YOLO
	// confidence so the most prominent bird dominates.
	var bestCrop image.Image
	var bestBox *capture.BBox
	speciesSum := make(map[string]float64)
	totalWeight := 0.0
	for _, bx := range birdBxs {
		w := float64(bx.x2-bx.x1) * (1 + 2*padFrac)
		h := float64(bx.y2-bx.y1) * (1 + 2*padFrac)
		cx := float64(bx.x1+bx.x2) / 2
		cy := float64(bx.y1+bx.y2) / 2
		x1 := int((cx - w/2) * scaleX)
		y1 := int((cy - h/2) * scaleY)
		x2 := int((cx + w/2) * scaleX)
		y2 := int((cy + h/2) * scaleY)
		if x1 < 0 {
			x1 = 0
		}
		if y1 < 0 {
			y1 = 0
		}
		if x2 > srcW {
			x2 = srcW
		}
		if y2 > srcH {
			y2 = srcH
		}
		if (x2-x1) < minCropPx || (y2-y1) < minCropPx {
			continue
		}
		crop := image.NewRGBA(image.Rect(0, 0, x2-x1, y2-y1))
		draw.Draw(crop, crop.Bounds(), img, image.Point{X: x1, Y: y1}, draw.Src)
		// First crop iterated is the highest-confidence one (birdBoxes is
		// sorted desc); remember it so callers can persist the actual sub-
		// image that fed the classifier — and its bbox, so the modal's
		// detection overlay matches the saved crop.
		if bestCrop == nil {
			bestCrop = crop
			bestBox = &capture.BBox{
				X1: float64(x1) / float64(srcW),
				Y1: float64(y1) / float64(srcH),
				X2: float64(x2) / float64(srcW),
				Y2: float64(y2) / float64(srcH),
			}
		}
		// Skip the actual species classifier when it's not loaded — we
		// still return the crop so the modal can show what was extracted.
		if d.birdClassif == nil {
			continue
		}

		guesses, err := d.birdClassif.Classify(crop, 5)
		if err != nil {
			log.Printf("detector: classifier: %v", err)
			continue
		}
		// Drop classifier guesses that aren't in the recently-observed
		// local species set when the eBird filter is active. Hidden when
		// inactive (no key, disabled, or empty cache) so we never throw
		// out everything by mistake.
		guesses = d.filterByEBird(guesses)
		weight := float64(bx.confidence)
		totalWeight += weight
		for _, g := range guesses {
			name := d.correctSpeciesName(g.Name)
			speciesSum[name] += g.Confidence * weight
		}
	}
	if totalWeight == 0 || len(speciesSum) == 0 {
		return bestCrop, bestBox
	}
	names := make([]string, 0, len(speciesSum))
	for n := range speciesSum {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return speciesSum[names[i]] > speciesSum[names[j]] })
	// Renormalize so reported confidences sum to ~1.
	var sum float64
	for _, v := range speciesSum {
		sum += v
	}
	const topK = 3
	k := topK
	if k > len(names) {
		k = len(names)
	}
	guesses := make([]classifier.Guess, k)
	for i := 0; i < k; i++ {
		guesses[i] = classifier.Guess{
			Name:       names[i],
			Confidence: speciesSum[names[i]] / sum,
		}
	}
	detections[birdIdx].Species = guesses
	return bestCrop, bestBox
}

// birdBoxes scans the just-completed YOLO output for boxes whose canonical
// class is "bird", performs NMS, and returns up to maxBoxes by descending
// confidence.
func (d *Detector) birdBoxes(s *yoloSession, threshold float32, maxBoxes int) []box {
	if s == nil {
		return nil
	}
	birdIDs := make([]int, 0, 1)
	for i, name := range s.canonical {
		if normalizeClass(name) == "bird" {
			birdIDs = append(birdIDs, i)
		}
	}
	if len(birdIDs) == 0 {
		return nil
	}
	out := s.output.GetData()
	if len(out) < (4+s.numClasses)*s.numPreds {
		return nil
	}
	boxes := make([]box, 0, 32)
	for _, classID := range birdIDs {
		row := out[s.numPreds*(classID+4) : s.numPreds*(classID+5)]
		for a, conf := range row {
			if conf < threshold {
				continue
			}
			cx := out[s.numPreds*0+a]
			cy := out[s.numPreds*1+a]
			w := out[s.numPreds*2+a]
			h := out[s.numPreds*3+a]
			boxes = append(boxes, box{
				x1: cx - w/2, y1: cy - h/2,
				x2: cx + w/2, y2: cy + h/2,
				confidence: conf, classID: classID,
			})
		}
	}
	kept := nms(boxes, 0.5)
	if maxBoxes > 0 && len(kept) > maxBoxes {
		kept = kept[:maxBoxes]
	}
	return kept
}

// mergeByName collapses a flat list of Detections to one entry per canonical
// name, keeping the highest-confidence sample (and its bbox, if any).
func mergeByName(dets []Detection) []Detection {
	if len(dets) == 0 {
		return dets
	}
	byName := make(map[string]Detection, len(dets))
	for _, d := range dets {
		if cur, ok := byName[d.Name]; !ok || d.Confidence > cur.Confidence {
			byName[d.Name] = d
		}
	}
	out := make([]Detection, 0, len(byName))
	for _, d := range byName {
		out = append(out, d)
	}
	return out
}

// setBirdBox overwrites the bird detection's bbox with the bbox of
// the saved bird-classifier crop. The merged detection's pre-existing
// box came from `runSessionForWatchlist`'s primary-session top
// anchor, but the actual saved crop may be from a different session
// or a different rank — the modal overlay needs to point at what was
// saved, not what was first detected.
func setBirdBox(dets []Detection, box *capture.BBox) {
	if box == nil {
		return
	}
	for i := range dets {
		if normalizeClass(dets[i].Name) != "bird" {
			continue
		}
		nb := *box
		dets[i].Box = &BBox{X1: nb.X1, Y1: nb.Y1, X2: nb.X2, Y2: nb.Y2}
		return
	}
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// normalizeClass folds case and whitespace/punctuation variants so that user
// inputs like "Dog", "dog", "deer" match class names like "Deer", "Dog".
func normalizeClass(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b = append(b, c)
		}
	}
	return string(b)
}

// writeImageToTensor resizes (stretch) the input image to inputSize×inputSize
// and writes planar float32 RGB data into the tensor, normalized to [0,1].
func writeImageToTensor(src image.Image, dst *ort.Tensor[float32], inputSize int) error {
	data := dst.GetData()
	channelSize := inputSize * inputSize
	if len(data) < channelSize*3 {
		return fmt.Errorf("tensor too small")
	}

	resized := image.NewRGBA(image.Rect(0, 0, inputSize, inputSize))
	draw.CatmullRom.Scale(resized, resized.Bounds(), src, src.Bounds(), draw.Over, nil)

	r := data[:channelSize]
	g := data[channelSize : channelSize*2]
	b := data[channelSize*2 : channelSize*3]

	i := 0
	pix := resized.Pix
	stride := resized.Stride
	for y := 0; y < inputSize; y++ {
		row := pix[y*stride : y*stride+inputSize*4]
		for x := 0; x < inputSize; x++ {
			px := row[x*4 : x*4+4]
			r[i] = float32(px[0]) / 255.0
			g[i] = float32(px[1]) / 255.0
			b[i] = float32(px[2]) / 255.0
			i++
		}
	}
	return nil
}

// readClassNames opens a temporary session on the model, reads the
// ultralytics-embedded "names" metadata (a Python-dict-style string like
// {0: 'Accordion', 1: 'Adhesive tape', ...}) and returns an index→name slice.
// Must be called after InitializeEnvironment().
func readClassNames(modelPath string, expect int) ([]string, error) {
	md, err := ort.GetModelMetadata(modelPath)
	if err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}
	defer md.Destroy()
	raw, ok, err := md.LookupCustomMetadataMap("names")
	if err != nil {
		return nil, fmt.Errorf("lookup names: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("model has no 'names' metadata")
	}
	return parseNamesMap(raw, expect)
}

var namesEntryRE = regexp.MustCompile(`(\d+)\s*:\s*(?:'([^']*)'|"([^"]*)")`)

func parseNamesMap(raw string, expect int) ([]string, error) {
	matches := namesEntryRE.FindAllStringSubmatch(raw, -1)
	if len(matches) == 0 {
		return nil, fmt.Errorf("no class entries parsed")
	}
	m := make(map[int]string, len(matches))
	for _, g := range matches {
		var idx int
		if _, err := fmt.Sscanf(g[1], "%d", &idx); err != nil {
			continue
		}
		name := g[2]
		if name == "" {
			name = g[3]
		}
		m[idx] = name
	}
	n := expect
	if n == 0 {
		for k := range m {
			if k+1 > n {
				n = k + 1
			}
		}
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		if v, ok := m[i]; ok {
			out[i] = v
		} else {
			out[i] = fmt.Sprintf("class_%d", i)
		}
	}
	return out, nil
}

// -----------------------------------------------------------------------------
// Per-box utilities used by DebugDetect.
// -----------------------------------------------------------------------------

type box struct {
	x1, y1, x2, y2 float32
	confidence     float32
	classID        int
}

func (b box) area() float32 {
	return (b.x2 - b.x1) * (b.y2 - b.y1)
}

func (a box) iou(b box) float32 {
	ix1 := maxf(a.x1, b.x1)
	iy1 := maxf(a.y1, b.y1)
	ix2 := minf(a.x2, b.x2)
	iy2 := minf(a.y2, b.y2)
	iw := maxf(0, ix2-ix1)
	ih := maxf(0, iy2-iy1)
	inter := iw * ih
	union := a.area() + b.area() - inter
	if union <= 0 {
		return 0
	}
	return inter / union
}

func maxf(a, b float32) float32 {
	if a > b {
		return a
	}
	return b
}
func minf(a, b float32) float32 {
	if a < b {
		return a
	}
	return b
}

// filterByEBird drops classifier guesses for species that aren't in the
// eBird-recent set for the user's region. Pass-through when the filter
// is disabled or its cache is empty (so we never accidentally drop
// every guess). When all guesses get filtered out, the original list is
// returned unchanged — better to keep an unlikely guess than zero data.
func (d *Detector) filterByEBird(guesses []classifier.Guess) []classifier.Guess {
	svc := d.currentEBird()
	if svc == nil || !svc.Active() {
		return guesses
	}
	kept := make([]classifier.Guess, 0, len(guesses))
	for _, g := range guesses {
		if svc.Has(g.Name) {
			kept = append(kept, g)
		}
	}
	if len(kept) == 0 {
		return guesses
	}
	return kept
}

// correctSpeciesName runs the user-configured classifier-name corrections
// against name. First match wins; literal rules use case-insensitive
// equality, regex rules are anchored only by what the user wrote (with an
// auto `(?i)` prefix during compilation). Returns the original name when
// no rule matches.
func (d *Detector) correctSpeciesName(name string) string {
	rules, regexes := d.activeCorrections()
	if len(rules) == 0 {
		return name
	}
	trimmed := strings.TrimSpace(name)
	for i, r := range rules {
		if r.Regex {
			if regexes[i] != nil && regexes[i].MatchString(name) {
				return r.Correction
			}
			continue
		}
		if strings.EqualFold(trimmed, strings.TrimSpace(r.Detected)) {
			return r.Correction
		}
	}
	return name
}

// activeCorrections returns the current correction rules + their compiled
// regexes, recompiling the cache when the live config differs from the
// last snapshot. The fast path is a single RWMutex read and a slice-equality
// check — cheap enough to call once per classifier guess.
func (d *Detector) activeCorrections() ([]config.CorrectionRule, []*regexp.Regexp) {
	live := d.cfgStore.Get().ClassifierCorrections

	d.correctMu.RLock()
	if correctionsEqual(live, d.cachedRules) {
		rules, regexes := d.cachedRules, d.cachedRegexes
		d.correctMu.RUnlock()
		return rules, regexes
	}
	d.correctMu.RUnlock()

	d.correctMu.Lock()
	defer d.correctMu.Unlock()
	if correctionsEqual(live, d.cachedRules) {
		return d.cachedRules, d.cachedRegexes
	}
	regexes := make([]*regexp.Regexp, len(live))
	for i, r := range live {
		if !r.Regex || strings.TrimSpace(r.Detected) == "" {
			continue
		}
		re, err := regexp.Compile("(?i)" + r.Detected)
		if err != nil {
			log.Printf("detector: bad correction regex %q (rule %d): %v", r.Detected, i, err)
			continue
		}
		regexes[i] = re
	}
	d.cachedRules = append([]config.CorrectionRule(nil), live...)
	d.cachedRegexes = regexes
	return d.cachedRules, d.cachedRegexes
}

func correctionsEqual(a, b []config.CorrectionRule) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// nms applies greedy non-max suppression.
func nms(boxes []box, iouThresh float32) []box {
	sort.Slice(boxes, func(i, j int) bool { return boxes[i].confidence > boxes[j].confidence })
	kept := make([]box, 0, len(boxes))
	for _, cand := range boxes {
		overlap := false
		for _, k := range kept {
			if cand.iou(k) > iouThresh {
				overlap = true
				break
			}
		}
		if !overlap {
			kept = append(kept, cand)
		}
	}
	return kept
}
