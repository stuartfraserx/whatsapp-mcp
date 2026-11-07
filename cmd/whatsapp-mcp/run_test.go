package main

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/config"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
)

func TestVersionPrintsBuildVersion(t *testing.T) {
	setVar(t, &version, "1.2.3-test")
	rec := useClient(t, newTestClient(false))

	got := runCLI(t, map[string]string{}, "version")

	if got.code != 0 || got.stdout != "whatsapp-mcp 1.2.3-test\n" || got.stderr != "" {
		t.Fatalf("got %+v", got)
	}
	if rec.calls.Load() != 0 {
		t.Fatal("version must not open a client")
	}
}

func TestHelpPrintsUsageOnStdout(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help", "-help"} {
		t.Run(arg, func(t *testing.T) {
			rec := useClient(t, newTestClient(false))

			got := runCLI(t, map[string]string{}, arg)

			if got.code != 0 || got.stderr != "" {
				t.Fatalf("got %+v", got)
			}
			for _, want := range []string{"Usage:", "serve", "pair", "status", "unpair", "version"} {
				if !strings.Contains(got.stdout, want) {
					t.Errorf("usage lacks %q:\n%s", want, got.stdout)
				}
			}
			if rec.calls.Load() != 0 {
				t.Fatal("help must not open a client")
			}
		})
	}
}

func TestUnknownCommandIsUsageError(t *testing.T) {
	rec := useClient(t, newTestClient(false))

	got := runCLI(t, map[string]string{}, "frobnicate")

	if got.code != 2 || got.stdout != "" {
		t.Fatalf("got code=%d stdout=%q", got.code, got.stdout)
	}
	if !strings.Contains(got.stderr, `unknown command "frobnicate"`) || !strings.Contains(got.stderr, "Usage:") {
		t.Fatalf("stderr = %q", got.stderr)
	}
	if rec.calls.Load() != 0 {
		t.Fatal("unknown command must not open a client")
	}
}

func TestSubcommandHelpFlagExitsZeroAndListsFlags(t *testing.T) {
	for _, cmd := range []string{"serve", "pair", "status", "unpair"} {
		t.Run(cmd, func(t *testing.T) {
			env, dataDir := baseEnv(t)
			rec := useClient(t, newTestClient(false))

			got := runCLI(t, env, cmd, "-h")

			if got.code != 0 || got.stdout != "" {
				t.Fatalf("got code=%d stdout=%q stderr=%q", got.code, got.stdout, got.stderr)
			}
			if !strings.Contains(got.stderr, "-data-dir") {
				t.Fatalf("flag help missing on stderr: %q", got.stderr)
			}
			if rec.calls.Load() != 0 {
				t.Fatal("help must not open a client")
			}
			if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("help must not create the data dir, stat err = %v", err)
			}
		})
	}
}

func TestConfigErrorsExitWithUsageStatus(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		args    []string
		wantErr string
	}{
		{"serve invalid send flag", map[string]string{config.EnvSendEnabled: "maybe"}, []string{"serve"}, config.EnvSendEnabled},
		{"serve malformed http addr", nil, []string{"serve", "--transport", "http", "--http-addr", "localhost:1"}, "literal IP"},
		{"serve blank http token", map[string]string{config.EnvHTTPToken: "   "}, []string{"serve"}, "blank"},
		{"serve unknown transport", nil, []string{"serve", "--transport", "carrier-pigeon"}, "invalid transport"},
		{"serve unknown flag", nil, []string{"serve", "--no-such-flag"}, "no-such-flag"},
		{"serve positional arg", nil, []string{"serve", "extra"}, "unexpected arguments"},
		{"serve rate interval below minimum", nil, []string{"serve", "--rate-interval", "1ms"}, "rate interval"},
		{"default command with bad flag value", map[string]string{config.EnvSendEnabled: "maybe"}, nil, config.EnvSendEnabled},
		{"status bad log level", nil, []string{"status", "--log-level", "loud"}, "log level"},
		{"pair bad config", map[string]string{config.EnvSendEnabled: "maybe"}, []string{"pair"}, config.EnvSendEnabled},
		{"unpair bad config", map[string]string{config.EnvSendEnabled: "maybe"}, []string{"unpair"}, config.EnvSendEnabled},
		{"pair invalid wait", nil, []string{"pair", "--wait", "soon"}, "invalid --wait"},
		{"pair negative wait", nil, []string{"pair", "--wait", "-1s"}, "invalid --wait"},
		{"pair wait without value", nil, []string{"pair", "--wait"}, "needs a value"},
		{"unpair invalid delete-data", nil, []string{"unpair", "--delete-data=perhaps"}, "invalid --delete-data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, dataDir := baseEnv(t)
			for k, v := range tc.env {
				env[k] = v
			}
			rec := useClient(t, newTestClient(false))

			got := runCLI(t, env, tc.args...)

			if got.code != 2 {
				t.Fatalf("exit code = %d, want 2; stderr=%q", got.code, got.stderr)
			}
			if got.stdout != "" {
				t.Errorf("stdout = %q, want empty", got.stdout)
			}
			if !strings.Contains(got.stderr, tc.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", got.stderr, tc.wantErr)
			}
			if rec.calls.Load() != 0 {
				t.Error("a rejected configuration must not open a client")
			}
			if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a rejected configuration must not create the data dir, stat err = %v", err)
			}
		})
	}
}

