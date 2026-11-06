//go:build unix

package instance

import (
	"errors"
	"testing"
)

func TestExclusiveNonBlockingLock(t *testing.T) {
	dir := t.TempDir()
	first, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := Acquire(dir); !errors.Is(err, ErrHeld) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second lock: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
}
