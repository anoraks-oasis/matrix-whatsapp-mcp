// Package mcpserver exposes the matrix-whatsapp MCP tools, backed by the
// Matrix service and the trigger dispatcher. The server itself is fail-closed
// about sending: the allow-list and rate limiter live in sendGate so they can
// be unit-tested without a homeserver.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/allowlist"
	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/matrix"
	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/trigger"
	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/whatsapp"
)

const (
	// Name is the MCP server name advertised in the initialize handshake.
	Name = "matrix-whatsapp-mcp"
	// Version is the MCP implementation version constant.
	Version = "0.1.0"

	defaultLimit = 20
	maxLimit     = 100

	// maxSearchQueryLen is the longest accepted search_messages query, in
	// characters. Past this a query is a document, not a search.
	maxSearchQueryLen = 512
	// defaultContextLimit is how many messages get_message_context returns on
	// each side of the target when the caller does not say.
	defaultContextLimit = 10

	// sendInterval is the minimum time between two accepted sends across all
	// chats: the token bucket holds one token and refills it every sendInterval.
	sendInterval = 3 * time.Second

	// listProbeParallel bounds how many chat probes (canonical alias +
	// WhatsApp check) run at once so list_chats does not do chat_count × 2
	// sequential network round-trips.
	listProbeParallel = 8
	// listChatsTimeout bounds the whole probe phase of one list_chats call.
	// Each chat costs up to two probes with a 10s network timeout each, so
	// without an overall bound a large account would pay minutes on a call an
	// agent makes casually. Past the bound the chats are still listed, with
	// the WhatsApp flag false for the ones the probes did not reach.
	listChatsTimeout = 10 * time.Second
	// chatNameParallel bounds how many canonical-alias probes one call's chat
	// name resolution runs at once, so a page of results spanning many chats
	// costs bounded time instead of one round trip per result.
	chatNameParallel = 8
	// chatNameTimeout bounds the display-name resolution a call does for the
	// chats its results mention (get_messages, get_message_context,
	// search_messages and get_new_messages), so a name that cannot be read costs
	// a short wait and then falls back to the chat ID.
	chatNameTimeout = 3 * time.Second
)

// instructions is the handshake text every MCP client reads first. It
// introduces the server as a WhatsApp tool, in the vocabulary the tools use.
const instructions = "Tools for an agent working a WhatsApp account, relayed through the bridge the account runs on. " +
	"Chats and messages use the IDs the bridge assigns, and every chat-scoped tool also accepts a chat name or a unique " +
	"case-insensitive substring of one. Sending is fail-closed: a chat can receive messages only when it is on the server's " +
	"send allowlist (ALLOWED_SEND_ROOMS unioned with the allowlist file, so adding a chat to the allowlist takes effect " +
	"without a restart), and naming a chat never grants that permission. " +
	"At most one message is accepted every 3 seconds, across all chats."

// RoomProber is the best-effort chat detail probe backed by *matrix.Client.
type RoomProber interface {
	// IsBridged reports whether a chat looks like a WhatsApp chat without
	// ever failing the caller on a probe error.
	IsBridged(ctx context.Context, roomID string) bool
	// CanonicalAlias returns the chat's canonical alias, or "".
	CanonicalAlias(ctx context.Context, roomID string) string
}

// TriggerReader reads recent triggers from the dispatcher ring buffer.
type TriggerReader interface {
	Recent(since int64, limit int) []trigger.Trigger
}

// Server holds the dependencies the tool handlers use.
type Server struct {
	matrix matrix.MatrixService
	prober RoomProber
	recent TriggerReader
	gate   *sendGate
	sends  *idempotencyCache
	// inlineBudget is the largest base64 payload get_media puts in a content
	// block. It is a field so a test can shrink it instead of building a
	// megabyte image.
	inlineBudget int
}

// New builds the MCP server with its nine tools wired to the given Matrix
// service, chat prober and trigger reader. allowed is a fixed send allow-list;
// an empty list keeps the server read-only (every send is rejected). Use
// NewWithAllowlist for the live allow-list the running server reads.
func New(svc matrix.MatrixService, prober RoomProber, recent TriggerReader, allowed []string) *mcp.Server {
	return NewWithAllowlist(svc, prober, recent, allowlist.FromSlice(allowed))
}

// NewWithAllowlist is New against a live allow-list source, so a chat added by
// `matrix-whatsapp-mcp chats allow` becomes sendable without a restart.
func NewWithAllowlist(svc matrix.MatrixService, prober RoomProber, recent TriggerReader, allow allowlist.List) *mcp.Server {
	return newServer(svc, prober, recent, allow, newIdempotencyCache())
}

// newServer wires the server around an explicit send cache, so a test can drive
// idempotency expiry with an injected clock instead of waiting ten minutes.
func newServer(svc matrix.MatrixService, prober RoomProber, recent TriggerReader, allow allowlist.List, sends *idempotencyCache) *mcp.Server {
	return newServerWithBudget(svc, prober, recent, allow, sends, defaultInlineMediaBudget)
}

