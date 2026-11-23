# Tool reference

Nine tools, seven that only read and two that send or upload. Every tool is
registered with a JSON schema, a description, and MCP annotations; every result is a
structured JSON object, never a bare array, so a tool can grow a field such as a count or
a cursor without changing the shape a model reads, and `get_media` additionally returns
the payload as an MCP content block. A call that cannot be answered fails as an MCP error with a short WhatsApp-
facing message — no tool returns an empty result to mean "something went wrong".

Every tool that acts on a chat accepts the chat either by `chat_id` or by `chat`, a
name or a unique case-insensitive substring of one (see [Chat by name](#chat-by-name)).

## Terminology

The tools speak WhatsApp, because the agent calling them is doing WhatsApp work. The IDs
underneath are Matrix IDs, supplied by the bridge.

| Word | Meaning |
| --- | --- |
| `chat`, `chat_id`, `chat` (input) | One WhatsApp conversation. The ID is a chat reference such as `!abc:example.com`; the bridge sometimes omits the `:server` part, which is valid. The `chat` input also accepts the display name. |
| `message`, `message_id` | One WhatsApp message. The ID is a message reference such as `$abc123`. |
| `contact` | A participant in a chat: a WhatsApp contact or group, the bridge bot, or a plain Matrix account. |
| `type` | `text`, `image`, `video`, `audio`, `document`, `sticker`, `location`, or `other`. |
| `whatsapp` | Whether the chat or contact belongs to WhatsApp rather than being a plain Matrix room or account. |
| `media_uri` | An opaque reference to an attachment's stored bytes. |

| Old name | New name |
| --- | --- |
| `list_rooms`, `read_messages`, `list_participants`, `get_recent_triggers` | `list_chats`, `get_messages`, `list_chat_contacts`, `get_new_messages` |
| `room_id`, `event_id`, `since_timestamp` | `chat_id`, `message_id`, `since` |
| `room_name`, `sender`, `body`, `msgtype`, `redacted`, `mxc_uri`, `bridged`, `bridge_kind` | `chat_name`, `from`, `text`, `type`, `deleted`, `media_uri`, `whatsapp`, `contact_type` |
| `scanned_rooms`, `scanned_events`, `participants` | `scanned_chats`, `scanned_messages`, `contacts` |

The old **input** names are still accepted as deprecated aliases, documented in the schema
of every field that has one: a model that guesses `room_id` or `event_id` still works, and
the canonical name wins when both are sent. The old **output** names are gone; a result
never carries `room_id`, `event_id`, `body`, `msgtype`, `redacted`, `mxc_uri`, `bridged` or
`bridge_kind`.

Two things deliberately keep Matrix vocabulary: the configuration surface (`MATRIX_*`,
`ALLOWED_SEND_ROOMS`, the allowlist file), which names the mechanism it configures, and
the `matrix_user` contact type, which names a participant who is not a WhatsApp contact.
Everything an agent reads — tool descriptions, errors, results and the webhook payload —
speaks WhatsApp. The webhook (`TRIGGER_WEBHOOK_URL`) carries `chat_id`, `chat_name`,
`message_id`, `from`, `text`, `type`, `timestamp` and, when the message has an
attachment, `mimetype`, `filename`, `size_bytes` and `media_uri`.

## Conventions

**Chat IDs.** `chat_id` is validated by one function everywhere (the allowlist, the client,
the MCP layer): a leading `!`, at most 255 bytes, and no whitespace, control characters,
`/`, `?` or `#`. A `:server` part is optional, because the WhatsApp bridge hands out chat
IDs without one. The deprecated alias `room_id` is validated identically.

**Message IDs.** Where a message ID is accepted (`message_id`, `reply_to`), it must be a `$`
sigil followed by the ID, at most 255 bytes, with no whitespace or control characters. A
malformed one is rejected before any request is made. `event_id` is its deprecated alias.

### Chat by name

`get_messages`, `search_messages` (when scoped), `get_message_context`,
`list_chat_contacts`, `get_media`, `send_message` and `send_media` accept `chat` as an
alternative to `chat_id`. Resolution order is:

1. an exact chat ID from the joined-chat list;
2. an exact name, case-insensitive;
3. a case-insensitive substring of exactly one name.

An ambiguous query and a query that matches nothing are errors that list candidate chat
names (at most 10), so a model can correct itself without another listing call. Sending
`chat` and `chat_id` together is an error, never a silent preference.

A name is only a way to find a chat. The resolved chat ID goes through exactly the same
checks as one passed as `chat_id`, which means the send allowlist still decides whether
`send_message` and `send_media` may write to it: naming a chat never grants permission.

Resolving a name reads the joined-chat list, so a call by `chat_id` is the cheaper path
when the ID is already known.

**Limits.** For every `limit` argument, an omitted, zero or negative value means 20, and
anything above 100 means 100. `limit_before` and `limit_after` on `get_message_context` are
the exception: they default to 10, are clamped to 0..50, and an explicit `0` means none (see
that tool).

**Reading is not gated by the send allowlist.** `ALLOWED_SEND_ROOMS` and the allowlist file
govern the two writing tools, `send_message` and `send_media`. The read tools, `get_media`
included, see every chat the account has joined.

## The message object

`get_messages`, `search_messages` and `get_message_context` all return messages in this one
shape.

| Field | Type | Meaning |
| --- | --- | --- |
| `chat_id` | string | The chat the message belongs to: a chat reference such as `!abc:server`. |
| `chat_name` | string | The display name of that chat, falling back to the chat ID when the name is not known. A single chat's page names it once; search results name the chat each match came from. |
| `message_id` | string | The message ID: a message reference such as `$abc123`. |
| `from` | string | The sender's user ID. |
| `text` | string | The message text. A media message with no caption gets a bracketed type marker such as `[image]` or `[document]`, derived from `type` alone; a deleted message has empty text. |
| `type` | string | The WhatsApp type: `text`, `image`, `video`, `audio`, `document`, `sticker`, `location`, or `other`. See the mapping below. |
| `timestamp` | integer | Unix milliseconds when the message was sent. |
| `encrypted` | boolean | Whether the message was encrypted. When it cannot be decrypted the text is the fixed placeholder `[encrypted message could not be decrypted]`, never ciphertext. |
| `reply_to` | string, optional | Message ID this message answers. Filled from either the nested reply relation or its flat form, so the parent does not have to be in the same page. |
| `edit_of` | string, optional | Message ID this message replaces. The edit stays in the stream with this field set, so a client can apply it to the original instead of showing a duplicate. An edit fills `edit_of`, not `reply_to`. |
| `mentions` | array of strings, optional | User IDs the message mentions; absent when nobody is mentioned. |
| `deleted` | boolean | True when the message was deleted. It keeps its ID, sender and timestamp, and the text is empty: the removed text is never returned. |
| `mimetype` | string, optional | MIME type of the attachment. Absent when the message carries none. |
| `filename` | string, optional | The filename the sender set. Absent when it set none; many clients put the filename in `text` instead, which is why `text` is never rewritten. |
| `size_bytes` | integer, optional | The attachment size the sender declared, in bytes. |
| `width` / `height` | integer, optional | Pixel dimensions the sender declared, typically for an image or a video. |
| `duration_ms` | integer, optional | The duration the sender declared, in milliseconds, typically for audio or a video. |
| `media_uri` | string, optional | An opaque reference to the attachment's stored bytes, valid for plaintext and encrypted media alike. Pass it back to the server's media API, or use `get_media` to fetch the bytes. |

The media fields are additive: a message with no attachment omits all seven, and the `text`
placeholder (`[image]`, `[audio]`, `[document]`, …) is unchanged apart from the file marker,
which is `[document]`.

### Message type mapping

The Matrix message type never reaches the caller; it is translated once, at the MCP
boundary.

| Matrix type | `type` | Placeholder when there is no caption |
| --- | --- | --- |
| `m.text`, `m.notice`, `m.emote` | `text` | none: an empty text stays empty |
| `m.image` | `image` | `[image]` |
| `m.video` | `video` | `[video]` |
| `m.audio` | `audio` | `[audio]` |
| `m.file` | `document` | `[document]` |
| `m.sticker` | `sticker` | `[sticker]` |
| `m.location` | `location` | `[location]` |
| anything else | `other` | `[other]` |

## list_chats

Lists every chat the account has joined, with a display name and a best-effort
"is this a WhatsApp chat" flag.

- **Input:** none.
- **Output:** an object `{"chats": [{chat_id, chat_name, whatsapp}]}`, sorted by chat name.
- **`chat_name`:** the chat name; if the chat has none, its canonical alias; otherwise `unnamed`.
- **`whatsapp`:** true when the chat carries a bridge state marker; failing that, when any
  joined contact's localpart contains `whatsapp` (case-insensitive). Probes are best effort,
  so an error means `whatsapp: false`, never a failed call.
- **Bounds:** probes run 8 at a time under a single 10-second deadline. Chats whose probes
  did not finish are still listed, with `whatsapp: false` and the name the joined-chat list
  carried.
- **Annotations:** `readOnlyHint: true`.

## get_messages

Reads one page of a chat's history, oldest first.

| Input | Required | Default | Notes |
| --- | --- | --- | --- |
| `chat_id` | one of | — | The chat to read, or omit it and pass `chat`. `room_id` is its deprecated alias. |
| `chat` | one of | — | The chat's name, or a unique case-insensitive substring of one. Not with `chat_id`. |
| `limit` | no | 20 | Clamped to 1..100. |
| `before` | no | — | The `next_token` of an earlier result. Opaque: it is not a message ID. Omit it to start at the newest message. |

**Output**

```json
{
  "messages": [ { "chat_id": "!...", "chat_name": "Cafe", "message_id": "$...", "from": "@...", "text": "hi", "type": "text", "timestamp": 1763600000000, "encrypted": false, "deleted": false } ],
  "has_more": true,
  "next_token": "eyJ2IjoxLCJyIjoiIXI6dGVzdCIsImMiOiJ0MTIzIn0.aBcDeF..."
}
```

`chat_name` is resolved once per call — the joined chat's display name, then its
canonical alias, then the chat ID — inside a short bound, so a page of messages costs one
name lookup rather than one per message and an unreadable name never fails the call.

The envelope exists because `structuredContent` is a JSON object and the cursor needs
somewhere to live. `next_token` is present only when there is history behind this page: pass
it back as `before` and repeat until `has_more` is false.

**The token.** `before` is issued by the running process: the server's pagination cursor
plus the chat it was issued for, signed with a per-process HMAC key and base64url-encoded.
It is opaque to the caller and holds no secret, so it is safe to log. Before any request:

- a malformed, edited or foreign token, or one minted by a previous process, fails with
  `invalid page token` (wrapped `matrix.ErrInvalidPageToken`);
- a token this process issued but the server no longer accepts fails with
  `page token is no longer valid: the server rejected it, so call again without before`
  (`matrix.ErrStalePageToken`).

A restart invalidates outstanding tokens by design. They have no clock of ours.

**Bounds.** One call walks at most 10 pages and scans at most `5 × limit` raw entries, with
or without a token. A busy chat's stream is mostly non-message entries, which is why a page
of raw entries can yield fewer messages than asked for; the call keeps paging until it has
enough messages, the chat is exhausted, or a bound is reached.

**`has_more`** reports that the server has more entries behind the page, not that the
next page contains messages. The remaining entries can be membership or other non-message
changes, in which case the next page is empty and carries its own `next_token`.

**Annotations:** `readOnlyHint: true`.

## search_messages

Answers "where did someone say this", using the server's search index first and a
bounded scan when the server has no index.

| Input | Required | Default | Notes |
| --- | --- | --- | --- |
| `query` | yes | — | Trimmed; must not be empty and is capped at 512 characters. |
| `chat` | no | — | Restricts the search to the named chat, or a unique case-insensitive substring of its name. Not with `chat_id`. |
| `chat_id` | no | — | Restricts the search to one chat. Omit both it and `chat` to search every joined chat, within the scan budget. `room_id` is its deprecated alias. |
| `limit` | no | 20 | Clamped to 1..100. |
| `since` | no | — | Unix milliseconds; keeps messages sent at or after this time (inclusive). `since_timestamp` is its deprecated alias. |

The query is validated before any request: empty or whitespace-only is
`search query is empty` (`matrix.ErrEmptyQuery`), and more than 512 characters is an
error naming the cap. An unavailable search index is `search is unavailable on this server`
when the failure is not the endpoint itself; a server without that endpoint degrades to
the bounded scan instead.

**Output**

```json
{
  "results": [ { "chat_id": "!...", "chat_name": "Cafe", "message_id": "$...", "from": "@...", "text": "dinner at 8", "type": "text", "timestamp": 1763600000000 } ],
  "truncated": false,
  "scanned_chats": 0,
  "scanned_messages": 0,
  "source": "server"
}
```

Each result is the message shape above, newest first. A search can span chats, so a
result's `chat_name` is how it says which chat the match came from.

**Server path.** `POST /_matrix/client/v3/search` with `keys: ["content.body"]`,
`order_by: recent`, a message filter, and a chat filter when `chat_id` is given. `truncated` is
true when more matches exist than were returned, when the server reports a next batch, or
when its count exceeds what it returned. `scanned_chats` and `scanned_messages` are 0 because
nothing was scanned. `since` is applied to what the server returned.

**Fallback path.** A server that does not implement `/search` — `M_UNRECOGNIZED`,
`M_NOT_FOUND`, `M_INVALID_PARAM`, `405`, `501`, or a bare 404 route — does not fail the call.
The refusal is remembered for the life of the process, and later searches skip the doomed
request. A network or authorization error is not treated this way: it is returned to the
caller, because degrading a timeout into "no matches" would be a lie.

The scan is bounded in code:

| Bound | Value |
| --- | --- |
| Chats read | 10 (`matrix.SearchMaxRooms`), taken from the joined-chat list before any of them is named |
| Messages read per chat | 50 (`matrix.SearchEventsPerRoom`), newest first |
| Messages read in total | 500 |

Matching is case-insensitive on the message text, and every whitespace-separated term of the
query must appear. Deleted messages never match. `truncated` is true whenever the answer is
partial: more matches than `limit`, a chat with history behind the page that was read, more
chats than the budget allows, or a chat that could not be read. One unreadable chat is
skipped and logged; a chat the caller named explicitly is an error when it cannot be read.
`source` is `fallback` on this path, and the scanned counters report what was actually read.

**Annotations:** `readOnlyHint: true`.

## get_message_context

Reads one message with its neighbourhood and resolves what it answers or replaces.

| Input | Required | Default | Notes |
| --- | --- | --- | --- |
| `chat` | one of | — | The chat's name, or a unique case-insensitive substring of one. Not with `chat_id`. |
| `chat_id` | one of | — | The chat the message belongs to, or omit it and pass `chat`. `room_id` is its deprecated alias. |
| `message_id` | yes | — | Message reference such as `$abc123`; validated before any request. `event_id` is its deprecated alias. |
| `limit_before` | no | 10 | Clamped to 0..50; `0` returns none. |
| `limit_after` | no | 10 | Clamped to 0..50; `0` returns none. |

The two limits are nullable integers on purpose: an omitted field means 10, while an explicit
`0` means "nothing on that side", which a plain integer could not express.

**Output:** `{messages_before, message, messages_after, reply_to, edit_of, source, truncated}`.

| Field | Meaning |
| --- | --- |
| `message` | The target message, in the message shape above. |
| `messages_before` | Up to `limit_before` messages before it, oldest first; messages only. |
| `messages_after` | Up to `limit_after` messages after it, oldest first; messages only. |
| `reply_to` | The message the target answers, resolved to a full message when it is in the window or can be fetched; absent when the target is not a reply or the message is gone. The target's own `reply_to` field still carries the bare message ID. |
| `edit_of` | The message the target replaces, resolved the same way. |
| `source` | `context` when the chat's `/context` endpoint answered, `fallback` when the neighbourhood was found by scanning. |
| `truncated` | True only on the fallback path, when its fixed budget ran out before the requested window was covered. |

**Fallback.** When the server does not implement `/context`, the message is fetched by ID
and the chat's history is walked backward until the target and the window are covered, the
timeline ends, or a fixed budget of 5 pages × 100 raw entries (`matrix.ContextScanPages`,
`matrix.ContextScanPageLimit`) is spent. The refusal is remembered per process. Resolving
`reply_to` and `edit_of` prefers the messages already in the window and fetches only what is
outside it; a failed resolution leaves that one field absent rather than failing the call.

**Errors** are typed, because an empty context would claim something different:

| Situation | Error |
| --- | --- |
| `message_id` is not a message reference | `invalid message ID: expected a message reference such as $abc123`, before any request |
| `chat_id` is not a valid chat ID | `invalid chat ID: expected a chat reference such as !abc:server`, before any request |
| the message is not in the chat (`M_NOT_FOUND`) | `message not found in this chat` |
| the entry exists but is not a message | `this message is not a text message` |

**Annotations:** `readOnlyHint: true`.

## list_chat_contacts

Lists every joined contact in a chat.

- **Input:** `chat_id` or `chat` (exactly one; `room_id` is the deprecated alias of `chat_id`).
- **Output:** `{contacts: [{user_id, display_name, membership, whatsapp, contact_type}]}`,
  sorted by display name (case-insensitive) and then user ID. `membership` is `join` for
  every entry, because the chat is asked for its joined contacts only.
- **`whatsapp` / `contact_type`:** one heuristic shared with `list_chats`:

| Localpart of the user ID | `whatsapp` | `contact_type` |
| --- | --- | --- |
| starts with `whatsapp_`, or `whatsapp_lid-`/`whatsapp-lid-` | true | `whatsapp_contact` |
| starts with `whatsapp` and ends with `bot` | true | `bridge_bot` |
| any other localpart containing `whatsapp` | true | `whatsapp_contact` |
| no match, but the member details carry `uk.half-shot.bridge`, `m.bridge` or `fi.mau.bridge` | true | `whatsapp_contact` |
| anything else | false | `matrix_user` |

- **Bounds:** the whole call is bounded by a 10-second timeout. A contact with no display
  name gets a profile lookup (8 in flight, 2 seconds each); a failed lookup leaves that one
  name empty and the contact still listed.
- **Annotations:** `readOnlyHint: true`.

## get_media

Fetches one message's attachment and returns it inline. The result is a **content array**,
which is how text and an image arrive in one response: a `text` block carrying the metadata
as JSON, followed by an `image` or `audio` block when the payload qualifies.

| Input | Required | Default | Notes |
| --- | --- | --- | --- |
| `chat` | one of | — | The chat's name, or a unique case-insensitive substring of one. Not with `chat_id`. |
| `chat_id` | one of | — | The chat the message belongs to, or omit it and pass `chat`. `room_id` is its deprecated alias. |
| `message_id` | yes | — | Message reference such as `$abc123`; validated before any request. `event_id` is its deprecated alias. |
| `max_dimension` | no | 1568 | Longest edge an inline image may keep, clamped to 256..4096. An omitted or zero value means the default. |

**Metadata.** The first block's text is this JSON, and the same object is the tool's
structured content:

| Field | Meaning |
| --- | --- |
| `mimetype` | The MIME type the message declares; when it declares none, the type the server served; when that is only `application/octet-stream`, the type the bytes themselves indicate. Used to choose the content block. |
| `filename` | The filename the sender set, when it set one. |
| `size_bytes` | The size of the payload that was downloaded; for encrypted media, the decrypted size. |
| `width` / `height` | The image's pixel size: the returned image after downscaling, or the source image when its payload did not fit. |
| `duration_ms` | The duration the sender declared, when it declared one. |
| `media_uri` | The attachment's opaque reference. |
| `encrypted` | True when the attachment was end-to-end encrypted and was decrypted before being returned. |
| `downscaled` | True when the returned image was resized or re-encoded smaller for `max_dimension` or the inline budget. |
| `included` | True when a second content block carries the payload. False means the text block is the whole answer. |
| `reason` | Present when `included` is false: why the payload was not included. |

**Reading path.** The message is read by ID and its attachment descriptor comes from the
plaintext media reference or the encrypted one plus its info block. The bytes come from the
media repository and are held in memory only: nothing is cached and nothing is written to
disk.

**How a payload is chosen.** Only images and audio become content blocks; a video, a document
and anything else returns metadata and text only, because base64-ing them would spend the
whole result on something no client renders.

| Payload | Result |
| --- | --- |
| `image/*` the standard library can decode (JPEG, PNG, GIF) | `image` block; downscaled and re-encoded to fit the budgets |
| `audio/*` within the inline budget | `audio` block, bytes unchanged |
| `audio/*` over the budget | metadata and text only, with `reason` |
| anything else | metadata and text only, with `reason` |

**Budgets.** All three are enforced, and none of them truncates a payload: if a payload does
not fit, the call returns its metadata and a reason.

| Budget | Value | Behaviour |
| --- | --- | --- |
| Download | 25 MiB (`matrix.MaxMediaDownloadBytes`) | A larger attachment is refused with `attachment is too large to download`, checked against the declared length before a byte is read and against the actual bytes while reading. Nothing is truncated. |
| `max_dimension` | default 1568, clamped to 256..4096 | An image whose long edge is longer is scaled down with Catmull-Rom interpolation, preserving the aspect ratio. |
| Inline payload | about 1.5 MiB of base64 | The re-encoder lowers JPEG quality (85, 75, 65, 55, 45, 35) and then shrinks the image by 3/4 steps down to a 64-pixel long edge. PNG is kept where it fits; otherwise the image becomes JPEG, with transparency flattened onto white. An image that still does not fit returns metadata and a `reason`. |

An image already inside both the dimension and the budget is returned byte for byte:
re-encoding a small JPEG would only lose quality, and re-encoding a GIF would drop its
animation.

**Encryption.** Media in an encrypted chat is encrypted at rest by the media repository, so
it is decrypted here: the message is decrypted with the crypto helper, then the attachment
with the key it carries, including the SHA-256 integrity check. With
`MATRIX_E2EE_ENABLED=false`, encrypted media is refused with
`this message is encrypted and end-to-end encryption is switched off on the server; set MATRIX_E2EE_ENABLED=true`
rather than returned as ciphertext.

**Errors** are typed, because "no payload" from a download failure would claim something
different:

| Situation | Error |
| --- | --- |
| `message_id` is not a message reference | `invalid message ID: expected a message reference such as $abc123`, before any request |
| `chat_id` is not a valid chat ID | `invalid chat ID: expected a chat reference such as !abc:server`, before any request |
| the message is not in the chat (`M_NOT_FOUND`) | `message not found in this chat` |
| the message exists but carries no attachment | `this message has no attachment` |
| the message is encrypted and E2EE is off | `this message is encrypted and end-to-end encryption is switched off on the server; set MATRIX_E2EE_ENABLED=true` |
| E2EE is on but the attachment cannot be decrypted | `this attachment could not be decrypted` |
| the download is refused or fails | `could not download the attachment` |
| the attachment is over 25 MiB | `attachment is too large to download` |
| an unsupported image format or encryption algorithm | `unsupported attachment format` |

**Annotations:** `readOnlyHint: true`.

## get_new_messages

Returns the incoming messages this process has seen, newest first.

| Input | Required | Default | Notes |
| --- | --- | --- | --- |
| `since` | no | — | Unix milliseconds; keeps messages received strictly after this time. `since_timestamp` is its deprecated alias. |
| `limit` | no | 20 | Clamped to 1..100. |

- **Output:** an object `{"messages": [{message_id, chat_id, chat_name, from, text, type, timestamp}]}`,
  newest first.
- **What it is:** an in-memory, process-local ring buffer holding the newest 100 messages. It
  is a recent-activity view, not a transcript: it starts empty and is empty again after a
  restart.
- **Duplicates:** a message whose ID is already in the buffer is not recorded twice.
- **Chat name:** the cached display name, or the chat ID when the name is not known.
- **Annotations:** `readOnlyHint: true`.

## send_message

Sends a plain-text message to a chat.

| Input | Required | Default | Notes |
| --- | --- | --- | --- |
| `chat_id` | one of | — | The chat to send to, or omit it and pass `chat`. Must be on the send allowlist. `room_id` is its deprecated alias. |
| `chat` | one of | — | The chat's name, or a unique case-insensitive substring of one. Not with `chat_id`. Resolving a name never grants send permission. |
| `text` | yes | — | Must not be empty or whitespace-only. |
| `reply_to` | no | — | Message ID to answer, sent as a reply relation that the bridge renders as a quote. The replied-to message can be in any chat; only the target chat must be allow-listed. |
| `idempotency_key` | no | — | At most 128 characters. Makes a retry safe. |

**Output:** `{message_id, deduplicated}`. `message_id` is the sent message's ID, or the
original send's when the call was deduplicated.

**The gate.** Before anything reaches the server, and after `chat` is resolved to an ID:

1. The chat is checked against the effective allowlist — `ALLOWED_SEND_ROOMS` unioned with
   `MATRIX_ALLOWLIST_FILE`, consulted per send. Both empty rejects every send with a read-only
   error; a chat missing from a non-empty list is rejected without consuming the rate-limit
   token.
2. One token bucket grants at most one message every 3 seconds across all chats. A throttled
   call returns `rate limited, retry in Ns` instead of blocking.

The client layer checks the same allowlist again, so the two cannot disagree.

**Replies.** A `reply_to` that is not a well-formed message ID is rejected before the
idempotency key is claimed and before the rate-limit token is spent.

**Idempotency.** A second call with the same key, chat and text within 10 minutes returns the
original message ID with `deduplicated: true` and sends nothing. The same key with a
different chat or text is an error, never a second send. A call that arrives while the first
is still in flight waits for it and returns its message ID rather than racing it to the
server. A deduplicated call does not consume a rate-limit token, and a send that failed
releases its key so a retry can send. The map is in-process, capped at 256 entries with the
oldest evicted, and has no persistence: a restart forgets every key. An in-flight entry is
never expired or evicted, so a hung send cannot turn a retry into a second message.

`idempotentHint` therefore stays `false`: only a keyed retry is idempotent, and the default
is not.

**Bridge acknowledgement.** The server accepting a message does not prove the bridge forwarded
it. After the send, the server polls the chat's newest entries every 100ms for up to
`MATRIX_BRIDGE_ACK_TIMEOUT` (default 1s), looking for a notice from another account whose
relation points at the sent message. If one appears, the call fails with
`WhatsApp did not deliver the message: <notice>`: the notice is flattened to one line,
control characters are replaced, and the text is capped at 300 characters before an ellipsis. No notice within the window means success. Setting the timeout to `0` disables the
guard and issues no extra request. A failing probe is logged once and leaves the send a
success.

**Encryption.** When `MATRIX_E2EE_ENABLED` is false and the chat is encrypted, the send is
refused with `this chat is encrypted and end-to-end encryption is switched off on the server; set MATRIX_E2EE_ENABLED=true`
instead of posting plaintext.

**Annotations:** `destructiveHint: true`, `idempotentHint: false`, `readOnlyHint: false`.

## send_media

Uploads one attachment and sends it as a single media message.

| Input | Required | Default | Notes |
| --- | --- | --- | --- |
| `chat_id` | one of | — | The chat to send to, or omit it and pass `chat`. Must be on the send allowlist. `room_id` is its deprecated alias. |
| `chat` | one of | — | The chat's name, or a unique case-insensitive substring of one. Not with `chat_id`. Resolving a name never grants send permission. |
| `path` | one of | — | Absolute path of a local file. It must exist, be readable, be a regular file and be at most 50 MiB. Pass exactly one of `path` and `data_base64`. |
| `data_base64` | one of | — | The payload as standard base64, at most 50 MiB once decoded. `mimetype` is required alongside it. |
| `mimetype` | with `data_base64` | — | The payload's MIME type, for example `image/jpeg`. With `path` it overrides the type detected from the file, which is otherwise taken from the extension and then sniffed from the bytes. |
| `caption` | no | — | Sent as the message text, which WhatsApp shows under the media. When empty the filename is used, so the message always has a textual representation. |
| `filename` | no | base name of `path`, else `upload` | Recorded on the message and sent as the upload's filename. |
| `reply_to` | no | — | Message ID to answer, sent as a reply relation. A malformed one is rejected before anything is uploaded. |
| `idempotency_key` | no | — | At most 128 characters. Makes a retry safe. |

**Output:** `{message_id, deduplicated, type, media_uri}`. `message_id` is the sent message's
ID, or the original send's when the call was deduplicated; `media_uri` is absent on a replay
because nothing was uploaded.

**Message type.** The MIME type decides how the bridge renders it in WhatsApp, and what
`type` reports:

| MIME type | `type` |
| --- | --- |
| `image/*` | `image` |
| `video/*` | `video` |
| `audio/*` | `audio` |
| anything else | `document` |

The message records its text (the caption, or the filename), `filename`, and its declared
MIME type and size. An outgoing image or video also carries its pixel dimensions read from
its header, so the preview is laid out correctly.

**The gate.** `send_media` goes through exactly the same gate as `send_message`, which is the
point: a media message is no less a message to the people in the chat.

1. The chat is checked against the effective allowlist — `ALLOWED_SEND_ROOMS` unioned with
   `MATRIX_ALLOWLIST_FILE`, consulted per send. Both empty rejects every upload and every send
   with the read-only error; a chat outside a non-empty list is rejected without consuming
   the rate-limit token. The client layer checks the same store again before uploading.
2. One token bucket grants at most one message every 3 seconds across all chats, and it is
   the same bucket `send_message` draws from: a text message and a media message cannot be
   sent within 3 seconds of each other. A throttled call returns `rate limited, retry in Ns`
   instead of blocking.
3. After the send, the bridge-ack guard watches for the bridge's rejection notice exactly as
   `send_message` does, and fails the call with the notice rather than reporting a message as
   delivered that WhatsApp refused.

Everything that can be rejected without the server is rejected before the gate: a payload
source that is neither `path` nor `data_base64` (or both), a path that is not absolute,
unreadable, a directory or not a regular file, invalid base64, a `data_base64` call without
`mimetype`, a payload over 50 MiB, a malformed `reply_to` and an over-long
`idempotency_key`. Nothing is uploaded and no rate-limit token is spent for any of them.

**Idempotency.** The cache is the same one `send_message` uses, so the key namespace is
shared, but a media entry is identified by a SHA-256 of the payload plus the filename,
mimetype, caption and reply target. A retry with the same key and the same bytes returns the
first message ID with `deduplicated: true` and **uploads nothing**; the same key with
different bytes is the usual conflict error, never a second upload. The 10-minute window, the
in-memory lifetime and the 256-entry cap are the ones described under `send_message`, and
`idempotentHint` is `false` for the same reason: only a keyed retry is idempotent.

**Encryption.** With E2EE on and an encrypted chat, the attachment is encrypted before upload
and the message carries its encrypted descriptor, so the media repository never holds a
readable copy. With E2EE off, an encrypted chat is refused with
`this chat is encrypted and end-to-end encryption is switched off on the server; set MATRIX_E2EE_ENABLED=true`,
exactly as a text send is.

**Logging.** The payload bytes are never logged, and neither is a file's contents; errors name
the failure, the file path and the size, never the bytes.

**Annotations:** `destructiveHint: true`, `idempotentHint: false`, `readOnlyHint: false`.

## Annotations

| Tool | `readOnlyHint` | `destructiveHint` | `idempotentHint` |
| --- | --- | --- | --- |
| `list_chats` | true | — | — |
| `get_messages` | true | — | — |
| `search_messages` | true | — | — |
| `get_message_context` | true | — | — |
| `list_chat_contacts` | true | — | — |
| `get_media` | true | — | — |
| `get_new_messages` | true | — | — |
| `send_message` | false | true | false |
| `send_media` | false | true | false |

`openWorldHint` is left unset on every tool, which means true: each one reaches a real
server and, through it, real people.

## Examples

Page back through a chat, by ID or by its name:

```json
{"name": "get_messages", "arguments": {"chat_id": "!cafe:example.com", "limit": 30}}
```

```json
{"name": "get_messages", "arguments": {"chat": "Cafe", "limit": 30}}
```

Send to a chat by a unique name fragment; the allowlist still applies:

```json
{"name": "send_message", "arguments": {"chat": "family", "text": "8pm works"}}
```

```json
{"name": "get_messages", "arguments": {"chat_id": "!cafe:example.com", "limit": 30, "before": "eyJ2IjoxLCJyIjoiIXI6dGVzdCIsImMiOiJ0MTIzIn0.aBcDeF..."}}
```

The deprecated input names still work, so an older prompt keeps functioning:

```json
{"name": "get_messages", "arguments": {"room_id": "!cafe:example.com", "limit": 30}}
```

Find a phrase and reply to the match:

```json
{"name": "search_messages", "arguments": {"query": "dinner", "limit": 5}}
```

```json
{"name": "send_message", "arguments": {"chat_id": "!family:example.com", "text": "8pm works", "reply_to": "$matchMessageID", "idempotency_key": "reply-dinner-1"}}
```

Inspect a message before answering it:

```json
{"name": "get_message_context", "arguments": {"chat_id": "!family:example.com", "message_id": "$targetMessageID", "limit_before": 3, "limit_after": 0}}
```

Read the photo in a message, downscaled to 1024 pixels on its long edge:

```json
{"name": "get_media", "arguments": {"chat_id": "!family:example.com", "message_id": "$photoMessageID", "max_dimension": 1024}}
```

Send a photo with a caption, answering an earlier message:

```json
{"name": "send_media", "arguments": {"chat_id": "!family:example.com", "path": "/home/me/Pictures/dinner.jpg", "caption": "here it is", "reply_to": "$questionMessageID", "idempotency_key": "photo-dinner-1"}}
```
