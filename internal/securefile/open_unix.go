//go:build unix

package securefile

import (
	"os"
	"syscall"
)

func open(path string) (*os.File, error) {
	// NONBLOCK prevents a substituted FIFO from hanging before fstat rejects it.
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
