package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/daemon"
	"github.com/fmflurry/whatsapp-mcp/internal/wa/watest"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestHealth(t *testing.T) {
	for _, state := range []string{"connected", "disconnected"} {
		t.Run(state, func(t *testing.T) {
			dir := t.TempDir()
			token := filepath.Join(dir, "token")
			os.WriteFile(token, []byte("private-test-token"), 0600)
			s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
			s.AddTool(&mcp.Tool{Name: "connection_status", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{StructuredContent: map[string]any{"state": state, "paired": true, "inbound_webhook": map[string]any{"enabled": true, "failed": 3}}}, nil
			})
			l := watest.NewListener()
			srv := &httptest.Server{Listener: l, Config: &http.Server{Handler: NewHTTPHandler(s, "private-test-token")}}
			srv.Start()
			defer srv.Close()
			old := newProxyTransport
			newProxyTransport = func() *http.Transport { return &http.Transport{DialContext: l.DialContext} }
			defer func() { newProxyTransport = old }()
			daemon.Write(dir, daemon.Info{HTTPAddr: "127.0.0.1:8765", TokenFile: token})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			h := Health(ctx, dir)
			if h.OK != (state == "connected") || !h.Reachable || h.State != state || !h.Paired || h.WebhookFailed != 3 || !h.WebhookEnabled || h.DaemonPID != os.Getpid() {
				t.Fatalf("%+v", h)
			}
			os.Chmod(token, 0644)
			if h = Health(ctx, dir); h.Reachable || h.Error == "" {
				t.Fatalf("public token accepted: %+v", h)
			}
		})
	}
}
