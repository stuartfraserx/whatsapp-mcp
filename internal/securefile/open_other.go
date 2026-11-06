//go:build !unix

package securefile

import "os"

func open(path string) (*os.File, error) { return os.Open(path) }
