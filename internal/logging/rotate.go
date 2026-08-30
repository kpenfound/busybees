package logging

import (
	"os"
	"sync"
)

// defaultMaxBytes is the size at which bees.log rotates. Rotation is not
// configurable on purpose: two generations of 10 MiB are enough to debug a
// factory run and nobody should have to tune it.
const defaultMaxBytes int64 = 10 << 20

// rotatingWriter appends to a file and, when a write would take it past
// MaxBytes, renames it out of the way: <path> -> <path>.1 -> <path>.2, and
// <path>.2 is dropped. It is safe for concurrent use.
type rotatingWriter struct {
	path string
	// MaxBytes is the size at which the file rotates.
	MaxBytes int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

func newRotatingWriter(path string, maxBytes int64) (*rotatingWriter, error) {
	w := &rotatingWriter{path: path, MaxBytes: maxBytes}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	size := int64(0)
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	w.f, w.size = f, size
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	// Rotate before the write, but never on an empty file: a record larger
	// than MaxBytes would otherwise rotate forever and never be written.
	if w.size > 0 && w.MaxBytes > 0 && w.size+int64(len(p)) > w.MaxBytes {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) rotate() error {
	if err := w.f.Close(); err != nil {
		return err
	}
	w.f = nil
	_ = os.Remove(w.path + ".2")
	_ = os.Rename(w.path+".1", w.path+".2")
	if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	return w.open()
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
