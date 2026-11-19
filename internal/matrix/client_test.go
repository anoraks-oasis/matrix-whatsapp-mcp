package matrix

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"

	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/config"
)

func testLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func testConfig(homeserver string, allowed ...string) *config.Config {
	return &config.Config{
		Homeserver:       homeserver,
		UserID:           "@bot:test",
		AccessToken:      "access-token",
		AllowedSendRooms: allowed,
	}
}

func TestOnEventTypeFiltersSelfMessages(t *testing.T) {
	c, err := New(testConfig("http://127.0.0.1:1"), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	c.OnEventType(event.EventMessage, func(_ context.Context, evt *event.Event) {
		got = append(got, evt.ID.String())
	})

	syncer := c.cli.Syncer.(*mautrix.DefaultSyncer)
	dispatch := func(evt *event.Event) {
		syncer.Dispatch(context.Background(), evt)
	}

	dispatch(&event.Event{Type: event.EventMessage, ID: "$self", Sender: c.cli.UserID,
		Content: event.Content{Parsed: &event.MessageEventContent{MsgType: event.MsgText, Body: "mine"}}})
	dispatch(&event.Event{Type: event.EventMessage, ID: "$other", Sender: "@alice:test",
		Content: event.Content{Parsed: &event.MessageEventContent{MsgType: event.MsgText, Body: "theirs"}}})
	dispatch(&event.Event{Type: event.EventMessage, ID: "$self2", Sender: c.cli.UserID,
		Content: event.Content{Parsed: &event.MessageEventContent{MsgType: event.MsgText, Body: "mine again"}}})

	if len(got) != 1 || got[0] != "$other" {
		t.Fatalf("handled event IDs = %v, want exactly [$other] (self messages filtered)", got)
	}
}

// TestSyncBackfillFiltering drives mautrix's full /sync handshake against a
// fake homeserver. It proves that the client registers DontProcessOldEvents
// before the first sync: the backfill event in the initial sync is dropped,
// while the event in the first incremental sync is dispatched.
func TestSyncBackfillFiltering(t *testing.T) {
	var syncCount atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_matrix/client/v3/account/whoami":
			io.WriteString(w, `{"user_id":"@bot:test","device_id":"DEVICE"}`)
		case r.URL.Path == "/_matrix/client/v3/user/@bot:test/filter":
			io.WriteString(w, `{"filter_id":"FILTER1"}`)
		case r.URL.Path == "/_matrix/client/v3/joined_rooms":
			io.WriteString(w, `{"joined_rooms":[]}`)
		case r.URL.Path == "/_matrix/client/v3/sync":
			n := syncCount.Add(1)
			switch n {
			case 1:
				// Initial sync (no since token): historical backfill must NOT dispatch.
				io.WriteString(w, `{"next_batch":"s1","rooms":{"join":{"!r:test":{"timeline":{"events":[`+
					`{"type":"m.room.message","event_id":"$old","sender":"@alice:test","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"old backfill"}}]}}}}}`)
			case 2:
				// Incremental sync (since=s1): the new event must dispatch.
				io.WriteString(w, `{"next_batch":"s2","rooms":{"join":{"!r:test":{"timeline":{"events":[`+
					`{"type":"m.room.message","event_id":"$new","sender":"@bob:test","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"new message"}}]}}}}}`)
			default:
				// Hold the long-poll open until the test cancels the run context.
				<-r.Context().Done()
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var handled []string
	c.OnEventType(event.EventMessage, func(_ context.Context, evt *event.Event) {
		mu.Lock()
		handled = append(handled, evt.ID.String())
		mu.Unlock()
	})

	ctx, cancel := context.WithCancel(context.Background())
	syncDone := make(chan error, 1)
	go func() { syncDone <- c.Sync(ctx) }()

	// The third /sync request only starts once the second has been fully
	// processed, so waiting for it guarantees the new event was dispatched.
	deadline := time.Now().Add(5 * time.Second)
	for syncCount.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := syncCount.Load(); n < 3 {
		t.Fatalf("sync loop issued %d /sync requests, want >= 3", n)
	}

	mu.Lock()
	got := append([]string(nil), handled...)
	mu.Unlock()
	if len(got) != 1 || got[0] != "$new" {
		t.Fatalf("handled event IDs = %v, want exactly [$new] (backfill filtered, incremental processed)", got)
	}

	cancel()
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-syncDone:
		if err != nil {
			t.Fatalf("Sync returned %v, want nil on cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Sync did not return after cancellation")
	}
}

func TestSendTextNotAllowedNeverReachesServer(t *testing.T) {
	var hit atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Store(true)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL, "!ok:test"), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.SendText(context.Background(), "!not-allowed:test", "hello")
	if !errors.Is(err, ErrSendNotAllowed) {
		t.Fatalf("SendText = %v, want ErrSendNotAllowed", err)
	}
	if hit.Load() {
		t.Fatal("SendText to a non-allowlisted room reached the homeserver")
	}
}

func TestSendTextAllowedRoomIssuesPut(t *testing.T) {
	var mu sync.Mutex
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/state/m.room.encryption"):
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"errcode":"M_NOT_FOUND","error":"Not found"}`)
		case strings.Contains(r.URL.Path, "/send/m.room.message/"):
			mu.Lock()
			gotMethod = r.Method
			gotPath = r.URL.Path
			body, _ := io.ReadAll(r.Body)
			gotBody = string(body)
			mu.Unlock()
			io.WriteString(w, `{"event_id":"$send1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL, "!ok:test"), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := c.SendText(context.Background(), "!ok:test", "hello world")
	if err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if eventID != "$send1" {
		t.Fatalf("SendText returned event ID %q, want $send1", eventID)
	}

	mu.Lock()
	method, path, body := gotMethod, gotPath, gotBody
	mu.Unlock()
	if method != http.MethodPut {
		t.Fatalf("send method = %q, want PUT", method)
	}
	if !strings.Contains(path, "/rooms/!ok:test/send/m.room.message/") {
		t.Fatalf("send path = %q, want it to target room !ok:test via m.room.message", path)
	}
	if !strings.Contains(body, "hello world") {
		t.Fatalf("send body = %q, want it to contain the message text", body)
	}
}

func TestSendTextEmptyMessage(t *testing.T) {
	c, err := New(testConfig("http://127.0.0.1:1", "!ok:test"), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"", "   ", "\t\n"} {
		if _, err := c.SendText(context.Background(), "!ok:test", text); !errors.Is(err, ErrEmptyMessage) {
			t.Fatalf("SendText(%q) = %v, want ErrEmptyMessage", text, err)
		}
	}
}

func TestMessagesClampsLimitAndConverts(t *testing.T) {
	var mu sync.Mutex
	var seenLimits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/messages") {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		seenLimits = append(seenLimits, r.URL.Query().Get("limit"))
		mu.Unlock()
		io.WriteString(w, `{"chunk":[`+
			`{"type":"m.room.message","event_id":"$m1","sender":"@alice:test","origin_server_ts":1000,"content":{"msgtype":"m.text","body":"first"}},`+
			`{"type":"m.room.message","event_id":"$m2","sender":"@bob:test","origin_server_ts":2000,"content":{"msgtype":"m.text","body":"second"}}]}`)
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.Messages(context.Background(), "not-a-room", 20); !errors.Is(err, ErrInvalidRoomID) {
		t.Fatalf("Messages(invalid room) = %v, want ErrInvalidRoomID", err)
	}

	msgs, err := c.Messages(context.Background(), "!r:test", 500)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("Messages returned %d messages, want 2", len(msgs))
	}
	if msgs[0].EventID != "$m1" || msgs[1].EventID != "$m2" {
		t.Fatalf("message order = %q, %q; want $m1 then $m2 (server order preserved)", msgs[0].EventID, msgs[1].EventID)
	}
	if msgs[0].Body != "first" || msgs[0].Sender != "@alice:test" || msgs[0].Timestamp != 1000 || msgs[0].RoomID != "!r:test" {
		t.Fatalf("converted message wrong: %+v", msgs[0])
	}

	if _, err := c.Messages(context.Background(), "!r:test", 0); err != nil {
		t.Fatalf("Messages(limit=0): %v", err)
	}

	mu.Lock()
	limits := append([]string(nil), seenLimits...)
	mu.Unlock()
	if len(limits) != 2 || limits[0] != "100" || limits[1] != "20" {
		t.Fatalf("limit query params = %v, want [100 20] (500 clamped to 100, 0 defaults to 20)", limits)
	}
}

