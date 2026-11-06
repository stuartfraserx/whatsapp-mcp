package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	result, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "list_chats"})
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
