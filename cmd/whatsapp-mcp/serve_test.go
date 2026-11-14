package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"github.com/fmflurry/whatsapp-mcp/internal/qr"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmflurry/whatsapp-mcp/internal/config"
)

var readTools = []string{
	"list_chats", "search_contacts", "list_messages", "get_message_context",
	"download_media", "connection_status", "show_pairing_qr", "resync_contacts", "status_views",
}

var sendTools = []string{"send_message", "send_file", "post_status", "edit_message", "delete_message"}

type stdioHarness struct {
	session *mcp.ClientSession
	stdout  *lineValidator
	stderr  *syncBuffer
	done    chan int
	cancel  context.CancelFunc
}

func startStdio(t *testing.T, env map[string]string, tc *testClient, args ...string) *stdioHarness {
	t.Helper()
	useClient(t, tc)
	stdinR, stdinW := io.Pipe()
	outR, outW := io.Pipe()
	h := &stdioHarness{
		stdout: &lineValidator{dst: outW},
		stderr: &syncBuffer{},
		done:   make(chan int, 1),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*testDeadline)
	h.cancel = cancel
	t.Cleanup(func() {
		cancel()
		_ = stdinW.Close()
		_ = outR.Close()
	})
	go func() {
		code := run(ctx, append([]string{"serve"}, args...), stdinR, h.stdout, h.stderr, envLookup(env))
		_ = outW.Close()
		h.done <- code
	}()

	client := mcp.NewClient(&mcp.Implementation{Name: "cli-test", Version: "0"}, nil)
	cs, err := client.Connect(ctx, &mcp.IOTransport{Reader: outR, Writer: stdinW}, nil)
	if err != nil {
		t.Fatalf("connect: %v\nstderr:\n%s", err, h.stderr.String())
	}
	h.session = cs
	return h
}

func (h *stdioHarness) toolNames(t *testing.T) []string {
	t.Helper()
	res, err := h.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

func (h *stdioHarness) assertStdoutIsOnlyJSONRPC(t *testing.T) {
	t.Helper()
	lines, invalid, partial := h.stdout.snapshot()
	if len(lines) == 0 {
		t.Fatal("server wrote nothing to stdout")
	}
	if len(invalid) > 0 {
		t.Fatalf("non JSON-RPC lines on stdout: %q", invalid)
	}
	if partial != "" {
		t.Fatalf("unterminated data on stdout: %q", partial)
	}
}

func sorted(groups ...[]string) []string {
	var all []string
	for _, g := range groups {
		all = append(all, g...)
	}
	slices.Sort(all)
	return all
}

func TestServeStdioEndToEnd(t *testing.T) {
	env, _ := baseEnv(t)
	tc := newTestClient(true)
	h := startStdio(t, env, tc)

	if got, want := h.toolNames(t), sorted(readTools, sendTools); !slices.Equal(got, want) {
		t.Fatalf("tools = %v, want %v", got, want)
	}
	if len(readTools)+len(sendTools) != 14 {
		t.Fatalf("test table drifted from the fourteen documented tools")
	}

	res, err := h.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "connection_status"})
	if err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	if res.IsError {
		t.Fatalf("connection_status returned an error: %+v", res.Content)
	}
	out, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content = %#v", res.StructuredContent)
	}
	if out["paired"] != true || out["send_enabled"] != true {
		t.Errorf("connection_status = %v", out)
	}

	waitFor(t, "serve to connect the client", func() bool { return tc.ConnectCalls() >= 1 })

	if err := h.session.Close(); err != nil {
		t.Logf("client close: %v", err)
	}
	if code := waitExit(t, h.done); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, h.stderr.String())
	}
	h.assertStdoutIsOnlyJSONRPC(t)
	if tc.disconnectCalls.Load() != 1 || tc.closeCalls.Load() != 1 {
		t.Fatalf("disconnect=%d close=%d, want 1/1", tc.disconnectCalls.Load(), tc.closeCalls.Load())
	}
	if !strings.Contains(h.stderr.String(), "serving MCP on stdio") {
		t.Errorf("logs should go to stderr, got:\n%s", h.stderr.String())
	}
}

