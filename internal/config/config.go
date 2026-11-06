// Package config loads the server configuration from flags and environment.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Transport selects how the MCP server is exposed.
type Transport string

// Supported transports.
const (
	TransportStdio Transport = "stdio"
	TransportHTTP  Transport = "http"
)

const (
	defaultHTTPAddr     = "127.0.0.1:8765"
	loopbackHost        = "127.0.0.1"
	defaultRateInterval = 3 * time.Second
	minRateInterval     = time.Second
	defaultRateHourly   = 60
	maxRateHourly       = 1000
	maxPort             = 65535
)

// Environment variable names.
const (
	EnvDataDir            = "WHATSAPP_MCP_DATA_DIR"
	EnvDownloadDir        = "WHATSAPP_MCP_DOWNLOAD_DIR"
	EnvOutboxDir          = "WHATSAPP_MCP_OUTBOX_DIR"
	EnvTransport          = "WHATSAPP_MCP_TRANSPORT"
	EnvHTTPPortFallback   = "WHATSAPP_MCP_HTTP_PORT_FALLBACK"
	EnvHTTPAddr           = "WHATSAPP_MCP_HTTP_ADDR"
	EnvHTTPToken          = "WHATSAPP_MCP_HTTP_TOKEN"
	EnvHTTPTokenFil       = "WHATSAPP_MCP_HTTP_TOKEN_FILE"
	EnvHTTPNoAuth         = "WHATSAPP_MCP_HTTP_NO_AUTH"
	EnvSendEnabled        = "WHATSAPP_MCP_SEND_ENABLED"
	EnvAllowlist          = "WHATSAPP_MCP_ALLOWLIST"
	EnvSendTimeout        = "WHATSAPP_MCP_SEND_TIMEOUT"
	EnvRateInterval       = "WHATSAPP_MCP_RATE_INTERVAL"
	EnvRateHourly         = "WHATSAPP_MCP_RATE_HOURLY"
	EnvLogLevel           = "WHATSAPP_MCP_LOG_LEVEL"
	EnvWebhookConfig      = "WHATSAPP_MCP_WEBHOOK_CONFIG"
	EnvDaemonStartCommand = "WHATSAPP_MCP_DAEMON_START_COMMAND"
)

// Config is the validated server configuration.
type Config struct {
	DataDir, DownloadDir, OutboxDir                  string
	WebhookConfig, DaemonStartCommand, HTTPTokenFile string
	Transport                                        Transport
	HTTPAddr, HTTPToken                              string
	HTTPPortFallback                                 int
	HTTPNoAuth                                       bool
	SendEnabled                                      bool
	Allowlist                                        []string
	SendTimeout                                      time.Duration
	RateInterval                                     time.Duration
	RateHourly                                       int
	LogLevel                                         slog.Level
}

// ReadFile reads the HTTP token file. It is a variable so tests can replace it.
var ReadFile = os.ReadFile

