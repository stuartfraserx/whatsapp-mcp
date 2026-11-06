package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmflurry/whatsapp-mcp/internal/config"
	"github.com/fmflurry/whatsapp-mcp/internal/daemon"
	"github.com/fmflurry/whatsapp-mcp/internal/securefile"
)

var startProxyDaemon = daemon.Start

var newProxyTransport = func() *http.Transport { return &http.Transport{Proxy: nil, ResponseHeaderTimeout: 90 * time.Second} }

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
	discoveryStat os.FileInfo
	toolsMirrored bool
	mu            sync.Mutex
	dataDir       string
	tokenFallback string
	fallback      daemon.Info
	discoveryErr  error
	server        *mcp.Server
	tools         map[string]bool
	lastStart     time.Time
	info          daemon.Info
	session       *mcp.ClientSession
	transport     *http.Transport
	logPath       string
}

// NewProxy mirrors the daemon tools, or exposes useful errors while it is down.
func NewProxy(ctx context.Context, version string, info daemon.Info, tokenFallback, dataDir string) (*mcp.Server, *Proxy) {
	p := &Proxy{dataDir: dataDir, tokenFallback: tokenFallback, fallback: info, tools: make(map[string]bool)}
	p.discover()
	s := mcp.NewServer(&mcp.Implementation{Name: "whatsapp-mcp", Version: version}, nil)
	p.server = s
	listCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var ts []*mcp.Tool
	_, _ = p.recover(listCtx)
	if !p.toolsMirrored {
		for _, name := range []string{"list_chats", "search_contacts", "list_messages", "get_message_context", "download_media", "send_message", "send_file", "edit_message", "delete_message", "connection_status", "show_pairing_qr", "resync_contacts"} {
			ts = append(ts, &mcp.Tool{Name: name, Description: "WhatsApp daemon tool", InputSchema: map[string]any{"type": "object"}})
		}
	}
	for _, t := range ts {
		s.AddTool(t, p.call)
		p.tools[t.Name] = true
	}
	return s, p
}

// discover runs under mu (or during construction). Invalid discovery never falls
// back to a previously trusted endpoint. Missing discovery retains caller defaults.
func (p *Proxy) discover() {
	stat, _ := os.Stat(daemon.Path(p.dataDir))
	changed := (stat == nil) != (p.discoveryStat == nil)
	if stat != nil && p.discoveryStat != nil {
		changed = !os.SameFile(stat, p.discoveryStat) || stat.Size() != p.discoveryStat.Size() || !stat.ModTime().Equal(p.discoveryStat.ModTime())
	}
	p.discoveryStat = stat
	info, err := daemon.Read(p.dataDir)
	if errors.Is(err, os.ErrNotExist) {
		info, err = p.fallback, nil
		info.StartCommand = nil
		if info.HTTPAddr == "" {
			info.HTTPAddr = "127.0.0.1:8765"
		}
	}
	if err == nil {
		err = config.ValidateHTTPAddr(info.HTTPAddr)
	}
	if info.TokenFile == "" {
		info.TokenFile = p.tokenFallback
	}
	if info.TokenFile == "" {
		info.TokenFile = filepath.Join(p.dataDir, "http-token")
	}
	if changed || !reflect.DeepEqual(info, p.info) || (err != nil) != (p.discoveryErr != nil) {
		p.closeSession()
	}
	p.info, p.discoveryErr = info, err
	p.logPath = info.LogPath
	if p.logPath == "" {
		p.logPath = filepath.Join(p.dataDir, "logs", "daemon.log")
	}
}

func proxyTools(ctx context.Context, cs *mcp.ClientSession) ([]*mcp.Tool, error) {
	var ts []*mcp.Tool
	params := &mcp.ListToolsParams{}
	for {
		result, err := cs.ListTools(ctx, params)
		if err != nil {
			return nil, err
		}
		ts = append(ts, result.Tools...)
		if result.NextCursor == "" {
			return ts, nil
		}
		params.Cursor = result.NextCursor
	}
}

func (p *Proxy) refreshTools(ctx context.Context, cs *mcp.ClientSession) {
	ts, err := proxyTools(ctx, cs)
	if err != nil {
		return
	}
	next := make(map[string]bool)
	for _, t := range ts {
		p.server.AddTool(t, p.call)
		next[t.Name] = true
	}
	for name := range p.tools {
		if !next[name] {
			p.server.RemoveTools(name)
		}
	}
	p.tools = next
	p.toolsMirrored = true
}

// recover never forwards a tool call. Every new connection reads the token anew.
func (p *Proxy) recover(ctx context.Context) (*mcp.ClientSession, error) {
	p.discover()
	cs, err := p.connect(ctx)
	if err == nil || ctx.Err() != nil {
		return cs, err
	}
	p.closeSession()
	p.discover()
	cs, err = p.connect(ctx)
	if err == nil || ctx.Err() != nil {
		return cs, err
	}
	if p.discoveryErr != nil || len(p.info.StartCommand) == 0 || time.Since(p.lastStart) < 30*time.Second {
		return nil, err
	}
	p.lastStart = time.Now()
	if startProxyDaemon(p.info.StartCommand) != nil {
		return nil, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		p.discover()
		if cs, err = p.connect(waitCtx); err == nil {
			return cs, nil
		}
		select {
		case <-waitCtx.Done():
			return nil, waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func (p *Proxy) connect(ctx context.Context) (*mcp.ClientSession, error) {
	if p.discoveryErr != nil {
		return nil, p.discoveryErr
	}
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
	httpClient := daemonHTTPClient(token, transport)
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
	if p.server != nil {
		p.refreshTools(connectCtx, cs)
	}
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
		cs, err := p.recover(ctx)
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
		if !isTransportError(err) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return result, err
		}
		p.closeSession()
		if !safeToReplay(err) && (r.Params.Name != "connection_status" || ctx.Err() != nil) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "whatsapp daemon connection lost; the request may or may not have been processed — check before retrying"}}}, nil
		}
		p.discover()
	}
	return p.unavailable(r.Params.Name)
}

func isTransportError(err error) bool {
	var ne net.Error
	return errors.Is(err, mcp.ErrConnectionClosed) || errors.Is(err, mcp.ErrSessionMissing) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) || errors.As(err, &ne)
}

// Ready reports whether upstream initialization succeeded.
func (p *Proxy) Ready() bool { p.mu.Lock(); defer p.mu.Unlock(); return p.session != nil }

func safeToReplay(err error) bool {
	var op *net.OpError
	return errors.Is(err, mcp.ErrSessionMissing) || (errors.As(err, &op) && op.Op == "dial")
}

// daemonHTTPClient keeps both proxy and health credentials on validated loopback.
func daemonHTTPClient(token string, transport *http.Transport) *http.Client {
	return &http.Client{Transport: bearerTransport{token, transport}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
