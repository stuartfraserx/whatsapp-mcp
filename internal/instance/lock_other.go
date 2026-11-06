//go:build !unix

package instance

import "errors"

var ErrHeld = errors.New("whatsapp session is already owned by another process")

type Lock struct{}

func Acquire(string) (*Lock, error) { return &Lock{}, nil }
func (*Lock) Close() error          { return nil }
