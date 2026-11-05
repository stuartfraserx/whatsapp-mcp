package tools

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmflurry/whatsapp-mcp/internal/datadir"
	"github.com/fmflurry/whatsapp-mcp/internal/redact"
)

const (
	maxCollisionSuffix = 1000
	maxNameBytes       = 200
	maxExtBytes        = 20
)

type downloadMediaInput struct {
	MessageID string `json:"message_id" jsonschema:"ID of the stored message that carries the media"`
	ChatJID   string `json:"chat_jid,omitempty" jsonschema:"Chat containing the message; required when the ID exists in several chats"`
	Filename  string `json:"filename,omitempty" jsonschema:"File name to save as (a single name, no directories); defaults to the original name"`
}

type downloadMediaOutput struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	MimeType  string `json:"mime_type,omitempty"`
}

func (h *handlers) downloadMedia(ctx context.Context, _ *mcp.CallToolRequest, in downloadMediaInput) (*mcp.CallToolResult, downloadMediaOutput, error) {
	id := strings.TrimSpace(in.MessageID)
	if id == "" {
		return nil, downloadMediaOutput{}, errors.New("message_id is required")
	}
	if err := validateOptionalJID("chat_jid", in.ChatJID); err != nil {
		return nil, downloadMediaOutput{}, err
	}
	if h.DownloadDir == "" {
		return nil, downloadMediaOutput{}, errors.New("download directory is not configured")
	}

	msg, err := h.Store.GetMessage(ctx, in.ChatJID, id)
	if err != nil {
		return nil, downloadMediaOutput{}, h.messageLookupError(ctx, id, err)
	}
	if msg.Media == nil {
		return nil, downloadMediaOutput{}, errors.New("message has no downloadable media")
	}

	limit := int64(MaxDownloadBytes)
	if h.DownloadLimit > 0 {
		limit = h.DownloadLimit
	}
	if msg.Media.FileLength > uint64(limit) {
		return nil, downloadMediaOutput{}, tooLargeError(limit)
	}

	name := in.Filename
	if name == "" {
		name = defaultFileName(msg.ID, msg.Media.FileName, msg.Media.MimeType)
	}
	if _, err := datadir.SafeJoin(h.DownloadDir, name); err != nil {
		return nil, downloadMediaOutput{}, errors.New("filename must be a single file name without directories or control characters")
	}

	data, err := h.Client.DownloadMedia(ctx, *msg.Media)
	if err != nil {
		h.Log.Warn("media download failed", "error", redact.Scrub(err.Error()))
		return nil, downloadMediaOutput{}, fmt.Errorf("download media: %s", redact.Scrub(err.Error()))
	}

	if int64(len(data)) > limit {
		return nil, downloadMediaOutput{}, tooLargeError(limit)
	}

	if err := datadir.EnsureDir(h.DownloadDir); err != nil {
		return nil, downloadMediaOutput{}, fmt.Errorf("prepare download directory: %w", err)
	}
	path, err := writeUnique(h.DownloadDir, name, data)
	if err != nil {
		return nil, downloadMediaOutput{}, err
	}
	return nil, downloadMediaOutput{Path: path, SizeBytes: int64(len(data)), MimeType: msg.Media.MimeType}, nil
}

func tooLargeError(limit int64) error {
	return fmt.Errorf("media is larger than the %d MiB download limit", limit>>20)
}

// writeUnique writes data to a new file named name inside dir, appending -1,
// -2, ... before the extension when the name is taken. Existing files are
// never touched.
func writeUnique(dir, name string, data []byte) (string, error) {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for n := 0; n <= maxCollisionSuffix; n++ {
		candidate := name
		if n > 0 {
			candidate = fmt.Sprintf("%s-%d%s", stem, n, ext)
		}
		path, err := datadir.SafeJoin(dir, candidate)
		if err != nil {
			return "", errors.New("file name is not usable")
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, datadir.FilePerm)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create download file: %w", err)
		}
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return "", fmt.Errorf("write download file: %w", err)
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(path)
			return "", fmt.Errorf("write download file: %w", err)
		}
		return path, nil
	}
	return "", errors.New("too many files with this name already exist")
}

// defaultFileName prefers the sender's original file name (sanitized) and
// falls back to <message id>.<extension from mime type>.
func defaultFileName(id, original, mimeType string) string {
	if name := sanitizeName(original); name != "" {
		return name
	}
	base := sanitizeName(id)
	if base == "" {
		base = "media"
	}
	return base + extensionFor(mimeType)
}

func sanitizeName(s string) string {
	if i := strings.LastIndexAny(s, `/\`); i >= 0 {
		s = s[i+1:]
	}
	s = strings.Map(func(r rune) rune {
		if r == 0 || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(s))
	if s == "." || s == ".." {
		return ""
	}
	if len(s) > maxNameBytes {
		ext := filepath.Ext(s)
		if len(ext) > maxExtBytes {
			ext = ""
		}
		cut := maxNameBytes - len(ext)
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + ext
	}
	return s
}

var preferredExtensions = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/webp":      ".webp",
	"image/gif":       ".gif",
	"video/mp4":       ".mp4",
	"audio/ogg":       ".ogg",
	"audio/mpeg":      ".mp3",
	"audio/mp4":       ".m4a",
	"application/pdf": ".pdf",
}

func extensionFor(mimeType string) string {
	mt, _, err := mime.ParseMediaType(mimeType)
	if err != nil {
		return ".bin"
	}
	if ext, ok := preferredExtensions[mt]; ok {
		return ext
	}
	exts, err := mime.ExtensionsByType(mt)
	if err != nil || len(exts) == 0 {
		return ".bin"
	}
	slices.Sort(exts)
	return exts[0]
}