func TestServeStdioKeepsLogsOffStdoutWhenUnpaired(t *testing.T) {
	env, _ := baseEnv(t)
	env[config.EnvLogLevel] = "debug"
	tc := newTestClient(false)
	h := startStdio(t, env, tc)

	h.toolNames(t)
	_ = h.session.Close()

	if code := waitExit(t, h.done); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, h.stderr.String())
	}
	h.assertStdoutIsOnlyJSONRPC(t)
	if !strings.Contains(h.stderr.String(), "WhatsApp not paired") {
		t.Errorf("stderr = %q", h.stderr.String())
	}
	if strings.Contains(h.stderr.String(), testPhone) {
		t.Errorf("phone number leaked to stderr: %q", h.stderr.String())
	}
}

func TestServeStdioHidesSendToolsWhenSendingDisabled(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		args []string
	}{
		{"env", map[string]string{config.EnvSendEnabled: "false"}, nil},
		{"flag", nil, []string{"--send-enabled=false"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env, _ := baseEnv(t)
			for k, v := range c.env {
				env[k] = v
			}
			tc := newTestClient(true)
			h := startStdio(t, env, tc, c.args...)

			names := h.toolNames(t)

			if want := sorted(readTools); !slices.Equal(names, want) {
				t.Fatalf("tools = %v, want %v", names, want)
			}
			res, err := h.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"to": "x", "text": "hi"}})
			if err == nil && !res.IsError {
				t.Fatal("send_message must not be callable when sending is disabled")
			}
			if n := len(tc.Sent()); n != 0 {
				t.Fatalf("%d messages were sent", n)
			}
			_ = h.session.Close()
			if code := waitExit(t, h.done); code != 0 {
				t.Fatalf("exit = %d", code)
			}
			h.assertStdoutIsOnlyJSONRPC(t)
		})
	}
}

func TestServeStdioStopsOnContextCancel(t *testing.T) {
	env, _ := baseEnv(t)
	tc := newTestClient(true)
	h := startStdio(t, env, tc)
	h.toolNames(t)

	h.cancel()

	if code := waitExit(t, h.done); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, h.stderr.String())
	}
	if tc.disconnectCalls.Load() != 1 || tc.closeCalls.Load() != 1 {
		t.Fatalf("disconnect=%d close=%d, want 1/1", tc.disconnectCalls.Load(), tc.closeCalls.Load())
	}
}

func TestServeDefaultsToStdioWithoutSubcommand(t *testing.T) {
	env, _ := baseEnv(t)
	useClient(t, newTestClient(true))

	stdinR, stdinW := io.Pipe()
	var stdout, stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(context.Background(), nil, stdinR, &stdout, &stderr, envLookup(env))
	}()

	waitFor(t, "stdio log line", func() bool { return strings.Contains(stderr.String(), "serving MCP on stdio") })
	_ = stdinW.Close()

	if code := waitExit(t, done); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout must stay empty without protocol traffic, got %q", stdout.String())
	}
}

var listeningRe = regexp.MustCompile(`http://(127\.0\.0\.1:\d+)/mcp`)

type headerTransport struct{ header, value string }

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(h.header, h.value)
	return http.DefaultTransport.RoundTrip(r)
}

func TestServeHTTPRequiresBearerTokenAndShutsDownOnCancel(t *testing.T) {
	const token = "s3cret-token-for-test"
	env, _ := baseEnv(t)
	env[config.EnvHTTPToken] = token
	tc := newTestClient(true)
	useClient(t, tc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"serve", "--transport", "http", "--http-addr", freeLoopbackAddr(t)}, strings.NewReader(""), &stdout, &stderr, envLookup(env))
	}()

	var addr string
	waitFor(t, "http listener address in logs", func() bool {
		m := listeningRe.FindStringSubmatch(stderr.String())
		if m == nil {
			return false
		}
		addr = m[1]
		return true
	})
	endpoint := "http://" + addr + "/mcp"

	resp, err := http.Post(endpoint, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("unauthenticated request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", resp.StatusCode)
	}

	bad := mcp.NewClient(&mcp.Implementation{Name: "bad", Version: "0"}, nil)
	if cs, err := bad.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: headerTransport{"Authorization", "Bearer wrong"}},
		MaxRetries: -1,
	}, nil); err == nil {
		_ = cs.Close()
		t.Fatal("a wrong bearer token must be rejected")
	}

	good := mcp.NewClient(&mcp.Implementation{Name: "good", Version: "0"}, nil)
	cs, err := good.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           &http.Client{Transport: headerTransport{"Authorization", "Bearer " + token}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}, nil)
	if err != nil {
		t.Fatalf("authorized connect: %v\nstderr:\n%s", err, stderr.String())
	}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	if len(tools.Tools) != 14 {
		t.Fatalf("got %d tools, want 14", len(tools.Tools))
	}
	_ = cs.Close()

	cancel()

	if code := waitExit(t, done); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("http mode must not write to stdout, got %q", stdout.String())
	}
	if strings.Contains(stderr.String(), token) {
		t.Errorf("bearer token leaked to logs:\n%s", stderr.String())
	}
	if tc.disconnectCalls.Load() != 1 || tc.closeCalls.Load() != 1 {
		t.Fatalf("disconnect=%d close=%d, want 1/1", tc.disconnectCalls.Load(), tc.closeCalls.Load())
	}
}

