# matrix-whatsapp-mcp

A Go MCP server that connects to a Matrix homeserver running a mautrix/whatsapp
bridge. It listens to incoming message events, keeps a small in-memory history,
optionally forwards them to a webhook, and exposes MCP tools for listing rooms,
reading messages, and sending replies (allow-list gated, fail closed).

Status: Phase E — the codebase is covered by unit and protocol tests (run under
the race detector), a smoke script exercises the built binary end to end,
`go vet`/`gofmt` are clean, and enrolling a room is one command (`rooms allow`)
instead of an Element lookup, an `.env` edit and a restart.

## Prerequisites

- Go 1.26+ (the toolchain requirement is imposed by the current mautrix
  release; see Architecture > Dependencies).
- A C compiler (gcc) with cgo enabled, because the SQLite store used by the
  crypto and state backends (`github.com/mattn/go-sqlite3`) is compiled from C.
- The pure-Go olm crypto backend, selected with the `goolm` build tag. Every
  build/run/test command below carries `-tags goolm`; the alternative is to
  install libolm (`libolm-dev` on Debian/Ubuntu) and build without the tag.
- A Matrix homeserver account with an access token.

## Configuration

All configuration is read from environment variables. The complete list, one
per variable, with an example value:

| Variable | Required | Default | Example | Meaning |
| --- | --- | --- | --- | --- |
| `MATRIX_HOMESERVER` | yes | — | `https://homeserver.example.com` | Homeserver base URL. |
| `MATRIX_USER_ID` | yes | — | `@bot:example.com` | The bot's Matrix user ID. |
| `MATRIX_ACCESS_TOKEN` | yes | — | `syt_...` | The bot's access token. Secret: never commit or log it. |
| `MATRIX_E2EE_ENABLED` | no | `false` | `false` | Enable end-to-end encryption. When true, `MATRIX_PICKLE_KEY` is required and crypto state is stored at `MATRIX_CRYPTO_DB_PATH`. |
| `MATRIX_CRYPTO_DB_PATH` | no | `matrix-crypto.db` | `matrix-crypto.db` | SQLite crypto store path, used only when E2EE is on. |
| `MATRIX_PICKLE_KEY` | no | — | `change-me-32-byte-random-string` | Crypto pickle key; required when E2EE is on. Secret. |
| `TRIGGER_WEBHOOK_URL` | no | — | `https://example.com/hooks/wa` | Optional URL to POST incoming-message triggers (JSON, 5s timeout, 1 retry). |
| `ALLOWED_SEND_ROOMS` | no | — (empty) | `!room1:example.com,!UBRW6WycBKpUJu3m4HP0RCMsGnUp_EwSZa2RLXHe34Y` | Comma-separated room IDs allowed to receive outbound messages; a `:server` suffix is optional. Unioned with `MATRIX_ALLOWLIST_FILE`. Both empty = read-only (sending fails closed). |
| `MATRIX_ALLOWLIST_FILE` | no | `$XDG_CONFIG_HOME/matrix-whatsapp-mcp/allow.txt`, else `~/.config/matrix-whatsapp-mcp/allow.txt` | `/home/me/.config/matrix-whatsapp-mcp/allow.txt` | Room IDs allowed to receive outbound messages, one per line, `#` comments allowed. Unioned with `ALLOWED_SEND_ROOMS`, and re-read while the server runs, so `rooms allow` needs no restart. |
| `MATRIX_AUTO_JOIN_INVITES` | no | `false` | `true` | Accept room invitations automatically instead of joining by hand in Element. Every join and every refusal is logged to stderr. |
| `MATRIX_INVITE_ALLOWLIST` | no | — (empty) | `@alice:example.com,@bob:example.com` | With auto-join on, accept invitations only from these user IDs (compared case-insensitively). Empty = accept from anyone. |
| `MATRIX_BRIDGE_ACK_TIMEOUT` | no | `1s` | `1s`, `500ms`, `0` | How long a send waits for the bridge's rejection notice. `0` disables the check. |
| `MCP_TRANSPORT` | no | `stdio` | `stdio` or `http` | How the MCP server is exposed. |
| `HTTP_ADDR` | no | `:8080` | `127.0.0.1:8080` | Listen address when `MCP_TRANSPORT=http`. |

