//go:build unix

package datadir_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/fmflurry/whatsapp-mcp/internal/datadir"
)

func withUmask(t *testing.T, mask int) {
	t.Helper()
	old := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(old) })
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

func TestEnsureDirCreatesPrivateDirectoryTree(t *testing.T) {
	withUmask(t, 0)
	root := t.TempDir()
	leaf := filepath.Join(root, "a", "b", "c")

	if err := datadir.EnsureDir(leaf); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	for _, d := range []string{filepath.Join(root, "a"), filepath.Join(root, "a", "b"), leaf} {
		if got := mode(t, d); got != 0o700 {
			t.Errorf("%s mode = %o, want 700", d, got)
		}
	}
}

func TestEnsureDirTightensExistingPermissiveDirectory(t *testing.T) {
	withUmask(t, 0)
	for _, perm := range []os.FileMode{0o755, 0o777, 0o750, 0o500} {
		dir := filepath.Join(t.TempDir(), "data")
		if err := os.Mkdir(dir, perm); err != nil {
			t.Fatal(err)
		}
		if err := datadir.EnsureDir(dir); err != nil {
			t.Fatalf("EnsureDir(%o): %v", perm, err)
		}
		if got := mode(t, dir); got != 0o700 {
			t.Errorf("dir created %o: mode after EnsureDir = %o, want 700", perm, got)
		}
	}
}

func TestEnsureDirIsIdempotentAndKeepsContent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := datadir.EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "keep.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := datadir.EnsureDir(dir); err != nil {
		t.Fatalf("second EnsureDir: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("EnsureDir removed existing content: %v", err)
	}
}

func TestEnsureDirRejectsRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := datadir.EnsureDir(path); err == nil {
		t.Fatal("EnsureDir on a regular file must fail")
	}
	if got := mode(t, path); got != 0o644 {
		t.Errorf("rejected file mode changed to %o", got)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "secret" {
		t.Errorf("rejected file content changed: %q %v", data, err)
	}
}

func TestEnsureDirRejectsPathBelowAFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := datadir.EnsureDir(filepath.Join(file, "sub")); err == nil {
		t.Fatal("EnsureDir below a regular file must fail")
	}
}

func TestWriteFileAtomicCreatesPrivateFileEvenWithPermissiveUmask(t *testing.T) {
	withUmask(t, 0)
	path := filepath.Join(t.TempDir(), "out.bin")
	want := []byte("payload\x00bytes")

	if err := datadir.WriteFileAtomic(path, want); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	if got := mode(t, path); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(want) {
		t.Errorf("content = %q, %v; want %q", got, err, want)
	}
}

func TestWriteFileAtomicReplacesExistingPermissiveFile(t *testing.T) {
	withUmask(t, 0)
	path := filepath.Join(t.TempDir(), "out.bin")
	if err := os.WriteFile(path, []byte("old content that is longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := datadir.WriteFileAtomic(path, []byte("new")); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	if got := mode(t, path); got != 0o600 {
		t.Errorf("mode after overwrite = %o, want 600", got)
	}
	if got, _ := os.ReadFile(path); string(got) != "new" {
		t.Errorf("content = %q, want %q", got, "new")
	}
}

func TestWriteFileAtomicLeavesNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	for range 3 {
		if err := datadir.WriteFileAtomic(path, []byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "out.bin" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("directory holds %v, want only out.bin", names)
	}
}

func TestWriteFileAtomicEmptyData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty")
	if err := datadir.WriteFileAtomic(path, nil); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 {
		t.Errorf("stat = %v, %v; want an empty file", info, err)
	}
}

func TestWriteFileAtomicFailureKeepsTargetAndLeavesNoTemp(t *testing.T) {
	t.Run("target is a directory", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := datadir.WriteFileAtomic(target, []byte("x")); err == nil {
			t.Fatal("overwriting a directory must fail")
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Errorf("failed write left %d entries, want only the directory", len(entries))
		}
	})
	t.Run("missing parent", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "nope", "file")
		if err := datadir.WriteFileAtomic(missing, []byte("x")); err == nil {
			t.Fatal("writing into a missing directory must fail")
		}
		if _, err := os.Stat(filepath.Dir(missing)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("WriteFileAtomic must not create parent directories, stat err = %v", err)
		}
	})
}

func TestHarden(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db-wal")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, 0o666); err != nil {
			t.Fatal(err)
		}
	}
	missing := filepath.Join(dir, "missing-shm")

	if err := datadir.Harden(a, missing, b); err != nil {
		t.Fatalf("Harden with a missing path: %v", err)
	}
	for _, p := range []string{a, b} {
		if got := mode(t, p); got != 0o600 {
			t.Errorf("%s mode = %o, want 600", p, got)
		}
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Harden must not create missing paths, stat err = %v", err)
	}
	if err := datadir.Harden(); err != nil {
		t.Errorf("Harden() with no paths: %v", err)
	}
	if err := datadir.Harden("bad\x00path"); err == nil {
		t.Error("Harden must report errors other than a missing file")
	}
}

