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
	// rooms: the token bucket holds one token and refills it every sendInterval.
	sendInterval = 3 * time.Second

	// listProbeParallel bounds how many room probes (canonical alias + bridged
	// check) run at once so list_rooms does not do room_count × 2 sequential
	// network round-trips.
	listProbeParallel = 8
)

// RoomProber is the best-effort room detail probe backed by *matrix.Client.
type RoomProber interface {
	// IsBridged reports whether a room looks bridged from WhatsApp without
	// ever failing the caller on a probe error.
	IsBridged(ctx context.Context, roomID string) bool
	// CanonicalAlias returns the room's canonical alias, or "".
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
}

// New builds the MCP server with its seven tools wired to the given Matrix
// service, room prober and trigger reader. allowed is a fixed send allow-list;
// an empty list keeps the server read-only (every send is rejected). Use
// NewWithAllowlist for the live allow-list the running server reads.
func New(svc matrix.MatrixService, prober RoomProber, recent TriggerReader, allowed []string) *mcp.Server {
	return NewWithAllowlist(svc, prober, recent, allowlist.FromSlice(allowed))
}

// NewWithAllowlist is New against a live allow-list source, so a room added by
// `matrix-whatsapp-mcp rooms allow` becomes sendable without a restart.
func NewWithAllowlist(svc matrix.MatrixService, prober RoomProber, recent TriggerReader, allow allowlist.List) *mcp.Server {
	return newServer(svc, prober, recent, allow, newIdempotencyCache())
}

// newServer wires the server around an explicit send cache, so a test can drive
// idempotency expiry with an injected clock instead of waiting ten minutes.
func newServer(svc matrix.MatrixService, prober RoomProber, recent TriggerReader, allow allowlist.List, sends *idempotencyCache) *mcp.Server {
	s := &Server{
		matrix: svc,
		prober: prober,
		recent: recent,
		gate:   newSendGateWith(allow, sendInterval),
		sends:  sends,
	}

	mcpServer := mcp.NewServer(&mcp.Implementation{Name: Name, Version: Version}, &mcp.ServerOptions{
		Instructions: "Tools for a Matrix homeserver running a mautrix/whatsapp bridge. " +
			"Sending is fail-closed: a room may receive messages only when it is in ALLOWED_SEND_ROOMS or in the allowlist file " +
			"(the two are unioned, so `matrix-whatsapp-mcp rooms allow` makes a room sendable without a restart). " +
			"At most one message is accepted every 3 seconds, across all rooms.",
	})

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name:        "list_rooms",
		Description: "List every room the account has joined, with the resolved display name and whether it appears bridged from WhatsApp.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleListRooms)

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "read_messages",
		Description: "Read messages from a room, oldest first. Without a cursor this returns the newest messages; pass before with the next_token of an earlier result " +
			"to page further back, and follow next_token while has_more is true. Every message carries its relations: the event it replies to, the event it edits, " +
			"mentioned user IDs, and whether the room redacted it.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleReadMessages)

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "search_messages",
		Description: fmt.Sprintf("Search message bodies case-insensitively and return the matches newest first. "+
			"The homeserver's own search index answers when it implements /search; when it does not, the call degrades to a bounded scan of the most recent %d messages "+
			"in each of at most %d rooms (%d messages in total) instead of failing, and truncated reports that the budget cut the search short. "+
			"scanned_rooms and scanned_events say what the scan actually read, and are 0 when the index answered.",
			matrix.SearchEventsPerRoom, matrix.SearchMaxRooms, matrix.SearchMaxRooms*matrix.SearchEventsPerRoom),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleSearchMessages)

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "get_message_context",
		Description: fmt.Sprintf("Read one message with the messages around it: up to limit_before messages before it and up to limit_after after it, %d each by default and capped at %d. "+
			"The message's reply_to and edit_of are resolved to the messages they point at, so a reply can be understood without fetching the whole room. "+
			"Uses the room's /context endpoint and falls back to scanning the room's recent history when the homeserver does not implement it; source reports which path answered. "+
			"An event ID that is not in the room is an error, not an empty result.",
			defaultContextLimit, matrix.ContextMaxLimit),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleMessageContext)

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "list_participants",
		Description: "List every joined member of a room with the display name the room reports, whether the member belongs to the WhatsApp bridge, and which kind of bridge user it is: " +
			"whatsapp_contact for a WhatsApp ghost, whatsapp_bot for the bridge bot, and empty for a plain Matrix user. " +
			"bridged uses the same heuristic as list_rooms: a user ID whose localpart starts with a mautrix bridge prefix such as whatsapp_ or whatsapp_lid-/whatsapp-lid-, any other localpart containing whatsapp, " +
			"or a bridge marker in the member event. The call is bounded by a timeout, and a member whose profile cannot be read is still listed.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleListParticipants)

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "get_recent_triggers",
		Description: fmt.Sprintf("Read the incoming-message triggers this server has seen, newest first. It is an in-memory, process-local ring buffer: "+
			"it holds the newest %d triggers and is empty again after a restart, so it is a recent-activity view and not a transcript. "+
			"since_timestamp keeps only triggers received strictly after that unix-millisecond time, and limit caps the answer at 1..100 (20 by default); "+
			"both apply to what the buffer still holds. Each trigger carries its event ID, room and room name, sender, body (media with no text body "+
			"carries a bracketed marker such as [image]), msgtype and receipt time.", trigger.BufferSize),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, s.handleRecentTriggers)

	mcp.AddTool(mcpServer, &mcp.Tool{
		Name: "send_message",
		Description: "Send a plain-text message to a room. Fails closed unless the room is allow-listed, and is rate-limited to one message every 3 seconds. " +
			"Set reply_to to answer a specific message, so the room shows it as a quote; set idempotency_key to make a retry safe. " +
			"A message can be accepted by Matrix and still be rejected by the WhatsApp bridge; when the bridge posts its rejection notice within the acknowledgement window, " +
			"the send fails with that notice instead of reporting success. The usual cause is relay mode (bridge.relay.enabled) not being configured for the sender's account.",
		Annotations: &mcp.ToolAnnotations{
			DestructiveHint: boolPtr(true),
			IdempotentHint:  false,
			ReadOnlyHint:    false,
		},
	}, s.handleSendMessage)

	return mcpServer
}