func TestMissingHomeDirIsUsageErrorUnlessDataDirGiven(t *testing.T) {
	t.Run("no data dir anywhere", func(t *testing.T) {
		setVar(t, &userHomeDir, func() (string, error) { return "", errors.New("no home") })
		rec := useClient(t, newTestClient(false))

		got := runCLI(t, map[string]string{}, "status")

		if got.code != 2 || !strings.Contains(got.stderr, "cannot determine the home directory") || got.stdout != "" {
			t.Fatalf("got %+v", got)
		}
		if rec.calls.Load() != 0 {
			t.Fatal("must not open a client")
		}
	})
	t.Run("data dir from env", func(t *testing.T) {
		setVar(t, &userHomeDir, func() (string, error) { return "", errors.New("no home") })
		useClient(t, newTestClient(false))
		dir := filepath.Join(t.TempDir(), "d")

		got := runCLI(t, map[string]string{config.EnvDataDir: dir}, "status")

		if got.code != 0 {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("data dir from flag", func(t *testing.T) {
		setVar(t, &userHomeDir, func() (string, error) { return "", errors.New("no home") })
		useClient(t, newTestClient(false))
		dir := filepath.Join(t.TempDir(), "d")

		got := runCLI(t, map[string]string{}, "status", "--data-dir", dir)

		if got.code != 0 {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestOpenClientFailureIsRuntimeError(t *testing.T) {
	for _, cmd := range []string{"status", "pair", "unpair", "serve"} {
		t.Run(cmd, func(t *testing.T) {
			env, _ := baseEnv(t)
			setVar(t, &readStatus, func(context.Context, string) (wa.Status, error) { return wa.Status{}, errors.New("session db locked") })
			setVar(t, &openClient, func(context.Context, string, *slog.Logger) (clientCloser, error) {
				return nil, errors.New("session db locked")
			})

			got := runCLI(t, env, cmd)

			if got.code != 1 || got.stdout != "" {
				t.Fatalf("got %+v", got)
			}
			if !strings.Contains(got.stderr, "open whatsapp session") || !strings.Contains(got.stderr, "session db locked") {
				t.Fatalf("stderr = %q", got.stderr)
			}
		})
	}
}

func TestRuntimeErrorsScrubPhoneNumbers(t *testing.T) {
	setVar(t, &readStatus, func(context.Context, string) (wa.Status, error) {
		return wa.Status{}, errors.New("device " + testAccountJID + " rejected")
	})
	env, _ := baseEnv(t)
	setVar(t, &openClient, func(context.Context, string, *slog.Logger) (clientCloser, error) {
		return nil, errors.New("device " + testAccountJID + " rejected")
	})

	got := runCLI(t, env, "status")

	if got.code != 1 || strings.Contains(got.stderr, testPhone) {
		t.Fatalf("got %+v", got)
	}
}

func TestDirectoriesAreCreatedPrivate(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "a", "b", "data")
	downloadDir := filepath.Join(root, "downloads-elsewhere")
	outboxDir := filepath.Join(root, "outbox-elsewhere")
	env, _ := baseEnv(t)
	env[config.EnvDataDir] = dataDir
	env[config.EnvDownloadDir] = downloadDir
	env[config.EnvOutboxDir] = outboxDir
	useClient(t, newTestClient(true))

	got := runCLI(t, env, "pair")

	if got.code != 0 {
		t.Fatalf("got %+v", got)
	}
	for _, dir := range []string{dataDir, downloadDir, outboxDir} {
		assertDirMode(t, dir, 0o700)
	}
}

func TestDefaultDownloadAndOutboxLiveUnderDataDirAndExistingDirsAreTightened(t *testing.T) {
	env, dataDir := baseEnv(t)
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	useClient(t, newTestClient(true))

	got := runCLI(t, env, "pair")

	if got.code != 0 {
		t.Fatalf("got %+v", got)
	}
	for _, dir := range []string{dataDir, filepath.Join(dataDir, "downloads"), filepath.Join(dataDir, "outbox")} {
		assertDirMode(t, dir, 0o700)
	}
}

func assertDirMode(t *testing.T, dir string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if !info.IsDir() || info.Mode().Perm() != want {
		t.Errorf("%s: mode = %v (dir=%v), want %v", dir, info.Mode().Perm(), info.IsDir(), want)
	}
}

func TestStatusUnpaired(t *testing.T) {
	env, dataDir := baseEnv(t)
	tc := newTestClient(false)
	useClient(t, tc)

	got := runCLI(t, env, "status")

	if got.code != 0 {
		t.Fatalf("got %+v", got)
	}
	for _, want := range []string{
		"paired: no\n",
		"data dir: " + dataDir + "\n",
		"session db: " + filepath.Join(dataDir, "whatsapp.db") + "\n",
		"messages db: " + filepath.Join(dataDir, "messages.db") + "\n",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, got.stdout)
		}
	}
	if tc.ConnectCalls() != 0 {
		t.Fatal("status must never connect")
	}
	if tc.closeCalls.Load() != 0 {
		t.Fatalf("status opened a client: Close calls = %d", tc.closeCalls.Load())
	}
}

func TestStatusPairedRedactsAccountAndNeverConnects(t *testing.T) {
	env, _ := baseEnv(t)
	tc := newTestClient(true)
	useClient(t, tc)

	got := runCLI(t, env, "status")

	if got.code != 0 {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(got.stdout, "paired: yes (***78@s.whatsapp.net)\n") {
		t.Errorf("stdout = %q", got.stdout)
	}
	if strings.Contains(got.stdout, testPhone) || strings.Contains(got.stderr, testPhone) {
		t.Errorf("full phone number leaked: stdout=%q stderr=%q", got.stdout, got.stderr)
	}
	if tc.ConnectCalls() != 0 {
		t.Fatal("status must never connect")
	}
}

func TestPairWhenAlreadyPaired(t *testing.T) {
	env, _ := baseEnv(t)
	tc := newTestClient(true)
	useClient(t, tc)

	got := runCLI(t, env, "pair", "--wait", "0")

	if got.code != 0 {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(got.stdout, "already paired as ***78@s.whatsapp.net") {
		t.Errorf("stdout = %q", got.stdout)
	}
	if strings.Contains(got.stdout+got.stderr, testPhone) {
		t.Errorf("full phone number leaked: %q %q", got.stdout, got.stderr)
	}
	if tc.ConnectCalls() != 0 {
		t.Fatal("an already paired account must not reconnect")
	}
	if tc.disconnectCalls.Load() != 1 || tc.closeCalls.Load() != 1 {
		t.Fatalf("disconnect=%d close=%d, want 1/1", tc.disconnectCalls.Load(), tc.closeCalls.Load())
	}
}

func pairingClient() *testClient {
	tc := newTestClient(false)
	tc.SetQRCodesOnConnect("2@qr-code-payload")
	tc.connectHook = func(c *testClient) {
		go c.Emit(wa.PairedEvent{AccountJID: testAccountJID})
	}
	return tc
}

func TestPairUnpairedWaitsForPairedEvent(t *testing.T) {
	env, dataDir := baseEnv(t)
	tc := pairingClient()
	useClient(t, tc)

	got := runCLI(t, env, "pair", "--wait", "0")

	if got.code != 0 {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(got.stdout, "Scan the QR code") || !strings.Contains(got.stdout, filepath.Join(dataDir, "pairing-qr.png")) {
		t.Errorf("stdout lacks QR instructions: %q", got.stdout)
	}
	if !strings.Contains(got.stdout, "Paired.\n") {
		t.Errorf("stdout lacks Paired.: %q", got.stdout)
	}
	if strings.Contains(got.stdout, "Ingesting") {
		t.Errorf("--wait 0 must skip history ingestion wait: %q", got.stdout)
	}
	if strings.Contains(got.stdout+got.stderr, testPhone) || strings.Contains(got.stdout+got.stderr, "qr-code-payload") {
		t.Errorf("secret leaked: %q %q", got.stdout, got.stderr)
	}
	if tc.ConnectCalls() != 1 {
		t.Fatalf("Connect calls = %d, want 1", tc.ConnectCalls())
	}
	if tc.disconnectCalls.Load() != 1 || tc.closeCalls.Load() != 1 {
		t.Fatalf("disconnect=%d close=%d, want 1/1", tc.disconnectCalls.Load(), tc.closeCalls.Load())
	}
}

func TestPairTimesOutWhenNeverPaired(t *testing.T) {
	env, _ := baseEnv(t)
	setVar(t, &pairTimeout, 50*time.Millisecond)
	tc := newTestClient(false)
	useClient(t, tc)

	got := runCLI(t, env, "pair", "--wait", "0")

	if got.code != 1 {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(got.stderr, "pairing did not complete") {
		t.Errorf("stderr = %q", got.stderr)
	}
	if strings.Contains(got.stdout, "Paired.") {
		t.Errorf("stdout = %q", got.stdout)
	}
	if tc.disconnectCalls.Load() != 1 || tc.closeCalls.Load() != 1 {
		t.Fatalf("disconnect=%d close=%d, want 1/1", tc.disconnectCalls.Load(), tc.closeCalls.Load())
	}
}

func TestPairReportsExpiredQRCode(t *testing.T) {
	env, _ := baseEnv(t)
	tc := newTestClient(false)
	tc.connectHook = func(c *testClient) {
		go func() {
			c.Emit(wa.QRCodeEvent{Code: "2@qr-code-payload", Timeout: time.Minute})
			c.Emit(wa.QRTimeoutEvent{})
		}()
	}
	useClient(t, tc)

	got := runCLI(t, env, "pair", "--wait", "0")

	if got.code != exitError {
		t.Fatalf("exit = %d, want 1: %+v", got.code, got)
	}
	if !strings.Contains(got.stderr, "QR code expired; run `whatsapp-mcp pair` again") {
		t.Errorf("stderr = %q", got.stderr)
	}
	if strings.Contains(got.stderr, "pairing did not complete") || strings.Contains(got.stdout, "Paired.") {
		t.Errorf("got %+v", got)
	}
}

func TestErrorsAndUsageAreScrubbedOfPhoneNumbers(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
	}{
		{"allowlist from flag", []string{"serve", "--allowlist", testPhone + "x"}, nil},
		{"allowlist from env", []string{"serve"}, map[string]string{config.EnvAllowlist: "+" + testPhone + "x"}},
		{"flag error echoing the value", []string{"serve", "--rate-hourly", testPhone + "x"}, nil},
		{"unknown command", []string{testPhone}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, _ := baseEnv(t)
			maps.Copy(env, tt.env)
			useClient(t, newTestClient(true))

			got := runCLI(t, env, tt.args...)

			if got.code == exitOK || got.stderr == "" {
				t.Fatalf("got %+v, want a failure with a message", got)
			}
			if strings.Contains(got.stderr, testPhone) {
				t.Errorf("phone number leaked on stderr:\n%s", got.stderr)
			}
		})
	}
}

func TestPairCancelledBeforePairing(t *testing.T) {
	env, _ := baseEnv(t)
	tc := newTestClient(false)
	useClient(t, tc)
	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"pair"}, strings.NewReader(""), &stdout, &stderr, envLookup(env))
	}()

	waitFor(t, "pair to connect", func() bool { return tc.ConnectCalls() == 1 })
	cancel()

	if code := waitExit(t, done); code != 1 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "pairing cancelled") {
		t.Errorf("stderr = %q", stderr.String())
	}
}

func TestPairIngestsHistoryUntilInterrupted(t *testing.T) {
	env, _ := baseEnv(t)
	tc := pairingClient()
	useClient(t, tc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"pair", "--wait", "1h"}, strings.NewReader(""), &stdout, &stderr, envLookup(env))
	}()

	waitFor(t, "history ingestion notice", func() bool { return strings.Contains(stdout.String(), "Ingesting message history for 1h0m0s") })
	cancel()

	if code := waitExit(t, done); code != 0 {
		t.Fatalf("exit = %d, stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Paired.") {
		t.Errorf("stdout = %q", stdout.String())
	}
}

func seedDataDir(t *testing.T, dataDir string, downloadDir, outboxDir string) []string {
	t.Helper()
	files := []string{
		filepath.Join(dataDir, "whatsapp.db"),
		filepath.Join(dataDir, "whatsapp.db-wal"),
		filepath.Join(dataDir, "whatsapp.db-shm"),
		filepath.Join(dataDir, "messages.db"),
		filepath.Join(dataDir, "messages.db-wal"),
		filepath.Join(dataDir, "messages.db-shm"),
		filepath.Join(dataDir, "pairing-qr.png"),
		filepath.Join(downloadDir, "x"),
		filepath.Join(outboxDir, "y"),
	}
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func assertGone(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s should have been removed, stat err = %v", p, err)
		}
	}
}

