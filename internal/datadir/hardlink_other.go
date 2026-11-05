//go:build !unix

package datadir

import "io/fs"

// HardLinked always reports false where link counts are unavailable.
func HardLinked(fs.FileInfo) bool { return false }
