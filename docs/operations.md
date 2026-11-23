# Operations

Running `matrix-whatsapp-mcp` as a long-lived service: where to put it, how to keep
it running, what its logs look like, and what to do when a check fails.

## Where to run it

The binary talks to the Matrix homeserver's client-server API and nothing else. It
does not need to reach WhatsApp, and it does not need to run on the bridge host: the
homeserver and the bridge already talk to each other. What it needs is reachability
to `MATRIX_HOMESERVER`.

The deployment these docs describe puts it on the same always-on host as the
homeserver and the bridge, on the loopback path to the homeserver: one host to back
up, one place to look when something is quiet, and no network hop in the sync loop.

Latency matters more than usual. The bridge-ack guard waits `MATRIX_BRIDGE_ACK_TIMEOUT`
(1 second by default) for the bridge to post a rejection notice, so a server on the
far side of an ocean will see more sends reported as successful simply because the
notice arrived after the window. Put it near the homeserver, and raise the timeout if
your bridge is slow.

Network exposure:

- **stdio** (default): no listening socket. The MCP client launches the process and
  speaks over its stdin/stdout.
- **http**: one listening socket on `HTTP_ADDR`. There is no authentication in front
  of it — the MCP transport carries none — so the address decides who can reach it.
  When the client is on the same host, bind `127.0.0.1` and put a reverse proxy with
  authentication in front if anything else must reach it. When the client is another
  machine on a private network, bind this host's private-network address and keep
  that address off the internet; the deployed unit does this. Never bind a public
  address.

The account needs no special power beyond being joined to the chats it should read.
Its access token is the whole of its identity, and it must belong to `MATRIX_USER_ID`
(the server checks this on the first sync and refuses to run on a mismatch).

## Running it beside the homeserver

On a co-located host, the whole stack lives under `/srv/matrix` — one directory to
inspect, back up and hand to the next operator, rather than a binary in `/usr/local`,
a unit in `/etc` and state in `/var/lib` with nothing tying them together:

```
/srv/matrix/
├── README.md                the operator's map of the whole stack
├── synapse -> /etc/matrix-synapse
├── synapse-data -> /var/lib/matrix-synapse
├── mautrix-whatsapp -> /opt/mautrix-whatsapp
└── matrix-whatsapp-mcp/
    ├── bin/matrix-whatsapp-mcp
    ├── etc/env              the environment file, root:matrix-mcp 0640
    ├── state/allow.txt      the send allowlist, with the crypto store beside it
    └── logs/                only used when output is redirected off journald
```

The homeserver and the bridge keep their upstream paths and are reached through
symlinks, so their own upgrades do not care about this layout while the tree still
shows what runs on the host. [`deploy/README.md`](../deploy/README.md) covers
installing the bundle into this shape.

### Ownership

Two rules cover it: configuration is root-writable and group-readable, and everything
the service writes belongs to the service user.

| Path | Owner | Mode | Why |
| --- | --- | --- | --- |
| `bin/` | `root:root` | `0755` | Code, replaced by an install, never written by the service. |
| `etc/env` | `root:matrix-mcp` | `0640` | Holds the access token, so it is not world-readable; the group bit is what lets `chats` and `doctor` be run as the service user. |
| `state/` | `matrix-mcp:matrix-mcp` | `0750` | The allowlist and the crypto store, written by the service and by `chats allow`. |
| `logs/` | `matrix-mcp:matrix-mcp` | `0750` | Writable only because the service may be pointed at a file instead of the journal. |

`matrix-mcp` is the placeholder for the account that owns `state/`; substitute your
own. The user is an ordinary system account with a nologin shell and no supplementary
groups.

### Hardening, and why each flag is there

| Setting | What it does |
| --- | --- |
| `User=` / `Group=` | Drops to the service account. The process needs its own files and a socket, nothing root provides. |
| `ProtectSystem=strict` | Makes the whole filesystem read-only for the service, `/etc`, `/usr` and `/srv` included, except the paths below. A bug or a bad config cannot rewrite the binary or the homeserver's configuration. |
| `ProtectHome=true` | `/home`, `/root` and `/run/user` are empty to the service. It is also why `MATRIX_ALLOWLIST_FILE` is set explicitly: the default is under `$HOME`. |
| `PrivateTmp=true` | Gives the service its own `/tmp` and `/var/tmp`, so a predictable temp name on the shared host is not a channel into the process. |
| `NoNewPrivileges=true` | No setuid binary can raise privileges, and no child inherits more than it started with. |
| `ReadWritePaths=...state ...logs` | The only writable paths, exactly the two the service needs. Everything else stays read-only even under `ProtectSystem=strict`. |