// Load parses args on top of the environment (via lookup) and home. Flags win
// over environment variables. The HTTP token is read only from the environment
// or from the file named by WHATSAPP_MCP_HTTP_TOKEN_FILE.
func Load(name string, args []string, lookup func(string) (string, bool), home string, stderr io.Writer) (Config, error) {
	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	env := func(key string) (string, bool) {
		v, ok := lookup(key)
		return v, ok && v != ""
	}
	envOr := func(key, def string) string {
		if v, ok := env(key); ok {
			return v
		}
		return def
	}

	dataDefault := envOr(EnvDataDir, filepath.Join(home, ".local", "share", "whatsapp-mcp"))

	sendDefault := true
	if v, ok := env(EnvSendEnabled); ok {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return Config{}, fmt.Errorf("%s: invalid boolean %q", EnvSendEnabled, v)
		}
		sendDefault = b
	}

	sendDefaultTimeout := 25 * time.Second
	if v, ok := env(EnvSendTimeout); ok {
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			return Config{}, fmt.Errorf("%s: invalid duration", EnvSendTimeout)
		}
		sendDefaultTimeout = d
	}
	intervalDefault := defaultRateInterval
	if v, ok := env(EnvRateInterval); ok {
		d, err := time.ParseDuration(strings.TrimSpace(v))
		if err != nil {
			return Config{}, fmt.Errorf("%s: invalid duration %q", EnvRateInterval, v)
		}
		intervalDefault = d
	}

	noAuthDefault := false
	if v, ok := env(EnvHTTPNoAuth); ok {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return Config{}, fmt.Errorf("%s: invalid boolean %q", EnvHTTPNoAuth, v)
		}
		noAuthDefault = b
	}

	hourlyDefault := defaultRateHourly
	if v, ok := env(EnvRateHourly); ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return Config{}, fmt.Errorf("%s: invalid integer %q", EnvRateHourly, v)
		}
		hourlyDefault = n
	}

	fallbackDefault := 0
	if v, ok := env(EnvHTTPPortFallback); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return Config{}, fmt.Errorf("%s: invalid integer", EnvHTTPPortFallback)
		}
		fallbackDefault = n
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	webhookConfig := fs.String("webhook-config", envOr(EnvWebhookConfig, ""), "private inbound webhook config file (empty disables notifier)")
	daemonStart := fs.String("daemon-start-command", envOr(EnvDaemonStartCommand, ""), "absolute path to daemon supervisor script")
	dataDir := fs.String("data-dir", dataDefault, "directory for session and message databases")
	downloadDir := fs.String("download-dir", envOr(EnvDownloadDir, ""), "directory for downloaded media (default <data-dir>/downloads)")
	outboxDir := fs.String("outbox-dir", envOr(EnvOutboxDir, ""), "directory send_file may read from (default <data-dir>/outbox)")
	transport := fs.String("transport", envOr(EnvTransport, string(TransportStdio)), "MCP transport: stdio or http")
	fallback := fs.Int("http-port-fallback", fallbackDefault, "additional HTTP ports to try when busy (0..50)")
	httpAddr := fs.String("http-addr", envOr(EnvHTTPAddr, defaultHTTPAddr), "HTTP listen address; host must be 127.0.0.1")
	httpNoAuth := fs.Bool("http-no-auth", noAuthDefault, "disable HTTP bearer authentication (loopback only)")
	sendEnabled := fs.Bool("send-enabled", sendDefault, "register the send_message, send_file, edit_message and delete_message tools")
	allowlist := fs.String("allowlist", envOr(EnvAllowlist, ""), "comma-separated JIDs or phone numbers allowed as recipients (empty = any)")
	sendTimeout := fs.Duration("send-timeout", sendDefaultTimeout, "send deadline (5s..120s)")
	rateInterval := fs.Duration("rate-interval", intervalDefault, "minimum interval between sends (>= 1s)")
	rateHourly := fs.Int("rate-hourly", hourlyDefault, "maximum sends per trailing hour (1..1000)")
	logLevel := fs.String("log-level", envOr(EnvLogLevel, "info"), "log level: debug, info, warn, error")
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	if fs.NArg() > 0 {
		return Config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}

	cfg := Config{
		WebhookConfig:      *webhookConfig,
		DaemonStartCommand: *daemonStart,
		HTTPTokenFile:      envOr(EnvHTTPTokenFil, ""),
		Transport:          Transport(strings.ToLower(strings.TrimSpace(*transport))),
		HTTPAddr:           strings.TrimSpace(*httpAddr),
		HTTPPortFallback:   *fallback,
		HTTPNoAuth:         *httpNoAuth,
		SendEnabled:        *sendEnabled,
		Allowlist:          splitList(*allowlist),
		SendTimeout:        *sendTimeout,
		RateInterval:       *rateInterval,
		RateHourly:         *rateHourly,
	}

	if cfg.HTTPPortFallback < 0 || cfg.HTTPPortFallback > 50 {
		return Config{}, errors.New("http port fallback must be 0..50")
	}
	if cfg.DaemonStartCommand != "" && !filepath.IsAbs(cfg.DaemonStartCommand) {
		return Config{}, errors.New("daemon start command must be an absolute executable path")
	}
	var err error
	if cfg.DataDir, err = absDir(*dataDir); err != nil {
		return Config{}, fmt.Errorf("data dir: %w", err)
	}
	if cfg.DownloadDir, err = absDirOr(*downloadDir, filepath.Join(cfg.DataDir, "downloads")); err != nil {
		return Config{}, fmt.Errorf("download dir: %w", err)
	}
	if cfg.OutboxDir, err = absDirOr(*outboxDir, filepath.Join(cfg.DataDir, "outbox")); err != nil {
		return Config{}, fmt.Errorf("outbox dir: %w", err)
	}

	if err := checkDirsOutsideData(cfg); err != nil {
		return Config{}, err
	}

	switch cfg.Transport {
	case TransportStdio:
	case TransportHTTP:
		if err := ValidateHTTPAddr(cfg.HTTPAddr); err != nil {
			return Config{}, err
		}
	default:
		return Config{}, fmt.Errorf("invalid transport %q: want stdio or http", cfg.Transport)
	}

	if cfg.SendTimeout < 5*time.Second || cfg.SendTimeout > 120*time.Second {
		return Config{}, errors.New("send timeout must be 5s..120s")
	}
	if cfg.RateInterval < minRateInterval {
		return Config{}, fmt.Errorf("rate interval %s is below the minimum of %s", cfg.RateInterval, minRateInterval)
	}
	if cfg.RateHourly < 1 || cfg.RateHourly > maxRateHourly {
		return Config{}, fmt.Errorf("rate hourly cap %d is out of range 1..%d", cfg.RateHourly, maxRateHourly)
	}
	if err := cfg.LogLevel.UnmarshalText([]byte(strings.TrimSpace(*logLevel))); err != nil {
		return Config{}, fmt.Errorf("invalid log level %q: want debug, info, warn or error", *logLevel)
	}

	if cfg.HTTPToken, err = loadToken(env); err != nil {
		return Config{}, err
	}
	if cfg.HTTPNoAuth && cfg.HTTPToken != "" {
		return Config{}, fmt.Errorf("--http-no-auth (%s) cannot be combined with %s or %s", EnvHTTPNoAuth, EnvHTTPToken, EnvHTTPTokenFil)
	}
	return cfg, nil
}

