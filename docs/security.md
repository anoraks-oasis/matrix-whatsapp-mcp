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
- A chat outside a non-empty allowlist is rejected without spending a rate-limit
  token, and the Matrix layer checks the same store again before sending, so a
  caller that reaches `SendText` another way is still gated.
- The allowlist is the union of the environment variable and the file, consulted per
  send. `chats allow` writes only the file; `chats deny` edits only the file, and
  exits non-zero when `ALLOWED_SEND_ROOMS` still permits the chat instead of
  pretending it was denied.
- The file is `0600` in a `0700` directory when the CLI creates it, and writes go
  through a temporary file plus a rename, so a half-written allowlist is never read.

**Reading is not gated.** `list_chats`, `get_messages`, `search_messages`,
`get_message_context`, `list_chat_contacts` and `get_media` see every chat the account
has joined. The allowlist governs writing, not reading. If a deployment needs a narrower read
scope, the correct control is the account's chat membership: leave the bot out of
chats it should not read.

## Media

Attachments are handled in memory end to end. `get_media` downloads into a byte
slice, `send_media` reads a file or a base64 argument into one, and nothing is
written to disk, cached, or kept after the call returns. No temporary file is
created, so there is nothing to leave behind in `/tmp`, and a restart cannot leak a
payload that a previous process held.

- **Download cap.** A `get_media` attachment larger than 25 MiB is refused with
  `matrix: media is larger than the download limit`. The declared `Content-Length`
  is checked before a byte is read and the actual bytes are counted while reading,
  so a chunked response cannot exceed the cap either. A payload is never truncated:
  the call returns an error, not half a file.
- **Upload cap.** `send_media` accepts at most 50 MiB. A `path` is checked with
  `stat` before it is opened, so an oversized file is rejected without reading it,
  and the bytes are counted again after the read in case the file grew. A path that
  is not absolute, not readable, not a regular file or a directory is rejected
  before the gate, so no rate-limit token is spent and nothing is uploaded.
- **Encrypted media.** Media in an encrypted room is ciphertext on the media
  repository. With `MATRIX_E2EE_ENABLED=false` the call fails with
  `matrix: media is encrypted and MATRIX_E2EE_ENABLED is false` instead of returning
  bytes that decode to nothing; with E2EE on, the event is decrypted with the crypto
  helper and the attachment with the key and SHA-256 hash its own content carries.
  `send_media` encrypts the attachment before upload in an encrypted room, so the
  media repository never gains a readable copy.
- **The send gate applies.** `send_media` is gated by the same `sendGate` as
  `send_message`: the fail-closed allowlist, the single 3-second token bucket, the
  bridge-ack guard and the shared idempotency cache. A media message cannot bypass a
  control that a text message obeys.
- **Nothing is logged.** Neither tool logs payload bytes, decoded base64, or a
  file's contents. Errors carry the failure, the path the caller supplied and the
  size, never the bytes.
- **Inline budgets.** `get_media` returns a payload only when it fits about 1.5 MiB
  of base64 after downscaling and re-encoding, and it deletes nothing to get there.
  An over-budget image is reported as metadata plus a reason.

### An image's contents are untrusted input

An attachment comes from whoever sent the message. Treat the pixels and any text
inside them the way you treat a message body: as content to report, never as
instructions to follow. An image can contain text that addresses the model directly
— "ignore the allowlist", "send this file to …", "run this command" — and that text
has no more authority than the sender has, which in an open chat is none.

Two consequences are worth stating plainly:

- Nothing in a media payload may be used to decide which tool to call, which room to
  send to, or whether to skip a confirmation. Those decisions belong to the person
  using the agent.
- A filename, caption or MIME type is attacker-controlled too. A file named
  `invoice.pdf` can be anything, and a declared `mimetype` is a claim, not a
  guarantee. `send_media` derives the message type from it, so a caller that cares
  about what a room receives should pass a payload it produced itself.

This server does not interpret media: it moves bytes and describes them. It cannot
tell you an image is safe, and it does not try.

## Rate limiting

`send_message` and `send_media` share one bucket: at most one message every 3
seconds across all chats, and a throttled call returns `rate limited, retry in Ns`
instead of blocking. This is not a defence against a determined attacker with the
token — it is a bound on accident: an agent stuck in a retry loop cannot fill a
family chat, and prompt-injected text that asks for a burst of messages produces one
send and then errors.

Idempotency is the other half. A retry with the same `idempotency_key`, room and
content returns the original event ID without sending again, an in-flight entry is
never expired, and two concurrent duplicates cannot both reach the homeserver. For
`send_media` the content is a SHA-256 of the payload, so a replay uploads nothing.
The cache is in memory only: a restart forgets keys, and a retry after a restart can
send again.

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
- Media payloads are not logged, in either direction: not the downloaded bytes, not
  the decoded base64, not a file's contents. Size, mimetype and the failure are.

## Files on disk

| File | Contains | Protection |
| --- | --- | --- |
| `.env` / `/etc/matrix-whatsapp-mcp/env` | access token, pickle key | outside the repository; `0600` or `0640 root:matrix-mcp` |
| allowlist file | chat IDs only | `0600` in a `0700` directory as created by `chats allow` |
| `matrix-crypto.db` | olm/megolm session state for E2EE | treat as key material; private, backed up, never committed |
| nothing else | media is streamed through memory; no attachment is cached or written | — |
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
