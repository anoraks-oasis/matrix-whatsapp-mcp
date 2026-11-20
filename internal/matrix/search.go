package matrix

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/config"
)

// Where a search's results came from.
const (
	// SearchSourceIndex means the homeserver's own /search index answered: it
	// covers the room's whole history, and nothing was scanned.
	SearchSourceIndex = "server"
	// SearchSourceScan means the homeserver does not implement /search and the
	// bounded fallback read recent messages from a fixed number of rooms.
	SearchSourceScan = "fallback"
)

const (
	// SearchMaxRooms is how many rooms the fallback scan reads when the caller
	// did not name one. Rooms are taken in the name order JoinedRooms returns.
	SearchMaxRooms = 10
	// SearchEventsPerRoom is how many messages the fallback scan reads from
	// each room, and so also the total it can read: SearchMaxRooms × this.
	SearchEventsPerRoom = 50
)

// SearchRequest is one search_messages query. An empty RoomID searches every
// joined room; a SinceTimestamp above zero keeps only messages sent at or after
// that unix-millisecond time. Limit is clamped to 1..100, defaulting to 20.
type SearchRequest struct {
	Query          string
	RoomID         string
	Limit          int
	SinceTimestamp int64
}

// SearchHit is one matching message plus the display name of its room.
type SearchHit struct {
	Message
	RoomName string `json:"room_name"`
}

// SearchOutcome is a result set and the cost of producing it. Truncated is true
// when the search stopped at a budget instead of covering what it was asked to
// search: more matches than Limit, a room with history behind the page that was
// read, more rooms than SearchMaxRooms, or a room that could not be read.
// ScannedRooms and ScannedEvents are zero when the homeserver's own index
// answered, because nothing was scanned.
type SearchOutcome struct {
	Hits          []SearchHit
	Truncated     bool
	ScannedRooms  int
	ScannedEvents int
	Source        string
}

// SearchMessages implements MatrixService. It asks the homeserver's search
// index first, because that index covers a room's whole history, and falls back
// to a bounded scan of recent messages when the server does not implement
// /search. The scan is bounded by SearchMaxRooms and SearchEventsPerRoom, so a
// large account cannot turn one call into an unbounded fan-out; the caller
// learns what was actually read from ScannedRooms and ScannedEvents.
//
// Results are newest first. The index decides what matches when it answers; the
// scan matches body text case-insensitively, requiring every whitespace-
// separated term of the query to appear. A homeserver that refuses /search
// degrades to the scan rather than failing the call, and the refusal is
// remembered for the life of the process so later searches skip the doomed
// request.
func (c *Client) SearchMessages(ctx context.Context, req SearchRequest) (SearchOutcome, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return SearchOutcome{}, ErrEmptyQuery
	}
	if req.RoomID != "" && !config.ValidRoomID(req.RoomID) {
		return SearchOutcome{}, ErrInvalidRoomID
	}
	req.Query = query
	switch {
	case req.Limit <= 0:
		req.Limit = defaultMsgLimit
	case req.Limit > maxMsgLimit:
		req.Limit = maxMsgLimit
	}

	if !c.searchUnavailable.Load() {
		out, err := c.indexSearch(ctx, req)
		if err == nil {
			return out, nil
		}
		if ctx.Err() != nil {
			return SearchOutcome{}, ctx.Err()
		}
		if !isSearchUnsupported(err) {
			return SearchOutcome{}, fmt.Errorf("matrix: search: %w", err)
		}
		c.searchUnavailable.Store(true)
		c.log.Printf("homeserver does not implement /search; falling back to a bounded room scan")
	}
	return c.scanSearch(ctx, req)
}

// indexSearch runs the query through the homeserver's search index.
func (c *Client) indexSearch(ctx context.Context, req SearchRequest) (SearchOutcome, error) {
	filter := &mautrix.FilterPart{Types: []event.Type{event.EventMessage}}
	if req.RoomID != "" {
		filter.Rooms = []id.RoomID{id.RoomID(req.RoomID)}
	}
	resp, err := c.cli.Search(ctx, &mautrix.ReqSearch{
		SearchTerm: req.Query,
		Keys:       []string{"content.body"},
		OrderBy:    "recent",
		Filter:     filter,
	})
	if err != nil {
		return SearchOutcome{}, err
	}
	if resp == nil {
		// A homeserver may answer with no search_categories.room_events object;
		// that is an empty result, not an error.
		resp = &mautrix.RespSearch{}
	}

	hits := make([]SearchHit, 0, len(resp.Results))
	seen := make(map[string]bool, len(resp.Results))
	for _, result := range resp.Results {
		if result == nil || result.Event == nil {
			continue
		}
		evt := result.Event
		roomID := evt.RoomID.String()
		if roomID == "" {
			roomID = req.RoomID
			if roomID == "" {
				continue
			}
			evt.RoomID = id.RoomID(roomID)
		}
		if req.SinceTimestamp > 0 && evt.Timestamp < req.SinceTimestamp {
			continue
		}
		if seen[evt.ID.String()] {
			continue
		}
		msg, ok := c.convert(ctx, evt)
		if !ok {
			continue
		}
		seen[msg.EventID] = true
		hits = append(hits, SearchHit{Message: msg, RoomName: c.RoomName(roomID)})
	}

	total := len(hits)
	// The server's own count covers every page it knows about, so a count
	// beyond what it returned means there are matches this call did not see.
	truncated := total > req.Limit || resp.NextBatch != "" || resp.Count > total
	hits, truncated = rankNewestFirst(hits, req.Limit, truncated)
	return SearchOutcome{
		Hits:      hits,
		Truncated: truncated,
		Source:    SearchSourceIndex,
	}, nil
}

