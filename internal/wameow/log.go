package wameow

import (
	"context"
	"fmt"
	"log/slog"

	waLog "go.mau.fi/whatsmeow/util/log"

	"github.com/fmflurry/whatsapp-mcp/internal/redact"
)

type slogLogger struct {
	log    *slog.Logger
	module string
}

func newWALogger(log *slog.Logger) waLog.Logger {
	return &slogLogger{log: log}
}

func (l *slogLogger) emit(level slog.Level, msg string, args []any) {
	ctx := context.Background()
	if !l.log.Enabled(ctx, level) {
		return
	}
	text := redact.Scrub(fmt.Sprintf(msg, args...))
	if l.module != "" {
		l.log.Log(ctx, level, text, "module", l.module)
		return
	}
	l.log.Log(ctx, level, text)
}

func (l *slogLogger) Debugf(msg string, args ...any) { l.emit(slog.LevelDebug, msg, args) }

// Infof is routed to debug: whatsmeow info lines routinely carry JIDs.
func (l *slogLogger) Infof(msg string, args ...any) { l.emit(slog.LevelDebug, msg, args) }

func (l *slogLogger) Warnf(msg string, args ...any)  { l.emit(slog.LevelWarn, msg, args) }
func (l *slogLogger) Errorf(msg string, args ...any) { l.emit(slog.LevelError, msg, args) }

func (l *slogLogger) Sub(module string) waLog.Logger {
	name := module
	if l.module != "" {
		name = l.module + "/" + module
	}
	return &slogLogger{log: l.log, module: name}
}