See [.env.example](.env.example) for a copy-paste template. Sending fails
closed: unless a room is in the effective allowlist — the union of
`ALLOWED_SEND_ROOMS` and `MATRIX_ALLOWLIST_FILE` — the server refuses to send to
it, and a send the WhatsApp bridge rejects is reported as a failure rather than
a success (see `MATRIX_BRIDGE_ACK_TIMEOUT`).

## Building and running

### stdio transport (default)

```sh
go build -tags goolm -o matrix-whatsapp-mcp ./cmd/server

MATRIX_HOMESERVER=https://homeserver.example.com \
MATRIX_USER_ID=@bot:example.com \
MATRIX_ACCESS_TOKEN=syt_... \
./matrix-whatsapp-mcp
```

Or load the values from a `.env` file:

```sh
cp .env.example .env   # then fill in real values
set -a; . ./.env; set +a
./matrix-whatsapp-mcp
```

### http transport

```sh
MATRIX_HOMESERVER=https://homeserver.example.com \
MATRIX_USER_ID=@bot:example.com \
MATRIX_ACCESS_TOKEN=syt_... \
MCP_TRANSPORT=http \
HTTP_ADDR=:8080 \
./matrix-whatsapp-mcp
```

The Streamable HTTP endpoint is then served on `HTTP_ADDR` (any path).

A `Makefile` wraps the tagged commands so the `goolm` tag cannot be forgotten:
`make build`, `make run`, `make test`, `make vet`.

## Enrolling a room in three steps

A new room is unusable until the bot has joined it and it is in the send
allowlist. Three steps, once per room.

### 1. Invite the bot from Element

Open the room in Element and invite the bot's account (`MATRIX_USER_ID`). This
is the one step that cannot be automated: Matrix requires the invited account to
accept the invitation itself.

With `MATRIX_AUTO_JOIN_INVITES=true` the bot joins within a few seconds and
logs the join:

```
matrix-whatsapp-mcp: auto-join: joined !cafe:example.com (invited by @you:example.com)
```

With `MATRIX_INVITE_ALLOWLIST` set, an invitation from anyone else is refused and
logged the same way, and the invitation stays pending in Element:

```
matrix-whatsapp-mcp: auto-join: refusing invitation to !spam:example.com from @stranger:example.com: not in MATRIX_INVITE_ALLOWLIST
```

With auto-join off (the default) accept the invitation by hand in Element; the
rest of the flow is identical. An invitation is also picked up after a restart:
the bot accepts pending invitations from its first sync, so inviting it while it
is down is safe.

### 2. Find the room

```sh
./matrix-whatsapp-mcp rooms
```

```
  #  NAME             ROOM ID                BRIDGED  SEND
  1  Cafe             !cafe:example.com      yes      no
  2  Family WhatsApp  !family:example.com    yes      no

2 room(s), 0 send-allowed
allowlist: 0 from ALLOWED_SEND_ROOMS, 0 from /home/me/.config/matrix-whatsapp-mcp/allow.txt (not created yet)
```

`#` is the number the next step takes. `BRIDGED` is the same best-effort check
`list_rooms` uses (an `m.bridge` state event, else a joined member whose
localpart contains `whatsapp`). `SEND` says whether the room is in the effective
allowlist right now.

### 3. Allow it

```sh
./matrix-whatsapp-mcp rooms allow 2
# allowed "Family WhatsApp" (!family:example.com)
# wrote /home/me/.config/matrix-whatsapp-mcp/allow.txt
# the running server picks this up within a second; no restart needed
```

`rooms deny` reverses it. Both accept a room ID, a name, or `--index N` with the
number printed above:

