package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmflurry/whatsapp-mcp/internal/datadir"
	"github.com/fmflurry/whatsapp-mcp/internal/ratelimit"
	"github.com/fmflurry/whatsapp-mcp/internal/redact"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

const (
	maxTextRunes = 4096
	maxFileBytes = 64 << 20

	statusSent      = "sent"
	statusAmbiguous = "ambiguous"
)

type sendMessageInput struct {
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Optional retry deduplication key (10 minute window)"`
	Recipient      string `json:"recipient" jsonschema:"Recipient JID, phone number with country code, or the exact name of a contact or chat"`
	Text           string `json:"text" jsonschema:"Message text, at most 4096 characters, not blank"`
}

type sendFileInput struct {
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Optional retry deduplication key (10 minute window)"`
	Recipient      string `json:"recipient" jsonschema:"Recipient JID, phone number with country code, or the exact name of a contact or chat"`
	Path           string `json:"path" jsonschema:"Path of a regular file inside the server's outbox or download directory, at most 64 MiB"`
	Caption        string `json:"caption,omitempty" jsonschema:"Optional caption, at most 4096 characters"`
}

type sendOutput struct {
	Status     string      `json:"status" jsonschema:"sent, unknown_maybe_sent when delivery is uncertain, or ambiguous when the recipient matched several chats"`
	MessageID  string      `json:"message_id,omitempty"`
	ChatJID    string      `json:"chat_jid,omitempty"`
	Timestamp  string      `json:"timestamp,omitempty"`
	Candidates []candidate `json:"candidates,omitempty"`
	Note       string      `json:"note,omitempty"`
}

func ambiguousOutput(cands []candidate) sendOutput {
	return sendOutput{
		Status:     statusAmbiguous,
		Candidates: cands,
		Note:       "Nothing was sent. The name is not an exact, unique match of a saved contact name. Names are never auto-sent when they also match a profile name, chat or group name, or several contacts. Ask the user which one is meant, then re-run with the exact JID.",
	}
}

func (h *handlers) sendMessageOnce(ctx context.Context, _ *mcp.CallToolRequest, in sendMessageInput) (*mcp.CallToolResult, sendOutput, error) {
	if strings.TrimSpace(in.Text) == "" {
		return nil, sendOutput{}, errors.New("text must not be blank")
	}
	if utf8.RuneCountInString(in.Text) > maxTextRunes {
		return nil, sendOutput{}, fmt.Errorf("text must be at most %d characters", maxTextRunes)
	}

	res, err := h.resolveRecipient(ctx, in.Recipient)
	if err != nil {
		return nil, sendOutput{}, err
	}
	if res.JID == "" {
		return nil, ambiguousOutput(res.Candidates), nil
	}
	slot, err := h.checkPolicy(res.JID)
	if err != nil {
		return nil, sendOutput{}, err
	}

	sent, err := h.Client.SendText(ctx, res.JID, in.Text)
	if err != nil {
		h.refundIfNotSent(slot, err)
		return nil, failedSend(sent, res.JID, err), h.sendError(err)
	}
	return nil, h.recordSent(ctx, res.JID, sent, in.Text, nil), nil
}

func (h *handlers) sendFileOnce(ctx context.Context, _ *mcp.CallToolRequest, in sendFileInput) (*mcp.CallToolResult, sendOutput, error) {
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
	if in.Caption != "" && wa.OutgoingMediaType(mimeType) == wa.MediaAudio {
		return nil, sendOutput{}, errors.New("captions are not supported for audio")
	}

	res, err := h.resolveRecipient(ctx, in.Recipient)
	if err != nil {
		return nil, sendOutput{}, err
	}
	if res.JID == "" {
		return nil, ambiguousOutput(res.Candidates), nil
	}

	data, err := readLimited(f)
	if err != nil {
		return nil, sendOutput{}, err
	}
	slot, err := h.checkPolicy(res.JID)
	if err != nil {
		return nil, sendOutput{}, err
	}

	file := wa.OutgoingFile{
		Data:     data,
		FileName: name,
		MimeType: mimeType,
		Caption:  in.Caption,
	}
	sent, err := h.Client.SendFile(ctx, res.JID, file)
	if err != nil {
		h.refundIfNotSent(slot, err)
		return nil, failedSend(sent, res.JID, err), h.sendError(err)
	}
	media := &wa.Media{
		Type:       wa.OutgoingMediaType(file.MimeType),
		MimeType:   file.MimeType,
		FileName:   file.FileName,
		FileLength: uint64(len(data)),
	}
	return nil, h.recordSent(ctx, res.JID, sent, in.Caption, media), nil
}

