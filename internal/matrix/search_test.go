package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// searchBody is the shape of the body the client POSTs to /search, narrowed to
// the fields these tests assert on.
type searchBody struct {
	SearchCategories struct {
		RoomEvents struct {
			SearchTerm string   `json:"search_term"`
			Keys       []string `json:"keys"`
			OrderBy    string   `json:"order_by"`
			Filter     struct {
				Rooms []string `json:"rooms"`
				Types []string `json:"types"`
			} `json:"filter"`
		} `json:"room_events"`
	} `json:"search_categories"`
}

// TestSearchUsesServerIndex proves the index path: the request carries the
// query, body keys, recency ordering and any room scope, and the results come
// back newest first with each hit's room name.
func TestSearchUsesServerIndex(t *testing.T) {
	var calls atomic.Int64
	var body searchBody
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/v3/search" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("search body is not JSON: %v", err)
		}
		io.WriteString(w, `{"search_categories":{"room_events":{"count":3,"results":[`+
			`{"rank":0.9,"result":{"type":"m.room.message","event_id":"$old","room_id":"!a:test","sender":"@alice:test","origin_server_ts":100,"content":{"msgtype":"m.text","body":"dinner at eight"}}},`+
			`{"rank":0.8,"result":{"type":"m.room.message","event_id":"$new","room_id":"!a:test","sender":"@bob:test","origin_server_ts":300,"content":{"msgtype":"m.text","body":"dinner tomorrow"}}},`+
			`{"rank":0.7,"result":{"type":"m.room.message","event_id":"$mid","room_id":"!b:test","sender":"@bob:test","origin_server_ts":200,"content":{"msgtype":"m.text","body":"no dinner"}}}`+
			`]}}}`)
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	c.setName("!a:test", "Alpha")
	c.setName("!b:test", "Bravo")

	out, err := c.SearchMessages(context.Background(), SearchRequest{Query: "dinner"})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if out.Source != SearchSourceIndex {
		t.Fatalf("source = %q, want %q", out.Source, SearchSourceIndex)
	}
	if out.Truncated || out.ScannedRooms != 0 || out.ScannedEvents != 0 {
		t.Fatalf("outcome = %+v, want nothing truncated and nothing scanned", out)
	}
	if len(out.Hits) != 3 {
		t.Fatalf("hits = %d, want 3", len(out.Hits))
	}
	wantOrder := []string{"$new", "$mid", "$old"}
	for i, want := range wantOrder {
		if out.Hits[i].EventID != want {
			t.Fatalf("hit %d = %s, want %s (newest first)", i, out.Hits[i].EventID, want)
		}
	}
	if out.Hits[0].RoomName != "Alpha" || out.Hits[1].RoomName != "Bravo" {
		t.Fatalf("room names = %q/%q, want Alpha/Bravo", out.Hits[0].RoomName, out.Hits[1].RoomName)
	}
	if out.Hits[0].Body != "dinner tomorrow" || out.Hits[0].Sender != "@bob:test" {
		t.Fatalf("hit body/sender = %q/%q, want the message content carried through", out.Hits[0].Body, out.Hits[0].Sender)
	}

	if calls.Load() != 1 {
		t.Fatalf("/search calls = %d, want 1", calls.Load())
	}
	if body.SearchCategories.RoomEvents.SearchTerm != "dinner" {
		t.Fatalf("search_term = %q, want dinner", body.SearchCategories.RoomEvents.SearchTerm)
	}
	if got := strings.Join(body.SearchCategories.RoomEvents.Keys, ","); got != "content.body" {
		t.Fatalf("keys = %q, want content.body (match the body, not senders)", got)
	}
	if body.SearchCategories.RoomEvents.OrderBy != "recent" {
		t.Fatalf("order_by = %q, want recent", body.SearchCategories.RoomEvents.OrderBy)
	}
	if got := strings.Join(body.SearchCategories.RoomEvents.Filter.Types, ","); got != "m.room.message" {
		t.Fatalf("filter types = %q, want m.room.message", got)
	}
	if len(body.SearchCategories.RoomEvents.Filter.Rooms) != 0 {
		t.Fatalf("filter rooms = %v, want none for an unscoped search", body.SearchCategories.RoomEvents.Filter.Rooms)
	}
}

