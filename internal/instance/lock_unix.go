//go:build unix

// Package instance enforces a single owner of a WhatsApp session.
package instance

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// ErrHeld identifies an existing owner without waiting for it.
var ErrHeld = errors.New("whatsapp session is already owned by another process")

// Lock owns the session until Close releases its file descriptor.
type Lock struct{ f *os.File }

// Acquire takes a non-blocking exclusive lock within dir.
func Acquire(dir string) (*Lock, error) {
	f, err := os.OpenFile(filepath.Join(dir, "instance.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("instance lock must be a regular file")
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrHeld
		}
		return nil, err
	}
	return &Lock{f}, nil
}

// Close releases session ownership.
func (l *Lock) Close() error { return l.f.Close() }