- an exact room ID wins outright;
- otherwise an exact name, case-insensitively;
- otherwise a case-insensitive substring of the name or the room ID.

Anything but a single match is an error, and an ambiguous query prints every
candidate and exits non-zero rather than guessing which room was meant:

```
error: "family" matches 2 rooms, refusing to guess:
  "Family Photos" (!family2:example.com)
  "Family WhatsApp" (!family:example.com)
name one of them exactly, pass the room ID, or use --index N
```

`rooms allow` writes the allowlist file atomically (a temporary file plus a
rename) and creates its directory when it does not exist. The running server
notices the change on its next send, so there is no restart and no `.env` edit.

An empty allowlist is still read-only and fails closed. `rooms deny` edits the
file only, so a room that is also listed in `ALLOWED_SEND_ROOMS` is reported as
still allowed and the command exits non-zero rather than claiming a denial that
did not happen.

### If the server runs from a different user

The default file path comes from `$XDG_CONFIG_HOME`/`$HOME`. A server started by
a systemd unit can resolve a different path than your shell, so set
`MATRIX_ALLOWLIST_FILE` explicitly to the same absolute path in both places.

## Doctor

```sh
./matrix-whatsapp-mcp doctor
```

```
matrix-whatsapp-mcp doctor

config
  homeserver    https://matrix.example.com
  user          @bot:example.com
  transport     stdio
  e2ee          disabled

checks
  [ok  ] versions        HTTP 200 in 43ms, 3 versions, newest v1.11
  [ok  ] whoami          @bot:example.com (device DEVICE)
  [ok  ] joined rooms    2 joined, 1 bridged from WhatsApp
  [ok  ] send allowlist  1 from ALLOWED_SEND_ROOMS, 1 from /home/me/.config/matrix-whatsapp-mcp/allow.txt
  [ok  ] crypto db       not needed, MATRIX_E2EE_ENABLED is false

joined rooms
  #  NAME             ROOM ID              BRIDGED  SEND
  1  Cafe             !cafe:example.com    yes      yes
  2  Family WhatsApp  !family:example.com  yes      yes

2 room(s), 2 send-allowed
allowlist: 1 from ALLOWED_SEND_ROOMS, 1 from /home/me/.config/matrix-whatsapp-mcp/allow.txt

send allowlist
  !cafe:example.com
      ALLOWED_SEND_ROOMS
  !family:example.com
      /home/me/.config/matrix-whatsapp-mcp/allow.txt
  file /home/me/.config/matrix-whatsapp-mcp/allow.txt

all 5 checks passed
```

Each line is one check:

| Check | What it proves |
| --- | --- |
| `config` | Every required variable is set and well formed. A failure prints every problem at once and stops the run. |
| `versions` | `GET {MATRIX_HOMESERVER}/_matrix/client/versions` answers `200`, which separates a wrong URL or an unreachable homeserver from a bad token. It is unauthenticated, so no secret is sent. |
| `whoami` | The token is valid and belongs to `MATRIX_USER_ID`. A mismatch names both IDs. |
| `joined rooms` | The rooms the account can see, and how many look bridged from WhatsApp. |
| `send allowlist` | The effective allowlist and where every entry came from: `ALLOWED_SEND_ROOMS`, the allowlist file, or both. Nothing allowed is reported as read-only, which is a valid state, not a failure. |
| `crypto db` | Only with E2EE on: the SQLite crypto store is writable. This is a `[warn]`, and the run still exits non-zero, because the server cannot start without it. |

Exit status is `0` when every check passes, `1` when any check failed (or warned,
as the crypto store does), and `2` for a usage error. Failures never echo the
access token or the pickle key.

## MCP client configuration

Point any MCP client at the server using one of these shapes.

stdio (the client launches the binary):

```json
{
  "mcpServers": {
    "matrix-whatsapp": {
      "command": "/absolute/path/to/matrix-whatsapp-mcp",
      "args": [],
      "env": {
        "MATRIX_HOMESERVER": "https://homeserver.example.com",
        "MATRIX_USER_ID": "@bot:example.com",
        "MATRIX_ACCESS_TOKEN": "syt_your_access_token",
        "MCP_TRANSPORT": "stdio"
      }
    }
  }
}
```