func TestSafeJoinAcceptsSingleElements(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "downloads")
	names := []string{
		"photo.jpg",
		"with space.png",
		".hidden",
		"..dotdot-prefix",
		"trailing.dot.",
		"unicode-é-日本.txt",
		"emoji-😀.png",
		"a:b",
		strings.Repeat("a", 255),
	}
	for _, name := range names {
		got, err := datadir.SafeJoin(root, name)
		if err != nil {
			t.Errorf("SafeJoin(%q) error = %v", name, err)
			continue
		}
		if want := filepath.Join(root, name); got != want {
			t.Errorf("SafeJoin(%q) = %q, want %q", name, got, want)
		}
		if filepath.Dir(got) != root {
			t.Errorf("SafeJoin(%q) = %q escapes %q", name, got, root)
		}
	}
}

func TestSafeJoinRejectsUnsafeNames(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tests := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"dot", "."},
		{"dotdot", ".."},
		{"parent traversal", "../x"},
		{"deep traversal", "../../etc/passwd"},
		{"nested", "a/b"},
		{"absolute", "/etc/passwd"},
		{"trailing slash", "x/"},
		{"backslash", "a\\b"},
		{"windows traversal", "..\\x"},
		{"nul byte", "a\x00b"},
		{"newline", "a\nb"},
		{"carriage return", "a\rb"},
		{"tab", "a\tb"},
		{"escape character", "a\x1bb"},
		{"delete character", "a\x7fb"},
		{"c1 control", "a\u0085b"},
		{"256 bytes", strings.Repeat("a", 256)},
		{"multibyte over 255 bytes", strings.Repeat("é", 128)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := datadir.SafeJoin(root, tt.in)
			if !errors.Is(err, datadir.ErrUnsafeName) {
				t.Errorf("SafeJoin(%q) = (%q, %v), want ErrUnsafeName", tt.in, got, err)
			}
			if got != "" {
				t.Errorf("SafeJoin(%q) returned a path %q alongside an error", tt.in, got)
			}
		})
	}
}

func TestSafeJoinHandlesUntidyRoots(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	for _, root := range []string{base + "/", filepath.Join(base, "sub", ".."), filepath.Join(base, "x") + "/."} {
		got, err := datadir.SafeJoin(root, "f.txt")
		if err != nil {
			t.Errorf("SafeJoin(%q, f.txt): %v", root, err)
			continue
		}
		if filepath.Base(got) != "f.txt" || !strings.HasPrefix(got, filepath.Clean(root)) {
			t.Errorf("SafeJoin(%q, f.txt) = %q", root, got)
		}
	}
}

