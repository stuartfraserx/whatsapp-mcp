package tools_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmflurry/whatsapp-mcp/internal/qr"
	"github.com/fmflurry/whatsapp-mcp/internal/ratelimit"
	"github.com/fmflurry/whatsapp-mcp/internal/sendpolicy"
	"github.com/fmflurry/whatsapp-mcp/internal/session"
	"github.com/fmflurry/whatsapp-mcp/internal/store"
	"github.com/fmflurry/whatsapp-mcp/internal/tools"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
	"github.com/fmflurry/whatsapp-mcp/internal/wa/watest"
)

const (
	aliceJID = "33611111111@s.whatsapp.net"
	bobJID   = "33622222222@s.whatsapp.net"
	carolJID = "33633333333@s.whatsapp.net"
	groupJID = "120363000000000001@g.us"
	selfJID  = "33699999999@s.whatsapp.net"

	sendWarning = "Sends immediately and irreversibly. Only call when the user explicitly asked to send this exact message to this exact recipient."
)

var epoch = time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
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

type options struct {
	sendDisabled      bool
	noPolicy          bool
	policyOff         bool
	noDownloadDir     bool
	allowlist         []string
	interval          time.Duration
	hourly            int
	downloadDir       string
	downloadLimit     int64
	downloadIsDataDir bool
	status            *wa.Status
	terminal          *bytes.Buffer
}

type env struct {
	t         *testing.T
	fake      *watest.Fake
	store     *store.Store
	sess      *session.Session
	clock     *fakeClock
	cs        *mcp.ClientSession
	root      string
	outbox    string
	downloads string
	qrPath    string
}

func newEnv(t *testing.T, mods ...func(*options)) *env {
	t.Helper()
	opts := options{hourly: 1000}
	for _, m := range mods {
		m(&opts)
	}

	root := t.TempDir()
	e := &env{
		t:         t,
		fake:      watest.NewFake(),
		clock:     &fakeClock{now: epoch},
		root:      root,
		outbox:    filepath.Join(root, "outbox"),
		downloads: filepath.Join(root, "downloads"),
		qrPath:    filepath.Join(root, "pairing-qr.png"),
	}
	if opts.downloadDir != "" {
		e.downloads = opts.downloadDir
	}
	if opts.downloadIsDataDir {
		e.downloads = root
	}
	if err := os.MkdirAll(e.outbox, 0o700); err != nil {
		t.Fatal(err)
	}
	e.fake.SetClock(func() time.Time { return epoch })
	status := wa.Status{State: wa.StateConnected, AccountJID: selfJID, Paired: true}
	if opts.status != nil {
		status = *opts.status
	}
	e.fake.SetStatus(status)

	st, err := store.Open(filepath.Join(root, "messages.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e.store = st

	presenter := qr.Presenter{PNGPath: e.qrPath}
	if opts.terminal != nil {
		presenter.Terminal = opts.terminal
	}
	e.sess = session.New(session.Options{Client: e.fake, Store: st, QR: presenter, Clock: e.clock})

	allow, err := sendpolicy.ParseAllowlist(opts.allowlist)
	if err != nil {
		t.Fatalf("ParseAllowlist: %v", err)
	}
	var policy *sendpolicy.Policy
	if !opts.noPolicy {
		policy = &sendpolicy.Policy{
			Enabled:   !opts.sendDisabled && !opts.policyOff,
			Allowlist: allow,
			Limiter:   ratelimit.New(opts.interval, opts.hourly, e.clock),
		}
	}

	deliveryDir := e.downloads
	if opts.noDownloadDir {
		deliveryDir = ""
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: "test-server", Version: "0"}, nil)
	tools.Register(srv, tools.Deps{
		Client:        e.fake,
		Store:         st,
		Session:       e.sess,
		Policy:        policy,
		SendEnabled:   !opts.sendDisabled,
		DataDir:       root,
		DownloadDir:   deliveryDir,
		DownloadLimit: opts.downloadLimit,
		OutboxDir:     e.outbox,
		RateInterval:  opts.interval,
		RateHourly:    opts.hourly,
	})

	ctx := context.Background()
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		_ = ss.Close()
	})
	e.cs = cs
	return e
}

