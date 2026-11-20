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

// New builds the MCP server with its four tools wired to the given Matrix
// service, room prober and trigger reader. allowed is a fixed send allow-list;
// an empty list keeps the server read-only (every send is rejected). Use
// NewWithAllowlist for the live allow-list the running server reads.
func New(svc matrix.MatrixService, prober RoomProber, recent TriggerReader, allowed []string) *mcp.Server {
	return NewWithAllowlist(svc, prober, recent, allowlist.FromSlice(allowed))
}

// NewWithAllowlist is New against a live allow-list source, so a room added by
// `matrix-whatsapp-mcp rooms allow` becomes sendable without a restart.
func NewWithAllowlist(svc matrix.MatrixService, prober RoomProber, recent TriggerReader, allow allowlist.List) *mcp.Server {
	s := &Server{
		matrix: svc,
		prober: prober,
		recent: recent,
		gate:   newSendGateWith(allow, sendInterval),
		sends:  newIdempotencyCache(),
	}

	mcpServer := mcp.NewServer(&mcp.Implementation{Name: Name, Version: Version}, &mcp.ServerOptions{
		Instructions: "Tools for a Matrix homeserver running a mautrix/whatsapp bridge. " +
			"Sending is fail-closed: only rooms listed in ALLOWED_SEND_ROOMS may receive messages, " +
			"at most one message every 3 seconds.",
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
		Name:        "get_recent_triggers",
		Description: "Read recent incoming-message triggers from the dispatcher ring buffer, newest first.",
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
		Messages:  make([]messageResult, 0, len(page.Messages)),
		NextToken: page.NextToken,
		HasMore:   page.HasMore,
	}
	for _, m := range page.Messages {
		out.Messages = append(out.Messages, messageResult{
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
		})
	}
	// The Matrix API returns newest first; the tool contract is chronological.
	for i, j := 0, len(out.Messages)-1; i < j; i, j = i+1, j-1 {
		out.Messages[i], out.Messages[j] = out.Messages[j], out.Messages[i]
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
