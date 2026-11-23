# Development

## Toolchain

- **Go 1.26 or newer.** `go.mod` declares `go 1.26.0` because mautrix v0.33.0 does.
  With the default `GOTOOLCHAIN=auto`, an older local Go downloads the 1.26 toolchain
  from the module proxy on first build.
- **gcc and cgo.** `github.com/mattn/go-sqlite3` — the store behind the crypto and
  state backends — is compiled from C. `CGO_ENABLED=0` will not build it.
- **The `goolm` build tag.** mautrix selects its olm crypto backend at compile time.
  With `-tags goolm` it uses the pure-Go implementation and needs nothing installed;
  without it, it compiles the C binding and fails unless libolm is present. Every
  command below carries the tag, and so do the Makefile, the smoke script and CI.

```sh
export GOFLAGS=-tags=goolm   # optional; the Makefile and CI set it per command
```

## The four gates

A change is ready when all four pass from a clean tree:

```sh
go build -tags goolm ./...
go vet -tags goolm ./...
gofmt -l .                      # must print nothing
go test -race -count=1 -tags goolm ./...
```

`-race` is not optional in the gate list: the client, the dispatcher and the
allowlist store are all concurrent, and the interesting failures are races.
`-count=1` defeats the test cache so a green run is a run.

`make test` and `make vet` run the tagged commands; `make build` produces
`./matrix-whatsapp-mcp` at the repository root (gitignored). A failing `gofmt -l .`
is fixed with `gofmt -w .`, not by a formatting-only commit that also changes code.

## Tests

```sh
go test -tags goolm ./...                 # fast pass
go test -tags goolm -race -count=1 ./...  # the gate
go test -tags goolm -race -run TestSearch ./internal/matrix
```

No test needs a homeserver. The design supports that on purpose:

- Everything above `internal/matrix` is written against the `MatrixService`
  interface, so the MCP tools and the CLI are tested with a mock.
- HTTP paths are exercised with `httptest` servers that answer the exact Matrix
  endpoints involved.
- Time-dependent behaviour — rate limiting, idempotency expiry, webhook retries — is
  tested with injected clocks and shortened timeouts rather than sleeps.
- `internal/cli` takes every process edge through `Deps` (writers, `getenv`, client
  constructor, HTTP client), so a command can be run in a test without a terminal, a
  real environment or a network.

The suite is also where the invariants live: stdout purity in stdio mode, goroutine
lifetime on shutdown, pagination bounds counted in requests, one send under
concurrent retries, and redaction of secrets from anything a config can be printed
as.

## Smoke test

```sh
./scripts/smoke_test.sh
```

It builds the binary with `-tags goolm` into a temporary directory and:

1. asserts the build produced an executable;
2. runs the binary with an empty environment and checks it exits non-zero naming
   `MATRIX_HOMESERVER`, `MATRIX_USER_ID` and `MATRIX_ACCESS_TOKEN`;
3. runs `chats`, `rooms` and `doctor` the same way, proving they do not fall through
   to serving MCP and hang on stdin;
4. performs a real stdio JSON-RPC round trip — `initialize`, `notifications/initialized`,
   `tools/list` — against a throwaway `127.0.0.1:9` configuration and asserts all
   nine tool names come back;
5. only when `MATRIX_HOMESERVER`, `MATRIX_USER_ID` and `MATRIX_ACCESS_TOKEN` are all
   set, checks `/versions` reachability and an authenticated `list_chats` call.

The script never prints the access token. Overrides: `GOOLM_TAG` (build tag) and
`SMOKE_STDIO_SETTLE` (seconds stdin is held open, default 2).

## Coverage, and the `covdata` quirk

```sh
go test -tags goolm -cover ./...
```

On a machine whose Go 1.26 toolchain was downloaded by `GOTOOLCHAIN`, this can exit
`1` with `go: no such tool "covdata"` for the packages that have **no test files**
(`cmd/server`, `internal/logger`). The downloaded toolchain is missing
`pkg/tool/<goos>_<goarch>/covdata`, so coverage merging for a package without tests
fails.

So `./internal/...` alone is not enough: it includes `internal/logger`. Ask for the
packages that have tests directly:

```sh
go test -tags goolm -cover ./internal/{allowlist,cli,config,matrix,mcpserver,trigger}
```

That exits `0` and prints a number per package. Nothing in this repository causes the
`covdata` failure and no repository change fixes it; a locally installed Go toolchain
of the same version does not have the problem. Treat it as an environment quirk when
you reproduce coverage numbers.

## Continuous integration

`.github/workflows/ci.yml` runs on every push and pull request to `main` on
`ubuntu-latest`, with `actions/setup-go` reading the version from `go.mod` and the
module cache enabled. `GOFLAGS: -tags=goolm` is set at the job level so no step can
silently miss the tag, and the steps are exactly the four gates above. There is no OS
matrix: the code is platform-neutral apart from cgo/SQLite, and one Linux runner is
the supported target.

The workflow also verifies that a formatting regression fails the build rather than
being reported and ignored:

```sh
unformatted="$(gofmt -l . | tee /dev/stderr | wc -l)"
test "$unformatted" -eq 0
```

## Repository layout

```
cmd/server/            entrypoint: config, wiring, serve, shutdown
internal/config/       environment → Config, chat-ID rule
internal/logger/       the stderr logger
internal/matrix/       mautrix wrapper: sync, reads, search, context, send, E2EE
internal/allowlist/    env ∪ file send allowlist, atomic edits
internal/trigger/      m.room.message → ring buffer + webhook
internal/mcpserver/    nine tools, send gate, idempotency cache, in-memory media
internal/cli/          chats, chats allow|deny (rooms is a hidden alias), doctor
scripts/smoke_test.sh  end-to-end smoke checks
deploy/                systemd unit, env template, host layout
docs/                  tool reference, architecture, operations, security, reviews
```
