// Package wameow is the whatsmeow adapter behind the wa.Client port.
//
// The adapter only reads, pairs, connects and sends what a caller explicitly
// asks it to send. It never marks messages as read, publishes presence or chat
// presence, or requests history from the phone.
package wameow

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"

	"github.com/fmflurry/whatsapp-mcp/internal/datadir"
	"github.com/fmflurry/whatsapp-mcp/internal/redact"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

const (
	osName          = "whatsapp-mcp"
	sqlitePragmas   = "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	sqliteDriver    = "sqlite"
	sqliteDialect   = "sqlite3"
	invalidJIDError = "invalid chat JID"
)

var osVersion = [3]uint32{0, 1, 0}

// Client implements wa.Client on top of a whatsmeow multi-device session.
type Client struct {
	reconnect *reconnecter
	cli       *whatsmeow.Client
	db        *sql.DB
	dbPath    string
	log       *slog.Logger

	lifeCtx    context.Context
	lifeCancel context.CancelFunc

	opMu           sync.Mutex
	SendTimeout    time.Duration
	keepAliveCount int
	lastKeepAlive  time.Time
	lastHeal       time.Time
	healing        bool
	now            func() time.Time
	healHook       func(context.Context) error

	mu            sync.Mutex
	handler       func(wa.Event)
	connecting    bool
	loggedOut     bool
	qrActive      bool
	qrGen         uint64
	qrCancel      context.CancelFunc
	pairedEmitted bool

	uploadHook        func(context.Context, []byte, wa.MediaType) (uploaded, error)
	sendGate          chan struct{}
	sendHook          func(context.Context, types.JID, *waE2E.Message, whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
	idHook            func() string
	sync              *appStateSyncer
	appStateMutations atomic.Int64
}

var _ wa.Client = (*Client)(nil)

// Open prepares the session database at sessionDBPath (created with mode 0600)
// and builds a whatsmeow client from its first device. It does not touch the
// network.
func Open(ctx context.Context, sessionDBPath string, log *slog.Logger) (*Client, error) {
	if log == nil {
		log = slog.Default()
	}
	abs, err := filepath.Abs(sessionDBPath)
	if err != nil {
		return nil, fmt.Errorf("resolve session db path: %w", err)
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_RDWR, datadir.FilePerm)
	if err != nil {
		return nil, fmt.Errorf("create session db: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close session db: %w", err)
	}

	dsn := (&url.URL{Scheme: "file", Path: abs, RawQuery: sqlitePragmas}).String()
	db, err := sql.Open(sqliteDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open session db: %w", err)
	}

	waLogger := newWALogger(log)
	container := sqlstore.NewWithDB(db, sqliteDialect, waLogger.Sub("Database"))
	if err := container.Upgrade(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("upgrade session db: %w", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("load device: %w", err)
	}
	store.SetOSInfo(osName, osVersion)

	lifeCtx, lifeCancel := context.WithCancel(context.Background())
	c := &Client{
		cli:        whatsmeow.NewClient(device, waLogger.Sub("Client")),
		db:         db,
		dbPath:     abs,
		log:        log,
		lifeCtx:    lifeCtx,
		lifeCancel: lifeCancel,
	}
	c.reconnect = newReconnecter(log, c.Connect)
	c.cli.InitialAutoReconnect = true
	// Full syncs then report every address-book entry as an events.Contact,
	// whose LID/phone-number pair onContactAction records.
	c.cli.EmitAppStateEventsOnFullSync = true
	c.sync = newAppStateSyncer(whatsmeowAppState{c: c}, log, c.publishContactNames)
	c.cli.AddEventHandler(c.handleEvent)
	if err := c.hardenFiles(); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) hardenFiles() error {
	return datadir.Harden(c.dbPath, c.dbPath+"-wal", c.dbPath+"-shm")
}

// Paired reports whether the session store holds a linked device.
func (c *Client) Paired() bool { return c.cli.Store.ID != nil }

// Close disconnects and releases the session database.
func (c *Client) Close() error {
	c.lifeCancel()
	c.Disconnect()
	err := c.db.Close()
	if herr := c.hardenFiles(); err == nil {
		err = herr
	}
	return err
}

// SetEventHandler implements wa.Client.
func (c *Client) SetEventHandler(h func(wa.Event)) {
	c.mu.Lock()
	c.handler = h
	c.mu.Unlock()
}

func (c *Client) emit(ev wa.Event) {
	c.mu.Lock()
	h := c.handler
	c.mu.Unlock()
	if h != nil {
		h(ev)
	}
}

// Connect implements wa.Client. An unpaired account starts a QR flow whose
// codes are delivered as events and which outlives ctx; a paired account
// connects directly.
func (c *Client) Connect(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.opMu.Lock()
	defer c.opMu.Unlock()

	if c.cli.Store.ID == nil {
		return c.connectUnpaired(ctx)
	}
	if c.cli.IsConnected() {
		return nil
	}
	c.mu.Lock()
	c.connecting = true
	c.loggedOut = false
	c.mu.Unlock()
	if err := c.cli.Connect(); err != nil {
		c.setConnecting(false)
		return scrubbed("connect", err)
	}
	return nil
}

func (c *Client) connectUnpaired(ctx context.Context) error {
	c.mu.Lock()
	active := c.qrActive
	c.mu.Unlock()
	switch qrStartAction(c.cli.IsConnected(), active) {
	case qrKeepFlow:
		return nil
	case qrReconnect:
		c.cli.Disconnect()
	case qrStartFlow:
	}

	qrCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	ch, err := c.cli.GetQRChannel(qrCtx)
	if err != nil {
		cancel()
		return scrubbed("start pairing", err)
	}

	c.mu.Lock()
	if c.qrCancel != nil {
		c.qrCancel()
	}
	c.qrGen++
	gen := c.qrGen
	c.qrCancel = cancel
	c.qrActive = true
	c.loggedOut = false
	c.pairedEmitted = false
	c.mu.Unlock()

	go c.forwardQR(qrCtx, gen, ch)

	if err := c.cli.Connect(); err != nil {
		c.endQR(gen)
		cancel()
		return scrubbed("connect", err)
	}
	return nil
}

type qrAction int

const (
	qrStartFlow qrAction = iota
	qrKeepFlow
	qrReconnect
)

// qrStartAction decides how to begin a QR flow. whatsmeow only hands out a QR
// channel before the socket opens, so a connection that is not running a QR
// flow must be dropped first.
func qrStartAction(connected, qrActive bool) qrAction {
	switch {
	case !connected:
		return qrStartFlow
	case qrActive:
		return qrKeepFlow
	default:
		return qrReconnect
	}
}

func (c *Client) forwardQR(ctx context.Context, gen uint64, ch <-chan whatsmeow.QRChannelItem) {
	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-ch:
			if !ok {
				c.endQR(gen)
				return
			}
			if !c.handleQRItem(gen, item) {
				return
			}
		}
	}
}