type listRoomsInput struct{}

type roomResult struct {
	RoomID  string `json:"room_id" jsonschema:"the Matrix room ID"`
	Name    string `json:"name" jsonschema:"the display name; falls back to the canonical alias, then to 'unnamed'"`
	Bridged bool   `json:"bridged" jsonschema:"best-effort detection of whether this room is bridged from WhatsApp"`
}

type readMessagesInput struct {
	RoomID string `json:"room_id" jsonschema:"the Matrix room ID to read messages from"`
	Limit  int    `json:"limit,omitempty" jsonschema:"maximum number of messages to return; default 20, clamped to 1..100"`
	Before string `json:"before,omitempty" jsonschema:"opaque pagination token: the next_token of an earlier read_messages result, which reads the page of messages older than that one. Omit it to start at the newest message. Treat it as opaque - it is not an event ID - and a token that is malformed or was issued by a previous server process is an error rather than an empty page"`
}

type messageResult struct {
	EventID   string   `json:"event_id" jsonschema:"the Matrix event ID"`
	RoomID    string   `json:"room_id" jsonschema:"the Matrix room ID"`
	Sender    string   `json:"sender" jsonschema:"the sender Matrix user ID"`
	Body      string   `json:"body" jsonschema:"the message body; media events with no text body carry a bracketed type marker such as [image] or [file]; a redacted message has an empty body"`
	MsgType   string   `json:"msgtype" jsonschema:"the message type, for example m.text, m.notice or m.image"`
	Timestamp int64    `json:"timestamp" jsonschema:"unix milliseconds when the message was sent"`
	Encrypted bool     `json:"encrypted" jsonschema:"whether the event was encrypted; an undecryptable body is a fixed placeholder"`
	ReplyTo   string   `json:"reply_to,omitempty" jsonschema:"the event ID this message replies to, present even when that message is outside this page. Carried from both the m.relates_to.m.in_reply_to and the flat m.relates_to.event_id shape"`
	EditOf    string   `json:"edit_of,omitempty" jsonschema:"the event ID this message replaces (rel_type m.replace). An edit stays in the stream with this field set, so a client can apply it to the original instead of showing a duplicate"`
	Mentions  []string `json:"mentions,omitempty" jsonschema:"user IDs the message mentions (m.mentions.user_ids); absent when it mentions nobody"`
	Redacted  bool     `json:"redacted" jsonschema:"true when the room has redacted (deleted) this message; its body is empty and the deleted text is never returned"`
}