func withSendDisabled(o *options) { o.sendDisabled = true }
func withNoPolicy(o *options)     { o.noPolicy = true }
func withPolicyOff(o *options)    { o.policyOff = true }
func withAllowlist(a ...string) func(*options) {
	return func(o *options) { o.allowlist = a }
}
func withRate(interval time.Duration, hourly int) func(*options) {
	return func(o *options) { o.interval, o.hourly = interval, hourly }
}
func withStatus(st wa.Status) func(*options)      { return func(o *options) { o.status = &st } }
func withDownloadDir(dir string) func(*options)   { return func(o *options) { o.downloadDir = dir } }
func withDownloadLimit(n int64) func(*options)    { return func(o *options) { o.downloadLimit = n } }
func withTerminal(b *bytes.Buffer) func(*options) { return func(o *options) { o.terminal = b } }

func (e *env) ctx() context.Context {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	e.t.Cleanup(cancel)
	return ctx
}

func (e *env) call(name string, args any) *mcp.CallToolResult {
	e.t.Helper()
	res, err := e.cs.CallTool(e.ctx(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		e.t.Fatalf("%s: protocol-level error (validation failures must be tool errors): %v", name, err)
	}
	return res
}

func resultText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (e *env) ok(name string, args any) *mcp.CallToolResult {
	e.t.Helper()
	res := e.call(name, args)
	if res.IsError {
		e.t.Fatalf("%s(%v) returned a tool error: %s", name, args, resultText(res))
	}
	return res
}

func (e *env) fail(name string, args any, wantSubstr string) string {
	e.t.Helper()
	res := e.call(name, args)
	if !res.IsError {
		e.t.Fatalf("%s(%v) succeeded, want a tool error containing %q; result: %s", name, args, wantSubstr, rawJSON(e.t, res))
	}
	text := resultText(res)
	if !strings.Contains(strings.ToLower(text), strings.ToLower(wantSubstr)) {
		e.t.Fatalf("%s(%v) error = %q, want it to contain %q", name, args, text, wantSubstr)
	}
	return text
}

func rawJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decode[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	var out T
	if err := json.Unmarshal([]byte(rawJSON(t, res.StructuredContent)), &out); err != nil {
		t.Fatalf("decode structured content %s: %v", rawJSON(t, res.StructuredContent), err)
	}
	return out
}

func callAs[T any](e *env, name string, args any) T {
	e.t.Helper()
	return decode[T](e.t, e.ok(name, args))
}

func (e *env) seedChats(chats ...wa.Chat) {
	e.t.Helper()
	if err := e.store.UpsertChats(e.ctx(), chats); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) seedMessages(msgs ...wa.Message) {
	e.t.Helper()
	for _, m := range msgs {
		if err := e.store.SaveMessage(e.ctx(), m); err != nil {
			e.t.Fatal(err)
		}
	}
}

func msg(id, chat string, at time.Time, text string) wa.Message {
	return wa.Message{ID: id, ChatJID: chat, SenderJID: chat, SenderName: "Sender", Timestamp: at, Text: text}
}

func (e *env) assertNothingSent() {
	e.t.Helper()
	if sent := e.fake.Sent(); len(sent) != 0 {
		e.t.Fatalf("expected nothing to be sent, got %d record(s): %+v", len(sent), sent)
	}
}

func (e *env) writeFile(path string, data []byte) string {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		e.t.Fatal(err)
	}
	return path
}

func assertPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("%s mode = %o, want %o", path, info.Mode().Perm(), want)
	}
}

