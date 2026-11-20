# Tool reference

Nine tools, seven that only read and two that send or upload. Every tool is
registered with a JSON schema, a description, and MCP annotations; every result is
structured JSON, and `get_media` additionally returns the payload as an MCP content
block.
A call that cannot be answered fails as an MCP error with a message that names the
reason — no tool returns an empty result to mean "something went wrong".

## Conventions

**Room IDs.** `room_id` is validated by one function everywhere (the allowlist, the
client, the MCP layer): a leading `!`, at most 255 bytes, and no whitespace, control
characters, `/`, `?` or `#`. A `:server` part is optional, because the WhatsApp
bridge hands out room IDs without one.

**Event IDs.** Where an event ID is accepted (`event_id`, `reply_to`), it must be a
`$` sigil followed by the ID, at most 255 bytes, with no whitespace or control
characters. A malformed one is rejected before any request is made.

**Limits.** For every `limit` argument, an omitted, zero or negative value means 20,
and anything above 100 means 100. `limit_before` and `limit_after` on
`get_message_context` are the exception: they default to 10, are clamped to 0..50,
and an explicit `0` means none (see that tool).

**Reading is not gated by the send allowlist.** `ALLOWED_SEND_ROOMS` and the
allowlist file govern the two writing tools, `send_message` and `send_media`. The
read tools, `get_media` included, see every room the account has joined.

## The message object

`read_messages`, `search_messages` and `get_message_context` all return messages in
this one shape.

| Field | Type | Meaning |
| --- | --- | --- |
| `event_id` | string | Matrix event ID. |
| `room_id` | string | Room the event belongs to. |
| `sender` | string | Sender's Matrix user ID. |
| `body` | string | Message text. A non-text event that carries no caption gets a bracketed type marker such as `[file]`, derived from `msgtype` alone; a redacted message has an empty body. |
| `msgtype` | string | For example `m.text` or `m.notice`. |
| `timestamp` | integer | Unix milliseconds. |
| `encrypted` | boolean | Whether the event was encrypted. When it cannot be decrypted the body is the fixed placeholder `[encrypted message could not be decrypted]`, never ciphertext. |
| `reply_to` | string, optional | Event ID this message answers. Filled from either `m.relates_to.m.in_reply_to.event_id` or the flat `m.relates_to.event_id`, so the parent does not have to be in the same page. |
| `edit_of` | string, optional | Event ID this event replaces (`rel_type: m.replace`). The edit stays in the stream with this field set, so a client can apply it to the original instead of showing a duplicate. An edit fills `edit_of`, not `reply_to`. |
| `mentions` | array of strings, optional | User IDs from `m.mentions.user_ids`; absent when nobody is mentioned. |
| `redacted` | boolean | True when the room has redacted the message. The event keeps its ID, sender and timestamp, and the body is empty: the removed text is never returned. |
| `mimetype` | string, optional | MIME type of the attachment, from `content.info.mimetype`. Absent when the message carries no attachment. |
| `filename` | string, optional | The filename the sender set (`content.filename`). Absent when it set none; many clients put the filename in `body` instead, which is why `body` is never rewritten. |
| `size_bytes` | integer, optional | The attachment size the sender declared (`content.info.size`), in bytes. |
| `width` / `height` | integer, optional | Pixel dimensions the sender declared (`content.info.w` / `content.info.h`), typically for an image or a video. |
| `duration_ms` | integer, optional | The duration the sender declared (`content.info.duration`), in milliseconds, typically for audio or a video. |
| `mxc_uri` | string, optional | The attachment's content URI: `content.url` for plaintext media, `content.file.url` for encrypted media. Pass it back to the homeserver's media API, or use `get_media` to fetch the bytes. |

The media fields are additive: a message with no attachment omits all seven, and the
`body` placeholder (`[image]`, `[audio]`, …) is unchanged.

## list_rooms

Lists every room the account has joined, with a display name and a best-effort
"bridged from WhatsApp" flag.

