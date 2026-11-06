//go:build unix

package daemon

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStartRejectsUnsafeCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "supervisor")
	os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700)
	for _, args := range [][]string{nil, {"relative"}, {filepath.Dir(path)}} {
		if Start(args) == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	os.Chmod(path, 0770)
	if Start([]string{path}) == nil {
		t.Fatal("accepted writable command")
	}
	link := path + "-link"
	os.Symlink(path, link)
	if Start([]string{link}) == nil {
		t.Fatal("accepted symlink executable")
	}
}