func listTools(t *testing.T, e *env) map[string]*mcp.Tool {
	t.Helper()
	res, err := e.cs.ListTools(e.ctx(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tl := range res.Tools {
		byName[tl.Name] = tl
	}
	return byName
}

func TestListToolsWithSendEnabledExposesAllTenTools(t *testing.T) {
	e := newEnv(t)
	got := listTools(t, e)

	want := []string{
		"list_chats", "search_contacts", "list_messages", "get_message_context", "download_media",
		"send_message", "send_file", "connection_status", "show_pairing_qr", "resync_contacts",
	}
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	slices.Sort(names)
	wantSorted := slices.Sorted(slices.Values(want))
	if !slices.Equal(names, wantSorted) {
		t.Fatalf("tools = %v, want %v", names, wantSorted)
	}
}

func TestListToolsWithSendDisabledOmitsSendTools(t *testing.T) {
	e := newEnv(t, withSendDisabled)
	got := listTools(t, e)

	if len(got) != 8 {
		t.Fatalf("got %d tools, want 8: %v", len(got), got)
	}
	for _, name := range []string{"send_message", "send_file"} {
		if _, present := got[name]; present {
			t.Errorf("%s must not be registered when sending is disabled", name)
		}
		_, err := e.cs.CallTool(e.ctx(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"recipient": aliceJID, "text": "x"}})
		if err == nil {
			t.Errorf("calling unregistered %s unexpectedly succeeded", name)
		}
	}
	e.assertNothingSent()
}

func TestSendToolDescriptionsCarryIrreversibilityWarning(t *testing.T) {
	got := listTools(t, newEnv(t))

	for _, name := range []string{"send_message", "send_file"} {
		if !strings.Contains(got[name].Description, sendWarning) {
			t.Errorf("%s description = %q, want it to contain %q", name, got[name].Description, sendWarning)
		}
		a := got[name].Annotations
		if a == nil || a.ReadOnlyHint {
			t.Errorf("%s must not be marked read-only: %+v", name, a)
		}
		if a == nil || a.DestructiveHint == nil || !*a.DestructiveHint {
			t.Errorf("%s must be annotated destructive: %+v", name, a)
		}
	}
	for name, tl := range got {
		if name != "send_message" && name != "send_file" && strings.Contains(tl.Description, sendWarning) {
			t.Errorf("%s must not carry the send warning", name)
		}
	}
}

func TestReadToolsAreAnnotatedReadOnly(t *testing.T) {
	got := listTools(t, newEnv(t))

	for _, name := range []string{"list_chats", "search_contacts", "list_messages", "get_message_context", "connection_status"} {
		if a := got[name].Annotations; a == nil || !a.ReadOnlyHint {
			t.Errorf("%s ReadOnlyHint = false, want true", name)
		}
	}
	for _, name := range []string{"download_media", "show_pairing_qr"} {
		if a := got[name].Annotations; a != nil && a.ReadOnlyHint {
			t.Errorf("%s writes files and must not be read-only", name)
		}
	}
}

func TestEveryInputPropertyIsDocumentedAndRequiredFieldsDeclared(t *testing.T) {
	got := listTools(t, newEnv(t))
	required := map[string][]string{
		"search_contacts":     {"query"},
		"get_message_context": {"message_id"},
		"download_media":      {"message_id"},
		"send_message":        {"recipient", "text"},
		"send_file":           {"recipient", "path"},
	}

	for name, tl := range got {
		var schema struct {
			Properties map[string]struct {
				Description string `json:"description"`
			} `json:"properties"`
			Required []string `json:"required"`
		}
		if err := json.Unmarshal([]byte(rawJSON(t, tl.InputSchema)), &schema); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for prop, def := range schema.Properties {
			if strings.TrimSpace(def.Description) == "" {
				t.Errorf("%s.%s has no description", name, prop)
			}
		}
		for _, r := range required[name] {
			if !slices.Contains(schema.Required, r) {
				t.Errorf("%s: %q must be a required input, required=%v", name, r, schema.Required)
			}
		}
	}
}

func TestValidationFailuresAreToolErrorsNotProtocolErrors(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name    string
		tool    string
		args    any
		wantErr string
	}{
		{"search_contacts missing query", "search_contacts", map[string]any{}, "query"},
		{"search_contacts blank query", "search_contacts", map[string]any{"query": "   "}, "query is required"},
		{"search_contacts limit too high", "search_contacts", map[string]any{"query": "a", "limit": 201}, "limit"},
		{"get_message_context missing id", "get_message_context", map[string]any{}, "message_id"},
		{"get_message_context blank id", "get_message_context", map[string]any{"message_id": " "}, "message_id is required"},
		{"get_message_context before too big", "get_message_context", map[string]any{"message_id": "x", "before": 51}, "before"},
		{"get_message_context after negative", "get_message_context", map[string]any{"message_id": "x", "after": -1}, "after"},
		{"get_message_context bad chat jid", "get_message_context", map[string]any{"message_id": "x", "chat_jid": "nope"}, "chat_jid"},
		{"download_media missing id", "download_media", map[string]any{}, "message_id"},
		{"download_media bad chat jid", "download_media", map[string]any{"message_id": "x", "chat_jid": "x@evil.com"}, "chat_jid"},
		{"list_chats limit too high", "list_chats", map[string]any{"limit": 201}, "limit"},
		{"list_chats negative limit", "list_chats", map[string]any{"limit": -5}, "limit"},
		{"list_chats negative offset", "list_chats", map[string]any{"offset": -1}, "offset"},
		{"list_chats bad sort", "list_chats", map[string]any{"sort_by": "random"}, "sort_by"},
		{"list_chats limit wrong type", "list_chats", map[string]any{"limit": "ten"}, "limit"},
		{"list_messages bad after", "list_messages", map[string]any{"after": "yesterday"}, "after"},
		{"list_messages date without zone", "list_messages", map[string]any{"after": "2025-01-31T09:00:00"}, "after"},
		{"list_messages bad before", "list_messages", map[string]any{"before": "2025-13-01T00:00:00Z"}, "before"},
		{"list_messages after equals before", "list_messages", map[string]any{"after": "2025-01-01T00:00:00Z", "before": "2025-01-01T00:00:00Z"}, "earlier than before"},
		{"list_messages after later than before", "list_messages", map[string]any{"after": "2025-02-01T00:00:00Z", "before": "2025-01-01T00:00:00Z"}, "earlier than before"},
		{"list_messages equal instants in different zones", "list_messages", map[string]any{"after": "2025-01-01T01:00:00+01:00", "before": "2025-01-01T00:00:00Z"}, "earlier than before"},
		{"list_messages bad chat jid", "list_messages", map[string]any{"chat_jid": "12345"}, "chat_jid"},
		{"list_messages bad sender jid", "list_messages", map[string]any{"sender_jid": "@s.whatsapp.net"}, "sender_jid"},
		{"list_messages limit zero-above", "list_messages", map[string]any{"limit": 1000}, "limit"},
		{"send_message missing text", "send_message", map[string]any{"recipient": aliceJID}, "text"},
		{"send_message missing recipient", "send_message", map[string]any{"text": "hi"}, "recipient"},
		{"send_message blank recipient", "send_message", map[string]any{"recipient": "  ", "text": "hi"}, "recipient is required"},
		{"send_message blank text", "send_message", map[string]any{"recipient": aliceJID, "text": " \n\t "}, "must not be blank"},
		{"send_message bad jid", "send_message", map[string]any{"recipient": "alice@nowhere", "text": "hi"}, "valid whatsapp jid"},
		{"send_message empty user jid", "send_message", map[string]any{"recipient": "@s.whatsapp.net", "text": "hi"}, "valid whatsapp jid"},
		{"send_file missing path", "send_file", map[string]any{"recipient": aliceJID}, "path"},
		{"send_file blank path", "send_file", map[string]any{"recipient": aliceJID, "path": " "}, "path is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e.fail(tt.tool, tt.args, tt.wantErr)
		})
	}
	e.assertNothingSent()
}