`Restart=always` rather than `on-failure`: the process is the only listener on the
port, and a clean exit would leave the tools unreachable while the host still looks
healthy. `RestartSec=5s` keeps a fast crash loop from filling the journal.

### What an operator runs

Status and restarts:

```sh
systemctl status matrix-whatsapp-mcp
sudo systemctl restart matrix-whatsapp-mcp
```

Restart only for a change to `etc/env`: the token, the homeserver URL, the listen
address, or the allowlist path. Logs come from the journal, not a file:

```sh
journalctl -u matrix-whatsapp-mcp -f
journalctl -u matrix-whatsapp-mcp --since "1 hour ago"
```

The subcommands talk to the homeserver with the service's own identity, so run them
as the service user with the environment file sourced — otherwise the CLI sees your
shell's environment, not the service's:

```sh
sudo -u matrix-mcp bash -c 'set -a; . /srv/matrix/matrix-whatsapp-mcp/etc/env; set +a; exec /srv/matrix/matrix-whatsapp-mcp/bin/matrix-whatsapp-mcp chats'
sudo -u matrix-mcp bash -c 'set -a; . /srv/matrix/matrix-whatsapp-mcp/etc/env; set +a; exec /srv/matrix/matrix-whatsapp-mcp/bin/matrix-whatsapp-mcp doctor'
```

`set -a` exports everything the file defines; without it the CLI reads an empty
environment and fails its config check.

To allow a chat, find it in the `chats` table and quote the number:

```sh
sudo -u matrix-mcp bash -c 'set -a; . /srv/matrix/matrix-whatsapp-mcp/etc/env; set +a; exec /srv/matrix/matrix-whatsapp-mcp/bin/matrix-whatsapp-mcp chats allow --index 2'
```

Nothing restarts. `chats allow` rewrites `state/allow.txt`, the running server
re-reads it within a second, and the same works for a hand edit of the file or for
`chats deny`. The next `send_message` to that chat either goes out or is refused
against the list as it stands now.

## Install

```sh
sudo useradd --system --home /srv/matrix/matrix-whatsapp-mcp --shell /usr/sbin/nologin matrix-mcp
sudo install -d -o root -g root -m 0755 /srv/matrix/matrix-whatsapp-mcp/bin
sudo install -d -o root -g matrix-mcp -m 0750 /srv/matrix/matrix-whatsapp-mcp/etc
sudo install -d -o matrix-mcp -g matrix-mcp -m 0750 \
  /srv/matrix/matrix-whatsapp-mcp/state /srv/matrix/matrix-whatsapp-mcp/logs
sudo install -m 0755 matrix-whatsapp-mcp /srv/matrix/matrix-whatsapp-mcp/bin/matrix-whatsapp-mcp
```

`state/` is the working directory: the crypto store (`matrix-crypto.db`) and the
allowlist file live there, both gitignored in a checkout and both private.

## Environment file

`deploy/matrix-whatsapp-mcp.env.example` is the template for
`/srv/matrix/matrix-whatsapp-mcp/etc/env`, mode `0640`, owner `root:matrix-mcp`:

```sh
MATRIX_HOMESERVER=https://matrix.example.com
MATRIX_USER_ID=@bot:example.com
MATRIX_ACCESS_TOKEN=YOUR_ACCESS_TOKEN

MATRIX_ALLOWLIST_FILE=/srv/matrix/matrix-whatsapp-mcp/state/allow.txt
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
sudo install -m 0640 -o root -g matrix-mcp deploy/matrix-whatsapp-mcp.env.example /srv/matrix/matrix-whatsapp-mcp/etc/env
sudo -e /srv/matrix/matrix-whatsapp-mcp/etc/env   # fill in the real values
sudo install -m 0644 deploy/matrix-whatsapp-mcp.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now matrix-whatsapp-mcp
systemctl status matrix-whatsapp-mcp
```

