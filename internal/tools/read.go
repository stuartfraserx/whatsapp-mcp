package tools

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmflurry/whatsapp-mcp/internal/store"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

type listChatsInput struct {
	Query  string `json:"query,omitempty" jsonschema:"Words matched against the chat name, the contact's saved, business or profile name, phone number or JID; accents and case are ignored"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum chats to return, 1-200 (default 20)"`
	Offset int    `json:"offset,omitempty" jsonschema:"Number of chats to skip, for pagination"`
	SortBy string `json:"sort_by,omitempty" jsonschema:"Sort order: last_active (default) or name"`
}

type chatOut struct {
	JID               string `json:"jid"`
	Name              string `json:"name"`
	IsGroup           bool   `json:"is_group"`
	LastMessageTime   string `json:"last_message_time,omitempty"`
	LastMessageText   string `json:"last_message_text,omitempty"`
	LastMessageFromMe bool   `json:"last_message_from_me"`
	LastSenderJID     string `json:"last_sender_jid,omitempty"`
}

type listChatsOutput struct {
	Chats      []chatOut `json:"chats"`
	HasMore    bool      `json:"has_more"`
	NextOffset int       `json:"next_offset,omitempty"`
}

func (h *handlers) listChats(ctx context.Context, _ *mcp.CallToolRequest, in listChatsInput) (*mcp.CallToolResult, listChatsOutput, error) {
	limit, offset, err := pageParams(in.Limit, in.Offset)
	if err != nil {
		return nil, listChatsOutput{}, err
	}
	switch in.SortBy {
	case "", "last_active", "name":
	default:
		return nil, listChatsOutput{}, errors.New(`sort_by must be "last_active" or "name"`)
	}
	query := strings.TrimSpace(in.Query)
	dir := h.contactDirectory(ctx)
	chats, hasMore, err := h.findChats(ctx, dir, query, in.SortBy, limit, offset)
	if err != nil {
		return nil, listChatsOutput{}, err
	}
	out := listChatsOutput{Chats: make([]chatOut, 0, len(chats)), HasMore: hasMore, NextOffset: nextOffset(hasMore, offset, len(chats))}
	for _, c := range chats {
		out.Chats = append(out.Chats, chatOut{
			JID:               c.JID,
			Name:              dir.chatName(c),
			IsGroup:           c.IsGroup,
			LastMessageTime:   formatTime(c.LastMessageTime),
			LastMessageText:   c.LastMessageText,
			LastMessageFromMe: c.LastMessageFromMe,
			LastSenderJID:     c.LastSenderJID,
		})
	}
	return nil, out, nil
}

// findChats pages the stored chats. A query is matched here rather than in
// the store so that it also finds chats by the address-book, business or push
// name of the contact, ignoring accents; past listChatsScanPages pages it falls
// back to the store's substring match on the stored name and JID.
func (h *handlers) findChats(ctx context.Context, dir *directory, query, sortBy string, limit, offset int) ([]store.ChatSummary, bool, error) {
	if query != "" {
		all, truncated, err := h.scanChats(ctx, sortBy, listChatsScanPages)
		if err != nil {
			return nil, false, err
		}
		if !truncated {
			matched := slices.DeleteFunc(all, func(c store.ChatSummary) bool { return !chatMatches(query, c, dir) })
			if offset >= len(matched) {
				return nil, false, nil
			}
			end := min(len(matched), offset+limit)
			return matched[offset:end], end < len(matched), nil
		}
	}
	chats, hasMore, err := h.Store.ListChats(ctx, store.ChatQuery{Query: query, Limit: limit, Offset: offset, SortBy: sortBy})
	if err != nil {
		return nil, false, fmt.Errorf("list chats: %w", err)
	}
	return chats, hasMore, nil
}

func chatMatches(query string, c store.ChatSummary, dir *directory) bool {
	if strings.Contains(strings.ToLower(wa.User(c.JID)), strings.ToLower(query)) {
		return true
	}
	f := matchFields{names: []string{c.Name}, pn: phoneOf(c.JID), lid: lidUser(c.JID)}
	if p := dir.lookup(c.JID); p != nil && !c.IsGroup {
		f = p.fields()
		f.names = append(f.names, c.Name)
	}
	_, ok := parseContactQuery(query).match(f)
	return ok
}

