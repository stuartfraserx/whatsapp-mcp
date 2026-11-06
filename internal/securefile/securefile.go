// Package securefile reads private configuration without following file substitutions.
package securefile

import (
	"errors"
	"io"
	"os"
)

// Read accepts a regular file with mode 0600 (or 0400 when exact is false).
func Read(path string, exact bool) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	valid := func(i os.FileInfo) bool {
		return i.Mode().IsRegular() && i.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0 && (i.Mode().Perm() == 0600 || (!exact && i.Mode().Perm() == 0400))
	}
	if !valid(before) {
		return nil, errors.New("file must be regular and private")
	}
	f, err := open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !valid(after) || !os.SameFile(before, after) {
		return nil, errors.New("file changed while opening")
	}
	b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if len(b) > 1<<20 {
		return nil, errors.New("private file exceeds size limit")
	}
	return b, err
}
