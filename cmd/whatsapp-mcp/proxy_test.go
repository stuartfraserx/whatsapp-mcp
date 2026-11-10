package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/daemon"
	"github.com/fmflurry/whatsapp-mcp/internal/instance"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestStdioLockHeldSelectsProxyWithoutOpeningSession(t *testing.T) {
	env, dir := baseEnv(t)
	os.MkdirAll(dir, 0700)
	lock, err := instance.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	// A private discovery record with an unavailable token fails before any HTTP.
	if err := daemon.Write(dir, daemon.Info{HTTPAddr: "127.0.0.1:8765", TokenFile: filepath.Join(dir, "missing-token")}); err != nil {
		t.Fatal(err)
	}
	tc := newTestClient(true)
	h := startStdio(t, env, tc)
	result, err := h.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "connection_status"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(result.StructuredContent)
	var status map[string]any
	json.Unmarshal(raw, &status)
	if status["state"] != "daemon_unavailable" {
		t.Fatalf("status %s", raw)
	}
	if tc.ConnectCalls() != 0 {
		t.Fatal("proxy connected WhatsApp")
	}
	for _, name := range []string{"whatsapp.db", "messages.db", "notifier.db"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("proxy opened %s: %v", name, err)
		}
	}
	h.cancel()
	select {
	case code := <-h.done:
		if code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, h.stderr.String())
		}
	case <-time.After(testDeadline):
		t.Fatal("proxy did not shut down")
	}
	h.assertStdoutIsOnlyJSONRPC(t)
}
func TestStaleDaemonMetadataNeverFallsBackToStandalone(t *testing.T) {
	env, dir := baseEnv(t)
	os.MkdirAll(dir, 0700)
	if err := daemon.Write(dir, daemon.Info{HTTPAddr: "127.0.0.1:8765", TokenFile: filepath.Join(dir, "missing")}); err != nil {
		t.Fatal(err)
	}
	setVar(t, &openClient, func(context.Context, string, *slog.Logger) (clientCloser, error) {
		t.Error("opened WhatsApp with stale daemon discovery")
		return nil, errors.New("must not open")
	})
	// EOF is enough to exercise mode selection without starting a WhatsApp client.
	got := runCLI(t, env, "serve")
	if got.code != 0 {
		t.Fatalf("result %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "whatsapp.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("standalone fallback opened session")
	}
}
func TestStatusDoesNotCreateStateOrTakeLock(t *testing.T) {
	env, dir := baseEnv(t)
	// Use the real offline status reader, never a network client.
	got := runCLI(t, env, "status")
	if got.code != 0 {
		t.Fatalf("status %+v", got)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("status created data dir: %v", err)
	}
}

func TestStdioRemoteUpstream(t *testing.T) {
	values, dir := baseEnv(t)
	upstreamServer := mcp.NewServer(&mcp.Implementation{Name: "remote", Version: "1"}, nil)
	upstreamServer.AddTool(&mcp.Tool{Name: "remote_tool", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "remote response"}}}, nil
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("TCP unavailable: %v", err)
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return upstreamServer }, nil)
	var authorized atomic.Int32
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer remote-secret" {
			http.Error(w, "unauthorized", 401)
			return
		}
		authorized.Add(1)
		handler.ServeHTTP(w, r)
	}))
	ts.Listener.Close()
	ts.Listener = listener
	ts.Start()
	defer ts.Close()
	os.MkdirAll(dir, 0700)
	token := filepath.Join(dir, "remote-token")
	os.WriteFile(token, []byte("remote-secret\n"), 0400)
	b, _ := json.Marshal(map[string]string{"url": ts.URL + "/mcp", "token_file": token})
	os.WriteFile(filepath.Join(dir, "upstream.json"), b, 0600)
	tc := newTestClient(true)
	h := startStdio(t, values, tc)
	result, err := h.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "remote_tool"})
	if err != nil || result.IsError {
		t.Fatalf("remote call: %v, %v", result, err)
	}
	if result.Content[0].(*mcp.TextContent).Text != "remote response" || authorized.Load() == 0 {
		t.Fatal("call not forwarded")
	}
	for _, name := range []string{"instance.lock", "daemon.json", "whatsapp.db", "messages.db", "notifier.db"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("created %s: %v", name, err)
		}
	}
	lock, err := instance.Acquire(dir)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	if tc.ConnectCalls() != 0 {
		t.Fatal("opened WhatsApp")
	}
	h.session.Close()
	if code := waitExit(t, h.done); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if strings.Contains(h.stderr.String(), "remote-secret") {
		t.Fatal("token leaked")
	}
}

func TestStdioUpstreamUnavailableBypassesLocalState(t *testing.T) {
	values, dir := baseEnv(t)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "token")
	os.WriteFile(token, []byte("secret"), 0600)
	values["WHATSAPP_MCP_UPSTREAM_URL"] = "https://127.0.0.1:1/mcp"
	values["WHATSAPP_MCP_UPSTREAM_TOKEN_FILE"] = token
	// A directory in place of the lock makes any attempted acquisition fail.
	os.Mkdir(filepath.Join(dir, "instance.lock"), 0700)
	tc := newTestClient(true)
	h := startStdio(t, values, tc)
	result, err := h.session.CallTool(context.Background(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{}})
	if err != nil || !result.IsError {
		t.Fatalf("unavailable: %v %v", result, err)
	}
	if tc.ConnectCalls() != 0 {
		t.Fatal("connected WhatsApp")
	}
	for _, name := range []string{"daemon.json", "whatsapp.db", "messages.db", "notifier.db"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("created %s: %v", name, err)
		}
	}
	h.session.Close()
	if code := waitExit(t, h.done); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
}
