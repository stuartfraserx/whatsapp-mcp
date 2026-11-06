#!/usr/bin/env bash
set -u
umask 077
: "${WHATSAPP_MCP_DATA_DIR:?set WHATSAPP_MCP_DATA_DIR}"
bin=${WHATSAPP_MCP_BIN:-whatsapp-mcp}
log_dir=${WHATSAPP_MCP_LOG_DIR:-$WHATSAPP_MCP_DATA_DIR/logs}
interval=${WHATSAPP_MCP_WATCHDOG_INTERVAL:-300}
recheck=${WHATSAPP_MCP_WATCHDOG_RECHECK:-60}
grace=${WHATSAPP_MCP_WATCHDOG_GRACE:-90}
alert_config=${WHATSAPP_MCP_ALERT_WEBHOOK_CONFIG:-/home/box/secrets/whatsapp-laura-alert-webhook.json}
mkdir -p "$log_dir"
exec 8>"$log_dir/watchdog.lock"
flock -n 8 || exit 0
printf '%s\n' "$$" > "$log_dir/watchdog.pid"
sleeper=
terminate() {
    if [[ -n "$sleeper" ]]; then kill "$sleeper" 2>/dev/null || :; fi
    exit 0
}
trap terminate TERM INT
trap 'rm -f "$log_dir/watchdog.pid"' EXIT
pause() {
    sleep "$1" 8>&- &
    sleeper=$!
    wait "$sleeper" || :
    sleeper=
}
log() {
    local file=$log_dir/watchdog.log
    if [[ -f "$file" ]] && (( $(stat -c '%s' "$file") >= 5242880 )); then mv -f "$file" "$file.1"; fi
    printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$*" >> "$file"
}
outage=0 restarted=0 alerted=0 since= last_failed=0 log_offset=0 log_inode=0
if [[ -f "$log_dir/watchdog.state" && ! -L "$log_dir/watchdog.state" ]]; then
    while IFS='=' read -r key value; do
        case "$key" in
            outage|restarted|alerted) [[ "$value" =~ ^[01]$ ]] && printf -v "$key" '%s' "$value" ;;
            last_failed|log_offset|log_inode) [[ "$value" =~ ^[0-9]+$ ]] && printf -v "$key" '%s' "$value" ;;
            since) [[ "$value" =~ ^[0-9TZ:-]+$ ]] && since=$value ;;
        esac
    done < "$log_dir/watchdog.state"