// newServerWithBudget is newServer with get_media's inline budget overridden, so
// a test can exercise the over-budget path without generating a megabyte
// fixture. It is a test seam, like newServer's explicit cache.
func newServerWithBudget(svc matrix.MatrixService, prober RoomProber, recent TriggerReader, allow allowlist.List, sends *idempotencyCache, inlineBudget int) *mcp.Server {
	s := &Server{
		matrix:       svc,
		prober:       prober,
		recent:       recent,
		gate:         newSendGateWith(allow, sendInterval),
		sends:        sends,
		inlineBudget: inlineBudget,
	}

	mcpServer := mcp.NewServer(&mcp.Implementation{Name: Name, Version: Version}, &mcp.ServerOptions{
		Instructions: instructions,
	})

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "list_chats",
		Description: fmt.Sprintf("List every chat the account is in, with its display name and whether it is a WhatsApp chat. "+
			"The result is a chats object holding one row per chat, each with chat_id, chat_name and whatsapp; use a row's chat_name as the chat argument of another tool. "+
			"Chat probes run %d at a time and the whole probe phase is cut off after %s, "+
			"so a large account answers promptly: a chat whose probes did not finish is still listed, with whatsapp false and the name the joined-chat list carried.",
			listProbeParallel, listChatsTimeout),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleListChats)

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "get_messages",
		Description: "Get messages from a chat, oldest first. Name the chat with chat_id from list_chats, or with chat: a chat name or a unique case-insensitive substring of one. " +
			"Without a cursor this returns the newest messages; pass before with the next_token of an earlier get_messages result to page further back, and follow next_token while has_more is true. " +
			"Every message carries its chat_id and chat_name, the display name of the chat it is in or that chat's ID when the name is not known, " +
			"and its relations: the message it replies to, the message it edits, mentioned user IDs, and whether it was deleted.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleGetMessages)

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "search_messages",
		Description: fmt.Sprintf("Search message text case-insensitively and return the matches newest first. "+
			"Scope the search to one chat with chat_id or chat, or omit both to search every joined chat within the scan budget. "+
			"The server's own search index answers when it has one; when it does not, the call degrades to a bounded scan of the most recent %d messages "+
			"in each of at most %d chats (%d messages in total) instead of failing, and truncated reports that the budget cut the search short. "+
			"scanned_chats and scanned_messages say what the scan actually read, and are 0 when the index answered.",
			matrix.SearchEventsPerRoom, matrix.SearchMaxRooms, matrix.SearchMaxRooms*matrix.SearchEventsPerRoom),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleSearchMessages)

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "get_message_context",
		Description: fmt.Sprintf("Get one message with the messages around it: up to limit_before messages before it and up to limit_after after it, %d each by default and capped at %d. "+
			"Name the chat with chat_id or chat. The message's reply_to and edit_of are resolved to the messages they point at, so a reply can be understood without fetching the whole chat. "+
			"The server's context endpoint answers when it has one, and the call falls back to scanning the chat's recent history when it does not; source reports which path answered. "+
			"A message_id that is not in the chat is an error, not an empty result.",
			defaultContextLimit, matrix.ContextMaxLimit),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleMessageContext)

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "list_chat_contacts",
		Description: "List every contact in a chat with the display name the chat reports, whether the contact belongs to WhatsApp, and which kind of contact it is: " +
			"whatsapp_contact for a WhatsApp contact or group, bridge_bot for the WhatsApp bridge bot, and matrix_user for a non-WhatsApp account. " +
			"Name the chat with chat_id or chat. whatsapp uses the same rule as list_chats: a user ID whose localpart starts with a bridge prefix such as whatsapp_ or whatsapp_lid-/whatsapp-lid-, " +
			"any other localpart containing whatsapp, or a bridge marker on the member. The call is bounded by a timeout, and a contact whose profile cannot be read is still listed.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleListChatContacts)

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "get_media",
		Description: fmt.Sprintf("Fetch one message's attachment and return it inline. Name the chat with chat_id or chat, and the message with message_id. "+
			"The result is a text block with the attachment's metadata as JSON "+
			"(mimetype, filename, size, actual dimensions, whether it was downscaled, and media_uri, an opaque reference to the stored bytes) plus, when the payload qualifies, a second content block holding it: "+
			"an image block for an image, an audio block for audio. An image is downscaled so its long edge is at most max_dimension (default %d, clamped to %d..%d) and "+
			"re-encoded until the base64 payload fits about %.1f MiB; audio over that budget, and every other type such as video or a document, returns metadata and text only "+
			"with reason saying why. Nothing is cached and nothing is written to disk, and a download larger than %d MiB is refused rather than truncated. "+
			"Encrypted media is decrypted when MATRIX_E2EE_ENABLED is on and refused with a clear error when it is off.",
			defaultMediaDimension, minMediaDimension, maxMediaDimension, float64(defaultInlineMediaBudget)/(1<<20), matrix.MaxMediaDownloadBytes>>20),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleGetMedia)

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "get_new_messages",
		Description: fmt.Sprintf("Read the incoming messages this server has seen, newest first. It is an in-memory, process-local ring buffer: "+
			"it holds the newest %d messages and is empty again after a restart, so it is a recent-activity view and not a transcript. "+
			"The result is a messages object holding one row per message. since keeps only messages received strictly after that unix-millisecond time, and limit caps the answer at 1..100 (20 by default); "+
			"both apply to what the buffer still holds. Each row carries its message_id, chat_id and chat_name, from, text (a media message with no text "+
			"carries a bracketed marker such as [image]), type and receipt time. since is the canonical input name; since_timestamp is still accepted as a deprecated alias.", trigger.BufferSize),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleGetNewMessages)

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "send_message",
		Description: "Send a plain-text message to a chat, named by chat_id or chat. Fails closed unless the resolved chat is allow-listed: naming a chat never grants permission. " +
			"Rate-limited to one message every 3 seconds. Set reply_to to answer a specific message, so the chat shows it as a quote; set idempotency_key to make a retry safe. " +
			"The server can accept a message that WhatsApp still refuses: when the bridge posts its rejection notice within the acknowledgement window, " +
			"the send fails with that notice instead of reporting success. The usual cause is relay mode (bridge.relay.enabled) not being configured for the sender's account.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: boolPtr(true),
			IdempotentHint:  false,
			ReadOnlyHint:    false,
		},
	}, s.handleSendMessage)

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "send_media",
		Description: fmt.Sprintf("Upload one attachment and send it as a single media message to a chat named by chat_id or chat. Pass exactly one of path (an absolute local file) or data_base64 (with mimetype); "+
			"the type reported follows the mimetype: image/* becomes image, video/* video, audio/* audio, anything else document. caption is sent as the message text, "+
			"which WhatsApp shows under the media, and reply_to makes the message a quote. It goes through the same gate as send_message: the resolved chat must be allow-listed, "+
			"at most one message is accepted every %s, a bridge rejection notice fails the call instead of reporting success, and idempotency_key makes a retry safe "+
			"without uploading or sending twice. The payload is capped at %d MiB, and the bytes are held in memory only.",
			sendInterval, maxMediaUploadBytes>>20),
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: boolPtr(true),
			IdempotentHint:  false,
			ReadOnlyHint:    false,
		},
	}, s.handleSendMedia)

	return mcpServer
}