type httpRun struct {
	stderr *syncBuffer
	addr   string
	done   chan int
	cancel context.CancelFunc
}

func startHTTPServe(t *testing.T, env map[string]string, extra ...string) *httpRun {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &httpRun{stderr: &syncBuffer{}, done: make(chan int, 1), cancel: cancel}
	args := append([]string{"serve", "--transport", "http", "--http-addr", freeLoopbackAddr(t)}, extra...)
	go func() {
		r.done <- run(ctx, args, strings.NewReader(""), io.Discard, r.stderr, envLookup(env))
	}()
	waitFor(t, "http listener address in logs", func() bool {
		m := listeningRe.FindStringSubmatch(r.stderr.String())
		if m == nil {
			return false
		}
		r.addr = m[1]
		return true
	})
	return r
}

func (r *httpRun) status(t *testing.T, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://"+r.addr+"/mcp", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func (r *httpRun) stop(t *testing.T) {
	t.Helper()
	r.cancel()
	if code := waitExit(t, r.done); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, r.stderr.String())
	}
}

func TestServeHTTPGeneratesAndReusesTokenFile(t *testing.T) {
	env, dataDir := baseEnv(t)
	useClient(t, newTestClient(true))
	tokenPath := filepath.Join(dataDir, "http-token")

	first := startHTTPServe(t, env)
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatalf("token file not written: %v", err)
	}
	token := strings.TrimSpace(string(data))
	if len(token) != 64 {
		t.Fatalf("token = %q, want 64 hex characters", token)
	}
	if _, err := hex.DecodeString(token); err != nil {
		t.Fatalf("token is not hex: %v", err)
	}
	if info, err := os.Stat(tokenPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, err = %v, want 0600", info.Mode().Perm(), err)
	}
	if got := first.status(t, ""); got != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", got)
	}
	if got := first.status(t, "wrong"); got != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", got)
	}
	if got := first.status(t, token); got == http.StatusUnauthorized {
		t.Fatal("the generated token must be accepted")
	}
	first.stop(t)
	logs := first.stderr.String()
	if strings.Contains(logs, token) {
		t.Errorf("token leaked to logs:\n%s", logs)
	}
	if !strings.Contains(logs, "HTTP bearer token stored in "+tokenPath) {
		t.Errorf("logs lack the token location:\n%s", logs)
	}

	second := startHTTPServe(t, env)
	if got := second.status(t, token); got == http.StatusUnauthorized {
		t.Fatal("the persisted token must be reused on restart")
	}
	second.stop(t)
	if again, _ := os.ReadFile(tokenPath); string(again) != string(data) {
		t.Fatal("token file was rewritten on restart")
	}
}

func TestServeHTTPExplicitTokenDoesNotCreateTokenFile(t *testing.T) {
	env, dataDir := baseEnv(t)
	env[config.EnvHTTPToken] = "explicit-token"
	useClient(t, newTestClient(true))

	r := startHTTPServe(t, env)
	if got := r.status(t, "explicit-token"); got == http.StatusUnauthorized {
		t.Fatal("explicit token must be accepted")
	}
	r.stop(t)

	if _, err := os.Lstat(filepath.Join(dataDir, "http-token")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("token file must not be created, err = %v", err)
	}
}

func TestServeHTTPNoAuthRejected(t *testing.T) {
	env, _ := baseEnv(t)
	got := runCLI(t, env, "serve", "--transport", "http", "--http-no-auth")
	if got.code == 0 || !strings.Contains(got.stderr, "requires bearer") {
		t.Fatal("HTTP must require authentication")
	}
}

func TestNonLoopbackRequiresTokenWithoutGeneratingFile(t *testing.T) {
	env, dir := baseEnv(t)
	rec := useClient(t, newTestClient(false))
	got := runCLI(t, env, "serve", "--transport", "http", "--http-addr", "0.0.0.0:8080")
	if got.code == 0 || !strings.Contains(got.stderr, "requires WHATSAPP_MCP_HTTP_TOKEN") {
		t.Fatal("remote listener must require token")
	}
	if rec.calls.Load() != 0 {
		t.Fatal("must refuse before opening WhatsApp")
	}
	if _, err := os.Lstat(filepath.Join(dir, "http-token")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("must not generate remote token")
	}
}

