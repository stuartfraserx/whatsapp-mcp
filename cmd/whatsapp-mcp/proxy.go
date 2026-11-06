package main

import (
	"context"
	"io"
	"log/slog"

	"github.com/fmflurry/whatsapp-mcp/internal/config"
	"github.com/fmflurry/whatsapp-mcp/internal/daemon"
	"github.com/fmflurry/whatsapp-mcp/internal/server"
)

func (e env) proxy(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	info, err := daemon.Read(cfg.DataDir)
	if err != nil {
		log.Warn("daemon discovery unavailable")
		info = daemon.Info{HTTPAddr: cfg.HTTPAddr}
	}
	srv, p := server.NewProxy(ctx, version, info, cfg.HTTPTokenFile, cfg.DataDir)
	defer p.Close()
	log.Info("serving MCP daemon proxy on stdio")
	return server.RunStdio(ctx, srv, io.NopCloser(e.stdin), nopWriteCloser{e.stdout})
}
