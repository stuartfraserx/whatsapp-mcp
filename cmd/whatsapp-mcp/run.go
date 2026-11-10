package main

import (
	"context"
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

	"golang.org/x/term"

	"github.com/fmflurry/whatsapp-mcp/internal/config"
	"github.com/fmflurry/whatsapp-mcp/internal/daemon"
	"github.com/fmflurry/whatsapp-mcp/internal/datadir"
	"github.com/fmflurry/whatsapp-mcp/internal/instance"
	"github.com/fmflurry/whatsapp-mcp/internal/notify"
	"github.com/fmflurry/whatsapp-mcp/internal/qr"
	"github.com/fmflurry/whatsapp-mcp/internal/ratelimit"
	"github.com/fmflurry/whatsapp-mcp/internal/redact"
	"github.com/fmflurry/whatsapp-mcp/internal/sendpolicy"
	"github.com/fmflurry/whatsapp-mcp/internal/server"
	"github.com/fmflurry/whatsapp-mcp/internal/session"
	"github.com/fmflurry/whatsapp-mcp/internal/store"
	"github.com/fmflurry/whatsapp-mcp/internal/tools"
	"github.com/fmflurry/whatsapp-mcp/internal/upstream"
	"github.com/fmflurry/whatsapp-mcp/internal/wa"
	"github.com/fmflurry/whatsapp-mcp/internal/wameow"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"
var processStartedAt = time.Now()

const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2

	defaultPairWait = 2 * time.Minute
	unpairPoll      = 200 * time.Millisecond
)

// Seams replaced by tests.
var (
	openClient = func(ctx context.Context, path string, log *slog.Logger) (clientCloser, error) {
		c, err := wameow.Open(ctx, path, log)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
	readStatus  = wameow.ReadStatus
	userHomeDir = os.UserHomeDir
	isTerminal  = func(w io.Writer) bool {
		f, ok := w.(*os.File)
		return ok && term.IsTerminal(int(f.Fd()))
	}
	pairTimeout          = 5 * time.Minute
	unpairConnectTimeout = 30 * time.Second
	unpairLogoutTimeout  = 30 * time.Second
)

// clientCloser is the WhatsApp client plus the offline operations the CLI needs.
type clientCloser interface {
	wa.Client
	Paired() bool
	Close() error
}

const usageText = `whatsapp-mcp connects a personal WhatsApp account to an MCP client.

Usage:
  whatsapp-mcp [serve] [flags]        run the MCP server (default)
  whatsapp-mcp pair [--wait 2m] [flags]
                                      link this server to WhatsApp and ingest history
  whatsapp-mcp health [--data-dir DIR] [--timeout 10s]
  whatsapp-mcp status [flags]         show pairing state without connecting
  whatsapp-mcp unpair [--delete-data] [--force] [flags]
                                      log out; with --delete-data also remove local data
                                      once logged out (--force: even if logout failed)
  whatsapp-mcp version
  whatsapp-mcp help

Run "whatsapp-mcp <command> -h" for the flags shared by the commands that
read the configuration (--data-dir, --transport, --http-addr, ...).
`

type usageError struct{ err error }

func (u usageError) Error() string { return u.err.Error() }
func (u usageError) Unwrap() error { return u.err }

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

type env struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	lookup         func(string) (string, bool)
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, lookup func(string) (string, bool)) int {
	e := env{stdin: stdin, stdout: stdout, stderr: stderr, lookup: lookup}
	cmd, rest := splitCommand(args)

	var err error
	switch cmd {
	case "serve":
		err = e.serve(ctx, rest)
	case "pair":
		err = e.pair(ctx, rest)
	case "health":
		return e.health(ctx, rest)
	case "status":
		err = e.status(ctx, rest)
	case "unpair":
		err = e.unpair(ctx, rest)
	case "version":
		fmt.Fprintf(stdout, "whatsapp-mcp %s\n", version)
	case "help":
		fmt.Fprint(stdout, usageText)
	default:
		fmt.Fprintf(stderr, "whatsapp-mcp: unknown command %q\n\n%s", redact.Scrub(cmd), usageText)
		return exitUsage
	}
	return e.exitCode(err)
}

func splitCommand(args []string) (cmd string, rest []string) {
	if len(args) == 0 {
		return "serve", nil
	}
	switch first := args[0]; {
	case first == "-h" || first == "-help" || first == "--help":
		return "help", nil
	case strings.HasPrefix(first, "-"):
		return "serve", args
	default:
		return first, args[1:]
	}
}

