# Operations

Running `matrix-whatsapp-mcp` as a long-lived service: where to put it, how to keep
it running, what its logs look like, and what to do when a check fails.

## Where to run it

The binary talks to the Matrix homeserver's client-server API and nothing else. It
does not need to reach WhatsApp, and it does not need to run on the bridge host: the
homeserver and the bridge already talk to each other. What it needs is HTTPS egress
to `MATRIX_HOMESERVER`.

Latency matters more than usual. The bridge-ack guard waits `MATRIX_BRIDGE_ACK_TIMEOUT`
(1 second by default) for the bridge to post a rejection notice, so a server on the
far side of an ocean will see more sends reported as successful simply because the
notice arrived after the window. Put it near the homeserver, and raise the timeout if
your bridge is slow.

Network exposure:

- **stdio** (default): no listening socket. The MCP client launches the process and
  speaks over its stdin/stdout.
- **http**: one listening socket on `HTTP_ADDR`. There is no authentication in front
  of it — the MCP transport carries none — so bind it to `127.0.0.1` and put a
  reverse proxy with authentication in front if anything else must reach it.

The account needs no special power beyond being joined to the rooms it should read.
Its access token is the whole of its identity, and it must belong to `MATRIX_USER_ID`
(the server checks this on the first sync and refuses to run on a mismatch).

## Install

```sh
sudo useradd --system --home /var/lib/matrix-whatsapp-mcp --shell /usr/sbin/nologin matrix-mcp
sudo install -d -o matrix-mcp -g matrix-mcp -m 0750 /var/lib/matrix-whatsapp-mcp
sudo install -d -m 0750 /etc/matrix-whatsapp-mcp
sudo install -m 0755 matrix-whatsapp-mcp /usr/local/bin/matrix-whatsapp-mcp
```

`/var/lib/matrix-whatsapp-mcp` is the working directory: the crypto store
(`matrix-crypto.db`) and the allowlist file live there, both gitignored in a
checkout and both private.

## Environment file

`deploy/matrix-whatsapp-mcp.env.example` is the template for
`/etc/matrix-whatsapp-mcp/env`, mode `0640`, owner `root:matrix-mcp`:

```sh
MATRIX_HOMESERVER=https://matrix.example.com
MATRIX_USER_ID=@bot:example.com
MATRIX_ACCESS_TOKEN=YOUR_ACCESS_TOKEN

MATRIX_ALLOWLIST_FILE=/var/lib/matrix-whatsapp-mcp/allow.txt
MCP_TRANSPORT=http
HTTP_ADDR=127.0.0.1:8080

# Optional
# TRIGGER_WEBHOOK_URL=https://example.com/hooks/wa
# MATRIX_AUTO_JOIN_INVITES=true
# MATRIX_INVITE_ALLOWLIST=@you:example.com
# MATRIX_BRIDGE_ACK_TIMEOUT=1s
```

Systemd reads the file as root before dropping to `matrix-mcp`, so the service
never needs to read it itself; `0640 root:matrix-mcp` is only so the CLI can be run
as that user too.

`MATRIX_ALLOWLIST_FILE` is set explicitly on purpose. The default path comes from
`$XDG_CONFIG_HOME`/`$HOME`, and the unit sets `ProtectHome=true`; leaving it unset
would make the service's allowlist path differ from the one you use in your shell.

## The unit

`deploy/matrix-whatsapp-mcp.service` is ready to install:

```sh
sudo install -m 0640 -o root -g matrix-mcp deploy/matrix-whatsapp-mcp.env.example /etc/matrix-whatsapp-mcp/env
sudo -e /etc/matrix-whatsapp-mcp/env      # fill in the real values
sudo install -m 0644 deploy/matrix-whatsapp-mcp.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now matrix-whatsapp-mcp
systemctl status matrix-whatsapp-mcp
```

It runs the binary with `MCP_TRANSPORT=http` (a unit has no client on stdin, so the
stdio default would sit idle), restarts on failure after 5 seconds, sends SIGTERM on
stop, and allows 20 seconds for the graceful shutdown path. `ProtectSystem=strict`,
`ProtectHome=true`, `PrivateTmp=true`, `NoNewPrivileges=true` and a single writable
path (`/var/lib/matrix-whatsapp-mcp`) keep the service away from the rest of the
host.

Configuration is read once at startup. Changing the environment file needs a
restart; changing the allowlist file does not.

## Logs

Everything the binary logs goes to stderr, and under the unit that means the
journal:

```sh
journalctl -u matrix-whatsapp-mcp -f
journalctl -u matrix-whatsapp-mcp --since "1 hour ago"
```

Lines worth knowing:

