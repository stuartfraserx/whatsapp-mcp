// Package datadir holds the filesystem helpers that keep on-disk state private
// and confine file access to known directories.
package datadir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Permissions applied to directories and files created by the server.
const (
	DirPerm  = 0o700
	FilePerm = 0o600
)

const maxNameLen = 255

// Errors returned by SafeJoin and ResolveInside.
var (
	ErrUnsafeName   = errors.New("unsafe file name")
	ErrOutsideRoots = errors.New("path is not a regular file inside an allowed directory")
)

// EnsureDir creates path (and parents) and tightens its mode to 0700. It fails
// when path exists and is not a directory.
func EnsureDir(path string) error {
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", path)
	}
	if err := os.MkdirAll(path, DirPerm); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}
	if err := os.Chmod(path, DirPerm); err != nil {
		return fmt.Errorf("chmod directory: %w", err)
	}
	return nil
}

// WriteFileAtomic writes data to a 0600 temp file in the target directory,
// syncs it and renames it over path.
func WriteFileAtomic(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err = tmp.Chmod(FilePerm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}
	return nil
}

// Harden sets mode 0600 on every path that exists; missing paths are ignored.
func Harden(paths ...string) error {
	for _, p := range paths {
		if err := os.Chmod(p, FilePerm); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("harden file: %w", err)
		}
	}
	return nil
}

// SafeJoin joins root and name, where name must be a single path element.
func SafeJoin(root, name string) (string, error) {
	if !safeElement(name) {
		return "", ErrUnsafeName
	}
	joined := filepath.Join(root, name)
	rel, err := filepath.Rel(root, joined)
	if err != nil || rel != name {
		return "", ErrUnsafeName
	}
	return joined, nil
}

func safeElement(name string) bool {
	if name == "" || name == "." || name == ".." || len(name) > maxNameLen {
		return false
	}
	if strings.ContainsAny(name, "/\\") {
		return false
	}
	for _, r := range name {
		if r == 0 || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ResolveInside resolves path through symlinks and returns it only when it is
// a regular file located inside one of roots.
func ResolveInside(path string, roots ...string) (string, error) {
	root, rel, err := LocateInside(path, roots...)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, rel), nil
}

// LocateInside resolves path through symlinks and, when it is a regular file
// inside one of roots, returns that root (also symlink-resolved) and the file's
// relative path below it. Callers open the file with os.OpenRoot so
// a later swap of path cannot escape root.
func LocateInside(path string, roots ...string) (root, rel string, err error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", ErrOutsideRoots
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", ErrOutsideRoots
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", "", ErrOutsideRoots
	}
	for _, candidate := range roots {
		if candidate == "" {
			continue
		}
		absRoot, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		realRoot, err := filepath.EvalSymlinks(absRoot)
		if err != nil {
			continue
		}
		if isInside(realRoot, resolved) {
			rel, err := filepath.Rel(realRoot, resolved)
			if err != nil {
				continue
			}
			return realRoot, rel, nil
		}
	}
	return "", "", ErrOutsideRoots
}

// Within reports whether path, resolved through symlinks when possible, is
// strictly inside dir.
func Within(dir, path string) bool {
	return isInside(realOrAbs(dir), realOrAbs(path))
}

func realOrAbs(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

func isInside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
