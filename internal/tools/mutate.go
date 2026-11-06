package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmflurry/whatsapp-mcp/internal/redact"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

type editMessageInput struct {
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Optional retry deduplication key (10 minute window)"`
	Chat           string `json:"chat" jsonschema:"Chat JID or international phone number with country code; names are not accepted"`
	MessageID      string `json:"message_id" jsonschema:"ID of the own text message to edit"`
	Text           string `json:"text" jsonschema:"New text, not blank, at most 4096 characters"`
}

type deleteMessageInput struct {
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"Optional retry deduplication key (10 minute window)"`
	Chat           string `json:"chat" jsonschema:"Chat JID or international phone number with country code; names are not accepted"`
	MessageID      string `json:"message_id" jsonschema:"ID of the own message to delete for everyone"`
}

type mutationOutput struct {
	Status    string `json:"status"`
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
	Timestamp string `json:"timestamp"`
}

func (h *handlers) editMessageOnce(ctx context.Context, _ *mcp.CallToolRequest, in editMessageInput) (*mcp.CallToolResult, mutationOutput, error) {
	if strings.TrimSpace(in.Text) == "" {
		return nil, mutationOutput{}, errors.New("text must not be blank")
	}
	if utf8.RuneCountInString(in.Text) > maxTextRunes {
		return nil, mutationOutput{}, fmt.Errorf("text must be at most %d characters", maxTextRunes)
	}
	out, err := h.mutateMessage(ctx, in.Chat, in.MessageID, in.Text, true)
	return nil, out, err
}

func (h *handlers) deleteMessageOnce(ctx context.Context, _ *mcp.CallToolRequest, in deleteMessageInput) (*mcp.CallToolResult, mutationOutput, error) {
	out, err := h.mutateMessage(ctx, in.Chat, in.MessageID, "", false)
	return nil, out, err
}

func (h *handlers) mutateMessage(ctx context.Context, chat, id, text string, edit bool) (mutationOutput, error) {
	if strings.TrimSpace(id) == "" {
		return mutationOutput{}, errors.New("message_id is required")
	}
	chat = strings.TrimSpace(chat)
	if !strings.Contains(chat, "@") {
		if _, ok := wa.NormalizePhone(chat); !ok {
			return mutationOutput{}, errors.New("chat must be a JID or phone number with country code; names are not accepted")
		}
	}
	res, err := h.resolveRecipient(ctx, chat)
	if err != nil {
		return mutationOutput{}, err
	}
	if res.JID == "" {
		return mutationOutput{}, errors.New("chat must resolve to exactly one JID")
	}
	msg, err := h.Store.GetMessage(ctx, res.JID, id)
	if err != nil {
		return mutationOutput{}, h.messageLookupError(ctx, id, err)
	}
	if !msg.FromMe {
		return mutationOutput{}, errors.New("only own messages can be edited or deleted")
	}
	if msg.Revoked {
		return mutationOutput{}, errors.New("message is already revoked")
	}
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	window, label := 48*time.Hour, "48-hour WhatsApp delete-for-everyone"
	if edit {
		if msg.Media != nil && msg.Media.Type != "" {
			return mutationOutput{}, errors.New("only text messages can be edited")
		}
		window, label = 15*time.Minute, "15-minute WhatsApp edit"
	}
	if now.Sub(msg.Timestamp) > window {
		action := "deleted"
		if edit {
			action = "edited"
		}
		return mutationOutput{}, fmt.Errorf("message is older than the %s window; it can no longer be %s", label, action)
	}
	slot, err := h.checkPolicy(res.JID)
	if err != nil {
		return mutationOutput{}, err
	}
	var sent wa.SentMessage
	if edit {
		sent, err = h.Client.EditText(ctx, res.JID, id, text)
	} else {
		sent, err = h.Client.RevokeMessage(ctx, res.JID, id)
	}
	if err != nil {
		h.refundIfNotSent(slot, err)
		out := failedSend(sent, res.JID, err)
		return mutationOutput{Status: out.Status, MessageID: out.MessageID, ChatJID: out.ChatJID}, h.sendError(err)
	}
	at := sent.Timestamp
	if at.IsZero() {
		at = now
	}
	status := "revoked"
	if edit {
		status = "edited"
		err = h.Store.MarkEditedOwn(ctx, res.JID, id, text, at)
	} else {
		err = h.Store.MarkRevokedOwn(ctx, res.JID, id, at)
	}
	// As with recordSent, delivery succeeded even if the local store failed.
	if err != nil {
		h.Log.Error("store message mutation failed", "error", redact.Scrub(err.Error()))
	}
	return mutationOutput{Status: status, MessageID: id, ChatJID: res.JID, Timestamp: formatTime(at)}, nil
}
