package wameow

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

func TestSendDeadlineRetainsGeneratedID(t *testing.T) {
	c := &Client{SendTimeout: time.Millisecond, idHook: func() string { return "known-id" }}
	var calls atomic.Int32
	c.sendHook = func(ctx context.Context, _ types.JID, _ *waE2E.Message, extra whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		calls.Add(1)
		<-ctx.Done()
		return whatsmeow.SendResponse{}, ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	sent, err := c.send(ctx, types.EmptyJID, &waE2E.Message{})
	var maybe *wa.MaybeSentError
	if !errors.Is(err, wa.ErrMaybeSent) || !errors.As(err, &maybe) || maybe.MessageID != "known-id" || sent.ID != "known-id" || calls.Load() != 1 {
		t.Fatalf("sent=%+v err=%v", sent, err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	_, err = c.send(ctx, types.EmptyJID, &waE2E.Message{})
	if !errors.Is(err, wa.ErrNotSent) || err.Error() != "not_sent: whatsapp connection stale" {
		t.Fatal(err)
	}
}
func TestSelfHealSingleFlightAndCooldown(t *testing.T) {
	now := time.Now()
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	c := &Client{now: func() time.Time { return now }, healHook: func(context.Context) error { started <- struct{}{}; <-release; return nil }}
	c.keepAliveTimeout()
	if c.keepAliveCount != 1 || !c.lastHeal.IsZero() {
		t.Fatal("first keepalive healed")
	}
	c.keepAliveTimeout()
	<-started
	c.selfHeal("overlap")
	release <- struct{}{}
	wait := func() {
		t.Helper()
		for i := 0; i < 1000; i++ {
			c.mu.Lock()
			running := c.healing
			c.mu.Unlock()
			if !running {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("heal hung")
	}
	wait()
	c.selfHeal("cooldown")
	select {
	case <-started:
		t.Fatal("cooldown ignored")
	default:
	}
	now = now.Add(30 * time.Second)
	c.selfHeal("after cooldown")
	<-started
	release <- struct{}{}
	wait()
	c.setConnected()
	if c.keepAliveCount != 0 {
		t.Fatal("connected did not reset health")
	}
}
func TestPrewriteFailuresRefundable(t *testing.T) {
	for _, text := range []string{"failed to get device list: context deadline exceeded", "failed to send usync query: websocket not connected", "websocket is not connected"} {
		if !errors.Is(sendFailure(errors.New(text)), wa.ErrNotSent) {
			t.Fatal(text)
		}
	}
}

func TestUploadDeadlineNeverAdvancesToSend(t *testing.T) {
	c := &Client{}
	release := make(chan struct{})
	finished := make(chan struct{})
	c.uploadHook = func(context.Context, []byte, wa.MediaType) (uploaded, error) {
		defer close(finished)
		<-release
		return uploaded{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := c.upload(ctx, []byte("file"), wa.MediaDocument)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	close(release)
	<-finished
}
func TestQueuedSendExpiresWithoutEnteringSendMessage(t *testing.T) {
	c := &Client{sendGate: make(chan struct{}, 1)}
	c.sendGate <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	_, err := c.send(ctx, types.EmptyJID, &waE2E.Message{})
	if !errors.Is(err, wa.ErrNotSent) {
		t.Fatal(err)
	}
	<-c.sendGate
}
func TestHardDeadlineWithSlowCancellation(t *testing.T) {
	c := &Client{idHook: func() string { return "known" }}
	release := make(chan struct{})
	finished := make(chan struct{})
	c.sendHook = func(context.Context, types.JID, *waE2E.Message, whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
		defer close(finished)
		<-release
		return whatsmeow.SendResponse{}, context.DeadlineExceeded
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := c.send(ctx, types.EmptyJID, &waE2E.Message{})
	if !errors.Is(err, wa.ErrMaybeSent) {
		t.Fatal(err)
	}
	close(release)
	<-finished
}

func TestSelfHealForceCloseBeforeDisconnectAndRetry(t *testing.T) {
	raw, peer := net.Pipe()
	defer peer.Close()
	var attempts atomic.Int32
	done := make(chan struct{})
	c := &Client{rawConns: map[net.Conn]struct{}{raw: {}}}
	c.disconnectHook = func() {
		if _, err := raw.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
			t.Error("raw socket was not closed before Disconnect", err)
		}
	}
	c.healHook = func(context.Context) error {
		if attempts.Add(1) == 1 {
			return errors.New("reconnect failed")
		}
		return nil
	}
	c.connectedHook = func() bool { return attempts.Load() == 2 }
	c.selfHeal("test")
	go func() {
		for {
			c.mu.Lock()
			running := c.healing
			c.mu.Unlock()
			if !running {
				close(done)
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("heal hung")
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d", attempts.Load())
	}
}

func TestConnectedImmediatelyRestoresSendHealth(t *testing.T) {
	c := &Client{keepAliveCount: 2, lastKeepAlive: time.Now(), connectedHook: func() bool { return true }}
	if c.HealthyForSend() {
		t.Fatal("expected stale health")
	}
	c.setConnected() // events.Connected calls this before publishing events.
	if !c.HealthyForSend() || !c.lastKeepAlive.IsZero() || c.keepAliveCount != 0 {
		t.Fatal("Connected did not restore health immediately")
	}
}
