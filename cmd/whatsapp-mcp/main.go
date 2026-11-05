// Command whatsapp-mcp serves a personal WhatsApp account over MCP.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	restrictUmask()
	// Stray prints from dependencies must never corrupt the stdio protocol.
	stdout := os.Stdout
	os.Stdout = os.Stderr

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdin, stdout, os.Stderr, os.LookupEnv)
	stop()
	os.Exit(code)
}