func TestServeHTTPRejectsUnsafeTokenFile(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, path string)
		want    string
	}{
		{"group readable", func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("tok\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatal(err)
			}
		}, "must be 0600"},
		{"directory", func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}, "regular file"},
		{"symlink", func(t *testing.T, path string) {
			target := filepath.Join(t.TempDir(), "real")
			if err := os.WriteFile(target, []byte("tok\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		}, "regular file"},
		{"empty", func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}, "empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, dataDir := baseEnv(t)
			tc := newTestClient(true)
			useClient(t, tc)
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				t.Fatal(err)
			}
			tt.prepare(t, filepath.Join(dataDir, "http-token"))

			got := runCLI(t, env, "serve", "--transport", "http", "--http-addr", freeLoopbackAddr(t))

			if got.code != exitError || !strings.Contains(got.stderr, tt.want) {
				t.Fatalf("got %+v, want exit 1 mentioning %q", got, tt.want)
			}
			if tc.ConnectCalls() != 0 {
				t.Fatal("serve must not connect to WhatsApp with an unusable token file")
			}
		})
	}
}

func TestServeHTTPListenFailureIsRuntimeError(t *testing.T) {
	env, _ := baseEnv(t)
	tc := newTestClient(true)
	useClient(t, tc)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	got := runCLI(t, env, "serve", "--transport", "http", "--http-addr", ln.Addr().String())

	if got.code != 1 || !strings.Contains(got.stderr, "all in use") {
		t.Fatalf("got %+v", got)
	}
	if tc.closeCalls.Load() != 0 || tc.disconnectCalls.Load() != 0 || tc.ConnectCalls() != 0 {
		t.Fatalf("resources must not be opened after a listen failure, close=%d", tc.closeCalls.Load())
	}
}

func TestServeStdioSendFileRefusesDataDirFiles(t *testing.T) {
	env, dataDir := baseEnv(t)
	env[config.EnvSendEnabled] = "true"
	outbox := filepath.Join(dataDir, "outbox")
	env[config.EnvOutboxDir] = outbox
	if err := os.MkdirAll(outbox, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dataDir, "whatsapp.db")
	allowed := filepath.Join(outbox, "note.txt")
	for _, f := range []string{secret, allowed} {
		if err := os.WriteFile(f, []byte("content"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tc := newTestClient(true)
	h := startStdio(t, env, tc)
	send := func(path string) *mcp.CallToolResult {
		t.Helper()
		res, err := h.session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "send_file",
			Arguments: map[string]any{"recipient": "+" + testPhone, "path": path},
		})
		if err != nil {
			t.Fatalf("send_file: %v", err)
		}
		return res
	}

	if res := send(secret); !res.IsError {
		t.Fatalf("send_file must refuse %s, got %+v", secret, res)
	}
	if n := len(tc.Sent()); n != 0 {
		t.Fatalf("%d files were sent after the refusal", n)
	}
	if res := send(allowed); res.IsError {
		t.Fatalf("send_file of an outbox file failed: %+v", res.Content)
	}
	if n := len(tc.Sent()); n != 1 {
		t.Fatalf("sent = %d, want 1", n)
	}
	_ = h.session.Close()
	if code := waitExit(t, h.done); code != 0 {
		t.Fatalf("exit = %d, stderr:\n%s", code, h.stderr.String())
	}
}

func TestServeLogsQRWithoutTerminalWhenEnabled(t *testing.T) {
	env, _ := baseEnv(t)
	env[config.EnvHTTPToken] = "test-token"
	env["WHATSAPP_MCP_LOG_QR"] = "1"
	tc := newTestClient(false)
	tc.SetQRCodesOnConnect("2@test-log-qr")
	useClient(t, tc)
	r := startHTTPServe(t, env)
	var expected bytes.Buffer
	if err := qr.RenderTerminal(&expected, "2@test-log-qr"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(r.stderr.String(), expected.String()) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	r.stop(t)
	if !strings.Contains(r.stderr.String(), expected.String()) {
		t.Fatal("QR block missing from non-terminal stderr")
	}
	if strings.Contains(r.stderr.String(), "2@test-log-qr") {
		t.Fatal("raw QR must not be logged")
	}
}
