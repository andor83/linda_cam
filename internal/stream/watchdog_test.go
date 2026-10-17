package stream

import (
	"bytes"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// A "camera" that accepts the RTSP TCP connection and then never answers,
// which is how a camera that lost power mid-session looks to ffmpeg.
func silentRTSP(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	return "rtsp://" + ln.Addr().String() + "/cam"
}

func TestWatchdogRestartsHungFFmpeg(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	stallTimeout, stallPoll = 3*time.Second, 500*time.Millisecond

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	s := New(ffmpeg, "copy", t.TempDir())
	s.SetURL(silentRTSP(t))
	t.Cleanup(s.Stop)

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Count(buf.String(), "restarting ffmpeg") >= 2 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	out := buf.String()
	if strings.Count(out, "restarting ffmpeg") < 2 {
		t.Fatalf("watchdog did not restart a hung ffmpeg twice within 15s; log:\n%s", out)
	}
	if !strings.Contains(out, "stalled (no HLS output") {
		t.Errorf("exit reason should name the stall; log:\n%s", out)
	}
	if s.Connected() {
		t.Error("Connected() must be false while no segments are being written")
	}
}
