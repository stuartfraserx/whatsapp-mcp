package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmflurry/whatsapp-mcp/internal/daemon"
	"github.com/fmflurry/whatsapp-mcp/internal/session"
	"github.com/fmflurry/whatsapp-mcp/internal/store"
	"github.com/fmflurry/whatsapp-mcp/internal/tools"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
	"github.com/fmflurry/whatsapp-mcp/internal/wa/watest"
)

func TestProxyMirrorsAndForwardsDaemonTools(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	os.WriteFile(token, []byte("test-token\n"), 0600)
	fake := watest.NewFake()
	fake.SetStatus(wa.Status{State: wa.StateConnected, Paired: true, AccountJID: "123456789@s.whatsapp.net", StreamReplacedCount: 2, LastDisconnectReason: "stream_replaced"})
	st, err := store.Open(filepath.Join(dir, "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sess := session.New(session.Options{Client: fake, Store: st, Log: log})
	upstream := NewMCPServer("test", tools.Deps{Client: fake, Store: st, Session: sess, Log: log})
	listener := watest.NewListener()
	srv := &httptest.Server{Listener: listener, Config: &http.Server{Handler: NewHTTPHandler(upstream, "test-token")}}
	srv.Start()
	defer srv.Close()
	previous := newProxyTransport
	newProxyTransport = func() *http.Transport { return &http.Transport{DialContext: listener.DialContext} }
	defer func() { newProxyTransport = previous }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	proxy, p := NewProxy(ctx, "test", daemon.Info{HTTPAddr: "127.0.0.1:8765", TokenFile: token}, "", dir)
	defer p.Close()
	if !p.Ready() {
		t.Fatal("proxy failed initialization")
	}
	a, b := mcp.NewInMemoryTransports()
	ss, err := proxy.Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 8 {
		t.Fatalf("mirrored tools %d", len(list.Tools))
	}
	result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "connection_status"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result.StructuredContent)
	var status map[string]any
	json.Unmarshal(raw, &status)
	if status["state"] != "connected" || status["stream_replaced_count"] != float64(2) || status["last_disconnect_reason"] != "stream_replaced" || status["inbound_webhook"] == nil {
		t.Fatalf("forwarded status %s", raw)
	}
	// Closing only the upstream session models a daemon restart without changing the endpoint.
	p.session.Close()
	result, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "connection_status"})
	if err != nil || result.IsError {
		t.Fatalf("reconnect: %v %+v", err, result)
	}
}
func TestProxyUnavailableAndTokenPermissions(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	os.WriteFile(token, []byte("secret"), 0644)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, p := NewProxy(ctx, "test", daemon.Info{HTTPAddr: "127.0.0.1:8765", TokenFile: token}, "", dir)
	defer p.Close()
	if p.Ready() {
		t.Fatal("accepted public token")
	}
	result, err := p.call(ctx, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "connection_status"}})
	if err != nil || result.IsError {
		t.Fatal("status unavailable")
	}
	raw, _ := json.Marshal(result.StructuredContent)
	if string(raw) != `{"paired":false,"state":"daemon_unavailable"}` {
		t.Fatalf("status %s", raw)
	}
	result, err = p.call(ctx, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "send_message"}})
	if err != nil || !result.IsError {
		t.Fatal("expected unavailable tool error")
	}
}

// The HTTP servers use loopback addresses routed through net.Pipe so these
// discovery/transport tests also run in environments without TCP privileges.
func proxyTestDaemon(t *testing.T, token, label string, calls *atomic.Int32, applicationError bool) *watest.Listener {
	t.Helper()
	s := mcp.NewServer(&mcp.Implementation{Name: label, Version: "1"}, nil)
	for _, name := range []string{"connection_status", "send_message", label} {
		s.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			calls.Add(1)
			if applicationError {
				return nil, &jsonrpc.Error{Code: -32000, Message: "application failure"}
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: label}}}, nil
		})
	}
	l := watest.NewListener()
	srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: NewHTTPHandler(s, token)}}
	srv.Start()
	t.Cleanup(srv.Close)
	return l
}

func proxyTestRouting(t *testing.T, routes map[string]*watest.Listener) {
	t.Helper()
	old := newProxyTransport
	newProxyTransport = func() *http.Transport {
		return &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if l := routes[addr]; l != nil {
				return l.DialContext(ctx, network, addr)
			}
			return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("daemon down")}
		}}
	}
	t.Cleanup(func() { newProxyTransport = old })
}

func writeProxyDiscovery(t *testing.T, dir, addr, token string) {
	t.Helper()
	if err := daemon.Write(dir, daemon.Info{HTTPAddr: addr, TokenFile: token}); err != nil {
		t.Fatal(err)
	}
}

