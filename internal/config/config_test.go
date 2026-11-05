package config_test

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fmflurry/whatsapp-mcp/internal/config"
)

const home = "/home/tester"

func lookupOf(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

func load(t *testing.T, env map[string]string, args ...string) (config.Config, error) {
	t.Helper()
	return config.Load("serve", args, lookupOf(env), home, io.Discard)
}

func mustLoad(t *testing.T, env map[string]string, args ...string) config.Config {
	t.Helper()
	cfg, err := load(t, env, args...)
	if err != nil {
		t.Fatalf("Load(%v): %v", args, err)
	}
	return cfg
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, nil)

	wantData := filepath.Join(home, ".local", "share", "whatsapp-mcp")
	if cfg.DataDir != wantData {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, wantData)
	}
	if want := filepath.Join(wantData, "downloads"); cfg.DownloadDir != want {
		t.Errorf("DownloadDir = %q, want %q", cfg.DownloadDir, want)
	}
	if want := filepath.Join(wantData, "outbox"); cfg.OutboxDir != want {
		t.Errorf("OutboxDir = %q, want %q", cfg.OutboxDir, want)
	}
	if cfg.Transport != config.TransportStdio {
		t.Errorf("Transport = %q, want stdio", cfg.Transport)
	}
	if cfg.HTTPAddr != "127.0.0.1:8765" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	if cfg.HTTPToken != "" {
		t.Errorf("HTTPToken = %q, want empty", cfg.HTTPToken)
	}
	if !cfg.SendEnabled {
		t.Error("SendEnabled default must be true")
	}
	if len(cfg.Allowlist) != 0 {
		t.Errorf("Allowlist = %v, want empty", cfg.Allowlist)
	}
	if cfg.RateInterval != 3*time.Second || cfg.RateHourly != 60 {
		t.Errorf("rate = %v / %d, want 3s / 60", cfg.RateInterval, cfg.RateHourly)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
}

func TestLoadNilLookupBehavesLikeEmptyEnvironment(t *testing.T) {
	t.Parallel()
	cfg, err := config.Load("serve", nil, nil, home, io.Discard)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Transport != config.TransportStdio || cfg.RateHourly != 60 {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestDerivedPathsLiveInsideTheDataDir(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, map[string]string{config.EnvDataDir: "/srv/wa"})
	if cfg.DataDir != "/srv/wa" {
		t.Fatalf("DataDir = %q", cfg.DataDir)
	}
	if got := cfg.SessionDBPath(); got != "/srv/wa/whatsapp.db" {
		t.Errorf("SessionDBPath = %q", got)
	}
	if got := cfg.MessagesDBPath(); got != "/srv/wa/messages.db" {
		t.Errorf("MessagesDBPath = %q", got)
	}
	if got := cfg.QRPNGPath(); got != "/srv/wa/pairing-qr.png" {
		t.Errorf("QRPNGPath = %q", got)
	}
	if cfg.SessionDBPath() == cfg.MessagesDBPath() {
		t.Error("session and message databases must be different files")
	}
}

func TestDefaultDownloadAndOutboxFollowDataDir(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, nil, "--data-dir=/srv/wa")
	if cfg.DownloadDir != "/srv/wa/downloads" || cfg.OutboxDir != "/srv/wa/outbox" {
		t.Errorf("download=%q outbox=%q", cfg.DownloadDir, cfg.OutboxDir)
	}
	cfg = mustLoad(t, nil, "--data-dir=/srv/wa", "--download-dir=/mnt/dl")
	if cfg.DownloadDir != "/mnt/dl" || cfg.OutboxDir != "/srv/wa/outbox" {
		t.Errorf("download=%q outbox=%q", cfg.DownloadDir, cfg.OutboxDir)
	}
}