func assertPresent(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s should still exist: %v", p, err)
		}
	}
}

func TestUnpairLogsOutAndKeepsDataByDefault(t *testing.T) {
	env, dataDir := baseEnv(t)
	tc := newTestClient(true)
	useClient(t, tc)
	files := seedDataDir(t, dataDir, filepath.Join(dataDir, "downloads"), filepath.Join(dataDir, "outbox"))

	got := runCLI(t, env, "unpair")

	if got.code != 0 || !strings.Contains(got.stdout, "logged out") {
		t.Fatalf("got %+v", got)
	}
	if tc.LogoutCalls() != 1 {
		t.Fatalf("Logout calls = %d, want 1", tc.LogoutCalls())
	}
	assertPresent(t, files...)
	if tc.disconnectCalls.Load() != 1 || tc.closeCalls.Load() != 1 {
		t.Fatalf("disconnect=%d close=%d, want 1/1", tc.disconnectCalls.Load(), tc.closeCalls.Load())
	}
}

func TestUnpairDeleteDataRemovesLocalState(t *testing.T) {
	env, dataDir := baseEnv(t)
	tc := newTestClient(true)
	useClient(t, tc)
	files := seedDataDir(t, dataDir, filepath.Join(dataDir, "downloads"), filepath.Join(dataDir, "outbox"))

	got := runCLI(t, env, "unpair", "--delete-data")

	if got.code != 0 {
		t.Fatalf("got %+v", got)
	}
	if tc.LogoutCalls() != 1 {
		t.Fatalf("Logout calls = %d, want 1", tc.LogoutCalls())
	}
	assertGone(t, files...)
	assertGone(t, filepath.Join(dataDir, "downloads"), filepath.Join(dataDir, "outbox"))
	assertPresent(t, dataDir)
	for _, want := range []string{"removed " + filepath.Join(dataDir, "whatsapp.db"), "removed " + filepath.Join(dataDir, "messages.db-wal"), "removed " + filepath.Join(dataDir, "pairing-qr.png")} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, got.stdout)
		}
	}
}

