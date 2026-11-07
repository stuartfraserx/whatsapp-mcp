// Package session glues the WhatsApp client's events to the message store and
// tracks the pairing state shown to MCP clients.
package session

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/qr"
	"github.com/fmflurry/whatsapp-mcp/internal/ratelimit"
	"github.com/fmflurry/whatsapp-mcp/internal/redact"
	"github.com/fmflurry/whatsapp-mcp/internal/store"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

const (
	defaultQRLifetime = time.Minute
	defaultQRWait     = 30 * time.Second
	storeTimeout      = 10 * time.Second
)

// ErrAlreadyPaired is returned by EnsureQR when the account is already linked.
var ErrAlreadyPaired = errors.New("whatsapp account is already paired")

// ErrQRExpired is returned by WaitPaired when the pairing QR flow ended
// without the account being linked.
var ErrQRExpired = errors.New("pairing qr code expired")

// Options configures a Session. Log and Clock default to a discarding logger
// and the system clock.
type Options struct {
	Client wa.Client
	Store  *store.Store
	QR     qr.Presenter
	Log    *slog.Logger
	Clock  ratelimit.Clock
	Notify interface{ Submit(wa.Message) }
}

// Session owns the client event handler. All methods are safe for concurrent
// use.
type Session struct {
	client wa.Client
	store  *store.Store
	qr     qr.Presenter
	log    *slog.Logger
	clock  ratelimit.Clock
	notify interface{ Submit(wa.Message) }

	mu         sync.Mutex
	code       string
	expiresAt  time.Time
	rendered   bool
	presentErr error
	account    string
	qrWaiters  []chan struct{}
	paired     chan struct{}
	qrExpired  chan struct{}
}

// New builds a Session and registers it as the client's event handler.
func New(o Options) *Session {
	s := &Session{
		client:    o.Client,
		notify:    o.Notify,
		store:     o.Store,
		qr:        o.QR,
		log:       o.Log,
		clock:     o.Clock,
		paired:    make(chan struct{}),
		qrExpired: make(chan struct{}),
	}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if s.clock == nil {
		s.clock = ratelimit.SystemClock{}
	}
	s.client.SetEventHandler(s.handle)
	return s
}

// Status returns the client's current connection status.
func (s *Session) Status() wa.Status { return s.client.Status() }

// QRRendered reports whether the current pairing code was also drawn on the
// terminal.
func (s *Session) QRRendered() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rendered && s.code != ""
}

func (s *Session) handle(ev wa.Event) {
	switch e := ev.(type) {
	case wa.MessageEvent:
		s.saveMessage(e.Message)
	case wa.HistoryEvent:
		s.saveHistory(e)
	case wa.ChatInfoEvent:
		s.upsertChats(e.Chats)
	case wa.ContactNamesEvent:
		s.updateChatNames(e.Chats)
	case wa.MessageEditEvent:
		s.applyEdit(e)
	case wa.QRCodeEvent:
		s.onQRCode(e)
	case wa.PairedEvent:
		s.onPaired(e.AccountJID)
	case wa.ConnectedEvent:
		s.onPaired(s.client.Status().AccountJID)
	case wa.QRTimeoutEvent:
		s.onQRTimeout()
	case wa.LoggedOutEvent:
		s.onLoggedOut(e.Reason)
	case wa.DisconnectedEvent:
		s.log.Debug("whatsapp disconnected")
	}
}

func storeContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), storeTimeout)
}

func (s *Session) logStoreError(op string, err error) {
	s.log.Error("store write failed", "op", op, "error", redact.Scrub(err.Error()))
}

func (s *Session) saveMessage(m wa.Message) {
	ctx, cancel := storeContext()
	defer cancel()
	inserted, err := s.store.SaveMessageResult(ctx, m)
	if err != nil {
		s.logStoreError("save_message", err)
		return
	}
	if inserted && s.notify != nil {
		s.notify.Submit(m)
	}
	s.log.Debug("message stored", "chat", redact.JID(m.ChatJID))
}

func (s *Session) saveHistory(e wa.HistoryEvent) {
	ctx, cancel := storeContext()
	defer cancel()
	if err := s.store.SaveHistory(ctx, e.Chats, e.Messages); err != nil {
		s.logStoreError("save_history", err)
		return
	}
	s.log.Info("history batch stored", "chats", len(e.Chats), "messages", len(e.Messages))
}

func (s *Session) upsertChats(chats []wa.Chat) {
	ctx, cancel := storeContext()
	defer cancel()
	if err := s.store.UpsertChats(ctx, chats); err != nil {
		s.logStoreError("upsert_chats", err)
	}
}

func (s *Session) updateChatNames(chats []wa.Chat) {
	ctx, cancel := storeContext()
	defer cancel()
	if err := s.store.UpdateChatNames(ctx, chats); err != nil {
		s.logStoreError("update_chat_names", err)
	}
}

func (s *Session) applyEdit(e wa.MessageEditEvent) {
	ctx, cancel := storeContext()
	defer cancel()
	if err := s.store.ApplyEdit(ctx, e.ChatJID, e.ID, e.SenderJID, e.Text); err != nil {
		s.logStoreError("apply_edit", err)
	}
}

