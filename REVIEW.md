# Phase C review — matrix-whatsapp-mcp

Re-reviewed at HEAD `3b4e244` (fix wave `d99c7e7`–`7d64dd8` + Phase D tests `e28a750`, `edfa5d0`, `bce28ef`, `3b4e244`). The previous review was at `c53fa55`.

Blocking issues: 0

## Re-audit of previous findings

| ID | Severity | Status | Fixed by | Notes |
| --- | --- | --- | --- | --- |
| F1 | blocking | **fixed** | d99c7e7 | README Prerequisites/Build/Troubleshooting document `-tags goolm`, every build/run/test command carries it, and a `Makefile` wraps it (`TAGS ?= goolm`, targets `build`/`run`/`test`/`vet`). |
| F2 | non-blocking | **fixed** | 167c1dc | `.gitignore` now has `*.db`, `*.db-wal`, `*.db-shm`, `*.db-journal`, `*.sqlite*`. |
| F3 | non-blocking | **fixed** | 85075ef | `WriteTimeout` removed from the HTTP server; only the explanatory comment remains. Grep for `WriteTimeout\|SetWriteDeadline` over the tree finds no live call, only comments. |
| F4 | nit | **fixed** | 7d64dd8 | `list_rooms` probes bounded to 8-way concurrency via a semaphore; per-index `results[i]` preserves room order, and cancellation still surfaces as `ctx.Err()` after `wg.Wait()`. Error semantics are unchanged (probes stay best-effort). |

## New findings from the re-audit

| ID | Severity | Area | Location | What is wrong | Why it matters | Fix |
| --- | --- | --- | --- | --- | --- | --- |
| N1 | nit | tests | internal/trigger/dispatcher.go:28-30,86-88 | No test pins the production webhook defaults; every timing test injects a fast `timeout`/`retryDelay`, so a green suite does not prove the shipped values. | The 5s / exactly-1-retry contract is load-bearing and was just refactored onto struct fields; a future change could silently alter it and the suite would stay green. | Add a `TestNewDispatcherDefaults` asserting `d.timeout == 5*time.Second`, `d.retries == 1`, `d.retryDelay == 500*time.Millisecond`. |

The production default is nonetheless correct — see the proof below. N1 is a test-coverage gap, not a code defect.

## Production webhook default (re-audit item 2)

The shipped defaults are literally 5s and exactly 1 retry, verified by inspection, not by the fast-timeout tests:

- `webhookTimeout = 5 * time.Second` (internal/trigger/dispatcher.go:28), `webhookRetries = 1` (dispatcher.go:30), `webhookRetryDelay = 500 * time.Millisecond` (dispatcher.go:29).
- `New()` assigns all three onto the struct (dispatcher.go:86-88); `cmd/server/main.go:55` calls `trigger.New(...)` with no overrides.
- The 5s bound is enforced per request by `context.WithTimeout(ctx, d.timeout)` inside `post` (dispatcher.go:254), so it applies even though the `http.Client` no longer carries its own `Timeout`.
- The retry loop (dispatcher.go:234-250) issues exactly `1 + d.retries` posts: `attempt 0` (initial) plus `attempt 1` (the single retry) before the `attempt >= d.retries` guard returns. With `retries == 1` that is exactly 2 requests.

## Test quality review (re-audit item 3)

- **Rate limiter is clock-injected** — `TestSendGateRateLimitWithInjectedClock` (internal/mcpserver/server_test.go:497) sets `g.now` to a fake clock and advances it; `TestSendGateRejectedSendDoesNotConsumeToken` and `TestSendGateEmptyAllowlistRejects` cover the remaining gate invariants. No sleeps in the gate tests.
- **Backfill is distinguished from incremental** — `TestSyncBackfillFiltering` (internal/matrix/client_test.go:64) drives a real `/sync` handshake against an `httptest` server: the initial sync's `$old` backfill event is dropped, the first incremental sync's `$new` event is dispatched, and the result is asserted to be exactly `[$new]`.
- **Retry test counts exactly 2 requests** — `TestWebhookExactlyOneRetry` (internal/trigger/dispatcher_test.go:154) polls until `calls == 2` then sleeps `3 * retryDelay` and re-asserts it did not become 3.
- **No test can reach a real homeserver** — matrix tests use `httptest.NewServer` or `http://127.0.0.1:1` (and the latter paths — self-filter registration, empty-text rejection — make no network call). `scripts/smoke_test.sh` is offline by default and only runs reachability/`list_rooms` when the operator explicitly supplies their own `MATRIX_*` credentials.
- **Sleeps** — a few bounded sleeps/polls exist (`5ms` poll with deadline, a `50ms` drain after `close(release)`, `3 * retryDelay` settle). They are bounded and support assertions that rest on channels/atomic counters plus deadlines, not on the sleeps alone; the timing-critical gate is clock-injected. Acceptable, mildly flaky-prone at worst.

## Full gate output (re-audit item 4), from a clean tree at `3b4e244`

