package qr_test

import (
	"bytes"
	"errors"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fmflurry/whatsapp-mcp/internal/qr"
)

const (
	pairingCode = "2@Abc123,Def456/ghiJKL789=,mno012pqr345==,stu678"
	otherCode   = "2@ZZZ999,Yyy888/xxxWWW777=,vvv666uuu555==,ttt444"
)

var errWrite = errors.New("write failed")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errWrite }

func decodePNG(t *testing.T, path string) (dark, light int, width, height int) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("%s is not a valid PNG: %v", path, err)
	}
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y < 128 {
				dark++
			} else {
				light++
			}
		}
	}
	return dark, light, b.Dx(), b.Dy()
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestWritePNGProducesAPrivateValidPNG(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "pairing-qr.png")
	if err := qr.WritePNG(path, pairingCode); err != nil {
		t.Fatalf("WritePNG: %v", err)
	}
	dark, light, w, h := decodePNG(t, path)
	if w != 512 || h != 512 {
		t.Errorf("image is %dx%d, want 512x512", w, h)
	}
	if dark == 0 || light == 0 {
		t.Errorf("image has %d dark and %d light pixels; a QR code needs both", dark, light)
	}
	if got := mode(t, path); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
}

func TestWritePNGEncodesTheCode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, a2, b := filepath.Join(dir, "a.png"), filepath.Join(dir, "a2.png"), filepath.Join(dir, "b.png")
	for path, code := range map[string]string{a: pairingCode, a2: pairingCode, b: otherCode} {
		if err := qr.WritePNG(path, code); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) []byte {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if !bytes.Equal(read(a), read(a2)) {
		t.Error("the same code must render the same image")
	}
	if bytes.Equal(read(a), read(b)) {
		t.Error("different codes must render different images")
	}
}

func TestWritePNGReplacesExistingPermissiveFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "pairing-qr.png")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := qr.WritePNG(path, pairingCode); err != nil {
		t.Fatalf("WritePNG: %v", err)
	}
	decodePNG(t, path)
	if got := mode(t, path); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
}

func TestWritePNGInvalidCodeKeepsPreviousImage(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "pairing-qr.png")
	if err := qr.WritePNG(path, pairingCode); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for name, code := range map[string]string{"empty": "", "too long": strings.Repeat("x", 5000)} {
		if err := qr.WritePNG(path, code); err == nil {
			t.Errorf("%s code: WritePNG must fail", name)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Errorf("a failed write must leave the previous image untouched (err=%v)", err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the PNG", len(entries))
	}
}

func TestWritePNGMissingDirectoryFails(t *testing.T) {
	t.Parallel()
	if err := qr.WritePNG(filepath.Join(t.TempDir(), "nope", "qr.png"), pairingCode); err == nil {
		t.Fatal("WritePNG into a missing directory must fail")
	}
}

func TestRenderTerminalWritesHalfBlockText(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := qr.RenderTerminal(&buf, pairingCode); err != nil {
		t.Fatalf("RenderTerminal: %v", err)
	}
	out := buf.String()
	if strings.Count(out, "\n") < 10 {
		t.Errorf("output has %d lines, want a QR code spanning many lines", strings.Count(out, "\n"))
	}
	for _, r := range out {
		switch r {
		case ' ', '\n', '▀', '▄', '█':
		default:
			t.Fatalf("unexpected rune %q in terminal rendering", r)
		}
	}
	if !strings.ContainsAny(out, "▀▄█") {
		t.Error("rendering contains no dark modules")
	}

	var other bytes.Buffer
	if err := qr.RenderTerminal(&other, otherCode); err != nil {
		t.Fatal(err)
	}
	if other.String() == out {
		t.Error("different codes must render differently")
	}
}

func TestRenderTerminalErrors(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := qr.RenderTerminal(&buf, ""); err == nil {
		t.Error("an empty code must fail")
	}
	if buf.Len() != 0 {
		t.Errorf("a failed render wrote %d bytes", buf.Len())
	}
	if err := qr.RenderTerminal(failingWriter{}, pairingCode); !errors.Is(err, errWrite) {
		t.Errorf("writer failure = %v, want it wrapped", err)
	}
}

func TestPresentWithoutTerminalWritesOnlyThePNG(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "pairing-qr.png")
	p := qr.Presenter{PNGPath: path}
	rendered, err := p.Present(pairingCode)
	if err != nil {
		t.Fatalf("Present: %v", err)
	}
	if rendered {
		t.Error("rendered must be false when Terminal is nil")
	}
	decodePNG(t, path)
	if got := mode(t, path); got != 0o600 {
		t.Errorf("mode = %o, want 600", got)
	}
}

