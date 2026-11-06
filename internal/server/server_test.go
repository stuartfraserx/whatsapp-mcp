package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmflurry/whatsapp-mcp/internal/qr"
	"github.com/fmflurry/whatsapp-mcp/internal/ratelimit"
	"github.com/fmflurry/whatsapp-mcp/internal/sendpolicy"
	"github.com/fmflurry/whatsapp-mcp/internal/server"
	"github.com/fmflurry/whatsapp-mcp/internal/session"
	"github.com/fmflurry/whatsapp-mcp/internal/store"
	"github.com/fmflurry/whatsapp-mcp/internal/tools"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
	"github.com/fmflurry/whatsapp-mcp/internal/wa/watest"
)

const (
	testToken     = "s3cret-token-value"
	initializeReq = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"1"}}}`
	waitLimit     = 10 * time.Second
)

func newDeps(t *testing.T) tools.Deps {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "messages.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fake := watest.NewFake()
	fake.SetStatus(wa.Status{State: wa.StateConnected, AccountJID: "33699999999@s.whatsapp.net", Paired: true})
	sess := session.New(session.Options{Client: fake, Store: st, QR: qr.Presenter{PNGPath: filepath.Join(dir, "qr.png")}})
	return tools.Deps{
		Client:      fake,
		Store:       st,
		Session:     sess,
		Policy:      &sendpolicy.Policy{Enabled: true, Limiter: ratelimit.New(time.Second, 10, nil)},
		SendEnabled: true,
		DownloadDir: filepath.Join(dir, "downloads"),
		OutboxDir:   filepath.Join(dir, "outbox"),
	}
}

func newMCP(t *testing.T) *mcp.Server {
	t.Helper()
	return server.NewMCPServer("1.2.3", newDeps(t))
}

type httpServer struct {
	URL  string
	Addr string
	stop func()
}

func startHTTP(t *testing.T, token string) *httpServer {
	t.Helper()
	ln, err := server.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, ln, server.NewHTTPHandler(newMCP(t), token)) }()
	hs := &httpServer{Addr: ln.Addr().String(), URL: "http://" + ln.Addr().String() + "/mcp"}
	var once sync.Once
	hs.stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("Serve returned %v after cancel", err)
				}
			case <-time.After(waitLimit):
				t.Error("Serve did not return after the context was cancelled")
			}
		})
	}
	t.Cleanup(hs.stop)
	return hs
}

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

func connectHTTPClient(t *testing.T, endpoint string, hc *http.Client) (*mcp.ClientSession, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	t.Cleanup(cancel)
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: hc, DisableStandaloneSSE: true}, nil)
	if err == nil {
		t.Cleanup(func() { _ = cs.Close() })
	}
	return cs, err
}

func rawPost(t *testing.T, url, host, auth, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if host != "" {
		req.Host = host
	}
	client := &http.Client{Timeout: waitLimit}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

func TestListenRejectsNonLoopbackAndMalformedAddresses(t *testing.T) {
	tests := []struct {
		name string
		addr string
	}{
		{"wildcard ipv4 with port", "0.0.0.0:8765"},
		{"wildcard ipv4 with ephemeral port", "0.0.0.0:0"},
		{"ipv6 loopback", "[::1]:8765"},
		{"ipv6 loopback ephemeral", "[::1]:0"},
		{"ipv6 wildcard", "[::]:8765"},
		{"localhost name", "localhost:8765"},
		{"localhost name ephemeral", "localhost:0"},
		{"private lan address", "192.168.1.20:8765"},
		{"other loopback address", "127.0.0.2:8765"},
		{"empty host", ":8765"},
		{"public address", "8.8.8.8:8765"},
		{"hostname", "example.com:8765"},
		{"missing port", "127.0.0.1"},
		{"empty", ""},
		{"port out of range", "127.0.0.1:65536"},
		{"non numeric port", "127.0.0.1:http"},
		{"negative port", "127.0.0.1:-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ln, err := server.Listen(tt.addr)
			if err == nil {
				_ = ln.Close()
				t.Fatalf("Listen(%q) succeeded on %s, want an error", tt.addr, ln.Addr())
			}
		})
	}
}

func TestListenAcceptsLoopbackWithEphemeralPort(t *testing.T) {
	ln, err := server.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.Equal(net.IPv4(127, 0, 0, 1)) || addr.Port == 0 {
		t.Fatalf("bound to %v, want 127.0.0.1 with a real port", ln.Addr())
	}
	if ln.Addr().Network() != "tcp" {
		t.Fatalf("network = %q", ln.Addr().Network())
	}
}

func TestListenFailsWhenPortIsTaken(t *testing.T) {
	first, err := server.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	if second, err := server.Listen(first.Addr().String()); err == nil {
		_ = second.Close()
		t.Fatal("binding the same address twice must fail")
	}
}

func TestNewMCPServerAdvertisesIdentityAndTools(t *testing.T) {
	srv := newMCP(t)
	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()
	serverT, clientT := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()

	info := cs.InitializeResult().ServerInfo
	if info.Name != "whatsapp-mcp" || info.Version != "1.2.3" {
		t.Fatalf("server info = %+v", info)
	}
	if instr := cs.InitializeResult().Instructions; !strings.Contains(instr, "explicitly asked") {
		t.Fatalf("instructions = %q, want the send safety guidance", instr)
	}
	list, err := cs.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 12 {
		t.Fatalf("tools = %d (err=%v), want 12", len(list.Tools), err)
	}
}

func TestHTTPWithoutTokenServesMCPClients(t *testing.T) {
	hs := startHTTP(t, "")
	cs, err := connectHTTPClient(t, hs.URL, nil)
	if err != nil {
		t.Fatalf("initialize without a configured token: %v", err)
	}

	list, err := cs.ListTools(context.Background(), nil)
	if err != nil || len(list.Tools) != 12 {
		t.Fatalf("tools = %d (err=%v), want 12", len(list.Tools), err)
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "connection_status", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("connection_status over HTTP: err=%v res=%+v", err, res)
	}
}

func TestHTTPWithoutTokenIgnoresAnyAuthorizationHeader(t *testing.T) {
	hs := startHTTP(t, "")

	resp, _ := rawPost(t, hs.URL, "", "Bearer whatever", initializeReq)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestHTTPTokenRequiresCorrectBearer(t *testing.T) {
	hs := startHTTP(t, testToken)
	tests := []struct {
		name   string
		auth   string
		wantOK bool
	}{
		{"missing header", "", false},
		{"wrong token", "Bearer not-the-token", false},
		{"token prefix", "Bearer " + testToken[:len(testToken)-1], false},
		{"token with suffix", "Bearer " + testToken + "x", false},
		{"empty bearer value", "Bearer ", false},
		{"bare token without scheme", testToken, false},
		{"basic scheme", "Basic " + testToken, false},
		{"token in uppercase", "Bearer " + strings.ToUpper(testToken), false},
		{"correct bearer", "Bearer " + testToken, true},
		{"scheme is case insensitive", "bearer " + testToken, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := rawPost(t, hs.URL, "", tt.auth, initializeReq)

			if tt.wantOK {
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
				}
				return
			}
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if got := resp.Header.Get("WWW-Authenticate"); got != "Bearer" {
				t.Fatalf("WWW-Authenticate = %q, want Bearer", got)
			}
			if strings.Contains(body, testToken) || strings.Contains(body, `"result"`) {
				t.Fatalf("401 body leaks data: %q", body)
			}
		})
	}
}

func TestHTTPTokenProtectsEveryMethod(t *testing.T) {
	hs := startHTTP(t, testToken)

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		req, err := http.NewRequest(method, hs.URL, strings.NewReader(initializeReq))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := (&http.Client{Timeout: waitLimit}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without token: status = %d, want 401", method, resp.StatusCode)
		}
	}
}

func TestHTTPTokenClientWithBearerWorksAndWithoutFails(t *testing.T) {
	hs := startHTTP(t, testToken)

	good := &http.Client{Transport: bearerTransport{token: testToken, base: http.DefaultTransport}}
	cs, err := connectHTTPClient(t, hs.URL, good)
	if err != nil {
		t.Fatalf("authenticated client: %v", err)
	}
	if _, err := cs.ListTools(context.Background(), nil); err != nil {
		t.Fatalf("ListTools: %v", err)
	}

	bad := &http.Client{Transport: bearerTransport{token: "wrong", base: http.DefaultTransport}}
	if cs, err := connectHTTPClient(t, hs.URL, bad); err == nil {
		_ = cs.Close()
		t.Fatal("client with the wrong token connected")
	}
	if cs, err := connectHTTPClient(t, hs.URL, nil); err == nil {
		_ = cs.Close()
		t.Fatal("client without a token connected")
	}
}

func TestHTTPOnlyServesMCPPath(t *testing.T) {
	for _, token := range []string{"", testToken} {
		hs := startHTTP(t, token)
		auth := ""
		if token != "" {
			auth = "Bearer " + token
		}
		for _, path := range []string{"/", "/mcp/", "/mcp/extra", "/MCP", "/sse", "/other", "/.well-known/oauth-authorization-server"} {
			t.Run("token="+token+" "+path, func(t *testing.T) {
				resp, _ := rawPost(t, "http://"+hs.Addr+path, "", auth, initializeReq)
				if resp.StatusCode != http.StatusNotFound {
					t.Fatalf("POST %s: status = %d, want 404", path, resp.StatusCode)
				}
			})
		}
	}
}

func TestHTTPRejectsNonLocalHostHeader(t *testing.T) {
	tests := []struct {
		name string
		host string
	}{
		{"external hostname", "evil.example.com"},
		{"external hostname with port", "evil.example.com:8765"},
		{"private address", "192.168.1.20:8765"},
		{"loopback lookalike subdomain", "127.0.0.1.evil.example.com"},
	}
	for _, token := range []string{"", testToken} {
		hs := startHTTP(t, token)
		auth := ""
		if token != "" {
			auth = "Bearer " + token
		}
		for _, tt := range tests {
			t.Run("token="+token+" "+tt.name, func(t *testing.T) {
				resp, body := rawPost(t, hs.URL, tt.host, auth, initializeReq)
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("Host %q: status = %d, want 403; body: %s", tt.host, resp.StatusCode, body)
				}
			})
		}
	}
}

func TestHTTPAcceptsLoopbackHostHeaders(t *testing.T) {
	hs := startHTTP(t, "")
	_, port, _ := net.SplitHostPort(hs.Addr)

	for _, host := range []string{"127.0.0.1:" + port, "localhost:" + port, "[::1]:" + port} {
		resp, body := rawPost(t, hs.URL, host, "", initializeReq)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("Host %q: status = %d, want 200; body: %s", host, resp.StatusCode, body)
		}
	}
}

func TestServeStopsWhenContextIsCancelledAndReleasesPort(t *testing.T) {
	hs := startHTTP(t, "")
	if _, err := net.DialTimeout("tcp", hs.Addr, waitLimit); err != nil {
		t.Fatalf("server not reachable before shutdown: %v", err)
	}

	hs.stop()

	if c, err := net.DialTimeout("tcp", hs.Addr, time.Second); err == nil {
		_ = c.Close()
		t.Fatal("listener still accepting connections after shutdown")
	}
}

func TestServeHTTPListensShutsDownAndReturnsNilOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.ServeHTTP(ctx, "127.0.0.1:0", server.NewHTTPHandler(newMCP(t), "")) }()

	select {
	case err := <-done:
		t.Fatalf("ServeHTTP returned before cancellation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeHTTP = %v, want nil after a clean shutdown", err)
		}
	case <-time.After(waitLimit):
		t.Fatal("ServeHTTP did not return after the context was cancelled")
	}
}

func TestServeHTTPRejectsInvalidAddressWithoutServing(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:8765", "localhost:8765", "[::1]:8765", "192.168.0.10:8765"} {
		err := server.ServeHTTP(context.Background(), addr, http.NotFoundHandler())
		if err == nil {
			t.Errorf("ServeHTTP(%q) returned nil, want an address error", addr)
		}
	}
}

func TestServeClosesListenerOnReturn(t *testing.T) {
	ln, err := server.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := server.Serve(ctx, ln, http.NotFoundHandler()); err != nil {
		t.Fatalf("Serve = %v", err)
	}
	if _, err := ln.Accept(); err == nil {
		t.Fatal("listener must be closed after Serve returns")
	}
}

type recordingWriteCloser struct {
	io.WriteCloser
	mu  sync.Mutex
	buf bytes.Buffer
}

func (r *recordingWriteCloser) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.buf.Write(p)
	r.mu.Unlock()
	return r.WriteCloser.Write(p)
}

func (r *recordingWriteCloser) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return bytes.Clone(r.buf.Bytes())
}

type stdioHarness struct {
	srvIn    *io.PipeReader
	clientW  *io.PipeWriter
	clientR  *io.PipeReader
	srvOut   *recordingWriteCloser
	done     chan error
	cancel   context.CancelFunc
	shutdown func()
}

func startStdio(t *testing.T) *stdioHarness {
	t.Helper()
	srvIn, clientW := io.Pipe()
	clientR, srvOutW := io.Pipe()
	h := &stdioHarness{
		srvIn:   srvIn,
		clientW: clientW,
		clientR: clientR,
		srvOut:  &recordingWriteCloser{WriteCloser: srvOutW},
		done:    make(chan error, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	srv := newMCP(t)
	go func() { h.done <- server.RunStdio(ctx, srv, srvIn, h.srvOut) }()
	t.Cleanup(func() {
		cancel()
		_ = clientW.Close()
		_ = clientR.Close()
	})
	return h
}

func (h *stdioHarness) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-h.done:
		return err
	case <-time.After(waitLimit):
		t.Fatal("RunStdio did not return")
		return nil
	}
}

func assertEveryLineIsJSONRPC(t *testing.T, raw []byte) int {
	t.Helper()
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	n := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			t.Errorf("blank line on the protocol stream")
			continue
		}
		n++
		var msg map[string]any
		if err := json.Unmarshal(line, &msg); err != nil {
			t.Errorf("stdout line %d is not JSON: %q (%v)", n, line, err)
			continue
		}
		if msg["jsonrpc"] != "2.0" {
			t.Errorf("stdout line %d is not a JSON-RPC 2.0 message: %q", n, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestStdioClientSessionAndEveryOutputLineIsJSONRPC(t *testing.T) {
	h := startStdio(t)
	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.IOTransport{Reader: h.clientR, Writer: h.clientW}, nil)
	if err != nil {
		t.Fatalf("initialize over stdio: %v", err)
	}

	list, err := cs.ListTools(ctx, nil)
	if err != nil || len(list.Tools) != 12 {
		t.Fatalf("tools/list = %d tools (err=%v), want 12", len(list.Tools), err)
	}
	ok, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "connection_status", Arguments: map[string]any{}})
	if err != nil || ok.IsError {
		t.Fatalf("connection_status: err=%v res=%+v", err, ok)
	}
	bad, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"recipient": "x@bad.example", "text": "t"}})
	if err != nil || !bad.IsError {
		t.Fatalf("invalid send: err=%v res=%+v, want a tool error", err, bad)
	}
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "no_such_tool"}); err == nil {
		t.Fatal("unknown tool must produce a protocol error")
	}

	if err := cs.Close(); err != nil {
		t.Logf("client close: %v", err)
	}
	if err := h.wait(t); err != nil {
		t.Fatalf("RunStdio = %v, want nil after the client hangs up", err)
	}
	if n := assertEveryLineIsJSONRPC(t, h.srvOut.Bytes()); n < 5 {
		t.Fatalf("only %d protocol lines were written, expected responses for every request", n)
	}
}

func TestStdioRawJSONRPCLinesProduceOnlyJSONRPCOutput(t *testing.T) {
	h := startStdio(t)
	send := func(line string) {
		t.Helper()
		if _, err := io.WriteString(h.clientW, line+"\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	responses := bufio.NewReader(h.clientR)
	readResponse := func() map[string]any {
		t.Helper()
		type result struct {
			line []byte
			err  error
		}
		ch := make(chan result, 1)
		go func() {
			line, err := responses.ReadBytes('\n')
			ch <- result{line, err}
		}()
		select {
		case r := <-ch:
			if r.err != nil {
				t.Fatalf("read response: %v", r.err)
			}
			var msg map[string]any
			if err := json.Unmarshal(r.line, &msg); err != nil {
				t.Fatalf("response is not JSON: %q (%v)", r.line, err)
			}
			return msg
		case <-time.After(waitLimit):
			t.Fatal("timed out waiting for a response line")
			return nil
		}
	}

	send(initializeReq)
	init := readResponse()
	if init["id"] != float64(1) || init["result"] == nil {
		t.Fatalf("initialize response = %v", init)
	}
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	list := readResponse()
	result, _ := list["result"].(map[string]any)
	toolList, _ := result["tools"].([]any)
	if list["id"] != float64(2) || len(toolList) != 12 {
		t.Fatalf("tools/list response = %v", list)
	}
	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_chats","arguments":{"limit":999}}}`)
	call := readResponse()
	callResult, _ := call["result"].(map[string]any)
	if call["id"] != float64(3) || callResult["isError"] != true {
		t.Fatalf("invalid limit must be a tool error, got %v", call)
	}

	_ = h.clientW.Close()
	if err := h.wait(t); err != nil {
		t.Fatalf("RunStdio = %v, want nil on input EOF", err)
	}
	if n := assertEveryLineIsJSONRPC(t, h.srvOut.Bytes()); n != 3 {
		t.Fatalf("server wrote %d lines, want exactly 3 responses", n)
	}
}

