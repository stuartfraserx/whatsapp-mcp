package tools

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/fmflurry/whatsapp-mcp/internal/datadir"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const attemptWindow = 10 * time.Minute

type attemptRecord struct {
	At     time.Time
	State  string
	Output json.RawMessage
	Error  string
}
type attemptLock struct {
	sync.Mutex
	record *attemptRecord
	users  int
}

func digest(parts ...string) string {
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func (h *handlers) attemptKey(ctx context.Context, op, key, recipient string, parts ...string) string {
	if key != "" {
		return digest(op, "client", key)
	}
	res, err := h.resolveRecipient(ctx, recipient)
	if err == nil && res.JID != "" {
		recipient = res.JID
	}
	return digest(append([]string{op, recipient}, parts...)...)
}

// attempt serializes duplicates through the completed result, before rate-slot
// allocation. The durable in_flight row also protects retries after a crash.
func attempt[O any](h *handlers, ctx context.Context, key string, run func() (*mcp.CallToolResult, O, error)) (*mcp.CallToolResult, O, error) {
	now := time.Now()
	if h.Now != nil {
		now = h.Now()
	}
	h.attemptsMu.Lock()
	if h.attempts == nil {
		h.attempts = make(map[string]*attemptLock)
	}
	for k, entry := range h.attempts {
		if entry.users == 0 && entry.TryLock() {
			if entry.record == nil || now.Sub(entry.record.At) >= attemptWindow {
				delete(h.attempts, k)
			}
			entry.Unlock()
		}
	}
	lock := h.attempts[key]
	if lock == nil {
		lock = &attemptLock{}
		h.attempts[key] = lock
	}
	lock.users++
	h.attemptsMu.Unlock()
	lock.Lock()
	defer func() { lock.Unlock(); h.attemptsMu.Lock(); lock.users--; h.attemptsMu.Unlock() }()
	var zero O
	var db *sql.DB
	if h.DataDir != "" {
		path := filepath.Join(h.DataDir, "send_attempts.db")
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, datadir.FilePerm)
		if err != nil {
			return nil, zero, errors.New("cannot open send attempt database")
		}
		_ = f.Close()
		if err = os.Chmod(path, datadir.FilePerm); err != nil {
			return nil, zero, errors.New("cannot secure send attempt database")
		}
		db, err = sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(5000)"}).String())
		if err != nil {
			return nil, zero, errors.New("cannot open send attempt database")
		}
		defer db.Close()
		db.SetMaxOpenConns(1)
		_, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS attempts(key TEXT PRIMARY KEY, at INTEGER NOT NULL, record BLOB NOT NULL)`)
		if err != nil {
			return nil, zero, errors.New("cannot initialize send attempt database")
		}
		if _, err = db.ExecContext(ctx, "DELETE FROM attempts WHERE at < ?", now.Add(-attemptWindow).Unix()); err != nil {
			return nil, zero, errors.New("cannot prune send attempt database")
		}
		if lock.record == nil {
			var raw []byte
			err = db.QueryRowContext(ctx, "SELECT record FROM attempts WHERE key=?", key).Scan(&raw)
			if err == nil {
				var record attemptRecord
				if json.Unmarshal(raw, &record) != nil {
					return nil, zero, errors.New("invalid send attempt record")
				}
				lock.record = &record
			} else if !errors.Is(err, sql.ErrNoRows) {
				return nil, zero, errors.New("cannot read send attempt database")
			}
		}
	}
	if r := lock.record; r != nil && now.Sub(r.At) < attemptWindow && r.State != "not_sent" {
		var out O
		if len(r.Output) > 0 {
			_ = json.Unmarshal(r.Output, &out)
		}
		var err error
		if r.Error != "" {
			err = errors.New(r.Error)
		}
		if r.State == "in_flight" {
			err = wa.ErrMaybeSent
			raw, _ := json.Marshal(map[string]string{"status": "unknown_maybe_sent"})
			_ = json.Unmarshal(raw, &out)
		}
		return attemptResult(out, err)
	}
	save := func(r *attemptRecord) error {
		lock.record = r
		if db == nil {
			return nil
		}
		raw, _ := json.Marshal(r)
		_, err := db.ExecContext(ctx, `INSERT INTO attempts(key,at,record) VALUES(?,?,?) ON CONFLICT(key) DO UPDATE SET at=excluded.at, record=excluded.record`, key, r.At.Unix(), raw)
		return err
	}
	if err := save(&attemptRecord{At: now, State: "in_flight"}); err != nil {
		lock.record = nil
		return nil, zero, errors.New("cannot persist send attempt")
	}
	result, out, err := run()
	raw, _ := json.Marshal(out)
	r := &attemptRecord{At: now, State: "sent", Output: raw}
	if err != nil {
		r.State = "not_sent"
		r.Error = err.Error()
		if errors.Is(err, wa.ErrMaybeSent) {
			r.State = "unknown"
		}
	}
	// Preserve a conservative in_flight disk record if final persistence fails.
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	ctx = finishCtx
	_ = save(r)
	if errors.Is(err, wa.ErrMaybeSent) {
		return attemptResult(out, err)
	}
	return result, out, err
}
func attemptResult[O any](out O, err error) (*mcp.CallToolResult, O, error) {
	if err != nil && err.Error() == wa.ErrMaybeSent.Error() {
		return &mcp.CallToolResult{IsError: true}, out, nil
	}
	return nil, out, err
}
func failedSend(sent wa.SentMessage, jid string, err error) sendOutput {
	if errors.Is(err, wa.ErrMaybeSent) || (!errors.Is(err, wa.ErrNotSent) && !errors.Is(err, wa.ErrNotConnected) && !errors.Is(err, wa.ErrNotPaired)) {
		var maybe *wa.MaybeSentError
		if errors.As(err, &maybe) {
			sent.ID = maybe.MessageID
		}
		return sendOutput{Status: "unknown_maybe_sent", MessageID: sent.ID, ChatJID: jid}
	}
	return sendOutput{Status: "not_sent", ChatJID: jid}
}
func (h *handlers) sendMessage(ctx context.Context, r *mcp.CallToolRequest, in sendMessageInput) (*mcp.CallToolResult, sendOutput, error) {
	key := h.attemptKey(ctx, "text", in.IdempotencyKey, in.Recipient, in.Text)
	return attempt(h, ctx, key, func() (*mcp.CallToolResult, sendOutput, error) { return h.sendMessageOnce(ctx, r, in) })
}
func (h *handlers) sendFile(ctx context.Context, r *mcp.CallToolRequest, in sendFileInput) (*mcp.CallToolResult, sendOutput, error) {
	key := h.attemptKey(ctx, "file", in.IdempotencyKey, in.Recipient, in.Path, in.Caption)
	return attempt(h, ctx, key, func() (*mcp.CallToolResult, sendOutput, error) { return h.sendFileOnce(ctx, r, in) })
}
func (h *handlers) editMessage(ctx context.Context, r *mcp.CallToolRequest, in editMessageInput) (*mcp.CallToolResult, mutationOutput, error) {
	key := h.attemptKey(ctx, "edit", in.IdempotencyKey, in.Chat, in.MessageID, in.Text)
	return attempt(h, ctx, key, func() (*mcp.CallToolResult, mutationOutput, error) { return h.editMessageOnce(ctx, r, in) })
}
func (h *handlers) deleteMessage(ctx context.Context, r *mcp.CallToolRequest, in deleteMessageInput) (*mcp.CallToolResult, mutationOutput, error) {
	key := h.attemptKey(ctx, "delete", in.IdempotencyKey, in.Chat, in.MessageID)
	return attempt(h, ctx, key, func() (*mcp.CallToolResult, mutationOutput, error) { return h.deleteMessageOnce(ctx, r, in) })
}
