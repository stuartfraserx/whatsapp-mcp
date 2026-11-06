package notify

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"github.com/fmflurry/whatsapp-mcp/internal/datadir"
	"github.com/fmflurry/whatsapp-mcp/internal/redact"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

// Clock controls debounce, reload and retry deadlines.
type Clock interface {
	Now() time.Time
	After(time.Duration) <-chan time.Time
}
type systemClock struct{}

func (systemClock) Now() time.Time                         { return time.Now() }
func (systemClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Details is resolved locally, outside the session event handler.
type Details struct {
	ChatName, SenderName, AccountJID string
	PN                               map[string]string
}

// Options supplies local state, enrichment and testable timing.
type Options struct {
	ConfigPath, DataDir string
	StartedAt           time.Time
	Log                 *slog.Logger
	Clock               Clock
	Resolve             func(context.Context, wa.Message) Details
	QueueSize           int
}

// Status contains only counters and safe configuration metadata.
type Status struct {
	Configured     bool              `json:"configured"`
	Enabled        bool              `json:"enabled"`
	ConfigError    string            `json:"config_error,omitempty"`
	URLHost        string            `json:"url_host,omitempty"`
	Sent           uint64            `json:"sent"`
	Failed         uint64            `json:"failed"`
	Dropped        uint64            `json:"dropped"`
	Retried        uint64            `json:"retried"`
	Deduped        uint64            `json:"deduped"`
	PendingChats   int               `json:"pending_chats"`
	Ignored        map[string]uint64 `json:"ignored"`
	LastSentAt     string            `json:"last_sent_at,omitempty"`
	LastError      string            `json:"last_error,omitempty"`
	LastStatusCode int               `json:"last_status_code"`
}

// Payload is the small event delivered to the configured endpoint.
type Payload struct {
	Event       string `json:"event"`
	ChatJID     string `json:"chat_jid"`
	ChatName    string `json:"chat_name"`
	IsGroup     bool   `json:"is_group"`
	SenderJID   string `json:"sender_jid"`
	SenderName  string `json:"sender_name"`
	MessageID   string `json:"message_id"`
	TS          string `json:"ts"`
	TextPreview string `json:"text_preview"`
	MediaType   string `json:"media_type"`
	Count       int    `json:"count"`
	AccountJID  string `json:"account_jid"`
}

type delivery struct {
	payload    Payload
	config     Config
	generation uint64
}
type pending struct {
	delivery
	deadline time.Time
}

// Notifier owns both the persistent dedupe store and delivery workers.
type Notifier struct {
	o          Options
	mu         sync.Mutex
	status     Status
	config     Config
	generation uint64
	dropped    atomic.Uint64
	input      chan wa.Message
	output     chan delivery
	db         *sql.DB
	cancel     context.CancelFunc
	done       chan struct{}
	workers    sync.WaitGroup
	http       *http.Client
	stamp      string
}

// New starts workers only when a configuration path is supplied.
func New(ctx context.Context, o Options) (*Notifier, error) {
	if o.Clock == nil {
		o.Clock = systemClock{}
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.StartedAt.IsZero() {
		o.StartedAt = o.Clock.Now()
	}
	n := &Notifier{o: o, done: make(chan struct{}), status: Status{Configured: o.ConfigPath != "", Ignored: map[string]uint64{}}}
	if o.ConfigPath == "" {
		close(n.done)
		return n, nil
	}
	path := filepath.Join(o.DataDir, "notifier.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err := datadir.Harden(path); err != nil {
		return nil, err
	}
	u := &url.URL{Scheme: "file", Path: path}
	n.db, err = sql.Open("sqlite", u.String()+"?_pragma=busy_timeout(1000)")
	if err != nil {
		return nil, err
	}
	n.db.SetMaxOpenConns(1)
	if _, err = n.db.Exec(`CREATE TABLE IF NOT EXISTS seen(chat_jid TEXT NOT NULL, message_id TEXT NOT NULL, seen_at INTEGER NOT NULL, PRIMARY KEY(chat_jid,message_id)); CREATE INDEX IF NOT EXISTS seen_message_id ON seen(message_id); DELETE FROM seen WHERE seen_at < ?`, o.Clock.Now().Add(-30*24*time.Hour).Unix()); err != nil {
		n.db.Close()
		return nil, err
	}
	size := o.QueueSize
	if size <= 0 {
		size = 256
	}
	n.input = make(chan wa.Message, size)
	n.output = make(chan delivery, size)
	n.http = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, IdleConnTimeout: time.Minute, TLSHandshakeTimeout: 10 * time.Second}, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	workCtx, cancel := context.WithCancel(context.Background())
	n.cancel = cancel
	n.reload()
	n.workers.Add(2)
	go n.guard(func() { n.run(workCtx) })
	go n.guard(func() { n.deliver(workCtx) })
	go func() {
		select {
		case <-ctx.Done():
		case <-n.done:
			return
		}
		// Stop accepting and scheduling immediately; permit only the current POST
		// a five-second grace period. Close also works when the parent is still live.
		n.Close()
	}()
	go func() { n.workers.Wait(); n.db.Close(); close(n.done) }()
	return n, nil
}

func (n *Notifier) guard(f func()) {
	defer n.workers.Done()
	defer func() {
		if recover() != nil {
			n.o.Log.Error("inbound webhook panic recovered")
			n.mu.Lock()
			n.status.Enabled = false
			n.status.LastError = redact.Scrub("notifier panic")
			n.mu.Unlock()
			n.cancel()
		}
	}()
	f()
}

// Submit performs only a non-blocking hand-off. It never accesses disk or HTTP.
func (n *Notifier) Submit(m wa.Message) {
	if n.input == nil {
		return
	}
	select {
	case n.input <- m:
	default:
		n.dropped.Add(1)
	}
}

func (n *Notifier) Status() Status {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := n.status
	s.Dropped = n.dropped.Load()
	s.Ignored = make(map[string]uint64, len(n.status.Ignored))
	for k, v := range n.status.Ignored {
		s.Ignored[k] = v
	}
	return s
}

// Close stops timers and workers and waits for the bounded HTTP grace period.
func (n *Notifier) Close() {
	if n.cancel != nil {
		n.cancel()
	}
	<-n.done
}

func (n *Notifier) reload() bool {
	info, err := os.Lstat(n.o.ConfigPath)
	stamp := "missing"
	if err == nil {
		stamp = info.ModTime().String() + "/" + info.Mode().String() + "/" + time.Unix(info.Size(), 0).String()
	}
	if stamp == n.stamp {
		return false
	}
	n.stamp = stamp
	c, err := Load(n.o.ConfigPath)
	n.mu.Lock()
	defer n.mu.Unlock()
	n.generation++
	n.config = c
	n.status.Enabled = err == nil && c.Enabled
	n.status.ConfigError = ""
	n.status.URLHost = ""
	if err != nil {
		n.status.ConfigError = err.Error()
		n.o.Log.Warn("inbound webhook configuration disabled", "error", redact.Scrub(err.Error()))
	}
	if u, e := url.Parse(c.URL); e == nil {
		n.status.URLHost = u.Host
	}
	return true
}

func (n *Notifier) run(ctx context.Context) {
	chats := map[string]*pending{}
	tick := n.o.Clock.After(100 * time.Millisecond)
	reloadAt := n.o.Clock.Now().Add(10 * time.Second)
	for {
		select {
		case <-ctx.Done():
			n.mu.Lock()
			n.status.PendingChats = 0
			n.mu.Unlock()
			return
		case m := <-n.input:
			n.accept(ctx, m, chats)
		case <-tick:
			now := n.o.Clock.Now()
			if !now.Before(reloadAt) {
				if n.reload() {
					clear(chats)
				}
				reloadAt = now.Add(10 * time.Second)
			}
			n.flush(now, chats)
			n.mu.Lock()
			n.status.PendingChats = len(chats)
			n.mu.Unlock()
			tick = n.o.Clock.After(100 * time.Millisecond)
		}
	}
}

func (n *Notifier) ignore(reason string) { n.mu.Lock(); n.status.Ignored[reason]++; n.mu.Unlock() }
func (n *Notifier) accept(ctx context.Context, m wa.Message, chats map[string]*pending) {
	n.mu.Lock()
	c, enabled, gen := n.config, n.status.Enabled, n.generation
	n.mu.Unlock()
	cutoff := n.o.StartedAt.Add(-time.Duration(c.CatchupMinutes) * time.Minute)
	reason := ""
	switch {
	case !enabled:
		reason = "disabled"
	case m.FromMe:
		reason = "from_me"
	case m.ChatJID == "status@broadcast":
		reason = "status_broadcast"
	case m.Timestamp.Before(cutoff):
		reason = "before_start"
	case m.Text == "" && m.Media == nil:
		reason = "not_message"
	}
	if reason != "" {
		n.ignore(reason)
		return
	}
	details := Details{SenderName: m.SenderName}
	if n.o.Resolve != nil {
		details = n.o.Resolve(ctx, m)
	}
	if !c.allowed(m, func(j string) string { return details.PN[j] }) {
		if wa.IsGroup(m.ChatJID) {
			n.ignore("group_not_allowed")
		} else {
			n.ignore("sender_not_allowed")
		}
		return
	}
	if _, ok := chats[m.ChatJID]; !ok && len(chats) >= cap(n.input) {
		n.dropped.Add(1)
		return
	}
	result, err := n.db.ExecContext(ctx, `INSERT OR IGNORE INTO seen(chat_jid,message_id,seen_at) SELECT ?,?,? WHERE NOT EXISTS(SELECT 1 FROM seen WHERE message_id=?)`, m.ChatJID, m.ID, n.o.Clock.Now().Unix(), m.ID)
	if err != nil {
		n.mu.Lock()
		n.status.LastError = redact.Scrub("notifier dedupe database error")
		n.mu.Unlock()
		n.dropped.Add(1)
		return
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		n.mu.Lock()
		n.status.Deduped++
		n.mu.Unlock()
		return
	}
	text := []rune(m.Text)
	if len(text) > c.TextPreviewChars {
		text = text[:c.TextPreviewChars]
	}
	p := Payload{Event: "whatsapp.message", ChatJID: m.ChatJID, ChatName: details.ChatName, IsGroup: wa.IsGroup(m.ChatJID), SenderJID: m.SenderJID, SenderName: details.SenderName, MessageID: m.ID, TS: m.Timestamp.UTC().Format(time.RFC3339), TextPreview: string(text), AccountJID: details.AccountJID, Count: 1}
	if m.Media != nil {
		p.MediaType = string(m.Media.Type)
	}
	if prev := chats[m.ChatJID]; prev != nil {
		p.Count = prev.payload.Count + 1
		prev.payload = p
	} else {
		chats[m.ChatJID] = &pending{delivery: delivery{p, c, gen}, deadline: n.o.Clock.Now().Add(time.Duration(c.DebounceSeconds) * time.Second)}
	}
	n.mu.Lock()
	n.status.PendingChats = len(chats)
	n.mu.Unlock()
}

func (n *Notifier) current(d delivery) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.status.Enabled && n.generation == d.generation
}
func (n *Notifier) deliver(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-n.output:
			if ctx.Err() != nil {
				return
			}
			n.post(ctx, d)
		}
	}
}
func (n *Notifier) post(ctx context.Context, d delivery) {
	data, _ := json.Marshal(d.payload)
	for attempt := 0; attempt < 5; attempt++ {
		if ctx.Err() != nil || !n.current(d) {
			return
		}
		// Parent cancellation stops scheduling immediately but lets this request finish.
		reqCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		stop := context.AfterFunc(ctx, func() { time.AfterFunc(5*time.Second, cancel) })
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, d.config.URL, bytes.NewReader(data))
		if err != nil {
			stop()
			cancel()
			n.failure(0)
			return
		}
		req.Header.Set("Authorization", d.config.AuthorizationHeader)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "whatsapp-mcp-notifier")
		req.Header.Set("Idempotency-Key", d.payload.MessageID)
		resp, err := n.http.Do(req)
		code := 0
		if resp != nil {
			code = resp.StatusCode
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
		}
		stop()
		cancel()
		n.mu.Lock()
		n.status.LastStatusCode = code
		n.mu.Unlock()
		if err == nil && code >= 200 && code < 300 {
			n.mu.Lock()
			n.status.Sent++
			n.status.LastSentAt = n.o.Clock.Now().UTC().Format(time.RFC3339)
			n.status.LastError = ""
			n.mu.Unlock()
			return
		}
		retry := errors.Is(err, syscall.ECONNREFUSED) || (err == nil && (code == 429 || code == 502 || code == 503 || code == 504))
		if !retry || attempt == 4 {
			n.failure(code)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-n.o.Clock.After(time.Duration(2<<attempt) * time.Second):
		}
		n.mu.Lock()
		n.status.Retried++
		n.mu.Unlock()
	}
}
func (n *Notifier) failure(code int) {
	n.o.Log.Warn("inbound webhook delivery failed", "status_code", code)
	n.mu.Lock()
	n.status.Failed++
	n.status.LastStatusCode = code
	n.status.LastError = redact.Scrub("webhook delivery failed")
	n.mu.Unlock()
}

func (n *Notifier) flush(now time.Time, chats map[string]*pending) {
	for jid, p := range chats {
		if !now.Before(p.deadline) {
			select {
			case n.output <- p.delivery:
			default:
				n.dropped.Add(1)
			}
			delete(chats, jid)
		}
	}
}