func callProxyTool(t *testing.T, p *Proxy, name, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := p.call(ctx, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: name}})
	if err != nil {
		t.Fatal(err)
	}
	if r.IsError || len(r.Content) == 0 || r.Content[0].(*mcp.TextContent).Text != want {
		t.Fatalf("%s: %+v", name, r)
	}
}

func TestProxyRediscoversMovedDaemon(t *testing.T) {
	for _, stopA := range []bool{false, true} {
		t.Run(fmt.Sprint("stopA=", stopA), func(t *testing.T) {
			dir := t.TempDir()
			token := filepath.Join(dir, "token")
			if err := os.WriteFile(token, []byte("first"), 0600); err != nil {
				t.Fatal(err)
			}
			var aCalls, bCalls atomic.Int32
			a := proxyTestDaemon(t, "first", "daemon_a", &aCalls, false)
			b := proxyTestDaemon(t, "second", "daemon_b", &bCalls, false)
			proxyTestRouting(t, map[string]*watest.Listener{"127.0.0.1:8765": a, "127.0.0.1:18765": b})
			writeProxyDiscovery(t, dir, "127.0.0.1:8765", token)
			_, p := NewProxy(context.Background(), "test", daemon.Info{}, "", dir)
			defer p.Close()
			callProxyTool(t, p, "connection_status", "daemon_a")
			if stopA {
				a.Close()
			}
			if err := os.WriteFile(token, []byte("second"), 0600); err != nil {
				t.Fatal(err)
			}
			writeProxyDiscovery(t, dir, "127.0.0.1:18765", token)
			callProxyTool(t, p, "connection_status", "daemon_b")
			callProxyTool(t, p, "send_message", "daemon_b")
			if aCalls.Load() != 1 || bCalls.Load() != 2 {
				t.Fatalf("calls A=%d B=%d", aCalls.Load(), bCalls.Load())
			}
			if p.tools["daemon_a"] || !p.tools["daemon_b"] {
				t.Fatalf("tools: %v", p.tools)
			}
		})
	}
}

func TestProxyRotatedTokenOnReconnect(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	os.WriteFile(token, []byte("old"), 0600)
	var calls atomic.Int32
	a := proxyTestDaemon(t, "old", "old_daemon", &calls, false)
	b := proxyTestDaemon(t, "new", "new_daemon", &calls, false)
	routes := map[string]*watest.Listener{"127.0.0.1:8765": a}
	proxyTestRouting(t, routes)
	writeProxyDiscovery(t, dir, "127.0.0.1:8765", token)
	_, p := NewProxy(context.Background(), "test", daemon.Info{}, "", dir)
	defer p.Close()
	// Close the upstream session, leaving discovery untouched.
	p.closeSession()
	routes["127.0.0.1:8765"] = b
	os.WriteFile(token, []byte("new"), 0600)
	callProxyTool(t, p, "send_message", "new_daemon")
}

func TestProxyStarterRateLimit(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "missing")
	if err := daemon.Write(dir, daemon.Info{HTTPAddr: "127.0.0.1:8765", TokenFile: token, StartCommand: []string{"/test/starter"}}); err != nil {
		t.Fatal(err)
	}
	old := startProxyDaemon
	defer func() { startProxyDaemon = old }()
	starts := 0
	startProxyDaemon = func([]string) error { starts++; return errors.New("rejected") }
	_, p := NewProxy(context.Background(), "test", daemon.Info{}, "", dir)
	defer p.Close()
	for i := 0; i < 3; i++ {
		p.call(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "connection_status"}})
	}
	if starts != 1 {
		t.Fatalf("starts=%d", starts)
	}
	p.lastStart = time.Now().Add(-31 * time.Second)
	p.call(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "connection_status"}})
	if starts != 2 {
		t.Fatalf("starts=%d", starts)
	}
}

func TestProxyApplicationErrorNotRetried(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	os.WriteFile(token, []byte("test"), 0600)
	var calls atomic.Int32
	l := proxyTestDaemon(t, "test", "test_tool", &calls, true)
	proxyTestRouting(t, map[string]*watest.Listener{"127.0.0.1:8765": l})
	writeProxyDiscovery(t, dir, "127.0.0.1:8765", token)
	_, p := NewProxy(context.Background(), "test", daemon.Info{}, "", dir)
	defer p.Close()
	_, err := p.call(context.Background(), &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "send_message"}})
	var rpc *jsonrpc.Error
	if !errors.As(err, &rpc) || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestProxyRefreshesFallbackToolsAfterStartup(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	writeProxyDiscovery(t, dir, "127.0.0.1:8765", token)
	srv, p := NewProxy(context.Background(), "test", daemon.Info{}, "", dir)
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, a, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	changed := make(chan struct{}, 1)
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			select {
			case changed <- struct{}{}:
			default:
			}
		},
	})
	cs, err := client.Connect(ctx, b, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	list, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 12 {
		t.Fatalf("fallback tools: %d", len(list.Tools))
	}
	var calls atomic.Int32
	l := proxyTestDaemon(t, "test", "new_tool", &calls, false)
	proxyTestRouting(t, map[string]*watest.Listener{"127.0.0.1:8765": l})
	if err := os.WriteFile(token, []byte("test"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "connection_status"})
	if err != nil || result.IsError {
		t.Fatalf("reconnect: %v %+v", err, result)
	}
	list, err = cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, tool := range list.Tools {
		names[tool.Name] = true
	}
	if len(names) != 3 || !names["new_tool"] || names["list_chats"] {
		t.Fatalf("tools: %v", names)
	}
	select {
	case <-changed:
	case <-ctx.Done():
		t.Fatal("missing tools/list_changed")
	}
}

