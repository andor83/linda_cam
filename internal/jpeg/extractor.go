package jpeg

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/linda/linda_cam/internal/logutil"
)

// Watchdog tuning. At 2 fps a healthy ffmpeg delivers a frame every 500 ms;
// going stallTimeout without one means the RTSP session is wedged (a
// half-open TCP connection after a camera or router power blip never
// errors out by itself) or the connect is hanging. Either way we kill
// ffmpeg and reconnect.
var (
	stallTimeout = 15 * time.Second
	stallPoll    = time.Second
)

const (
	healthyRun = 60 * time.Second // a run this long resets the retry backoff
	maxBackoff = 30 * time.Second
)

// StaleAfter is how old the latest frame may be before consumers should
// treat the camera as disconnected rather than analyze a frozen image.
const StaleAfter = 10 * time.Second

// Extractor runs a long-lived ffmpeg subprocess that reads the RTSP stream and
// emits JPEG stills on stdout at a fixed rate. The most recent JPEG is held
// in memory so the rest of the app (manual capture, detector) can grab it.
type Extractor struct {
	ffmpegPath string
	fps        int

	mu      sync.RWMutex
	url     string
	cancel  context.CancelFunc
	running bool

	frameMu    sync.RWMutex
	latest     []byte
	latestTime time.Time
	frameSeq   atomic.Uint64
	// lastFrameNano mirrors latestTime for lock-free freshness checks.
	lastFrameNano atomic.Int64
}

func New(ffmpegPath string, fps int) *Extractor {
	if fps <= 0 {
		fps = 2
	}
	return &Extractor{ffmpegPath: ffmpegPath, fps: fps}
}

func (e *Extractor) SetURL(url string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.url == url && e.running {
		return
	}
	e.url = url
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
	if url == "" {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	e.running = true
	go e.loop(ctx, url)
}

func (e *Extractor) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
	e.running = false
	e.lastFrameNano.Store(0)
}

// Connected reports whether a frame arrived within StaleAfter. It turns
// false shortly after the camera drops instead of staying true until ffmpeg
// happens to exit.
func (e *Extractor) Connected() bool {
	return e.FrameAge() < StaleAfter
}

// FrameAge returns how long ago the latest frame arrived, or a very large
// duration if none has.
func (e *Extractor) FrameAge() time.Duration {
	last := e.lastFrameNano.Load()
	if last == 0 {
		return time.Duration(1<<63 - 1)
	}
	return time.Since(time.Unix(0, last))
}

// LastFrameAt returns when the latest frame arrived (zero if never).
func (e *Extractor) LastFrameAt() time.Time {
	last := e.lastFrameNano.Load()
	if last == 0 {
		return time.Time{}
	}
	return time.Unix(0, last)
}

func (e *Extractor) Latest() ([]byte, time.Time) {
	e.frameMu.RLock()
	defer e.frameMu.RUnlock()
	if len(e.latest) == 0 {
		return nil, time.Time{}
	}
	b := make([]byte, len(e.latest))
	copy(b, e.latest)
	return b, e.latestTime
}

func (e *Extractor) FrameSeq() uint64 {
	return e.frameSeq.Load()
}

func (e *Extractor) loop(ctx context.Context, url string) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		err := e.runOnce(ctx, url)
		e.lastFrameNano.Store(0)
		if ctx.Err() != nil {
			return
		}
		// A run that delivered frames for a while was a real connection;
		// don't carry an outage's accumulated backoff into its reconnect.
		if time.Since(started) >= healthyRun {
			backoff = time.Second
		}
		if err != nil {
			log.Printf("jpeg extractor: ffmpeg exited: %v; retrying in %v", err, backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
		}
	}
}

func (e *Extractor) runOnce(ctx context.Context, url string) error {
	args := []string{
		"-loglevel", "error",
		"-rtsp_transport", "tcp",
		"-i", url,
		"-vf", fmt.Sprintf("fps=%d", e.fps),
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"-q:v", "5",
		"pipe:1",
	}
	// runCtx lets the watchdog kill this ffmpeg without stopping the
	// extractor; the loop then reconnects with backoff.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	cmd := exec.CommandContext(runCtx, e.ffmpegPath, args...)
	cmd.WaitDelay = 5 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	sup := &logutil.Suppressor{Prefix: "ffmpeg: "}
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 4096), 1<<20)
		for sc.Scan() {
			sup.Log(sc.Text())
		}
		sup.Flush()
	}()

	// Watchdog: no frame for stallTimeout (from process start if none ever
	// arrived) kills ffmpeg so the loop reconnects.
	var stalled atomic.Bool
	go func() {
		t := time.NewTicker(stallPoll)
		defer t.Stop()
		started := time.Now()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
			}
			last := e.lastFrameNano.Load()
			idle := time.Since(started)
			if last != 0 {
				idle = time.Since(time.Unix(0, last))
			}
			if idle >= stallTimeout {
				log.Printf("jpeg extractor: no frame in %v; restarting ffmpeg", idle.Round(time.Second))
				stalled.Store(true)
				runCancel()
				return
			}
		}
	}()

	reader := newJPEGReader(stdout)
	for {
		frame, err := reader.Next()
		if err != nil {
			_ = cmd.Wait()
			if stalled.Load() {
				return errors.New("stalled (no frames)")
			}
			return err
		}
		now := time.Now()
		e.frameMu.Lock()
		e.latest = frame
		e.latestTime = now
		e.frameMu.Unlock()
		e.lastFrameNano.Store(now.UnixNano())
		e.frameSeq.Add(1)
	}
}

// jpegReader parses a stream of concatenated JPEG frames (SOI..EOI markers)
// produced by ffmpeg's image2pipe+mjpeg muxer.
type jpegReader struct {
	r     *bufio.Reader
	frame []byte
}

func newJPEGReader(r io.Reader) *jpegReader {
	return &jpegReader{r: bufio.NewReaderSize(r, 1<<20), frame: make([]byte, 0, 1<<20)}
}

func (j *jpegReader) Next() ([]byte, error) {
	if err := j.seekSOI(); err != nil {
		return nil, err
	}
	j.frame = j.frame[:0]
	j.frame = append(j.frame, 0xFF, 0xD8)
	for {
		b, err := j.r.ReadByte()
		if err != nil {
			return nil, err
		}
		j.frame = append(j.frame, b)
		if b == 0xFF {
			n, err := j.r.ReadByte()
			if err != nil {
				return nil, err
			}
			j.frame = append(j.frame, n)
			if n == 0xD9 {
				out := make([]byte, len(j.frame))
				copy(out, j.frame)
				return out, nil
			}
		}
	}
}

func (j *jpegReader) seekSOI() error {
	for {
		b, err := j.r.ReadByte()
		if err != nil {
			return err
		}
		if b != 0xFF {
			continue
		}
		n, err := j.r.ReadByte()
		if err != nil {
			return err
		}
		if n == 0xD8 {
			return nil
		}
	}
}

// TestURL runs ffmpeg for a short window and returns nil if at least one
// JPEG frame was produced, otherwise the ffmpeg error.
func TestURL(ffmpegPath, url string, timeout time.Duration) error {
	if url == "" {
		return errors.New("empty url")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := []string{
		"-loglevel", "error",
		"-rtsp_transport", "tcp",
		"-i", url,
		"-frames:v", "1",
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"pipe:1",
	}
	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	reader := newJPEGReader(stdout)
	_, readErr := reader.Next()
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	if readErr != nil {
		return errors.New("no frames received")
	}
	return nil
}