// handleQRItem forwards one QR channel item and reports whether the channel
// should keep being read.
func (c *Client) handleQRItem(gen uint64, item whatsmeow.QRChannelItem) bool {
	switch item.Event {
	case whatsmeow.QRChannelEventCode:
		if c.currentQR(gen) {
			c.emit(wa.QRCodeEvent{Code: item.Code, Timeout: item.Timeout})
		}
		return true
	case whatsmeow.QRChannelEventPasskeyRequest, whatsmeow.QRChannelEventPasskeyResponse:
		c.log.Warn("pairing asked for a passkey, which is not supported")
		return true
	case whatsmeow.QRChannelSuccess.Event:
		if c.endQR(gen) {
			c.onPaired(c.ownJID().String())
		}
		return false
	default:
		if item.Error != nil {
			c.log.Warn("pairing failed", "event", item.Event, "error", redact.Scrub(item.Error.Error()))
		}
		if c.endQR(gen) {
			c.emit(wa.QRTimeoutEvent{})
		}
		return false
	}
}

func (c *Client) currentQR(gen uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.qrActive && c.qrGen == gen
}

// endQR marks the QR flow of generation gen as finished and reports whether
// gen was still the active flow.
func (c *Client) endQR(gen uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.qrGen != gen || !c.qrActive {
		return false
	}
	c.qrActive = false
	return true
}