func TestToolsAcceptNoArgumentsWhenAllInputsAreOptional(t *testing.T) {
	e := newEnv(t)

	for _, name := range []string{"list_chats", "list_messages", "connection_status"} {
		res, err := e.cs.CallTool(e.ctx(), &mcp.CallToolParams{Name: name})
		if err != nil {
			t.Fatalf("%s with nil arguments: %v", name, err)
		}
		if res.IsError {
			t.Fatalf("%s with nil arguments: %s", name, resultText(res))
		}
	}
}

type statusOut struct {
	State               string  `json:"state"`
	Paired              bool    `json:"paired"`
	AccountJID          string  `json:"account_jid"`
	SendEnabled         bool    `json:"send_enabled"`
	AllowlistActive     bool    `json:"allowlist_active"`
	RateIntervalSeconds float64 `json:"rate_interval_seconds"`
	RateHourlyCap       int     `json:"rate_hourly_cap"`
}

func TestConnectionStatusFields(t *testing.T) {
	tests := []struct {
		name   string
		mods   []func(*options)
		status wa.Status
		want   statusOut
	}{
		{
			name:   "paired and connected with default policy",
			mods:   []func(*options){withRate(3*time.Second, 60)},
			status: wa.Status{State: wa.StateConnected, AccountJID: selfJID, Paired: true},
			want: statusOut{State: "connected", Paired: true, AccountJID: selfJID, SendEnabled: true,
				RateIntervalSeconds: 3, RateHourlyCap: 60},
		},
		{
			name:   "allowlist reported active",
			mods:   []func(*options){withRate(1500*time.Millisecond, 10), withAllowlist("+33 6 11 11 11 11")},
			status: wa.Status{State: wa.StateConnected, AccountJID: selfJID, Paired: true},
			want: statusOut{State: "connected", Paired: true, AccountJID: selfJID, SendEnabled: true, AllowlistActive: true,
				RateIntervalSeconds: 1.5, RateHourlyCap: 10},
		},
		{
			name:   "unpaired needs a QR scan",
			mods:   []func(*options){withRate(3*time.Second, 60)},
			status: wa.Status{State: wa.StateNeedsQR},
			want:   statusOut{State: "needs_qr_scan", SendEnabled: true, RateIntervalSeconds: 3, RateHourlyCap: 60},
		},
		{
			name:   "send disabled hides rate settings",
			mods:   []func(*options){withSendDisabled, withRate(3*time.Second, 60)},
			status: wa.Status{State: wa.StateDisconnected, AccountJID: selfJID, Paired: true},
			want:   statusOut{State: "disconnected", Paired: true, AccountJID: selfJID},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.mods...)
			e.fake.SetStatus(tt.status)

			got := callAs[statusOut](e, "connection_status", map[string]any{})
			if got != tt.want {
				t.Fatalf("connection_status = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestConnectionStatusOmitsAccountWhenUnpaired(t *testing.T) {
	e := newEnv(t)
	e.fake.SetStatus(wa.Status{State: wa.StateNeedsQR})

	raw := rawJSON(t, e.ok("connection_status", map[string]any{}).StructuredContent)
	if strings.Contains(raw, "account_jid") {
		t.Fatalf("account_jid must be omitted when unpaired: %s", raw)
	}
}

type qrOut struct {
	Status             string `json:"status"`
	PNGPath            string `json:"png_path"`
	ExpiresAt          string `json:"expires_at"`
	RenderedInTerminal bool   `json:"rendered_in_terminal"`
	Instructions       string `json:"instructions"`
}

func TestShowPairingQRWhenPaired(t *testing.T) {
	e := newEnv(t)

	got := callAs[qrOut](e, "show_pairing_qr", map[string]any{})
	if got.Status != "already_paired" || got.PNGPath != "" || got.ExpiresAt != "" {
		t.Fatalf("show_pairing_qr = %+v, want already_paired without a PNG", got)
	}
	if e.fake.ConnectCalls() != 0 {
		t.Fatal("a paired account must not restart the QR flow")
	}
	if _, err := os.Stat(e.qrPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no PNG must be written for a paired account, stat err = %v", err)
	}
}

func TestShowPairingQRWhenUnpairedWritesPNGAndNeverLeaksPayload(t *testing.T) {
	const payload = "2@PAYLOAD-SECRET-xYz9/abcDEF==,keyMaterial,identityKey"
	var term bytes.Buffer
	tests := []struct {
		name         string
		mods         []func(*options)
		wantRendered bool
	}{
		{"no terminal", nil, false},
		{"terminal attached", []func(*options){withTerminal(&term)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t, tt.mods...)
			e.fake.SetStatus(wa.Status{State: wa.StateNeedsQR})
			e.fake.SetQRCodesOnConnect(payload)

			res := e.ok("show_pairing_qr", map[string]any{})
			got := decode[qrOut](t, res)

			if got.Status != "scan_required" {
				t.Fatalf("status = %q, want scan_required", got.Status)
			}
			if got.PNGPath != e.qrPath {
				t.Fatalf("png_path = %q, want %q", got.PNGPath, e.qrPath)
			}
			assertPerm(t, got.PNGPath, 0o600)
			data, err := os.ReadFile(got.PNGPath)
			if err != nil || !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
				t.Fatalf("png_path does not hold a PNG (err=%v)", err)
			}
			if want := e.clock.Now().Add(time.Minute).Format(time.RFC3339); got.ExpiresAt != want {
				t.Fatalf("expires_at = %q, want %q", got.ExpiresAt, want)
			}
			if got.RenderedInTerminal != tt.wantRendered {
				t.Fatalf("rendered_in_terminal = %v, want %v", got.RenderedInTerminal, tt.wantRendered)
			}
			if got.Instructions == "" {
				t.Fatal("instructions must tell the user what to do")
			}
			full := rawJSON(t, res)
			for _, frag := range []string{payload, "PAYLOAD-SECRET", "keyMaterial", "identityKey"} {
				if strings.Contains(full, frag) {
					t.Fatalf("pairing payload fragment %q leaked in the tool result: %s", frag, full)
				}
			}
		})
	}
}

func TestShowPairingQRReusesValidCodeAndStopsOncePaired(t *testing.T) {
	e := newEnv(t)
	e.fake.SetStatus(wa.Status{State: wa.StateNeedsQR})
	e.fake.SetQRCodesOnConnect("code-1")

	first := callAs[qrOut](e, "show_pairing_qr", map[string]any{})
	second := callAs[qrOut](e, "show_pairing_qr", map[string]any{})
	if first.ExpiresAt != second.ExpiresAt || e.fake.ConnectCalls() != 1 {
		t.Fatalf("valid code must be reused: first=%+v second=%+v connects=%d", first, second, e.fake.ConnectCalls())
	}

	e.clock.Advance(2 * time.Minute)
	e.fake.SetQRCodesOnConnect("code-2")
	third := callAs[qrOut](e, "show_pairing_qr", map[string]any{})
	if third.ExpiresAt == first.ExpiresAt || e.fake.ConnectCalls() != 2 {
		t.Fatalf("expired code must be refreshed: third=%+v connects=%d", third, e.fake.ConnectCalls())
	}

	e.fake.SetStatus(wa.Status{State: wa.StateConnected, AccountJID: selfJID, Paired: true})
	e.fake.Emit(wa.PairedEvent{AccountJID: selfJID})
	if got := callAs[qrOut](e, "show_pairing_qr", map[string]any{}); got.Status != "already_paired" {
		t.Fatalf("after pairing status = %q", got.Status)
	}
	if _, err := os.Stat(e.qrPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("PNG must be removed after pairing, stat err = %v", err)
	}
}
