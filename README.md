# matrix-whatsapp-mcp

A Go MCP server that connects to a Matrix homeserver running a mautrix/whatsapp
bridge. It listens to incoming message events, keeps a small in-memory history,
optionally forwards them to a webhook, and exposes MCP tools for listing rooms,
reading messages, and sending replies (allow-list gated, fail closed).

Status: Phase B — the MCP server and its four tool handlers are wired and
served over stdio or Streamable HTTP. Phase C (review) and Phase D (tests and
scripts) remain.

## Requirements

- Go 1.26+ (the toolchain requirement is imposed by the current mautrix
  release; see Architecture > Dependencies)
- A Matrix homeserver account with an access token

## Configuration

All configuration is read from environment variables. See [.env.example](.env.example)
for the full list. In short:

| Variable | Required | Meaning |
| --- | --- | --- |
| MATRIX_HOMESERVER | yes | Homeserver base URL |
| MATRIX_USER_ID | yes | Bot Matrix user ID |
| MATRIX_ACCESS_TOKEN | yes | Bot access token (secret) |
| MATRIX_E2EE_ENABLED | no | Enable end-to-end encryption (default false) |
| MATRIX_CRYPTO_DB_PATH | no | SQLite crypto store path |
| MATRIX_PICKLE_KEY | no | Crypto pickle key; required when E2EE is on |
| TRIGGER_WEBHOOK_URL | no | Webhook to POST incoming messages |
| ALLOWED_SEND_ROOMS | no | Comma-separated room IDs allowed to send; empty = read-only |
| MCP_TRANSPORT | no | stdio (default) or http |
| HTTP_ADDR | no | Listen address for http transport (default :8080) |

Sending fails closed: unless a room is listed in ALLOWED_SEND_ROOMS, the server
will refuse to send to it.

## Running

```sh
cp .env.example .env   # then fill in real values
set -a; . ./.env; set +a
go run ./cmd/server
```

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

func (c *Config) ReadOnly() bool
func (c *Config) SendAllowed(roomID string) bool
func (c *Config) String() string // redacts secrets
func (c *Config) GoString() string
```

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
dispatcher's ring buffer newest-first.

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

### cmd/server

`cmd/server` loads the config, builds the Matrix client and dispatcher, starts
the sync loop in a managed goroutine, and serves the MCP server. With
`MCP_TRANSPORT=stdio` (the default) it serves `mcp.StdioTransport` on
stdin/stdout; with `MCP_TRANSPORT=http` it serves
`mcp.NewStreamableHTTPHandler` on `HTTP_ADDR` behind an `http.Server` with
read/write/idle timeouts. On SIGINT/SIGTERM (or a fatal sync error) the run
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
