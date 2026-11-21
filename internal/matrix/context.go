package matrix

import (
	"context"
	"errors"
	"sort"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/config"
)

// How a context response was produced.
const (
	// ContextSourceEndpoint means the room's /context endpoint answered.
	ContextSourceEndpoint = "context"
	// ContextSourceScan means the homeserver does not implement /context and
	// the event's neighbourhood was found by walking /messages instead.
	ContextSourceScan = "fallback"
)

const (
	// ContextMaxLimit caps LimitBefore and LimitAfter.
	ContextMaxLimit = 50
	// ContextScanPages and ContextScanPageLimit bound the fallback: at most
	// ContextScanPages × ContextScanPageLimit raw events are read.
	ContextScanPages     = 5
	ContextScanPageLimit = 100
)

// ContextOutcome is one event with the messages around it. Before and After are
// chronological (oldest first), and contain only message events: membership and
// other non-message events in the window are dropped, as they are everywhere
// else in this package. ReplyTo and EditOf are the messages the target points
// at, resolved when they can be found or fetched, and nil when they cannot.
//
// Truncated is true only on the fallback path, when the scan ran out of budget
// before the requested window was covered.
type ContextOutcome struct {
	Before    []Message
	Event     Message
	After     []Message
	ReplyTo   *Message
	EditOf    *Message
	Source    string
	Truncated bool
}

// MessageContext implements MatrixService. It reads one event with up to
// `before` messages before it and `after` messages after it, and resolves the
// event it replies to and the event it edits, so a reply can be understood
// without reading the whole room. Both limits are clamped to 0..ContextMaxLimit.
//
// The room's /context endpoint is used when the homeserver implements it. When
// it does not (M_UNRECOGNIZED, or a route that does not exist), the event is
// fetched directly and its neighbourhood is found by walking the room's recent
// history within a fixed budget; Source reports which path answered and
// Truncated reports whether the fallback budget ran out.
//
// An event ID that is well formed but not in the room is ErrEventNotFound, and
// an event that exists but is not a message event is ErrNotAMessage: neither is
// reported as an empty result.
func (c *Client) MessageContext(ctx context.Context, roomID, eventID string, before, after int) (ContextOutcome, error) {
	if !config.ValidRoomID(roomID) {
		return ContextOutcome{}, ErrInvalidRoomID
	}
	if !ValidEventID(eventID) {
		return ContextOutcome{}, ErrInvalidEventID
	}
	before, after = clampContext(before), clampContext(after)

	if !c.contextUnavailable.Load() {
		out, err := c.endpointContext(ctx, roomID, eventID, before, after)
		if err == nil {
			return out, nil
		}
		if ctx.Err() != nil {
			return ContextOutcome{}, ctx.Err()
		}
		if !isUnsupportedEndpoint(err) {
			return ContextOutcome{}, err
		}
		c.contextUnavailable.Store(true)
		c.log.Printf("homeserver does not implement /context; scanning room history instead")
	}
	return c.scanContext(ctx, roomID, eventID, before, after)
}

// endpointContext reads the event and its neighbourhood from /context.
func (c *Client) endpointContext(ctx context.Context, roomID, eventID string, before, after int) (ContextOutcome, error) {
	limit := before + after
	if limit < 1 {
		limit = 1
	}
	resp, err := c.cli.Context(ctx, id.RoomID(roomID), id.EventID(eventID), nil, limit)
	if err != nil {
		if errors.Is(err, mautrix.MNotFound) {
			return ContextOutcome{}, c.failedAs(ErrEventNotFound, err)
		}
		return ContextOutcome{}, c.failedText("could not read the message context", err)
	}
	if resp == nil || resp.Event == nil {
		return ContextOutcome{}, ErrEventNotFound
	}
	target, ok := c.contextMessage(ctx, roomID, resp.Event)
	if !ok {
		return ContextOutcome{}, ErrNotAMessage
	}

	out := ContextOutcome{
		Before: chronological(c.convertEvents(ctx, roomID, resp.EventsBefore)),
		Event:  target,
		After:  chronological(c.convertEvents(ctx, roomID, resp.EventsAfter)),
		Source: ContextSourceEndpoint,
	}
	// The endpoint splits its limit between the two sides; honour the caller's
	// own split rather than whatever the homeserver chose.
	if len(out.Before) > before {
		out.Before = out.Before[len(out.Before)-before:]
	}
	if len(out.After) > after {
		out.After = out.After[:after]
	}
	out.ReplyTo = c.resolveRelation(ctx, roomID, out, target.ReplyTo)
	out.EditOf = c.resolveRelation(ctx, roomID, out, target.EditOf)
	return out, nil
}

// scanContext is the fallback for a homeserver without /context: the event is
// fetched directly, then the room's history is walked backward from the newest
// message until the event and the requested window around it are covered.
func (c *Client) scanContext(ctx context.Context, roomID, eventID string, before, after int) (ContextOutcome, error) {
	evt, err := c.cli.GetEvent(ctx, id.RoomID(roomID), id.EventID(eventID))
	if err != nil {
		if errors.Is(err, mautrix.MNotFound) {
			return ContextOutcome{}, c.failedAs(ErrEventNotFound, err)
		}
		return ContextOutcome{}, c.failedText("could not read the message", err)
	}
	target, ok := c.contextMessage(ctx, roomID, evt)
	if !ok {
		return ContextOutcome{}, ErrNotAMessage
	}

	window, err := c.scanAround(ctx, roomID, target, before, after)
	if err != nil {
		return ContextOutcome{}, err
	}
	out := ContextOutcome{Event: target, Source: ContextSourceScan, Truncated: window.truncated}
	if window.found {
		newer := window.messages[:window.index]
		if len(newer) > after {
			newer = newer[:after]
		}
		out.After = reversed(newer)
		older := window.messages[window.index+1:]
		if len(older) > before {
			older = older[:before]
		}
		out.Before = reversed(older)
	} else {
		// The event exists, but the scan's budget ended before reaching it, so
		// there is no neighbourhood to report.
		out.Truncated = true
	}
	out.ReplyTo = c.resolveRelation(ctx, roomID, out, target.ReplyTo)
	out.EditOf = c.resolveRelation(ctx, roomID, out, target.EditOf)
	return out, nil
}

