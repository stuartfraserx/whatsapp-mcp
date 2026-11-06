#!/usr/bin/env bash
set -euo pipefail
umask 077
root=$(cd "$(dirname "$0")/.." && pwd)
bash -n "$root/scripts/whatsapp-mcp-watchdog.sh" "$root/scripts/whatsapp-mcp-supervise.sh"
if command -v shellcheck >/dev/null; then shellcheck "$root/scripts/whatsapp-mcp-watchdog.sh" "$root/scripts/whatsapp-mcp-supervise.sh"; fi
for dependency in jq flock ss; do command -v "$dependency" >/dev/null || { echo "missing $dependency" >&2; exit 1; }; done
tmp=$(mktemp -d)
sleep 300 &
pid=$!
trap 'kill "$pid" 2>/dev/null || :; rm -rf "$tmp"' EXIT
mkdir -p "$tmp/data" "$tmp/logs" "$tmp/bin"
printf '%s\n' "$pid" > "$tmp/logs/daemon.pid"
printf '{"http_addr":"127.0.0.1:18765"}\n' > "$tmp/data/daemon.json"
# Socket inspection is deterministic and never opens a real listener.
printf '#!/usr/bin/env bash\nprintf '\''LISTEN 0 128 127.0.0.1:18765 0.0.0.0:* users:(("sleep",pid=%s,fd=3))\\n'\''\n' "$pid" > "$tmp/bin/ss"
chmod +x "$tmp/bin/ss"
printf '#!/usr/bin/env bash\ncat "$HEALTH_FILE"\n' > "$tmp/health"
chmod +x "$tmp/health"
export PATH="$tmp/bin:$PATH" HEALTH_FILE="$tmp/health.json"
export WHATSAPP_MCP_DATA_DIR="$tmp/data" WHATSAPP_MCP_LOG_DIR="$tmp/logs"
export WHATSAPP_MCP_HEALTH_CMD="$tmp/health" WHATSAPP_MCP_WATCHDOG_INTERVAL=1 WHATSAPP_MCP_WATCHDOG_RECHECK=1 WHATSAPP_MCP_WATCHDOG_ONESHOT=1 WHATSAPP_MCP_WATCHDOG_GRACE=0
export WHATSAPP_MCP_ALERT_WEBHOOK_CONFIG="$tmp/missing"
health() { printf '{"ok":%s,"state":"%s","paired":true,"webhook_failed":%s}\n' "$1" "$2" "$3" > "$HEALTH_FILE"; }
cycle() { bash "$root/scripts/whatsapp-mcp-watchdog.sh"; kill -0 "$pid"; }
health true connected 0
cycle
[[ ! -f "$tmp/logs/watchdog.log" ]] || ! grep -q whatsapp_daemon_down "$tmp/logs/watchdog.log"
health false disconnected 0
cycle; cycle; cycle
[[ $(grep -c '"event":"whatsapp_daemon_down"' "$tmp/logs/watchdog.log") == 1 ]]
grep -q 'no alert webhook configured' "$tmp/logs/watchdog.log"
health true connected 0
cycle
[[ $(grep -c '"event":"whatsapp_daemon_recovered"' "$tmp/logs/watchdog.log") == 1 ]]
printf '{"url":"https://127.0.0.1:1/","authorization_header":"Authorization: Bearer FAKE_SECRET_NEVER_LOG"}\n' > "$tmp/alert.json"
export WHATSAPP_MCP_ALERT_WEBHOOK_CONFIG="$tmp/alert.json"
health false disconnected 0
cycle
grep -q 'alert failed' "$tmp/logs/watchdog.log"
health true connected 0
cycle
# A failure increase alone must cause an outage, including after the recheck.
health true connected 1
cycle
[[ $(grep -c '"event":"whatsapp_daemon_down"' "$tmp/logs/watchdog.log") == 3 ]]
grep -q 'webhook failures increased' "$tmp/logs/watchdog.log"
! grep -q FAKE_SECRET_NEVER_LOG "$tmp/logs/watchdog.log"
# A fresh daemon has command arguments that would permit TERM without grace.
kill "$pid"
wait "$pid" 2>/dev/null || :
bash -c 'trap "exit 0" TERM; while :; do sleep 0.1; done' daemon serve --transport http &
pid=$!
printf '%s\n' "$pid" > "$tmp/logs/daemon.pid"
printf '#!/usr/bin/env bash\nprintf '\''LISTEN 0 128 127.0.0.1:18765 0.0.0.0:* users:(("bash",pid=%s,fd=3))\\n'\''\n' "$pid" > "$tmp/bin/ss"
rm -f "$tmp/logs/watchdog.state" "$tmp/logs/watchdog.log"
# Stub only the watchdog's startup pause so a long grace needs no real wait.
printf '#!/usr/bin/env bash\nprintf "%%s\\n" "$1" >> "$HEALTH_FILE.sleeps"\n' > "$tmp/bin/sleep"
chmod +x "$tmp/bin/sleep"
health false connecting 0
WHATSAPP_MCP_WATCHDOG_GRACE=3600 cycle
grep -q '^3600$' "$HEALTH_FILE.sleeps"
grep -q 'starting:' "$tmp/logs/watchdog.log"
! grep -Eq 'unhealthy|restart requested|whatsapp_daemon_down' "$tmp/logs/watchdog.log"
printf '{"ok":false,"state":"connected","paired":false,"webhook_failed":0}\n' > "$HEALTH_FILE"
WHATSAPP_MCP_WATCHDOG_GRACE=3600 cycle
! grep -Eq 'unhealthy|restart requested|whatsapp_daemon_down' "$tmp/logs/watchdog.log"
rm "$tmp/bin/sleep"
health true connected 0
# Verify preferred-port lookup separately from the published fallback listener.
cp "$tmp/bin/ss" "$tmp/bin/ss-daemon"
cat > "$tmp/bin/ss" <<'SS'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$HEALTH_FILE.ss"
exec "$(dirname "$0")/ss-daemon"
SS
printf 'HTTP ports busy; using fallback: address already in use\n' > "$tmp/logs/daemon.log"
WHATSAPP_MCP_HTTP_ADDR=127.0.0.1:19999 cycle
cycle
grep -q 'sport = :19999' "$HEALTH_FILE.ss"
[[ $(grep -c 'preferred port busy; fallback in use' "$tmp/logs/watchdog.log") == 1 ]]
grep -q "preferred port busy; fallback in use: pid=$pid cmd=" "$tmp/logs/watchdog.log"
! grep -Eq 'unhealthy|restart requested|whatsapp_daemon_down|alert' "$tmp/logs/watchdog.log"
echo 'watchdog tests passed' 