// chatRef names the chat a call acts on. A chat can be named by ID or by name:
// Chat is what an agent naturally has, ChatID is what a previous result
// carried. room_id stays accepted as a deprecated alias so a caller that
// guesses the old word still works.
type chatRef struct {
	Chat   string `json:"chat,omitempty" jsonschema:"the chat to act on, by name or a unique case-insensitive substring of a name, resolved against the chats the account has joined. Pass either chat or chat_id, never both. A resolved name is still subject to the send allowlist"`
	ChatID string `json:"chat_id,omitempty" jsonschema:"the chat ID: a chat reference such as !abc:example.com, as list_chats reports it. Required unless chat names the chat. The bridge sometimes omits the :server part, which is valid here"`
	RoomID string `json:"room_id,omitempty" jsonschema:"deprecated alias of chat_id; still accepted, and chat_id wins when both are set. Use chat_id in new calls"`
}

// id resolves the chat ID, preferring the canonical name over its alias.
func (c chatRef) chatID() string {
	if c.ChatID != "" {
		return c.ChatID
	}
	return c.RoomID
}

// messageRef names the message a call acts on, with the same alias rule as
// chatRef.
type messageRef struct {
	MessageID string `json:"message_id,omitempty" jsonschema:"the message ID: a message reference such as $abc123. It must start with $ or the call is rejected before anything is requested. Required unless the deprecated alias event_id is given instead"`
	EventID   string `json:"event_id,omitempty" jsonschema:"deprecated alias of message_id; still accepted, and message_id wins when both are set. Use message_id in new calls"`
}

// id resolves the message ID, preferring the canonical name over its alias.
func (m messageRef) messageID() string {
	if m.MessageID != "" {
		return m.MessageID
	}
	return m.EventID
}

type listChatsInput struct{}

type chatResult struct {
	ChatID   string `json:"chat_id" jsonschema:"the chat ID: a chat reference such as !abc:example.com, as list_chats reports it"`
	ChatName string `json:"chat_name" jsonschema:"the display name; falls back to the chat's alternate name, then to 'unnamed'"`
	WhatsApp bool   `json:"whatsapp" jsonschema:"true when this chat is a WhatsApp chat relayed by the bridge"`
}

// listChatsResult wraps the rows in an object: structured content is an object
// everywhere else in this server, and a field such as a count or a page cursor
// can join chats later without changing the shape a model sees.
type listChatsResult struct {
	Chats []chatResult `json:"chats" jsonschema:"every chat the account has joined, sorted by chat name"`
}

type getMessagesInput struct {
	chatRef
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum number of messages to return; default 20, clamped to 1..100"`
	Before string `json:"before,omitempty" jsonschema:"opaque pagination token: the next_token of an earlier get_messages result, which reads the page of messages older than that one. Omit it to start at the newest message. Treat it as opaque - it is not a message ID - and a token that is malformed or was issued by a previous server process is an error rather than an empty page"`
}