```
$ go build -tags goolm ./...            # exit 0, no output
$ go vet   -tags goolm ./...            # exit 0, no output
$ gofmt -l .                            # exit 0, no output
$ go mod tidy -diff                     # exit 0, no output
$ go test -race -count=1 -tags goolm ./...
?   github.com/ButterHost69/matrix-whatsapp-mcp/cmd/server           [no test files]
?   github.com/ButterHost69/matrix-whatsapp-mcp/internal/config       [no test files]
?   github.com/ButterHost69/matrix-whatsapp-mcp/internal/logger       [no test files]
ok  github.com/ButterHost69/matrix-whatsapp-mcp/internal/matrix       1.022s
ok  github.com/ButterHost69/matrix-whatsapp-mcp/internal/mcpserver    1.088s
ok  github.com/ButterHost69/matrix-whatsapp-mcp/internal/trigger      1.702s
$ bash scripts/smoke_test.sh
INFO: building binary with -tags goolm
PASS: built executable binary with -tags goolm
PASS: binary exits non-zero and names every missing variable
INFO: skipping homeserver reachability check: MATRIX_HOMESERVER/MATRIX_USER_ID/MATRIX_ACCESS_TOKEN not all set
INFO: running offline stdio JSON-RPC round trip
PASS: stdio round trip returned all four tools
INFO: skipping authenticated section: MATRIX_HOMESERVER/MATRIX_USER_ID/MATRIX_ACCESS_TOKEN not all set
INFO: smoke test complete
# exit 0
```

## What was verified, and how (re-audit item 5)

1. **stdout discipline** — `grep -rnE 'os\.Stdout|fmt\.Print|println\(|print\('` over every `.go` file, test files included, returns nothing. Logging is `log.New(os.Stderr, …)` (internal/logger/logger.go:14) and mautrix `zerolog.New(os.Stderr)` (internal/matrix/client.go:132); the only stdout writer is the SDK stdio transport (`nopCloserWriter{os.Stdout}`), and the SDK's own logger is `slog.DiscardHandler` when unconfigured.
2. **goroutine lifetime / races** — unchanged wiring in `cmd/server/main.go` (signal watcher, sync loop, dispatcher workers, HTTP server all share the run ctx and are joined). New tests pin the join behaviour: `TestCloseJoinsBackgroundWork` proves `Close` unblocks a background goroutine mid-request, `TestSyncBackfillFiltering` proves `Sync` returns nil after cancel, and `TestConcurrentBufferAccess` runs 8 writers × 8 readers under `-race`.
3. **backfill / self-message filtering** — `DontProcessOldEvents` is registered before the first sync (internal/matrix/client.go:519-522); self-sender is dropped in `Client.OnEventType` before the handler (client.go:163-170). Both now have direct tests (`TestSyncBackfillFiltering`, `TestOnEventTypeFiltersSelfMessages`).
4. **ring buffer** — unchanged 100-capacity `sync.RWMutex` ring; `Recent` is strict `Timestamp > since`, newest-first. Pinned by `TestRingBufferBeforeFill`, `TestRingBufferWrapsAtCapacity` (evicts 1..50, keeps 51..150), `TestRecentSinceTimestampBoundary` (strict boundary + non-nil empty), `TestConcurrentBufferAccess`.
5. **webhook dispatch** — async, per-request 5s timeout, exactly 1 retry, non-blocking enqueue, bounded queue 256 × 4 workers; defaults proven above; `TestWebhookDispatchIsAsync`, `TestWebhookExactlyOneRetry`, `TestWebhookTimesOut` cover the behaviour.
6. **send_message** — fail-closed gate unchanged and now unit-tested at both layers: handler rejections (`TestSendMessageRejections`) and clock-injected gate invariants (`TestSendGate*`). Empty allow-list, non-listed room, whitespace, one-per-3s, and reject-without-consuming-token are all asserted.
7. **secrets** — tree and full `git log -p --all` contain no real tokens; the only `syt_` hits are placeholders (`syt_...`, `syt_your_access_token`) and the smoke script's `smoke-test-token` example. `.env` is gitignored; `Config.String`/`GoString` still redact.
8. **MCP surface** — still exactly four tools with correct annotations and required/optional schemas; now enforced by `TestToolsListHasFourToolsWithSchemas` and exercised end-to-end by the smoke script's `tools/list` round trip.
9. **config** — unchanged single-read `LoadFrom` with typed `VarError` and redacting `String`; smoke script asserts the missing-config error names each variable and says "required but not set" without values.
10. **build hygiene** — the full gate above passes from a clean tree, including `go test -race -count=1`, which is no longer vacuous (matrix/mcpserver/trigger all carry tests).

## Could not verify

- **Live Matrix behaviour** — no homeserver or credentials exist here, so the sync loop, E2EE decryption, and an authenticated `list_rooms`/`send_message` were not exercised against a real server (the smoke script's authenticated section is skipped without `MATRIX_*`).
- **Webhook in flight** — no live webhook endpoint to observe the 5s timeout and retry end to end; verified by code inspection plus the local `httptest` suite.
- **SSE stream lifetime** — F3 is established from the go-sdk (`http.NewResponseController` flush, no hijack) and net/http deadline semantics; not reproduced against a live Streamable HTTP client.
