// Package store persists chats and messages in a local SQLite database.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/fmflurry/whatsapp-mcp/internal/datadir"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

// Errors returned by lookups.
var (
	ErrNotFound  = errors.New("not found")
	ErrAmbiguous = errors.New("message id matches more than one chat")
)

const (
	defaultLimit = 20
	maxLimit     = 200
	maxContext   = 50

	sortByName = "name"
)

// Store is the message store. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open creates (0600) and migrates the database at path.
func Open(path string) (*Store, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, datadir.FilePerm)
	if err != nil {
		return nil, fmt.Errorf("create message store file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close message store file: %w", err)
	}

	db, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		return nil, fmt.Errorf("open message store: %w", err)
	}
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	if err := migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := datadir.Harden(path, path+"-wal", path+"-shm"); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func dsn(path string) string {
	escaped := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	return "file:" + escaped + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// UpsertChats inserts chats and updates their names; an empty name never
// overwrites a stored one.
func (s *Store) UpsertChats(ctx context.Context, chats []wa.Chat) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := upsertChats(ctx, tx, chats); err != nil {
		return err
	}
	return tx.Commit()
}

// SaveMessage stores m, ensures its chat exists and advances the chat's last
// message time. An existing message is refreshed only when it comes from the
// same sender; a copy claiming another sender leaves the row untouched.
func (s *Store) SaveMessage(ctx context.Context, m wa.Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := saveMessage(ctx, tx, m); err != nil {
		return err
	}
	return tx.Commit()
}

// ApplyEdit replaces the text of the stored message id in chatJID sent by
// senderJID. It changes nothing else and does nothing when no such message
// exists.
func (s *Store) ApplyEdit(ctx context.Context, chatJID, id, senderJID, newText string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE messages SET text = ? WHERE chat_jid = ? AND id = ? AND sender_jid = ?",
		newText, chatJID, id, senderJID)
	if err != nil {
		return fmt.Errorf("apply edit: %w", err)
	}
	return nil
}

// UpdateChatNames sets the name of existing chats that have none. It never
// creates chats and never overwrites a stored name.
func (s *Store) UpdateChatNames(ctx context.Context, chats []wa.Chat) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, c := range chats {
		if c.JID == "" || c.Name == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE chats SET name = ? WHERE jid = ? AND name = ''", c.Name, c.JID); err != nil {
			return fmt.Errorf("update chat name: %w", err)
		}
	}
	return tx.Commit()
}

