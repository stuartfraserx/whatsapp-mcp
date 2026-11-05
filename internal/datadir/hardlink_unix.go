//go:build unix

package datadir

import (
	"io/fs"
	"syscall"
)

// HardLinked reports whether info describes a file with more than one hard
// link, which can alias a file outside the directory it was found in.
func HardLinked(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(st.Nlink) > 1
}