It runs the binary from `bin/` with `etc/env` as its environment and `state/` as its
working directory, sets `MCP_TRANSPORT=http` (a unit has no client on stdin, so the
stdio default would sit idle), restarts on any exit after 5 seconds, sends SIGTERM on
stop, and allows 20 seconds for the graceful shutdown path. `User=` and `Group=` name
the account that owns `state/` — `matrix-mcp` in the example; substitute your own.
`ProtectSystem=strict`, `ProtectHome=true`, `PrivateTmp=true`, `NoNewPrivileges=true`
and two writable paths (`state/`, `logs/`) keep the service away from the rest of the
host.

The unit binds the host's private-network address when you put one in `HTTP_ADDR`,
because the MCP client is another machine on that network. There is no
authentication in front of the listener, so that address must stay private.

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
| `serving MCP over HTTP on …` | The listener is up, on the configured `HTTP_ADDR`. |
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
redirect output to a file instead (`StandardOutput=append:/srv/matrix/matrix-whatsapp-mcp/logs/matrix-whatsapp-mcp.log`),
use logrotate with `copytruncate`: the process holds stderr open and has no reopen
signal.

## Enrolling a chat

Enrolling a chat is three steps: invite the bot from Element, find the chat with
`chats`, then allow it with `chats allow --index N`. When the service runs as another
user, both the service and your shell must resolve the same allowlist path — that is
the one difference. `rooms` and `rooms allow|deny` still work as hidden aliases, so a
script written before the rename keeps working.

```sh
sudo -u matrix-mcp bash -c 'set -a; . /srv/matrix/matrix-whatsapp-mcp/etc/env; set +a; exec /srv/matrix/matrix-whatsapp-mcp/bin/matrix-whatsapp-mcp chats'
sudo -u matrix-mcp bash -c 'set -a; . /srv/matrix/matrix-whatsapp-mcp/etc/env; set +a; exec /srv/matrix/matrix-whatsapp-mcp/bin/matrix-whatsapp-mcp chats allow --index 2'
```

The running server re-reads the file within a second; nothing restarts.

## What `doctor` checks

```sh
sudo -u matrix-mcp bash -c 'set -a; . /srv/matrix/matrix-whatsapp-mcp/etc/env; set +a; exec /srv/matrix/matrix-whatsapp-mcp/bin/matrix-whatsapp-mcp doctor'
```

| Check | What it proves |
| --- | --- |
| `config` | Every required variable is set and well formed. A failure prints every problem at once and stops the run. |
| `versions` | `GET {MATRIX_HOMESERVER}/_matrix/client/versions` answers `200`, which separates a wrong URL or an unreachable homeserver from a bad token. It is unauthenticated, so no secret is sent. |
| `whoami` | The token is valid and belongs to `MATRIX_USER_ID`. A mismatch names both IDs. |
| `joined chats` | The chats the account can see and how many are on WhatsApp. The WhatsApp probes are cut off after 20 seconds and the output says so. |
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
- **`list_chats` returns nothing** — the account has joined no chats, they are
  inaccessible to the token, or the bridge has not populated them yet.
- **Sending is rejected** — sending fails closed. Run `chats` and read the `SEND`
  column, then `chats allow --index N`. An empty allowlist means read-only.
- **`chats allow` did not make a chat sendable** — the running server and the command
  resolved different `MATRIX_ALLOWLIST_FILE` paths. Set the same absolute path in
  both places.
- **`chats deny` exits non-zero and the chat stays sendable** — the chat is listed in
  `ALLOWED_SEND_ROOMS`, which the command does not edit. Remove it there and restart.
- **The bot never joins after an invite** — look for an `auto-join:` line. Without
  `MATRIX_AUTO_JOIN_INVITES=true` nothing joins for you; with
  `MATRIX_INVITE_ALLOWLIST` set, other inviters are refused by design.
- **`--index 3` says it is out of range** — the numbers come from the current
  listing; re-run `chats` and use the number printed there.
- **Every send succeeds but nothing arrives on WhatsApp** — relay mode. See the
  relay section of the [README](../README.md#whatsapp-delivery-needs-relay-mode).
