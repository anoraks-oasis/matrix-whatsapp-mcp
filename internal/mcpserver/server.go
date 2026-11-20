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

	"github.com/modelcontextprotocol/go-sdk/mcp"

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
}

// New builds the MCP server with its four tools wired to the given Matrix
// service, room prober and trigger reader. allowed is the send allow-list; an
// empty list keeps the server read-only (every send is rejected).
func New(svc matrix.MatrixService, prober RoomProber, recent TriggerReader, allowed []string) *mcp.Server {
	s := &Server{
		matrix: svc,
		prober: prober,
		recent: recent,
		gate:   newSendGate(allowed, sendInterval),
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
		Name:        "read_messages",
		Description: "Read recent messages from a room, oldest first.",
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
}

type messageResult struct {
	EventID   string `json:"event_id" jsonschema:"the Matrix event ID"`
	RoomID    string `json:"room_id" jsonschema:"the Matrix room ID"`
	Sender    string `json:"sender" jsonschema:"the sender Matrix user ID"`
	Body      string `json:"body" jsonschema:"the message body; media events with no text body carry a bracketed type marker such as [image] or [file]"`
	MsgType   string `json:"msgtype" jsonschema:"the message type, for example m.text, m.notice or m.image"`
	Timestamp int64  `json:"timestamp" jsonschema:"unix milliseconds when the message was sent"`
	Encrypted bool   `json:"encrypted" jsonschema:"whether the event was encrypted; an undecryptable body is a fixed placeholder"`
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
	RoomID string `json:"room_id" jsonschema:"the Matrix room ID to send the message to"`
	Text   string `json:"text" jsonschema:"the plain-text message body"`
}

type sendMessageResult struct {
	EventID string `json:"event_id" jsonschema:"the Matrix event ID of the sent message"`
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

func (s *Server) handleReadMessages(ctx context.Context, _ *mcp.CallToolRequest, in readMessagesInput) (*mcp.CallToolResult, []messageResult, error) {
	msgs, err := s.matrix.Messages(ctx, in.RoomID, clampLimit(in.Limit))
	if err != nil {
		return nil, nil, err
	}
	out := make([]messageResult, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, messageResult{
			EventID:   m.EventID,
			RoomID:    m.RoomID,
			Sender:    m.Sender,
			Body:      m.Body,
			MsgType:   m.MsgType,
			Timestamp: m.Timestamp,
			Encrypted: m.Encrypted,
		})
	}
	// The Matrix API returns newest first; the tool contract is chronological.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
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
	if err := s.gate.check(in.RoomID); err != nil {
		return nil, sendMessageResult{}, err
	}
	eventID, err := s.matrix.SendText(ctx, in.RoomID, in.Text)
	if err != nil {
		return nil, sendMessageResult{}, err
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
	allowed  map[string]struct{}
	interval time.Duration
	now      func() time.Time

	mu   sync.Mutex
	next time.Time
}

var (
	errReadOnly       = errors.New("sending is disabled: ALLOWED_SEND_ROOMS is empty (read-only mode)")
	errRoomNotAllowed = errors.New("sending to this room is not allowed")
	errRateLimited    = errors.New("rate limited")
)

// newSendGate builds a sendGate from the allowed room set. The token bucket
// holds a single token and refills it every interval.
func newSendGate(allowed []string, interval time.Duration) *sendGate {
	g := &sendGate{
		allowed:  make(map[string]struct{}, len(allowed)),
		interval: interval,
		now:      time.Now,
	}
	for _, r := range allowed {
		g.allowed[r] = struct{}{}
	}
	return g
}

// check validates the room against the allow-list and acquires the single
// rate-limit token. It fails closed: an empty allow-list rejects every send,
// and a room missing from a non-empty allow-list is rejected without consuming
// the token. When throttled it returns an error naming the retry delay in
// seconds rather than blocking past the deadline.
func (g *sendGate) check(roomID string) error {
	if len(g.allowed) == 0 {
		return errReadOnly
	}
	if _, ok := g.allowed[roomID]; !ok {
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