func (s *Session) onQRTimeout() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgetCodeLocked()
	select {
	case <-s.qrExpired:
	default:
		close(s.qrExpired)
	}
}

func (s *Session) onQRCode(e wa.QRCodeEvent) {
	lifetime := e.Timeout
	if lifetime <= 0 {
		lifetime = defaultQRLifetime
	}
	rendered, err := s.qr.Present(e.Code)
	if err != nil {
		s.log.Error("present pairing qr failed", "error", redact.Scrub(err.Error()))
	}

	s.mu.Lock()
	select {
	case <-s.qrExpired:
		s.qrExpired = make(chan struct{})
	default:
	}
	s.code = e.Code
	s.expiresAt = s.clock.Now().Add(lifetime)
	s.rendered = rendered
	s.presentErr = err
	s.wakeQRWaitersLocked()
	s.mu.Unlock()
}

func (s *Session) onPaired(account string) {
	if err := s.qr.Clear(); err != nil {
		s.log.Warn("clear pairing qr failed", "error", redact.Scrub(err.Error()))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgetCodeLocked()
	if account != "" {
		s.account = account
	}
	select {
	case <-s.paired:
	default:
		close(s.paired)
	}
	s.wakeQRWaitersLocked()
}

func (s *Session) onLoggedOut(reason string) {
	s.log.Warn("whatsapp account logged out", "reason", redact.Scrub(reason))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgetCodeLocked()
	s.account = ""
	select {
	case <-s.paired:
		s.paired = make(chan struct{})
	default:
	}
}

func (s *Session) forgetCodeLocked() {
	s.code = ""
	s.expiresAt = time.Time{}
	s.rendered = false
	s.presentErr = nil
}

func (s *Session) wakeQRWaitersLocked() {
	for _, w := range s.qrWaiters {
		close(w)
	}
	s.qrWaiters = nil
}

// EnsureQR returns the path of the PNG holding a valid pairing code and its
// expiry. When no valid code is held it restarts the client's QR flow and
// waits up to wait (default 30s) for the next code. It returns
// ErrAlreadyPaired when the account is linked.
func (s *Session) EnsureQR(ctx context.Context, wait time.Duration) (pngPath string, expiresAt time.Time, err error) {
	if s.client.Status().Paired {
		return "", time.Time{}, ErrAlreadyPaired
	}
	if wait <= 0 {
		wait = defaultQRWait
	}

	s.mu.Lock()
	if path, exp, ok, perr := s.currentQRLocked(); ok {
		s.mu.Unlock()
		return path, exp, perr
	}
	waiter := make(chan struct{})
	s.qrWaiters = append(s.qrWaiters, waiter)
	s.mu.Unlock()

	if err := s.client.Connect(ctx); err != nil {
		s.dropWaiter(waiter)
		return "", time.Time{}, err
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-waiter:
	case <-ctx.Done():
		s.dropWaiter(waiter)
		return "", time.Time{}, ctx.Err()
	case <-timer.C:
		s.dropWaiter(waiter)
		return "", time.Time{}, errors.New("timed out waiting for a pairing code from whatsapp")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if path, exp, ok, perr := s.currentQRLocked(); ok {
		return path, exp, perr
	}
	if s.client.Status().Paired || s.account != "" {
		return "", time.Time{}, ErrAlreadyPaired
	}
	return "", time.Time{}, errors.New("pairing code is no longer available; try again")
}

func (s *Session) currentQRLocked() (path string, exp time.Time, ok bool, err error) {
	if s.code == "" || !s.clock.Now().Before(s.expiresAt) {
		return "", time.Time{}, false, nil
	}
	if s.presentErr != nil {
		return "", time.Time{}, true, errors.New("could not write the pairing qr image; see server logs")
	}
	return s.qr.PNGPath, s.expiresAt, true, nil
}

func (s *Session) dropWaiter(w chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, c := range s.qrWaiters {
		if c == w {
			s.qrWaiters = append(s.qrWaiters[:i], s.qrWaiters[i+1:]...)
			return
		}
	}
}

// WaitPaired blocks until the account is paired and returns its JID. It
// returns ErrQRExpired when the QR flow ends unpaired, or ctx's error when ctx
// ends first.
func (s *Session) WaitPaired(ctx context.Context) (accountJID string, err error) {
	if st := s.client.Status(); st.Paired && st.AccountJID != "" {
		return st.AccountJID, nil
	}
	s.mu.Lock()
	jid, ch, expired := s.account, s.paired, s.qrExpired
	s.mu.Unlock()
	if jid != "" {
		return jid, nil
	}

	select {
	case <-ch:
	case <-expired:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	s.mu.Lock()
	jid = s.account
	s.mu.Unlock()
	if jid == "" {
		jid = s.client.Status().AccountJID
	}
	if jid != "" {
		return jid, nil
	}
	select {
	case <-expired:
		return "", ErrQRExpired
	default:
		return "", errors.New("paired without an account identifier")
	}
}

// PairingCode returns a snapshot of the active pairing code for authenticated clients.
func (s *Session) PairingCode() (string, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.code == "" || !s.clock.Now().Before(s.expiresAt) || s.client.Status().Paired {
		return "", time.Time{}
	}
	return s.code, s.expiresAt
}
