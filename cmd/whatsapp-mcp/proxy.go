package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/config"
	"github.com/fmflurry/whatsapp-mcp/internal/daemon"
	"github.com/fmflurry/whatsapp-mcp/internal/instance"
	"github.com/fmflurry/whatsapp-mcp/internal/server"
)

func (e env) proxy(ctx context.Context, cfg config.Config, log *slog.Logger, start bool) error {
	info, err := daemon.Read(cfg.DataDir)
	if err != nil {
		log.Warn("daemon discovery unavailable")
		info = daemon.Info{HTTPAddr: cfg.HTTPAddr}
	}
	if start && err == nil && len(info.StartCommand) > 0 {
		if err := daemon.Start(info.StartCommand); err != nil {
			log.Warn("daemon startup rejected")
		} else {
			waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
		wait:
			for {
				lock, err := instance.Acquire(cfg.DataDir)
				if errors.Is(err, instance.ErrHeld) {
					// Discovery is written after binding the HTTP listener.
					if fresh, err := daemon.Read(cfg.DataDir); err == nil {
						_, p := server.NewProxy(waitCtx, version, fresh, cfg.HTTPTokenFile, cfg.DataDir)
						if p.Ready() {
							p.Close()
							info = fresh
							break wait
						}
						p.Close()
					}
				} else if lock != nil {
					lock.Close()
				}
				select {
				case <-waitCtx.Done():
					break wait
				case <-ticker.C:
				}
			}
		}
	}
	srv, p := server.NewProxy(ctx, version, info, cfg.HTTPTokenFile, cfg.DataDir)
	defer p.Close()
	log.Info("serving MCP daemon proxy on stdio")
	return server.RunStdio(ctx, srv, io.NopCloser(e.stdin), nopWriteCloser{e.stdout})
}