| Line | Meaning |
| --- | --- |
| `serving MCP over HTTP on 127.0.0.1:8080` | The listener is up. |
| `sync stopped (...); retrying in ...` | Transient sync failure, backing off from 1s to 1m with jitter. |
| `auto-join: joined !room:server (invited by @you:server)` | An invitation was accepted. |
| `auto-join: refusing invitation to ...: not in MATRIX_INVITE_ALLOWLIST` | An invitation was refused on purpose and left pending. |
| `allowlist file ...: line N is not a room ID and was ignored` | A hand edit left a bad line; the rest of the file still applies. The line's content is never echoed. |
| `allowlist file ... is unreadable, keeping the previous list: ...` | Permissions or I/O problem; the last known list stays in force. |
| `bridge-ack probe failed for ...` | The guard could not read the room once; the send is still reported as a success. |
| `webhook delivery failed for $event after 1 retries: ...` | The trigger was dropped after one retry. The URL is not logged. |
| `shutdown signal received` | Graceful shutdown started. |
| `fatal: ...` | The process is exiting non-zero (bad config, invalid token, user mismatch). |

Journald rotates by size and time on its own, so no logrotate rule is needed. If you
redirect output to a file instead (`StandardOutput=append:/var/log/...`), use
logrotate with `copytruncate`: the process holds stderr open and has no reopen
signal.

## Enrolling a room

Enrolling a room is three steps: invite the bot from Element, find the room with
`rooms`, then allow it with `rooms allow --index N`. When the service runs as another
user, both the service and your shell must resolve the same allowlist path — that is
the one difference.

```sh
sudo -u matrix-mcp bash -c 'set -a; . /etc/matrix-whatsapp-mcp/env; set +a; exec matrix-whatsapp-mcp rooms'
sudo -u matrix-mcp bash -c 'set -a; . /etc/matrix-whatsapp-mcp/env; set +a; exec matrix-whatsapp-mcp rooms allow --index 2'
```

The running server re-reads the file within a second; nothing restarts.

## What `doctor` checks

```sh
matrix-whatsapp-mcp doctor
```

| Check | What it proves |
| --- | --- |
| `config` | Every required variable is set and well formed. A failure prints every problem at once and stops the run. |
| `versions` | `GET {MATRIX_HOMESERVER}/_matrix/client/versions` answers `200`, which separates a wrong URL or an unreachable homeserver from a bad token. It is unauthenticated, so no secret is sent. |
| `whoami` | The token is valid and belongs to `MATRIX_USER_ID`. A mismatch names both IDs. |
| `joined rooms` | The rooms the account can see and how many look bridged from WhatsApp. The bridged probes are cut off after 20 seconds and the output says so. |
| `send allowlist` | The effective allowlist and where each entry came from: `ALLOWED_SEND_ROOMS`, the allowlist file, or both. Empty is reported as read-only, which is a valid state. |
| `crypto db` | Only with E2EE on: the SQLite crypto store is writable. This is a `[warn]` and still exits non-zero, because the server cannot start without it. |

Exit status is `0` when everything passed, `1` when a check failed or warned, and `2`
for a usage error. Failures never echo the access token or the pickle key.

## Troubleshooting

- **`olm/olm.h: No such file or directory`** — the C libolm backend was selected.
  Build with `-tags goolm`, or install `libolm-dev` and build without the tag. `make
  build` and the smoke script always use the tag.
- **A C compiler error about sqlite3** — the SQLite store is compiled from C.
  Install gcc and make sure `CGO_ENABLED` is not `0`.
- **`bind: address already in use`** — something else owns `HTTP_ADDR`. Change it, or
  find the listener with `ss -ltnp`.
- **`M_UNKNOWN_TOKEN` / authentication errors** — the access token was revoked or
  expired. Issue a new one, update the environment file, restart. The process exits
  non-zero on an invalid token so a supervisor can restart it once the fix is in.
- **`list_rooms` returns nothing** — the account has joined no rooms, the rooms are
  inaccessible to the token, or the bridge has not populated them yet.
- **Sending is rejected** — sending fails closed. Run `rooms` and read the `SEND`
  column, then `rooms allow --index N`. An empty allowlist means read-only.
- **`rooms allow` did not make a room sendable** — the running server and the command
  resolved different `MATRIX_ALLOWLIST_FILE` paths. Set the same absolute path in
  both places.
- **`rooms deny` exits non-zero and the room stays sendable** — the room is listed in
  `ALLOWED_SEND_ROOMS`, which the command does not edit. Remove it there and restart.
- **The bot never joins after an invite** — look for an `auto-join:` line. Without
  `MATRIX_AUTO_JOIN_INVITES=true` nothing joins for you; with
  `MATRIX_INVITE_ALLOWLIST` set, other inviters are refused by design.
- **`--index 3` says it is out of range** — the numbers come from the current
  listing; re-run `rooms` and use the number printed there.
- **Every send succeeds but nothing arrives on WhatsApp** — relay mode. See the
  relay section of the [README](../README.md#whatsapp-delivery-needs-relay-mode).