http (start the server first with `MCP_TRANSPORT=http`, then point the client
at it):

```json
{
  "mcpServers": {
    "matrix-whatsapp": {
      "url": "http://127.0.0.1:8080/mcp"
    }
  }
}
```

## Testing

```sh
# Unit, protocol, and race tests.
go test -tags goolm ./...
go test -tags goolm -race -count=1 ./...

# Static checks.
go vet -tags goolm ./...
gofmt -l .

# End-to-end smoke test of the built binary (offline by default).
./scripts/smoke_test.sh
```

The Makefile wraps the tagged test/vet commands as `make test` and `make vet`.
The smoke script builds with the `goolm` tag, checks the missing-config exit
path, performs a real stdio JSON-RPC `initialize` + `tools/list` round trip, and
— only when `MATRIX_HOMESERVER`/`MATRIX_USER_ID`/`MATRIX_ACCESS_TOKEN` are all
set — checks homeserver reachability and runs an authenticated `list_rooms`.
It never prints the access token.

## Troubleshooting

- **Build fails with `olm/olm.h: No such file or directory`** — the C libolm
  backend is being selected. Build with `-tags goolm` (or install `libolm-dev`
  and build without the tag). The Makefile and smoke script already use the tag.
- **Build fails with a C compiler error about sqlite3** — the SQLite store is
  compiled from C. Install gcc (and ensure `CGO_ENABLED` is not `0`).
- **`bind: address already in use` on the http transport** — something already
  listens on `HTTP_ADDR` (default `:8080`). Set `HTTP_ADDR` to a free port.
- **`M_UNKNOWN_TOKEN` in the log / auth errors** — the access token has expired
  or been revoked. Issue a fresh token, update `MATRIX_ACCESS_TOKEN`, and
  restart. The process exits non-zero on an invalid token so a supervisor can
  restart it.
- **`list_rooms` returns nothing** — the account has joined no rooms, the rooms
  are inaccessible to the token, or the bridge has not populated the rooms yet.
  Verify the account is a member of the bridged rooms and that `MATRIX_USER_ID`
  matches the token's user.
- **Sending is rejected** — sending fails closed. Run `./matrix-whatsapp-mcp
  rooms` and check the `SEND` column, then `rooms allow <index>`. An empty
  allowlist means read-only mode. Room IDs the bridge lists without a `:server`
  suffix are valid and can be allow-listed as-is.
- **`rooms allow` did not make a room sendable** — the server and the command
  resolve `MATRIX_ALLOWLIST_FILE` differently. The default comes from
  `$XDG_CONFIG_HOME`/`$HOME`, which a systemd unit usually sets to something
  other than your shell's. Set `MATRIX_ALLOWLIST_FILE` to the same absolute path
  in both.
- **`rooms deny` exits non-zero and the room stays sendable** — it is listed in
  `ALLOWED_SEND_ROOMS`, which `rooms` does not edit; remove it there and
  restart, or leave the file as the only source.
- **The bot never joins the room after the invite** — check stderr for an
  `auto-join:` line. Without `MATRIX_AUTO_JOIN_INVITES=true` nothing joins for
  you; with `MATRIX_INVITE_ALLOWLIST` set, an invitation from anyone else is
  refused on purpose. The pending invitation stays visible in Element until it
  is accepted or withdrawn.
- **`rooms allow 3` says the index is out of range** — the numbers come from the
  current listing; re-run `rooms` and use the number printed there.

## Architecture

### Internal logger

All logging goes to stderr. stdout is reserved for MCP JSON-RPC frames in stdio
mode, so nothing in the codebase may write to stdout.

### config

