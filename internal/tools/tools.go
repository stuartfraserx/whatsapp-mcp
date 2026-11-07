// Package tools registers the WhatsApp MCP tools on an MCP server.
package tools

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmflurry/whatsapp-mcp/internal/notify"
	"github.com/fmflurry/whatsapp-mcp/internal/sendpolicy"
	"github.com/fmflurry/whatsapp-mcp/internal/session"
	"github.com/fmflurry/whatsapp-mcp/internal/store"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

// MaxDownloadBytes is the largest media file download_media will fetch.
const MaxDownloadBytes = 100 << 20

const (
	defaultLimit = 20
	maxLimit     = 200
	maxContext   = 50
	defaultCtx   = 5
)

// Deps are the collaborators the tools need. RateInterval, RateHourly and
// AllowlistActive are informational and surface in connection_status.
type Deps struct {
	Notifier interface{ Status() notify.Status }
	Client   wa.Client
	Store    *store.Store
	Session  *session.Session
	Policy   *sendpolicy.Policy

	// Now overrides the clock used for edit and revoke windows.
	Now         func() time.Time
	SendEnabled bool
	DataDir     string
	DownloadDir string
	OutboxDir   string
	Log         *slog.Logger

	// DownloadLimit overrides MaxDownloadBytes when positive.
	DownloadLimit int64

	RateInterval    time.Duration
	RateHourly      int
	AllowlistActive bool
}

type handlers struct {
	Deps
	attemptsMu sync.Mutex
	attempts   map[string]*attemptLock
}

// Register adds the tools to s. The send tools are registered only when
// d.SendEnabled is set.
func Register(s *mcp.Server, d Deps) {
	if d.Log == nil {
		d.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	h := &handlers{Deps: d}

	mcp.AddTool(s, readOnlyTool("list_chats", "List WhatsApp chats from the local message store, most recently active first.", "List chats"), h.listChats)
	mcp.AddTool(s, readOnlyTool("search_contacts", "Search contacts and direct chats by saved name, business or profile name, phone number (any format) or JID. Accents and case are ignored; every word must match, in any order.", "Search contacts"), h.searchContacts)
	mcp.AddTool(s, readOnlyTool("list_messages", "List or search stored messages, filtered by chat, sender, text and date range.", "List messages"), h.listMessages)
	mcp.AddTool(s, readOnlyTool("get_message_context", "Return a stored message with the messages that precede and follow it in the same chat.", "Get message context"), h.getMessageContext)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "download_media",
		Title:       "Download media",
		Description: "Download the media attached to a stored message into the server's download directory. Never overwrites existing files.",
		Annotations: &mcp.ToolAnnotations{
			Title:           "Download media",
			DestructiveHint: ptr(false),
			OpenWorldHint:   ptr(true),
		},
	}, h.downloadMedia)
	mcp.AddTool(s, readOnlyTool("connection_status", "Report the WhatsApp connection state, pairing state, send safety settings and address-book sync state (counts only).", "Connection status"), h.connectionStatus)
	mcp.AddTool(s, &mcp.Tool{
		Name:  "resync_contacts",
		Title: "Resync contacts",
		Description: "Ask WhatsApp to send this linked device the account's address book (saved contact names) and chat settings again, " +
			"in the background. It does not send any chat message to anyone and changes nothing on the phone; " +
			"if a decryption key is missing, WhatsApp's library asks your own phone to share it again. " +
			"Use it when search_contacts finds no saved names; follow progress in connection_status.",
		Annotations: &mcp.ToolAnnotations{
			Title:           "Resync contacts",
			DestructiveHint: ptr(false),
			IdempotentHint:  true,
			OpenWorldHint:   ptr(true),
		},
	}, h.resyncContacts)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "show_pairing_qr",
		Title:       "Show pairing QR",
		Description: "Start WhatsApp pairing and return a text QR, raw pairing code, expiry and server PNG path. Treat the pairing code as sensitive. Scan it from WhatsApp > Linked devices.",
		Annotations: &mcp.ToolAnnotations{
			Title:           "Show pairing QR",
			DestructiveHint: ptr(false),
			IdempotentHint:  true,
			OpenWorldHint:   ptr(true),
		},
	}, h.showPairingQR)

	if !d.SendEnabled {
		return
	}
	mcp.AddTool(s, sendTool("send_message", "Send message",
		"Send a text message to one WhatsApp recipient (JID, phone number or exact contact/chat name). "+sendWarning), h.sendMessage)
	mcp.AddTool(s, sendTool("edit_message", "Edit message", "Edit an own text message within 15 minutes of its original timestamp. Acts on the user's real WhatsApp account and is visible to the other people in the chat. Only use for a message the user explicitly asked to edit."), h.editMessage)
	mcp.AddTool(s, sendTool("delete_message", "Delete message", "Delete an own message for everyone within 48 hours of its original timestamp. Acts on the user's real WhatsApp account and is visible to the other people in the chat. Only use for a message the user explicitly asked to delete."), h.deleteMessage)
	mcp.AddTool(s, sendTool("send_file", "Send file",
		"Send one file from the server's outbox or download directory to one WhatsApp recipient (JID, phone number or exact contact/chat name). "+sendWarning), h.sendFile)
}

const sendWarning = "Sends immediately and irreversibly. Only call when the user explicitly asked to send this exact message to this exact recipient."

func readOnlyTool(name, description, title string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Title:       title,
		Description: description,
		Annotations: &mcp.ToolAnnotations{
			Title:         title,
			ReadOnlyHint:  true,
			OpenWorldHint: ptr(false),
		},
	}
}

func sendTool(name, title, description string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Title:       title,
		Description: description,
		Annotations: &mcp.ToolAnnotations{
			Title:           title,
			DestructiveHint: ptr(true),
			OpenWorldHint:   ptr(true),
		},
	}
}

func ptr[T any](v T) *T { return &v }

// pageParams validates limit/offset and applies the default limit.
func pageParams(limit, offset int) (int, int, error) {
	if limit == 0 {
		limit = defaultLimit
	}
	if limit < 0 || limit > maxLimit {
		return 0, 0, fmt.Errorf("limit must be between 1 and %d", maxLimit)
	}
	if offset < 0 {
		return 0, 0, errors.New("offset must not be negative")
	}
	return limit, offset, nil
}

func nextOffset(hasMore bool, offset, count int) int {
	if !hasMore {
		return 0
	}
	return offset + count
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