type messageResult struct {
	ChatID     string   `json:"chat_id" jsonschema:"the chat the message is in: a chat reference such as !abc:example.com"`
	ChatName   string   `json:"chat_name" jsonschema:"the display name of the chat the message is in, falling back to the chat ID when the name is not known"`
	MessageID  string   `json:"message_id" jsonschema:"the message ID: a message reference such as $abc123"`
	From       string   `json:"from" jsonschema:"the sender's user ID, for example @15551234:example.com"`
	Text       string   `json:"text" jsonschema:"the message text; a media message with no caption carries a bracketed type marker such as [image] or [document]; a deleted message has empty text"`
	Type       string   `json:"type" jsonschema:"the WhatsApp message type: text, image, video, audio, document, sticker, location, or other"`
	Timestamp  int64    `json:"timestamp" jsonschema:"unix milliseconds when the message was sent"`
	Encrypted  bool     `json:"encrypted" jsonschema:"whether the message was encrypted; text that cannot be decrypted is a fixed placeholder"`
	ReplyTo    string   `json:"reply_to,omitempty" jsonschema:"the message ID this message replies to, present even when that message is outside this page. Carried from both the reply relation and its flat form"`
	EditOf     string   `json:"edit_of,omitempty" jsonschema:"the message ID this message replaces. An edit stays in the stream with this field set, so a client can apply it to the original instead of showing a duplicate"`
	Mentions   []string `json:"mentions,omitempty" jsonschema:"user IDs the message mentions; absent when it mentions nobody"`
	Deleted    bool     `json:"deleted" jsonschema:"true when the message was deleted; its text is empty and the deleted content is never returned"`
	Mimetype   string   `json:"mimetype,omitempty" jsonschema:"the MIME type of the message's attachment; absent when the message carries no attachment"`
	Filename   string   `json:"filename,omitempty" jsonschema:"the filename the sender set on the attachment; absent when the sender set none. Many clients put the filename in the text instead"`
	SizeBytes  int64    `json:"size_bytes,omitempty" jsonschema:"the attachment's size in bytes as the sender declared it; absent when the message carries no attachment or declares no size"`
	Width      int      `json:"width,omitempty" jsonschema:"the attachment's width in pixels when the sender reported one, as an image or video; absent otherwise"`
	Height     int      `json:"height,omitempty" jsonschema:"the attachment's height in pixels when the sender reported one, as an image or video; absent otherwise"`
	DurationMS int64    `json:"duration_ms,omitempty" jsonschema:"the attachment's duration in milliseconds when the sender reported one, as audio or video; absent otherwise"`
	MediaURI   string   `json:"media_uri,omitempty" jsonschema:"an opaque reference to the attachment's stored bytes, valid for plaintext and encrypted media alike; pass it back to the server's media API. Absent when the message carries no attachment"`
}

type getMessagesResult struct {
	Messages  []messageResult `json:"messages" jsonschema:"the messages in this page, oldest first"`
	NextToken string          `json:"next_token,omitempty" jsonschema:"opaque token to pass back as before to read the next older page; absent when the chat's history ends here"`
	HasMore   bool            `json:"has_more" jsonschema:"whether the server has more entries behind this page, so a next_token is present. A later page can still contain no messages when the remaining entries are membership or other non-message changes"`
}

type searchMessagesInput struct {
	chatRef
	Query          string `json:"query" jsonschema:"the text to search for, matched against message text case-insensitively; every word of a multi-word query must appear when the bounded scan answers. It must not be empty and is capped at 512 characters"`
	Limit          int    `json:"limit,omitempty" jsonschema:"maximum number of results to return; default 20, clamped to 1..100"`
	Since          int64  `json:"since,omitempty" jsonschema:"only return messages sent at or after this unix-millisecond timestamp; omit it to search every message the search looks at"`
	SinceTimestamp int64  `json:"since_timestamp,omitempty" jsonschema:"deprecated alias of since; still accepted, and since wins when both are set. Use since in new calls"`
}

type searchMessagesResult struct {
	Results         []messageResult `json:"results" jsonschema:"the matching messages, newest first, in the same message shape every tool returns; empty when nothing matched. A search can span chats, so chat_name is how a result says which one it came from"`
	Truncated       bool            `json:"truncated" jsonschema:"true when the search stopped at a budget instead of covering everything it was asked to search: more matches than limit, a chat with history behind the page that was read, more chats than the scan may read, or a chat that could not be read. The results are then a partial answer"`
	ScannedChats    int             `json:"scanned_chats" jsonschema:"how many chats the bounded fallback scan actually read; 0 when the server's search index answered, because nothing was scanned"`
	ScannedMessages int             `json:"scanned_messages" jsonschema:"how many messages the bounded fallback scan actually looked at; 0 when the server's search index answered"`
	Source          string          `json:"source" jsonschema:"which path produced the results: server when the server's search index answered, fallback when the bounded chat scan did"`
}