// scanWindow is the fallback scan's raw material: the messages it read newest
// first, the index of the target among them, and whether it stopped early.
type scanWindow struct {
	messages  []Message
	found     bool
	index     int
	truncated bool
}

// scanAround walks /messages backward until the target and the requested window
// around it are covered, the timeline ends, or the fixed raw-event budget is
// spent. It reads at most ContextScanPages pages of ContextScanPageLimit events.
func (c *Client) scanAround(ctx context.Context, roomID string, target Message, before, after int) (scanWindow, error) {
	window := scanWindow{index: -1}
	budget := ContextScanPages * ContextScanPageLimit
	raw := 0
	from := ""
	satisfied := false
	reachedEnd := false
	for page := 0; page < ContextScanPages && raw < budget; page++ {
		want := min(ContextScanPageLimit, budget-raw)
		resp, err := c.cli.Messages(ctx, id.RoomID(roomID), from, "", mautrix.DirectionBackward, nil, want)
		if err != nil {
			return scanWindow{}, c.failedText("could not read chat history", err)
		}
		raw += len(resp.Chunk)
		for _, evt := range resp.Chunk {
			if evt == nil {
				continue
			}
			if evt.ID.String() == target.EventID && !window.found {
				window.found = true
				window.index = len(window.messages)
				// Keep the caller's copy: it was fetched and converted on its
				// own, so it is authoritative even if this page cannot parse it.
				window.messages = append(window.messages, target)
				continue
			}
			evt.RoomID = id.RoomID(roomID)
			if msg, ok := c.convert(ctx, evt); ok {
				window.messages = append(window.messages, msg)
			}
		}
		if window.found && window.index >= after && len(window.messages)-window.index-1 >= before {
			satisfied = true
			break
		}
		if len(resp.Chunk) == 0 || resp.End == "" {
			reachedEnd = true
			break
		}
		if !window.found && resp.Chunk[len(resp.Chunk)-1].Timestamp < target.Timestamp {
			// The page is older than the target and did not contain it, so the
			// event is outside what this account can see in the timeline.
			reachedEnd = true
			break
		}
		from = resp.End
	}
	window.truncated = !satisfied && !reachedEnd
	return window, nil
}

// resolveRelation finds the message a relation points at, preferring what the
// context already read and fetching it only when it is outside the window. A
// failed lookup leaves the field nil: context is still useful without it, so it
// never fails the call.
func (c *Client) resolveRelation(ctx context.Context, roomID string, out ContextOutcome, eventID string) *Message {
	if eventID == "" {
		return nil
	}
	for _, m := range out.Before {
		if m.EventID == eventID {
			return &m
		}
	}
	for _, m := range out.After {
		if m.EventID == eventID {
			return &m
		}
	}
	evt, err := c.cli.GetEvent(ctx, id.RoomID(roomID), id.EventID(eventID))
	if err != nil {
		c.log.Printf("context: resolving %s failed: %v", eventID, err)
		return nil
	}
	msg, ok := c.contextMessage(ctx, roomID, evt)
	if !ok {
		return nil
	}
	return &msg
}

// contextMessage converts one event of a context response, which carries no
// room ID of its own.
func (c *Client) contextMessage(ctx context.Context, roomID string, evt *event.Event) (Message, bool) {
	if evt == nil {
		return Message{}, false
	}
	evt.RoomID = id.RoomID(roomID)
	return c.convert(ctx, evt)
}

// convertEvents converts a context window, dropping the events that are not
// messages.
func (c *Client) convertEvents(ctx context.Context, roomID string, evts []*event.Event) []Message {
	out := make([]Message, 0, len(evts))
	for _, evt := range evts {
		if evt == nil {
			continue
		}
		if msg, ok := c.contextMessage(ctx, roomID, evt); ok {
			out = append(out, msg)
		}
	}
	return out
}

// chronological sorts a converted window oldest first. The homeserver hands out
// events_before in reverse-chronological order and events_after in
// chronological order; sorting by timestamp makes the output contract the same
// on both sides without trusting either.
func chronological(msgs []Message) []Message {
	sort.SliceStable(msgs, func(i, j int) bool { return msgs[i].Timestamp < msgs[j].Timestamp })
	return msgs
}

// reversed returns msgs oldest last, i.e. chronological from a newest-first
// slice, in a new slice so the caller's is untouched.
func reversed(msgs []Message) []Message {
	out := make([]Message, len(msgs))
	for i, m := range msgs {
		out[len(msgs)-1-i] = m
	}
	return out
}

// clampContext keeps a limit_before or limit_after inside 0..ContextMaxLimit.
// The default of 10 when the caller omits it belongs to the MCP layer, because
// only there is "absent" distinguishable from an explicit 0.
func clampContext(n int) int {
	if n < 0 {
		return 0
	}
	if n > ContextMaxLimit {
		return ContextMaxLimit
	}
	return n
}
