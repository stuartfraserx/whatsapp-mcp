package wameow

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types/events"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

func TestStreamReplacedBackoffAndStableReset(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	delays := make(chan time.Duration, 20)
	proceed := make(chan struct{}, 20)
	var calls atomic.Int32
	r := newReconnecter(log, func(context.Context) error {
		if calls.Add(1) < 8 {
			return errors.New("offline")
		}
		return nil
	})
	r.enabled = true
	now := time.Now()
	r.now = func() time.Time { return now }
	r.sleep = func(ctx context.Context, d time.Duration) error {
		delays <- d
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-proceed:
			return nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Client{reconnect: r, log: log, lifeCtx: ctx}
	event := make(chan wa.Event, 10)
	c.SetEventHandler(func(ev wa.Event) { event <- ev })
	c.handleEvent(&events.StreamReplaced{})
	if _, ok := (<-event).(wa.StreamReplacedEvent); !ok {
		t.Fatal("missing stream event")
	}
	for _, want := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute} {
		select {
		case got := <-delays:
			if got != want {
				t.Fatalf("delay %s want %s", got, want)
			}
			proceed <- struct{}{}
		case <-time.After(time.Second):
			t.Fatal("no reconnect scheduled")
		}
	}
	deadline := time.Now().Add(time.Second)
	for {
		r.mu.Lock()
		running := r.running
		r.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reconnect still running")
		}
		time.Sleep(time.Millisecond)
	}
	r.connected()
	now = now.Add(10 * time.Minute)
	c.handleEvent(&events.StreamReplaced{})
	<-event
	if got := <-delays; got != 5*time.Second {
		t.Fatalf("stable delay %s", got)
	}
	cancel()
	r.mu.Lock()
	count, reason := r.count, r.reason
	r.mu.Unlock()
	if count != 2 || reason != "stream_replaced" {
		t.Fatalf("count %d reason %s", count, reason)
	}
}
func TestStreamReplacedWithoutLockDoesNotConnect(t *testing.T) {
	r := newReconnecter(slog.Default(), func(context.Context) error { t.Error("reconnect without ownership"); return nil })
	r.replaced(context.Background())
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running || r.count != 1 {
		t.Fatal("incorrect no-owner status")
	}
}
