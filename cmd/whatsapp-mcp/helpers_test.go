package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/config"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
	"github.com/fmflurry/whatsapp-mcp/internal/wa/watest"
)

const (
	testDeadline = 10 * time.Second
	testPoll     = 5 * time.Millisecond

	testAccountJID = "33612345678@s.whatsapp.net"
	testPhone      = "33612345678"
)

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type testClient struct {
	*watest.Fake

	disconnectCalls atomic.Int32
	closeCalls      atomic.Int32
	logoutErr       error
	connectHook     func(*testClient)
}

func newTestClient(paired bool) *testClient {
	tc := &testClient{Fake: watest.NewFake()}
	if paired {
		tc.SetStatus(wa.Status{State: wa.StateConnected, Paired: true, AccountJID: testAccountJID})
	}
	return tc
}

func (c *testClient) Paired() bool { return c.Status().Paired }

func (c *testClient) Close() error {
	c.closeCalls.Add(1)
	return nil
}

func (c *testClient) Disconnect() {
	c.disconnectCalls.Add(1)
	c.Fake.Disconnect()
}

func (c *testClient) Connect(ctx context.Context) error {
	err := c.Fake.Connect(ctx)
	if c.connectHook != nil {
		c.connectHook(c)
	}
	return err
}

func (c *testClient) Logout(ctx context.Context) error {
	if c.logoutErr != nil {
		_ = c.Fake.Logout(ctx)
		return c.logoutErr
	}
	return c.Fake.Logout(ctx)
}

type openRecorder struct {
	calls atomic.Int32
}

func useClient(t *testing.T, c clientCloser) *openRecorder {
	t.Helper()
	rec := &openRecorder{}
	prev := openClient
	previousStatus := readStatus
	readStatus = func(context.Context, string) (wa.Status, error) { return c.Status(), nil }
	openClient = func(context.Context, string, *slog.Logger) (clientCloser, error) {
		rec.calls.Add(1)
		return c, nil
	}
	t.Cleanup(func() { openClient = prev; readStatus = previousStatus })
	return rec
}

func setVar[T any](t *testing.T, target *T, value T) {
	t.Helper()
	prev := *target
	*target = value
	t.Cleanup(func() { *target = prev })
}

func envLookup(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

func baseEnv(t *testing.T) (env map[string]string, dataDir string) {
	t.Helper()
	dataDir = filepath.Join(t.TempDir(), "data")
	setVar(t, &userHomeDir, func() (string, error) { return t.TempDir(), nil })
	setVar(t, &isTerminal, func(io.Writer) bool { return false })
	return map[string]string{config.EnvDataDir: dataDir}, dataDir
}

type result struct {
	code           int
	stdout, stderr string
}

func runCLI(t *testing.T, env map[string]string, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*testDeadline)
	defer cancel()
	var stdout, stderr bytes.Buffer
	code := run(ctx, args, bytes.NewReader(nil), &stdout, &stderr, envLookup(env))
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testDeadline)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(testPoll)
	}
}

func waitExit(t *testing.T, done <-chan int) int {
	t.Helper()
	select {
	case code := <-done:
		return code
	case <-time.After(testDeadline):
		t.Fatal("run did not return in time")
		return -1
	}
}

type lineValidator struct {
	mu      sync.Mutex
	dst     io.Writer
	pending []byte
	lines   []string
	invalid []string
}

func (v *lineValidator) Write(p []byte) (int, error) {
	v.mu.Lock()
	v.pending = append(v.pending, p...)
	for {
		i := bytes.IndexByte(v.pending, '\n')
		if i < 0 {
			break
		}
		line := string(v.pending[:i])
		v.pending = v.pending[i+1:]
		v.lines = append(v.lines, line)
		var msg struct {
			JSONRPC string `json:"jsonrpc"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil || msg.JSONRPC != "2.0" {
			v.invalid = append(v.invalid, line)
		}
	}
	v.mu.Unlock()
	return v.dst.Write(p)
}

func (v *lineValidator) snapshot() (lines, invalid []string, partial string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.lines...), append([]string(nil), v.invalid...), string(v.pending)
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}