func (c *Client) onPaired(account string) {
	c.mu.Lock()
	if c.pairedEmitted {
		c.mu.Unlock()
		return
	}
	c.pairedEmitted = true
	c.qrActive = false
	c.mu.Unlock()
	c.emit(wa.PairedEvent{AccountJID: account})
}

func (c *Client) setConnecting(v bool) {
	c.mu.Lock()
	c.connecting = v
	c.mu.Unlock()
}

func (c *Client) setConnected() {
	c.mu.Lock()
	c.connecting = false
	c.loggedOut = false
	c.keepAliveCount = 0
	c.mu.Unlock()
}

func (c *Client) setLoggedOut() {
	c.mu.Lock()
	c.connecting = false
	c.loggedOut = true
	c.qrActive = false
	c.mu.Unlock()
}

// Disconnect implements wa.Client.
func (c *Client) Disconnect() {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	c.mu.Lock()
	if c.qrCancel != nil {
		c.qrCancel()
		c.qrCancel = nil
	}
	c.qrActive = false
	c.connecting = false
	c.mu.Unlock()
	c.cli.Disconnect()
}

// Status implements wa.Client.
func (c *Client) Status() wa.Status {
	c.mu.Lock()
	qrActive, connecting, loggedOut := c.qrActive, c.connecting, c.loggedOut
	c.mu.Unlock()

	st := wa.Status{}
	if c.reconnect != nil {
		c.reconnect.mu.Lock()
		st.StreamReplacedCount = c.reconnect.count
		st.LastDisconnectReason = c.reconnect.reason
		c.reconnect.mu.Unlock()
	}
	id := c.cli.Store.ID
	if id == nil {
		switch {
		case qrActive:
			st.State = wa.StateNeedsQR
		case loggedOut:
			st.State = wa.StateLoggedOut
		default:
			st.State = wa.StateDisconnected
		}
		return st
	}
	st.Paired = true
	st.AccountJID = id.ToNonAD().String()

	switch {
	case c.cli.IsConnected() && c.cli.IsLoggedIn():
		st.State = wa.StateConnected
	case connecting:
		st.State = wa.StateConnecting
	case loggedOut:
		st.State = wa.StateLoggedOut
	default:
		st.State = wa.StateDisconnected
	}
	return st
}

func (c *Client) target(chatJID string) (types.JID, error) {
	if !c.HealthyForSend() {
		return types.EmptyJID, wa.ErrNotSentStale
	}
	jid, err := types.ParseJID(chatJID)
	if err != nil || jid.User == "" || jid.Server == "" {
		return types.EmptyJID, fmt.Errorf("%w: %s", wa.ErrNotSent, invalidJIDError)
	}
	return jid, nil
}

// SendText implements wa.Client.
func (c *Client) SendText(ctx context.Context, chatJID, text string) (wa.SentMessage, error) {
	jid, err := c.target(chatJID)
	if err != nil {
		return wa.SentMessage{}, err
	}
	sendCtx, cancel := context.WithTimeout(ctx, c.sendTimeout())
	defer cancel()
	return c.send(sendCtx, jid, &waE2E.Message{Conversation: proto.String(text)})
}

// SendFile implements wa.Client.
func (c *Client) SendFile(ctx context.Context, chatJID string, f wa.OutgoingFile) (wa.SentMessage, error) {
	jid, err := c.target(chatJID)
	if err != nil {
		return wa.SentMessage{}, err
	}
	if len(f.Data) == 0 {
		return wa.SentMessage{}, fmt.Errorf("%w: file is empty", wa.ErrNotSent)
	}
	kind := wa.OutgoingMediaType(f.MimeType)
	sendCtx, cancel := context.WithTimeout(ctx, c.sendTimeout())
	defer cancel()
	up, err := c.upload(sendCtx, f.Data, kind)
	if err != nil {
		if sendCtx.Err() != nil || staleTimeout(err) {
			c.selfHeal("upload timeout")
			return wa.SentMessage{}, wa.ErrNotSentStale
		}
		return wa.SentMessage{}, scrubbed("upload file", notSent(err))
	}
	msg := buildOutgoingMedia(kind, up, f)
	return c.send(sendCtx, jid, msg)
}