func TestLoadParsesEnvironment(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, map[string]string{
		config.EnvDataDir:      "/srv/data",
		config.EnvDownloadDir:  "/srv/dl",
		config.EnvOutboxDir:    "/srv/out",
		config.EnvTransport:    "http",
		config.EnvHTTPAddr:     "127.0.0.1:9000",
		config.EnvHTTPToken:    "tok",
		config.EnvSendEnabled:  "false",
		config.EnvAllowlist:    "+33612345678,123-456@g.us",
		config.EnvRateInterval: "5s",
		config.EnvRateHourly:   "10",
		config.EnvLogLevel:     "debug",
	})
	want := config.Config{
		DataDir: "/srv/data", DownloadDir: "/srv/dl", OutboxDir: "/srv/out",
		Transport: config.TransportHTTP, HTTPAddr: "127.0.0.1:9000", HTTPToken: "tok",
		SendEnabled: false, Allowlist: []string{"+33612345678", "123-456@g.us"},
		RateInterval: 5 * time.Second, RateHourly: 10, LogLevel: slog.LevelDebug,
	}
	if !equalConfig(cfg, want) {
		t.Errorf("cfg = %+v\nwant %+v", cfg, want)
	}
}

func equalConfig(a, b config.Config) bool {
	return a.DataDir == b.DataDir && a.DownloadDir == b.DownloadDir && a.OutboxDir == b.OutboxDir &&
		a.Transport == b.Transport && a.HTTPAddr == b.HTTPAddr && a.HTTPToken == b.HTTPToken &&
		a.SendEnabled == b.SendEnabled && slices.Equal(a.Allowlist, b.Allowlist) &&
		a.RateInterval == b.RateInterval && a.RateHourly == b.RateHourly && a.LogLevel == b.LogLevel
}

func TestFlagsOverrideEnvironment(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		config.EnvDataDir:      "/env/data",
		config.EnvDownloadDir:  "/env/dl",
		config.EnvOutboxDir:    "/env/out",
		config.EnvTransport:    "stdio",
		config.EnvHTTPAddr:     "127.0.0.1:1111",
		config.EnvSendEnabled:  "true",
		config.EnvAllowlist:    "111111111",
		config.EnvRateInterval: "9s",
		config.EnvRateHourly:   "99",
		config.EnvLogLevel:     "error",
	}
	cfg := mustLoad(t, env,
		"--data-dir=/flag/data",
		"--download-dir=/flag/dl",
		"--outbox-dir=/flag/out",
		"--transport=http",
		"--http-addr=127.0.0.1:2222",
		"--send-enabled=false",
		"--allowlist=222222222,333333333",
		"--rate-interval=2s",
		"--rate-hourly=7",
		"--log-level=warn",
	)
	want := config.Config{
		DataDir: "/flag/data", DownloadDir: "/flag/dl", OutboxDir: "/flag/out",
		Transport: config.TransportHTTP, HTTPAddr: "127.0.0.1:2222",
		SendEnabled: false, Allowlist: []string{"222222222", "333333333"},
		RateInterval: 2 * time.Second, RateHourly: 7, LogLevel: slog.LevelWarn,
	}
	if !equalConfig(cfg, want) {
		t.Errorf("cfg = %+v\nwant %+v", cfg, want)
	}
}

func TestFlagCanEnableWhatEnvDisables(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, map[string]string{config.EnvSendEnabled: "false"}, "--send-enabled=true")
	if !cfg.SendEnabled {
		t.Error("--send-enabled=true must override WHATSAPP_MCP_SEND_ENABLED=false")
	}
}

func TestEmptyEnvironmentValuesAreTreatedAsUnset(t *testing.T) {
	t.Parallel()
	env := map[string]string{}
	for _, k := range []string{
		config.EnvDataDir, config.EnvDownloadDir, config.EnvOutboxDir, config.EnvTransport, config.EnvHTTPAddr,
		config.EnvHTTPToken, config.EnvHTTPTokenFil, config.EnvSendEnabled, config.EnvAllowlist,
		config.EnvRateInterval, config.EnvRateHourly, config.EnvLogLevel,
	} {
		env[k] = ""
	}
	cfg := mustLoad(t, env)
	if cfg.DataDir != filepath.Join(home, ".local", "share", "whatsapp-mcp") || !cfg.SendEnabled ||
		cfg.RateHourly != 60 || cfg.Transport != config.TransportStdio || cfg.HTTPToken != "" {
		t.Errorf("empty env values must fall back to defaults, got %+v", cfg)
	}
}

