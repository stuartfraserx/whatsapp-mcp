#!/usr/bin/env bash
# A private KEY=VALUE file is parsed as data, never sourced as shell code.
set -eu
umask 077

env_file=${WHATSAPP_MCP_ENV_FILE:-$HOME/.config/whatsapp-mcp-laura/env}
if [[ ! -f "$env_file" || -L "$env_file" || $(stat -c '%a' "$env_file") != 600 ]]; then
    echo "supervisor env file must be a regular file with mode 0600" >&2
    exit 1
fi
while IFS= read -r line || [[ -n "$line" ]]; do
    case "$line" in ''|'#'*) continue ;; esac
    key=${line%%=*}
    value=${line#*=}
    case "$key" in
        WHATSAPP_MCP_HTTP_PORT_FALLBACK|WHATSAPP_MCP_WATCHDOG|WHATSAPP_MCP_WATCHDOG_INTERVAL|WHATSAPP_MCP_WATCHDOG_RECHECK|WHATSAPP_MCP_WATCHDOG_GRACE|WHATSAPP_MCP_ALERT_WEBHOOK_CONFIG|WHATSAPP_MCP_WATCHDOG_DISABLE|WHATSAPP_MCP_DATA_DIR|WHATSAPP_MCP_HTTP_ADDR|WHATSAPP_MCP_HTTP_TOKEN_FILE|WHATSAPP_MCP_WEBHOOK_CONFIG|WHATSAPP_MCP_BIN|WHATSAPP_MCP_LOG_DIR|WHATSAPP_MCP_DAEMON_START_COMMAND)
            export "$key=$value" ;;
        *) echo "unsupported key in supervisor env file" >&2; exit 1 ;;
    esac
done < "$env_file"
: "${WHATSAPP_MCP_DATA_DIR:?set WHATSAPP_MCP_DATA_DIR}"
: "${WHATSAPP_MCP_BIN:?set WHATSAPP_MCP_BIN}"
log_dir=${WHATSAPP_MCP_LOG_DIR:-$WHATSAPP_MCP_DATA_DIR/logs}
export WHATSAPP_MCP_LOG_DIR=$log_dir
mkdir -p "$log_dir"
script=$(readlink -f "$0")
export WHATSAPP_MCP_DAEMON_START_COMMAND=${WHATSAPP_MCP_DAEMON_START_COMMAND:-$script}
export WHATSAPP_MCP_ENV_FILE=$env_file
pid_file=$log_dir/supervisor.pid
child_file=$log_dir/daemon.pid
watchdog_file=$log_dir/watchdog.pid
watchdog_loop_file=$log_dir/watchdog-loop.pid
watchdog=${WHATSAPP_MCP_WATCHDOG:-$(dirname "$script")/whatsapp-mcp-watchdog}
if [[ ! -e "$watchdog" && -z ${WHATSAPP_MCP_WATCHDOG:-} ]]; then watchdog=$watchdog.sh; fi
export WHATSAPP_MCP_WATCHDOG=$watchdog
export WHATSAPP_MCP_HTTP_PORT_FALLBACK=${WHATSAPP_MCP_HTTP_PORT_FALLBACK:-0}
export WHATSAPP_MCP_WATCHDOG_INTERVAL=${WHATSAPP_MCP_WATCHDOG_INTERVAL:-300}
export WHATSAPP_MCP_WATCHDOG_GRACE=${WHATSAPP_MCP_WATCHDOG_GRACE:-90}
export WHATSAPP_MCP_WATCHDOG_RECHECK=${WHATSAPP_MCP_WATCHDOG_RECHECK:-60}
export WHATSAPP_MCP_ALERT_WEBHOOK_CONFIG=${WHATSAPP_MCP_ALERT_WEBHOOK_CONFIG:-/home/box/secrets/whatsapp-laura-alert-webhook.json}
export WHATSAPP_MCP_WATCHDOG_DISABLE=${WHATSAPP_MCP_WATCHDOG_DISABLE:-0}