type listMessagesInput struct {
	ChatJID     string `json:"chat_jid,omitempty" jsonschema:"Only messages in this chat JID"`
	SenderJID   string `json:"sender_jid,omitempty" jsonschema:"Only messages from this sender JID"`
	Query       string `json:"query,omitempty" jsonschema:"Case-insensitive text to search for in message bodies"`
	After       string `json:"after,omitempty" jsonschema:"Only messages at or after this RFC3339 time, e.g. 2025-01-31T09:00:00Z"`
	Before      string `json:"before,omitempty" jsonschema:"Only messages before this RFC3339 time"`
	Limit       int    `json:"limit,omitempty" jsonschema:"Maximum messages to return, 1-200 (default 20)"`
	Offset      int    `json:"offset,omitempty" jsonschema:"Number of messages to skip, for pagination"`
	OldestFirst bool   `json:"oldest_first,omitempty" jsonschema:"Return oldest messages first instead of newest first"`
}

type messageOut struct {
	Edited     bool   `json:"edited,omitempty"`
	Revoked    bool   `json:"revoked,omitempty"`
	EditedAt   string `json:"edited_at,omitempty"`
	RevokedAt  string `json:"revoked_at,omitempty"`
	ID         string `json:"id"`
	ChatJID    string `json:"chat_jid"`
	SenderJID  string `json:"sender_jid"`
	SenderName string `json:"sender_name"`
	Timestamp  string `json:"timestamp"`
	FromMe     bool   `json:"from_me"`
	Text       string `json:"text"`
	MediaType  string `json:"media_type,omitempty"`
	FileName   string `json:"file_name,omitempty"`
}

func toMessageOut(m wa.Message, dir *directory) messageOut {
	out := messageOut{
		Edited: m.Edited, Revoked: m.Revoked, EditedAt: formatTime(m.EditedAt), RevokedAt: formatTime(m.RevokedAt),
		ID:         m.ID,
		ChatJID:    m.ChatJID,
		SenderJID:  m.SenderJID,
		SenderName: dir.senderName(m),
		Timestamp:  formatTime(m.Timestamp),
		FromMe:     m.FromMe,
		Text:       m.Text,
	}
	if m.Media != nil {
		out.MediaType = string(m.Media.Type)
		out.FileName = m.Media.FileName
	}
	return out
}

func toMessagesOut(msgs []wa.Message, dir *directory) []messageOut {
	out := make([]messageOut, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, toMessageOut(m, dir))
	}
	return out
}

type listMessagesOutput struct {
	Messages   []messageOut `json:"messages"`
	HasMore    bool         `json:"has_more"`
	NextOffset int          `json:"next_offset,omitempty"`
}

func parseTime(field, v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be an RFC3339 timestamp such as 2025-01-31T09:00:00Z", field)
	}
	return t, nil
}

var (
	minStoreTime = time.Unix(0, math.MinInt64)
	maxStoreTime = time.Unix(0, math.MaxInt64)
)

// clampToStoreRange pins t to the span the store can represent as UnixNano
// (about 1678 to 2261); beyond it the conversion would overflow and wrap.
func clampToStoreRange(t time.Time) time.Time {
	switch {
	case t.IsZero():
		return t
	case t.Before(minStoreTime):
		return minStoreTime
	case t.After(maxStoreTime):
		return maxStoreTime
	default:
		return t
	}
}

func validateOptionalJID(field, v string) error {
	if v != "" && !wa.ValidJID(v) {
		return fmt.Errorf("%s is not a valid WhatsApp JID", field)
	}
	return nil
}

