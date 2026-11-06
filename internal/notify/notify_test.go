package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/session"
	"github.com/fmflurry/whatsapp-mcp/internal/store"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
	"github.com/fmflurry/whatsapp-mcp/internal/wa/watest"
)

func eventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
func newTestNotifier(t *testing.T, handler http.HandlerFunc) (*Notifier, *watest.ManualClock, string, chan Payload) {
	t.Helper()
	received := make(chan Payload, 10)
	listener := watest.NewListener()
	srv := &httptest.Server{Listener: listener, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test" || r.Header.Get("User-Agent") != "whatsapp-mcp-notifier" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("missing delivery headers")
		}
		var p Payload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Error(err)
		}
		if r.Header.Get("Idempotency-Key") != p.MessageID {
			t.Error("missing idempotency key")
		}
		received <- p
		if handler != nil {
			handler(w, r)
		} else {
			w.WriteHeader(200)
		}
	})}}
	// NewUnstartedServer normally creates a TCP socket, which restricted test
	// environments may prohibit. Construct Server directly with the memory listener.
	srv.Start()
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	path := filepath.Join(dir, "webhook.json")
	writeConfig(t, path, `{"enabled":true,"url":"http://127.0.0.1:8765/wake?secret=hidden","authorization_header":"Authorization: Bearer test","debounce_seconds":1,"text_preview_chars":3}`)
	clock := watest.NewManualClock(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	n, err := New(ctx, Options{ConfigPath: path, DataDir: dir, Clock: clock, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Resolve: func(context.Context, wa.Message) Details {
		return Details{ChatName: "Saved", SenderName: "Sender", AccountJID: "own@s.whatsapp.net"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	n.http.Transport = &http.Transport{DialContext: listener.DialContext}
	t.Cleanup(n.Close)
	eventually(t, func() bool { return clock.Timers() > 0 })
	return n, clock, path, received
}
func message(clock *watest.ManualClock, id string) wa.Message {
	return wa.Message{ID: id, ChatJID: "573133992340@s.whatsapp.net", SenderJID: "573133992340@s.whatsapp.net", Timestamp: clock.Now(), Text: "héllo"}
}
func TestDisabledHasNoWorkersOrDB(t *testing.T) {
	n, err := New(context.Background(), Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	n.Submit(wa.Message{})
	if n.input != nil || n.db != nil || n.Status().Configured {
		t.Fatal("disabled notifier doing work")
	}
}
func TestDebounceLatestAndPersistentDedupe(t *testing.T) {
	n, clock, path, received := newTestNotifier(t, nil)
	n.Submit(message(clock, "first"))
	eventually(t, func() bool { return n.Status().PendingChats == 1 })
	latest := message(clock, "latest")
	latest.Text = "último"
	n.Submit(latest)
	eventually(t, func() bool { var rows int; n.db.QueryRow(`SELECT count(*) FROM seen`).Scan(&rows); return rows == 2 })
	clock.Advance(time.Second)
	eventually(t, func() bool { return n.Status().Sent == 1 })
	p := <-received
	if p.Count != 2 || p.MessageID != "latest" || p.TextPreview != "últ" || p.ChatName != "Saved" || p.AccountJID == "" {
		t.Fatalf("payload %+v", p)
	}
	n.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	again, err := New(ctx, Options{ConfigPath: path, DataDir: filepath.Dir(path), Clock: clock, StartedAt: clock.Now().Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	again.Submit(latest)
	eventually(t, func() bool { return again.Status().Deduped == 1 })
	if again.Status().PendingChats != 0 {
		t.Fatal("duplicate queued")
	}
}
func TestFiltering(t *testing.T) {
	n, clock, _, _ := newTestNotifier(t, nil)
	cases := []struct {
		reason string
		change func(*wa.Message)
	}{
		{"from_me", func(m *wa.Message) { m.FromMe = true }},
		{"status_broadcast", func(m *wa.Message) { m.ChatJID = "status@broadcast" }},
		{"before_start", func(m *wa.Message) { m.Timestamp = clock.Now().Add(-61 * time.Minute) }},
		{"not_message", func(m *wa.Message) { m.Text = "" }},
		{"group_not_allowed", func(m *wa.Message) { m.ChatJID = "1@g.us" }},
	}
	for i, tc := range cases {
		m := message(clock, fmt.Sprint(i))
		tc.change(&m)
		n.Submit(m)
		eventually(t, func() bool { return n.Status().Ignored[tc.reason] == 1 })
	}
	n.mu.Lock()
	n.config.AllowedSenders = []string{"9999999"}
	n.mu.Unlock()
	n.Submit(message(clock, "sender"))
	eventually(t, func() bool { return n.Status().Ignored["sender_not_allowed"] == 1 })
}
func TestCatchupWindow(t *testing.T) {
	n, clock, path, received := newTestNotifier(t, nil)
	m := message(clock, "catchup")
	m.Timestamp = n.o.StartedAt.Add(-30 * time.Minute)
	n.Submit(m)
	eventually(t, func() bool { return n.Status().PendingChats == 1 })
	clock.Advance(time.Second)
	eventually(t, func() bool { return n.Status().Sent == 1 })
	if p := <-received; p.MessageID != m.ID {
		t.Fatal("wrong catch-up message delivered")
	}
	n.Submit(m)
	eventually(t, func() bool { return n.Status().Deduped == 1 })
	if n.Status().PendingChats != 0 || n.Status().Sent != 1 {
		t.Fatal("duplicate catch-up queued or sent")
	}
	old := message(clock, "too-old")
	old.Timestamp = n.o.StartedAt.Add(-61 * time.Minute)
	n.Submit(old)
	eventually(t, func() bool { return n.Status().Ignored["before_start"] == 1 })
	writeConfig(t, path, `{"enabled":true,"url":"http://localhost:8765/wake","catchup_minutes":0,"debounce_seconds":1}`)
	clock.Advance(10 * time.Second)
	eventually(t, func() bool { n.mu.Lock(); defer n.mu.Unlock(); return n.config.CatchupMinutes == 0 })
	strict := message(clock, "strict")
	strict.Timestamp = n.o.StartedAt.Add(-time.Second)
	n.Submit(strict)
	eventually(t, func() bool { return n.Status().Ignored["before_start"] == 2 })
	if n.Status().PendingChats != 0 || n.Status().Sent != 1 {
		t.Fatal("ignored message queued or sent")
	}
}

func TestRetryAndNoRetry(t *testing.T) {
	for _, code := range []int{429, 502, 503, 504, 500, 400} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var calls atomic.Int32
			n, clock, _, received := newTestNotifier(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(code)
				} else {
					w.WriteHeader(200)
				}
			})
			n.Submit(message(clock, "retry"))
			eventually(t, func() bool { return n.Status().PendingChats == 1 })
			clock.Advance(time.Second)
			eventually(t, func() bool { return calls.Load() == 1 })
			<-received
			if code == 400 || code == 500 {
				eventually(t, func() bool { return n.Status().Failed == 1 })
				clock.Advance(time.Minute)
				if n.Status().Retried != 0 {
					t.Fatal("retried 400")
				}
				return
			}
			eventually(t, func() bool { return clock.Timers() >= 2 })
			clock.Advance(time.Second)
			if calls.Load() != 1 {
				t.Fatal("retried too soon")
			}
			clock.Advance(time.Second)
			eventually(t, func() bool { return n.Status().Sent == 1 })
			if n.Status().Retried != 1 {
				t.Fatal("retry count")
			}
		})
	}
}
func TestQueueFullNonBlocking(t *testing.T) {
	n := &Notifier{input: make(chan wa.Message, 1)}
	n.Submit(wa.Message{})
	done := make(chan struct{})
	go func() {
		for range 10000 {
			n.Submit(wa.Message{})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Submit blocked")
	}
	if n.dropped.Load() != 10000 {
		t.Fatalf("drops %d", n.dropped.Load())
	}
}
func TestHotReloadInvalidAndEnabled(t *testing.T) {
	n, clock, path, _ := newTestNotifier(t, nil)
	n.Submit(message(clock, "pending"))
	eventually(t, func() bool { return n.Status().PendingChats == 1 })
	writeConfig(t, path, `{"enabled":true,"url":"http://external.test/?secret=abc"}`)
	clock.Advance(10 * time.Second)
	eventually(t, func() bool { return n.Status().ConfigError != "" })
	if n.Status().Enabled || n.Status().PendingChats != 0 {
		t.Fatal("invalid reload retained enabled/pending")
	}
	writeConfig(t, path, `{"enabled":true,"url":"http://localhost:8765/wake","debounce_seconds":1}`)
	eventually(t, func() bool { return clock.Timers() > 0 })
	clock.Advance(10 * time.Second)
	eventually(t, func() bool { return n.Status().Enabled })
	if n.Status().ConfigError != "" {
		t.Fatal("error retained")
	}
}

func TestSessionHandoffNeverWaitsForHTTP(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	n, clock, _, _ := newTestNotifier(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(200)
	})
	defer close(release)
	fake := watest.NewFake()
	st, err := store.Open(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	session.New(session.Options{Client: fake, Store: st, Notify: n, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	fake.Emit(wa.MessageEvent{Message: message(clock, "one")})
	eventually(t, func() bool { return n.Status().PendingChats == 1 })
	clock.Advance(time.Second)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("HTTP did not start")
	}
	done := make(chan struct{})
	go func() {
		for i := range 20 {
			fake.Emit(wa.MessageEvent{Message: message(clock, fmt.Sprint(i))})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler waited for hung HTTP")
	}
	m := message(clock, "history-only")
	fake.Emit(wa.HistoryEvent{Messages: []wa.Message{m}})
	var seen int
	n.db.QueryRow(`SELECT count(*) FROM seen WHERE message_id='history-only'`).Scan(&seen)
	if seen != 0 {
		t.Fatal("history reached notifier")
	}
}

func TestDeliveryQueueFullDrops(t *testing.T) {
	n := &Notifier{output: make(chan delivery, 1), status: Status{Ignored: map[string]uint64{}}}
	n.output <- delivery{}
	now := time.Now()
	chats := map[string]*pending{"chat": {deadline: now}}
	n.flush(now, chats)
	if n.dropped.Load() != 1 || len(chats) != 0 {
		t.Fatal("delivery queue drop not counted")
	}
}

func TestRetryBackoffExhaustion(t *testing.T) {
	var calls atomic.Int32
	n, clock, _, _ := newTestNotifier(t, func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(503) })
	n.Submit(message(clock, "exhaust"))
	eventually(t, func() bool { return n.Status().PendingChats == 1 })
	clock.Advance(time.Second)
	for i, delay := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second} {
		eventually(t, func() bool { return calls.Load() == int32(i+1) && clock.Timers() >= 2 })
		clock.Advance(delay)
	}
	eventually(t, func() bool { return n.Status().Failed == 1 })
	if calls.Load() != 5 || n.Status().Retried != 4 {
		t.Fatalf("calls=%d status=%+v", calls.Load(), n.Status())
	}
}

func TestResolverPanicRecovered(t *testing.T) {
	n, clock, _, _ := newTestNotifier(t, nil)
	n.o.Resolve = func(context.Context, wa.Message) Details { panic("secret should never be logged") }
	n.Submit(message(clock, "panic"))
	eventually(t, func() bool { return !n.Status().Enabled })
	n.Close()
	if n.Status().LastError != "notifier panic" {
		t.Fatalf("panic status: %+v", n.Status())
	}
}

func TestStartupPrunesOldDedupeRows(t *testing.T) {
	n, clock, path, _ := newTestNotifier(t, nil)
	_, err := n.db.Exec(`INSERT INTO seen VALUES('old','id',?)`, clock.Now().Add(-31*24*time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	_, err = n.db.Exec(`INSERT INTO seen VALUES('recent','id',?)`, clock.Now().Add(-time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	n.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	next, err := New(ctx, Options{DataDir: filepath.Dir(path), ConfigPath: path, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	var count int
	if err := next.db.QueryRow(`SELECT count(*) FROM seen`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("pruned rows=%d err=%v", count, err)
	}
}
func TestMissingConfigCanBeCreatedWithoutRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.json")
	clock := watest.NewManualClock(time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n, err := New(ctx, Options{DataDir: dir, ConfigPath: path, Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	if !n.Status().Configured || n.Status().Enabled || n.Status().ConfigError != "not found" {
		t.Fatalf("status %+v", n.Status())
	}
	eventually(t, func() bool { return clock.Timers() > 0 })
	writeConfig(t, path, `{"enabled":true,"url":"https://example.test/secret?key=secret"}`)
	clock.Advance(10 * time.Second)
	eventually(t, func() bool { return n.Status().Enabled })
	if n.Status().URLHost != "example.test" || n.Status().ConfigError != "" {
		t.Fatalf("reloaded status %+v", n.Status())
	}
}

func TestDedupeByMessageIDAcrossChats(t *testing.T) {
	n, clock, _, received := newTestNotifier(t, nil)
	first := message(clock, "same-id")
	n.Submit(first)
	n.Submit(first)
	first.ChatJID = "other@s.whatsapp.net"
	n.Submit(first)
	eventually(t, func() bool { return n.Status().Deduped == 2 })
	clock.Advance(time.Second)
	eventually(t, func() bool { return n.Status().Sent == 1 })
	<-received
	if len(received) != 0 {
		t.Fatal("duplicate delivered")
	}
}
func TestWrittenPOSTTimeoutDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	n, clock, _, received := newTestNotifier(t, func(w http.ResponseWriter, r *http.Request) { calls.Add(1); <-release; w.WriteHeader(200) })
	defer close(release)
	n.http.Timeout = 15 * time.Millisecond
	n.Submit(message(clock, "timeout-after-write"))
	eventually(t, func() bool { return n.Status().PendingChats == 1 })
	clock.Advance(time.Second)
	<-received
	eventually(t, func() bool { return n.Status().Failed == 1 })
	clock.Advance(time.Minute)
	if n.Status().Retried != 0 || calls.Load() != 1 {
		t.Fatalf("ambiguous POST retried: calls=%d status=%+v", calls.Load(), n.Status())
	}
}
