package stream

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Streamer runs a long-lived ffmpeg subprocess that pulls the RTSP stream
// and produces an HLS playlist + fMP4 segments into OutDir. Video is encoded
// with h264_nvenc when LINDA_HWACCEL=cuda, otherwise libx264 on CPU.
// Audio is either copied (AAC passthrough) or transcoded to MP3, controlled
// by AudioMode.
type Streamer struct {
	ffmpegPath string
	audioMode  string // "copy" or "mp3"
	outDir     string

	mu        sync.Mutex
	url       string
	cancel    context.CancelFunc
	done      chan struct{}
	running   bool
	connected atomic.Bool
}

func New(ffmpegPath, audioMode, outDir string) *Streamer {
	if audioMode != "mp3" {
		audioMode = "copy"
	}
	return &Streamer{
		ffmpegPath: ffmpegPath,
		audioMode:  audioMode,
		outDir:     outDir,
	}
}

func (s *Streamer) OutDir() string { return s.outDir }

func (s *Streamer) Connected() bool { return s.connected.Load() }

func (s *Streamer) SetURL(url string) {
	s.mu.Lock()
	if s.url == url && s.running {
		s.mu.Unlock()
		return
	}
	s.url = url
	oldCancel, oldDone := s.cancel, s.done
	s.cancel, s.done, s.running = nil, nil, false
	s.mu.Unlock()

	if oldCancel != nil {
		oldCancel()
	}
	if oldDone != nil {
		<-oldDone
	}
	s.connected.Store(false)

	if url == "" {
		return
	}
	if err := resetDir(s.outDir); err != nil {
		log.Printf("stream: reset outDir %s: %v", s.outDir, err)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	s.mu.Lock()
	s.cancel = cancel
	s.done = done
	s.running = true
	s.mu.Unlock()

	go func() {
		defer close(done)
		s.loop(ctx, url)
	}()
}

func (s *Streamer) Stop() {
	s.mu.Lock()
	oldCancel, oldDone := s.cancel, s.done
	s.cancel, s.done, s.running = nil, nil, false
	s.mu.Unlock()

	if oldCancel != nil {
		oldCancel()
	}
	if oldDone != nil {
		<-oldDone
	}
	s.connected.Store(false)
}

func (s *Streamer) loop(ctx context.Context, url string) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := s.runOnce(ctx, url)
		s.connected.Store(false)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("stream: ffmpeg exited: %v; retrying in %v", err, backoff)
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

func (s *Streamer) runOnce(ctx context.Context, url string) error {
	hwaccel := os.Getenv("LINDA_HWACCEL")
	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
	}
	if hwaccel == "cuda" {
		args = append(args, "-hwaccel", "cuda", "-hwaccel_output_format", "cuda")
	}
	args = append(args,
		"-rtsp_transport", "tcp",
		"-i", url,
	)
	if hwaccel == "cuda" {
		args = append(args,
			"-c:v", "h264_nvenc",
			"-preset", "p4",
			"-rc", "vbr",
			"-cq", "23",
			"-b:v", "5M",
			"-maxrate", "8M",
			"-g", "30",
		)
	} else {
		// CPU encode. veryfast keeps a single modern core busy at ~1080p; if
		// the source is 4K and the host CPU can't keep up, ffmpeg will drop
		// frames rather than blocking — visible as A/V drift but not a crash.
		args = append(args,
			"-c:v", "libx264",
			"-preset", "veryfast",
			"-tune", "zerolatency",
			"-crf", "23",
			"-maxrate", "5M",
			"-bufsize", "10M",
			"-g", "30",
			"-pix_fmt", "yuv420p",
		)
	}
	if s.audioMode == "mp3" {
		args = append(args, "-c:a", "libmp3lame", "-b:a", "128k")
	} else {
		// MPEG-TS requires ADTS-framed AAC; the camera's RFC 3640 AAC has
		// no ADTS headers, so "-c:a copy" produces a broken audio track
		// that players silently discard. Re-encoding fixes the framing.
		args = append(args, "-c:a", "aac", "-b:a", "96k", "-ar", "48000", "-ac", "2")
	}
	args = append(args,
		"-f", "hls",
		"-hls_time", "2",
		"-hls_list_size", "6",
		"-hls_flags", "delete_segments+append_list+omit_endlist",
		"-hls_segment_filename", filepath.Join(s.outDir, "seg_%05d.ts"),
		filepath.Join(s.outDir, "stream.m3u8"),
	)

	cmd := exec.CommandContext(ctx, s.ffmpegPath, args...)
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
			line := sc.Text()
			// The first "Opening ..." or "Output #0" line is a good signal
			// that ffmpeg connected; the stream file appearing is better but
			// this is cheap.
			log.Printf("ffmpeg-stream: %s", line)
			s.connected.Store(true)
		}
	}()

	return cmd.Wait()
}

// resetDir removes all entries inside dir (but not dir itself) and ensures
// dir exists. Keeps the parent so a long-lived http.ServeFile handler that
// holds a cached stat of the directory keeps working.
func resetDir(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		_ = os.RemoveAll(filepath.Join(dir, e.Name()))
	}
	return nil
}
