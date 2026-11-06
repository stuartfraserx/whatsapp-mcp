package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/wa"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAttemptSingleFlightDurableAndExpiry(t *testing.T) {
	now := time.Now()
	h := &handlers{Deps: Deps{DataDir: t.TempDir(), Now: func() time.Time { return now }}}
	var calls atomic.Int32
	run := func() (*mcp.CallToolResult, sendOutput, error) {
		calls.Add(1)
		time.Sleep(time.Millisecond)
		return nil, sendOutput{Status: "sent", MessageID: "generated"}, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, out, err := attempt(h, context.Background(), "same", run)
			if err != nil || out.MessageID != "generated" {
				t.Errorf("out=%+v err=%v", out, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("calls=%d", calls.Load())
	}
	// A fresh handler has no memory of the original send.
	h = &handlers{Deps: h.Deps}
	_, out, err := attempt(h, context.Background(), "same", run)
	if err != nil || out.MessageID != "generated" || calls.Load() != 1 {
		t.Fatal("durable replay failed")
	}
	now = now.Add(attemptWindow + time.Nanosecond)
	_, _, _ = attempt(h, context.Background(), "same", run)
	if calls.Load() != 2 {
		t.Fatal("expired attempt suppressed")
	}
}
func TestAttemptUnknownRetainsID(t *testing.T) {
	h := &handlers{Deps: Deps{DataDir: t.TempDir()}}
	calls := 0
	run := func() (*mcp.CallToolResult, sendOutput, error) {
		calls++
		return nil, sendOutput{Status: "unknown_maybe_sent", MessageID: "maybe"}, wa.ErrMaybeSent
	}
	for i := 0; i < 2; i++ {
		res, out, err := attempt(h, context.Background(), "key", run)
		if err != nil || !res.IsError || out.MessageID != "maybe" || out.Status != "unknown_maybe_sent" {
			t.Fatalf("out=%+v err=%v", out, err)
		}
		h = &handlers{Deps: h.Deps}
	}
	if calls != 1 {
		t.Fatal("unknown was resent")
	}
}
func TestAttemptNotSentAllowsRetry(t *testing.T) {
	h := &handlers{}
	calls := 0
	run := func() (*mcp.CallToolResult, sendOutput, error) {
		calls++
		return nil, sendOutput{}, errors.New("not_sent")
	}
	for i := 0; i < 2; i++ {
		_, _, _ = attempt(h, context.Background(), "key", run)
	}
	if calls != 2 {
		t.Fatal("not_sent prevented retry")
	}
}
func TestClientKeyOverridesFingerprint(t *testing.T) {
	h := &handlers{}
	a := h.attemptKey(context.Background(), "text", "client-key", "a", "first")
	b := h.attemptKey(context.Background(), "text", "client-key", "b", "different")
	if a != b {
		t.Fatal("client key did not override fingerprint")
	}
}

func TestCrashedInFlightAttemptIsUnknown(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	h := &handlers{Deps: Deps{DataDir: dir}}
	// Simulate a crash between durable reservation and completion by replacing
	// the persisted final record with its in_flight predecessor.
	_, _, err := attempt(h, context.Background(), "crash", func() (*mcp.CallToolResult, sendOutput, error) { return nil, sendOutput{Status: "sent"}, nil })
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "send_attempts.db"))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(attemptRecord{At: now, State: "in_flight"})
	if _, err = db.Exec("UPDATE attempts SET record=? WHERE key=?", raw, "crash"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	h = &handlers{Deps: h.Deps}
	result, out, err := attempt(h, context.Background(), "crash", func() (*mcp.CallToolResult, sendOutput, error) {
		t.Fatal("crashed attempt resent")
		return nil, sendOutput{}, nil
	})
	if err != nil || !result.IsError || out.Status != "unknown_maybe_sent" {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	info, err := os.Stat(filepath.Join(dir, "send_attempts.db"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("send attempt database is not private")
	}
}