// TestSearchAppliesLimitAndSince is the index path's budget: the caller's limit
// caps the hits and marks the answer truncated, and since_timestamp filters.
func TestSearchAppliesLimitAndSince(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"search_categories":{"room_events":{"count":3,"results":[`+
			`{"result":{"type":"m.room.message","event_id":"$old","room_id":"!a:test","sender":"@a:test","origin_server_ts":100,"content":{"msgtype":"m.text","body":"one"}}},`+
			`{"result":{"type":"m.room.message","event_id":"$mid","room_id":"!a:test","sender":"@a:test","origin_server_ts":200,"content":{"msgtype":"m.text","body":"two"}}},`+
			`{"result":{"type":"m.room.message","event_id":"$new","room_id":"!a:test","sender":"@a:test","origin_server_ts":300,"content":{"msgtype":"m.text","body":"three"}}}`+
			`]}}}`)
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}

	limited, err := c.SearchMessages(context.Background(), SearchRequest{Query: "o", Limit: 2})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if len(limited.Hits) != 2 || !limited.Truncated {
		t.Fatalf("limit 2 = %d hits, truncated %t; want 2 and truncated", len(limited.Hits), limited.Truncated)
	}
	if limited.Hits[0].EventID != "$new" || limited.Hits[1].EventID != "$mid" {
		t.Fatalf("limited hits = %+v, want the two newest", limited.Hits)
	}

	since, err := c.SearchMessages(context.Background(), SearchRequest{Query: "o", SinceTimestamp: 200})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	var ids []string
	for _, hit := range since.Hits {
		ids = append(ids, hit.EventID)
	}
	if strings.Join(ids, ",") != "$new,$mid" {
		t.Fatalf("since 200 hits = %v, want [$new $mid] (at or after the timestamp)", ids)
	}
}

// TestSearchFallsBackWhenIndexUnavailable proves the fallback and its memory:
// a server that answers M_UNRECOGNIZED for /search is scanned instead, and the
// refusal is not repeated on the next call.
func TestSearchFallsBackWhenIndexUnavailable(t *testing.T) {
	var searches, messagePages atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/search"):
			searches.Add(1)
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"errcode":"M_UNRECOGNIZED","error":"Unrecognized request"}`)
		case strings.HasSuffix(r.URL.Path, "/joined_rooms"):
			io.WriteString(w, `{"joined_rooms":["!a:test","!b:test"]}`)
		case strings.Contains(r.URL.Path, "/messages"):
			messagePages.Add(1)
			room := strings.Split(strings.TrimPrefix(r.URL.Path, "/_matrix/client/v3/rooms/"), "/")[0]
			switch room {
			case "!a:test":
				io.WriteString(w, `{"chunk":[`+textEvent("$a2", 300, "DINNER tonight")+`,`+textEvent("$a1", 100, "lunch")+`],"end":""}`)
			default:
				io.WriteString(w, `{"chunk":[`+textEvent("$b1", 200, "dinner plans")+`],"end":""}`)
			}
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	c.setName("!a:test", "Alpha")
	c.setName("!b:test", "Bravo")

	out, err := c.SearchMessages(context.Background(), SearchRequest{Query: "DiNnEr"})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if out.Source != SearchSourceScan {
		t.Fatalf("source = %q, want %q", out.Source, SearchSourceScan)
	}
	if out.Truncated {
		t.Fatalf("outcome truncated = true, want false: both rooms were read to the end")
	}
	if out.ScannedRooms != 2 {
		t.Fatalf("scanned rooms = %d, want 2", out.ScannedRooms)
	}
	if out.ScannedEvents != 3 {
		t.Fatalf("scanned events = %d, want 3", out.ScannedEvents)
	}
	var ids []string
	var rooms []string
	for _, hit := range out.Hits {
		ids = append(ids, hit.EventID)
		rooms = append(rooms, hit.RoomName)
	}
	// $a2 matches despite the case difference, $b1 matches, and $a1 ("lunch")
	// is filtered out by the same predicate: the 3 scanned events yield 2 hits.
	if strings.Join(ids, ",") != "$a2,$b1" {
		t.Fatalf("hits = %v, want [$a2 $b1] newest first, matched case-insensitively", ids)
	}
	if strings.Join(rooms, ",") != "Alpha,Bravo" {
		t.Fatalf("hit rooms = %v, want each hit labelled with its room", rooms)
	}

	// The refusal is remembered: a second search scans without re-POSTing.
	if _, err := c.SearchMessages(context.Background(), SearchRequest{Query: "dinner"}); err != nil {
		t.Fatalf("second SearchMessages: %v", err)
	}
	if searches.Load() != 1 {
		t.Fatalf("/search requests = %d, want 1 (the refusal is cached per process)", searches.Load())
	}
	if messagePages.Load() != 4 {
		t.Fatalf("/messages requests = %d, want 4 (2 rooms on each of 2 searches)", messagePages.Load())
	}
}