fi
save() {
    printf 'outage=%s\nrestarted=%s\nalerted=%s\nsince=%s\nlast_failed=%s\nlog_offset=%s\nlog_inode=%s\n' "$outage" "$restarted" "$alerted" "$since" "$last_failed" "$log_offset" "$log_inode" > "$log_dir/watchdog.state.tmp"
    mv -f "$log_dir/watchdog.state.tmp" "$log_dir/watchdog.state"
}
# No config content is ever evaluated by the shell or passed in argv.
alert() {
    local event=$1 code config
    log "$event"
    if [[ ! -f "$alert_config" || -L "$alert_config" ]] ||
       [[ $(stat -c '%a:%u' "$alert_config" 2>/dev/null) != "600:$(id -u)" ]] ||
       ! jq -e '(.url | type == "string" and startswith("https://") and (test("[\\r\\n]")|not)) and (.authorization_header | type == "string" and length > 0 and (test("[\\r\\n]")|not))' "$alert_config" >/dev/null 2>&1; then
        log 'alert (not sent: no alert webhook configured)'
        return
    fi
    # jq's JSON string escaping also safely quotes curl config values.
    config=$(jq -r '"url = " + (.url|tojson), "header = " + (.authorization_header|tojson)' "$alert_config")
    if code=$(printf '%s\n' "$config" | curl -q -K - --max-time 15 -sS -o /dev/null -w '%{http_code}' --header 'Content-Type: application/json' --data-binary "$event" 2>/dev/null); then
        log "alert sent http=$code"
    else
        log 'alert failed'
    fi
}
# Use the same listener/process inspection for failures and fallback notices.
listener_owner() {
    local sockets=$1 owner cmd
    if [[ "$sockets" =~ pid=([0-9]+) ]]; then
        owner=${BASH_REMATCH[1]}
        cmd=$(tr '\0' ' ' < "/proc/$owner/cmdline" 2>/dev/null) || cmd=
        printf 'pid=%s cmd=%.120s' "$owner" "$cmd"
    else
        printf 'unknown'
    fi
}
probe() {
    reason= port_owner= state= paired=false healthy=true
    local pid= addr= port= sockets= health= failed=0 bytes=0 inode=0 age= young=false new_log= keepalive_count=0 preferred= busy_line= busy_owner= health_ok=false
    if [[ -f "$log_dir/daemon.pid" ]]; then IFS= read -r pid < "$log_dir/daemon.pid" || :; fi
    if [[ ! "$pid" =~ ^[1-9][0-9]*$ ]] || ! kill -0 "$pid" 2>/dev/null; then
        healthy=false; reason+='daemon pid is not alive; '
    else
        age=$(ps -o etimes= -p "$pid" 2>/dev/null) || age=
        age=${age//[[:space:]]/}
        if [[ $age =~ ^[0-9]+$ ]] && (( age < grace )); then young=true; fi
    fi
    addr=$(jq -r '.http_addr // empty' "$WHATSAPP_MCP_DATA_DIR/daemon.json" 2>/dev/null) || addr=
    if [[ "$addr" =~ ^127\.0\.0\.1:([0-9]+)$ ]]; then
        port=${BASH_REMATCH[1]}
        sockets=$(ss -ltnpH "sport = :$port" 2>/dev/null) || sockets=
        if [[ -z "$sockets" ]]; then
            healthy=false; reason+='daemon address is not listening; '
        elif [[ ! "$pid" =~ ^[1-9][0-9]*$ ]] || [[ "$sockets" != *"pid=$pid,"* ]]; then
            healthy=false; reason+='daemon port belongs to another or unknown process; '
            port_owner=$(listener_owner "$sockets")
        fi
    else
        healthy=false; reason+='invalid or missing daemon address; '
    fi
    if [[ -n ${WHATSAPP_MCP_HEALTH_CMD:-} ]]; then
        health=$(bash -c "$WHATSAPP_MCP_HEALTH_CMD" 8>&- 2>/dev/null) || :
    else
        health=$("$bin" health --data-dir "$WHATSAPP_MCP_DATA_DIR" 8>&- 2>/dev/null) || :
    fi
    if ! jq -e 'type == "object" and (.ok|type == "boolean") and (.paired|type == "boolean") and (.state|type == "string") and (.webhook_failed|type == "number" and . >= 0 and . == floor)' <<< "$health" >/dev/null 2>&1; then
        healthy=false; reason+='invalid health response; '
    else
        state=$(jq -r '.state' <<< "$health")
        paired=$(jq -r '.paired' <<< "$health")
        failed=$(jq -r '.webhook_failed' <<< "$health")
        health_ok=$(jq -r '.ok' <<< "$health")
        if [[ $(jq -r '.ok' <<< "$health") != true ]]; then
            if $young && { [[ "$state" != connected ]] || [[ "$paired" != true ]]; }; then
                log "starting: daemon age=${age}s grace=${grace}s state=$state paired=$paired"
            else
                healthy=false; reason+='health reports not connected and paired; '
            fi
        fi
        if (( failed > last_failed )); then healthy=false; reason+='webhook failures increased; '; fi
        last_failed=$failed
    fi
    if [[ -f "$log_dir/daemon.log" ]]; then
        read -r bytes inode < <(stat -c '%s %i' "$log_dir/daemon.log")
        # With no known inode, establish a baseline without replaying history.
        if (( log_inode == 0 )); then
            log_offset=$bytes
        elif (( inode != log_inode || bytes < log_offset )); then
            log_offset=0
        fi
        new_log=$(tail -c "+$((log_offset+1))" "$log_dir/daemon.log" | head -c "$((bytes-log_offset))")
        keepalive_count=$(grep -Fc 'whatsapp keepalive timeout' <<< "$new_log") || :
        if (( keepalive_count >= 2 )); then
            healthy=false
            reason+="repeated whatsapp keepalive timeouts ($keepalive_count); "
        fi
        if grep -q 'HTTP ports busy; using fallback' <<< "$new_log"; then
            preferred=${WHATSAPP_MCP_HTTP_ADDR:-127.0.0.1:8765}
            port=${preferred##*:}
            sockets=
            if [[ "$port" =~ ^[0-9]+$ ]]; then
                sockets=$(ss -ltnpH "sport = :$port" 2>/dev/null) || sockets=
            fi
            log "preferred port busy; fallback in use: $(listener_owner "$sockets")"
        fi
        while IFS= read -r busy_line; do
            [[ ${busy_line,,} == *'address already in use'* ]] || continue
            [[ "$busy_line" == *'HTTP ports busy; using fallback'* ]] && continue
            busy_owner=${port_owner:-unknown}
            if [[ "$busy_line" =~ listen\ on\ 127\.0\.0\.1:([0-9]+) ]]; then
                sockets=$(ss -ltnpH "sport = :${BASH_REMATCH[1]}" 2>/dev/null) || sockets=
                busy_owner=$(listener_owner "$sockets")
            fi
            if $healthy && $health_ok; then
                log "address already in use (daemon healthy); port owner: $busy_owner"
            else
                healthy=false
                port_owner=$busy_owner
                reason+="address already in use; port owner: $port_owner; "
            fi
        done <<< "$new_log"
        log_offset=$bytes log_inode=$inode
    fi
}
event() {
    jq -cn --arg event "$1" --arg reason "$2" --arg state "$state" --argjson paired "$paired" --arg owner "$port_owner" --arg ts "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg since "$since" \
        '{event:$event,reason:$reason,state:$state,paired:$paired,port_owner:$owner,ts:$ts} + (if $event == "whatsapp_daemon_recovered" then {down_since:$since} else {} end)'
}
if [[ ${WHATSAPP_MCP_WATCHDOG_ONESHOT:-0} != 1 || ${WHATSAPP_MCP_WATCHDOG_GRACE+x} ]]; then
    pause "$grace"
fi
while :; do
    baseline_failed=$last_failed
    probe
    if ! $healthy; then
        log "unhealthy: $reason"
        if (( outage == 0 )); then outage=1; since=$(date -u +%Y-%m-%dT%H:%M:%SZ); fi
        if (( restarted == 0 )); then
            restarted=1
            save
            pid=
            IFS= read -r pid < "$log_dir/daemon.pid" 2>/dev/null || :
            if [[ "$pid" =~ ^[1-9][0-9]*$ && -r /proc/$pid/cmdline ]]; then
                args=()
                mapfile -d '' -t args < "/proc/$pid/cmdline"
                has_serve=false has_http=false
                for (( i=1; i<${#args[@]}; i++ )); do
                    [[ ${args[i]} == serve ]] && has_serve=true
                    if [[ ${args[i]} == --transport && ${args[i+1]:-} == http ]]; then has_http=true; fi
                done
                if $has_serve && $has_http; then
                    kill -TERM "$pid" 2>/dev/null || :
                    log 'daemon restart requested'
                else log 'restart skipped: daemon pid command does not match serve http'; fi
            fi
            pause "$recheck"
            last_failed=$baseline_failed
            probe
        fi
        if ! $healthy && (( alerted == 0 )); then
            alert "$(event whatsapp_daemon_down "$reason")"
            alerted=1
        fi
    fi
    if $healthy && (( outage == 1 )); then
        if (( alerted == 1 )); then port_owner=; alert "$(event whatsapp_daemon_recovered 'healthy again')"; fi
        outage=0 restarted=0 alerted=0 since=
        log 'healthy again'
    fi
    save
    [[ ${WHATSAPP_MCP_WATCHDOG_ONESHOT:-0} == 1 ]] && break
    pause "$interval"
done
