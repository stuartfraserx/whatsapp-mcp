// Package watest provides an in-memory wa.Client for tests.
package watest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

// SentRecord is an outgoing message captured by the Fake.
type SentRecord struct {
	MessageID string
	Operation string
	ChatJID   string
	Text      string
	File      *wa.OutgoingFile
	At        time.Time
}

// Fake is a mutex-protected wa.Client that records sends instead of delivering
// them.
type Fake struct {
	mu           sync.Mutex
	handler      func(wa.Event)
	status       wa.Status
	contacts     []wa.Contact
	media        map[string][]byte
	sendErr      error
	qrCodes      []string
	sent         []SentRecord
	connectCalls int
	logoutCalls  int
	seq          int
	now          func() time.Time

	resyncCalls   int
	resyncOutcome wa.ResyncOutcome
	resyncErr     error
	contactSync   wa.ContactSyncStatus
}

var _ wa.Client = (*Fake)(nil)

// NewFake returns a disconnected, unpaired Fake.
func NewFake() *Fake {
	return &Fake{
		status: wa.Status{State: wa.StateDisconnected},
		media:  map[string][]byte{},
		now:    time.Now,
	}
}

// SetStatus replaces the reported status.
func (f *Fake) SetStatus(st wa.Status) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = st
}

// SetContacts replaces the contacts returned by Contacts.
func (f *Fake) SetContacts(c []wa.Contact) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contacts = slices.Clone(c)
}

// SetMedia registers data to be returned for a Media with this DirectPath.
func (f *Fake) SetMedia(directPath string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.media[directPath] = slices.Clone(data)
}

// SetSendError makes SendText and SendFile fail with err (nil clears it). Wrap
// err with wa.ErrNotSent to model a failure before anything reached the network.
func (f *Fake) SetSendError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sendErr = err
}

// SetQRCodesOnConnect makes Connect on an unpaired client emit these codes.
func (f *Fake) SetQRCodesOnConnect(codes ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.qrCodes = slices.Clone(codes)
}

// SetClock replaces the time source used to stamp sends.
func (f *Fake) SetClock(now func() time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = now
}

// Emit delivers ev to the registered handler synchronously.
func (f *Fake) Emit(ev wa.Event) {
	f.mu.Lock()
	h := f.handler
	f.mu.Unlock()
	if h != nil {
		h(ev)
	}
}

// Sent returns a copy of the recorded sends.
func (f *Fake) Sent() []SentRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sent)
}

// ConnectCalls returns how many times Connect was called.
func (f *Fake) ConnectCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connectCalls
}

// LogoutCalls returns how many times Logout was called.
func (f *Fake) LogoutCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logoutCalls
}

// SetEventHandler registers the event handler.
func (f *Fake) SetEventHandler(h func(wa.Event)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = h
}

// Connect records the call; an unpaired Fake emits the configured QR codes.
func (f *Fake) Connect(context.Context) error {
	f.mu.Lock()
	f.connectCalls++
	paired := f.status.Paired
	codes := slices.Clone(f.qrCodes)
	f.mu.Unlock()

	if paired {
		return nil
	}
	for _, code := range codes {
		f.Emit(wa.QRCodeEvent{Code: code, Timeout: time.Minute})
	}
	return nil
}

// Disconnect marks the client disconnected.
func (f *Fake) Disconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status.State = wa.StateDisconnected
}

// Status returns the configured status.
func (f *Fake) Status() wa.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

// SendText records a text message.
func (f *Fake) SendText(_ context.Context, chatJID, text string) (wa.SentMessage, error) {
	return f.record(SentRecord{ChatJID: chatJID, Text: text})
}

// SendFile records a file message.
func (f *Fake) SendFile(_ context.Context, chatJID string, file wa.OutgoingFile) (wa.SentMessage, error) {
	file.Data = slices.Clone(file.Data)
	return f.record(SentRecord{ChatJID: chatJID, Text: file.Caption, File: &file})
}

func (f *Fake) record(r SentRecord) (wa.SentMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return wa.SentMessage{}, f.sendErr
	}
	f.seq++
	r.At = f.now()
	f.sent = append(f.sent, r)
	return wa.SentMessage{ID: fmt.Sprintf("FAKE%06d", f.seq), ChatJID: r.ChatJID, Timestamp: r.At}, nil
}

// DownloadMedia returns the data registered for m.DirectPath.
func (f *Fake) DownloadMedia(_ context.Context, m wa.Media) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.media[m.DirectPath]
	if !ok {
		return nil, errors.New("media not available")
	}
	return slices.Clone(data), nil
}

// Contacts returns a copy of the configured contacts.
func (f *Fake) Contacts(context.Context) ([]wa.Contact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.contacts), nil
}

// SetResync makes ResyncContacts return outcome and err.
func (f *Fake) SetResync(outcome wa.ResyncOutcome, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resyncOutcome, f.resyncErr = outcome, err
}

// ResyncCalls returns how many times ResyncContacts was called.
func (f *Fake) ResyncCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resyncCalls
}

// SetContactSync replaces the status returned by ContactSync.
func (f *Fake) SetContactSync(st wa.ContactSyncStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.contactSync = st
	f.contactSync.Patches = slices.Clone(st.Patches)
}

// ResyncContacts records the call and returns the configured outcome
// (ResyncStarted by default). It never touches the send path.
func (f *Fake) ResyncContacts(context.Context) (wa.ResyncOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resyncCalls++
	if f.resyncErr != nil {
		return "", f.resyncErr
	}
	if f.resyncOutcome == "" {
		return wa.ResyncStarted, nil
	}
	return f.resyncOutcome, nil
}

// ContactSync returns the configured status (ContactSyncNotRun by default).
func (f *Fake) ContactSync(context.Context) (wa.ContactSyncStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.contactSync
	st.Patches = slices.Clone(f.contactSync.Patches)
	if st.State == "" {
		st.State = wa.ContactSyncNotRun
	}
	return st, nil
}

// Logout records the call and marks the client logged out and unpaired.
func (f *Fake) Logout(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logoutCalls++
	f.status = wa.Status{State: wa.StateLoggedOut}
	return nil
}

// EditText records an outgoing edit.
func (f *Fake) EditText(_ context.Context, chatJID, id, text string) (wa.SentMessage, error) {
	return f.record(SentRecord{ChatJID: chatJID, MessageID: id, Text: text, Operation: "edit"})
}

// RevokeMessage records an outgoing revoke.
func (f *Fake) RevokeMessage(_ context.Context, chatJID, id string) (wa.SentMessage, error) {
	return f.record(SentRecord{ChatJID: chatJID, MessageID: id, Operation: "revoke"})
}

// PostStatus records a story separately from chat sends.
func (f *Fake) PostStatus(_ context.Context, file wa.OutgoingFile) (wa.SentMessage, error) {
	file.Data = slices.Clone(file.Data)
	return f.record(SentRecord{Operation: "post_status", ChatJID: "status@broadcast", Text: file.Caption, File: &file})
}
