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

	"github.com/linda/linda_cam/internal/logutil"
)

// Watchdog tuning. ffmpeg rewrites the playlist every segment (2 s), so a
// playlist that has not changed in stallTimeout means ffmpeg is wedged —
// typically a half-open TCP session after the camera or router lost power,
// which never errors out on its own. It also covers a connect that hangs.
var (
	stallTimeout = 20 * time.Second
	stallPoll    = 2 * time.Second
)

const (
	liveWindow      = 10 * time.Second // playlist updated this recently => "connected"
	healthyRun      = 60 * time.Second // a run this long resets the retry backoff
	maxBackoff      = 30 * time.Second
	restartAnnounce = "stream: no new HLS segment in %v; restarting ffmpeg"
)

// Streamer runs a long-lived ffmpeg subprocess that pulls the RTSP stream
// and produces an HLS playlist + fMP4 segments into OutDir. Video is encoded
// with h264_nvenc when LINDA_HWACCEL=cuda, h264_videotoolbox when it is
// "videotoolbox" (macOS), otherwise libx264 on CPU.
// Audio is either copied (AAC passthrough) or transcoded to MP3, controlled
// by AudioMode.
type Streamer struct {
	ffmpegPath string
	audioMode  string // "copy" or "mp3"
	outDir     string

	mu      sync.Mutex
	url     string
	cancel  context.CancelFunc
	done    chan struct{}
	running bool

	// lastProgress is the unix-nano time the playlist was last seen to
	// change; zero while no ffmpeg is producing output.
	lastProgress atomic.Int64
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

// Connected reports whether ffmpeg has written a fresh playlist recently.
// It goes false within liveWindow of the camera dropping, instead of staying
// true until the process happens to exit.
func (s *Streamer) Connected() bool {
	last := s.lastProgress.Load()
	return last != 0 && time.Since(time.Unix(0, last)) < liveWindow
}

// LastProgress returns when the HLS output last advanced (zero if never).
func (s *Streamer) LastProgress() time.Time {
	last := s.lastProgress.Load()
	if last == 0 {
		return time.Time{}
	}
	return time.Unix(0, last)
}

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
	s.lastProgress.Store(0)

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
	s.lastProgress.Store(0)
}

func (s *Streamer) loop(ctx context.Context, url string) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		started := time.Now()
		err := s.runOnce(ctx, url)
		s.lastProgress.Store(0)
		if ctx.Err() != nil {
			return
		}
		// A run that streamed for a while before dying was a real
		// connection; don't punish the reconnect with the backoff that
		// accumulated during an earlier outage.
		if time.Since(started) >= healthyRun {
			backoff = time.Second
		}
		if err != nil {
			log.Printf("stream: ffmpeg exited: %v; retrying in %v", err, backoff)
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

func (s *Streamer) runOnce(ctx context.Context, url string) error {
	hwaccel := os.Getenv("LINDA_HWACCEL")
	args := []string{
		"-hide_banner",
		"-loglevel", "warning",
	}
	switch hwaccel {
	case "cuda":
		args = append(args, "-hwaccel", "cuda", "-hwaccel_output_format", "cuda")
	case "videotoolbox":
		// Decode on the GPU but let frames land back in system memory:
		// h264_videotoolbox accepts them either way, and skipping
		// -hwaccel_output_format avoids format-negotiation failures on
		// sources the encoder can't take directly.
		args = append(args, "-hwaccel", "videotoolbox")
	}
	args = append(args,
		"-rtsp_transport", "tcp",
		"-i", url,
	)
	switch hwaccel {
	case "cuda":
		args = append(args,
			"-c:v", "h264_nvenc",
			"-preset", "p4",
			"-rc", "vbr",
			"-cq", "23",
			"-b:v", "5M",
			"-maxrate", "8M",
			"-g", "30",
		)
	case "videotoolbox":
		// VideoToolbox has no CRF mode — it is bitrate-driven only. realtime
		// keeps the encoder from buffering ahead, which matters for a live
		// HLS ladder.
		args = append(args,
			"-c:v", "h264_videotoolbox",
			"-profile:v", "high",
			"-b:v", "5M",
			"-maxrate", "8M",
			"-bufsize", "10M",
			"-realtime", "1",
			"-g", "30",
			"-pix_fmt", "yuv420p",
		)
	default:
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
		// append_list continues the media sequence across ffmpeg restarts
		// so players don't see the playlist jump backwards; discont_start
		// marks the first new segment with EXT-X-DISCONTINUITY so they
		// expect the timestamp reset instead of stalling on it.
		"-hls_flags", "delete_segments+append_list+omit_endlist+discont_start",
		"-hls_segment_filename", filepath.Join(s.outDir, "seg_%05d.ts"),
		filepath.Join(s.outDir, "stream.m3u8"),
	)

	// runCtx lets the watchdog kill this ffmpeg without tearing down the
	// whole streamer; the loop then reconnects with backoff.
	runCtx, runCancel := context.WithCancel(ctx)
	defer runCancel()

	cmd := exec.CommandContext(runCtx, s.ffmpegPath, args...)
	cmd.WaitDelay = 5 * time.Second
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start ffmpeg: %w", err)
	}

	sup := &logutil.Suppressor{Prefix: "ffmpeg-stream: "}
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 4096), 1<<20)
		for sc.Scan() {
			sup.Log(sc.Text())
		}
		sup.Flush()
	}()

	stalled := make(chan time.Duration, 1)
	go s.watchdog(runCtx, runCancel, filepath.Join(s.outDir, "stream.m3u8"), stalled)

	waitErr := cmd.Wait()
	select {
	case idle := <-stalled:
		return fmt.Errorf("stalled (no HLS output for %v)", idle.Round(time.Second))
	default:
	}
	if runCtx.Err() != nil && ctx.Err() == nil {
		return fmt.Errorf("killed by watchdog")
	}
	return waitErr
}

// watchdog polls the playlist's mtime. Each change refreshes lastProgress;
// no change for stallTimeout (measured from process start if ffmpeg never
// produced anything) cancels runCtx, which SIGKILLs ffmpeg. The idle
// duration is reported on stalled so runOnce can say why it died.
func (s *Streamer) watchdog(runCtx context.Context, runCancel context.CancelFunc, playlist string, stalled chan<- time.Duration) {
	t := time.NewTicker(stallPoll)
	defer t.Stop()
	// Seed with the current mtime so a playlist left over from the previous
	// run doesn't count as progress for the new process.
	var lastMod time.Time
	if st, err := os.Stat(playlist); err == nil {
		lastMod = st.ModTime()
	}
	lastChange := time.Now()
	for {
		select {
		case <-runCtx.Done():
			return
		case <-t.C:
		}
		if st, err := os.Stat(playlist); err == nil && !st.ModTime().Equal(lastMod) {
			lastMod = st.ModTime()
			lastChange = time.Now()
			s.lastProgress.Store(lastChange.UnixNano())
			continue
		}
		if idle := time.Since(lastChange); idle >= stallTimeout {
			log.Printf(restartAnnounce, idle.Round(time.Second))
			s.lastProgress.Store(0)
			stalled <- idle
			runCancel()
			return
		}
	}
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
