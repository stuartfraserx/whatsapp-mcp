//go:build linux

package repotest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSupervisorWithFakeBinary(t *testing.T) {
	for _, name := range []string{"bash", "flock", "setsid", "nohup", "stat", "readlink"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s unavailable", name)
		}
	}
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	envFile := filepath.Join(dir, "supervisor-env")
	fake := filepath.Join(dir, "fake-binary")
	if err := os.WriteFile(fake, []byte("#!/usr/bin/env bash\ntrap 'exit 0' TERM INT\nif [[ $* != 'serve --transport http' ]]; then exit 2; fi\nwhile :; do sleep 0.1; done\n"), 0700); err != nil {
		t.Fatal(err)
	}
	watchdog := filepath.Join(dir, "fake-watchdog")
	if err := os.WriteFile(watchdog, []byte(`#!/usr/bin/env bash
set -u
[[ "$WHATSAPP_MCP_WATCHDOG_GRACE" == 17 ]] || exit 2
printf '%s\n' "$$" > "$WHATSAPP_MCP_LOG_DIR/watchdog.pid"
trap 'rm -f "$WHATSAPP_MCP_LOG_DIR/watchdog.pid"; exit 0' TERM INT
while :; do sleep 0.1; done
`), 0700); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(dir, "fake-state")
	env := []byte("WHATSAPP_MCP_DATA_DIR=" + dataDir + "\nWHATSAPP_MCP_BIN=" + fake + "\nWHATSAPP_MCP_LOG_DIR=" + logDir + "\nWHATSAPP_MCP_WATCHDOG=" + watchdog + "\nWHATSAPP_MCP_WATCHDOG_GRACE=17\n")
	if err := os.WriteFile(envFile, env, 0600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repoRoot(t), "scripts", "whatsapp-mcp-supervise.sh")
	command := func(args ...string) *exec.Cmd {
		c := exec.Command(script, args...)
		c.Env = append(os.Environ(), "WHATSAPP_MCP_ENV_FILE="+envFile)
		return c
	}
	var output bytes.Buffer
	cmd := command("--foreground")
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				cmd.Process.Kill()
				<-done
			}
		}
	})
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(logDir, "daemon.pid")); err == nil {
			if _, err := os.Stat(filepath.Join(logDir, "watchdog.pid")); err != nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor did not start fake daemon")
		}
		time.Sleep(10 * time.Millisecond)
	}
	out, err := command("status").CombinedOutput()
	if err != nil || !strings.Contains(string(out), "supervisor: running") || !strings.Contains(string(out), "daemon: running") || !strings.Contains(string(out), "watchdog: running") {
		t.Fatalf("status: %s %v", out, err)
	}
	out, err = command("--foreground").CombinedOutput()
	if err != nil || len(out) != 0 {
		t.Fatalf("second supervisor: %s %v", out, err)
	}
	if out, err = command("stop").CombinedOutput(); err != nil {
		t.Fatalf("stop: %s %v", out, err)
	}
	select {
	case err := <-done:
		stopped = true
		if err != nil {
			t.Fatalf("supervisor: %v %s", err, output.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not stop")
	}
	for _, name := range []string{"supervisor.pid", "daemon.pid", "watchdog.pid", "watchdog-loop.pid"} {
		if _, err := os.Stat(filepath.Join(logDir, name)); !os.IsNotExist(err) {
			t.Fatalf("stale %s", name)
		}
	}
	os.Chmod(envFile, 0644)
	if err := command("status").Run(); err == nil {
		t.Fatal("accepted public env file")
	}
}