type readMessagesResult struct {
	Messages  []messageResult `json:"messages" jsonschema:"the messages in this page, oldest first"`
	NextToken string          `json:"next_token,omitempty" jsonschema:"opaque token to pass back as before to read the next older page; absent when the room's history ends here"`
	HasMore   bool            `json:"has_more" jsonschema:"whether the homeserver has more events behind this page, so a next_token is present. A later page can still contain no messages when the remaining events are membership or other non-message events"`
}

type searchMessagesInput struct {
	Query          string `json:"query" jsonschema:"the text to search for, matched against message bodies case-insensitively; every word of a multi-word query must appear when the bounded scan answers. It must not be empty and is capped at 512 characters"`
	RoomID         string `json:"room_id,omitempty" jsonschema:"restrict the search to this Matrix room ID; omit it to search every joined room, up to the bounded scan budget this tool's description states"`
	Limit          int    `json:"limit,omitempty" jsonschema:"maximum number of results to return; default 20, clamped to 1..100"`
	SinceTimestamp int64  `json:"since_timestamp,omitempty" jsonschema:"only return messages sent at or after this unix-millisecond timestamp; omit it to search every message the search looks at"`
}

type searchHitResult struct {
	messageResult
	RoomName string `json:"room_name" jsonschema:"the display name of the room the match is in, falling back to the room ID when the name is not known"`
}

type searchMessagesResult struct {
	Results       []searchHitResult `json:"results" jsonschema:"the matching messages, newest first, each in the same shape read_messages returns plus its room name; empty when nothing matched"`
	Truncated     bool              `json:"truncated" jsonschema:"true when the search stopped at a budget instead of covering everything it was asked to search: more matches than limit, a room with history behind the page that was read, more rooms than the scan may read, or a room that could not be read. The results are then a partial answer"`
	ScannedRooms  int               `json:"scanned_rooms" jsonschema:"how many rooms the bounded fallback scan actually read; 0 when the homeserver's search index answered, because nothing was scanned"`
	ScannedEvents int               `json:"scanned_events" jsonschema:"how many messages the bounded fallback scan actually looked at; 0 when the homeserver's search index answered"`
	Source        string            `json:"source" jsonschema:"which path produced the results: server when the homeserver's /search index answered, fallback when the bounded room scan did"`
}

type getMessageContextInput struct {
	RoomID      string `json:"room_id" jsonschema:"the Matrix room ID the event belongs to"`
	EventID     string `json:"event_id" jsonschema:"the event ID of the message to read context for, for example $abc123. It must be a Matrix event ID ($ followed by the ID) or the call is rejected before anything is requested"`
	LimitBefore *int   `json:"limit_before,omitempty" jsonschema:"how many messages before the target to return; default 10, clamped to 0..50, and 0 returns none"`
	LimitAfter  *int   `json:"limit_after,omitempty" jsonschema:"how many messages after the target to return; default 10, clamped to 0..50, and 0 returns none"`
}

type getMessageContextResult struct {
	EventsBefore []messageResult `json:"events_before" jsonschema:"the messages before the target, oldest first; only message events, so membership and other state events in the window are not listed"`
	Event        messageResult   `json:"event" jsonschema:"the target message, in the same shape read_messages returns"`
	EventsAfter  []messageResult `json:"events_after" jsonschema:"the messages after the target, oldest first; only message events"`
	ReplyTo      *messageResult  `json:"reply_to,omitempty" jsonschema:"the message the target replies to, resolved to its full body when it is in the window or can be fetched; absent when the target is not a reply or the message is no longer available. The target's own reply_to field carries the event ID alone"`
	EditOf       *messageResult  `json:"edit_of,omitempty" jsonschema:"the message the target edits, resolved to its full body when it is in the window or can be fetched; absent when the target is not an edit or the message is no longer available. The target's own edit_of field carries the event ID alone"`
	Source       string          `json:"source" jsonschema:"which path produced the context: context when the room's /context endpoint answered, fallback when the neighbourhood was found by scanning the room's recent messages"`
	Truncated    bool            `json:"truncated" jsonschema:"true when the fallback scan ran out of its fixed budget before covering the requested window, so events_before or events_after are shorter than asked for"`
}