```go
package config

type Transport string

const (
    TransportStdio Transport = "stdio"
    TransportHTTP  Transport = "http"
)

type Config struct {
    Homeserver       string
    UserID           string
    AccessToken      string
    E2EEEnabled      bool
    CryptoDBPath     string
    PickleKey        string
    TriggerWebhook   string
    AllowedSendRooms []string
    Transport        Transport
    HTTPAddr         string
    AutoJoinInvites  bool
    InviteAllowlist  []string
    AllowlistFile    string
}

func Load() (*Config, error)

// ValidRoomID is the single source of truth for room ID shape: a leading "!",
// at most 255 bytes, and no whitespace, control characters, "/", "?", or "#".
// A ":server" part is *not* required — the WhatsApp bridge hands out room IDs
// without one.
func ValidRoomID(roomID string) bool

func (c *Config) ReadOnly() bool
func (c *Config) SendAllowed(roomID string) bool
func (c *Config) InviteAllowed(userID string) bool
func (c *Config) String() string // redacts secrets
func (c *Config) GoString() string
```

`ValidRoomID` is what both `Load()` (for `ALLOWED_SEND_ROOMS`) and
`matrix.Client` (for `Messages` and `SendText`) use, so the allow-list and the
client can never disagree about which room IDs are acceptable.

`ReadOnly` and `SendAllowed` describe the `ALLOWED_SEND_ROOMS` baseline only;
the effective allowlist is `allowlist.Store`, which unions that baseline with
`MATRIX_ALLOWLIST_FILE`. `InviteAllowed` is true when
`MATRIX_INVITE_ALLOWLIST` is empty or names the inviter, ignoring case.

`Load()` validates every variable and returns a joined error of
`*config.VarError` values. Error messages name the offending variable and
never echo its value, so tokens and pickle keys cannot leak through errors.

### allowlist

```go
package allowlist

const EnvSource = "ALLOWED_SEND_ROOMS"

type List interface {
    Rooms() []string
    Allowed(roomID string) bool
}

type Entry struct {
    RoomID  string
    Sources []string // EnvSource, the file path, or both
}

type Store struct{ /* ... */ }

func New(baseline []string, path string, logger *log.Logger, opts ...Option) *Store
func WithMinInterval(d time.Duration) Option
func FromSlice(rooms []string) List

func (s *Store) Rooms() []string
func (s *Store) Allowed(roomID string) bool
func (s *Store) Entries() []Entry
func (s *Store) Path() string
func (s *Store) Refresh()
func (s *Store) Reload()

func Add(path, roomID string) (changed bool, err error)
func Remove(path, roomID string) (changed bool, err error)
```

`Store` is the single source of truth for "may this room receive a message": the
`ALLOWED_SEND_ROOMS` baseline unioned with the file, deduplicated, sorted. Both
the Matrix client and the MCP send gate hold the same instance, so they cannot
disagree.

The file is read at most once per second and only when the allowlist is
consulted — on the next send, or the next `rooms`/`doctor` run. There is no
watcher goroutine, so there is nothing to leak, and no restart to forget. A
re-read compares the bytes it gets with the bytes it has instead of trusting
`mtime`, because timestamp resolution varies by filesystem (`/tmp` on this
machine truncates to the millisecond, the checkout does not) and a same-size
rewrite inside one tick would otherwise go unnoticed. An unreadable file keeps
the last known list and logs the problem once, and a malformed line is a
warning that names its line number and is otherwise skipped — never a crash.

`Add` and `Remove` rewrite the file through a temporary file in the same
directory plus a `rename`, so a concurrent reader sees either the old file or the
new one. They create the directory, keep the existing comments and ordering, and
report whether anything changed. An empty result is still read-only: the store
exposes no way to make the gate permissive.

### matrix