func TestBooleanParsing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want bool
	}{
		{"true", true}, {"1", true}, {"T", true}, {"TRUE", true},
		{"false", false}, {"0", false}, {"f", false}, {"False", false}, {" false ", false},
	}
	for _, tt := range tests {
		cfg, err := load(t, map[string]string{config.EnvSendEnabled: tt.in})
		if err != nil || cfg.SendEnabled != tt.want {
			t.Errorf("SEND_ENABLED=%q -> (%v, %v), want %v", tt.in, cfg.SendEnabled, err, tt.want)
		}
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		env  map[string]string
		args []string
	}{
		{"env bool garbage", map[string]string{config.EnvSendEnabled: "maybe"}, nil},
		{"env bool yes", map[string]string{config.EnvSendEnabled: "yes"}, nil},
		{"flag bool garbage", nil, []string{"--send-enabled=maybe"}},
		{"env duration garbage", map[string]string{config.EnvRateInterval: "fast"}, nil},
		{"env duration without unit", map[string]string{config.EnvRateInterval: "3"}, nil},
		{"flag duration garbage", nil, []string{"--rate-interval=fast"}},
		{"env hourly garbage", map[string]string{config.EnvRateHourly: "many"}, nil},
		{"env hourly float", map[string]string{config.EnvRateHourly: "1.5"}, nil},
		{"flag hourly garbage", nil, []string{"--rate-hourly=many"}},
		{"hourly zero", nil, []string{"--rate-hourly=0"}},
		{"hourly negative", nil, []string{"--rate-hourly=-1"}},
		{"hourly above max", nil, []string{"--rate-hourly=1001"}},
		{"env hourly above max", map[string]string{config.EnvRateHourly: "100000"}, nil},
		{"interval 999ms", nil, []string{"--rate-interval=999ms"}},
		{"interval zero", nil, []string{"--rate-interval=0s"}},
		{"interval negative", nil, []string{"--rate-interval=-5s"}},
		{"env interval below one second", map[string]string{config.EnvRateInterval: "500ms"}, nil},
		{"log level verbose", nil, []string{"--log-level=verbose"}},
		{"env log level garbage", map[string]string{config.EnvLogLevel: "loud"}, nil},
		{"log level number word", nil, []string{"--log-level=three"}},
		{"transport grpc", nil, []string{"--transport=grpc"}},
		{"env transport websocket", map[string]string{config.EnvTransport: "websocket"}, nil},
		{"empty data dir flag", nil, []string{"--data-dir="}},
		{"blank data dir flag", nil, []string{"--data-dir=   "}},
		{"unknown flag", nil, []string{"--nope"}},
		{"positional argument", nil, []string{"extra"}},
		{"positional after flags", nil, []string{"--log-level=info", "extra", "more"}},
		{"http token as flag", nil, []string{"--http-token=secret"}},
		{"token as flag", nil, []string{"--token=secret"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if cfg, err := load(t, tt.env, tt.args...); err == nil {
				t.Errorf("Load succeeded with %+v, want an error", cfg)
			}
		})
	}
}

func TestRateBoundariesAreAccepted(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"--rate-interval=1s"},
		{"--rate-interval=1m"},
		{"--rate-hourly=1"},
		{"--rate-hourly=1000"},
	} {
		if _, err := load(t, nil, args...); err != nil {
			t.Errorf("Load(%v): %v", args, err)
		}
	}
}

func TestLogLevels(t *testing.T) {
	t.Parallel()
	tests := map[string]slog.Level{
		"debug": slog.LevelDebug, "info": slog.LevelInfo, "warn": slog.LevelWarn, "error": slog.LevelError,
		"WARN": slog.LevelWarn, " error ": slog.LevelError,
	}
	for in, want := range tests {
		cfg, err := load(t, nil, "--log-level="+in)
		if err != nil || cfg.LogLevel != want {
			t.Errorf("--log-level=%q -> (%v, %v), want %v", in, cfg.LogLevel, err, want)
		}
	}
}