func TestPresentWithTerminalWritesPNGAndRendersTheSameCode(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "pairing-qr.png")
	var term bytes.Buffer
	rendered, err := qr.Presenter{PNGPath: path, Terminal: &term}.Present(pairingCode)
	if err != nil || !rendered {
		t.Fatalf("Present = (%v, %v), want (true, nil)", rendered, err)
	}
	decodePNG(t, path)
	var want bytes.Buffer
	if err := qr.RenderTerminal(&want, pairingCode); err != nil {
		t.Fatal(err)
	}
	if term.String() != want.String() || term.Len() == 0 {
		t.Error("terminal output must equal RenderTerminal's output for the same code")
	}
}

func TestPresentWithoutPNGPathOnlyRendersToTerminal(t *testing.T) {
	t.Parallel()
	var term bytes.Buffer
	rendered, err := qr.Presenter{Terminal: &term}.Present(pairingCode)
	if err != nil || !rendered || term.Len() == 0 {
		t.Errorf("Present = (%v, %v) with %d bytes", rendered, err, term.Len())
	}
	rendered, err = qr.Presenter{}.Present(pairingCode)
	if err != nil || rendered {
		t.Errorf("Present with nothing configured = (%v, %v), want (false, nil)", rendered, err)
	}
}

func TestPresentFailures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	t.Run("invalid code", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(dir, "invalid.png")
		var term bytes.Buffer
		rendered, err := qr.Presenter{PNGPath: path, Terminal: &term}.Present("")
		if err == nil || rendered {
			t.Errorf("Present = (%v, %v), want (false, error)", rendered, err)
		}
		if term.Len() != 0 {
			t.Error("nothing may be rendered for an invalid code")
		}
	})
	t.Run("png path not writable skips terminal", func(t *testing.T) {
		t.Parallel()
		var term bytes.Buffer
		p := qr.Presenter{PNGPath: filepath.Join(dir, "missing", "qr.png"), Terminal: &term}
		rendered, err := p.Present(pairingCode)
		if err == nil || rendered {
			t.Errorf("Present = (%v, %v), want (false, error)", rendered, err)
		}
		if term.Len() != 0 {
			t.Error("terminal must not be written when the PNG failed")
		}
	})
	t.Run("terminal write fails", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(dir, "term-fail.png")
		rendered, err := qr.Presenter{PNGPath: path, Terminal: failingWriter{}}.Present(pairingCode)
		if !errors.Is(err, errWrite) || rendered {
			t.Errorf("Present = (%v, %v), want (false, write error)", rendered, err)
		}
	})
}

func TestClearRemovesPNGAndToleratesMissingFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "pairing-qr.png")
	p := qr.Presenter{PNGPath: path}
	if _, err := p.Present(pairingCode); err != nil {
		t.Fatal(err)
	}
	if err := p.Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("PNG still exists after Clear: %v", err)
	}
	if err := p.Clear(); err != nil {
		t.Errorf("second Clear must tolerate a missing file: %v", err)
	}
}

func TestClearWithoutPNGPathIsANoOp(t *testing.T) {
	t.Parallel()
	if err := (qr.Presenter{}).Clear(); err != nil {
		t.Errorf("Clear: %v", err)
	}
}

func TestClearReportsUnremovableTarget(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "qr.png")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "child"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (qr.Presenter{PNGPath: dir}).Clear(); err == nil {
		t.Error("Clear on a non-empty directory must report the failure")
	}
}

func TestClearOnlyRemovesTheConfiguredFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	keep := filepath.Join(dir, "keep.png")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (qr.Presenter{PNGPath: filepath.Join(dir, "pairing-qr.png")}).Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("Clear removed an unrelated file: %v", err)
	}
}
