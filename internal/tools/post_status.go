package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/fmflurry/whatsapp-mcp/internal/datadir"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const statusJID = "status@broadcast"

type postStatusInput struct {
	Path           string `json:"path" jsonschema:"Path of a regular image or video inside the server's outbox or download directory, at most 64 MiB"`
	Caption        string `json:"caption,omitempty" jsonschema:"Optional caption, at most 4096 characters"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Optional retry deduplication key (10 minute window)"`
}

func (h *handlers) postStatus(ctx context.Context, r *mcp.CallToolRequest, in postStatusInput) (*mcp.CallToolResult, sendOutput, error) {
	key := h.attemptKey(ctx, "post_status", in.IdempotencyKey, statusJID, in.Path, in.Caption)
	return attempt(h, ctx, key, func() (*mcp.CallToolResult, sendOutput, error) { return h.postStatusOnce(ctx, r, in) })
}

func (h *handlers) postStatusOnce(ctx context.Context, _ *mcp.CallToolRequest, in postStatusInput) (*mcp.CallToolResult, sendOutput, error) {
	if strings.TrimSpace(in.Path) == "" {
		return nil, sendOutput{}, errors.New("path is required")
	}
	if utf8.RuneCountInString(in.Caption) > maxTextRunes {
		return nil, sendOutput{}, fmt.Errorf("caption must be at most %d characters", maxTextRunes)
	}
	f, name, err := h.openOutgoing(in.Path)
	if err != nil {
		return nil, sendOutput{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || datadir.HardLinked(info) {
		return nil, sendOutput{}, errors.New("path must be a regular file inside the outbox or download directory")
	}
	if info.Size() == 0 {
		return nil, sendOutput{}, errors.New("file is empty")
	}
	if info.Size() > maxFileBytes {
		return nil, sendOutput{}, fmt.Errorf("file is larger than the %d MiB limit", maxFileBytes>>20)
	}
	mimeType := mimeFor(name)
	kind := wa.StatusMediaType(mimeType)
	if kind == "" {
		return nil, sendOutput{}, errors.New("status requires a JPEG, PNG or WebP image or a video; GIF, audio, documents and stickers are not supported")
	}
	data, err := readLimited(f)
	if err != nil {
		return nil, sendOutput{}, err
	}
	if len(data) == 0 {
		return nil, sendOutput{}, errors.New("file is empty")
	}
	slot, err := h.checkPolicy(statusJID)
	if err != nil {
		return nil, sendOutput{}, err
	}
	file := wa.OutgoingFile{Data: data, FileName: name, MimeType: mimeType, Caption: in.Caption}
	sent, err := h.Client.PostStatus(ctx, file)
	if err != nil {
		h.refundIfNotSent(slot, err)
		return nil, failedSend(sent, statusJID, err), h.sendError(err)
	}
	return nil, h.recordSent(ctx, statusJID, sent, in.Caption, &wa.Media{Type: kind, MimeType: mimeType, FileName: name, FileLength: uint64(len(data))}), nil
}
