package logutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "x.log")
	r, err := OpenRotating(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("a", 30) + "\n" // 31 bytes
	for i := 0; i < 12; i++ {              // 372 bytes => 3 rotations
		if _, err := r.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"x.log", "x.log.1", "x.log.2"} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if st.Size() > 100 {
			t.Errorf("%s is %d bytes, over the cap", name, st.Size())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "x.log.3")); !os.IsNotExist(err) {
		t.Errorf("x.log.3 should have been dropped (keep=2)")
	}

	// Reopening an oversized file rotates it immediately.
	if err := os.WriteFile(path, []byte(strings.Repeat("b", 500)), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err = OpenRotating(path, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if st, _ := os.Stat(path); st.Size() != 0 {
		t.Errorf("live file should be fresh after reopen, got %d bytes", st.Size())
	}
	if b, _ := os.ReadFile(path + ".1"); len(b) != 500 {
		t.Errorf("oversized file should have become .1, got %d bytes", len(b))
	}
}

func TestSuppressor(t *testing.T) {
	s := &Suppressor{Prefix: "t: "}
	s.Log("same")
	s.Log("same")
	s.Log("same")
	if s.count != 2 {
		t.Errorf("expected 2 suppressed repeats, got %d", s.count)
	}
	s.Log("different")
	if s.count != 0 || s.last != "different" {
		t.Errorf("new message should reset the run: count=%d last=%q", s.count, s.last)
	}
}