```go
package matrix

type Room struct {
    ID   string `json:"id"`
    Name string `json:"name"`
}

type Message struct {
    EventID   string `json:"event_id"`
    RoomID    string `json:"room_id"`
    Sender    string `json:"sender"`
    Body      string `json:"body"`
    MsgType   string `json:"msgtype"`
    Timestamp int64  `json:"timestamp"` // unix milliseconds
    Encrypted bool   `json:"encrypted"`
}

type MatrixService interface {
    JoinedRooms(ctx context.Context) ([]Room, error)
    Messages(ctx context.Context, roomID string, limit int) ([]Message, error)
    SendText(ctx context.Context, roomID, text string) (string, error)
    Sync(ctx context.Context) error
    Close() error
}

func New(cfg *config.Config, logger *log.Logger) (*Client, error)
```

`New` performs no network I/O: authentication, optional crypto setup, and the
sync loop all begin on the first `Sync` call, so downstream code can be tested
against a mock `MatrixService` with no homeserver in the loop. It does build the
`allowlist.Store`, which only stats and reads a local file, and that store is
reachable through `Client.Allowlist()` so the MCP layer gates on the same one.

Behavioral guarantees:

- `Sync` blocks until its context is cancelled (returns nil) or a fatal error
  occurs (invalid access token, token/user mismatch), with exponential backoff
  and jitter on transient failures.
- The initial sync uses `DontProcessOldEvents`, so historical backfill never
  triggers handlers.
- Events where `evt.Sender == client.UserID` are filtered out before reaching
  `OnEventType` handlers registered through `Client.OnEventType`.
- `Client.RoomName(roomID)` resolves cached room names without network I/O.
- With `MATRIX_AUTO_JOIN_INVITES=true`, an invitation whose inviter passes
  `MATRIX_INVITE_ALLOWLIST` (empty list = anyone) is accepted automatically and
  logged. The decision is made on the sync goroutine and the join request runs
  off it, deduplicated per room and joined by `Close`, so a slow or hanging join
  cannot stall the sync loop. Auto-join is a *sync listener* rather than an
  event handler on purpose: it is installed before `DontProcessOldEvents`, so
  the invitations carried by the first sync response — the ones that arrive
  while the bot is restarting — are accepted too. Refusals are logged and the
  invitation is left pending.
- `SendText` checks the `allowlist.Store`, not the static env slice, so a room
  enrolled in the file is sendable by the running process.
- `Messages` pages backward through `/messages` until it has the requested
  number of convertible messages or the room is exhausted. A page of raw events
  in a busy room is mostly `m.room.member`, edits and reactions, so a single
  page would often return far fewer messages than asked for. Paging is bounded
  by 10 pages and by `5 x limit` raw events scanned.
- A media event with an empty `body` (`m.image`, `m.video`, `m.audio`,
  `m.file`, ...) is reported with a bracketed type marker such as `[image]`
  derived from the `msgtype` alone, never invented content. Non-empty bodies
  and text-like types (`m.text`, `m.notice`, `m.emote`) are returned verbatim.
- `SendText` returns a `*matrix.BridgeNoticeError` (matching
  `matrix.ErrNotBridged`) when the bridge reports the message was not forwarded
  within `MATRIX_BRIDGE_ACK_TIMEOUT`.
- With `MATRIX_E2EE_ENABLED=true`, `New`/first `Sync` wires
  `crypto/cryptohelper` with a SQLite store at `MATRIX_CRYPTO_DB_PATH`, and
  `SendText` refuses plaintext sends into encrypted rooms
  (`ErrRoomEncrypted`) when E2EE is disabled.

### trigger

```go
package trigger

type Trigger struct {
    EventID   string `json:"event_id"`
    RoomID    string `json:"room_id"`
    RoomName  string `json:"room_name"`
    Sender    string `json:"sender"`
    Body      string `json:"body"`
    MsgType   string `json:"msgtype"`
    Timestamp int64  `json:"timestamp"` // unix milliseconds
}

type RoomNamer interface {
    RoomName(roomID string) string
}

type EventSource interface {
    OnEventType(t event.Type, handler mautrix.EventHandler)
}

func New(webhookURL string, namer RoomNamer, logger *log.Logger) *Dispatcher

func (d *Dispatcher) Attach(src EventSource)
func (d *Dispatcher) Recent(since int64, limit int) []Trigger
func (d *Dispatcher) Start(ctx context.Context)
func (d *Dispatcher) Close() error
```

