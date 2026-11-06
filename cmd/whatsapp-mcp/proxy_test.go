package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
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
