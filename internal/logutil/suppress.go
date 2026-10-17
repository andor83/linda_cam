// Package logutil holds small helpers for keeping the server log readable:
// a repeated-message suppressor for chatty subprocess output and a
// size-capped rotating log file.
package logutil

import (
	"log"
	"sync"
	"time"
)

// Suppressor collapses runs of identical log lines. The first occurrence is
// logged immediately; repeats within Window are counted and summarized as a
// single "repeated N times" line once the window elapses or a different
// message arrives. ffmpeg and the RTSP decoder can emit the same warning
// thousands of times a second during a flaky connection, which previously
// grew the log by hundreds of megabytes.
type Suppressor struct {
	Prefix string        // logged before every message, e.g. "ffmpeg-stream: "
	Window time.Duration // how long identical lines are collapsed for

	mu       sync.Mutex
	last     string
	count    int
	lastSeen time.Time
	firstAt  time.Time
}

// Printf-free on purpose: callers hand us the already-formatted message.
func (s *Suppressor) Log(msg string) {
	window := s.Window
	if window <= 0 {
		window = 30 * time.Second
	}
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()

	if msg == s.last && now.Sub(s.firstAt) < window {
		s.count++
		s.lastSeen = now
		return
	}
	s.flushLocked()
	log.Printf("%s%s", s.Prefix, msg)
	s.last = msg
	s.count = 0
	s.firstAt = now
	s.lastSeen = now
}

// Flush writes any pending "repeated N times" summary. Call it when the
// producer (an ffmpeg process, an RTSP session) goes away so the count is
// not lost.
func (s *Suppressor) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
	s.last = ""
}

func (s *Suppressor) flushLocked() {
	if s.count > 0 {
		log.Printf("%slast message repeated %d times over %s", s.Prefix, s.count,
			s.lastSeen.Sub(s.firstAt).Round(time.Second))
	}
	s.count = 0
}