type getMessageContextInput struct {
	chatRef
	messageRef
	LimitBefore *int `json:"limit_before,omitempty" jsonschema:"how many messages before the target to return; default 10, clamped to 0..50, and 0 returns none"`
	LimitAfter  *int `json:"limit_after,omitempty" jsonschema:"how many messages after the target to return; default 10, clamped to 0..50, and 0 returns none"`
}

type getMessageContextResult struct {
	MessagesBefore []messageResult `json:"messages_before" jsonschema:"the messages before the target, oldest first; only messages, so membership and other non-message entries in the window are not listed"`
	Message        messageResult   `json:"message" jsonschema:"the target message, in the same shape get_messages returns"`
	MessagesAfter  []messageResult `json:"messages_after" jsonschema:"the messages after the target, oldest first; only messages"`
	ReplyTo        *messageResult  `json:"reply_to,omitempty" jsonschema:"the message the target replies to, resolved to its full text when it is in the window or can be fetched; absent when the target is not a reply or the message is no longer available. The target's own reply_to field carries the message ID alone"`
	EditOf         *messageResult  `json:"edit_of,omitempty" jsonschema:"the message the target edits, resolved to its full text when it is in the window or can be fetched; absent when the target is not an edit or the message is no longer available. The target's own edit_of field carries the message ID alone"`
	Source         string          `json:"source" jsonschema:"which path produced the context: context when the server answered the chat's context request, fallback when the neighbourhood was found by scanning the chat's recent messages"`
	Truncated      bool            `json:"truncated" jsonschema:"true when the fallback scan ran out of its fixed budget before covering the requested window, so messages_before or messages_after are shorter than asked for"`
}

type listChatContactsInput struct {
	chatRef
}

// Contact types reported by list_chat_contacts.
const (
	contactTypeBridge = "bridge_bot"
	contactTypeMatrix = "matrix_user"
)

type contactResult struct {
	UserID      string `json:"user_id" jsonschema:"the contact's user ID"`
	DisplayName string `json:"display_name" jsonschema:"the display name the chat reports for the contact, or the contact's profile name when the member details carry none; empty when neither could be read"`
	Membership  string `json:"membership" jsonschema:"the membership the chat reports for this contact, which is join for every entry in this list"`
	WhatsApp    bool   `json:"whatsapp" jsonschema:"true when this contact belongs to WhatsApp: a WhatsApp contact or group, or the WhatsApp bridge bot"`
	ContactType string `json:"contact_type" jsonschema:"the kind of contact: whatsapp_contact for a WhatsApp contact or group, bridge_bot for the WhatsApp bridge bot, and matrix_user for a non-WhatsApp account"`
}

type listChatContactsResult struct {
	Contacts []contactResult `json:"contacts" jsonschema:"the contacts in the chat, sorted by display name and then user ID"`
}

type getNewMessagesInput struct {
	Since          int64 `json:"since,omitempty" jsonschema:"only return messages newer than this unix-millisecond timestamp"`
	SinceTimestamp int64 `json:"since_timestamp,omitempty" jsonschema:"deprecated alias of since; still accepted, and since wins when both are set. Use since in new calls"`
	Limit          int   `json:"limit,omitempty" jsonschema:"maximum number of messages to return; default 20, clamped to 1..100"`
}

type triggerResult struct {
	MessageID string `json:"message_id" jsonschema:"the message ID: a message reference such as $abc123"`
	ChatID    string `json:"chat_id" jsonschema:"the chat ID: a chat reference such as !abc:example.com, as list_chats reports it"`
	ChatName  string `json:"chat_name" jsonschema:"the display name of the chat the message is in, falling back to the chat ID when the name is not known"`
	From      string `json:"from" jsonschema:"the sender's user ID"`
	Text      string `json:"text" jsonschema:"the message text; a media message with no caption carries a bracketed type marker such as [image] or [document]"`
	Type      string `json:"type" jsonschema:"the WhatsApp message type: text, image, video, audio, document, sticker, location, or other"`
	Timestamp int64  `json:"timestamp" jsonschema:"unix milliseconds when the message was received"`
}

// getNewMessagesResult wraps the rows in an object, for the same reason
// listChatsResult does: structured content is an object everywhere else, and a
// count or a cursor can join messages later without reshaping the result.
type getNewMessagesResult struct {
	Messages []triggerResult `json:"messages" jsonschema:"the incoming messages the buffer still holds, newest first; empty when nothing has arrived since the process started"`
}

type sendMessageInput struct {
	chatRef
	Text           string `json:"text" jsonschema:"the plain-text message to send"`
	ReplyTo        string `json:"reply_to,omitempty" jsonschema:"optional message ID this one answers, for example $abc123. It must be a message reference ($ followed by the ID) or the call is rejected before anything is sent. The resolved chat must still be allow-listed; the replied-to message can be in any chat"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"optional key of at most 128 characters that makes a retry safe. A second call with the same key, chat and text within 10 minutes returns the first message ID with deduplicated true and sends nothing; the same key with a different chat or text is an error. Keys are held in memory only, so a restart forgets them"`
}