func (e env) exitCode(err error) int {
	var usage usageError
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		return exitOK
	case errors.As(err, &usage):
		fmt.Fprintf(e.stderr, "whatsapp-mcp: %s\n", redact.Scrub(usage.Error()))
		return exitUsage
	default:
		fmt.Fprintf(e.stderr, "whatsapp-mcp: %s\n", redact.Scrub(err.Error()))
		return exitError
	}
}

// splitExtra pulls subcommand-specific flags out of args so the remainder can
// go to config.Load. extras maps a flag name to whether it takes a value.
func splitExtra(args []string, extras map[string]bool) (rest []string, values map[string]string, err error) {
	values = map[string]string{}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		trimmed, isFlag := strings.CutPrefix(arg, "--")
		if !isFlag {
			trimmed, isFlag = strings.CutPrefix(arg, "-")
		}
		name, value, hasValue := strings.Cut(trimmed, "=")
		takesValue, known := extras[name]
		if !isFlag || !known {
			rest = append(rest, arg)
			continue
		}
		switch {
		case hasValue:
		case takesValue:
			if i+1 >= len(args) {
				return nil, nil, usageError{fmt.Errorf("flag --%s needs a value", name)}
			}
			i++
			value = args[i]
		default:
			value = "true"
		}
		values[name] = value
	}
	return rest, values, nil
}

func hasDataDirArg(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		name := strings.TrimLeft(a, "-")
		if strings.HasPrefix(a, "-") && (name == "data-dir" || strings.HasPrefix(name, "data-dir=")) {
			return true
		}
	}
	return false
}

func (e env) loadConfig(name string, args []string) (config.Config, *slog.Logger, error) {
	home, _ := userHomeDir()
	if home == "" {
		if v, ok := e.lookup(config.EnvDataDir); !(ok && v != "") && !hasDataDirArg(args) {
			return config.Config{}, nil, usageError{fmt.Errorf("cannot determine the home directory; set %s or --data-dir", config.EnvDataDir)}
		}
	}
	out := &scrubWriter{w: e.stderr}
	cfg, err := config.Load(name, args, e.lookup, home, out)
	_ = out.Flush()
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return config.Config{}, nil, err
		}
		return config.Config{}, nil, usageError{err}
	}
	if name != "status" {
		for _, dir := range []string{cfg.DataDir, cfg.DownloadDir, cfg.OutboxDir} {
			if err := datadir.EnsureDir(dir); err != nil {
				return config.Config{}, nil, err
			}
		}
	}
	log := slog.New(slog.NewTextHandler(e.stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	return cfg, log, nil
}

// terminalOrNil returns w when it is an interactive terminal, else nil.
func terminalOrNil(w io.Writer) io.Writer {
	if isTerminal(w) {
		return w
	}
	return nil
}

type resources struct {
	notifier *notify.Notifier
	client   clientCloser
	store    *store.Store
	session  *session.Session
	log      *slog.Logger
}

func openResources(ctx context.Context, cfg config.Config, log *slog.Logger, terminal io.Writer) (*resources, error) {
	st, err := store.Open(cfg.MessagesDBPath())
	if err != nil {
		return nil, fmt.Errorf("open message store: %w", err)
	}
	client, err := openClient(ctx, cfg.SessionDBPath(), log)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("open whatsapp session: %w", err)
	}
	if sender, ok := client.(interface{ SetSendTimeout(time.Duration) }); ok {
		sender.SetSendTimeout(cfg.SendTimeout)
	}
	if owner, ok := client.(interface{ SetInstanceOwner(bool) }); ok {
		owner.SetInstanceOwner(true)
	}
	notifier, err := notify.New(ctx, notify.Options{ConfigPath: cfg.WebhookConfig, DataDir: cfg.DataDir, StartedAt: processStartedAt, Log: log, Resolve: func(ctx context.Context, m wa.Message) notify.Details {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		chat, sender, pn := tools.MessageNames(ctx, client, st, m)
		return notify.Details{ChatName: chat, SenderName: sender, PN: pn, AccountJID: client.Status().AccountJID}
	}})
	if err != nil {
		client.Close()
		st.Close()
		return nil, fmt.Errorf("open notifier: %w", err)
	}
	sess := session.New(session.Options{
		Notify: notifier,
		Client: client,
		Store:  st,
		QR:     qr.Presenter{PNGPath: cfg.QRPNGPath(), Terminal: terminal},
		Log:    log,
		Clock:  ratelimit.SystemClock{},
	})
	return &resources{notifier: notifier, client: client, store: st, session: sess, log: log}, nil
}