// SaveHistory stores a history batch in a single transaction.
func (s *Store) SaveHistory(ctx context.Context, chats []wa.Chat, msgs []wa.Message) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := upsertChats(ctx, tx, chats); err != nil {
		return err
	}
	for _, m := range msgs {
		if err := saveMessage(ctx, tx, m); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func upsertChats(ctx context.Context, ex execer, chats []wa.Chat) error {
	for _, c := range chats {
		if c.JID == "" {
			continue
		}
		_, err := ex.ExecContext(ctx, `
INSERT INTO chats (jid, name, is_group) VALUES (?, ?, ?)
ON CONFLICT(jid) DO UPDATE SET
	name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE chats.name END,
	is_group = excluded.is_group`,
			c.JID, c.Name, boolInt(c.IsGroup))
		if err != nil {
			return fmt.Errorf("upsert chat: %w", err)
		}
	}
	return nil
}

func saveMessage(ctx context.Context, ex execer, m wa.Message) error {
	if m.ChatJID == "" || m.ID == "" {
		return errors.New("message requires chat jid and id")
	}
	ts := timeToInt(m.Timestamp)
	_, err := ex.ExecContext(ctx, `
INSERT INTO chats (jid, name, is_group, last_message_time) VALUES (?, '', ?, ?)
ON CONFLICT(jid) DO UPDATE SET last_message_time = MAX(chats.last_message_time, excluded.last_message_time)`,
		m.ChatJID, boolInt(wa.IsGroup(m.ChatJID)), ts)
	if err != nil {
		return fmt.Errorf("ensure chat: %w", err)
	}

	var media wa.Media
	if m.Media != nil {
		media = *m.Media
	}
	_, err = ex.ExecContext(ctx, `
INSERT INTO messages (chat_jid, id, sender_jid, sender_name, ts, from_me, text,
	media_type, mime_type, file_name, direct_path, url,
	media_key, file_sha256, file_enc_sha256, file_length)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(chat_jid, id) DO UPDATE SET
	sender_name = CASE WHEN excluded.sender_name <> '' THEN excluded.sender_name ELSE messages.sender_name END,
	ts = excluded.ts,
	from_me = excluded.from_me,
	text = excluded.text,
	media_type = excluded.media_type,
	mime_type = excluded.mime_type,
	file_name = excluded.file_name,
	direct_path = excluded.direct_path,
	url = excluded.url,
	media_key = excluded.media_key,
	file_sha256 = excluded.file_sha256,
	file_enc_sha256 = excluded.file_enc_sha256,
	file_length = excluded.file_length
WHERE messages.sender_jid = excluded.sender_jid`,
		m.ChatJID, m.ID, m.SenderJID, m.SenderName, ts, boolInt(m.FromMe), m.Text,
		string(media.Type), media.MimeType, media.FileName, media.DirectPath, media.URL,
		media.MediaKey, media.FileSHA256, media.FileEncSHA256, int64(media.FileLength))
	if err != nil {
		return fmt.Errorf("save message: %w", err)
	}
	return nil
}

// ChatSummary is a chat with a preview of its latest message.
type ChatSummary struct {
	JID               string
	Name              string
	IsGroup           bool
	LastMessageTime   time.Time
	LastMessageText   string
	LastMessageFromMe bool
	LastSenderJID     string
}

// ChatQuery filters and pages ListChats. SortBy is "last_active" (default) or
// "name".
type ChatQuery struct {
	Query         string
	Limit, Offset int
	SortBy        string
}

const chatSelect = `
SELECT c.jid, c.name, c.is_group, c.last_message_time,
	COALESCE(m.text, ''), COALESCE(m.from_me, 0), COALESCE(m.sender_jid, '')
FROM chats c
LEFT JOIN messages m ON m.rowid = (
	SELECT rowid FROM messages WHERE chat_jid = c.jid ORDER BY ts DESC, id DESC LIMIT 1
)`

// ListChats returns chats matching q (case-insensitive substring of name or
// jid) and whether more results exist beyond the page.
func (s *Store) ListChats(ctx context.Context, q ChatQuery) (chats []ChatSummary, hasMore bool, err error) {
	limit, offset := clampPage(q.Limit, q.Offset)

	query := chatSelect
	var args []any
	if q.Query != "" {
		pat := likePattern(q.Query)
		query += " WHERE (c.name LIKE ? ESCAPE '\\' OR c.jid LIKE ? ESCAPE '\\')"
		args = append(args, pat, pat)
	}
	if q.SortBy == sortByName {
		query += " ORDER BY COALESCE(NULLIF(c.name, ''), c.jid) COLLATE NOCASE, c.jid"
	} else {
		query += " ORDER BY c.last_message_time DESC, c.jid"
	}
	query += " LIMIT ? OFFSET ?"
	args = append(args, limit+1, offset)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("list chats: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, false, err
		}
		chats = append(chats, c)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("list chats: %w", err)
	}
	if len(chats) > limit {
		chats, hasMore = chats[:limit], true
	}
	return chats, hasMore, nil
}

// GetChat returns the chat with jid or ErrNotFound.
func (s *Store) GetChat(ctx context.Context, jid string) (ChatSummary, error) {
	rows, err := s.db.QueryContext(ctx, chatSelect+" WHERE c.jid = ?", jid)
	if err != nil {
		return ChatSummary{}, fmt.Errorf("get chat: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return ChatSummary{}, fmt.Errorf("get chat: %w", err)
		}
		return ChatSummary{}, ErrNotFound
	}
	return scanChat(rows)
}

func scanChat(rows *sql.Rows) (ChatSummary, error) {
	var (
		c                  ChatSummary
		isGroup, fromMe, t int64
	)
	if err := rows.Scan(&c.JID, &c.Name, &isGroup, &t, &c.LastMessageText, &fromMe, &c.LastSenderJID); err != nil {
		return ChatSummary{}, fmt.Errorf("scan chat: %w", err)
	}
	c.IsGroup = isGroup != 0
	c.LastMessageFromMe = fromMe != 0
	c.LastMessageTime = intToTime(t)
	return c, nil
}

// MessageQuery filters and pages ListMessages. After is inclusive and Before
// exclusive; zero times are unbounded. Results are newest first unless Oldest
// is set.
type MessageQuery struct {
	ChatJID, SenderJID, Text string
	After, Before            time.Time
	Limit, Offset            int
	Oldest                   bool
}

const messageSelect = `
SELECT chat_jid, id, sender_jid, sender_name, ts, from_me, text,
	media_type, mime_type, file_name, direct_path, url,
	media_key, file_sha256, file_enc_sha256, file_length
FROM messages`

// ListMessages returns messages matching q and whether more exist beyond the
// page.
func (s *Store) ListMessages(ctx context.Context, q MessageQuery) (msgs []wa.Message, hasMore bool, err error) {
	limit, offset := clampPage(q.Limit, q.Offset)

	var (
		where []string
		args  []any
	)
	if q.ChatJID != "" {
		where = append(where, "chat_jid = ?")
		args = append(args, q.ChatJID)
	}
	if q.SenderJID != "" {
		where = append(where, "sender_jid = ?")
		args = append(args, q.SenderJID)
	}
	if q.Text != "" {
		where = append(where, "text LIKE ? ESCAPE '\\'")
		args = append(args, likePattern(q.Text))
	}
	if !q.After.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, timeToInt(q.After))
	}
	if !q.Before.IsZero() {
		where = append(where, "ts < ?")
		args = append(args, timeToInt(q.Before))
	}

	query := messageSelect
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	if q.Oldest {
		query += " ORDER BY ts ASC, id ASC"
	} else {
		query += " ORDER BY ts DESC, id DESC"
	}
	query += " LIMIT ? OFFSET ?"
	args = append(args, limit+1, offset)

	msgs, err = s.queryMessages(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	if len(msgs) > limit {
		msgs, hasMore = msgs[:limit], true
	}
	return msgs, hasMore, nil
}