type sendMessageResult struct {
	MessageID    string `json:"message_id" jsonschema:"the message ID of the sent message, or of the original send when the call was deduplicated"`
	Deduplicated bool   `json:"deduplicated" jsonschema:"true when an earlier send with the same idempotency_key was returned instead of sending again"`
}

func (s *Server) handleListChats(ctx context.Context, _ *mcp.CallToolRequest, _ listChatsInput) (*mcp.CallToolResult, listChatsResult, error) {
	rooms, err := s.matrix.JoinedRooms(ctx)
	if err != nil {
		return nil, listChatsResult{}, err
	}

	results := make([]chatResult, len(rooms))
	sem := make(chan struct{}, listProbeParallel)
	// The probe phase has its own deadline: every chat costs up to two
	// requests, so an unbounded phase would scale with the account. Past it,
	// probeChat's best-effort fallbacks fill the row in.
	probeCtx, cancel := context.WithTimeout(ctx, listChatsTimeout)
	defer cancel()
	var wg sync.WaitGroup
	for i, r := range rooms {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, r matrix.Room) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = s.probeChat(probeCtx, r)
		}(i, r)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, listChatsResult{}, ctx.Err()
	}
	return nil, listChatsResult{Chats: results}, nil
}

// probeChat resolves a chat's display name and WhatsApp flag. Both probes are
// best effort: any error inside them (including cancellation) yields the
// fallback value rather than failing the list_chats call.
func (s *Server) probeChat(ctx context.Context, r matrix.Room) chatResult {
	res := chatResult{ChatID: r.ID, ChatName: r.Name}
	if res.ChatName == "" || res.ChatName == r.ID {
		res.ChatName = "unnamed"
		if s.prober != nil {
			if alias := s.prober.CanonicalAlias(ctx, r.ID); alias != "" {
				res.ChatName = alias
			}
		}
	}
	res.WhatsApp = s.prober != nil && s.prober.IsBridged(ctx, r.ID)
	return res
}

func (s *Server) handleGetMessages(ctx context.Context, _ *mcp.CallToolRequest, in getMessagesInput) (*mcp.CallToolResult, getMessagesResult, error) {
	chatID, err := s.resolveChat(ctx, in.chatRef)
	if err != nil {
		return nil, getMessagesResult{}, err
	}
	page, err := s.matrix.Messages(ctx, chatID, clampLimit(in.Limit), in.Before)
	if err != nil {
		return nil, getMessagesResult{}, err
	}
	chatName := s.chatNames().name(ctx, chatID)
	out := getMessagesResult{
		Messages:  toMessageResults(page.Messages, chatName),
		NextToken: page.NextToken,
		HasMore:   page.HasMore,
	}
	// The server returns newest first; the tool contract is chronological.
	for i, j := 0, len(out.Messages)-1; i < j; i, j = i+1, j-1 {
		out.Messages[i], out.Messages[j] = out.Messages[j], out.Messages[i]
	}
	return nil, out, nil
}

// handleSearchMessages validates the query here rather than in the Matrix
// layer so an empty or oversized query costs no request at all, and clamps the
// limit once at the MCP boundary so the description and the behaviour agree.
func (s *Server) handleSearchMessages(ctx context.Context, _ *mcp.CallToolRequest, in searchMessagesInput) (*mcp.CallToolResult, searchMessagesResult, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return nil, searchMessagesResult{}, matrix.ErrEmptyQuery
	}
	if utf8.RuneCountInString(query) > maxSearchQueryLen {
		return nil, searchMessagesResult{}, fmt.Errorf("query must be at most %d characters", maxSearchQueryLen)
	}
	chatID, err := s.resolveChat(ctx, in.chatRef)
	if err != nil {
		return nil, searchMessagesResult{}, err
	}
	since := in.Since
	if since == 0 {
		since = in.SinceTimestamp
	}
	found, err := s.matrix.SearchMessages(ctx, matrix.SearchRequest{
		Query:          query,
		RoomID:         chatID,
		Limit:          clampLimit(in.Limit),
		SinceTimestamp: since,
	})
	if err != nil {
		return nil, searchMessagesResult{}, err
	}
	names := s.chatNames()
	chatIDs := make([]string, 0, len(found.Hits))
	for _, hit := range found.Hits {
		chatIDs = append(chatIDs, hit.Message.RoomID)
	}
	names.resolve(ctx, chatIDs)
	out := searchMessagesResult{
		Results:         make([]messageResult, 0, len(found.Hits)),
		Truncated:       found.Truncated,
		ScannedChats:    found.ScannedRooms,
		ScannedMessages: found.ScannedEvents,
		Source:          found.Source,
	}
	for _, hit := range found.Hits {
		// A search can span chats, so each hit names the chat it was found in.
		// The resolver named every distinct chat once, ahead of this loop, so it
		// answers from the cache here whatever the matrix layer's own cache held.
		out.Results = append(out.Results, toMessageResult(hit.Message, names.name(ctx, hit.Message.RoomID)))
	}
	return nil, out, nil
}

