// Package server exposes the MCP server over stdio and loopback HTTP.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmflurry/whatsapp-mcp/internal/config"
	"github.com/fmflurry/whatsapp-mcp/internal/tools"
)

const (
	mcpPath           = "/mcp"
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 5 * time.Second
)

const instructions = "Reads and sends messages on the user's personal WhatsApp account. " +
	"Read tools use a local copy of the user's messages. " +
	"Never send a message unless the user explicitly asked for that exact message to that exact recipient."

// NewMCPServer builds the MCP server with all tools registered.
func NewMCPServer(version string, d tools.Deps) *mcp.Server {
	s := mcp.NewServer(
		&mcp.Implementation{Name: "whatsapp-mcp", Version: version},
		&mcp.ServerOptions{Instructions: instructions, Logger: withoutShutdownErrors(d.Log)},
	)
	tools.Register(s, d)
	return s
}

// withoutShutdownErrors wraps l so that the SDK's error records for a
// cancelled run or an input that simply closed are not logged: both are
// regular shutdowns of a stdio server. A nil l stays nil.
func withoutShutdownErrors(l *slog.Logger) *slog.Logger {
	if l == nil {
		return nil
	}
	return slog.New(shutdownFilter{l.Handler()})
}

type shutdownFilter struct{ slog.Handler }

func (f shutdownFilter) Handle(ctx context.Context, r slog.Record) error {
	if r.Level == slog.LevelError && isShutdownRecord(r) {
		return nil
	}
	return f.Handler.Handle(ctx, r)
}

func (f shutdownFilter) WithAttrs(attrs []slog.Attr) slog.Handler {
	return shutdownFilter{f.Handler.WithAttrs(attrs)}
}

func (f shutdownFilter) WithGroup(name string) slog.Handler {
	return shutdownFilter{f.Handler.WithGroup(name)}
}

func isShutdownRecord(r slog.Record) bool {
	if r.Message != "server run cancelled" && r.Message != "server session ended with error" {
		return false
	}
	clean := false
	r.Attrs(func(a slog.Attr) bool {
		if err, ok := a.Value.Any().(error); ok && a.Key == "error" {
			clean = errors.Is(err, context.Canceled) || errors.Is(err, io.EOF)
		}
		return true
	})
	return clean
}

// RunStdio serves s over the given streams until they close or ctx ends. A
// cancelled ctx or a closed input is a clean shutdown.
func RunStdio(ctx context.Context, s *mcp.Server, in io.ReadCloser, out io.WriteCloser) error {
	err := s.Run(ctx, &mcp.IOTransport{Reader: in, Writer: out})
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// NewHTTPHandler returns the streamable HTTP handler for s, mounted at /mcp
// (every other path is 404). DNS-rebinding protection stays enabled. When
// token is non-empty requests must carry "Authorization: Bearer <token>".
func NewHTTPHandler(s *mcp.Server, token string) http.Handler {
	var h http.Handler = mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{},
	)
	if token != "" {
		h = bearerAuth(h, token)
	}
	mux := http.NewServeMux()
	mux.Handle(mcpPath, h)
	return mux
}

func bearerAuth(next http.Handler, token string) http.Handler {
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := sha256.Sum256([]byte(bearerToken(r)))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	scheme, value, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(value)
}

// Listen opens a TCP listener on a validated 127.0.0.1 address. Port 0 is
// accepted so callers can pick a free port.
func Listen(addr string) (net.Listener, error) {
	check := addr
	if host, port, err := net.SplitHostPort(addr); err == nil && port == "0" {
		check = net.JoinHostPort(host, "1")
	}
	if err := config.ValidateHTTPAddr(check); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", addr, err)
	}
	return ln, nil
}

// ServeHTTP listens on addr and serves h until ctx ends, then shuts down
// gracefully.
func ServeHTTP(ctx context.Context, addr string, h http.Handler) error {
	ln, err := Listen(addr)
	if err != nil {
		return err
	}
	return Serve(ctx, ln, h)
}

// Serve serves h on ln until ctx ends, then shuts down gracefully. It always
// closes ln.
func Serve(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: readHeaderTimeout}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		_ = srv.Close()
	}
	<-errCh
	return nil
}