`Dispatcher` subscribes to `m.room.message` events, keeps the last 100
triggers in a mutex-protected ring buffer (`Recent` returns newest-first), and
when `TRIGGER_WEBHOOK_URL` is set forwards each one over HTTP with a 5s
timeout and exactly one retry. Delivery is fully async on worker goroutines and
never blocks the sync loop; every goroutine exits on context cancellation and is
joined by `Close`.

### mcpserver

```go
package mcpserver

const (
    Name    = "matrix-whatsapp-mcp"
    Version = "0.1.0"
)

type RoomProber interface {
    IsBridged(ctx context.Context, roomID string) bool
    CanonicalAlias(ctx context.Context, roomID string) string
}

type TriggerReader interface {
    Recent(since int64, limit int) []trigger.Trigger
}

func New(svc matrix.MatrixService, prober RoomProber, recent TriggerReader,
    allowed []string) *mcp.Server

func NewWithAllowlist(svc matrix.MatrixService, prober RoomProber,
    recent TriggerReader, allow allowlist.List) *mcp.Server
```

`New` is the fixed-list form; `NewWithAllowlist` is what the binary uses, with
the client's own `allowlist.Store`, so `rooms allow` takes effect without a
restart.

`New` builds an `mcp.Server` with exactly four tools:

| Tool | Inputs | Annotations |
| --- | --- | --- |
| `list_rooms` | none | `readOnlyHint: true` |
| `read_messages` | `room_id` (required), `limit` (default 20, clamped 1..100) | `readOnlyHint: true` |
| `get_recent_triggers` | `since_timestamp` (unix millis), `limit` (default 20, clamped 1..100) | `readOnlyHint: true` |
| `send_message` | `room_id` (required), `text` (required, non-whitespace) | `destructiveHint: true`, `idempotentHint: false`, `readOnlyHint: false` |

`read_messages` returns events oldest-first; `get_recent_triggers` returns the
dispatcher's ring buffer newest-first. Every returned message carries its
`msgtype` and, on `read_messages`, its `encrypted` flag, so a media event is
never a blank body with no hint about what it was; a media event with no text
body reports a bracketed type marker (`[image]`, `[file]`, ...).

Bridged detection in `list_rooms` is best effort and never fails the whole
call. A room is `bridged: true` when an `m.bridge` state event is present on
the room; failing that, when any joined member's localpart contains
`whatsapp` (case-insensitive). Any probe error leaves `bridged: false` and
keeps the room. The room `name` is the room name, falling back to the
canonical alias, then to `unnamed`.

Sending is gated by an unexported `sendGate` type that combines the live
allowlist and a token bucket (one message per 3 seconds across all rooms). An
empty allowlist rejects every send with a read-only error; a room that is not in
a non-empty allowlist is rejected without consuming the token; a throttled call
returns `rate limited, retry in Ns` rather than blocking. The allowlist is
consulted per send, so a room added by `rooms allow` becomes sendable in the
running process. The access token is
never logged and never appears in an error or tool result.

Matrix accepting an event does not prove the bridge forwarded it. When a
bridge rejects a message it posts an `m.notice` into the room whose
`m.relates_to.event_id` is the sent event, for example:

> ⚠️ Your message was not bridged: You're not logged in (relay not set and
> receiver login is owned by @alice:example.com, not @bot:example.com)

`send_message` therefore fails closed on the bridge too. After the send,
`SendText` polls the room's newest events every 100ms for up to
`MATRIX_BRIDGE_ACK_TIMEOUT` (default 1s) looking for a notice from another
account that relates to the sent event. If one appears, the tool returns a
`*matrix.BridgeNoticeError` matching `matrix.ErrNotBridged`: it names the sent
event ID and quotes the notice, flattened to one line with control characters
removed. No notice within the window means normal success. With the timeout
set to `0` the guard is off entirely and the send issues no extra request.