func TestTransportParsing(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, nil, "--transport=HTTP")
	if cfg.Transport != config.TransportHTTP {
		t.Errorf("Transport = %q, want http", cfg.Transport)
	}
	cfg = mustLoad(t, nil, "--transport=stdio", "--http-addr=0.0.0.0:80")
	if cfg.Transport != config.TransportStdio {
		t.Errorf("Transport = %q, want stdio", cfg.Transport)
	}
}

func TestHTTPTransportValidatesAddress(t *testing.T) {
	t.Parallel()
	if _, err := load(t, nil, "--transport=http"); err != nil {
		t.Errorf("http transport with the default address: %v", err)
	}
	for _, addr := range []string{"0.0.0.0:8765", "localhost:8765", "192.168.1.2:8765", "[::1]:8765", "127.0.0.1", "127.0.0.1:0", ":8765"} {
		if cfg, err := load(t, nil, "--transport=http", "--http-addr="+addr); err == nil {
			t.Errorf("http transport accepted address %q: %+v", addr, cfg)
		}
		if _, err := load(t, map[string]string{config.EnvTransport: "http", config.EnvHTTPAddr: addr}); err == nil {
			t.Errorf("http transport accepted env address %q", addr)
		}
	}
}

func TestValidateHTTPAddr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		addr string
		ok   bool
	}{
		{"127.0.0.1:8765", true},
		{"127.0.0.1:1", true},
		{"127.0.0.1:65535", true},
		{"127.0.0.1:0", false},
		{"127.0.0.1:65536", false},
		{"127.0.0.1:70000", false},
		{"127.0.0.1:abc", false},
		{"127.0.0.1:http", false},
		{"127.0.0.1:+80", false},
		{"127.0.0.1:-1", false},
		{"127.0.0.1:", false},
		{"127.0.0.1:80:80", false},
		{"127.0.0.1", false},
		{"", false},
		{":8765", false},
		{"0.0.0.0:8765", false},
		{"localhost:8765", false},
		{"LOCALHOST:8765", false},
		{"::1", false},
		{"[::1]:1", false},
		{"[::1]:8765", false},
		{"192.168.1.2:8765", false},
		{"127.0.0.2:8765", false},
		{"127.1:8765", false},
		{"127.0.0.01:8765", false},
		{" 127.0.0.1:8765", false},
		{"127.0.0.1 :8765", false},
		{"example.com:8765", false},
		{"[::ffff:127.0.0.1]:8765", false},
	}
	for _, tt := range tests {
		err := config.ValidateHTTPAddr(tt.addr)
		if (err == nil) != tt.ok {
			t.Errorf("ValidateHTTPAddr(%q) error = %v, want ok=%v", tt.addr, err, tt.ok)
		}
	}
}

func TestRelativePathsBecomeAbsolute(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, nil, "--data-dir=rel/data", "--download-dir=dl", "--outbox-dir=./out/../out2")
	for name, p := range map[string]string{"data": cfg.DataDir, "download": cfg.DownloadDir, "outbox": cfg.OutboxDir} {
		if !filepath.IsAbs(p) {
			t.Errorf("%s dir %q is not absolute", name, p)
		}
	}
	wantData, _ := filepath.Abs("rel/data")
	if cfg.DataDir != wantData {
		t.Errorf("DataDir = %q, want %q", cfg.DataDir, wantData)
	}
	if !strings.HasSuffix(cfg.OutboxDir, string(filepath.Separator)+"out2") {
		t.Errorf("OutboxDir = %q, want a cleaned path ending in out2", cfg.OutboxDir)
	}
	cfg = mustLoad(t, map[string]string{config.EnvDataDir: "env-rel"})
	if !filepath.IsAbs(cfg.DataDir) || !filepath.IsAbs(cfg.DownloadDir) || !filepath.IsAbs(cfg.OutboxDir) {
		t.Errorf("env relative data dir must yield absolute dirs: %+v", cfg)
	}
}