func (s *Server) handleMessageContext(ctx context.Context, _ *mcp.CallToolRequest, in getMessageContextInput) (*mcp.CallToolResult, getMessageContextResult, error) {
	messageID := in.messageID()
	if !matrix.ValidEventID(messageID) {
		return nil, getMessageContextResult{}, fmt.Errorf("%w: message_id must be a message reference, a $ sigil followed by the ID", matrix.ErrInvalidEventID)
	}
	chatID, err := s.resolveChat(ctx, in.chatRef)
	if err != nil {
		return nil, getMessageContextResult{}, err
	}
	out, err := s.matrix.MessageContext(ctx, chatID, messageID, contextLimit(in.LimitBefore), contextLimit(in.LimitAfter))
	if err != nil {
		return nil, getMessageContextResult{}, err
	}
	chatName := s.chatNames().name(ctx, chatID)
	res := getMessageContextResult{
		MessagesBefore: toMessageResults(out.Before, chatName),
		Message:        toMessageResult(out.Event, chatName),
		MessagesAfter:  toMessageResults(out.After, chatName),
		Source:         out.Source,
		Truncated:      out.Truncated,
	}
	if out.ReplyTo != nil {
		reply := toMessageResult(*out.ReplyTo, chatName)
		res.ReplyTo = &reply
	}
	if out.EditOf != nil {
		edit := toMessageResult(*out.EditOf, chatName)
		res.EditOf = &edit
	}
	return nil, res, nil
}

func (s *Server) handleListChatContacts(ctx context.Context, _ *mcp.CallToolRequest, in listChatContactsInput) (*mcp.CallToolResult, listChatContactsResult, error) {
	chatID, err := s.resolveChat(ctx, in.chatRef)
	if err != nil {
		return nil, listChatContactsResult{}, err
	}
	members, err := s.matrix.Participants(ctx, chatID)
	if err != nil {
		return nil, listChatContactsResult{}, err
	}
	out := listChatContactsResult{Contacts: make([]contactResult, 0, len(members))}
	for _, m := range members {
		out.Contacts = append(out.Contacts, contactResult{
			UserID:      m.UserID,
			DisplayName: m.DisplayName,
			Membership:  m.Membership,
			WhatsApp:    m.Bridged,
			ContactType: contactType(m.BridgeKind),
		})
	}
	return nil, out, nil
}

// contactType maps the bridge classification to the reported contact type. A
// plain Matrix account carries no kind in the matrix layer, so it is named here.
func contactType(kind string) string {
	switch kind {
	case "":
		return contactTypeMatrix
	case matrix.BridgeKindBot:
		return contactTypeBridge
	default:
		return kind
	}
}

func (s *Server) handleGetNewMessages(ctx context.Context, _ *mcp.CallToolRequest, in getNewMessagesInput) (*mcp.CallToolResult, getNewMessagesResult, error) {
	since := in.Since
	if since == 0 {
		since = in.SinceTimestamp
	}
	triggers := s.recent.Recent(since, clampLimit(in.Limit))
	// The buffer records the name its sync loop knew when the message arrived,
	// which is the chat ID until the first /sync lands. Resolve at emit time
	// instead, exactly as get_messages does, so a row never reports a chat by
	// its ID while every other tool reports its name.
	names := s.chatNames()
	chatIDs := make([]string, 0, len(triggers))
	for _, t := range triggers {
		chatIDs = append(chatIDs, t.ChatID)
	}
	names.resolve(ctx, chatIDs)
	out := make([]triggerResult, 0, len(triggers))
	for _, t := range triggers {
		out = append(out, triggerResult{
			MessageID: t.MessageID,
			ChatID:    t.ChatID,
			ChatName:  names.name(ctx, t.ChatID),
			From:      t.From,
			Text:      t.Text,
			Type:      t.Type,
			Timestamp: t.Timestamp,
		})
	}
	return nil, getNewMessagesResult{Messages: out}, nil
}

