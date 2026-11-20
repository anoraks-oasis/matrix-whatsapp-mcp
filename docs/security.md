# Security

This server holds a Matrix account's access token and can write into real
conversations. Two things follow from that: the token is the most sensitive value in
the deployment, and the send allowlist is the only thing standing between a model and
the people in a chat.

## Token handling

- The access token and the pickle key are read from the environment. Nothing is
  written to disk except the crypto store and the allowlist file; `.env` and `*.db*`
  are gitignored.
- Validation never echoes a value. A `config` error names the variable and says what
  is wrong with it (`config: MATRIX_ACCESS_TOKEN: required but not set`), never the
  content. `Config.String()` and `Config.GoString()` print the token and pickle key
  as `<redacted>` and the webhook URL as `<set>`, so `%v`, `%+v` and `%#v` are all
  safe.
- The token is never put into a tool result, an error message or a log line. The
  Matrix client's own logger writes to stderr at warning level, so its request logs
  name methods and URLs, not authorization headers.
- The server verifies on the first sync that the token belongs to `MATRIX_USER_ID`
  and refuses to run on a mismatch, which catches the common copy-paste of the wrong
  account's token before anything is read.
- If a token is exposed: revoke the device in the homeserver's admin tooling or from
  Element, issue a new token, update the environment, restart. Treat anything the
  account could read in the meantime as disclosed.

## The allowlist is the send gate

Sending fails closed by construction:

- An empty effective allowlist — `ALLOWED_SEND_ROOMS` and the allowlist file both
  empty — rejects every send as read-only.
- A room outside a non-empty allowlist is rejected without spending a rate-limit
  token, and the Matrix layer checks the same store again before sending, so a
  caller that reaches `SendText` another way is still gated.
- The allowlist is the union of the environment variable and the file, consulted per
  send. `rooms allow` writes only the file; `rooms deny` edits only the file, and
  exits non-zero when `ALLOWED_SEND_ROOMS` still permits the room instead of
  pretending it was denied.
- The file is `0600` in a `0700` directory when the CLI creates it, and writes go
  through a temporary file plus a rename, so a half-written allowlist is never read.

**Reading is not gated.** `list_rooms`, `read_messages`, `search_messages`,
`get_message_context` and `list_participants` see every room the account has joined.
The allowlist governs writing, not reading. If a deployment needs a narrower read
scope, the correct control is the account's room membership: leave the bot out of
rooms it should not read.

## Rate limiting

`send_message` accepts at most one message every 3 seconds across all rooms, and a
throttled call returns `rate limited, retry in Ns` instead of blocking. This is not a
defence against a determined attacker with the token — it is a bound on accident: an
agent stuck in a retry loop cannot fill a family chat, and prompt-injected text that
asks for a burst of messages produces one send and then errors.

Idempotency is the other half. A retry with the same `idempotency_key`, room and text
returns the original event ID without sending again, an in-flight entry is never
expired, and two concurrent duplicates cannot both reach the homeserver. The cache is
in memory only: a restart forgets keys, and a retry after a restart can send again.

## The relay caveat

The bridge only forwards a message when the sending account has a WhatsApp login it
can speak for. Without relay mode the bridge posts a rejection notice into the room
while Matrix happily accepts the event. The bridge-ack guard watches for that notice
for `MATRIX_BRIDGE_ACK_TIMEOUT` and fails the send with the notice's text, but it can
only report what it sees inside the window: a bridge that posts its notice later than
the timeout lets the call succeed. `MATRIX_BRIDGE_ACK_TIMEOUT=0` turns the guard off
entirely.

The guard is detection, not proof of delivery. It does not confirm that a phone
received anything; it only catches the bridge's own rejection notice.

## Pagination tokens

`before` is a signed token, not the homeserver's cursor: the cursor, a version and
the room are HMAC-SHA256 signed with a random key generated at process start,
truncated to 16 bytes and base64url-encoded. It carries no secret, is bound to one
room, may not exceed 1024 bytes, and is rejected before any request when malformed,
edited, foreign or issued by a previous process. A restart therefore invalidates
outstanding tokens, and a caller cannot use a token to page a room it was not issued
for. Verification checks the shape and the signature only; the reason strings never
echo the token.

## What must never be logged

- The access token, the pickle key, and the full contents of the environment file.
- Webhook URLs that carry credentials: transport errors are stripped of the URL
  before they are logged.
- Untrusted remote text verbatim: a bridge rejection notice is flattened to one line,
  control characters are replaced with spaces, and the result is capped at 300
  characters before it reaches a tool result or a log.
- Message bodies are not logged. Logs name room IDs, event IDs, senders and error
  causes.

## Files on disk

| File | Contains | Protection |
| --- | --- | --- |
| `.env` / `/etc/matrix-whatsapp-mcp/env` | access token, pickle key | outside the repository; `0600` or `0640 root:matrix-mcp` |
| allowlist file | room IDs only | `0600` in a `0700` directory as created by `rooms allow` |
| `matrix-crypto.db` | olm/megolm session state for E2EE | treat as key material; private, backed up, never committed |
| built binary | nothing secret | gitignored |

When E2EE is on, losing the crypto store means losing the ability to decrypt existing
sessions; leaking it means leaking those sessions' keys. Back it up somewhere private,
or accept re-initialising the device.

## HTTP transport

`MCP_TRANSPORT=http` serves a Streamable HTTP endpoint with no authentication of its
own. Bind it to loopback (`HTTP_ADDR=127.0.0.1:8080`) and put an authenticated reverse
proxy in front of anything that must reach it from elsewhere. Anyone who can reach the
endpoint can call the tools, including `send_message`, subject only to the allowlist
and rate limit.

## Reporting a vulnerability

Please do not open a public issue for a security problem, and never paste a token, a
pickle key, a `.env` file or a crypto store into an issue, a pull request or a chat.

Use GitHub's private vulnerability reporting on this repository: **Security → Report a
vulnerability** (Security Advisories). That starts a private thread visible only to the
maintainers. Include the commit or version, what an attacker needs, the impact, and
reproduction steps that do not contain live credentials.

If a credential has already been exposed, revoke it first; a report about a token that
is still valid is a live incident, not a bug report.