func TestUnpairDeleteDataKeepsDownloadDirOutsideDataDir(t *testing.T) {
	env, dataDir := baseEnv(t)
	outside := filepath.Join(t.TempDir(), "my-downloads")
	env[config.EnvDownloadDir] = outside
	useClient(t, newTestClient(true))
	files := seedDataDir(t, dataDir, outside, filepath.Join(dataDir, "outbox"))

	got := runCLI(t, env, "unpair", "--delete-data")

	if got.code != 0 {
		t.Fatalf("got %+v", got)
	}
	assertPresent(t, filepath.Join(outside, "x"))
	assertGone(t, files[:7]...)
	assertGone(t, files[8])
	if !strings.Contains(got.stdout, "kept "+outside) || !strings.Contains(got.stdout, "outside the data dir") {
		t.Errorf("stdout lacks kept notice:\n%s", got.stdout)
	}
}

func TestUnpairDeleteDataKeepsSymlinkEscapingDataDir(t *testing.T) {
	env, dataDir := baseEnv(t)
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "precious"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dataDir, "downloads")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	useClient(t, newTestClient(true))

	got := runCLI(t, env, "unpair", "--delete-data")

	if got.code != 0 {
		t.Fatalf("got %+v", got)
	}
	assertPresent(t, filepath.Join(target, "precious"))
	if !strings.Contains(got.stdout, "kept "+link) {
		t.Errorf("stdout = %q", got.stdout)
	}
}