type listParticipantsInput struct {
	RoomID string `json:"room_id" jsonschema:"the Matrix room ID to list the joined members of"`
}

type participantResult struct {
	UserID      string `json:"user_id" jsonschema:"the member's Matrix user ID"`
	DisplayName string `json:"display_name" jsonschema:"the display name the room reports for the member, or the member's profile name when the member event carries none; empty when neither could be read"`
	Membership  string `json:"membership" jsonschema:"the membership the room reports for this member, which is join for every entry in this list"`
	Bridged     bool   `json:"bridged" jsonschema:"true when this member belongs to the WhatsApp bridge: a ghost contact, a ghost group, or the bridge bot"`
	BridgeKind  string `json:"bridge_kind" jsonschema:"the kind of bridge user: whatsapp_contact for a WhatsApp ghost, whatsapp_bot for the bridge bot, and empty for a plain Matrix user"`
}

type listParticipantsResult struct {
	Participants []participantResult `json:"participants" jsonschema:"the joined members of the room, sorted by display name and then user ID"`
}

type recentTriggersInput struct {
	SinceTimestamp int64 `json:"since_timestamp,omitempty" jsonschema:"only return triggers newer than this unix-millisecond timestamp"`
	Limit          int   `json:"limit,omitempty" jsonschema:"maximum number of triggers to return; default 20, clamped to 1..100"`
}

type triggerResult struct {
	EventID   string `json:"event_id" jsonschema:"the Matrix event ID"`
	RoomID    string `json:"room_id" jsonschema:"the Matrix room ID"`
	RoomName  string `json:"room_name" jsonschema:"the display name of the room"`
	Sender    string `json:"sender" jsonschema:"the sender Matrix user ID"`
	Body      string `json:"body" jsonschema:"the message body; media events with no text body carry a bracketed type marker such as [image] or [file]"`
	MsgType   string `json:"msgtype" jsonschema:"the message type, for example m.text, m.notice or m.image"`
	Timestamp int64  `json:"timestamp" jsonschema:"unix milliseconds when the message was received"`
}

type sendMessageInput struct {
	RoomID         string `json:"room_id" jsonschema:"the Matrix room ID to send the message to"`
	Text           string `json:"text" jsonschema:"the plain-text message body"`
	ReplyTo        string `json:"reply_to,omitempty" jsonschema:"optional event ID of the message this one answers, for example $abc123. It must be a Matrix event ID ($ followed by the ID) or the call is rejected before anything is sent. The room must still be allow-listed; the replied-to message can be in any room"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"optional key of at most 128 characters that makes a retry safe. A second call with the same key, room and text within 10 minutes returns the first event ID with deduplicated true and sends nothing; the same key with a different room or text is an error. Keys are held in memory only, so a restart forgets them"`
}

type sendMessageResult struct {
	EventID      string `json:"event_id" jsonschema:"the Matrix event ID of the sent message, or of the original send when the call was deduplicated"`
	Deduplicated bool   `json:"deduplicated" jsonschema:"true when an earlier send with the same idempotency_key was returned instead of sending again"`
}

func (s *Server) handleListRooms(ctx context.Context, _ *mcp.CallToolRequest, _ listRoomsInput) (*mcp.CallToolResult, []roomResult, error) {
	rooms, err := s.matrix.JoinedRooms(ctx)
	if err != nil {
		return nil, nil, err
	}

	results := make([]roomResult, len(rooms))
	sem := make(chan struct{}, listProbeParallel)
	var wg sync.WaitGroup
	for i, r := range rooms {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, r matrix.Room) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = s.probeRoom(ctx, r)
		}(i, r)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	return nil, results, nil
}