- **Input:** none.
- **Output:** an array of `{room_id, name, bridged}`, sorted by name.
- **Name:** the room name; if the room has none, its canonical alias; otherwise
  `unnamed`.
- **Bridged:** true when the room carries an `m.bridge` state event; failing that,
  when any joined member's localpart contains `whatsapp` (case-insensitive). Probes
  are best effort, so an error means `bridged: false`, never a failed call.
- **Bounds:** probes run 8 at a time under a single 10-second deadline. Rooms whose
  probes did not finish are still listed, with `bridged: false` and the name the
  joined-room list carried.
- **Annotations:** `readOnlyHint: true`.

## read_messages

Reads one page of a room's history, oldest first.

| Input | Required | Default | Notes |
| --- | --- | --- | --- |
| `room_id` | yes | — | The room to read. |
| `limit` | no | 20 | Clamped to 1..100. |
| `before` | no | — | The `next_token` of an earlier result. Opaque: it is not an event ID. Omit it to start at the newest message. |

**Output**

```json
{
  "messages": [ { "event_id": "$...", "room_id": "!...", "sender": "@...", "body": "hi", "msgtype": "m.text", "timestamp": 1763600000000, "encrypted": false, "redacted": false } ],
  "has_more": true,
  "next_token": "eyJ2IjoxLCJyIjoiIXI6dGVzdCIsImMiOiJ0MTIzIn0.aBcDeF..."
}
```

The envelope exists because `structuredContent` is a JSON object and the cursor
needs somewhere to live. `next_token` is present only when there is history behind
this page: pass it back as `before` and repeat until `has_more` is false.

**The token.** `before` is issued by the running process: the homeserver's
`/messages` cursor plus the room it was issued for, signed with a per-process
HMAC key and base64url-encoded. It is opaque to the caller and holds no secret, so
it is safe to log. Before any request:

- a malformed, edited or foreign token, or one minted by a previous process, fails
  with `matrix: invalid page token` (wrapped `matrix.ErrInvalidPageToken`);
- a token this process issued but the homeserver no longer accepts fails with
  `matrix: page token is no longer valid` (`matrix.ErrStalePageToken`) — start again
  without `before`.

A restart invalidates outstanding tokens by design. They have no clock of ours.

**Bounds.** One call walks at most 10 `/messages` pages and scans at most
`5 × limit` raw events, with or without a token. A busy room's stream is mostly
non-message events, which is why a page of raw events can yield fewer messages than
asked for; the call keeps paging until it has enough messages, the room is
exhausted, or a bound is reached.

**`has_more`** reports that the homeserver has more events behind the page, not that
the next page contains messages. The remaining events can be membership or other
non-message events, in which case the next page is empty and carries its own
`next_token`.

**Annotations:** `readOnlyHint: true`.

## search_messages

Answers "where did someone say this", using the homeserver's search index first and
a bounded scan when the server has no index.

| Input | Required | Default | Notes |
| --- | --- | --- | --- |
| `query` | yes | — | Trimmed; must not be empty and is capped at 512 characters. |
| `room_id` | no | — | Restricts the search to one room. Omit it to search every joined room, within the scan budget. |
| `limit` | no | 20 | Clamped to 1..100. |
| `since_timestamp` | no | — | Unix milliseconds; keeps messages sent at or after this time (inclusive). |

The query is validated before any request: empty or whitespace-only is
`matrix: search query is empty` (`matrix.ErrEmptyQuery`), and more than 512
characters is an error naming the cap.

**Output**

```json
{
  "results": [ { "event_id": "$...", "room_id": "!...", "room_name": "Cafe", "body": "dinner at 8", "msgtype": "m.text", "timestamp": 1763600000000 } ],
  "truncated": false,
  "scanned_rooms": 0,
  "scanned_events": 0,
  "source": "server"
}
```

Each result is the message shape above plus `room_name`, newest first.

