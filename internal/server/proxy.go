package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmflurry/whatsapp-mcp/internal/config"
	"github.com/fmflurry/whatsapp-mcp/internal/daemon"
	"github.com/fmflurry/whatsapp-mcp/internal/securefile"
)

var newProxyTransport = func() *http.Transport { return &http.Transport{Proxy: nil, ResponseHeaderTimeout: 10 * time.Second} }

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// Proxy owns an upstream MCP session. The server mirrors upstream tool schemas.
type Proxy struct {
	mu        sync.Mutex
	info      daemon.Info
	session   *mcp.ClientSession
	transport *http.Transport
	logPath   string
}

// NewProxy mirrors the daemon tools, or exposes useful errors while it is down.
func NewProxy(ctx context.Context, version string, info daemon.Info, tokenFallback, dataDir string) (*mcp.Server, *Proxy) {
	if info.HTTPAddr == "" {
		info.HTTPAddr = "127.0.0.1:8765"
	}
	if info.TokenFile == "" {
		info.TokenFile = tokenFallback
	}
	if info.TokenFile == "" {
		info.TokenFile = filepath.Join(dataDir, "http-token")
	}
	p := &Proxy{info: info, logPath: info.LogPath}
	if p.logPath == "" {
		p.logPath = filepath.Join(dataDir, "logs", "daemon.log")
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "whatsapp-mcp", Version: version}, nil)
	listCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var ts []*mcp.Tool
	if cs, err := p.connect(listCtx); err == nil {
		params := &mcp.ListToolsParams{}
		for {
			result, err := cs.ListTools(listCtx, params)
			if err != nil {
				ts = nil
				break
			}
			ts = append(ts, result.Tools...)
			if result.NextCursor == "" {
				break
			}
			params.Cursor = result.NextCursor
		}
	}
	if len(ts) == 0 {
		for _, name := range []string{"list_chats", "search_contacts", "list_messages", "get_message_context", "download_media", "send_message", "send_file", "edit_message", "delete_message", "connection_status", "show_pairing_qr", "resync_contacts"} {
			ts = append(ts, &mcp.Tool{Name: name, Description: "WhatsApp daemon tool", InputSchema: map[string]any{"type": "object"}})
		}
	}
	for _, t := range ts {
		s.AddTool(t, p.call)
	}
	return s, p
}
func (p *Proxy) connect(ctx context.Context) (*mcp.ClientSession, error) {
	if p.session != nil {
		return p.session, nil
	}
	b, err := securefile.Read(p.info.TokenFile, true)
	if err != nil {
		return nil, errors.New("daemon token unavailable")
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return nil, errors.New("daemon token is empty")
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "whatsapp-mcp-proxy", Version: "1"}, nil)
	// No proxy environment or redirects: credentials stay on validated loopback.
	transport := newProxyTransport()
	httpClient := &http.Client{Transport: bearerTransport{token, transport}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if err := config.ValidateHTTPAddr(p.info.HTTPAddr); err != nil {
		return nil, err
	}
	connectCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cs, err := client.Connect(connectCtx, &mcp.StreamableClientTransport{Endpoint: "http://" + p.info.HTTPAddr + "/mcp", HTTPClient: httpClient, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		transport.CloseIdleConnections()
		return nil, err
	}
	p.session = cs
	p.transport = transport
	return cs, nil
}
func (p *Proxy) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeSession()
}

func (p *Proxy) closeSession() {
	if p.session != nil {
		p.session.Close()
		p.session = nil
	}
	if p.transport != nil {
		p.transport.CloseIdleConnections()
		p.transport = nil
	}
}
func (p *Proxy) unavailable(name string) (*mcp.CallToolResult, error) {
	if name == "connection_status" {
		v := map[string]any{"state": "daemon_unavailable", "paired": false}
		b, _ := json.Marshal(v)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}, StructuredContent: v}, nil
	}
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "whatsapp daemon is not running; see " + p.logPath}}}, nil
}
func (p *Proxy) call(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		cs, err := p.connect(ctx)
		if err != nil {
			return p.unavailable(r.Params.Name)
		}
		meta := maps.Clone(r.Params.Meta)
		delete(meta, mcp.MetaKeyProtocolVersion)
		delete(meta, mcp.MetaKeyClientCapabilities)
		delete(meta, mcp.MetaKeyClientInfo)
		result, err := cs.CallTool(ctx, &mcp.CallToolParams{Meta: meta, Name: r.Params.Name, Arguments: r.Params.Arguments, InputResponses: r.Params.InputResponses, RequestState: r.Params.RequestState})
		if err == nil {
			return result, nil
		}
		// JSON-RPC application errors must pass through without retrying a send.
		var rpc *jsonrpc.Error
		if errors.As(err, &rpc) || ctx.Err() != nil {
			return result, err
		}
		if !isTransportError(err) {
			return result, err
		}
		p.closeSession()
	}
	return p.unavailable(r.Params.Name)
}

func isTransportError(err error) bool {
	var ne net.Error
	return errors.Is(err, mcp.ErrConnectionClosed) || errors.Is(err, mcp.ErrSessionMissing) || errors.Is(err, io.EOF) || errors.As(err, &ne)
}

// Ready reports whether upstream initialization succeeded.
func (p *Proxy) Ready() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.session != nil }
