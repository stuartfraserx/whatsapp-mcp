package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/config"
	"github.com/fmflurry/whatsapp-mcp/internal/server"
)

func (e env) health(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	dirDefault, _ := e.lookup(config.EnvDataDir)
	if dirDefault == "" {
		home, err := userHomeDir()
		if err != nil {
			fmt.Fprintln(e.stderr, "cannot determine data directory")
			return exitError
		}
		dirDefault = filepath.Join(home, ".local", "share", "whatsapp-mcp")
	}
	dir := fs.String("data-dir", dirDefault, "daemon data directory")
	timeout := fs.Duration("timeout", 10*time.Second, "health request timeout")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return exitOK
		}
		return exitUsage
	}
	if fs.NArg() != 0 || *timeout <= 0 || *dir == "" {
		fmt.Fprintln(e.stderr, "health: expected --data-dir DIR and positive --timeout duration")
		return exitUsage
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	result := server.Health(ctx, *dir)
	if err := json.NewEncoder(e.stdout).Encode(result); err != nil {
		return exitError
	}
	if result.OK {
		return exitOK
	}
	return exitError
}