// notSent marks err as a failure that happened before any send.
func notSent(err error) error { return fmt.Errorf("%w: %w", wa.ErrNotSent, err) }

// sendFailure marks the SendMessage errors that whatsmeow returns before
// writing anything to the socket. Timeouts and server errors are left
// unmarked because the message may have been delivered.
func sendFailure(err error) error {
	for _, pre := range []error{
		whatsmeow.ErrNotLoggedIn, whatsmeow.ErrNotConnected, whatsmeow.ErrRecipientADJID,
		whatsmeow.ErrUnknownServer, whatsmeow.ErrBroadcastListUnsupported, whatsmeow.ErrNoSession,
	} {
		if errors.Is(err, pre) {
			return notSent(err)
		}
	}
	text := strings.ToLower(err.Error())
	for _, pattern := range []string{"failed to get device list", "failed to get group members", "failed to send usync", "failed to query usync", "websocket not connected", "websocket is not connected", "websocket: not connected", "failed to get devices", "failed to query devices"} {
		if strings.Contains(text, pattern) {
			return notSent(err)
		}
	}
	return err
}

func sentMessage(resp whatsmeow.SendResponse, to types.JID) wa.SentMessage {
	return wa.SentMessage{ID: resp.ID, ChatJID: to.String(), Timestamp: resp.Timestamp.UTC()}
}

// DownloadMedia implements wa.Client.
func (c *Client) DownloadMedia(ctx context.Context, m wa.Media) ([]byte, error) {
	if c.cli.Store.ID == nil {
		return nil, wa.ErrNotPaired
	}
	if !c.cli.IsConnected() || !c.cli.IsLoggedIn() {
		return nil, wa.ErrNotConnected
	}
	if m.DirectPath == "" {
		return nil, errors.New("media has no download path")
	}
	mediaKey := m.MediaKey
	if m.FileEncSHA256 == nil {
		mediaKey = nil
	}
	data, err := c.cli.DownloadMediaWithPath(ctx, m.DirectPath, m.FileEncSHA256, m.FileSHA256, mediaKey,
		whatsmeowMediaType(m.Type), "", false)
	if err != nil {
		return nil, scrubbed("download media", err)
	}
	return data, nil
}

// Logout implements wa.Client.
func (c *Client) Logout(ctx context.Context) error {
	if c.cli.Store.ID == nil {
		return wa.ErrNotPaired
	}
	if err := c.cli.Logout(ctx); err != nil {
		return scrubbed("logout", err)
	}
	c.setLoggedOut()
	return nil
}

// scrubbedError hides phone numbers in a whatsmeow error message while keeping
// the original error reachable for errors.Is and errors.As.
type scrubbedError struct {
	op  string
	err error
}

func (e *scrubbedError) Error() string { return e.op + ": " + redact.Scrub(e.err.Error()) }
func (e *scrubbedError) Unwrap() error { return e.err }

func scrubbed(op string, err error) error {
	return &scrubbedError{op: op, err: err}
}

// EditText implements wa.Client.
func (c *Client) EditText(ctx context.Context, chatJID, id, text string) (wa.SentMessage, error) {
	jid, err := c.target(chatJID)
	if err != nil {
		return wa.SentMessage{}, err
	}
	sendCtx, cancel := context.WithTimeout(ctx, c.sendTimeout())
	defer cancel()
	return c.send(sendCtx, jid, c.cli.BuildEdit(jid, id, &waE2E.Message{Conversation: proto.String(text)}))
}

// RevokeMessage implements wa.Client.
func (c *Client) RevokeMessage(ctx context.Context, chatJID, id string) (wa.SentMessage, error) {
	jid, err := c.target(chatJID)
	if err != nil {
		return wa.SentMessage{}, err
	}
	sendCtx, cancel := context.WithTimeout(ctx, c.sendTimeout())
	defer cancel()
	return c.send(sendCtx, jid, c.cli.BuildRevoke(jid, types.EmptyJID, id))
}