// probeRoom resolves a room's display name and bridged flag. Both probes are
// best effort: any error inside them (including cancellation) yields the
// fallback value rather than failing the list_rooms call.
func (s *Server) probeRoom(ctx context.Context, r matrix.Room) roomResult {
	res := roomResult{RoomID: r.ID, Name: r.Name}
	if res.Name == "" || res.Name == r.ID {
		res.Name = "unnamed"
		if s.prober != nil {
			if alias := s.prober.CanonicalAlias(ctx, r.ID); alias != "" {
				res.Name = alias
			}
		}
	}
	res.Bridged = s.prober != nil && s.prober.IsBridged(ctx, r.ID)
	return res
}

func (s *Server) handleReadMessages(ctx context.Context, _ *mcp.CallToolRequest, in readMessagesInput) (*mcp.CallToolResult, readMessagesResult, error) {
	page, err := s.matrix.Messages(ctx, in.RoomID, clampLimit(in.Limit), in.Before)
	if err != nil {
		return nil, readMessagesResult{}, err
	}
	out := readMessagesResult{
		Messages:  toMessageResults(page.Messages),
		NextToken: page.NextToken,
		HasMore:   page.HasMore,
	}
	// The Matrix API returns newest first; the tool contract is chronological.
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
		return nil, searchMessagesResult{}, fmt.Errorf("%w: query must not be empty", matrix.ErrEmptyQuery)
	}
	if utf8.RuneCountInString(query) > maxSearchQueryLen {
		return nil, searchMessagesResult{}, fmt.Errorf("query must be at most %d characters", maxSearchQueryLen)
	}
	found, err := s.matrix.SearchMessages(ctx, matrix.SearchRequest{
		Query:          query,
		RoomID:         in.RoomID,
		Limit:          clampLimit(in.Limit),
		SinceTimestamp: in.SinceTimestamp,
	})
	if err != nil {
		return nil, searchMessagesResult{}, err
	}
	out := searchMessagesResult{
		Results:       make([]searchHitResult, 0, len(found.Hits)),
		Truncated:     found.Truncated,
		ScannedRooms:  found.ScannedRooms,
		ScannedEvents: found.ScannedEvents,
		Source:        found.Source,
	}
	for _, hit := range found.Hits {
		out.Results = append(out.Results, searchHitResult{
			messageResult: toMessageResult(hit.Message),
			RoomName:      hit.RoomName,
		})
	}
	return nil, out, nil
}

func (s *Server) handleMessageContext(ctx context.Context, _ *mcp.CallToolRequest, in getMessageContextInput) (*mcp.CallToolResult, getMessageContextResult, error) {
	if !matrix.ValidEventID(in.EventID) {
		return nil, getMessageContextResult{}, fmt.Errorf("%w: event_id must be a Matrix event ID, a $ sigil followed by the ID", matrix.ErrInvalidEventID)
	}
	out, err := s.matrix.MessageContext(ctx, in.RoomID, in.EventID, contextLimit(in.LimitBefore), contextLimit(in.LimitAfter))
	if err != nil {
		return nil, getMessageContextResult{}, err
	}
	res := getMessageContextResult{
		EventsBefore: toMessageResults(out.Before),
		Event:        toMessageResult(out.Event),
		EventsAfter:  toMessageResults(out.After),
		Source:       out.Source,
		Truncated:    out.Truncated,
	}
	if out.ReplyTo != nil {
		reply := toMessageResult(*out.ReplyTo)
		res.ReplyTo = &reply
	}
	if out.EditOf != nil {
		edit := toMessageResult(*out.EditOf)
		res.EditOf = &edit
	}
	return nil, res, nil
}

func (s *Server) handleListParticipants(ctx context.Context, _ *mcp.CallToolRequest, in listParticipantsInput) (*mcp.CallToolResult, listParticipantsResult, error) {
	members, err := s.matrix.Participants(ctx, in.RoomID)
	if err != nil {
		return nil, listParticipantsResult{}, err
	}
	out := listParticipantsResult{Participants: make([]participantResult, 0, len(members))}
	for _, m := range members {
		out.Participants = append(out.Participants, participantResult{
			UserID:      m.UserID,
			DisplayName: m.DisplayName,
			Membership:  m.Membership,
			Bridged:     m.Bridged,
			BridgeKind:  m.BridgeKind,
		})
	}
	return nil, out, nil
}

