//go:build !unix

package daemon

import "errors"

func Start([]string) error {
	return errors.New("detached daemon startup is unsupported on this platform")
}