func TestAllowlistParsing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"single", "33612345678", []string{"33612345678"}},
		{"trims entries", " 33612345678 , 123-456@g.us ", []string{"33612345678", "123-456@g.us"}},
		{"drops empty entries", ",,a,, ,b,", []string{"a", "b"}},
		{"only separators", " , ,, ", nil},
		{"keeps spaces inside entries", "+33 6 12 34 56 78,0044 7911 123456", []string{"+33 6 12 34 56 78", "0044 7911 123456"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg := mustLoad(t, map[string]string{config.EnvAllowlist: tt.in})
			if !slices.Equal(cfg.Allowlist, tt.want) {
				t.Errorf("env %q -> %q, want %q", tt.in, cfg.Allowlist, tt.want)
			}
			if tt.in != "" {
				cfg = mustLoad(t, nil, "--allowlist="+tt.in)
				if !slices.Equal(cfg.Allowlist, tt.want) {
					t.Errorf("flag %q -> %q, want %q", tt.in, cfg.Allowlist, tt.want)
				}
			}
		})
	}
}

func TestTokenFromEnvironmentIsTrimmed(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, map[string]string{config.EnvHTTPToken: "  s3cret \n"})
	if cfg.HTTPToken != "s3cret" {
		t.Errorf("HTTPToken = %q, want s3cret", cfg.HTTPToken)
	}
}

func TestTokenFromFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    string
		wantErr bool
	}{
		{"trailing newline", "s3cret\n", "s3cret", false},
		{"surrounding whitespace", "  \ts3cret \r\n\n", "s3cret", false},
		{"plain", "s3cret", "s3cret", false},
		{"inner whitespace kept", "a b", "a b", false},
		{"empty file", "", "", true},
		{"whitespace only", " \n\t\n", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "http.token")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := load(t, map[string]string{config.EnvHTTPTokenFil: path})
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load error = %v, wantErr %v", err, tt.wantErr)
			}
			if cfg.HTTPToken != tt.want {
				t.Errorf("HTTPToken = %q, want %q", cfg.HTTPToken, tt.want)
			}
		})
	}
}

func TestTokenFileMissingIsAnError(t *testing.T) {
	t.Parallel()
	_, err := load(t, map[string]string{config.EnvHTTPTokenFil: filepath.Join(t.TempDir(), "absent")})
	if err == nil {
		t.Fatal("a missing token file must fail Load rather than silently disable auth")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error %v should wrap os.ErrNotExist", err)
	}
}

func TestTokenAndTokenFileAreMutuallyExclusive(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "http.token")
	if err := os.WriteFile(path, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := load(t, map[string]string{config.EnvHTTPToken: "from-env-secret", config.EnvHTTPTokenFil: path})
	if err == nil {
		t.Fatal("setting both a token and a token file must fail")
	}
	for _, leak := range []string{"from-env-secret", "from-file"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q leaks the token value %q", err, leak)
		}
	}
}

func TestUsageErrorsAreWrittenToStderrWriter(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	_, err := config.Load("serve", []string{"--no-such-flag"}, lookupOf(nil), home, &buf)
	if err == nil {
		t.Fatal("unknown flag must fail")
	}
	if !strings.Contains(buf.String(), "no-such-flag") {
		t.Errorf("stderr writer got %q, want the flag name", buf.String())
	}
}

func TestHelpFlagReturnsErrHelp(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	_, err := config.Load("serve", []string{"-h"}, lookupOf(nil), home, &buf)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("Load(-h) error = %v, want flag.ErrHelp", err)
	}
	for _, name := range []string{"data-dir", "transport", "http-addr", "send-enabled", "allowlist", "rate-interval", "rate-hourly", "log-level"} {
		if !strings.Contains(buf.String(), name) {
			t.Errorf("usage output lacks --%s", name)
		}
	}
	if strings.Contains(buf.String(), "-http-token") || strings.Contains(buf.String(), "-token") {
		t.Error("usage must not advertise a token flag; tokens come from env or file only")
	}
	if !strings.Contains(buf.String(), "serve") {
		t.Errorf("usage should be named after the subcommand, got %q", buf.String())
	}
}