func TestJoinedRoomsResolvesNamesAndSorts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_matrix/client/v3/joined_rooms":
			io.WriteString(w, `{"joined_rooms":["!a:test","!b:test"]}`)
		case strings.Contains(r.URL.Path, "!a:test") && strings.Contains(r.URL.Path, "/state/m.room.name"):
			io.WriteString(w, `{"name":"Alpha"}`)
		case strings.Contains(r.URL.Path, "!b:test") && strings.Contains(r.URL.Path, "/state/m.room.name"):
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"errcode":"M_NOT_FOUND","error":"Not found"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	rooms, err := c.JoinedRooms(context.Background())
	if err != nil {
		t.Fatalf("JoinedRooms: %v", err)
	}
	if len(rooms) != 2 {
		t.Fatalf("JoinedRooms returned %d rooms, want 2", len(rooms))
	}
	// "!b:test" (no name, falls back to ID) sorts before "Alpha".
	if rooms[0].ID != "!b:test" || rooms[0].Name != "!b:test" {
		t.Fatalf("rooms[0] = %+v, want !b:test with name fallback", rooms[0])
	}
	if rooms[1].ID != "!a:test" || rooms[1].Name != "Alpha" {
		t.Fatalf("rooms[1] = %+v, want !a:test named Alpha", rooms[1])
	}
}

// TestCloseJoinsBackgroundWork proves Close cancels the background context and
// joins the seeded-name goroutine even while it is blocked mid-request.
func TestCloseJoinsBackgroundWork(t *testing.T) {
	entered := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_matrix/client/v3/account/whoami":
			io.WriteString(w, `{"user_id":"@bot:test","device_id":"DEVICE"}`)
		case r.URL.Path == "/_matrix/client/v3/joined_rooms":
			close(entered)
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	if err := c.init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("background name seeding never started")
	}

	closeDone := make(chan struct{})
	go func() {
		_ = c.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked while the background goroutine was mid-request; goroutine leaked")
	}
}
