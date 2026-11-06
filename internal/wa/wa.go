// Package wa defines the WhatsApp domain types and the Client port used by
// the rest of the server. It deliberately does not import whatsmeow.
package wa

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
)

// JID server names.
const (
	ServerUser       = "s.whatsapp.net"
	ServerGroup      = "g.us"
	ServerLID        = "lid"
	ServerBroadcast  = "broadcast"
	ServerNewsletter = "newsletter"
)

var knownServers = map[string]struct{}{
	ServerUser:       {},
	ServerGroup:      {},
	ServerLID:        {},
	ServerBroadcast:  {},
	ServerNewsletter: {},
}

// IsGroup reports whether jid addresses a group chat.
func IsGroup(jid string) bool { return Server(jid) == ServerGroup }

// Server returns the part of jid after '@', or "" when there is none.
func Server(jid string) string {
	_, server, ok := strings.Cut(jid, "@")
	if !ok {
		return ""
	}
	return server
}

// User returns the part of jid before '@' without ":device" and ".agent"
// suffixes. A string without '@' is treated as a bare user.
func User(jid string) string {
	user, _, _ := strings.Cut(jid, "@")
	if i := strings.IndexByte(user, ':'); i >= 0 {
		user = user[:i]
	}
	if i := strings.IndexByte(user, '.'); i >= 0 {
		user = user[:i]
	}
	return user
}

// NormalizePhone strips formatting characters and the international prefix
// ("+" or "00") from s. ok is true when 7..15 digits remain.
func NormalizePhone(s string) (digits string, ok bool) {
	var b strings.Builder
	plus := false
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && b.Len() == 0 && !plus:
			plus = true
		case r == ' ' || r == '-' || r == '.' || r == '(' || r == ')' || unicode.IsSpace(r):
		default:
			return "", false
		}
	}
	digits = strings.TrimPrefix(b.String(), "00")
	if len(digits) < 7 || len(digits) > 15 {
		return "", false
	}
	return digits, true
}

// UserJID builds the one-to-one chat JID for phoneDigits.
func UserJID(phoneDigits string) string { return phoneDigits + "@" + ServerUser }

// ValidJID reports whether s has a non-empty user and a known server.
func ValidJID(s string) bool {
	user, server, ok := strings.Cut(s, "@")
	if !ok || user == "" {
		return false
	}
	_, known := knownServers[server]
	return known
}

// ConnState is the connection state of the WhatsApp client.
type ConnState string

// Connection states.
const (
	StateDisconnected ConnState = "disconnected"
	StateConnecting   ConnState = "connecting"
	StateNeedsQR      ConnState = "needs_qr_scan"
	StateConnected    ConnState = "connected"
	StateLoggedOut    ConnState = "logged_out"
)

// Status is a snapshot of the client connection.
type Status struct {
	StreamReplacedCount  uint64
	LastDisconnectReason string
	State                ConnState
	AccountJID           string
	Paired               bool
}

// MediaType is the kind of a media attachment.
type MediaType string

// Media types.
const (
	MediaImage    MediaType = "image"
	MediaVideo    MediaType = "video"
	MediaAudio    MediaType = "audio"
	MediaDocument MediaType = "document"
	MediaSticker  MediaType = "sticker"
)

// Media carries the metadata needed to download an attachment later.
type Media struct {
	Type          MediaType
	MimeType      string
	FileName      string
	DirectPath    string
	URL           string
	MediaKey      []byte
	FileSHA256    []byte
	FileEncSHA256 []byte
	FileLength    uint64
}

// Message is a chat message. Text holds the body or the media caption.
type Message struct {
	Edited     bool
	EditedAt   time.Time
	Revoked    bool
	RevokedAt  time.Time
	ID         string
	ChatJID    string
	SenderJID  string
	SenderName string
	Timestamp  time.Time
	FromMe     bool
	Text       string
	Media      *Media
}

// Chat is a conversation, either one-to-one or a group.
type Chat struct {
	JID     string
	Name    string
	IsGroup bool
}