func TestDownloadAndOutboxDirsMustNotContainTheDataDir(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		args []string
	}{
		{"download equals data", []string{"--data-dir=/srv/wa", "--download-dir=/srv/wa"}},
		{"outbox equals data", []string{"--data-dir=/srv/wa", "--outbox-dir=/srv/wa/"}},
		{"download is ancestor", []string{"--data-dir=/srv/wa/data", "--download-dir=/srv"}},
		{"outbox is ancestor", []string{"--data-dir=/srv/wa/data", "--outbox-dir=/"}},
		{"unclean ancestor", []string{"--data-dir=/srv/wa/data", "--outbox-dir=/srv/wa/x/.."}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := load(t, nil, tt.args...); err == nil {
				t.Fatalf("Load(%v) accepted a directory containing the data dir", tt.args)
			}
		})
	}
	for _, args := range [][]string{
		{"--data-dir=/srv/wa", "--download-dir=/srv/wa/downloads", "--outbox-dir=/srv/wa-outbox"},
		{"--data-dir=/srv/wa", "--download-dir=/srv/wa..x"},
		{"--data-dir=/srv/wa", "--download-dir=/srv/other"},
	} {
		if _, err := load(t, nil, args...); err != nil {
			t.Errorf("Load(%v): %v", args, err)
		}
	}
}

func TestDownloadDirThroughSymlinkToDataDirIsRejected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	data := filepath.Join(root, "data")
	if err := os.Mkdir(data, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(data, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := load(t, nil, "--data-dir="+data, "--download-dir="+link); err == nil {
		t.Fatal("a symlink to the data dir must be rejected")
	}
	if _, err := load(t, nil, "--data-dir="+data, "--outbox-dir="+root); err == nil {
		t.Fatal("an ancestor of the data dir must be rejected")
	}
}

func TestDownloadDirSymlinkToAncestorOfMissingDataDirIsRejected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	data := filepath.Join(root, "data", "wa")
	for _, flag := range []string{"--download-dir=", "--outbox-dir="} {
		if _, err := load(t, nil, "--data-dir="+data, flag+link); err == nil {
			t.Errorf("%s symlinked to an ancestor of a not-yet-created data dir must be rejected", flag)
		}
	}
	if _, err := load(t, nil, "--data-dir="+data, "--download-dir="+filepath.Join(root, "elsewhere")); err != nil {
		t.Errorf("an unrelated missing dir must be accepted: %v", err)
	}
}

func TestHTTPNoAuth(t *testing.T) {
	t.Parallel()
	if cfg := mustLoad(t, nil); cfg.HTTPNoAuth {
		t.Error("HTTPNoAuth must default to false")
	}
	if cfg := mustLoad(t, map[string]string{config.EnvHTTPNoAuth: "true"}); !cfg.HTTPNoAuth {
		t.Error("env must enable HTTPNoAuth")
	}
	if cfg := mustLoad(t, nil, "--http-no-auth"); !cfg.HTTPNoAuth {
		t.Error("flag must enable HTTPNoAuth")
	}
	if cfg := mustLoad(t, map[string]string{config.EnvHTTPNoAuth: "true"}, "--http-no-auth=false"); cfg.HTTPNoAuth {
		t.Error("flag must win over env")
	}
	if _, err := load(t, map[string]string{config.EnvHTTPNoAuth: "maybe"}); err == nil {
		t.Error("invalid boolean must fail")
	}
}

func TestHTTPNoAuthConflictsWithToken(t *testing.T) {
	t.Parallel()
	if _, err := load(t, map[string]string{config.EnvHTTPToken: "s3cret-token"}, "--http-no-auth"); err == nil {
		t.Fatal("--http-no-auth with a token must fail")
	} else if strings.Contains(err.Error(), "s3cret-token") {
		t.Errorf("error leaks the token: %v", err)
	}
	path := filepath.Join(t.TempDir(), "tok")
	if err := os.WriteFile(path, []byte("from-file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := load(t, map[string]string{config.EnvHTTPNoAuth: "1", config.EnvHTTPTokenFil: path}); err == nil {
		t.Fatal("no-auth with a token file must fail")
	}
}

func TestHTTPTokenPath(t *testing.T) {
	t.Parallel()
	cfg := mustLoad(t, nil, "--data-dir=/srv/wa")
	if want := filepath.Join("/srv/wa", "http-token"); cfg.HTTPTokenPath() != want {
		t.Errorf("HTTPTokenPath() = %q, want %q", cfg.HTTPTokenPath(), want)
	}
}