**Server path.** `POST /_matrix/client/v3/search` with `keys: ["content.body"]`,
`order_by: recent`, a message-event filter, and a room filter when `room_id` is
given. `truncated` is true when more matches exist than were returned, when the
server reports a next batch, or when its count exceeds what it returned.
`scanned_rooms` and `scanned_events` are 0 because nothing was scanned.
`since_timestamp` is applied to what the server returned.

**Fallback path.** A homeserver that does not implement `/search` — `M_UNRECOGNIZED`,
`M_NOT_FOUND`, `M_INVALID_PARAM`, `405`, `501`, or a bare 404 route — does not fail
the call. The refusal is remembered for the life of the process, and later searches
skip the doomed request. A network or authorization error is not treated this way:
it is returned to the caller, because degrading a timeout into "no matches" would
be a lie.

The scan is bounded in code:

| Bound | Value |
| --- | --- |
| Rooms read | 10 (`matrix.SearchMaxRooms`), taken from the joined-room list before any of them is named |
| Messages read per room | 50 (`matrix.SearchEventsPerRoom`), newest first |
| Messages read in total | 500 |

Matching is case-insensitive on `body`, and every whitespace-separated term of the
query must appear. Redacted messages never match. `truncated` is true whenever the
answer is partial: more matches than `limit`, a room with history behind the page
that was read, more rooms than the budget allows, or a room that could not be read.
One unreadable room is skipped and logged; a room the caller named explicitly is an
error when it cannot be read. `source` is `fallback` on this path, and the scanned
counters report what was actually read.

**Annotations:** `readOnlyHint: true`.

## get_message_context

Reads one message with its neighbourhood and resolves what it answers or replaces.

| Input | Required | Default | Notes |
| --- | --- | --- | --- |
| `room_id` | yes | — | The room the event belongs to. |
| `event_id` | yes | — | Matrix event ID; validated before any request. |
| `limit_before` | no | 10 | Clamped to 0..50; `0` returns none. |
| `limit_after` | no | 10 | Clamped to 0..50; `0` returns none. |

The two limits are nullable integers on purpose: an omitted field means 10, while an
explicit `0` means "nothing on that side", which a plain integer could not express.

**Output:** `{events_before, event, events_after, reply_to, edit_of, source, truncated}`.

| Field | Meaning |
| --- | --- |
| `event` | The target message, in the message shape above. |
| `events_before` | Up to `limit_before` messages before it, oldest first; message events only. |
| `events_after` | Up to `limit_after` messages after it, oldest first; message events only. |
| `reply_to` | The message the target answers, resolved to a full message when it is in the window or can be fetched; absent when the target is not a reply or the message is gone. The target's own `reply_to` field still carries the bare event ID. |
| `edit_of` | The message the target replaces, resolved the same way. |
| `source` | `context` when `GET /rooms/{roomId}/context/{eventId}` answered, `fallback` when the neighbourhood was found by scanning. |
| `truncated` | True only on the fallback path, when its fixed budget ran out before the requested window was covered. |

**Fallback.** When the homeserver does not implement `/context`, the event is fetched
with `GET /rooms/{roomId}/event/{eventId}` and the room's history is walked backward
until the target and the window are covered, the timeline ends, or a fixed budget of
5 pages × 100 raw events (`matrix.ContextScanPages`, `matrix.ContextScanPageLimit`)
is spent. The refusal is remembered per process. Resolving `reply_to` and `edit_of`
prefers the events already in the window and fetches only what is outside it; a
failed resolution leaves that one field absent rather than failing the call.

**Errors** are typed, because an empty context would claim something different:

| Situation | Error |
| --- | --- |
| `event_id` is not a Matrix event ID | `matrix: invalid event ID`, before any request |
| `room_id` is not a valid room ID | `matrix: invalid room ID`, before any request |
| the event is not in the room (`M_NOT_FOUND`) | `matrix: event not found in room` |
| the event exists but is not a message event | `matrix: event is not a message` |

**Annotations:** `readOnlyHint: true`.

## list_participants

Lists every joined member of a room.