// Contact is an address-book entry. JID is the phone-number JID when it is
// known, otherwise the LID JID; PN and LID hold both forms when known. Name is
// the name saved in the account's address book (full name, else first name).
// BusinessName and PushName are chosen by the contact, not by the account
// owner.
type Contact struct {
	JID          string
	Name         string
	FirstName    string
	BusinessName string
	PushName     string
	PN           string
	LID          string
}

// SentMessage identifies a message accepted by the server.
type SentMessage struct {
	ID        string
	ChatJID   string
	Timestamp time.Time
}

// OutgoingFile is an attachment to send.
type OutgoingFile struct {
	Data     []byte
	FileName string
	MimeType string
	Caption  string
}

// Event is implemented by every value delivered to the client event handler.
type Event interface{ isEvent() }

// QRCodeEvent announces a new pairing code.
type QRCodeEvent struct {
	Code    string
	Timeout time.Duration
}

// QRTimeoutEvent signals that the QR flow ended without pairing.
type QRTimeoutEvent struct{}

// PairedEvent signals a successful pairing.
type PairedEvent struct{ AccountJID string }

// ConnectedEvent signals an established connection.
type ConnectedEvent struct{}

// DisconnectedEvent signals a lost connection.
type DisconnectedEvent struct{}

// LoggedOutEvent signals that the account was unlinked.
type LoggedOutEvent struct{ Reason string }

// MessageEvent carries a live incoming or outgoing message.
type MessageEvent struct{ Message Message }

// HistoryEvent carries one history-sync batch.
type HistoryEvent struct {
	Chats    []Chat
	Messages []Message
}

// ChatInfoEvent carries chat or group name updates.
type ChatInfoEvent struct{ Chats []Chat }

func (QRCodeEvent) isEvent()       {}
func (QRTimeoutEvent) isEvent()    {}
func (PairedEvent) isEvent()       {}
func (ConnectedEvent) isEvent()    {}
func (DisconnectedEvent) isEvent() {}
func (LoggedOutEvent) isEvent()    {}
func (MessageEvent) isEvent()      {}
func (HistoryEvent) isEvent()      {}
func (ChatInfoEvent) isEvent()     {}

// Client is the port to a WhatsApp account.
type Client interface {
	// SetEventHandler must be called before Connect and may be called from
	// any goroutine.
	SetEventHandler(h func(Event))
	// Connect starts the QR flow when the account is not paired, otherwise
	// it connects.
	Connect(ctx context.Context) error
	Disconnect()
	Status() Status
	EditText(ctx context.Context, chatJID, id, text string) (SentMessage, error)
	RevokeMessage(ctx context.Context, chatJID, id string) (SentMessage, error)
	SendText(ctx context.Context, chatJID, text string) (SentMessage, error)
	SendFile(ctx context.Context, chatJID string, f OutgoingFile) (SentMessage, error)
	DownloadMedia(ctx context.Context, m Media) ([]byte, error)
	Contacts(ctx context.Context) ([]Contact, error)
	// ResyncContacts asks WhatsApp for the account's app state (address book,
	// chat settings) in the background. It sends no chat message.
	ResyncContacts(ctx context.Context) (ResyncOutcome, error)
	ContactSync(ctx context.Context) (ContactSyncStatus, error)
	Logout(ctx context.Context) error
}

// Sentinel errors returned by Client implementations.
var (
	ErrNotConnected = errors.New("whatsapp client is not connected")
	ErrNotPaired    = errors.New("whatsapp account is not paired; scan the pairing QR code first")
	// ErrNotSent marks a send failure that happened before anything reached
	// the network, so the message cannot have been delivered. Failures without
	// it (timeouts, server errors) may have been delivered and must not be
	// retried blindly.
	ErrNotSent = errors.New("message was not sent")
)

// StreamReplacedEvent announces another client using this session.
type StreamReplacedEvent struct{}

func (StreamReplacedEvent) isEvent() {}