// TestSearchRoomScopeAndUnreadableRoom covers two scan details: a named room is
// the only one read (with its name resolved), and a room that cannot be read
// does not fail the search but does mark it truncated.
func TestSearchRoomScopeAndUnreadableRoom(t *testing.T) {
	t.Run("named room", func(t *testing.T) {
		var joined int
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/search"):
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"errcode":"M_UNRECOGNIZED"}`)
			case strings.Contains(r.URL.Path, "/state/m.room.name"):
				io.WriteString(w, `{"name":"Scoped room"}`)
			case strings.Contains(r.URL.Path, "/messages"):
				io.WriteString(w, `{"chunk":[`+textEvent("$x", 100, "needle")+`],"end":""}`)
			case strings.HasSuffix(r.URL.Path, "/joined_rooms"):
				joined++
				io.WriteString(w, `{"joined_rooms":[]}`)
			default:
				t.Errorf("unexpected request %s", r.URL.Path)
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		c, err := New(testConfig(srv.URL), testLogger())
		if err != nil {
			t.Fatal(err)
		}
		out, err := c.SearchMessages(context.Background(), SearchRequest{Query: "needle", RoomID: "!only:test"})
		if err != nil {
			t.Fatalf("SearchMessages: %v", err)
		}
		if joined != 0 {
			t.Fatalf("joined_rooms was called %d times, want 0 for a room-scoped search", joined)
		}
		if out.ScannedRooms != 1 || out.ScannedEvents != 1 {
			t.Fatalf("scanned = %d rooms / %d events, want 1/1", out.ScannedRooms, out.ScannedEvents)
		}
		if len(out.Hits) != 1 || out.Hits[0].RoomName != "Scoped room" {
			t.Fatalf("hits = %+v, want one hit labelled with the room's name", out.Hits)
		}
	})

	t.Run("unreadable room in a multi-room scan", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/search"):
				w.WriteHeader(http.StatusNotFound)
				io.WriteString(w, `{"errcode":"M_UNRECOGNIZED"}`)
			case strings.HasSuffix(r.URL.Path, "/joined_rooms"):
				io.WriteString(w, `{"joined_rooms":["!broken:test","!fine:test"]}`)
			case strings.Contains(r.URL.Path, "/messages"):
				if strings.Contains(r.URL.Path, "!broken:test") {
					w.WriteHeader(http.StatusForbidden)
					io.WriteString(w, `{"errcode":"M_FORBIDDEN"}`)
					return
				}
				io.WriteString(w, `{"chunk":[`+textEvent("$ok", 100, "needle")+`],"end":""}`)
			default:
				http.NotFound(w, r)
			}
		}))
		defer srv.Close()

		c, err := New(testConfig(srv.URL), testLogger())
		if err != nil {
			t.Fatal(err)
		}
		c.setName("!broken:test", "Broken")
		c.setName("!fine:test", "Fine")

		out, err := c.SearchMessages(context.Background(), SearchRequest{Query: "needle"})
		if err != nil {
			t.Fatalf("one unreadable room failed the whole search: %v", err)
		}
		if len(out.Hits) != 1 || out.Hits[0].EventID != "$ok" {
			t.Fatalf("hits = %+v, want the readable room's match", out.Hits)
		}
		if !out.Truncated {
			t.Fatal("truncated = false after skipping a room: the answer is partial and must say so")
		}
		if out.ScannedRooms != 1 {
			t.Fatalf("scanned rooms = %d, want 1 (the unreadable room was not scanned)", out.ScannedRooms)
		}

		// A room the caller named, on the other hand, must fail loudly rather
		// than report an empty search.
		if _, err := c.SearchMessages(context.Background(), SearchRequest{Query: "needle", RoomID: "!broken:test"}); err == nil {
			t.Fatal("a room-scoped search of an unreadable room returned no error")
		}
	})
}

// TestSearchFallbackIsBounded is the budget proof: a fake that answers every
// query with 200 matching messages per room and 12 rooms must be cut off, and
// must say so.
func TestSearchFallbackIsBounded(t *testing.T) {
	var messagePages atomic.Int64
	events := make([]string, 0, 200)
	for i := 0; i < 200; i++ {
		events = append(events, textEvent("$m"+itoa(int64(i)), int64(1000+i), "boom"))
	}
	page := `{"chunk":[` + strings.Join(events, ",") + `],"end":"next"}`
	rooms := make([]string, 0, 12)
	for i := 0; i < 12; i++ {
		rooms = append(rooms, `"!r`+itoa(int64(i))+`:test"`)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/search"):
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"errcode":"M_UNRECOGNIZED"}`)
		case strings.HasSuffix(r.URL.Path, "/joined_rooms"):
			io.WriteString(w, `{"joined_rooms":[`+strings.Join(rooms, ",")+`]}`)
		case strings.Contains(r.URL.Path, "/messages"):
			messagePages.Add(1)
			io.WriteString(w, page)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		// Names in room order so the room the scan takes first is deterministic.
		c.setName("!r"+itoa(int64(i))+":test", "Room "+string(rune('A'+i)))
	}

	out, err := c.SearchMessages(context.Background(), SearchRequest{Query: "boom", Limit: 20})
	if err != nil {
		t.Fatalf("SearchMessages: %v", err)
	}
	if !out.Truncated {
		t.Fatal("truncated = false: a search that hit its budget must say so")
	}
	if len(out.Hits) != 20 {
		t.Fatalf("hits = %d, want the requested 20 of 2400 matches", len(out.Hits))
	}
	if out.ScannedRooms != SearchMaxRooms {
		t.Fatalf("scanned rooms = %d, want %d (the room budget)", out.ScannedRooms, SearchMaxRooms)
	}
	if want := SearchMaxRooms * SearchEventsPerRoom; out.ScannedEvents != want {
		t.Fatalf("scanned events = %d, want %d (the per-room budget)", out.ScannedEvents, want)
	}
	if got := messagePages.Load(); got != int64(SearchMaxRooms) {
		t.Fatalf("/messages pages = %d, want %d: the scan must stop at the room budget, never fan out over all 12", got, SearchMaxRooms)
	}
	for i := 1; i < len(out.Hits); i++ {
		if out.Hits[i-1].Timestamp < out.Hits[i].Timestamp {
			t.Fatalf("hit %d is newer than the one before it: results must be newest first", i)
		}
	}
}

// TestSearchRejectsBadInput proves the query and room checks happen before any
// request is made.
func TestSearchRejectsBadInput(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.SearchMessages(context.Background(), SearchRequest{Query: "   "}); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("empty query error = %v, want ErrEmptyQuery", err)
	}
	if _, err := c.SearchMessages(context.Background(), SearchRequest{Query: "x", RoomID: "not-a-room"}); !errors.Is(err, ErrInvalidRoomID) {
		t.Fatalf("invalid room error = %v, want ErrInvalidRoomID", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("a rejected search made %d requests, want 0", requests.Load())
	}
}

// TestSearchHonoursCancellation proves a cancelled call returns the context's
// error instead of a half-filled result.
func TestSearchHonoursCancellation(t *testing.T) {
	started := make(chan struct{}, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-time.After(300 * time.Millisecond):
		}
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()

	_, err = c.SearchMessages(ctx, SearchRequest{Query: "dinner"})
	if err == nil {
		t.Fatal("SearchMessages returned no error after cancellation")
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("SearchMessages error = %v, want the context error", err)
	}
}
