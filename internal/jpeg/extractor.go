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
)

// Extractor runs a long-lived ffmpeg subprocess that reads the RTSP stream and
// emits JPEG stills on stdout at a fixed rate. The most recent JPEG is held
// in memory so the rest of the app (manual capture, detector) can grab it.
type Extractor struct {
	ffmpegPath string
	fps        int

	mu        sync.RWMutex
	url       string
	cancel    context.CancelFunc
	running   bool
	connected atomic.Bool

	frameMu    sync.RWMutex
	latest     []byte
	latestTime time.Time
	frameSeq   atomic.Uint64
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
	e.connected.Store(false)
}

func (e *Extractor) Connected() bool {
	return e.connected.Load()
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
		err := e.runOnce(ctx, url)
		e.connected.Store(false)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("jpeg extractor: ffmpeg exited: %v; retrying in %v", err, backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
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
	cmd := exec.CommandContext(ctx, e.ffmpegPath, args...)
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

	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 4096), 1<<20)
		for sc.Scan() {
			log.Printf("ffmpeg: %s", sc.Text())
		}
	}()

	reader := newJPEGReader(stdout)
	for {
		frame, err := reader.Next()
		if err != nil {
			_ = cmd.Wait()
			return err
		}
		e.connected.Store(true)
		e.frameMu.Lock()
		e.latest = frame
		e.latestTime = time.Now()
		e.frameMu.Unlock()
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