// GetMessage returns the message with id. An empty chatJID searches all chats
// and yields ErrAmbiguous when several chats hold that id.
func (s *Store) GetMessage(ctx context.Context, chatJID, id string) (wa.Message, error) {
	query := messageSelect + " WHERE id = ?"
	args := []any{id}
	if chatJID != "" {
		query += " AND chat_jid = ?"
		args = append(args, chatJID)
	}
	query += " ORDER BY chat_jid LIMIT 2"

	msgs, err := s.queryMessages(ctx, query, args...)
	if err != nil {
		return wa.Message{}, err
	}
	switch len(msgs) {
	case 0:
		return wa.Message{}, ErrNotFound
	case 1:
		return msgs[0], nil
	default:
		return wa.Message{}, ErrAmbiguous
	}
}

// MessageChats lists the chat JIDs that contain a message with id, for
// reporting ErrAmbiguous to callers.
func (s *Store) MessageChats(ctx context.Context, id string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT chat_jid FROM messages WHERE id = ? ORDER BY chat_jid", id)
	if err != nil {
		return nil, fmt.Errorf("list message chats: %w", err)
	}
	defer rows.Close()
	var jids []string
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err != nil {
			return nil, fmt.Errorf("scan chat jid: %w", err)
		}
		jids = append(jids, jid)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list message chats: %w", err)
	}
	return jids, nil
}

// MessageContext returns the target message with up to before older and after
// newer messages of the same chat, both in chronological order.
func (s *Store) MessageContext(ctx context.Context, chatJID, id string, before, after int) (older []wa.Message, target wa.Message, newer []wa.Message, err error) {
	target, err = s.GetMessage(ctx, chatJID, id)
	if err != nil {
		return nil, wa.Message{}, nil, err
	}
	ts := timeToInt(target.Timestamp)

	if n := clamp(before, 0, maxContext); n > 0 {
		older, err = s.queryMessages(ctx, messageSelect+`
WHERE chat_jid = ? AND (ts < ? OR (ts = ? AND id < ?))
ORDER BY ts DESC, id DESC LIMIT ?`, target.ChatJID, ts, ts, target.ID, n)
		if err != nil {
			return nil, wa.Message{}, nil, err
		}
		slices.Reverse(older)
	}
	if n := clamp(after, 0, maxContext); n > 0 {
		newer, err = s.queryMessages(ctx, messageSelect+`
WHERE chat_jid = ? AND (ts > ? OR (ts = ? AND id > ?))
ORDER BY ts ASC, id ASC LIMIT ?`, target.ChatJID, ts, ts, target.ID, n)
		if err != nil {
			return nil, wa.Message{}, nil, err
		}
	}
	return older, target, newer, nil
}

func (s *Store) queryMessages(ctx context.Context, query string, args ...any) ([]wa.Message, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()
	var msgs []wa.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	return msgs, nil
}

func scanMessage(rows *sql.Rows) (wa.Message, error) {
	var (
		m          wa.Message
		ts         int64
		fromMe     int64
		length     int64
		mediaType  string
		media      wa.Media
		mediaKey   []byte
		sha, encSH []byte
	)
	err := rows.Scan(&m.ChatJID, &m.ID, &m.SenderJID, &m.SenderName, &ts, &fromMe, &m.Text,
		&mediaType, &media.MimeType, &media.FileName, &media.DirectPath, &media.URL,
		&mediaKey, &sha, &encSH, &length)
	if err != nil {
		return wa.Message{}, fmt.Errorf("scan message: %w", err)
	}
	m.Timestamp = intToTime(ts)
	m.FromMe = fromMe != 0
	if mediaType != "" {
		media.Type = wa.MediaType(mediaType)
		media.MediaKey, media.FileSHA256, media.FileEncSHA256 = mediaKey, sha, encSH
		if length > 0 {
			media.FileLength = uint64(length)
		}
		m.Media = &media
	}
	return m, nil
}

func likePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

func clampPage(limit, offset int) (int, int) {
	if limit == 0 {
		limit = defaultLimit
	}
	return clamp(limit, 1, maxLimit), max(offset, 0)
}

func clamp(v, lo, hi int) int { return min(max(v, lo), hi) }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func timeToInt(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func intToTime(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n).UTC()
}