func TestResolveInside(t *testing.T) {
	t.Parallel()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outbox := filepath.Join(base, "outbox")
	downloads := filepath.Join(base, "downloads")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{outbox, downloads, outside, filepath.Join(outbox, "sub")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string) string {
		t.Helper()
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	inOutbox := write(filepath.Join(outbox, "a.pdf"))
	nested := write(filepath.Join(outbox, "sub", "n.pdf"))
	inDownloads := write(filepath.Join(downloads, "d.jpg"))
	secret := write(filepath.Join(outside, "secret.txt"))
	siblingPrefix := filepath.Join(base, "outbox-evil")
	if err := os.Mkdir(siblingPrefix, 0o700); err != nil {
		t.Fatal(err)
	}
	prefixFile := write(filepath.Join(siblingPrefix, "f.txt"))

	escapeFile := filepath.Join(outbox, "escape-file")
	escapeDir := filepath.Join(outbox, "escape-dir")
	insideLink := filepath.Join(outbox, "inside-link")
	linkToDownloads := filepath.Join(outbox, "to-downloads")
	rootLink := filepath.Join(base, "outbox-link")
	for link, target := range map[string]string{
		escapeFile:      secret,
		escapeDir:       outside,
		insideLink:      inOutbox,
		linkToDownloads: inDownloads,
		rootLink:        outbox,
	} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	fifo := filepath.Join(outbox, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		path  string
		roots []string
		want  string
	}{
		{"file in outbox", inOutbox, []string{outbox, downloads}, inOutbox},
		{"nested file", nested, []string{outbox, downloads}, nested},
		{"file in second root", inDownloads, []string{outbox, downloads}, inDownloads},
		{"symlink inside root to file inside root", insideLink, []string{outbox}, inOutbox},
		{"symlink inside root to file in other allowed root", linkToDownloads, []string{outbox, downloads}, inDownloads},
		{"path through a symlinked root", filepath.Join(rootLink, "a.pdf"), []string{outbox}, inOutbox},
		{"symlinked root", inOutbox, []string{rootLink}, inOutbox},
		{"unclean path inside root", filepath.Join(outbox, "sub", "..", "a.pdf"), []string{outbox}, inOutbox},
		{"empty root entry ignored", inOutbox, []string{"", outbox}, inOutbox},

		{"file outside roots", secret, []string{outbox, downloads}, ""},
		{"symlink escaping root to file", escapeFile, []string{outbox}, ""},
		{"path through symlinked directory escaping root", filepath.Join(escapeDir, "secret.txt"), []string{outbox}, ""},
		{"dotdot traversal out of root", filepath.Join(outbox, "..", "outside", "secret.txt"), []string{outbox}, ""},
		{"sibling directory sharing the root prefix", prefixFile, []string{outbox}, ""},
		{"directory", filepath.Join(outbox, "sub"), []string{outbox}, ""},
		{"the root itself", outbox, []string{outbox}, ""},
		{"symlink to directory", escapeDir, []string{outbox, outside}, ""},
		{"fifo", fifo, []string{outbox}, ""},
		{"missing file", filepath.Join(outbox, "missing.pdf"), []string{outbox}, ""},
		{"dangling symlink", filepath.Join(outbox, "dangling"), []string{outbox}, ""},
		{"empty path", "", []string{outbox}, ""},
		{"no roots", inOutbox, nil, ""},
		{"only empty roots", inOutbox, []string{""}, ""},
		{"missing root", inOutbox, []string{filepath.Join(base, "nope")}, ""},
	}
	if err := os.Symlink(filepath.Join(base, "nowhere"), filepath.Join(outbox, "dangling")); err != nil {
		t.Fatal(err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := datadir.ResolveInside(tt.path, tt.roots...)
			if tt.want == "" {
				if !errors.Is(err, datadir.ErrOutsideRoots) || got != "" {
					t.Errorf("ResolveInside(%q) = (%q, %v), want ErrOutsideRoots", tt.path, got, err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("ResolveInside(%q) = (%q, %v), want %q", tt.path, got, err, tt.want)
			}
		})
	}
}

func TestResolveInsideRelativePath(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	outbox := filepath.Join(base, "outbox")
	if err := os.Mkdir(outbox, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outbox, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "b.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(outbox)

	got, err := datadir.ResolveInside("a.txt", outbox)
	if err != nil || got != filepath.Join(outbox, "a.txt") {
		t.Errorf("relative path inside root = (%q, %v)", got, err)
	}
	if _, err := datadir.ResolveInside("../b.txt", outbox); !errors.Is(err, datadir.ErrOutsideRoots) {
		t.Errorf("relative traversal error = %v, want ErrOutsideRoots", err)
	}
	if got, err := datadir.ResolveInside("a.txt", "."); err != nil || got != filepath.Join(outbox, "a.txt") {
		t.Errorf("relative root = (%q, %v)", got, err)
	}
}

func TestLocateInsideReturnsTheContainingRootAndRelativePath(t *testing.T) {
	base := t.TempDir()
	outbox := filepath.Join(base, "outbox")
	downloads := filepath.Join(base, "downloads")
	for _, f := range []string{filepath.Join(outbox, "sub", "a.txt"), filepath.Join(downloads, "b.txt")} {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}

	root, rel, err := datadir.LocateInside(filepath.Join(outbox, "sub", "a.txt"), downloads, outbox)
	if err != nil || root != filepath.Join(realBase, "outbox") || rel != filepath.Join("sub", "a.txt") {
		t.Fatalf("LocateInside = (%q, %q, %v)", root, rel, err)
	}
	root, rel, err = datadir.LocateInside(filepath.Join(downloads, "b.txt"), outbox, downloads)
	if err != nil || root != filepath.Join(realBase, "downloads") || rel != "b.txt" {
		t.Fatalf("LocateInside = (%q, %q, %v)", root, rel, err)
	}
	if _, _, err := datadir.LocateInside(filepath.Join(base, "nope.txt"), outbox, downloads); !errors.Is(err, datadir.ErrOutsideRoots) {
		t.Fatalf("missing file: err = %v, want ErrOutsideRoots", err)
	}
}

func TestWithinIsStrictAndResolvesSymlinks(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "data")
	if err := os.MkdirAll(filepath.Join(data, "outbox"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(data, link); err != nil {
		t.Fatal(err)
	}

	if datadir.Within(data, data) {
		t.Error("a directory is not strictly within itself")
	}
	if !datadir.Within(data, filepath.Join(data, "outbox")) || !datadir.Within(link, filepath.Join(data, "outbox")) {
		t.Error("a subdirectory is within its parent, also through a symlink")
	}
	if datadir.Within(filepath.Join(data, "outbox"), data) || datadir.Within(data, base) {
		t.Error("an ancestor is not within its descendant")
	}
	if datadir.Within(data, filepath.Join(base, "data-evil", "x")) {
		t.Error("a sibling sharing a prefix is not within")
	}
}

func TestHardLinked(t *testing.T) {
	dir := t.TempDir()
	orig := filepath.Join(dir, "orig")
	if err := os.WriteFile(orig, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(orig)
	if datadir.HardLinked(info) {
		t.Error("a file with one link is not hard-linked")
	}
	if err := os.Link(orig, filepath.Join(dir, "alias")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	info, _ = os.Stat(orig)
	if !datadir.HardLinked(info) {
		t.Error("a file with two links is hard-linked")
	}
}
