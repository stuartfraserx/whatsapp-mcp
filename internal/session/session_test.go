package session_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/qr"
	"github.com/fmflurry/whatsapp-mcp/internal/session"
	"github.com/fmflurry/whatsapp-mcp/internal/store"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
	"github.com/fmflurry/whatsapp-mcp/internal/wa/watest"
)

const (
	userJID  = "33612345678@s.whatsapp.net"
	groupJID = "120363000000000001@g.us"
	account  = "33699999999@s.whatsapp.net"
)

var pngMagic = []byte("\x89PNG\r\n\x1a\n")

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock {
	return &fakeClock{now: time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type env struct {
	fake    *watest.Fake
	store   *store.Store
	sess    *session.Session
	clock   *fakeClock
	pngPath string
	logs    *syncBuffer
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newEnvWith(t *testing.T, client func(*watest.Fake) wa.Client, terminal *bytes.Buffer, pngPath string) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "messages.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	fake := watest.NewFake()
	clock := newClock()
	logs := &syncBuffer{}
	if pngPath == "" {
		pngPath = filepath.Join(dir, "pairing-qr.png")
	}
	presenter := qr.Presenter{PNGPath: pngPath}
	if terminal != nil {
		presenter.Terminal = terminal
	}
	var c wa.Client = fake
	if client != nil {
		c = client(fake)
	}
	sess := session.New(session.Options{
		Client: c,
		Store:  st,
		QR:     presenter,
		Log:    slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Clock:  clock,
	})
	return &env{fake: fake, store: st, sess: sess, clock: clock, pngPath: pngPath, logs: logs}
}

func newEnv(t *testing.T) *env {
	t.Helper()
	return newEnvWith(t, nil, nil, "")
}

func ctx5(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func assertNoFile(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s to be absent, stat err = %v", path, err)
	}
}

func TestNewRegistersEventHandlerOnClient(t *testing.T) {
	e := newEnv(t)
	e.fake.Emit(wa.MessageEvent{Message: wa.Message{ID: "A1", ChatJID: userJID, Timestamp: e.clock.Now(), Text: "hi"}})

	if _, err := e.store.GetMessage(ctx5(t), userJID, "A1"); err != nil {
		t.Fatalf("message emitted through the client was not stored: %v", err)
	}
}

func TestStatusDelegatesToClient(t *testing.T) {
	e := newEnv(t)
	want := wa.Status{State: wa.StateConnected, AccountJID: account, Paired: true}
	e.fake.SetStatus(want)

	if got := e.sess.Status(); got != want {
		t.Fatalf("Status() = %+v, want %+v", got, want)
	}
}

func TestMessageEventIsPersisted(t *testing.T) {
	ts := time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
	tests := []struct {
		name string
		msg  wa.Message
	}{
		{"incoming text", wa.Message{ID: "M1", ChatJID: userJID, SenderJID: userJID, SenderName: "Alice", Timestamp: ts, Text: "hello"}},
		{"outgoing text", wa.Message{ID: "M2", ChatJID: userJID, SenderJID: account, Timestamp: ts, FromMe: true, Text: "yo"}},
		{"group message", wa.Message{ID: "M3", ChatJID: groupJID, SenderJID: userJID, Timestamp: ts, Text: "team"}},
		{"media message", wa.Message{ID: "M4", ChatJID: userJID, SenderJID: userJID, Timestamp: ts, Text: "caption", Media: &wa.Media{
			Type: wa.MediaImage, MimeType: "image/jpeg", FileName: "a.jpg", DirectPath: "/v/t1/x", FileLength: 42,
			MediaKey: []byte{1, 2}, FileSHA256: []byte{3}, FileEncSHA256: []byte{4},
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.fake.Emit(wa.MessageEvent{Message: tt.msg})

			got, err := e.store.GetMessage(ctx5(t), tt.msg.ChatJID, tt.msg.ID)
			if err != nil {
				t.Fatalf("GetMessage: %v", err)
			}
			if got.Text != tt.msg.Text || got.FromMe != tt.msg.FromMe || got.SenderJID != tt.msg.SenderJID ||
				got.SenderName != tt.msg.SenderName || !got.Timestamp.Equal(tt.msg.Timestamp) {
				t.Fatalf("stored message = %+v, want %+v", got, tt.msg)
			}
			if (got.Media == nil) != (tt.msg.Media == nil) {
				t.Fatalf("media presence = %v, want %v", got.Media != nil, tt.msg.Media != nil)
			}
			if tt.msg.Media != nil {
				if got.Media.DirectPath != tt.msg.Media.DirectPath || got.Media.FileLength != 42 ||
					got.Media.Type != wa.MediaImage || !bytes.Equal(got.Media.MediaKey, []byte{1, 2}) {
					t.Fatalf("stored media = %+v, want %+v", got.Media, tt.msg.Media)
				}
			}
			chat, err := e.store.GetChat(ctx5(t), tt.msg.ChatJID)
			if err != nil {
				t.Fatalf("chat row missing for stored message: %v", err)
			}
			if wa.IsGroup(tt.msg.ChatJID) != chat.IsGroup {
				t.Fatalf("chat IsGroup = %v for %s", chat.IsGroup, tt.msg.ChatJID)
			}
		})
	}
}

func TestMessageEventDuplicateIDUpdatesInPlace(t *testing.T) {
	e := newEnv(t)
	ts := e.clock.Now()
	e.fake.Emit(wa.MessageEvent{Message: wa.Message{ID: "E1", ChatJID: userJID, Timestamp: ts, Text: "original"}})
	e.fake.Emit(wa.MessageEvent{Message: wa.Message{ID: "E1", ChatJID: userJID, Timestamp: ts, Text: "edited"}})

	msgs, _, err := e.store.ListMessages(ctx5(t), store.MessageQuery{ChatJID: userJID})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Text != "edited" {
		t.Fatalf("messages = %+v, want a single edited message", msgs)
	}
}

func TestHistoryEventPersistsChatsAndMessages(t *testing.T) {
	e := newEnv(t)
	base := time.Date(2025, 1, 10, 8, 0, 0, 0, time.UTC)
	e.fake.Emit(wa.HistoryEvent{
		Chats: []wa.Chat{
			{JID: userJID, Name: "Alice"},
			{JID: groupJID, Name: "Team", IsGroup: true},
		},
		Messages: []wa.Message{
			{ID: "H1", ChatJID: userJID, Timestamp: base, Text: "one"},
			{ID: "H2", ChatJID: userJID, Timestamp: base.Add(time.Minute), Text: "two", FromMe: true},
			{ID: "H3", ChatJID: groupJID, Timestamp: base.Add(2 * time.Minute), Text: "three"},
		},
	})

	chats, _, err := e.store.ListChats(ctx5(t), store.ChatQuery{SortBy: "name"})
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	names := map[string]store.ChatSummary{}
	for _, c := range chats {
		names[c.JID] = c
	}
	if len(chats) != 2 || names[userJID].Name != "Alice" || !names[groupJID].IsGroup || names[groupJID].Name != "Team" {
		t.Fatalf("chats = %+v", chats)
	}
	if names[userJID].LastMessageText != "two" || !names[userJID].LastMessageFromMe {
		t.Fatalf("last message of DM chat = %+v", names[userJID])
	}
	msgs, _, err := e.store.ListMessages(ctx5(t), store.MessageQuery{Oldest: true})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 3 || msgs[0].ID != "H1" || msgs[2].ID != "H3" {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestChatInfoEventUpdatesNamesButNeverBlanksThem(t *testing.T) {
	e := newEnv(t)
	e.fake.Emit(wa.ChatInfoEvent{Chats: []wa.Chat{{JID: groupJID, Name: "Old name", IsGroup: true}}})
	e.fake.Emit(wa.ChatInfoEvent{Chats: []wa.Chat{{JID: groupJID, Name: "New name", IsGroup: true}}})
	e.fake.Emit(wa.ChatInfoEvent{Chats: []wa.Chat{{JID: groupJID, Name: "", IsGroup: true}}})

	c, err := e.store.GetChat(ctx5(t), groupJID)
	if err != nil {
		t.Fatalf("GetChat: %v", err)
	}
	if c.Name != "New name" || !c.IsGroup {
		t.Fatalf("chat = %+v, want name %q", c, "New name")
	}
}

func TestStoreFailuresAreLoggedWithoutPanicOrLeakingContent(t *testing.T) {
	e := newEnv(t)
	if err := e.store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	secret := "TOP-SECRET-BODY-TEXT"

	e.fake.Emit(wa.MessageEvent{Message: wa.Message{ID: "X", ChatJID: userJID, Timestamp: e.clock.Now(), Text: secret}})
	e.fake.Emit(wa.HistoryEvent{Chats: []wa.Chat{{JID: userJID}}, Messages: []wa.Message{{ID: "Y", ChatJID: userJID, Text: secret}}})
	e.fake.Emit(wa.ChatInfoEvent{Chats: []wa.Chat{{JID: groupJID, Name: secret}}})

	logs := e.logs.String()
	for _, op := range []string{"save_message", "save_history", "upsert_chats"} {
		if !strings.Contains(logs, "op="+op) {
			t.Errorf("expected a logged store failure for %s; logs:\n%s", op, logs)
		}
	}
	if strings.Contains(logs, secret) {
		t.Fatalf("log output leaks message content:\n%s", logs)
	}
}

func TestQRCodeEventWritesPNGWithOwnerOnlyPermissions(t *testing.T) {
	e := newEnv(t)
	e.fake.Emit(wa.QRCodeEvent{Code: "2@pairing-payload", Timeout: 20 * time.Second})

	data := readFile(t, e.pngPath)
	if !bytes.HasPrefix(data, pngMagic) {
		t.Fatalf("qr file is not a PNG: % x", data[:min(8, len(data))])
	}
	info, err := os.Stat(e.pngPath)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("qr png mode = %o, want 600", info.Mode().Perm())
	}
}

func TestEnsureQRReturnsCurrentCodeWithoutReconnecting(t *testing.T) {
	e := newEnv(t)
	e.fake.Emit(wa.QRCodeEvent{Code: "code-1", Timeout: 20 * time.Second})
	wantExpiry := e.clock.Now().Add(20 * time.Second)

	path, exp, err := e.sess.EnsureQR(ctx5(t), time.Second)
	if err != nil {
		t.Fatalf("EnsureQR: %v", err)
	}
	if path != e.pngPath {
		t.Fatalf("path = %q, want %q", path, e.pngPath)
	}
	if !exp.Equal(wantExpiry) {
		t.Fatalf("expiry = %v, want %v", exp, wantExpiry)
	}
	if n := e.fake.ConnectCalls(); n != 0 {
		t.Fatalf("Connect called %d times for a still-valid code", n)
	}
}

func TestQRCodeEventWithoutTimeoutDefaultsToOneMinute(t *testing.T) {
	e := newEnv(t)
	e.fake.Emit(wa.QRCodeEvent{Code: "code-1"})

	_, exp, err := e.sess.EnsureQR(ctx5(t), time.Second)
	if err != nil {
		t.Fatalf("EnsureQR: %v", err)
	}
	if want := e.clock.Now().Add(time.Minute); !exp.Equal(want) {
		t.Fatalf("expiry = %v, want %v", exp, want)
	}
}

func TestEnsureQRCodeExpiryBoundary(t *testing.T) {
	tests := []struct {
		name          string
		advance       time.Duration
		wantReconnect bool
	}{
		{"one nanosecond before expiry is still valid", 20*time.Second - time.Nanosecond, false},
		{"exactly at expiry is expired", 20 * time.Second, true},
		{"after expiry is expired", 25 * time.Second, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			e.fake.SetQRCodesOnConnect("fresh")
			e.fake.Emit(wa.QRCodeEvent{Code: "stale", Timeout: 20 * time.Second})
			e.clock.Advance(tt.advance)

			if _, _, err := e.sess.EnsureQR(ctx5(t), time.Second); err != nil {
				t.Fatalf("EnsureQR: %v", err)
			}
			if got := e.fake.ConnectCalls() > 0; got != tt.wantReconnect {
				t.Fatalf("reconnected = %v, want %v", got, tt.wantReconnect)
			}
		})
	}
}

func TestEnsureQRExpiredCodeReconnectsAndReturnsFreshCode(t *testing.T) {
	e := newEnv(t)
	e.fake.SetQRCodesOnConnect("fresh-code")
	e.fake.Emit(wa.QRCodeEvent{Code: "stale-code", Timeout: 20 * time.Second})
	stalePNG := readFile(t, e.pngPath)
	e.clock.Advance(21 * time.Second)

	path, exp, err := e.sess.EnsureQR(ctx5(t), time.Second)
	if err != nil {
		t.Fatalf("EnsureQR: %v", err)
	}
	if n := e.fake.ConnectCalls(); n != 1 {
		t.Fatalf("Connect calls = %d, want 1", n)
	}
	if want := e.clock.Now().Add(time.Minute); !exp.Equal(want) {
		t.Fatalf("expiry = %v, want %v (fake emits a 1 minute code)", exp, want)
	}
	if bytes.Equal(readFile(t, path), stalePNG) {
		t.Fatal("PNG still encodes the stale code")
	}
}

func TestEnsureQRWithoutAnyCodeStartsFlow(t *testing.T) {
	e := newEnv(t)
	e.fake.SetQRCodesOnConnect("first")

	path, _, err := e.sess.EnsureQR(ctx5(t), time.Second)
	if err != nil {
		t.Fatalf("EnsureQR: %v", err)
	}
	if e.fake.ConnectCalls() != 1 {
		t.Fatalf("Connect calls = %d, want 1", e.fake.ConnectCalls())
	}
	if !bytes.HasPrefix(readFile(t, path), pngMagic) {
		t.Fatal("returned path is not a PNG")
	}
}

func TestEnsureQRKeepsLatestOfSeveralCodes(t *testing.T) {
	e := newEnv(t)
	e.fake.SetQRCodesOnConnect("a", "b", "c")

	if _, _, err := e.sess.EnsureQR(ctx5(t), time.Second); err != nil {
		t.Fatalf("EnsureQR: %v", err)
	}
	e.fake.Emit(wa.QRCodeEvent{Code: "d", Timeout: time.Minute})
	if _, _, err := e.sess.EnsureQR(ctx5(t), time.Second); err != nil {
		t.Fatalf("second EnsureQR: %v", err)
	}
	if n := e.fake.ConnectCalls(); n != 1 {
		t.Fatalf("Connect calls = %d, want 1 (second call reuses current code)", n)
	}
}

func TestEnsureQRWhenPairedReturnsErrAlreadyPaired(t *testing.T) {
	e := newEnv(t)
	e.fake.SetStatus(wa.Status{State: wa.StateConnected, AccountJID: account, Paired: true})

	path, exp, err := e.sess.EnsureQR(ctx5(t), time.Second)
	if !errors.Is(err, session.ErrAlreadyPaired) {
		t.Fatalf("err = %v, want ErrAlreadyPaired", err)
	}
	if path != "" || !exp.IsZero() {
		t.Fatalf("got path=%q exp=%v, want zero values", path, exp)
	}
	if n := e.fake.ConnectCalls(); n != 0 {
		t.Fatalf("Connect called %d times for a paired client", n)
	}
}

type connectHook struct {
	*watest.Fake
	onConnect func()
}

func (c connectHook) Connect(ctx context.Context) error {
	if c.onConnect != nil {
		c.onConnect()
	}
	return c.Fake.Connect(ctx)
}

type connectErrClient struct {
	*watest.Fake
	err error
}

func (c connectErrClient) Connect(context.Context) error { return c.err }

func TestEnsureQRPairingDuringWaitReturnsErrAlreadyPaired(t *testing.T) {
	var e *env
	hook := connectHook{}
	e = newEnvWith(t, func(f *watest.Fake) wa.Client {
		hook.Fake = f
		hook.onConnect = func() { f.Emit(wa.PairedEvent{AccountJID: account}) }
		return hook
	}, nil, "")

	_, _, err := e.sess.EnsureQR(ctx5(t), time.Second)
	if !errors.Is(err, session.ErrAlreadyPaired) {
		t.Fatalf("err = %v, want ErrAlreadyPaired", err)
	}
}

func TestEnsureQRPropagatesConnectError(t *testing.T) {
	boom := errors.New("dial failed")
	e := newEnvWith(t, func(f *watest.Fake) wa.Client { return connectErrClient{Fake: f, err: boom} }, nil, "")

	_, _, err := e.sess.EnsureQR(ctx5(t), time.Second)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

func TestEnsureQRContextEnded(t *testing.T) {
	t.Run("cancelled context", func(t *testing.T) {
		e := newEnv(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, _, err := e.sess.EnsureQR(ctx, time.Minute)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
	t.Run("deadline exceeded", func(t *testing.T) {
		e := newEnv(t)
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()

		_, _, err := e.sess.EnsureQR(ctx, time.Minute)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	})
}

func TestEnsureQRWaitTimeoutWhenNoCodeArrives(t *testing.T) {
	e := newEnv(t)

	_, _, err := e.sess.EnsureQR(ctx5(t), 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v, want a timeout error", err)
	}
	if errors.Is(err, session.ErrAlreadyPaired) {
		t.Fatal("timeout must not be reported as already paired")
	}
}

func TestEnsureQRAfterTimedOutWaitDoesNotLeakWaiters(t *testing.T) {
	e := newEnv(t)
	if _, _, err := e.sess.EnsureQR(ctx5(t), 5*time.Millisecond); err == nil {
		t.Fatal("expected timeout")
	}
	e.fake.SetQRCodesOnConnect("late")

	if _, _, err := e.sess.EnsureQR(ctx5(t), time.Second); err != nil {
		t.Fatalf("EnsureQR after a timed out wait: %v", err)
	}
}

func TestQRTimeoutEventForgetsCode(t *testing.T) {
	e := newEnv(t)
	e.fake.Emit(wa.QRCodeEvent{Code: "c", Timeout: time.Minute})
	e.fake.Emit(wa.QRTimeoutEvent{})
	e.fake.SetQRCodesOnConnect("again")

	if _, _, err := e.sess.EnsureQR(ctx5(t), time.Second); err != nil {
		t.Fatalf("EnsureQR: %v", err)
	}
	if n := e.fake.ConnectCalls(); n != 1 {
		t.Fatalf("Connect calls = %d, want 1: a timed-out code must not be served", n)
	}
	if e.sess.QRRendered() {
		t.Fatal("QRRendered must be false without a terminal")
	}
}

func TestQRPresentationFailureIsReportedWithoutLeakingCode(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir", "qr.png")
	e := newEnvWith(t, nil, nil, missing)
	e.fake.SetQRCodesOnConnect("SUPER-SECRET-PAIRING-PAYLOAD")

	_, _, err := e.sess.EnsureQR(ctx5(t), time.Second)
	if err == nil {
		t.Fatal("expected an error when the PNG cannot be written")
	}
	if strings.Contains(err.Error(), "SUPER-SECRET-PAIRING-PAYLOAD") || strings.Contains(e.logs.String(), "SUPER-SECRET-PAIRING-PAYLOAD") {
		t.Fatalf("pairing payload leaked: err=%v logs=%s", err, e.logs.String())
	}
}

func TestQRRenderedReflectsTerminalPresenter(t *testing.T) {
	var term bytes.Buffer
	e := newEnvWith(t, nil, &term, "")

	if e.sess.QRRendered() {
		t.Fatal("QRRendered true before any code")
	}
	e.fake.Emit(wa.QRCodeEvent{Code: "rendered-code", Timeout: time.Minute})
	if !e.sess.QRRendered() {
		t.Fatal("QRRendered false although the terminal received the code")
	}
	if term.Len() == 0 {
		t.Fatal("nothing was drawn on the terminal writer")
	}
	e.fake.Emit(wa.PairedEvent{AccountJID: account})
	if e.sess.QRRendered() {
		t.Fatal("QRRendered must reset once paired")
	}
}

func TestPairedEventClearsPNGAndUnblocksWaitPaired(t *testing.T) {
	e := newEnv(t)
	e.fake.Emit(wa.QRCodeEvent{Code: "c", Timeout: time.Minute})
	readFile(t, e.pngPath)

	type result struct {
		jid string
		err error
	}
	done := make(chan result, 1)
	go func() {
		jid, err := e.sess.WaitPaired(ctx5(t))
		done <- result{jid, err}
	}()

	e.fake.Emit(wa.PairedEvent{AccountJID: account})

	r := <-done
	if r.err != nil || r.jid != account {
		t.Fatalf("WaitPaired = (%q, %v), want (%q, nil)", r.jid, r.err, account)
	}
	assertNoFile(t, e.pngPath)
}

func TestConnectedEventClearsPNGAndUnblocksWaitPaired(t *testing.T) {
	e := newEnv(t)
	e.fake.Emit(wa.QRCodeEvent{Code: "c", Timeout: time.Minute})

	e.fake.SetStatus(wa.Status{State: wa.StateConnected, AccountJID: account})
	e.fake.Emit(wa.ConnectedEvent{})

	assertNoFile(t, e.pngPath)
	jid, err := e.sess.WaitPaired(ctx5(t))
	if err != nil || jid != account {
		t.Fatalf("WaitPaired = (%q, %v), want (%q, nil)", jid, err, account)
	}
}

func TestPairedEventDiscardsCodeSoNextEnsureQRRestartsFlow(t *testing.T) {
	e := newEnv(t)
	e.fake.Emit(wa.QRCodeEvent{Code: "c", Timeout: time.Minute})
	e.fake.Emit(wa.PairedEvent{AccountJID: account})

	_, _, err := e.sess.EnsureQR(ctx5(t), 5*time.Millisecond)
	if err == nil {
		t.Fatal("a code from before pairing must not be served")
	}
	if e.fake.ConnectCalls() != 1 {
		t.Fatalf("Connect calls = %d, want 1", e.fake.ConnectCalls())
	}
}

func TestWaitPairedImmediateWhenClientAlreadyPaired(t *testing.T) {
	e := newEnv(t)
	e.fake.SetStatus(wa.Status{State: wa.StateConnected, AccountJID: account, Paired: true})

	jid, err := e.sess.WaitPaired(ctx5(t))
	if err != nil || jid != account {
		t.Fatalf("WaitPaired = (%q, %v), want (%q, nil)", jid, err, account)
	}
}

func TestWaitPairedImmediateAfterEarlierPairedEvent(t *testing.T) {
	e := newEnv(t)
	e.fake.Emit(wa.PairedEvent{AccountJID: account})

	jid, err := e.sess.WaitPaired(ctx5(t))
	if err != nil || jid != account {
		t.Fatalf("WaitPaired = (%q, %v), want (%q, nil)", jid, err, account)
	}
}

func TestWaitPairedContextEnded(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		e := newEnv(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if _, err := e.sess.WaitPaired(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		e := newEnv(t)
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()

		if _, err := e.sess.WaitPaired(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want context.DeadlineExceeded", err)
		}
	})
}

func TestPairedWithoutAccountIdentifierIsAnError(t *testing.T) {
	e := newEnv(t)
	e.fake.Emit(wa.PairedEvent{})

	if _, err := e.sess.WaitPaired(ctx5(t)); err == nil {
		t.Fatal("expected an error when pairing yields no account JID")
	}
}

func TestLoggedOutEventResetsPairingState(t *testing.T) {
	e := newEnv(t)
	e.fake.Emit(wa.PairedEvent{AccountJID: account})
	e.fake.Emit(wa.QRCodeEvent{Code: "c", Timeout: time.Minute})
	e.fake.Emit(wa.LoggedOutEvent{Reason: "removed from phone 33612345678"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if jid, err := e.sess.WaitPaired(ctx); err == nil {
		t.Fatalf("WaitPaired returned %q after logout; it must block until re-pairing", jid)
	}

	e.fake.SetQRCodesOnConnect("new")
	if _, _, err := e.sess.EnsureQR(ctx5(t), time.Second); err != nil {
		t.Fatalf("EnsureQR after logout: %v", err)
	}
	if e.fake.ConnectCalls() != 1 {
		t.Fatalf("Connect calls = %d, want 1 (old code discarded on logout)", e.fake.ConnectCalls())
	}
	logs := e.logs.String()
	if !strings.Contains(logs, "logged out") {
		t.Fatalf("expected a warning about the logout; logs:\n%s", logs)
	}
	if strings.Contains(logs, "33612345678") {
		t.Fatalf("logout reason leaked a phone number:\n%s", logs)
	}
}

func TestRepairingAfterLogoutUnblocksWaitPairedAgain(t *testing.T) {
	e := newEnv(t)
	e.fake.Emit(wa.PairedEvent{AccountJID: account})
	e.fake.Emit(wa.LoggedOutEvent{})

	const second = "33688888888@s.whatsapp.net"
	done := make(chan string, 1)
	go func() {
		jid, _ := e.sess.WaitPaired(ctx5(t))
		done <- jid
	}()
	e.fake.Emit(wa.PairedEvent{AccountJID: second})

	if jid := <-done; jid != second {
		t.Fatalf("WaitPaired = %q, want %q", jid, second)
	}
}

func TestDisconnectedEventIsHarmless(t *testing.T) {
	e := newEnv(t)
	e.fake.Emit(wa.QRCodeEvent{Code: "c", Timeout: time.Minute})
	e.fake.Emit(wa.DisconnectedEvent{})

	if _, _, err := e.sess.EnsureQR(ctx5(t), time.Second); err != nil {
		t.Fatalf("disconnect must not invalidate the current code: %v", err)
	}
	if e.fake.ConnectCalls() != 0 {
		t.Fatalf("unexpected Connect calls: %d", e.fake.ConnectCalls())
	}
}

func TestConcurrentEventsAndCalls(t *testing.T) {
	e := newEnv(t)
	e.fake.SetQRCodesOnConnect("q")
	const writers, perWriter = 8, 25
	base := e.clock.Now()

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range perWriter {
				e.fake.Emit(wa.MessageEvent{Message: wa.Message{
					ID: fmt.Sprintf("W%d-%d", w, i), ChatJID: userJID, Timestamp: base.Add(time.Duration(w*perWriter+i) * time.Second), Text: "x",
				}})
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				e.fake.Emit(wa.QRCodeEvent{Code: "c", Timeout: time.Minute})
				_, _, _ = e.sess.EnsureQR(ctx5(t), time.Second)
				_ = e.sess.Status()
				_ = e.sess.QRRendered()
				e.clock.Advance(time.Second)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 20 {
			e.fake.Emit(wa.ChatInfoEvent{Chats: []wa.Chat{{JID: groupJID, Name: fmt.Sprintf("g%d", i), IsGroup: true}}})
			e.fake.Emit(wa.QRTimeoutEvent{})
		}
	}()
	wg.Wait()

	e.fake.Emit(wa.PairedEvent{AccountJID: account})
	msgs, _, err := e.store.ListMessages(ctx5(t), store.MessageQuery{ChatJID: userJID, Limit: 200})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != writers*perWriter {
		t.Fatalf("stored %d messages, want %d", len(msgs), writers*perWriter)
	}
	if _, err := e.sess.WaitPaired(ctx5(t)); err != nil {
		t.Fatalf("WaitPaired: %v", err)
	}
}

func TestMessageEditEventUpdatesTextOfTheSendersMessage(t *testing.T) {
	e := newEnv(t)
	ts := e.clock.Now()
	e.fake.Emit(wa.MessageEvent{Message: wa.Message{ID: "E1", ChatJID: groupJID, SenderJID: userJID, Timestamp: ts, Text: "typo"}})

	e.fake.Emit(wa.MessageEditEvent{ChatJID: groupJID, ID: "E1", SenderJID: account, Text: "hijacked"})
	e.fake.Emit(wa.MessageEditEvent{ChatJID: groupJID, ID: "E1", SenderJID: userJID, Text: "fixed"})

	msgs, _, err := e.store.ListMessages(ctx5(t), store.MessageQuery{})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Text != "fixed" || msgs[0].SenderJID != userJID || !msgs[0].Timestamp.Equal(ts) {
		t.Fatalf("messages = %+v, want one message edited to %q by its sender only", msgs, "fixed")
	}
}

func TestContactNamesEventNamesExistingChatsOnly(t *testing.T) {
	e := newEnv(t)
	e.fake.Emit(wa.MessageEvent{Message: wa.Message{ID: "M1", ChatJID: userJID, SenderJID: userJID, Timestamp: e.clock.Now(), Text: "hi"}})

	e.fake.Emit(wa.ContactNamesEvent{Chats: []wa.Chat{
		{JID: userJID, Name: "Alice"},
		{JID: "33600000000@s.whatsapp.net", Name: "Nobody"},
	}})

	chats, _, err := e.store.ListChats(ctx5(t), store.ChatQuery{})
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(chats) != 1 || chats[0].JID != userJID || chats[0].Name != "Alice" {
		t.Fatalf("chats = %+v, want only the existing chat, named Alice", chats)
	}
}

func TestWaitPairedReturnsErrQRExpiredOnQRTimeout(t *testing.T) {
	t.Run("timeout while waiting", func(t *testing.T) {
		e := newEnv(t)
		errCh := make(chan error, 1)
		go func() {
			_, err := e.sess.WaitPaired(ctx5(t))
			errCh <- err
		}()
		e.fake.Emit(wa.QRCodeEvent{Code: "c", Timeout: time.Minute})
		e.fake.Emit(wa.QRTimeoutEvent{})

		if err := <-errCh; !errors.Is(err, session.ErrQRExpired) {
			t.Fatalf("WaitPaired error = %v, want ErrQRExpired", err)
		}
	})
	t.Run("timeout before waiting", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Emit(wa.QRCodeEvent{Code: "c", Timeout: time.Minute})
		e.fake.Emit(wa.QRTimeoutEvent{})

		if _, err := e.sess.WaitPaired(ctx5(t)); !errors.Is(err, session.ErrQRExpired) {
			t.Fatalf("WaitPaired error = %v, want ErrQRExpired", err)
		}
	})
	t.Run("a new code starts a fresh wait", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Emit(wa.QRTimeoutEvent{})
		e.fake.Emit(wa.QRCodeEvent{Code: "c2", Timeout: time.Minute})
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		if _, err := e.sess.WaitPaired(ctx); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("WaitPaired error = %v, want to keep waiting until the deadline", err)
		}
	})
	t.Run("pairing wins over an earlier timeout", func(t *testing.T) {
		e := newEnv(t)
		e.fake.Emit(wa.QRTimeoutEvent{})
		e.fake.Emit(wa.PairedEvent{AccountJID: account})

		jid, err := e.sess.WaitPaired(ctx5(t))
		if err != nil || jid != account {
			t.Fatalf("WaitPaired = (%q, %v), want (%q, nil)", jid, err, account)
		}
	})
}

type notifyCounter struct{ calls int }

func (n *notifyCounter) Submit(wa.Message) { n.calls++ }
func TestRedeliveryOnlyNotifiesOnInsert(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fake := watest.NewFake()
	counter := &notifyCounter{}
	_ = session.New(session.Options{Client: fake, Store: st, Notify: counter})
	msg := wa.Message{ID: "redelivered", ChatJID: userJID, SenderJID: userJID, Text: "hello", Timestamp: time.Now()}
	fake.Emit(wa.MessageEvent{Message: msg})
	fake.Emit(wa.MessageEvent{Message: msg})
	if counter.calls != 1 {
		t.Fatalf("notifications=%d", counter.calls)
	}
	// History insertion followed by socket redelivery must also remain silent.
	msg.ID = "history"
	fake.Emit(wa.HistoryEvent{Messages: []wa.Message{msg}})
	fake.Emit(wa.MessageEvent{Message: msg})
	if counter.calls != 1 {
		t.Fatalf("history redelivery notified: %d", counter.calls)
	}
}

func TestStatusViewsPersistWithoutDuplicates(t *testing.T) {
	e := newEnv(t)
	event := wa.StatusViewEvent{Views: []wa.StatusView{{MessageID: "status", ViewerJID: userJID, ReceiptType: "read", Timestamp: e.clock.Now()}}}
	e.fake.Emit(event)
	e.fake.Emit(event)
	got, err := e.store.ListStatusViews(context.Background(), store.StatusViewsQuery{MessageID: "status"})
	if err != nil || len(got) != 1 || len(got[0].Viewers) != 1 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}
