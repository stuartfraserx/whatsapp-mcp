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
        WHATSAPP_MCP_DATA_DIR|WHATSAPP_MCP_HTTP_ADDR|WHATSAPP_MCP_HTTP_TOKEN_FILE|WHATSAPP_MCP_WEBHOOK_CONFIG|WHATSAPP_MCP_BIN|WHATSAPP_MCP_LOG_DIR|WHATSAPP_MCP_DAEMON_START_COMMAND)
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
        exit 0 ;;
    stop)
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
stopping=false
terminate() {
    stopping=true
    if [[ -n "$child" ]]; then kill "$child" 2>/dev/null || :; fi
    if [[ -n "$sleeper" ]]; then kill "$sleeper" 2>/dev/null || :; fi
}
cleanup() { rm -f "$pid_file" "$child_file"; }
trap terminate TERM INT
trap cleanup EXIT
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