func TestProxyStarterPublishesNewDiscovery(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	var calls atomic.Int32
	l := proxyTestDaemon(t, "fresh", "started_tool", &calls, false)
	proxyTestRouting(t, map[string]*watest.Listener{"127.0.0.1:18765": l})
	if err := daemon.Write(dir, daemon.Info{HTTPAddr: "127.0.0.1:8765", TokenFile: token, StartCommand: []string{"/test/starter"}}); err != nil {
		t.Fatal(err)
	}
	old := startProxyDaemon
	defer func() { startProxyDaemon = old }()
	starts := 0
	startProxyDaemon = func([]string) error {
		starts++
		if err := os.WriteFile(token, []byte("fresh"), 0600); err != nil {
			return err
		}
		return daemon.Write(dir, daemon.Info{HTTPAddr: "127.0.0.1:18765", TokenFile: token})
	}
	_, p := NewProxy(context.Background(), "test", daemon.Info{}, "", dir)
	defer p.Close()
	callProxyTool(t, p, "send_message", "started_tool")
	if starts != 1 {
		t.Fatalf("starts=%d", starts)
	}
}

func TestProxyAmbiguousCallNeverReplayed(t *testing.T) {
	for _, mode := range []string{"eof", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			token := filepath.Join(dir, "token")
			if err := os.WriteFile(token, []byte("test"), 0600); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
			s.AddTool(&mcp.Tool{Name: "send_message", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				calls.Add(1)
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "sent"}}}, nil
			})
			handler := NewHTTPHandler(s, "test")
			l := watest.NewListener()
			srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(strings.NewReader(string(b)))
				if strings.Contains(string(b), `"tools/call"`) {
					// Execute the real handler, then lose its response after execution.
					recorder := httptest.NewRecorder()
					handler.ServeHTTP(recorder, r)
					if mode == "timeout" {
						<-r.Context().Done()
						return
					}
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						conn.Close()
					}
					return
				}
				handler.ServeHTTP(w, r)
			})}}
			srv.Start()
			defer srv.Close()
			old := newProxyTransport
			newProxyTransport = func() *http.Transport {
				return &http.Transport{DialContext: l.DialContext, ResponseHeaderTimeout: 100 * time.Millisecond}
			}
			defer func() { newProxyTransport = old }()
			writeProxyDiscovery(t, dir, "127.0.0.1:8765", token)
			_, p := NewProxy(context.Background(), "test", daemon.Info{}, "", dir)
			defer p.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			result, err := p.call(ctx, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "send_message"}})
			if err != nil || result == nil || !result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "check before retrying") {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if calls.Load() != 1 {
				t.Fatalf("calls=%d", calls.Load())
			}
			if p.Ready() {
				t.Fatal("ambiguous session retained")
			}
		})
	}
}

func TestProxyDialFailureRediscoversBeforeReplay(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	os.WriteFile(token, []byte("test"), 0600)
	var calls atomic.Int32
	l := proxyTestDaemon(t, "test", "new_daemon", &calls, false)
	var fail atomic.Bool
	old := newProxyTransport
	newProxyTransport = func() *http.Transport {
		return &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if fail.CompareAndSwap(true, false) {
				writeProxyDiscovery(t, dir, "127.0.0.1:18765", token)
				return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
			}
			return l.DialContext(ctx, network, addr)
		}}
	}
	defer func() { newProxyTransport = old }()
	writeProxyDiscovery(t, dir, "127.0.0.1:8765", token)
	_, p := NewProxy(context.Background(), "test", daemon.Info{}, "", dir)
	defer p.Close()
	fail.Store(true)
	callProxyTool(t, p, "send_message", "new_daemon")
	if calls.Load() != 1 || p.info.HTTPAddr != "127.0.0.1:18765" {
		t.Fatalf("calls=%d addr=%s", calls.Load(), p.info.HTTPAddr)
	}
}

func TestProxySendHeaderDeadline(t *testing.T) {
	transport := newProxyTransport()
	defer transport.CloseIdleConnections()
	if transport.ResponseHeaderTimeout != 90*time.Second {
		t.Fatalf("header timeout=%v", transport.ResponseHeaderTimeout)
	}
}