func TestRunStdioReturnsNilWhenContextCancelled(t *testing.T) {
	h := startStdio(t)
	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()
	if _, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).
		Connect(ctx, &mcp.IOTransport{Reader: h.clientR, Writer: h.clientW}, nil); err != nil {
		t.Fatal(err)
	}

	h.cancel()

	if err := h.wait(t); err != nil {
		t.Fatalf("RunStdio = %v, want nil on cancellation", err)
	}
}

func TestRunStdioReturnsNilWhenInputIsAlreadyClosed(t *testing.T) {
	h := startStdio(t)

	_ = h.clientW.Close()

	if err := h.wait(t); err != nil {
		t.Fatalf("RunStdio = %v, want nil on EOF", err)
	}
	if out := h.srvOut.Bytes(); len(out) != 0 {
		t.Fatalf("server wrote %q without receiving any request", out)
	}
}

func TestRunStdioDoesNotLogRegularShutdownAsAnError(t *testing.T) {
	tests := []struct {
		name     string
		shutdown func(h *stdioHarness)
	}{
		{"context cancelled", func(h *stdioHarness) { h.cancel() }},
		{"input closed", func(h *stdioHarness) { _ = h.clientW.Close() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs bytes.Buffer
			deps := newDeps(t)
			deps.Log = slog.New(slog.NewTextHandler(&logs, nil))
			srvIn, clientW := io.Pipe()
			clientR, srvOutW := io.Pipe()
			h := &stdioHarness{clientW: clientW, clientR: clientR, done: make(chan error, 1)}
			ctx, cancel := context.WithCancel(context.Background())
			h.cancel = cancel
			t.Cleanup(func() { cancel(); _ = clientW.Close(); _ = clientR.Close() })
			go func() {
				h.done <- server.RunStdio(ctx, server.NewMCPServer("1", deps), srvIn, srvOutW)
			}()
			cctx, ccancel := context.WithTimeout(context.Background(), waitLimit)
			defer ccancel()
			if _, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).
				Connect(cctx, &mcp.IOTransport{Reader: clientR, Writer: clientW}, nil); err != nil {
				t.Fatal(err)
			}

			tt.shutdown(h)

			if err := h.wait(t); err != nil {
				t.Fatalf("RunStdio = %v, want nil", err)
			}
			if strings.Contains(logs.String(), "level=ERROR") {
				t.Fatalf("regular shutdown logged an error:\n%s", logs.String())
			}
		})
	}
}

func TestShutdownFilterKeepsRealErrors(t *testing.T) {
	var logs bytes.Buffer
	deps := newDeps(t)
	deps.Log = slog.New(slog.NewTextHandler(&logs, nil))
	srv := server.NewMCPServer("1", deps)

	err := srv.Run(context.Background(), &failingTransport{})
	if err == nil {
		t.Fatal("Run with a failing transport must fail")
	}
	if !strings.Contains(logs.String(), "server connect failed") {
		t.Fatalf("real error was filtered out:\n%s", logs.String())
	}
}

type failingTransport struct{}

func (*failingTransport) Connect(context.Context) (mcp.Connection, error) {
	return nil, errors.New("boom")
}