// scanSearch is the fallback: it reads the newest SearchEventsPerRoom messages
// of at most SearchMaxRooms rooms and matches them in this process.
func (c *Client) scanSearch(ctx context.Context, req SearchRequest) (SearchOutcome, error) {
	rooms, err := c.searchRooms(ctx, req.RoomID)
	if err != nil {
		return SearchOutcome{}, err
	}

	out := SearchOutcome{Source: SearchSourceScan}
	terms := strings.Fields(strings.ToLower(req.Query))
	hits := make([]SearchHit, 0, req.Limit)
	truncated := false
	for i, room := range rooms {
		if i >= SearchMaxRooms {
			// More rooms were eligible than the budget allows, so this search
			// did not look at everything.
			truncated = true
			break
		}
		if err := ctx.Err(); err != nil {
			return SearchOutcome{}, err
		}
		page, err := c.Messages(ctx, room.ID, SearchEventsPerRoom, "")
		if err != nil {
			if ctx.Err() != nil {
				return SearchOutcome{}, ctx.Err()
			}
			if req.RoomID != "" {
				// The caller named this room: an unreadable room is an error,
				// not an empty search.
				return SearchOutcome{}, err
			}
			// One unreadable room must not fail the whole search, but its
			// messages were not searched and the caller is told so.
			c.log.Printf("search: skipping %s: %v", room.ID, err)
			truncated = true
			continue
		}
		out.ScannedRooms++
		msgs := page.Messages
		if len(msgs) > SearchEventsPerRoom {
			msgs = msgs[:SearchEventsPerRoom]
		}
		if page.HasMore || len(page.Messages) > SearchEventsPerRoom {
			// This room has history behind the page that was read.
			truncated = true
		}
		for _, m := range msgs {
			out.ScannedEvents++
			if req.SinceTimestamp > 0 && m.Timestamp < req.SinceTimestamp {
				continue
			}
			if m.Redacted || !matchesTerms(m.Body, terms) {
				continue
			}
			hits = append(hits, SearchHit{Message: m, RoomName: room.Name})
		}
	}

	out.Hits, out.Truncated = rankNewestFirst(hits, req.Limit, truncated)
	return out, nil
}

// searchRooms resolves the rooms a scan covers: the one the caller named, or
// every joined room. Names come from JoinedRooms (which resolves them) or from
// the cache; a name is never invented.
func (c *Client) searchRooms(ctx context.Context, roomID string) ([]Room, error) {
	if roomID == "" {
		return c.JoinedRooms(ctx)
	}
	if !config.ValidRoomID(roomID) {
		return nil, ErrInvalidRoomID
	}
	// One room is cheap to name even when the cache was not seeded yet, and a
	// hit whose room_name is just its room ID is much less useful.
	nameCtx, cancel := context.WithTimeout(ctx, nameFetchTimeout)
	c.fetchName(nameCtx, roomID)
	cancel()
	return []Room{{ID: roomID, Name: c.RoomName(roomID)}}, nil
}

// matchesTerms reports whether every lowercased term appears in body. The scan
// has no index to lean on, so a multi-word query means "all of these words",
// which is what a reader expects from a search box.
func matchesTerms(body string, terms []string) bool {
	haystack := strings.ToLower(body)
	for _, term := range terms {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	return true
}

// rankNewestFirst sorts hits newest first, breaking ties deterministically, and
// cuts them to limit. It reports truncation when cutting something away.
func rankNewestFirst(hits []SearchHit, limit int, truncated bool) ([]SearchHit, bool) {
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Timestamp != hits[j].Timestamp {
			return hits[i].Timestamp > hits[j].Timestamp
		}
		if hits[i].RoomID != hits[j].RoomID {
			return hits[i].RoomID < hits[j].RoomID
		}
		return hits[i].EventID < hits[j].EventID
	})
	if len(hits) > limit {
		hits = hits[:limit]
		truncated = true
	}
	return hits, truncated
}

// isSearchUnsupported reports whether err is the homeserver refusing the search
// endpoint itself rather than one query: a route that does not exist, or a
// request shape the endpoint cannot accept. A network failure or an
// authorization error is not on this list — those are returned to the caller
// instead of being dressed up as an empty scan.
func isSearchUnsupported(err error) bool {
	return isUnsupportedEndpoint(err) ||
		errors.Is(err, mautrix.MNotFound) ||
		errors.Is(err, mautrix.MInvalidParam)
}
