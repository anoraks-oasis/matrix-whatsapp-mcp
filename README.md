# matrix-whatsapp-mcp

A Go MCP server that connects to a Matrix homeserver running a mautrix/whatsapp
bridge. It listens to incoming message events, keeps a small in-memory history,
optionally forwards them to a webhook, and exposes MCP tools for listing rooms,
reading messages, and sending replies (allow-list gated, fail closed).

Status: Phase D — the codebase is covered by unit and protocol tests (run under
the race detector), a smoke script exercises the built binary end to end, and
`go vet`/`gofmt` are clean.

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
| `ALLOWED_SEND_ROOMS` | no | — (empty) | `!room1:example.com,!UBRW6WycBKpUJu3m4HP0RCMsGnUp_EwSZa2RLXHe34Y` | Comma-separated room IDs allowed to receive outbound messages; a `:server` suffix is optional. Empty = read-only (sending fails closed). |
| `MATRIX_BRIDGE_ACK_TIMEOUT` | no | `1s` | `1s`, `500ms`, `0` | How long a send waits for the bridge's rejection notice. `0` disables the check. |
| `MCP_TRANSPORT` | no | `stdio` | `stdio` or `http` | How the MCP server is exposed. |
| `HTTP_ADDR` | no | `:8080` | `127.0.0.1:8080` | Listen address when `MCP_TRANSPORT=http`. |

See [.env.example](.env.example) for a copy-paste template. Sending fails
closed: unless a room is listed in `ALLOWED_SEND_ROOMS`, the server refuses to
send to it, and a send the WhatsApp bridge rejects is reported as a failure
rather than a success (see `MATRIX_BRIDGE_ACK_TIMEOUT`).

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
- **Sending is rejected** — sending fails closed. Add the room ID to
  `ALLOWED_SEND_ROOMS`; an empty value means read-only mode. Room IDs the
  bridge lists without a `:server` suffix are valid and can be allow-listed
  as-is.

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
}

func Load() (*Config, error)

// ValidRoomID is the single source of truth for room ID shape: a leading "!",
// at most 255 bytes, and no whitespace, control characters, "/", "?", or "#".
// A ":server" part is *not* required — the WhatsApp bridge hands out room IDs
// without one.
func ValidRoomID(roomID string) bool

func (c *Config) ReadOnly() bool
func (c *Config) SendAllowed(roomID string) bool
func (c *Config) String() string // redacts secrets
func (c *Config) GoString() string
```

`ValidRoomID` is what both `Load()` (for `ALLOWED_SEND_ROOMS`) and
`matrix.Client` (for `Messages` and `SendText`) use, so the allow-list and the
client can never disagree about which room IDs are acceptable.

`Load()` validates every variable and returns a joined error of
`*config.VarError` values. Error messages name the offending variable and
never echo its value, so tokens and pickle keys cannot leak through errors.

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
against a mock `MatrixService` with no homeserver in the loop.

Behavioral guarantees:

- `Sync` blocks until its context is cancelled (returns nil) or a fatal error
  occurs (invalid access token, token/user mismatch), with exponential backoff
  and jitter on transient failures.
- The initial sync uses `DontProcessOldEvents`, so historical backfill never
  triggers handlers.
- Events where `evt.Sender == client.UserID` are filtered out before reaching
  `OnEventType` handlers registered through `Client.OnEventType`.
- `Client.RoomName(roomID)` resolves cached room names without network I/O.
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
```

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

Sending is gated by an unexported `sendGate` type that combines the allow-list
and a token bucket (one message per 3 seconds across all rooms). An empty
`ALLOWED_SEND_ROOMS` rejects every send with a read-only error; a room not in
a non-empty list is rejected without consuming the token; a throttled call
returns `rate limited, retry in Ns` rather than blocking. The access token is
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
the guard can only report what it sees inside the window. It also only matches
notices that carry `m.relates_to` pointing at the sent event, which is what
mautrix-whatsapp emits.

The usual cause of a rejection notice is relay mode: `bridge.relay.enabled`
must be set in the bridge's config, and the sending account must be permitted
to relay through the bridge bot. Without it the bridge has no WhatsApp login to
speak for the sender and reports "not logged in".

### cmd/server

`cmd/server` loads the config, builds the Matrix client and dispatcher, starts
the sync loop in a managed goroutine, and serves the MCP server. With
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
