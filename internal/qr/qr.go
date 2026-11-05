// Package qr renders WhatsApp pairing codes as a PNG file or terminal text.
package qr

import (
	"errors"
	"fmt"
	"io"
	"os"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/fmflurry/whatsapp-mcp/internal/datadir"
)

const pngSize = 512

// WritePNG renders code as a 512px PNG at path with mode 0600.
func WritePNG(path, code string) error {
	png, err := qrcode.Encode(code, qrcode.Medium, pngSize)
	if err != nil {
		return fmt.Errorf("encode qr code: %w", err)
	}
	return datadir.WriteFileAtomic(path, png)
}

// RenderTerminal writes a half-block rendering of code to w.
func RenderTerminal(w io.Writer, code string) error {
	q, err := qrcode.New(code, qrcode.Medium)
	if err != nil {
		return fmt.Errorf("encode qr code: %w", err)
	}
	if _, err := io.WriteString(w, q.ToSmallString(false)); err != nil {
		return fmt.Errorf("write qr code: %w", err)
	}
	return nil
}

// Presenter publishes pairing codes. Terminal is nil when no TTY is available.
type Presenter struct {
	PNGPath  string
	Terminal io.Writer
}

// Present writes the PNG (when PNGPath is set) and, when Terminal is set,
// renders the code there. rendered reports whether the terminal got it.
func (p Presenter) Present(code string) (rendered bool, err error) {
	if p.PNGPath != "" {
		if err := WritePNG(p.PNGPath, code); err != nil {
			return false, err
		}
	}
	if p.Terminal == nil {
		return false, nil
	}
	if err := RenderTerminal(p.Terminal, code); err != nil {
		return false, err
	}
	return true, nil
}

// Clear removes the PNG if it exists.
func (p Presenter) Clear() error {
	if p.PNGPath == "" {
		return nil
	}
	if err := os.Remove(p.PNGPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove qr png: %w", err)
	}
	return nil
}
