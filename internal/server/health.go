package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/fmflurry/whatsapp-mcp/internal/daemon"
	"github.com/fmflurry/whatsapp-mcp/internal/securefile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// HealthResult intentionally contains no credentials, account IDs or webhook URL.
type HealthResult struct {
	OK             bool   `json:"ok"`
	Reachable      bool   `json:"reachable"`
	HTTPAddr       string `json:"http_addr"`
	DaemonPID      int    `json:"daemon_pid"`
	State          string `json:"state"`
	Paired         bool   `json:"paired"`
	WebhookFailed  uint64 `json:"webhook_failed"`
	WebhookEnabled bool   `json:"webhook_enabled"`
	Error          string `json:"error"`
}

// Health probes discovery directly. It never starts a daemon or takes a lock.
func Health(ctx context.Context, dir string) HealthResult {
	var out HealthResult
	info, err := daemon.Read(dir)
	if err != nil {
		out.Error = "daemon discovery unavailable or invalid"
		return out
	}
	out.HTTPAddr, out.DaemonPID = info.HTTPAddr, info.PID
	b, err := securefile.Read(info.TokenFile, true)
	if err != nil || strings.TrimSpace(string(b)) == "" {
		out.Error = "daemon token unavailable or invalid"
		return out
	}
	transport := newProxyTransport()
	defer transport.CloseIdleConnections()
	hc := daemonHTTPClient(strings.TrimSpace(string(b)), transport)
	client := mcp.NewClient(&mcp.Implementation{Name: "whatsapp-mcp-health", Version: "1"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: "http://" + info.HTTPAddr + "/mcp", HTTPClient: hc, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		out.Error = "daemon connection failed"
		return out
	}
	defer cs.Close()
	result, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "connection_status"})
	if err != nil {
		out.Error = "daemon status request failed"
		return out
	}
	out.Reachable = true
	if result.IsError {
		out.Error = "daemon status tool failed"
		return out
	}
	var status struct {
		State   string `json:"state"`
		Paired  bool   `json:"paired"`
		Webhook struct {
			Failed  uint64 `json:"failed"`
			Enabled bool   `json:"enabled"`
		} `json:"inbound_webhook"`
	}
	raw, err := json.Marshal(result.StructuredContent)
	if result.StructuredContent == nil {
		err = errors.New("missing status")
		for _, c := range result.Content {
			if t, ok := c.(*mcp.TextContent); ok {
				raw, err = []byte(t.Text), nil
				break
			}
		}
	}
	if err != nil || json.Unmarshal(raw, &status) != nil || status.State == "" {
		out.Error = "invalid daemon status"
		return out
	}
	out.State, out.Paired = status.State, status.Paired
	out.WebhookFailed, out.WebhookEnabled = status.Webhook.Failed, status.Webhook.Enabled
	out.OK = out.Reachable && out.State == "connected" && out.Paired
	if !out.OK {
		out.Error = "WhatsApp is not connected and paired"
	}
	return out
}