- **Input:** `room_id` (required).
- **Output:** `{participants: [{user_id, display_name, membership, bridged, bridge_kind}]}`,
  sorted by display name (case-insensitive) and then user ID. `membership` is `join`
  for every entry, because the room is asked for its joined members only.
- **`bridged` / `bridge_kind`:** one heuristic shared with `list_rooms`:

| Localpart of the user ID | `bridged` | `bridge_kind` |
| --- | --- | --- |
| starts with `whatsapp_`, or `whatsapp_lid-`/`whatsapp-lid-` | true | `whatsapp_contact` |
| starts with `whatsapp` and ends with `bot` | true | `whatsapp_bot` |
| any other localpart containing `whatsapp` | true | `whatsapp_contact` |
| no match, but the member event carries `uk.half-shot.bridge`, `m.bridge` or `fi.mau.bridge` | true | `whatsapp_contact` |
| anything else | false | empty |

- **Bounds:** the whole call is bounded by a 10-second timeout. A member with no
  display name gets a profile lookup (8 in flight, 2 seconds each); a failed lookup
  leaves that one name empty and the member still listed.
- **Annotations:** `readOnlyHint: true`.

## get_media

Fetches one message's attachment and returns it inline. The result is a **content
array**, which is how text and an image arrive in one response: a `text` block
carrying the metadata as JSON, followed by an `image` or `audio` block when the
payload qualifies.

| Input | Required | Default | Notes |
| --- | --- | --- | --- |
| `room_id` | yes | — | The room the event belongs to. |
| `event_id` | yes | — | Matrix event ID; validated before any request. |
| `max_dimension` | no | 1568 | Longest edge an inline image may keep, clamped to 256..4096. An omitted or zero value means the default. |

**Metadata.** The first block's text is this JSON, and the same object is the tool's
structured content:

| Field | Meaning |
| --- | --- |
| `mimetype` | The MIME type the message declares; when it declares none, the type the homeserver served; when that is only `application/octet-stream`, the type the bytes themselves indicate. Used to choose the content block. |
| `filename` | The filename the sender set, when it set one. |
| `size_bytes` | The size of the payload that was downloaded; for encrypted media, the decrypted size. |
| `width` / `height` | The image's pixel size: the returned image after downscaling, or the source image when its payload did not fit. |
| `duration_ms` | The duration the sender declared, when it declared one. |
| `mxc_uri` | The attachment's content URI. |
| `encrypted` | True when the attachment was end-to-end encrypted and was decrypted before being returned. |
| `downscaled` | True when the returned image was resized or re-encoded smaller for `max_dimension` or the inline budget. |
| `included` | True when a second content block carries the payload. False means the text block is the whole answer. |
| `reason` | Present when `included` is false: why the payload was not included. |

**Reading path.** The event is read with `GET /rooms/{roomId}/event/{eventId}` and its
attachment descriptor comes from `content.url` (plaintext media) or `content.file`
(encrypted media) plus `content.info`. The bytes come from the media repository and
are held in memory only: nothing is cached and nothing is written to disk.

**How a payload is chosen.** Only images and audio become content blocks; a video, a
document and anything else returns metadata and text only, because base64-ing them
would spend the whole result on something no client renders.

| Payload | Result |
| --- | --- |
| `image/*` the standard library can decode (JPEG, PNG, GIF) | `image` block; downscaled and re-encoded to fit the budgets |
| `audio/*` within the inline budget | `audio` block, bytes unchanged |
| `audio/*` over the budget | metadata and text only, with `reason` |
| anything else | metadata and text only, with `reason` |

**Budgets.** All three are enforced, and none of them truncates a payload: if a
payload does not fit, the call returns its metadata and a reason.