// ValidateHTTPAddr accepts only "127.0.0.1:<port>".
func ValidateHTTPAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid http address %q: %w", addr, err)
	}
	if host != loopbackHost {
		return fmt.Errorf("invalid http address %q: host must be exactly %s", addr, loopbackHost)
	}
	for _, r := range port {
		if r < '0' || r > '9' {
			return fmt.Errorf("invalid http address %q: port must be numeric", addr)
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > maxPort {
		return fmt.Errorf("invalid http address %q: port must be 1..%d", addr, maxPort)
	}
	return nil
}

// SessionDBPath is the whatsmeow device/session database.
func (c Config) SessionDBPath() string { return filepath.Join(c.DataDir, "whatsapp.db") }

// MessagesDBPath is the message store database.
func (c Config) MessagesDBPath() string { return filepath.Join(c.DataDir, "messages.db") }

// QRPNGPath is where the pairing QR image is written.
func (c Config) QRPNGPath() string { return filepath.Join(c.DataDir, "pairing-qr.png") }

// HTTPTokenPath is where the auto-generated HTTP bearer token is stored.
func (c Config) HTTPTokenPath() string { return filepath.Join(c.DataDir, "http-token") }

// checkDirsOutsideData rejects download and outbox directories that equal or
// contain the data directory, which would expose the session and message
// databases to media downloads and send_file.
func checkDirsOutsideData(cfg Config) error {
	for _, d := range []struct{ name, path string }{
		{"download dir", cfg.DownloadDir},
		{"outbox dir", cfg.OutboxDir},
	} {
		if containsPath(d.path, cfg.DataDir) {
			return fmt.Errorf("%s %q must not be the data dir or one of its ancestors", d.name, d.path)
		}
	}
	return nil
}

// containsPath reports whether outer equals inner or is an ancestor of it,
// comparing cleaned absolute paths and then symlink-resolved paths. A path that
// does not exist yet is resolved through its deepest existing ancestor, so a
// symlinked directory is caught before the data dir is first created.
func containsPath(outer, inner string) bool {
	if within(filepath.Clean(outer), filepath.Clean(inner)) {
		return true
	}
	return within(resolveExisting(outer), resolveExisting(inner))
}

func resolveExisting(p string) string {
	p = filepath.Clean(p)
	suffix := ""
	for cur := p; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, suffix)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		suffix = filepath.Join(filepath.Base(cur), suffix)
		cur = parent
	}
}

func within(outer, inner string) bool {
	rel, err := filepath.Rel(outer, inner)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func loadToken(env func(string) (string, bool)) (string, error) {
	token, hasToken := env(EnvHTTPToken)
	path, hasFile := env(EnvHTTPTokenFil)
	switch {
	case hasToken && hasFile:
		return "", fmt.Errorf("%s and %s are mutually exclusive", EnvHTTPToken, EnvHTTPTokenFil)
	case hasFile:
		b, err := ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", EnvHTTPTokenFil, err)
		}
		token = strings.TrimSpace(string(b))
		if token == "" {
			return "", errors.New(EnvHTTPTokenFil + ": token file is empty")
		}
		return token, nil
	case hasToken:
		token = strings.TrimSpace(token)
		if token == "" {
			return "", errors.New(EnvHTTPToken + ": token is blank")
		}
		return token, nil
	default:
		return "", nil
	}
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func absDir(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("must not be empty")
	}
	return filepath.Abs(p)
}

func absDirOr(p, def string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return def, nil
	}
	return filepath.Abs(p)
}