func (s *Server) handleRecentTriggers(_ context.Context, _ *mcp.CallToolRequest, in recentTriggersInput) (*mcp.CallToolResult, []triggerResult, error) {
	triggers := s.recent.Recent(in.SinceTimestamp, clampLimit(in.Limit))
	out := make([]triggerResult, 0, len(triggers))
	for _, t := range triggers {
		out = append(out, triggerResult{
			EventID:   t.EventID,
			RoomID:    t.RoomID,
			RoomName:  t.RoomName,
			Sender:    t.Sender,
			Body:      t.Body,
			MsgType:   t.MsgType,
			Timestamp: t.Timestamp,
		})
	}
	return nil, out, nil
}

func (s *Server) handleSendMessage(ctx context.Context, _ *mcp.CallToolRequest, in sendMessageInput) (*mcp.CallToolResult, sendMessageResult, error) {
	if strings.TrimSpace(in.Text) == "" {
		return nil, sendMessageResult{}, errors.New("text must not be empty or whitespace-only")
	}
	// A malformed reply target is rejected here, before the rate-limit token is
	// spent or an idempotency key is claimed: nothing is sent and the caller can
	// retry the same call with a corrected ID.
	if in.ReplyTo != "" && !matrix.ValidEventID(in.ReplyTo) {
		return nil, sendMessageResult{}, fmt.Errorf("%w: reply_to must be a Matrix event ID, a $ sigil followed by the ID", matrix.ErrInvalidEventID)
	}
	if utf8.RuneCountInString(in.IdempotencyKey) > maxIdempotencyKeyLen {
		return nil, sendMessageResult{}, fmt.Errorf("idempotency_key must be at most %d characters", maxIdempotencyKeyLen)
	}

	// The key is claimed before the rate limiter so that a retry of a send that
	// already went through returns the original event ID instead of
	// "rate limited", and so that a concurrent duplicate waits for the first
	// send rather than racing it to the homeserver.
	var claim *sendClaim
	if in.IdempotencyKey != "" {
		c, err := s.sends.claim(ctx, in.IdempotencyKey, in.RoomID, in.Text)
		if err != nil {
			return nil, sendMessageResult{}, err
		}
		if c.replayed {
			return nil, sendMessageResult{EventID: c.eventID, Deduplicated: true}, nil
		}
		claim = c
		defer s.sends.release(claim)
	}

	if err := s.gate.check(in.RoomID); err != nil {
		return nil, sendMessageResult{}, err
	}
	eventID, err := s.matrix.SendText(ctx, in.RoomID, in.Text, in.ReplyTo)
	if err != nil {
		return nil, sendMessageResult{}, err
	}
	if claim != nil {
		s.sends.settle(claim, eventID)
	}
	return nil, sendMessageResult{EventID: eventID}, nil
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
// every tool that returns a message returns exactly the same one.
func toMessageResult(m matrix.Message) messageResult {
	return messageResult{
		EventID:   m.EventID,
		RoomID:    m.RoomID,
		Sender:    m.Sender,
		Body:      m.Body,
		MsgType:   m.MsgType,
		Timestamp: m.Timestamp,
		Encrypted: m.Encrypted,
		ReplyTo:   m.ReplyTo,
		EditOf:    m.EditOf,
		Mentions:  m.Mentions,
		Redacted:  m.Redacted,
	}
}

func toMessageResults(msgs []matrix.Message) []messageResult {
	out := make([]messageResult, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, toMessageResult(m))
	}
	return out
}

func boolPtr(b bool) *bool { return &b }

// sendGate enforces the send allow-list and a single-token rate limit across
// all rooms. It is the unit of allow-list and rate-limit logic that Phase D
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
	errRoomNotAllowed = errors.New("sending to this room is not allowed")
	errRateLimited    = errors.New("rate limited")
)

// newSendGate builds a sendGate over a fixed room set. The token bucket holds a
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

// check validates the room against the allow-list and acquires the single
// rate-limit token. It fails closed: an empty allow-list rejects every send,
// and a room missing from a non-empty allow-list is rejected without consuming
// the token. When throttled it returns an error naming the retry delay in
// seconds rather than blocking past the deadline.
func (g *sendGate) check(roomID string) error {
	if len(g.allow.Rooms()) == 0 {
		return errReadOnly
	}
	if !g.allow.Allowed(roomID) {
		return fmt.Errorf("%w: %s", errRoomNotAllowed, roomID)
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