| Budget | Value | Behaviour |
| --- | --- | --- |
| Download | 25 MiB (`matrix.MaxMediaDownloadBytes`) | A larger attachment is refused with `matrix: media is larger than the download limit`, checked against the declared length before a byte is read and against the actual bytes while reading. Nothing is truncated. |
| `max_dimension` | default 1568, clamped to 256..4096 | An image whose long edge is longer is scaled down with Catmull-Rom interpolation, preserving the aspect ratio. |
| Inline payload | about 1.5 MiB of base64 | The re-encoder lowers JPEG quality (85, 75, 65, 55, 45, 35) and then shrinks the image by 3/4 steps down to a 64-pixel long edge. PNG is kept where it fits; otherwise the image becomes JPEG, with transparency flattened onto white. An image that still does not fit returns metadata and a `reason`. |

An image already inside both the dimension and the budget is returned byte for byte:
re-encoding a small JPEG would only lose quality, and re-encoding a GIF would drop
its animation.

**Encryption.** Media in an encrypted room is encrypted at rest by the media
repository, so it is decrypted here: the event is decrypted with the crypto helper,
then the attachment with the key its own `content.file` carries, including the SHA-256
integrity check. With `MATRIX_E2EE_ENABLED=false`, encrypted media is refused with
`matrix: media is encrypted and MATRIX_E2EE_ENABLED is false` rather than returned as
ciphertext.

**Errors** are typed, because "no payload" from a download failure would claim
something different:

| Situation | Error |
| --- | --- |
| `event_id` is not a Matrix event ID | `matrix: invalid event ID`, before any request |
| `room_id` is not a valid room ID | `matrix: invalid room ID`, before any request |
| the event is not in the room (`M_NOT_FOUND`) | `matrix: event not found in room` |
| the event exists but carries no attachment | `matrix: event carries no media attachment` |
| the event is `m.room.encrypted` and E2EE is off | `matrix: media is encrypted and MATRIX_E2EE_ENABLED is false` |
| E2EE is on but the attachment cannot be decrypted | `matrix: encrypted media could not be decrypted` |
| the download is refused or fails | `matrix: media download failed` |
| the attachment is over 25 MiB | `matrix: media is larger than the download limit` |
| an unsupported image format or encryption algorithm | `matrix: unsupported media codec` |

**Annotations:** `readOnlyHint: true`.

## get_recent_triggers

Returns the incoming-message triggers this process has seen, newest first.

| Input | Required | Default | Notes |
| --- | --- | --- | --- |
| `since_timestamp` | no | — | Unix milliseconds; keeps triggers received strictly after this time. |
| `limit` | no | 20 | Clamped to 1..100. |

- **Output:** an array of `{event_id, room_id, room_name, sender, body, msgtype, timestamp}`, newest first.
- **What it is:** an in-memory, process-local ring buffer holding the newest 100
  triggers. It is a recent-activity view, not a transcript: it starts empty and is
  empty again after a restart.
- **Duplicates:** a trigger whose event ID is already in the buffer is not recorded
  twice.
- **Room name:** the cached display name, or the room ID when the name is not known.
- **Annotations:** `readOnlyHint: true`.

## send_message

Sends a plain-text message to a room.

| Input | Required | Default | Notes |
| --- | --- | --- | --- |
| `room_id` | yes | — | Must be on the send allowlist. |
| `text` | yes | — | Must not be empty or whitespace-only. |
| `reply_to` | no | — | Event ID to answer. Sent as `m.relates_to.m.in_reply_to`, which the bridge renders as a quote. The replied-to message can be in any room; only the target room must be allow-listed. |
| `idempotency_key` | no | — | At most 128 characters. Makes a retry safe. |

**Output:** `{event_id, deduplicated}`. `event_id` is the sent event's ID, or the
original send's when the call was deduplicated.

**The gate.** Before anything reaches the homeserver:

1. The room is checked against the effective allowlist — `ALLOWED_SEND_ROOMS` unioned
   with `MATRIX_ALLOWLIST_FILE`, consulted per send. Both empty rejects every send
   with a read-only error; a room missing from a non-empty list is rejected without
   consuming the rate-limit token.
2. One token bucket grants at most one message every 3 seconds across all rooms. A
   throttled call returns `rate limited, retry in Ns` instead of blocking.

The Matrix layer checks the same allowlist again, so the two cannot disagree.