// openOutgoing opens path for sending through an os.Root anchored at the
// outbox or download directory that contains it, so the file cannot be swapped
// for one outside that directory after the check. Anything inside the data
// directory (databases, token, QR image) is refused unless the containing root
// is itself a subdirectory of the data directory.
func (h *handlers) openOutgoing(path string) (*os.File, string, error) {
	errRefused := errors.New("path must be a regular file inside the outbox or download directory")
	for _, dir := range []string{h.OutboxDir, h.DownloadDir} {
		root, rel, err := datadir.LocateInside(path, dir)
		if err != nil {
			continue
		}
		if h.DataDir != "" && datadir.Within(h.DataDir, filepath.Join(root, rel)) && !datadir.Within(h.DataDir, root) {
			continue
		}
		r, err := os.OpenRoot(root)
		if err != nil {
			continue
		}
		f, err := r.Open(rel)
		_ = r.Close()
		if err != nil {
			return nil, "", errors.New("cannot read file")
		}
		return f, filepath.Base(rel), nil
	}
	return nil, "", errRefused
}

func (h *handlers) checkPolicy(jid string) (ratelimit.Slot, error) {
	if h.Policy == nil {
		return ratelimit.Slot{}, errors.New("send policy is not configured; refusing to send")
	}
	st := h.Client.Status()
	if !st.Paired {
		return ratelimit.Slot{}, wa.ErrNotPaired
	}
	if st.State != wa.StateConnected {
		return ratelimit.Slot{}, wa.ErrNotConnected
	}
	var slot ratelimit.Slot
	var err error
	if jid == "status@broadcast" {
		slot, err = h.Policy.CheckStatusSlot()
	} else {
		slot, err = h.Policy.CheckSlot(jid)
	}
	if err != nil {
		h.Log.Info("send refused by policy", "reason", err.Error())
		return ratelimit.Slot{}, err
	}
	return slot, nil
}

// refundIfNotSent returns the rate slot only when the failure proves nothing
// reached the network: wa.ErrNotConnected, wa.ErrNotPaired or wa.ErrNotSent.
// Any other error, such as an ack timeout, may mean the message was delivered,
// so its slot stays consumed.
func (h *handlers) refundIfNotSent(slot ratelimit.Slot, err error) {
	if errors.Is(err, wa.ErrNotConnected) || errors.Is(err, wa.ErrNotPaired) || errors.Is(err, wa.ErrNotSent) {
		h.Policy.Refund(slot)
	}
}

func (h *handlers) sendError(err error) error {
	if errors.Is(err, wa.ErrMaybeSent) {
		return wa.ErrMaybeSent
	}
	if errors.Is(err, wa.ErrNotSentStale) {
		return errors.New("not_sent: whatsapp connection stale")
	}
	if errors.Is(err, wa.ErrNotConnected) || errors.Is(err, wa.ErrNotPaired) {
		return err
	}
	h.Log.Warn("send failed", "error", redact.Scrub(err.Error()))
	if !errors.Is(err, wa.ErrNotSent) {
		return wa.ErrMaybeSent
	}
	return fmt.Errorf("send failed: %s", redact.Scrub(err.Error()))
}

// recordSent stores the outgoing message. The message is already delivered, so
// a store failure is logged rather than reported as a failed send.
func (h *handlers) recordSent(ctx context.Context, jid string, sent wa.SentMessage, text string, media *wa.Media) sendOutput {
	chat := sent.ChatJID
	if chat == "" {
		chat = jid
	}
	ts := sent.Timestamp
	if ts.IsZero() {
		ts = time.Now()
	}
	msg := wa.Message{
		ID:        sent.ID,
		ChatJID:   chat,
		SenderJID: h.Client.Status().AccountJID,
		Timestamp: ts.UTC(),
		FromMe:    true,
		Text:      text,
		Media:     media,
	}
	if err := h.Store.SaveMessage(ctx, msg); err != nil {
		h.Log.Error("store sent message failed", "error", redact.Scrub(err.Error()))
	}
	h.Log.Info("message sent", "chat", redact.JID(chat))
	return sendOutput{Status: statusSent, MessageID: sent.ID, ChatJID: chat, Timestamp: formatTime(ts)}
}

func readLimited(f *os.File) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(f, maxFileBytes+1))
	if err != nil {
		return nil, errors.New("cannot read file")
	}
	if len(data) > maxFileBytes {
		return nil, fmt.Errorf("file is larger than the %d MiB limit", maxFileBytes>>20)
	}
	return data, nil
}

func mimeFor(path string) string {
	raw := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if raw == "" {
		return "application/octet-stream"
	}
	if mt, _, err := mime.ParseMediaType(raw); err == nil {
		return mt
	}
	return raw
}