Raise `MATRIX_BRIDGE_ACK_TIMEOUT` if your bridge is slow to post its notice:
the guard can only report what it sees inside the window. It matches a notice
that points at the sent event through either relation shape —
`m.relates_to.event_id` or `m.relates_to.m.in_reply_to.event_id`; mautrix-whatsapp
uses the reply form, so checking only the flat field misses every live
rejection.

The usual cause of a rejection notice is relay mode: `bridge.relay.enabled`
must be set in the bridge's config, and the sending account must be permitted
to relay through the bridge bot. Without it the bridge has no WhatsApp login to
speak for the sender and reports "not logged in".

### cli

```go
package cli

type RoomService interface {
    JoinedRooms(ctx context.Context) ([]matrix.Room, error)
    IsBridged(ctx context.Context, roomID string) bool
    WhoAmI(ctx context.Context) (userID, deviceID string, err error)
    Close() error
}

type Deps struct {
    Stdout     io.Writer
    Stderr     io.Writer
    Getenv     func(string) string
    NewClient  func(*config.Config, *log.Logger) (RoomService, error)
    HTTPClient *http.Client
}

func DefaultDeps(stdout, stderr io.Writer) Deps
func Run(ctx context.Context, args []string, deps Deps) int
```

`Run` returns the process exit code (`0` ok, `1` failed, `2` usage) and takes
every process edge through `Deps`, so the commands are tested against a fake
`RoomService`, a fake environment and an `httptest` server; no test touches a
real homeserver. Every subcommand is bounded by one context timeout, so an
unreachable homeserver produces an error rather than a terminal that never comes
back. Human output goes to stdout; diagnostics, warnings and the Matrix client's
own logger go to stderr, exactly like the server.

Room queries resolve in this order: exact room ID, exact name
(case-insensitive), then a case-insensitive substring of the name or the ID.
Multiple candidates are an error that lists them, never a guess. `--index N` uses
the row number printed by `rooms`, which comes from `JoinedRooms` sorted by name,
so the numbers are stable between the listing and the follow-up command.

### cmd/server

`cmd/server` loads the config, builds the Matrix client and dispatcher, starts
the sync loop in a managed goroutine, and serves the MCP server. With no
arguments that is all it does; any argument is dispatched to `cli.Run` instead,
so `rooms` and `doctor` exit rather than fall through to serving. With
`MCP_TRANSPORT=stdio` (the default) it serves `mcp.StdioTransport` on
stdin/stdout; with `MCP_TRANSPORT=http` it serves
`mcp.NewStreamableHTTPHandler` on `HTTP_ADDR` behind an `http.Server` with
read-header, read and idle timeouts but no `WriteTimeout`. The write deadline
is left at 0 on purpose: the Streamable HTTP transport holds a long-lived GET
SSE stream open for server→client messages, and a server-wide write deadline
would cut that stream. On SIGINT/SIGTERM (or a fatal sync error) the run
context is cancelled, the HTTP server and MCP session are shut down, the
dispatcher and Matrix client are closed, and every goroutine is joined before
the process exits. A fatal start or sync error exits non-zero; a graceful
shutdown exits zero. Logging goes to stderr only, so nothing but MCP frames
ever reaches stdout in stdio mode.

### Dependencies

- `maunium.net/go/mautrix` v0.33.0 — Matrix client, `crypto/cryptohelper`,
  SQLite state/crypto stores.
- `github.com/modelcontextprotocol/go-sdk` v1.8.0 — MCP protocol (used by
  Phase B; pinned here so the module graph is stable).

Both are the latest stable releases resolved from the module proxy. mautrix
v0.33.0 declares `go 1.26.0`, so this module's `go` directive is 1.26.0
rather than 1.23.

## Security

- No secrets are hardcoded or committed; `.env` is gitignored.
- Validation errors never echo secret values; `Config.String()` redacts them.
- Sending is disabled by default (empty allow-list = read-only).