**Replies.** A `reply_to` that is not a well-formed event ID is rejected before the
idempotency key is claimed and before the rate-limit token is spent.

**Idempotency.** A second call with the same key, room and text within 10 minutes
returns the original event ID with `deduplicated: true` and sends nothing. The same
key with a different room or text is an error, never a second send. A call that
arrives while the first is still in flight waits for it and returns its event ID
rather than racing it to the homeserver. A deduplicated call does not consume a
rate-limit token, and a send that failed releases its key so a retry can send. The
map is in-process, capped at 256 entries with the oldest evicted, and has no
persistence: a restart forgets every key. An in-flight entry is never expired or
evicted, so a hung send cannot turn a retry into a second message.

`idempotentHint` therefore stays `false`: only a keyed retry is idempotent, and the
default is not.

**Bridge acknowledgement.** Matrix accepting an event does not prove the bridge
forwarded it. After the send, the server polls the room's newest events every 100ms
for up to `MATRIX_BRIDGE_ACK_TIMEOUT` (default 1s), looking for an `m.notice` from
another account whose relation points at the sent event. If one appears, the call
fails with `matrix: bridge did not bridge the message: event $...: <notice>`: the
notice is flattened to one line, control characters are replaced, and the text is
capped at 300 characters before an ellipsis. No notice within the window means success. Setting the
timeout to `0` disables the guard and issues no extra request. A failing probe is
logged once and leaves the send a success.

**Encryption.** When `MATRIX_E2EE_ENABLED` is false and the room is encrypted, the
send is refused with `matrix: room is encrypted and MATRIX_E2EE_ENABLED is false`
instead of posting plaintext.

**Annotations:** `destructiveHint: true`, `idempotentHint: false`, `readOnlyHint: false`.

## send_media

Uploads one attachment and sends it as a single media message.

| Input | Required | Default | Notes |
| --- | --- | --- | --- |
| `room_id` | yes | — | Must be on the send allowlist. |
| `path` | one of | — | Absolute path of a local file. It must exist, be readable, be a regular file and be at most 50 MiB. Pass exactly one of `path` and `data_base64`. |
| `data_base64` | one of | — | The payload as standard base64, at most 50 MiB once decoded. `mimetype` is required alongside it. |
| `mimetype` | with `data_base64` | — | The payload's MIME type, for example `image/jpeg`. With `path` it overrides the type detected from the file, which is otherwise taken from the extension and then sniffed from the bytes. |
| `caption` | no | — | Sent as the message body, which WhatsApp shows under the media. When empty the filename is used, so the event always has a textual representation. |
| `filename` | no | base name of `path`, else `upload` | Recorded on the message and sent as the upload's filename. |
| `reply_to` | no | — | Event ID to answer, sent as `m.relates_to.m.in_reply_to`. A malformed one is rejected before anything is uploaded. |
| `idempotency_key` | no | — | At most 128 characters. Makes a retry safe. |

**Output:** `{event_id, deduplicated, msgtype, mxc_uri}`. `event_id` is the sent
event's ID, or the original send's when the call was deduplicated; `mxc_uri` is
absent on a replay because nothing was uploaded.

**Message type.** The MIME type decides how the bridge renders it in WhatsApp:

| MIME type | `msgtype` |
| --- | --- |
| `image/*` | `m.image` |
| `video/*` | `m.video` |
| `audio/*` | `m.audio` |
| anything else | `m.file` |

The message records `body` (the caption, or the filename), `filename`, and
`info.mimetype` with `info.size`. An outgoing image or video also carries
`info.w`/`info.h` read from its header, so the preview is laid out correctly.

**The gate.** `send_media` goes through exactly the same gate as `send_message`,
which is the point: a media message is no less a message to the people in the chat.

1. The room is checked against the effective allowlist — `ALLOWED_SEND_ROOMS` unioned
   with `MATRIX_ALLOWLIST_FILE`, consulted per send. Both empty rejects every upload
   and every send with the read-only error; a room outside a non-empty list is
   rejected without consuming the rate-limit token. The Matrix layer checks the same
   store again before uploading.