running() {
    [[ -f "$1" ]] || return 1
    local pid
    IFS= read -r pid < "$1" || return 1
    case "$pid" in ''|*[!0-9]*) return 1 ;; esac
    kill -0 "$pid" 2>/dev/null
}
case ${1:-} in
    status)
        if running "$pid_file"; then echo 'supervisor: running'; else echo 'supervisor: stopped'; fi
        if running "$child_file"; then echo 'daemon: running'; else echo 'daemon: stopped'; fi
        if running "$watchdog_file"; then echo 'watchdog: running'; else echo 'watchdog: stopped'; fi
        if [[ -f "$WHATSAPP_MCP_DATA_DIR/daemon.json" ]]; then jq -r '"http_addr: " + .http_addr' "$WHATSAPP_MCP_DATA_DIR/daemon.json" 2>/dev/null || :; fi
        exit 0 ;;
    stop)
        if running "$watchdog_loop_file"; then kill "$(cat "$watchdog_loop_file")" 2>/dev/null || :; fi
        if running "$watchdog_file"; then kill "$(cat "$watchdog_file")" 2>/dev/null || :; fi
        if running "$pid_file"; then kill "$(cat "$pid_file")" 2>/dev/null || :; fi
        if running "$child_file"; then kill "$(cat "$child_file")" 2>/dev/null || :; fi
        exit 0 ;;
    '')
        nohup setsid "$script" --foreground </dev/null >>"$log_dir/supervisor.log" 2>&1 &
        exit 0 ;;
    --foreground) ;;
    *) echo "usage: $0 [--foreground|status|stop]" >&2; exit 2 ;;
esac
exec 9>"$log_dir/supervisor.lock"
flock -n 9 || exit 0
printf '%s\n' "$$" > "$pid_file"
child=
sleeper=
watchdog_loop=
stopping=false
terminate() {
    stopping=true
    if [[ -n "$watchdog_loop" ]]; then kill "$watchdog_loop" 2>/dev/null || :; fi
    if [[ -n "$child" ]]; then kill "$child" 2>/dev/null || :; fi
    if [[ -n "$sleeper" ]]; then kill "$sleeper" 2>/dev/null || :; fi
}
cleanup() { if [[ -n "$watchdog_loop" ]]; then wait "$watchdog_loop" 2>/dev/null || :; fi; rm -f "$pid_file" "$child_file" "$watchdog_loop_file"; }
trap terminate TERM INT
trap cleanup EXIT
if [[ "$WHATSAPP_MCP_WATCHDOG_DISABLE" != 1 && -x "$watchdog" ]]; then
    (
        exec 9>&-
        watcher=
        nap=
        finish_watchdog() {
            trap - TERM INT
            [[ -z "$watcher" ]] || kill "$watcher" 2>/dev/null || :
            [[ -z "$nap" ]] || kill "$nap" 2>/dev/null || :
            [[ -z "$watcher" ]] || wait "$watcher" 2>/dev/null || :
            exit 0
        }
        trap finish_watchdog TERM INT
        while :; do
            "$watchdog" &
            watcher=$!
            wait "$watcher" || :
            watcher=
            sleep 10 &
            nap=$!
            wait "$nap" || :
            nap=
        done
    ) &
    watchdog_loop=$!
    printf '%s\n' "$watchdog_loop" > "$watchdog_loop_file"
fi
backoff=2
while ! $stopping; do
    log=$log_dir/daemon.log
    if [[ -f "$log" && $(wc -c < "$log") -gt 10485760 ]]; then mv -f "$log" "$log.1"; fi
    started=$(date +%s)
    "$WHATSAPP_MCP_BIN" serve --transport http >>"$log" 2>&1 9>&- &
    child=$!
    printf '%s\n' "$child" > "$child_file"
    wait "$child" || :
    if $stopping; then wait "$child" 2>/dev/null || :; break; fi
    child=
    rm -f "$child_file"
    uptime=$(( $(date +%s) - started ))
    if (( uptime >= 300 )); then backoff=2; fi
    sleep "$backoff" 9>&- &
    sleeper=$!
    wait "$sleeper" || :
    sleeper=
    backoff=$((backoff * 2))
    if (( backoff > 60 )); then backoff=60; fi
done