func (h *handlers) listMessages(ctx context.Context, _ *mcp.CallToolRequest, in listMessagesInput) (*mcp.CallToolResult, listMessagesOutput, error) {
	limit, offset, err := pageParams(in.Limit, in.Offset)
	if err != nil {
		return nil, listMessagesOutput{}, err
	}
	if err := validateOptionalJID("chat_jid", in.ChatJID); err != nil {
		return nil, listMessagesOutput{}, err
	}
	if err := validateOptionalJID("sender_jid", in.SenderJID); err != nil {
		return nil, listMessagesOutput{}, err
	}
	after, err := parseTime("after", in.After)
	if err != nil {
		return nil, listMessagesOutput{}, err
	}
	before, err := parseTime("before", in.Before)
	if err != nil {
		return nil, listMessagesOutput{}, err
	}
	if !after.IsZero() && !before.IsZero() && !after.Before(before) {
		return nil, listMessagesOutput{}, errors.New("after must be earlier than before")
	}

	msgs, hasMore, err := h.Store.ListMessages(ctx, store.MessageQuery{
		ChatJID:   in.ChatJID,
		SenderJID: in.SenderJID,
		Text:      in.Query,
		After:     clampToStoreRange(after),
		Before:    clampToStoreRange(before),
		Limit:     limit,
		Offset:    offset,
		Oldest:    in.OldestFirst,
	})
	if err != nil {
		return nil, listMessagesOutput{}, fmt.Errorf("list messages: %w", err)
	}
	return nil, listMessagesOutput{
		Messages:   toMessagesOut(msgs, h.contactDirectory(ctx)),
		HasMore:    hasMore,
		NextOffset: nextOffset(hasMore, offset, len(msgs)),
	}, nil
}

type messageContextInput struct {
	MessageID string `json:"message_id" jsonschema:"ID of the target message"`
	ChatJID   string `json:"chat_jid,omitempty" jsonschema:"Chat containing the message; required when the ID exists in several chats"`
	Before    *int   `json:"before,omitempty" jsonschema:"Messages to include before the target, 0-50 (default 5)"`
	After     *int   `json:"after,omitempty" jsonschema:"Messages to include after the target, 0-50 (default 5)"`
}

type messageContextOutput struct {
	Before  []messageOut `json:"before"`
	Message messageOut   `json:"message"`
	After   []messageOut `json:"after"`
}

func contextCount(field string, v *int) (int, error) {
	if v == nil {
		return defaultCtx, nil
	}
	if *v < 0 || *v > maxContext {
		return 0, fmt.Errorf("%s must be between 0 and %d", field, maxContext)
	}
	return *v, nil
}

func (h *handlers) getMessageContext(ctx context.Context, _ *mcp.CallToolRequest, in messageContextInput) (*mcp.CallToolResult, messageContextOutput, error) {
	id := strings.TrimSpace(in.MessageID)
	if id == "" {
		return nil, messageContextOutput{}, errors.New("message_id is required")
	}
	if err := validateOptionalJID("chat_jid", in.ChatJID); err != nil {
		return nil, messageContextOutput{}, err
	}
	before, err := contextCount("before", in.Before)
	if err != nil {
		return nil, messageContextOutput{}, err
	}
	after, err := contextCount("after", in.After)
	if err != nil {
		return nil, messageContextOutput{}, err
	}

	older, target, newer, err := h.Store.MessageContext(ctx, in.ChatJID, id, before, after)
	if err != nil {
		return nil, messageContextOutput{}, h.messageLookupError(ctx, id, err)
	}
	dir := h.contactDirectory(ctx)
	return nil, messageContextOutput{
		Before:  toMessagesOut(older, dir),
		Message: toMessageOut(target, dir),
		After:   toMessagesOut(newer, dir),
	}, nil
}

// messageLookupError turns store lookup failures into user-facing errors; an
// ambiguous ID lists the chats that hold it.
func (h *handlers) messageLookupError(ctx context.Context, id string, err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return errors.New("message not found")
	case errors.Is(err, store.ErrAmbiguous):
		chats, lerr := h.Store.MessageChats(ctx, id)
		if lerr != nil || len(chats) == 0 {
			return errors.New("message_id exists in several chats; pass chat_jid")
		}
		return fmt.Errorf("message_id exists in several chats; pass chat_jid as one of: %s", strings.Join(chats, ", "))
	default:
		return fmt.Errorf("look up message: %w", err)
	}
}