func (r *resources) close() {
	r.notifier.Close()
	r.client.Disconnect()
	if err := r.client.Close(); err != nil {
		r.log.Warn("close whatsapp session", "error", redact.Scrub(err.Error()))
	}
	if err := r.store.Close(); err != nil {
		r.log.Warn("close message store", "error", redact.Scrub(err.Error()))
	}
}

func (e env) serve(ctx context.Context, args []string) error {
	cfg, log, err := e.loadConfig("serve", args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if cfg.Transport == config.TransportStdio {
		remote, err := upstream.Load(cfg.DataDir, e.lookup)
		if err != nil {
			return err
		}
		if remote != nil {
			srv, p := server.NewUpstreamProxy(ctx, version, *remote)
			defer p.Close()
			log.Info("serving MCP upstream proxy on stdio")
			return server.RunStdio(ctx, srv, io.NopCloser(e.stdin), nopWriteCloser{e.stdout})
		}
	}
	lock, lockErr := instance.Acquire(cfg.DataDir)
	if cfg.Transport == config.TransportStdio {
		_, discoveryErr := os.Lstat(daemon.Path(cfg.DataDir))
		if errors.Is(lockErr, instance.ErrHeld) || (lockErr == nil && !errors.Is(discoveryErr, os.ErrNotExist)) {
			if lock != nil {
				lock.Close()
			}
			return e.proxy(ctx, cfg, log)
		}
	}
	if lockErr != nil {
		return lockErr
	}
	defer lock.Close()
	allowlist, err := sendpolicy.ParseAllowlist(cfg.Allowlist)
	if err != nil {
		return usageError{err}
	}
	var httpToken string
	if cfg.Transport == config.TransportHTTP {
		if httpToken, err = resolveHTTPToken(cfg, log); err != nil {
			return err
		}
	}
	var ln net.Listener
	if cfg.Transport == config.TransportHTTP {
		ln, err = server.ListenFallback(cfg.HTTPAddr, cfg.HTTPPortFallback, log)
		if err != nil {
			return err
		}
		defer ln.Close()
	}
	terminal := terminalOrNil(e.stderr)
	if v, _ := e.lookup("WHATSAPP_MCP_LOG_QR"); v == "1" {
		terminal = e.stderr
	}
	res, err := openResources(ctx, cfg, log, terminal)
	if err != nil {
		return err
	}
	defer res.close()

	deps := tools.Deps{
		Notifier: res.notifier,
		Client:   res.client,
		Store:    res.store,
		Session:  res.session,
		Policy: &sendpolicy.Policy{
			Enabled:   cfg.SendEnabled,
			Allowlist: allowlist,
			Limiter:   ratelimit.New(cfg.RateInterval, cfg.RateHourly, ratelimit.SystemClock{}),
		},
		SendEnabled:     cfg.SendEnabled,
		DataDir:         cfg.DataDir,
		DownloadDir:     cfg.DownloadDir,
		OutboxDir:       cfg.OutboxDir,
		Log:             log,
		RateInterval:    cfg.RateInterval,
		RateHourly:      cfg.RateHourly,
		AllowlistActive: !allowlist.Empty(),
	}
	srv := server.NewMCPServer(version, deps)

	if !res.client.Paired() {
		log.Info("WhatsApp not paired; call show_pairing_qr or run `whatsapp-mcp pair`", "qr_png", cfg.QRPNGPath())
	}
	go func() {
		if err := res.client.Connect(ctx); err != nil && ctx.Err() == nil {
			log.Warn("whatsapp connect failed", "error", redact.Scrub(err.Error()))
		}
	}()

	switch cfg.Transport {
	case config.TransportHTTP:
		if config.IsLoopbackHTTPAddr(cfg.HTTPAddr) {
			tokenFile := cfg.HTTPTokenFile
			if tokenFile == "" {
				tokenFile = cfg.HTTPTokenPath()
				if cfg.HTTPToken != "" {
					tokenFile = filepath.Join(cfg.DataDir, "daemon-http-token")
				}
				if httpToken != "" {
					if err := datadir.WriteFileAtomic(tokenFile, []byte(httpToken+"\n")); err != nil {
						ln.Close()
						return err
					}
				}
			}
			tokenFile, _ = filepath.Abs(tokenFile)
			info := daemon.Info{HTTPAddr: ln.Addr().String(), TokenFile: tokenFile}
			if cfg.DaemonStartCommand != "" {
				info.StartCommand = []string{cfg.DaemonStartCommand}
			}
			if dir, ok := e.lookup("WHATSAPP_MCP_LOG_DIR"); ok {
				info.LogPath = filepath.Join(dir, "daemon.log")
			}
			if err := daemon.Write(cfg.DataDir, info); err != nil {
				ln.Close()
				return err
			}
		}
		log.Info("listening on http://" + ln.Addr().String() + "/mcp")
		err = server.Serve(ctx, ln, server.NewHTTPHandler(srv, httpToken, func() bool { return string(res.session.Status().State) == "connected" }))
	default:
		log.Info("serving MCP on stdio", "version", version)
		err = server.RunStdio(ctx, srv, io.NopCloser(e.stdin), nopWriteCloser{e.stdout})
	}
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (e env) pair(ctx context.Context, args []string) error {
	rest, extra, err := splitExtra(args, map[string]bool{"wait": true})
	if err != nil {
		return err
	}
	wait := defaultPairWait
	if v, ok := extra["wait"]; ok {
		if wait, err = time.ParseDuration(v); err != nil || wait < 0 {
			return usageError{fmt.Errorf("invalid --wait %q: want a non-negative duration such as 2m", v)}
		}
	}
	cfg, log, err := e.loadConfig("pair", rest)
	if err != nil {
		return err
	}
	lock, err := instance.Acquire(cfg.DataDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	res, err := openResources(ctx, cfg, log, terminalOrNil(e.stdout))
	if err != nil {
		return err
	}
	defer res.close()

	if res.client.Paired() {
		fmt.Fprintf(e.stdout, "already paired as %s\n", redact.JID(res.client.Status().AccountJID))
		return nil
	}
	fmt.Fprintf(e.stdout, "Scan the QR code with WhatsApp > Settings > Linked devices > Link a device. PNG: %s\n", cfg.QRPNGPath())
	if err := res.client.Connect(ctx); err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	pairCtx, cancel := context.WithTimeout(ctx, pairTimeout)
	defer cancel()
	if _, err := res.session.WaitPaired(pairCtx); err != nil {
		if ctx.Err() != nil {
			return errors.New("pairing cancelled")
		}
		if errors.Is(err, session.ErrQRExpired) {
			return errors.New("QR code expired; run `whatsapp-mcp pair` again")
		}
		return fmt.Errorf("pairing did not complete within %s; run pair again", pairTimeout)
	}
	fmt.Fprintln(e.stdout, "Paired.")

	if wait > 0 {
		fmt.Fprintf(e.stdout, "Ingesting message history for %s; press Ctrl+C to stop early.\n", wait)
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
	}
	return nil
}

func (e env) status(ctx context.Context, args []string) error {
	cfg, _, err := e.loadConfig("status", args)
	if err != nil {
		return err
	}
	st, err := readStatus(ctx, cfg.SessionDBPath())
	if err != nil {
		return fmt.Errorf("open whatsapp session: %w", err)
	}

	if st.Paired {
		fmt.Fprintf(e.stdout, "paired: yes (%s)\n", redact.JID(st.AccountJID))
	} else {
		fmt.Fprintln(e.stdout, "paired: no")
	}
	fmt.Fprintf(e.stdout, "data dir: %s\n", cfg.DataDir)
	fmt.Fprintf(e.stdout, "session db: %s\n", cfg.SessionDBPath())
	fmt.Fprintf(e.stdout, "messages db: %s\n", cfg.MessagesDBPath())
	return nil
}

func (e env) unpair(ctx context.Context, args []string) error {
	rest, extra, err := splitExtra(args, map[string]bool{"delete-data": false, "force": false})
	if err != nil {
		return err
	}
	deleteData, err := boolFlag(extra, "delete-data")
	if err != nil {
		return err
	}
	force, err := boolFlag(extra, "force")
	if err != nil {
		return err
	}
	cfg, log, err := e.loadConfig("unpair", rest)
	if err != nil {
		return err
	}
	lock, err := instance.Acquire(cfg.DataDir)
	if err != nil {
		return err
	}
	defer lock.Close()
	loggedOut, logoutErr := e.logout(ctx, cfg, log)
	if deleteData {
		if loggedOut || force {
			if err := removeData(e.stdout, cfg); err != nil {
				return errors.Join(logoutErr, err)
			}
		} else {
			e.reportKeptData(cfg)
		}
	}
	return logoutErr
}

func boolFlag(extra map[string]string, name string) (bool, error) {
	v, ok := extra[name]
	if !ok {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, usageError{fmt.Errorf("invalid --%s %q", name, v)}
	}
	return b, nil
}

func (e env) reportKeptData(cfg config.Config) {
	fmt.Fprintf(e.stdout, "kept all local data (%s, %s, downloads and outbox): the logout did not succeed\n",
		cfg.SessionDBPath(), cfg.MessagesDBPath())
	fmt.Fprintln(e.stdout, "fix the problem and run unpair again, or pass --force to delete the data anyway")
}

// logout unlinks the device. loggedOut is true when the device is no longer
// linked: either the logout succeeded or the session was never paired.
func (e env) logout(ctx context.Context, cfg config.Config, log *slog.Logger) (loggedOut bool, err error) {
	client, err := openClient(ctx, cfg.SessionDBPath(), log)
	if err != nil {
		return false, fmt.Errorf("open whatsapp session: %w", err)
	}
	defer func() {
		client.Disconnect()
		_ = client.Close()
	}()

	if !client.Paired() {
		fmt.Fprintln(e.stdout, "not paired; nothing to log out")
		return true, nil
	}
	connectCtx, cancelConnect := context.WithTimeout(ctx, unpairConnectTimeout)
	defer cancelConnect()
	if err := client.Connect(connectCtx); err != nil {
		e.warnLogoutFailed(err)
	} else if !waitConnected(connectCtx, client) {
		fmt.Fprintln(e.stdout, "warning: could not connect to WhatsApp in time; attempting logout anyway")
	}

	logoutCtx, cancelLogout := context.WithTimeout(ctx, unpairLogoutTimeout)
	defer cancelLogout()
	if err := client.Logout(logoutCtx); err != nil {
		e.warnLogoutFailed(err)
		return false, errors.New("logout failed; the device may still be linked")
	}
	fmt.Fprintln(e.stdout, "logged out; the device was removed from your linked devices")
	return true, nil
}

func (e env) warnLogoutFailed(err error) {
	fmt.Fprintf(e.stdout, "warning: logout failed: %s\n", redact.Scrub(err.Error()))
	fmt.Fprintln(e.stdout, "the device may still be listed under WhatsApp > Linked devices; remove it there")
}

func waitConnected(ctx context.Context, c wa.Client) bool {
	ticker := time.NewTicker(unpairPoll)
	defer ticker.Stop()
	for {
		if c.Status().State == wa.StateConnected {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func removeData(w io.Writer, cfg config.Config) error {
	var errs []error
	files := []string{cfg.SessionDBPath(), cfg.MessagesDBPath(), cfg.QRPNGPath(), filepath.Join(cfg.DataDir, "notifier.db")}
	for _, db := range []string{cfg.SessionDBPath(), cfg.MessagesDBPath()} {
		files = append(files, db+"-wal", db+"-shm")
	}
	for _, path := range files {
		switch err := os.Remove(path); {
		case err == nil:
			fmt.Fprintf(w, "removed %s\n", path)
		case !errors.Is(err, os.ErrNotExist):
			errs = append(errs, err)
		}
	}
	for _, dir := range []string{cfg.DownloadDir, cfg.OutboxDir} {
		errs = append(errs, removeTreeInside(w, cfg.DataDir, dir))
	}
	return errors.Join(errs...)
}

func removeTreeInside(w io.Writer, root, dir string) error {
	resolved, err := filepath.EvalSymlinks(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(resolvedRoot, resolved)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		fmt.Fprintf(w, "kept %s (outside the data dir)\n", dir)
		return nil
	}
	if err := os.RemoveAll(resolved); err != nil {
		return err
	}
	fmt.Fprintf(w, "removed %s\n", dir)
	return nil
}
