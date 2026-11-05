package wameow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

func TestOpenEnablesReconnectOnInitialConnectFailure(t *testing.T) {
	c, err := Open(context.Background(), filepath.Join(t.TempDir(), "whatsapp.db"), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if !c.cli.InitialAutoReconnect || !c.cli.EnableAutoReconnect {
		t.Fatalf("InitialAutoReconnect=%v EnableAutoReconnect=%v, want both true",
			c.cli.InitialAutoReconnect, c.cli.EnableAutoReconnect)
	}
}

func TestQRStartAction(t *testing.T) {
	tests := []struct {
		name              string
		connected, qrFlow bool
		want              qrAction
	}{
		{"idle client starts the flow", false, false, qrStartFlow},
		{"stale qr flag without connection starts the flow", false, true, qrStartFlow},
		{"connected with a running flow keeps it", true, true, qrKeepFlow},
		{"connected without a flow must reconnect first", true, false, qrReconnect},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := qrStartAction(tt.connected, tt.qrFlow); got != tt.want {
				t.Fatalf("qrStartAction(%v, %v) = %v, want %v", tt.connected, tt.qrFlow, got, tt.want)
			}
		})
	}
}

func TestSendFailureMarksOnlyPreSendErrorsAsNotSent(t *testing.T) {
	tests := []struct {
		err     error
		notSent bool
	}{
		{whatsmeow.ErrNotLoggedIn, true},
		{whatsmeow.ErrNotConnected, true},
		{whatsmeow.ErrRecipientADJID, true},
		{fmt.Errorf("%w example.org", whatsmeow.ErrUnknownServer), true},
		{whatsmeow.ErrMessageTimedOut, false},
		{whatsmeow.ErrIQTimedOut, false},
		{context.DeadlineExceeded, false},
		{fmt.Errorf("%w 479", whatsmeow.ErrServerReturnedError), false},
	}
	for _, tt := range tests {
		got := errors.Is(scrubbed("send message", sendFailure(tt.err)), wa.ErrNotSent)
		if got != tt.notSent {
			t.Errorf("%v: ErrNotSent = %v, want %v", tt.err, got, tt.notSent)
		}
	}
}
