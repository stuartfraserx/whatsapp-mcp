# whatsapp-mcp

A [Model Context Protocol](https://modelcontextprotocol.io) server for your
personal WhatsApp account. An MCP client such as OpenBot can list your chats,
search messages, download media, and send messages on your behalf.

- Single Go binary, no runtime to install.
- Connects through [whatsmeow](https://github.com/tulir/whatsmeow) as a linked device (multi-device, QR pairing).
- Stores everything locally in SQLite. No cloud component, no telemetry.

> **Warning.** This project uses whatsmeow, an unofficial WhatsApp client
> library. WhatsApp does not endorse it. Using unofficial clients is against
> WhatsApp's Terms of Service, and WhatsApp can ban the phone number you link.
> By running this software, you accept that risk for your own account. The
> anti-ban measures described below reduce the risk. They are mitigations, not
> guarantees.

## Build

Requires Go 1.26 or newer. The build is pure Go and does not need CGO.

```sh
go build -o whatsapp-mcp ./cmd/whatsapp-mcp
```

## Data directory and configuration

Default data directory: `$HOME/.local/share/whatsapp-mcp`. The server creates it
with mode `0700`. Files inside are created with mode `0600`.

| Env var | Flag | Default | Meaning |
|---|---|---|---|
| `WHATSAPP_MCP_DATA_DIR` | `--data-dir` | `$HOME/.local/share/whatsapp-mcp` | Root for all local state. |
| `WHATSAPP_MCP_DOWNLOAD_DIR` | `--download-dir` | `<data>/downloads` | Where `download_media` writes files. Must not be the data dir or one of its ancestors. |
| `WHATSAPP_MCP_OUTBOX_DIR` | `--outbox-dir` | `<data>/outbox` | Only folder (besides the download dir) that `send_file` may read from. Must not be the data dir or one of its ancestors. |
| `WHATSAPP_MCP_TRANSPORT` | `--transport` | `stdio` | `stdio` or `http`. |
| `WHATSAPP_MCP_HTTP_ADDR` | `--http-addr` | `127.0.0.1:8765` | Listen address for HTTP mode. The host must be exactly `127.0.0.1`. Any other host is refused. |
| `WHATSAPP_MCP_HTTP_TOKEN` | none | empty | Bearer token for HTTP mode. Env only, never a flag. When neither a token nor a token file is set, the server uses an auto-generated token (see [HTTP](#http)). |
| `WHATSAPP_MCP_HTTP_TOKEN_FILE` | none | empty | File containing the bearer token (trimmed). Mutually exclusive with `WHATSAPP_MCP_HTTP_TOKEN`. |
| `WHATSAPP_MCP_HTTP_NO_AUTH` | `--http-no-auth` | `false` | Disable bearer authentication in HTTP mode. Refused when a token or token file is also set. |
| `WHATSAPP_MCP_SEND_ENABLED` | `--send-enabled` | `true` | Set to `false` to remove the send tools entirely. |
| `WHATSAPP_MCP_ALLOWLIST` | `--allowlist` | empty | Comma-separated JIDs or phone numbers. Empty means any recipient. |
| `WHATSAPP_MCP_RATE_INTERVAL` | `--rate-interval` | `3s` | Minimum time between two sends. Must be at least `1s`. |
| `WHATSAPP_MCP_RATE_HOURLY` | `--rate-hourly` | `60` | Maximum sends in any trailing hour. Range 1 to 1000. |
| `WHATSAPP_MCP_WEBHOOK_CONFIG` | `--webhook-config` | empty | Private JSON inbound webhook configuration; empty disables the notifier entirely. |
| `WHATSAPP_MCP_DAEMON_START_COMMAND` | `--daemon-start-command` | empty | Absolute path to the supervisor executable, saved in daemon discovery for automatic startup. |
| `WHATSAPP_MCP_LOG_LEVEL` | `--log-level` | `info` | `debug`, `info`, `warn`, or `error`. |

Flags override environment variables.

The download and outbox directories may be anywhere except the data directory
itself or a directory that contains it. The server refuses to start otherwise,
so `whatsapp.db`, `messages.db`, the HTTP token and the pairing QR can never be
read through `send_file` or overwritten by `download_media`. `send_file` also
refuses any file inside the data directory unless it sits in a subdirectory of
it that is the outbox or the download directory.

## Commands

| Command | What it does |
|---|---|
| `serve` | Run the MCP server (`--transport stdio` or `http`). |
| `pair` | Pair this device by QR code and ingest the first history batches. |
| `status` | Print the pairing and connection state. |
| `unpair [--delete-data] [--force]` | Log the linked device out of WhatsApp, optionally deleting local data. See [Unpair and delete data](#unpair-and-delete-data). |
| `version` | Print the version. |
| `help` | Print usage. |

Files in the data directory:

| Path | Contents |
|---|---|
| `whatsapp.db` (plus `-wal`, `-shm`) | WhatsApp session (linked-device keys). Treat as a secret. |
| `messages.db` (plus `-wal`, `-shm`) | Local copy of chats and messages. |
| `http-token` | Auto-generated HTTP bearer token (mode `0600`). Created on the first HTTP start without a configured token. Treat as a secret. |
| `notifier.db` | Separate persistent webhook dedupe store; `messages.db` keeps its existing schema. |
| `instance.lock` | Single-owner session lock, held by serve, pair and unpair. |
| `daemon.json` | Private HTTP daemon address, token file path, PID, start time and optional start command. Contains no token. |
| `daemon-http-token` | Private proxy token file when HTTP authentication was supplied directly through the environment. |
| `pairing-qr.png` | Pairing QR code. Exists only while pairing is pending. |
| `downloads/` | Media saved by `download_media`. |
| `outbox/` | Files you place here for `send_file`. |

## First-time pairing

```sh
./whatsapp-mcp pair
```

The command prints the QR code in the terminal (when stdout is a TTY) and
writes it to `<data>/pairing-qr.png`. The PNG is created with mode `0600`.

On your phone: **Settings → Linked devices → Link a device**, then scan the code.

- Keep the command running. After pairing, WhatsApp sends your history in
  batches. `pair` keeps ingesting for `--wait` (default `2m`) before it exits.
- QR codes rotate and expire. The PNG is replaced with each new code. Scan the
  newest one.
- The PNG is deleted automatically once pairing succeeds.

You can also pair from an MCP client. Call the `show_pairing_qr` tool. It returns
the path to the PNG, never the QR payload. Open that file locally and scan it.

## Running on a Linux PC

### stdio

Most MCP clients start the server as a child process. This mode is the default
when you run the binary with no arguments.

```sh
whatsapp-mcp serve --transport stdio
```

Stdout carries only MCP JSON. Logs go to stderr.

### HTTP

HTTP mode listens on loopback only.

```sh
whatsapp-mcp serve --transport http
```

The endpoint is **`http://127.0.0.1:8765/mcp`**. The handler is mounted at the
path `/mcp`. Clients must send `Authorization: Bearer <token>`.

The token comes from, in order:

1. `WHATSAPP_MCP_HTTP_TOKEN`, or the file named by `WHATSAPP_MCP_HTTP_TOKEN_FILE`.
2. `<data>/http-token`, when it exists. It must be a regular file with mode `0600`.
3. Otherwise the server generates a random 32-byte token (hex), writes it to
   `<data>/http-token` with mode `0600`, and logs the file path. The token itself
   is never logged. Read it with `cat ~/.local/share/whatsapp-mcp/http-token`.

To run without authentication, set `WHATSAPP_MCP_HTTP_NO_AUTH=true` (or pass
`--http-no-auth`). The server logs a warning. The combination with a token or
token file is refused.

Prefer `WHATSAPP_MCP_HTTP_TOKEN_FILE` for long-running services, and keep that
file at mode `0600`.

### Optional: systemd user service (HTTP)

`~/.config/systemd/user/whatsapp-mcp.service`:

```ini
[Unit]
Description=whatsapp-mcp (HTTP)

[Service]
ExecStart=%h/bin/whatsapp-mcp serve --transport http
EnvironmentFile=%h/.config/whatsapp-mcp/env
Restart=on-failure

[Install]
WantedBy=default.target
```

`~/.config/whatsapp-mcp/env` (mode `0600`):

```sh
WHATSAPP_MCP_HTTP_TOKEN=REPLACE_WITH_YOUR_TOKEN
```

```sh
mkdir -p ~/.config/whatsapp-mcp
chmod 600 ~/.config/whatsapp-mcp/env
systemctl --user daemon-reload
systemctl --user enable --now whatsapp-mcp
```

Pair the device with `whatsapp-mcp pair` before enabling the unit. The service
does not show a QR code, but it logs the PNG path while the account is unpaired.

## Inbound webhook (optional)

Set `WHATSAPP_MCP_WEBHOOK_CONFIG=/absolute/path/webhook.json` or pass
`--webhook-config`. With no path, there are no notifier workers or database, and
`connection_status.inbound_webhook.configured` is false. The notifier only runs
while `serve` or `pair` is running.

Create the file with mode **0600 or 0400**. Symlinks, non-regular files and other
permissions are refused. A missing or invalid file disables delivery and reports
`config_error`; it does not prevent the MCP server from starting. Example:

```json
{
  "enabled": false,
  "url": "",
  "authorization_header": "",
  "allowed_senders": ["573133992340"],
  "allowed_groups": [],
  "debounce_seconds": 20,
  "text_preview_chars": 280
}
```

Fill in the HTTPS URL and authorization value, then set `enabled` to true.
HTTP is allowed only for `localhost` and `127.0.0.1`. The authorization value may
include a leading `Authorization:` label, which is stripped. Redirects are not
followed. The header and URL path/query are never logged or included in MCP
status. Protect the file: it contains credentials and controls where message
previews are sent.

The file's modification time, size and permissions are checked every ten seconds.
A change reloads it and discards pending batches and queued deliveries from the
previous configuration. Invalid replacements leave delivery disabled. An already
in-flight HTTP request may finish.

An empty sender list allows all direct chats. Entries accept international phone
numbers with formatting, a `+` or `00` prefix, bare digits, or user JIDs. Both chat
and sender are checked, including locally known LID-to-phone mappings. Groups
require an exact `@g.us` entry in `allowed_groups`; sender restrictions do not
apply to allowed groups.

Only live, newly stored text/media messages enter the notifier. Messages from the
account itself, status broadcasts, messages older than process startup, and
non-message events are ignored. History batches, reactions, edits, revokes and
view-once content never enter the live notification path. Local name resolution
uses the same saved contact/chat/push-name rules as the read tools and performs
no network lookup.

The first accepted message starts a per-chat debounce window (default 20 seconds,
range 1–600). Messages during that window replace the payload with the latest
arrival and increment `count`; they do not extend the deadline. Preview limits
count Unicode characters (default 280, maximum 2000). A media message without a
caption has an empty preview.

Each request has `Content-Type: application/json` and
`User-Agent: whatsapp-mcp-notifier`. Example payload:

```json
{
  "event": "whatsapp.message",
  "chat_jid": "573133992340@s.whatsapp.net",
  "chat_name": "Saved contact",
  "is_group": false,
  "sender_jid": "573133992340@s.whatsapp.net",
  "sender_name": "Saved contact",
  "message_id": "latest-message-id",
  "ts": "2026-11-06T12:00:00Z",
  "text_preview": "Message text or media caption",
  "media_type": "",
  "count": 2,
  "account_jid": "own-account@s.whatsapp.net"
}
```

Dedupe keys `(chat_jid, message_id)` are recorded when accepted into a batch and
persist across restarts in `<data>/notifier.db`. Rows older than 30 days are
pruned at startup. This is a best-effort wakeup service: a crash, shutdown, full
queue, failed delivery or configuration change can lose notifications, and seen
messages are not replayed. Message storage is independent of delivery.

A single worker sends requests with a ten-second HTTP timeout. Network failures,
429 and 5xx responses retry, up to five total attempts, with 2, 4, 8 and 16 second
waits. Other unsuccessful responses fail immediately. The ingress queue, delivery
queue and pending chat count are bounded at 256 each; saturation increments
`dropped`. Shutdown discards pending batches and allows an active POST up to five
seconds before cancellation, then closes the dedupe database.

`connection_status.inbound_webhook` reports `configured`, effective `enabled`,
optional `config_error`, `url_host` (host only), `sent`, `failed`, `dropped`,
`retried`, `deduped`, `pending_chats`, `ignored` counters by reason,
`last_sent_at`, scrubbed `last_error`, and `last_status_code`. Ignored reasons
include `disabled`, `from_me`, `status_broadcast`, `before_start`, `not_message`,
`sender_not_allowed`, and `group_not_allowed`. Events discarded upstream never
reach these counters.

## One daemon, many stdio clients

On Unix, `serve`, `pair` and `unpair` take an exclusive, non-blocking
`<data>/instance.lock` before opening the WhatsApp database. Stop the daemon
before pairing or unpairing. The `status` command stays lock-free and reads the
session database without migration or creating local state. Platforms without
Unix flock support use a no-op lock fallback.

HTTP mode writes `<data>/daemon.json` with mode 0600 after binding the listener:

```json
{
  "http_addr": "127.0.0.1:8765",
  "token_file": "/absolute/path/to/http-token",
  "pid": 123,
  "started_at": "2026-11-06T12:00:00Z",
  "start_command": ["/absolute/path/to/whatsapp-mcp-supervise.sh"]
}
```

`start_command` is optional; set it with `--daemon-start-command` or
`WHATSAPP_MCP_DAEMON_START_COMMAND`. Only an absolute executable owned by the
current user, without group/world write permissions, can be launched. Discovery
is kept after shutdown so a later stdio process knows a daemon is expected.

A stdio process that finds the session lock held becomes a thin MCP proxy. It
opens neither WhatsApp nor message databases. Tools and their schemas come from
the daemon over Streamable HTTP; calls and their results/errors are forwarded.
The upstream session is reopened lazily after transport failure, retrying once.
The token is read from the discovery `token_file`, falling back to
`WHATSAPP_MCP_HTTP_TOKEN_FILE`, then `<data>/http-token`; it must be a regular
0600 file. HTTP mode with an environment-only token writes a private
`daemon-http-token` file for discovery. Use authenticated HTTP mode for proxying.
All stdio protocol output stays on stdout and logs stay on stderr.

If the lock is free and discovery exists, stdio launches its optional start
command detached, waits up to 30 seconds for ownership and an answering HTTP
endpoint, then proxies. It never falls back to connecting WhatsApp directly.
When unavailable, tools return `whatsapp daemon is not running; see <log path>`;
`connection_status` still works and returns `state: "daemon_unavailable"`.
With no discovery file and a free lock, stdio retains standalone behavior. To
return to standalone operation, stop the daemon and remove `daemon.json`.

A WhatsApp stream replaced by another client is logged at WARN and increments
`connection_status.stream_replaced_count`; `last_disconnect_reason` records the
cause. The lock owner reconnects after 5, 10, 20 seconds and so on, capped at five
minutes. Ten minutes of stable connection reset the delay. Normal disconnects
continue to use whatsmeow's built-in reconnect behavior. Keepalive timeouts and
connection failures also produce warnings.

### Supervisor

`scripts/whatsapp-mcp-supervise.sh` is a Bash supervisor for Linux (`flock`,
`setsid`, `nohup`, `stat` and `readlink` must be installed). It reads a regular
0600 file at `${WHATSAPP_MCP_ENV_FILE:-$HOME/.config/whatsapp-mcp-laura/env}`.
Lines are literal `KEY=VALUE` data, with no shell expansion or surrounding quotes;
blank lines and lines starting with `#` are ignored. Supported variables:

| Variable | Meaning |
|---|---|
| `WHATSAPP_MCP_ENV_FILE` | Override the supervisor env file path (set before invoking the script). |
| `WHATSAPP_MCP_DATA_DIR` | Required data directory. |
| `WHATSAPP_MCP_HTTP_ADDR` | Optional HTTP listener address; default `127.0.0.1:8765`. |
| `WHATSAPP_MCP_HTTP_TOKEN_FILE` | Optional private HTTP bearer token file. |
| `WHATSAPP_MCP_WEBHOOK_CONFIG` | Optional notifier JSON file. |
| `WHATSAPP_MCP_BIN` | Required absolute path to the built binary. |
| `WHATSAPP_MCP_LOG_DIR` | Log/PID directory; default `<data>/logs`. |
| `WHATSAPP_MCP_DAEMON_START_COMMAND` | Supervisor executable path; defaults to the script's resolved absolute path. |

Example private env file:

```text
WHATSAPP_MCP_DATA_DIR=/home/YOUR_USER/.local/share/whatsapp-mcp
WHATSAPP_MCP_BIN=/home/YOUR_USER/bin/whatsapp-mcp
WHATSAPP_MCP_HTTP_ADDR=127.0.0.1:8765
WHATSAPP_MCP_HTTP_TOKEN_FILE=/home/YOUR_USER/.local/share/whatsapp-mcp/http-token
WHATSAPP_MCP_WEBHOOK_CONFIG=/home/YOUR_USER/.config/whatsapp-mcp/webhook.json
WHATSAPP_MCP_LOG_DIR=/home/YOUR_USER/.local/state/whatsapp-mcp
```

If specifying a token file, create it with mode 0600 first. Otherwise omit that
line to let the daemon generate its default token.

```sh
scripts/whatsapp-mcp-supervise.sh             # detach and return immediately
scripts/whatsapp-mcp-supervise.sh --foreground
scripts/whatsapp-mcp-supervise.sh status
scripts/whatsapp-mcp-supervise.sh stop
```

A lock on `<log-dir>/supervisor.lock` admits one supervisor; a second exits
quietly. The supervisor records `supervisor.pid` and `daemon.pid`, appends daemon
output to `daemon.log`, and rotates a log larger than 10 MiB to `daemon.log.1`
before the next start. Exit restarts use a 2-second backoff doubling to 60 seconds,
reset after five minutes of uptime. SIGTERM is forwarded to the child and ends
the loop. `status` reports the two processes; `stop` terminates both.

## Connect OpenBot

OpenBot reads the generic `mcpServers` format. Field names can change between
OpenBot versions, so check its MCP settings if a key is rejected.

Ready-to-edit files:

- [`examples/mcp-config.stdio.json`](examples/mcp-config.stdio.json): launches the binary.
- [`examples/mcp-config.http.json`](examples/mcp-config.http.json): connects to a running HTTP server.

stdio (abridged):

```json
{
  "mcpServers": {
    "whatsapp": {
      "command": "/home/YOUR_USER/bin/whatsapp-mcp",
      "args": ["serve", "--transport", "stdio"],
      "env": {
        "WHATSAPP_MCP_DATA_DIR": "/home/YOUR_USER/.local/share/whatsapp-mcp",
        "WHATSAPP_MCP_SEND_ENABLED": "true",
        "WHATSAPP_MCP_ALLOWLIST": ""
      }
    }
  }
}
```

HTTP (abridged):

```json
{
  "mcpServers": {
    "whatsapp": {
      "type": "streamable-http",
      "url": "http://127.0.0.1:8765/mcp",
      "headers": {
        "Authorization": "Bearer REPLACE_WITH_YOUR_TOKEN"
      }
    }
  }
}
```

Omit the `headers` block only when you run with `WHATSAPP_MCP_HTTP_NO_AUTH=true`.

## Tools

| Name | Kind | Description |
|---|---|---|
| `list_chats` | read | List chats by last activity or name. Optional filter on the chat name, the contact's saved, business or push name, phone number or JID (accents and case ignored). Paginated. |
| `search_contacts` | read | Find contacts by saved name, business name, push name, phone number in any format, or JID. Accents and case are ignored, and every word must match in any order. Merges your address book with direct chats, and merges a person's `@lid` and phone-number entries. |
| `list_messages` | read | Search messages by chat, sender, text, or time range. Paginated. |
| `get_message_context` | read | Return a message with the messages before and after it in the same chat. |
| `download_media` | read | Download an attachment into the download directory. Fetches from WhatsApp and writes a local file. Refuses media larger than 100 MiB. |
| `edit_message` | send | Edit an own text message within 15 minutes, visible to everyone in the chat. Requires an explicit user request. |
| `delete_message` | send | Delete an own message for everyone within 48 hours. Requires an explicit user request. |
| `send_message` | send | Send a text message. |
| `send_file` | send | Send a file from the outbox or download directory, with an optional caption. Maximum 64 MiB. |
| `connection_status` | status | Report connection state, pairing, send settings, rate limits, and the address-book sync state (`contact_sync`, counts only). |
| `show_pairing_qr` | status | Return the pairing PNG path when unpaired. Never returns the QR payload. |
| `resync_contacts` | status | Ask WhatsApp to send the address book (saved contact names) and chat settings to this linked device again, in the background. Sends no message. |

`send_message`, `send_file`, `edit_message` and `delete_message` are registered only when `WHATSAPP_MCP_SEND_ENABLED`
is `true`. With sending disabled, the server exposes the read and status tools only.

Both send tools carry this warning in their descriptions:

> Sends immediately and irreversibly. Only call when the user explicitly asked to send this exact message to this exact recipient.

`download_media` refuses a message whose declared media size is above 100 MiB
before downloading, and refuses downloaded data above that size.

### Recipient resolution

The `recipient` argument of the send tools accepts three forms:

1. **A JID** (contains `@`). It must be a valid user, group or LID JID without a
   `:device` or `.agent` suffix. Use the plain JID.
2. **A phone number in international format.** It must start with `+` or `00`
   (for example `+33612345678`). A bare-digit number is an error asking you to
   include the country code. `search_contacts` and allowlist entries stay
   lenient and accept bare digits.
3. **A saved contact name.** The name is sent to a contact automatically only when
   it equals, case-insensitively, the saved name of exactly one contact **and**
   nothing else matches it, not even partially: no other contact name, push name,
   chat name or group name.
   - Otherwise nothing is sent. The result has `status: "ambiguous"` and lists
     candidates as `{jid, name, source}` where `source` is `contact`, `push_name`
     or `chat`. The note tells the client to re-run with the exact JID.
   - Push names and chat or group names are chosen by other people, so they are
     only ever returned as candidates, never sent to directly.
   - No match at all: the tool returns an error.

Resolution runs before the allowlist and rate-limit checks. The send tools check
that the account is paired and connected before they use a rate slot, and a send
that fails after the rate slot was taken gives the slot back.

`send_file` reads only regular files with a single hard link inside the outbox or
download directory. The file is opened under that directory, so replacing it with
a link to another location after the check does not escape it. Audio files cannot
carry a caption.

## Contact names (address-book sync)

### Why names were missing

WhatsApp does not put the names saved in your phone's address book in messages.
It sends them to linked devices as encrypted *app state*, in the
`critical_unblock_low` collection (one `ContactAction` with the full name, first
name, and the contact's LID and phone-number JIDs per entry). Push names (the
profile names people choose) arrive with messages and history, so they were
always present.

whatsmeow fetches app state on its own in only two cases. It fetches when the
phone shares the app-state sync keys, which normally happens once, right after
pairing (`handleAppStateSyncKeyShare` in whatsmeow `message.go`). It also fetches
when the server announces a change to one collection (`handleAppStateNotification`
in `notification.go`). It never fetches on connect, and earlier versions of this
server never asked for app state. If the post-pairing fetch of a collection
failed, nothing ever retried it. On the affected account, only `critical_block`
and `regular_low` had a stored version. `critical_unblock_low`, `regular_high` and
`regular` were never synced, so every contact had a push name but no saved name.

### What the server does now

- **Sync after connect.** About 10 seconds after each connection, the server
  fetches every app state collection: `critical_unblock_low` first, then
  `critical_block`, `regular_high`, `regular`, `regular_low`.
  - The collections are fetched one at a time, 3 seconds apart.
  - A collection that was never synced is downloaded in full. The others are
    updated incrementally.
  - When contacts are stored but none has a saved name, the address book is
    downloaded in full once per process.
  - A failed fetch is retried at most twice, after 15 s and 45 s.
  - A run never starts within 2 minutes of the previous one, and two runs never
    overlap.
- **Storage.** whatsmeow stores the names it receives in its own session
  database (`whatsapp.db`). The server also records the LID ↔ phone-number pairs
  that address-book entries carry. `messages.db` is not migrated: names are
  resolved when a tool runs, and chats without a name get one filled in when a
  sync finishes. A stored chat name is never overwritten.
- **Logs.** The info-level logs give only collection names, mutation counts and
  contact counts, never names or numbers.
- **`resync_contacts`.** Starts the same run on demand, always with a full
  download of the address book. It sends no chat message. It is refused while a run is in progress or
  within 2 minutes of the previous one.
- **`connection_status.contact_sync`.** Reports:
  - the run `state`: `not_run`, `running`, `done`, `keys_missing`, `failed` or
    `not_connected`;
  - `contacts_total` and `contacts_with_name`;
  - for each collection, its stored `version` (0 means never synced) and the
    result of the last fetch;
  - a `hint` when something needs attention.

Search and display use the names as follows:

- `search_contacts` matches saved full and first names, business names, push
  names, stored chat names, phone numbers and JIDs. `06 77 00 00 99`,
  `+33 6 77 00 00 99`, `0033677000099` and `33677000099` all find
  `33677000099@s.whatsapp.net`. A LID is matched only from its first digit.
- Results are ranked: exact name (any word order), then name prefix, then
  substring, then push name only, then number or JID only.
- Each result has `name`, `push_name`, `jid` (phone-number JID when known),
  `phone`, and `pn` and `lid` when the mapping is known.
- The `name` of a direct chat in `list_chats` is, in order: the saved name
  (looked up through the LID ↔ phone-number map), the stored chat name, the
  business name, the push name, then `+<number>`. The saved name comes first
  because stored names of direct chats were often push names. It is empty only when no number
  is known, such as a LID with no mapping.
- `sender_name` in messages prefers the saved name over the push name recorded
  with the message.

Send recipient resolution still sends automatically only on an exact saved
name, never on a business or push name. A person's `@lid` chat and
phone-number contact now count as one recipient, so a saved name no longer
comes back as two candidates for the same person.

### What to do after updating

Normally nothing:

1. Replace the binary and restart the MCP server. Your pairing and data are kept.
2. Wait about a minute, then call `connection_status`. `contact_sync.state` should
   be `done` and `contacts_with_name` should be well above 0.
3. Use `search_contacts` as usual. If names are still missing, call
   `resync_contacts`, wait a minute, and check `connection_status` again.

A re-pairing is needed only if WhatsApp cannot give this device the keys that
decrypt the address book. You recognise it like this:

- `contact_sync.state` is `keys_missing`, or a patch shows `last_result:
  keys_missing`. The `hint` says so.
- The log has `app state patch sync failed` with an error containing `didn't find
  app state key`.

When a key is missing, whatsmeow automatically asks the account's own primary
phone for it, with a protocol message to your own phone that is not a chat
message. If the phone answers, whatsmeow syncs again by itself. Keep the phone
online with WhatsApp open, then call `resync_contacts` a few minutes later. If the
state stays `keys_missing`, unlink the device on the phone (WhatsApp > Linked
devices), run `whatsapp-mcp pair`, and scan the new QR code. A new pairing makes
the phone share fresh keys.

Other failures (`failed`, with a server or timeout error) are retried on the next
connect. They do not call for a re-pairing.

This was tested only with unit tests and fakes, never against a live WhatsApp
account. The protocol behaviour described here comes from reading the pinned
whatsmeow source.

## Anti-ban measures

These limits make the account behave less like a bot. They do not remove the
risk described in the warning above.

- **Rate limiter.** Default: at most one message every 3 seconds and 60 per hour.
  The limiter refuses a send that exceeds these limits and returns the retry
  delay. It never queues sends.
- **Allowlist.** Set `WHATSAPP_MCP_ALLOWLIST` to restrict sends to known recipients.
  The allowlist check runs before a rate slot is used. Entries are phone numbers
  (international format, `+` or `00` optional) or JIDs. A phone number matches the
  user JID `<digits>@s.whatsapp.net` only. A JID entry matches that user and server
  only, whatever the device suffix. Group JIDs match exactly.
- **No bulk sending.** Each send call targets one recipient and one message.
- **No scheduling and no auto-reply.** Nothing sends unless a tool call asks for it.
- **No read receipts and no mark-as-read.** The server never marks a chat as read.
- **Reading and searching never send messages, read receipts, presence updates or
  history requests.** whatsmeow still performs protocol-level traffic (connection
  keep-alives, delivery acknowledgements required for decryption, group metadata
  and media-connection queries).
- **App state sync is paced.** The post-connect sync makes one request per
  collection, one at a time, a few seconds apart. Runs start at least 2 minutes
  apart. See
  [Contact names](#contact-names-address-book-sync).
- **Passive history only.** The server receives the history WhatsApp pushes after
  pairing. It never requests history on demand.

## Privacy and security

- **Local only.** All state lives in the data directory. The server has no telemetry.
- **Network access.** WhatsApp is reached through whatsmeow. An explicitly configured
  inbound webhook can send message previews to its HTTPS endpoint. Stdio proxies
  communicate only with the loopback daemon.
- **Log redaction.** At `info` and above, logs do not contain message bodies,
  full phone numbers, or full JIDs. Phone numbers and JIDs are masked.
- **HTTP is loopback-only.** The listener binds to `127.0.0.1`. The server also
  rejects requests with unexpected `Host` headers (DNS-rebinding protection).
- **Bearer token.** HTTP mode requires a token by default: yours (`WHATSAPP_MCP_HTTP_TOKEN`
  or `_FILE`) or an auto-generated one in `<data>/http-token`. Tokens are compared
  in constant time over SHA-256 digests.
- **`--http-no-auth` means no access control.** With it, any local process
  that can reach `127.0.0.1:8765` can call the server, and so can any program
  running as your user. Use it only on a machine where nothing else runs.
- **Never commit the data directory.** It holds session keys and message history.
  Keep it outside any git checkout. The repository `.gitignore` covers the usual
  names (`*.db`, `whatsapp-mcp-data/`, `pairing-qr.png`, `downloads/`, `outbox/`),
  and a test checks that these rules stay in place.
- **Checked by tests.** A dependency allowlist test fails if a module outside
  the approved list appears in `go.mod`. An AST test confines HTTP client calls to the notifier and daemon proxy.
  Other production packages cannot make outbound HTTP requests.

## Unpair and delete data

Unlink the device from the server:

```sh
whatsapp-mcp unpair
```

This logs the linked device out of WhatsApp, if the server can reach WhatsApp.
Local files are kept. If the logout fails, the command prints a warning and exits
with code 1.

Also delete the local data:

```sh
whatsapp-mcp unpair --delete-data
```

`--delete-data` removes `whatsapp.db*`, `messages.db*`, `notifier.db` and `pairing-qr.png` from
the data directory. It removes the `downloads/` and `outbox/` folders only when
they are inside the data directory; a custom download or outbox directory elsewhere
is never touched.

If the logout fails, `unpair --delete-data` deletes nothing, because the device
would stay linked on your phone without a local session to manage it. It prints
what it kept and why. Pass `--force` to delete the local data anyway, then remove
the device on your phone (**Settings → Linked devices**).

You can also remove the linked device on your phone (**Settings → Linked devices**)
and delete the directory yourself:

```sh
rm -rf ~/.local/share/whatsapp-mcp
```

## Development

```sh
go build ./...
go vet ./...
go test -race ./...
CGO_ENABLED=0 go build ./...
```

Tests never connect to a real WhatsApp account.

### Architecture

The domain package (`internal/wa`) has no whatsmeow import. It talks to WhatsApp through the
`wa.Client` interface in `internal/wa`. The real implementation lives in
`internal/wameow`, a thin adapter over whatsmeow. Tests use the in-memory fake in
`internal/wa/watest`. Other packages:

- `internal/store`: local message store (SQLite, `messages.db`). Text search folds case for ASCII letters only: `é` does not match `É`.
- `internal/session`: connects client events, the store, and pairing state.
- `internal/tools`: MCP tool definitions and handlers.
- `internal/server`: stdio, HTTP and daemon proxy transports.
- `internal/notify`: inbound webhook configuration, debounce, persistent dedupe and delivery.
- `internal/instance`, `internal/daemon`, `internal/securefile`: session ownership, daemon discovery/startup and private-file reads.
- `internal/ratelimit` and `internal/sendpolicy`: send rate limits and allowlist.
- `internal/config`, `internal/datadir`, `internal/qr`, `internal/redact`: configuration, file safety, QR output, and log redaction.
- `cmd/whatsapp-mcp`: CLI entry point.

## License and credits

Released under the [MIT License](LICENSE).

This project adapts ideas and portions of code from
[lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp) (MIT, Copyright
(c) 2025 Luke Harries). A reference copy is kept in `reference/lharries-whatsapp-mcp/`.
See [NOTICE.md](NOTICE.md) for attribution and third-party dependency licenses.