func TestUnpairRejectsDownloadDirEqualToDataDir(t *testing.T) {
	env, dataDir := baseEnv(t)
	env[config.EnvDownloadDir] = dataDir
	tc := newTestClient(true)
	useClient(t, tc)
	marker := filepath.Join(dataDir, "notes.txt")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := runCLI(t, env, "unpair", "--delete-data")

	if got.code != exitUsage || tc.LogoutCalls() != 0 {
		t.Fatalf("got %+v, logout calls = %d", got, tc.LogoutCalls())
	}
	assertPresent(t, dataDir, marker)
}

func TestUnpairWhenNotPairedSkipsLogout(t *testing.T) {
	env, dataDir := baseEnv(t)
	tc := newTestClient(false)
	useClient(t, tc)
	files := seedDataDir(t, dataDir, filepath.Join(dataDir, "downloads"), filepath.Join(dataDir, "outbox"))

	got := runCLI(t, env, "unpair", "--delete-data")

	if got.code != 0 || !strings.Contains(got.stdout, "not paired; nothing to log out") {
		t.Fatalf("got %+v", got)
	}
	if tc.LogoutCalls() != 0 || tc.ConnectCalls() != 0 {
		t.Fatalf("logout=%d connect=%d, want 0/0", tc.LogoutCalls(), tc.ConnectCalls())
	}
	assertGone(t, files...)
}

