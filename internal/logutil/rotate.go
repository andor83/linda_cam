package logutil

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// RotatingFile is an io.WriteCloser that appends to path and, once the file
// exceeds maxBytes, renames it to path.1 (shifting older copies to .2, .3,
// ...) and starts a fresh file. At most keep rotated copies are retained.
//
// Rotation is by rename, so any other process that holds the file open in
// append mode (launchd's StandardOutPath, for instance) keeps writing into
// the rotated copy rather than a dangling inode — nothing is lost, and the
// next service start reopens the live path.
type RotatingFile struct {
	path     string
	maxBytes int64
	keep     int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// OpenRotating opens (or creates) path for appending. If the existing file is
// already over maxBytes it is rotated immediately so a runaway log from a
// previous run does not linger as the live file.
func OpenRotating(path string, maxBytes int64, keep int) (*RotatingFile, error) {
	if maxBytes <= 0 {
		maxBytes = 20 << 20
	}
	if keep < 0 {
		keep = 0
	}
	r := &RotatingFile{path: path, maxBytes: maxBytes, keep: keep}
	if err := r.open(); err != nil {
		return nil, err
	}
	if r.size > r.maxBytes {
		if err := r.rotateLocked(); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f = f
	r.size = st.Size()
	return nil
}

func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, os.ErrClosed
	}
	if r.size+int64(len(p)) > r.maxBytes && r.size > 0 {
		if err := r.rotateLocked(); err != nil {
			// Keep logging into the oversized file rather than dropping
			// output; rotation will be retried on the next write.
			fmt.Fprintf(os.Stderr, "logutil: rotate %s: %v\n", r.path, err)
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *RotatingFile) rotateLocked() error {
	if r.f != nil {
		r.f.Close()
		r.f = nil
	}
	if r.keep == 0 {
		if err := os.Remove(r.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else {
		// Shift path.(keep-1) -> path.keep, ..., path.1 -> path.2; the
		// oldest copy falls off the end.
		_ = os.Remove(r.rotated(r.keep))
		for i := r.keep - 1; i >= 1; i-- {
			if err := os.Rename(r.rotated(i), r.rotated(i+1)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err := os.Rename(r.path, r.rotated(1)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return r.open()
}

func (r *RotatingFile) rotated(i int) string {
	return filepath.Clean(fmt.Sprintf("%s.%d", r.path, i))
}

func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}