func (s *Server) handleSendMessage(ctx context.Context, _ *mcp.CallToolRequest, in sendMessageInput) (*mcp.CallToolResult, sendMessageResult, error) {
	if strings.TrimSpace(in.Text) == "" {
		return nil, sendMessageResult{}, errors.New("text must not be empty or whitespace-only")
	}
	// A malformed reply target is rejected here, before the rate-limit token is
	// spent or an idempotency key is claimed: nothing is sent and the caller can
	// retry the same call with a corrected ID.
	if in.ReplyTo != "" && !matrix.ValidEventID(in.ReplyTo) {
		return nil, sendMessageResult{}, fmt.Errorf("%w: reply_to must be a message ID, a $ sigil followed by the ID", matrix.ErrInvalidEventID)
	}
	chatID, err := s.resolveChat(ctx, in.chatRef)
	if err != nil {
		return nil, sendMessageResult{}, err
	}
	if utf8.RuneCountInString(in.IdempotencyKey) > maxIdempotencyKeyLen {
		return nil, sendMessageResult{}, fmt.Errorf("idempotency_key must be at most %d characters", maxIdempotencyKeyLen)
	}

	// The key is claimed before the rate limiter so that a retry of a send that
	// already went through returns the original message ID instead of
	// "rate limited", and so that a concurrent duplicate waits for the first
	// send rather than racing it to the homeserver.
	var claim *sendClaim
	if in.IdempotencyKey != "" {
		c, err := s.sends.claim(ctx, in.IdempotencyKey, chatID, in.Text)
		if err != nil {
			return nil, sendMessageResult{}, err
		}
		if c.replayed {
			return nil, sendMessageResult{MessageID: c.eventID, Deduplicated: true}, nil
		}
		claim = c
		defer s.sends.release(claim)
	}

	if err := s.gate.check(chatID); err != nil {
		return nil, sendMessageResult{}, err
	}
	messageID, err := s.matrix.SendText(ctx, chatID, in.Text, in.ReplyTo)
	if err != nil {
		return nil, sendMessageResult{}, err
	}
	if claim != nil {
		s.sends.settle(claim, messageID)
	}
	return nil, sendMessageResult{MessageID: messageID}, nil
}

func clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultLimit
	case limit > maxLimit:
		return maxLimit
	default:
		return limit
	}
}

// contextLimit applies the get_message_context default and clamp. A nil
// pointer is an omitted field, which means 10; an explicit 0 means none, so the
// two cannot be confused the way a bare int would confuse them.
func contextLimit(v *int) int {
	if v == nil {
		return defaultContextLimit
	}
	switch {
	case *v < 0:
		return 0
	case *v > matrix.ContextMaxLimit:
		return matrix.ContextMaxLimit
	default:
		return *v
	}
}

// toMessageResult is the one place a Matrix message becomes the tool shape, so
// every tool that returns a message returns exactly the same one. The type, the
// placeholder text and the chat's display name are the only values translated;
// every ID is passed through untouched.
func toMessageResult(m matrix.Message, chatName string) messageResult {
	return messageResult{
		ChatID:     m.RoomID,
		ChatName:   chatName,
		MessageID:  m.EventID,
		From:       m.Sender,
		Text:       m.Body,
		Type:       whatsapp.Type(m.MsgType),
		Timestamp:  m.Timestamp,
		Encrypted:  m.Encrypted,
		ReplyTo:    m.ReplyTo,
		EditOf:     m.EditOf,
		Mentions:   m.Mentions,
		Deleted:    m.Redacted,
		Mimetype:   m.MimeType,
		Filename:   m.Filename,
		SizeBytes:  m.SizeBytes,
		Width:      m.Width,
		Height:     m.Height,
		DurationMS: m.DurationMS,
		MediaURI:   m.MXCURI,
	}
}

// toMessageResults converts one page of one chat's messages, which all carry
// the same chat name.
func toMessageResults(msgs []matrix.Message, chatName string) []messageResult {
	out := make([]messageResult, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, toMessageResult(m, chatName))
	}
	return out
}

func boolPtr(b bool) *bool { return &b }

// sendGate enforces the send allow-list and a single-token rate limit across
// all chats. It is the unit of allow-list and rate-limit logic that Phase D
// tests directly, with no homeserver in the loop.
type sendGate struct {
	allow    allowlist.List
	interval time.Duration
	now      func() time.Time

	mu   sync.Mutex
	next time.Time
}

var (
	errReadOnly       = errors.New("sending is disabled: ALLOWED_SEND_ROOMS and the allowlist file are both empty (read-only mode)")
	errRoomNotAllowed = errors.New("sending to this chat is not allowed")
	errRateLimited    = errors.New("rate limited")
)

// newSendGate builds a sendGate over a fixed chat set. The token bucket holds a
// single token and refills it every interval.
func newSendGate(allowed []string, interval time.Duration) *sendGate {
	return newSendGateWith(allowlist.FromSlice(allowed), interval)
}

// newSendGateWith builds a sendGate over a live allowlist. The list is consulted
// per send, and it is cheap to do so: the file behind it is re-read at most once
// per second.
func newSendGateWith(allow allowlist.List, interval time.Duration) *sendGate {
	return &sendGate{allow: allow, interval: interval, now: time.Now}
}

// check validates the chat against the allow-list and acquires the single
// rate-limit token. It fails closed: an empty allow-list rejects every send,
// and a chat missing from a non-empty allow-list is rejected without consuming
// the token. When throttled it returns an error naming the retry delay in
// seconds rather than blocking past the deadline.
func (g *sendGate) check(chatID string) error {
	if len(g.allow.Rooms()) == 0 {
		return errReadOnly
	}
	if !g.allow.Allowed(chatID) {
		return fmt.Errorf("%w: %s", errRoomNotAllowed, chatID)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if now.Before(g.next) {
		wait := g.next.Sub(now)
		secs := int((wait + time.Second - 1) / time.Second)
		if secs < 1 {
			secs = 1
		}
		return fmt.Errorf("%w, retry in %ds", errRateLimited, secs)
	}
	g.next = now.Add(g.interval)
	return nil
}
