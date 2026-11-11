package wameow

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"

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

func TestPostStatusUploadsAndTargetsStatusBroadcast(t *testing.T) {
	for _, mime := range []string{"image/jpeg", "image/png", "image/webp", "video/mp4"} {
		t.Run(mime, func(t *testing.T) {
			file := wa.OutgoingFile{Data: []byte("media"), MimeType: mime, Caption: "story"}
			var uploadedKind wa.MediaType
			c := &Client{connectedHook: func() bool { return true }, idHook: func() string { return "story-id" }}
			c.uploadHook = func(_ context.Context, data []byte, kind wa.MediaType) (uploaded, error) {
				if string(data) != "media" {
					t.Errorf("data=%q", data)
				}
				uploadedKind = kind
				return uploaded{URL: "uploaded-url", FileLength: uint64(len(data))}, nil
			}
			c.sendHook = func(_ context.Context, to types.JID, msg *waE2E.Message, _ whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
				if to != types.StatusBroadcastJID {
					t.Errorf("target=%s", to)
				}
				if mime == "video/mp4" {
					if msg.GetVideoMessage().GetCaption() != "story" || msg.GetVideoMessage().GetMimetype() != mime {
						t.Error("incorrect video")
					}
				} else if msg.GetImageMessage().GetCaption() != "story" || msg.GetImageMessage().GetMimetype() != mime || msg.GetImageMessage().GetURL() != "uploaded-url" {
					t.Error("incorrect image")
				}
				return whatsmeow.SendResponse{ID: "story-id"}, nil
			}
			sent, err := c.PostStatus(context.Background(), file)
			if err != nil || sent.ChatJID != types.StatusBroadcastJID.String() || uploadedKind != wa.StatusMediaType(mime) {
				t.Fatalf("sent=%+v kind=%s err=%v", sent, uploadedKind, err)
			}
		})
	}
}

func TestChatSendMethodsRejectStatusBroadcast(t *testing.T) {
	c := &Client{connectedHook: func() bool { return true }}
	for _, jid := range []string{"status@broadcast", "123@newsletter"} {
		_, err := c.SendText(context.Background(), jid, "hello")
		if !errors.Is(err, wa.ErrNotSent) {
			t.Fatalf("SendText(%s) err=%v", jid, err)
		}
		_, err = c.SendFile(context.Background(), jid, wa.OutgoingFile{Data: []byte("x"), MimeType: "image/png"})
		if !errors.Is(err, wa.ErrNotSent) {
			t.Fatalf("SendFile(%s) err=%v", jid, err)
		}
	}
}

func TestPostStatusRejectsUnsupportedMedia(t *testing.T) {
	for _, mime := range []string{"image/gif", "audio/mpeg", "application/pdf"} {
		t.Run(mime, func(t *testing.T) {
			c := &Client{connectedHook: func() bool { return true }}
			c.uploadHook = func(context.Context, []byte, wa.MediaType) (uploaded, error) {
				t.Fatal("unsupported status media must not be uploaded")
				return uploaded{}, nil
			}
			_, err := c.PostStatus(context.Background(), wa.OutgoingFile{Data: []byte("media"), MimeType: mime})
			if !errors.Is(err, wa.ErrNotSent) {
				t.Fatalf("PostStatus(%s) err=%v, want ErrNotSent", mime, err)
			}
		})
	}
}