2. One token bucket grants at most one message every 3 seconds across all rooms, and
   it is the same bucket `send_message` draws from: a text message and a media message
   cannot be sent within 3 seconds of each other. A throttled call returns
   `rate limited, retry in Ns` instead of blocking.
3. After the send, the bridge-ack guard watches for the bridge's rejection notice
   exactly as `send_message` does, and fails the call with the notice rather than
   reporting a message as delivered that WhatsApp refused.

Everything that can be rejected without the homeserver is rejected before the gate:
a payload source that is neither `path` nor `data_base64` (or both), a path that is
not absolute, unreadable, a directory or not a regular file, invalid base64, a
`data_base64` call without `mimetype`, a payload over 50 MiB, a malformed `reply_to`
and an over-long `idempotency_key`. Nothing is uploaded and no rate-limit token is
spent for any of them.

**Idempotency.** The cache is the same one `send_message` uses, so the key namespace is
shared, but a media entry is identified by a SHA-256 of the payload plus the filename,
mimetype, caption and reply target. A retry with the same key and the same bytes
returns the first event ID with `deduplicated: true` and **uploads nothing**; the same
key with different bytes is the usual conflict error, never a second upload. The
10-minute window, the in-memory lifetime and the 256-entry cap are the ones described
under `send_message`, and `idempotentHint` is `false` for the same reason: only a
keyed retry is idempotent.

**Encryption.** With E2EE on and an encrypted room, the attachment is encrypted
before upload and the event carries `content.file`, so the media repository never
holds a readable copy. With E2EE off, an encrypted room is refused with
`matrix: room is encrypted and MATRIX_E2EE_ENABLED is false`, exactly as a text send
is.

**Logging.** The payload bytes are never logged, and neither is a file's contents;
errors name the failure, the file path and the size, never the bytes.

**Annotations:** `destructiveHint: true`, `idempotentHint: false`, `readOnlyHint: false`.

## Annotations

| Tool | `readOnlyHint` | `destructiveHint` | `idempotentHint` |
| --- | --- | --- | --- |
| `list_rooms` | true | — | — |
| `read_messages` | true | — | — |
| `search_messages` | true | — | — |
| `get_message_context` | true | — | — |
| `list_participants` | true | — | — |
| `get_media` | true | — | — |
| `get_recent_triggers` | true | — | — |
| `send_message` | false | true | false |
| `send_media` | false | true | false |

`openWorldHint` is left unset on every tool, which means true: each one reaches a
real homeserver and, through it, real people.

## Examples

Page back through a room:

```json
{"name": "read_messages", "arguments": {"room_id": "!cafe:example.com", "limit": 30}}
```

```json
{"name": "read_messages", "arguments": {"room_id": "!cafe:example.com", "limit": 30, "before": "eyJ2IjoxLCJyIjoiIXI6dGVzdCIsImMiOiJ0MTIzIn0.aBcDeF..."}}
```

Find a phrase and reply to the match:

```json
{"name": "search_messages", "arguments": {"query": "dinner", "limit": 5}}
```

```json
{"name": "send_message", "arguments": {"room_id": "!family:example.com", "text": "8pm works", "reply_to": "$matchEventID", "idempotency_key": "reply-dinner-1"}}
```

Inspect a message before answering it:

```json
{"name": "get_message_context", "arguments": {"room_id": "!family:example.com", "event_id": "$targetEventID", "limit_before": 3, "limit_after": 0}}
```

Read the photo in a message, downscaled to 1024 pixels on its long edge:

```json
{"name": "get_media", "arguments": {"room_id": "!family:example.com", "event_id": "$photoEventID", "max_dimension": 1024}}
```

Send a photo with a caption, answering an earlier message:

```json
{"name": "send_media", "arguments": {"room_id": "!family:example.com", "path": "/home/me/Pictures/dinner.jpg", "caption": "here it is", "reply_to": "$questionEventID", "idempotency_key": "photo-dinner-1"}}
```