func TestUnpairConnectsBeforeLogout(t *testing.T) {
	env, _ := baseEnv(t)
	tc := newTestClient(true)
	useClient(t, tc)

	got := runCLI(t, env, "unpair")

	if got.code != 0 || tc.ConnectCalls() != 1 || tc.LogoutCalls() != 1 {
		t.Fatalf("code=%d connect=%d logout=%d stdout=%q", got.code, tc.ConnectCalls(), tc.LogoutCalls(), got.stdout)
	}
}

func TestUnpairWarnsWhenConnectionNeverEstablishes(t *testing.T) {
	env, _ := baseEnv(t)
	setVar(t, &unpairConnectTimeout, 50*time.Millisecond)
	tc := newTestClient(true)
	tc.SetStatus(wa.Status{State: wa.StateDisconnected, Paired: true, AccountJID: testAccountJID})
	useClient(t, tc)

	got := runCLI(t, env, "unpair")

	if got.code != 0 {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(got.stdout, "could not connect to WhatsApp in time") {
		t.Errorf("stdout = %q", got.stdout)
	}
	if tc.LogoutCalls() != 1 {
		t.Fatalf("Logout calls = %d, want 1 (logout is still attempted)", tc.LogoutCalls())
	}
}

func TestUnpairLogoutFailureExitsNonZeroAndKeepsDataUnlessForced(t *testing.T) {
	failing := func(t *testing.T) (map[string]string, string, []string) {
		env, dataDir := baseEnv(t)
		tc := newTestClient(true)
		tc.logoutErr = errors.New("server said no for " + testPhone + " today")
		useClient(t, tc)
		files := seedDataDir(t, dataDir, filepath.Join(dataDir, "downloads"), filepath.Join(dataDir, "outbox"))
		return env, dataDir, files
	}
	assertWarned := func(t *testing.T, got result) {
		t.Helper()
		if got.code != exitError {
			t.Fatalf("exit = %d, want 1: %+v", got.code, got)
		}
		if !strings.Contains(got.stdout, "warning: logout failed") || !strings.Contains(got.stdout, "Linked devices") {
			t.Errorf("stdout = %q", got.stdout)
		}
		if strings.Contains(got.stdout, testPhone) || strings.Contains(got.stderr, testPhone) {
			t.Errorf("phone number leaked: stdout=%q stderr=%q", got.stdout, got.stderr)
		}
		if strings.Contains(got.stdout, "logged out; the device was removed") {
			t.Errorf("must not claim success: %q", got.stdout)
		}
	}

	t.Run("without --delete-data", func(t *testing.T) {
		env, _, files := failing(t)
		got := runCLI(t, env, "unpair")
		assertWarned(t, got)
		assertPresent(t, files...)
	})
	t.Run("--delete-data keeps everything", func(t *testing.T) {
		env, dataDir, files := failing(t)
		got := runCLI(t, env, "unpair", "--delete-data")
		assertWarned(t, got)
		assertPresent(t, files...)
		assertPresent(t, filepath.Join(dataDir, "downloads"), filepath.Join(dataDir, "outbox"))
		if !strings.Contains(got.stdout, "kept all local data") || !strings.Contains(got.stdout, "--force") {
			t.Errorf("stdout lacks the kept-data notice:\n%s", got.stdout)
		}
	})
	t.Run("--delete-data --force deletes but still fails", func(t *testing.T) {
		env, _, files := failing(t)
		got := runCLI(t, env, "unpair", "--delete-data", "--force")
		assertWarned(t, got)
		assertGone(t, files...)
	})
}
