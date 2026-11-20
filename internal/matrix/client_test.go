package matrix

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

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
	_, err = c.SendText(context.Background(), "!not-allowed:test", "hello", "")
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
	eventID, err := c.SendText(context.Background(), "!ok:test", "hello world", "")
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
		if _, err := c.SendText(context.Background(), "!ok:test", text, ""); !errors.Is(err, ErrEmptyMessage) {
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

	if _, err := c.Messages(context.Background(), "not-a-room", 20, ""); !errors.Is(err, ErrInvalidRoomID) {
		t.Fatalf("Messages(invalid room) = %v, want ErrInvalidRoomID", err)
	}

	page, err := c.Messages(context.Background(), "!r:test", 500, "")
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	msgs := page.Messages
	if len(msgs) != 2 {
		t.Fatalf("Messages returned %d messages, want 2", len(msgs))
	}
	if msgs[0].EventID != "$m1" || msgs[1].EventID != "$m2" {
		t.Fatalf("message order = %q, %q; want $m1 then $m2 (server order preserved)", msgs[0].EventID, msgs[1].EventID)
	}
	if msgs[0].Body != "first" || msgs[0].Sender != "@alice:test" || msgs[0].Timestamp != 1000 || msgs[0].RoomID != "!r:test" {
		t.Fatalf("converted message wrong: %+v", msgs[0])
	}

	if _, err := c.Messages(context.Background(), "!r:test", 0, ""); err != nil {
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

// domainlessRoomID has the shape the live WhatsApp bridge actually hands out:
// no ":server" part. Requiring one rejected every real room.
const domainlessRoomID = "!UBRW6WycBKpUJu3m4HP0RCMsGnUp_EwSZa2RLXHe34Y"

func TestRoomIDValidation(t *testing.T) {
	var mu sync.Mutex
	var sentPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/state/m.room.encryption"):
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"errcode":"M_NOT_FOUND","error":"Not found"}`)
		case strings.Contains(r.URL.Path, "/messages"):
			io.WriteString(w, `{"chunk":[]}`)
		case strings.Contains(r.URL.Path, "/send/m.room.message/"):
			mu.Lock()
			sentPaths = append(sentPaths, r.URL.Path)
			mu.Unlock()
			io.WriteString(w, `{"event_id":"$send1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL, domainlessRoomID), testLogger())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.Messages(context.Background(), domainlessRoomID, 10, ""); err != nil {
		t.Fatalf("Messages(%q) = %v, want success", domainlessRoomID, err)
	}
	if _, err := c.SendText(context.Background(), domainlessRoomID, "hi", ""); err != nil {
		t.Fatalf("SendText(%q) = %v, want success", domainlessRoomID, err)
	}

	// The 255-byte cap is inclusive.
	boundary := "!" + strings.Repeat("x", 254)
	if _, err := c.Messages(context.Background(), boundary, 10, ""); err != nil {
		t.Fatalf("Messages(%d-byte room ID) = %v, want success", len(boundary), err)
	}

	rejected := []string{
		"",
		"not-a-room",
		"!a/b",
		"!a b",
		"!a\tb",
		"!a?b",
		"!a#b",
		"!" + strings.Repeat("x", 255),
	}
	for _, roomID := range rejected {
		if _, err := c.Messages(context.Background(), roomID, 10, ""); !errors.Is(err, ErrInvalidRoomID) {
			t.Errorf("Messages(%q) = %v, want ErrInvalidRoomID", roomID, err)
		}
		if _, err := c.SendText(context.Background(), roomID, "hi", ""); !errors.Is(err, ErrInvalidRoomID) {
			t.Errorf("SendText(%q) = %v, want ErrInvalidRoomID", roomID, err)
		}
	}

	mu.Lock()
	paths := append([]string(nil), sentPaths...)
	mu.Unlock()
	want := "/rooms/" + domainlessRoomID + "/send/m.room.message/"
	if len(paths) != 1 || !strings.Contains(paths[0], want) {
		t.Fatalf("send paths = %v, want exactly one send to %s", paths, want)
	}
}

// messagesRequest is one recorded /messages call.
type messagesRequest struct {
	from  string
	limit string
}

// messagesServer serves scripted /messages pages. page(n) returns the raw chunk
// JSON and the end token for request n; requests past the script fail the test.
func messagesServer(t *testing.T, page func(n int) (chunk, end string)) (*httptest.Server, func() []messagesRequest) {
	t.Helper()
	var mu sync.Mutex
	var reqs []messagesRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/messages") {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		n := len(reqs)
		reqs = append(reqs, messagesRequest{from: r.URL.Query().Get("from"), limit: r.URL.Query().Get("limit")})
		mu.Unlock()
		chunk, end := page(n)
		io.WriteString(w, `{"chunk":[`+chunk+`],"end":"`+end+`"}`)
	}))
	return srv, func() []messagesRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]messagesRequest(nil), reqs...)
	}
}

func textEvent(id string, ts int64, body string) string {
	return `{"type":"m.room.message","event_id":"` + id + `","sender":"@alice:test","origin_server_ts":` +
		itoa(ts) + `,"content":{"msgtype":"m.text","body":"` + body + `"}}`
}

func memberEvent(id, user string) string {
	return `{"type":"m.room.member","event_id":"` + id + `","sender":"` + user + `","state_key":"` + user +
		`","origin_server_ts":1,"content":{"membership":"join"}}`
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestMessagesPagesPastNonMessageEvents proves that Messages keeps paging
// backward with the /messages end token until it has the requested number of
// convertible messages: the live defect was a page full of member events
// yielding fewer messages than asked for.
func TestMessagesPagesPastNonMessageEvents(t *testing.T) {
	srv, requests := messagesServer(t, func(n int) (string, string) {
		switch n {
		case 0:
			return memberEvent("$j1", "@alice:test") + "," + textEvent("$m1", 100, "one") + "," +
				memberEvent("$j2", "@bob:test") + "," + textEvent("$m2", 200, "two"), "page2"
		case 1:
			return textEvent("$m3", 300, "three") + "," + textEvent("$m4", 400, "four"), ""
		default:
			t.Errorf("unexpected /messages request %d", n+1)
			return "", ""
		}
	})
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	page, err := c.Messages(context.Background(), "!r:test", 4, "")
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	msgs := page.Messages
	var got []string
	for _, m := range msgs {
		got = append(got, m.EventID)
	}
	want := []string{"$m1", "$m2", "$m3", "$m4"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("Messages = %v, want %v (server order, paged past member events)", got, want)
	}

	reqs := requests()
	if len(reqs) != 2 {
		t.Fatalf("/messages requests = %d (%+v), want 2", len(reqs), reqs)
	}
	if reqs[0].from != "" || reqs[0].limit != "4" {
		t.Fatalf("first request = %+v, want from=\"\" limit=4", reqs[0])
	}
	if reqs[1].from != "page2" || reqs[1].limit != "2" {
		t.Fatalf("second request = %+v, want from=page2 limit=2 (2 messages still missing)", reqs[1])
	}
}

// TestMessagesPaginationIsBounded proves the two hard bounds: page count and
// raw events scanned, so a room that converts nothing cannot make read_messages
// walk forever.
func TestMessagesPaginationIsBounded(t *testing.T) {
	t.Run("page cap", func(t *testing.T) {
		srv, requests := messagesServer(t, func(n int) (string, string) {
			return memberEvent("$j"+itoa(int64(n)), "@alice:test"), "next" + itoa(int64(n))
		})
		defer srv.Close()

		c, err := New(testConfig(srv.URL), testLogger())
		if err != nil {
			t.Fatal(err)
		}
		page, err := c.Messages(context.Background(), "!r:test", 2, "")
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
		msgs := page.Messages
		if len(msgs) != 0 {
			t.Fatalf("Messages = %+v, want none (only member events)", msgs)
		}
		if n := len(requests()); n != maxMessagePages {
			t.Fatalf("/messages requests = %d, want %d (page cap)", n, maxMessagePages)
		}
	})

	t.Run("raw event budget", func(t *testing.T) {
		var chunk []string
		for i := 0; i < 5; i++ {
			chunk = append(chunk, memberEvent("$j"+itoa(int64(i)), "@alice:test"))
		}
		srv, requests := messagesServer(t, func(n int) (string, string) {
			return strings.Join(chunk, ","), "next" + itoa(int64(n))
		})
		defer srv.Close()

		c, err := New(testConfig(srv.URL), testLogger())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Messages(context.Background(), "!r:test", 2, ""); err != nil {
			t.Fatalf("Messages: %v", err)
		}
		want := (maxMessageRawFactor * 2) / 5
		if n := len(requests()); n != want {
			t.Fatalf("/messages requests = %d, want %d (raw event budget)", n, want)
		}
	})
}

// TestMessagesMediaPlaceholder covers the live media defect: a bridge sends
// m.image with an empty body, which surfaced as a blank message.
func TestMessagesMediaPlaceholder(t *testing.T) {
	page := `{"type":"m.room.message","event_id":"$img","sender":"@wa:test","origin_server_ts":100,"content":{"msgtype":"m.image","body":""}},` +
		`{"type":"m.room.message","event_id":"$txt","sender":"@wa:test","origin_server_ts":101,"content":{"msgtype":"m.text","body":"hello there"}},` +
		`{"type":"m.room.message","event_id":"$cap","sender":"@wa:test","origin_server_ts":102,"content":{"msgtype":"m.image","body":"a caption"}},` +
		`{"type":"m.room.message","event_id":"$file","sender":"@wa:test","origin_server_ts":103,"content":{"msgtype":"m.file","body":""}},` +
		`{"type":"m.room.message","event_id":"$note","sender":"@wa:test","origin_server_ts":104,"content":{"msgtype":"m.notice","body":""}}`
	srv, _ := messagesServer(t, func(int) (string, string) { return page, "" })
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	result, err := c.Messages(context.Background(), "!r:test", 10, "")
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	msgs := result.Messages
	if len(msgs) != 5 {
		t.Fatalf("Messages returned %d messages, want 5", len(msgs))
	}
	wantBodies := map[string]string{
		"$img":  "[image]",
		"$txt":  "hello there",
		"$cap":  "a caption",
		"$file": "[file]",
		"$note": "",
	}
	wantTypes := map[string]string{
		"$img":  "m.image",
		"$txt":  "m.text",
		"$cap":  "m.image",
		"$file": "m.file",
		"$note": "m.notice",
	}
	for _, m := range msgs {
		if m.Body != wantBodies[m.EventID] {
			t.Errorf("%s body = %q, want %q", m.EventID, m.Body, wantBodies[m.EventID])
		}
		if m.MsgType != wantTypes[m.EventID] {
			t.Errorf("%s msgtype = %q, want %q", m.EventID, m.MsgType, wantTypes[m.EventID])
		}
	}
}

// bridgeAckServer is a fake homeserver for the bridge-ack guard. sendEvent is
// the event ID returned by the send endpoint; messages is the JSON chunk served
// by /messages (empty string serves an empty chunk).
type bridgeAckServer struct {
	mu       sync.Mutex
	messages []string
	sends    int
	polls    int
	sendID   string // event ID returned by the send endpoint; defaults to $send1
}

func (b *bridgeAckServer) setMessages(chunk string) {
	b.mu.Lock()
	b.messages = []string{chunk}
	b.mu.Unlock()
}

func (b *bridgeAckServer) counts() (sends, polls int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sends, b.polls
}

func (b *bridgeAckServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/state/m.room.encryption"):
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"errcode":"M_NOT_FOUND","error":"Not found"}`)
		case strings.Contains(r.URL.Path, "/send/m.room.message/"):
			b.mu.Lock()
			b.sends++
			sendID := b.sendID
			b.mu.Unlock()
			if sendID == "" {
				sendID = "$send1"
			}
			io.WriteString(w, `{"event_id":"`+sendID+`"}`)
		case strings.Contains(r.URL.Path, "/messages"):
			b.mu.Lock()
			b.polls++
			chunk := ""
			if len(b.messages) > 0 {
				chunk = b.messages[0]
			}
			b.mu.Unlock()
			io.WriteString(w, `{"chunk":[`+chunk+`],"end":""}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testConfigWithAck(homeserver string, ack time.Duration, allowed ...string) *config.Config {
	cfg := testConfig(homeserver, allowed...)
	cfg.BridgeAckTimeout = ack
	return cfg
}

// TestSendTextBridgeNoticeFailsClosed reproduces the live hole: Matrix accepts
// the event, the bridge then posts a notice relating to it, and the caller must
// see a failure rather than a delivered-looking success.
func TestSendTextBridgeNoticeFailsClosed(t *testing.T) {
	fake := &bridgeAckServer{}
	fake.setMessages(`{"type":"m.room.message","event_id":"$n1","sender":"@whatsappbot:test","origin_server_ts":2,` +
		`"content":{"msgtype":"m.notice","body":"⚠️ Your message was not bridged: You're not logged in\n(relay not set)\u001b[0m",` +
		`"m.relates_to":{"rel_type":"m.reference","event_id":"$send1"}}}`)
	srv := fake.start(t)

	c, err := New(testConfigWithAck(srv.URL, 250*time.Millisecond, "!ok:test"), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	eventID, err := c.SendText(context.Background(), "!ok:test", "hello world", "")
	if err == nil {
		t.Fatalf("SendText = %q, nil; want a bridge rejection error", eventID)
	}
	if !errors.Is(err, ErrNotBridged) {
		t.Fatalf("SendText error = %v, want it to match ErrNotBridged", err)
	}
	var noticeErr *BridgeNoticeError
	if !errors.As(err, &noticeErr) {
		t.Fatalf("SendText error %v is not a *BridgeNoticeError", err)
	}
	if noticeErr.EventID != "$send1" {
		t.Fatalf("BridgeNoticeError.EventID = %q, want $send1", noticeErr.EventID)
	}
	if !strings.Contains(noticeErr.Notice, "Your message was not bridged") || !strings.Contains(noticeErr.Notice, "(relay not set)") {
		t.Fatalf("BridgeNoticeError.Notice = %q, want the bridge's notice text", noticeErr.Notice)
	}
	if strings.ContainsAny(noticeErr.Notice, "\n\r\t") || strings.ContainsFunc(noticeErr.Notice, unicode.IsControl) {
		t.Fatalf("BridgeNoticeError.Notice = %q, want a single line with no control characters", noticeErr.Notice)
	}
	if msg := err.Error(); !strings.Contains(msg, "$send1") || !strings.Contains(msg, "not bridged") {
		t.Fatalf("send error text = %q, want it to name the event and the notice", msg)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("SendText took %v, want it to return as soon as the notice appears", elapsed)
	}
}

// TestSendTextNoNoticeSucceeds proves the guard does not fail a normal send.
func TestSendTextNoNoticeSucceeds(t *testing.T) {
	fake := &bridgeAckServer{}
	// A notice for a different event, plus an ordinary message: neither is the
	// bridge rejecting our send.
	fake.setMessages(`{"type":"m.room.message","event_id":"$n1","sender":"@whatsappbot:test","origin_server_ts":2,` +
		`"content":{"msgtype":"m.notice","body":"different notice","m.relates_to":{"rel_type":"m.reference","event_id":"$other"}}}` +
		`,` + textEvent("$m1", 3, "hi"))
	srv := fake.start(t)

	c, err := New(testConfigWithAck(srv.URL, 50*time.Millisecond, "!ok:test"), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	eventID, err := c.SendText(context.Background(), "!ok:test", "hello world", "")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("SendText: %v, want success when no bridge notice appears", err)
	}
	if eventID != "$send1" {
		t.Fatalf("SendText event ID = %q, want $send1", eventID)
	}
	if elapsed < 40*time.Millisecond || elapsed > time.Second {
		t.Fatalf("SendText waited %v, want roughly the configured 50ms window", elapsed)
	}
	if _, polls := fake.counts(); polls == 0 {
		t.Fatal("guard never polled the room, so it could not have seen a notice")
	}
}

// TestSendTextAckDisabledIssuesNoExtraRequest proves MATRIX_BRIDGE_ACK_TIMEOUT=0
// turns the guard off entirely: no poll, no wait.
func TestSendTextAckDisabledIssuesNoExtraRequest(t *testing.T) {
	fake := &bridgeAckServer{}
	fake.setMessages(`{"type":"m.room.message","event_id":"$n1","sender":"@whatsappbot:test","origin_server_ts":2,` +
		`"content":{"msgtype":"m.notice","body":"would have been a rejection","m.relates_to":{"rel_type":"m.reference","event_id":"$send1"}}}`)
	srv := fake.start(t)

	c, err := New(testConfigWithAck(srv.URL, 0, "!ok:test"), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := c.SendText(context.Background(), "!ok:test", "hello world", ""); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("SendText took %v with the guard disabled, want no wait at all", elapsed)
	}
	sends, polls := fake.counts()
	if sends != 1 || polls != 0 {
		t.Fatalf("requests = %d sends, %d polls; want 1 send and no /messages poll", sends, polls)
	}
}

// TestSanitizeNoticeBoundsAndFlattens covers the notice text hygiene rule.
func TestSanitizeNoticeBoundsAndFlattens(t *testing.T) {
	got := sanitizeNotice("  line one\nline\ttwo\x00end  ")
	if got != "line one line two end" {
		t.Fatalf("sanitizeNotice = %q, want a flattened single line", got)
	}
	if empty := sanitizeNotice(" \n\t "); empty != "(empty notice)" {
		t.Fatalf("sanitizeNotice(blank) = %q, want a placeholder", empty)
	}
	long := sanitizeNotice(strings.Repeat("x", 1000))
	if len(long) != 303 || !strings.HasSuffix(long, "...") {
		t.Fatalf("sanitizeNotice(1000 chars) produced %d chars, want a 300-char bound", len(long))
	}
}

// liveNotBridgedEventID is the event ID from the live failure: the server
// reported success, and the bridge then rejected it with a notice.
const liveNotBridgedEventID = "$yXXiCBRYgaKwxnPTokViHJLeaZ8gOhnk3iQIcYBX8i0"

// TestSendTextLiveBridgeNoticeShape replays the bridge notice captured from the
// real homeserver byte for byte. It relates to the sent event through
// m.relates_to.m.in_reply_to.event_id (not the flat m.relates_to.event_id),
// carries m.mentions and an emoji, and was missed by the original matcher.
func TestSendTextLiveBridgeNoticeShape(t *testing.T) {
	const liveNotice = `{"body":"⚠️ Your message was not bridged: You're not logged in (relay not set and receiver login is owned by @anorak:oasis, not @mcp-bot:oasis)",` +
		`"m.mentions":{"user_ids":["@mcp-bot:oasis"]},` +
		`"m.relates_to":{"m.in_reply_to":{"event_id":"` + liveNotBridgedEventID + `"}},` +
		`"msgtype":"m.notice"}`

	fake := &bridgeAckServer{sendID: liveNotBridgedEventID}
	fake.setMessages(`{"type":"m.room.message","event_id":"$notice","sender":"@whatsappbot:oasis","origin_server_ts":1763600000000,"content":` + liveNotice + `}`)
	srv := fake.start(t)

	c, err := New(testConfigWithAck(srv.URL, 250*time.Millisecond, "!ok:test"), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := c.SendText(context.Background(), "!ok:test", "hello world", "")
	if err == nil {
		t.Fatalf("SendText = %q, nil; want the live bridge rejection to surface", eventID)
	}
	if !errors.Is(err, ErrNotBridged) {
		t.Fatalf("SendText error = %v, want it to match ErrNotBridged", err)
	}
	var noticeErr *BridgeNoticeError
	if !errors.As(err, &noticeErr) {
		t.Fatalf("SendText error %v is not a *BridgeNoticeError", err)
	}
	if noticeErr.EventID != liveNotBridgedEventID {
		t.Fatalf("BridgeNoticeError.EventID = %q, want %q", noticeErr.EventID, liveNotBridgedEventID)
	}
	for _, want := range []string{
		"Your message was not bridged",
		"relay not set",
		"@anorak:oasis",
		"@mcp-bot:oasis",
	} {
		if !strings.Contains(noticeErr.Notice, want) {
			t.Fatalf("BridgeNoticeError.Notice = %q, want it to contain %q", noticeErr.Notice, want)
		}
	}
	if strings.ContainsAny(noticeErr.Notice, "\n\r\t") || strings.ContainsFunc(noticeErr.Notice, unicode.IsControl) {
		t.Fatalf("BridgeNoticeError.Notice = %q, want a single line with no control characters", noticeErr.Notice)
	}
}

// TestRelatesToEventID covers both relation shapes a bridge may emit.
func TestRelatesToEventID(t *testing.T) {
	flat := &event.RelatesTo{Type: event.RelReference, EventID: "$flat"}
	reply := &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: "$reply"}}
	both := &event.RelatesTo{EventID: "$flat", InReplyTo: &event.InReplyTo{EventID: "$reply"}}
	for _, tc := range []struct {
		name string
		rel  *event.RelatesTo
		want id.EventID
	}{
		{"nil", nil, ""},
		{"flat", flat, "$flat"},
		{"reply", reply, "$reply"},
		{"flat wins when both are present", both, "$flat"},
		{"empty reply", &event.RelatesTo{InReplyTo: &event.InReplyTo{}}, ""},
		{"empty relation", &event.RelatesTo{}, ""},
	} {
		if got := relatesToEventID(tc.rel); got != tc.want {
			t.Fatalf("%s: relatesToEventID = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// inviteSyncBody is one /sync response for the auto-join tests: one invitation
// from sender, carried by the invite state exactly as a real homeserver sends
// it. batch is the response's next_batch, so consecutive responses differ the
// way a real server's do.
func inviteSyncBody(sender, batch string) string {
	return `{"next_batch":"` + batch + `","rooms":{"invite":{"!invited:test":{"invite_state":{"events":[` +
		`{"type":"m.room.member","state_key":"@bot:test","sender":"` + sender + `","content":{"membership":"invite"}},` +
		`{"type":"m.room.name","state_key":"","sender":"` + sender + `","content":{"name":"Family"}}]}}}}}`
}

// inviteServer is a fake homeserver that sends the same invitation in two
// consecutive /sync responses, the way a server repeats a pending invite while
// the join is in flight, and records every join request.
//
// A join request blocks in the handler until joinGate is closed or the request
// is cancelled. That is what makes the two auto-join properties testable: a
// join still in flight when the next response repeats the invitation is the
// only way to exercise the in-flight dedupe, and a join that blocks proves the
// sync loop kept going without it.
type inviteServer struct {
	sender   string
	inviteOn int64
	// joinGate releases held joins and is opened by the test's cleanup.
	joinGate chan struct{}

	syncCount atomic.Int64
	joinCount atomic.Int64
	joinPath  atomic.Value
}

func (s *inviteServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	if s.joinGate == nil {
		s.joinGate = make(chan struct{})
	}
	t.Cleanup(func() {
		select {
		case <-s.joinGate:
		default:
			close(s.joinGate)
		}
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_matrix/client/v3/account/whoami":
			io.WriteString(w, `{"user_id":"@bot:test","device_id":"DEVICE"}`)
		case r.URL.Path == "/_matrix/client/v3/user/@bot:test/filter":
			io.WriteString(w, `{"filter_id":"FILTER1"}`)
		case r.URL.Path == "/_matrix/client/v3/joined_rooms":
			io.WriteString(w, `{"joined_rooms":[]}`)
		case strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/") && strings.HasSuffix(r.URL.Path, "/join"):
			s.joinCount.Add(1)
			s.joinPath.Store(r.URL.Path)
			// Consume the body before blocking: until it is read, net/http will
			// not start the background read that notices the client hanging up,
			// so a cancelled join would leave this handler stuck.
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-s.joinGate:
			case <-r.Context().Done():
			}
			io.WriteString(w, `{"room_id":"!invited:test"}`)
		case r.URL.Path == "/_matrix/client/v3/sync":
			n := s.syncCount.Add(1)
			switch {
			case n == s.inviteOn || n == s.inviteOn+1:
				io.WriteString(w, inviteSyncBody(s.sender, "s"+itoa(n)))
			case n < s.inviteOn:
				io.WriteString(w, `{"next_batch":"s0"}`)
			default:
				// Hold the long-poll open until the test cancels the run.
				<-r.Context().Done()
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runSync starts the client's sync loop and returns a stop function that
// cancels, closes and joins it.
func runSync(t *testing.T, c *Client) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Sync(ctx) }()
	return func() {
		cancel()
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Sync returned %v, want nil on cancellation", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Sync did not return after cancellation")
		}
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestAutoJoinAcceptsInviteFromAllowlistedInviter also proves the offline case:
// the invitation arrives in the first sync response, which DontProcessOldEvents
// drops for event handlers, so joining here means an invite that landed while
// the bot was down is not lost.
//
// The fake repeats the invitation in the next response and holds the join open,
// so the second invitation arrives while the first join is still in flight.
// Both properties MATRIX_AUTO_JOIN_INVITES exists for are then observable: only
// one join is issued for the repeated invitation, and the sync loop keeps
// polling while the join is stalled.
func TestAutoJoinAcceptsInviteFromAllowlistedInviter(t *testing.T) {
	fake := &inviteServer{sender: "@alice:test", inviteOn: 1}
	srv := fake.start(t)

	cfg := testConfig(srv.URL)
	cfg.AutoJoinInvites = true
	cfg.InviteAllowlist = []string{"@alice:test"}
	c, err := New(cfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	stop := runSync(t, c)
	defer stop()

	waitFor(t, "the join request", func() bool { return fake.joinCount.Load() > 0 })
	if !strings.Contains(fake.joinPath.Load().(string), "!invited:test") {
		t.Fatalf("join path = %q, want it to target !invited:test", fake.joinPath.Load())
	}
	// The join is held open in the fake. Reaching the third sync request means
	// the loop processed the repeated invitation and polled again while that
	// join was still blocked, which a join made on the sync goroutine could
	// not do.
	waitFor(t, "the sync loop to poll again while the join is held open", func() bool { return fake.syncCount.Load() >= 3 })
	if n := fake.joinCount.Load(); n != 1 {
		t.Fatalf("issued %d join requests for one repeated invitation, want exactly 1", n)
	}

	// Letting the join finish must not release a second one either.
	close(fake.joinGate)
	time.Sleep(100 * time.Millisecond)
	if n := fake.joinCount.Load(); n != 1 {
		t.Fatalf("issued %d join requests after the join completed, want exactly 1", n)
	}
}

func TestAutoJoinRefusesUnlistedInviter(t *testing.T) {
	fake := &inviteServer{sender: "@mallory:test", inviteOn: 1}
	srv := fake.start(t)

	cfg := testConfig(srv.URL)
	cfg.AutoJoinInvites = true
	cfg.InviteAllowlist = []string{"@alice:test"}
	c, err := New(cfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	stop := runSync(t, c)
	defer stop()

	// The second /sync request only starts once the first response has been
	// fully processed, so reaching it means the invitation was already judged.
	waitFor(t, "the invitation to be processed", func() bool { return fake.syncCount.Load() >= 2 })
	time.Sleep(100 * time.Millisecond)
	if n := fake.joinCount.Load(); n != 0 {
		t.Fatalf("issued %d join requests for an unlisted inviter, want 0", n)
	}
}

func TestAutoJoinDisabledByDefaultLeavesInvitePending(t *testing.T) {
	fake := &inviteServer{sender: "@alice:test", inviteOn: 1}
	srv := fake.start(t)

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	stop := runSync(t, c)
	defer stop()

	waitFor(t, "the invitation to be processed", func() bool { return fake.syncCount.Load() >= 2 })
	time.Sleep(100 * time.Millisecond)
	if n := fake.joinCount.Load(); n != 0 {
		t.Fatalf("issued %d join requests with auto-join off, want 0", n)
	}
}

// TestAutoJoinAcceptsAnyoneWhenAllowlistIsEmpty covers the documented
// "empty allow-list accepts from anyone" rule.
func TestAutoJoinAcceptsAnyoneWhenAllowlistIsEmpty(t *testing.T) {
	fake := &inviteServer{sender: "@mallory:test", inviteOn: 1}
	srv := fake.start(t)

	cfg := testConfig(srv.URL)
	cfg.AutoJoinInvites = true
	c, err := New(cfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	stop := runSync(t, c)
	defer stop()

	waitFor(t, "the join request", func() bool { return fake.joinCount.Load() > 0 })
}

// TestAutoJoinAcceptsInviteFromIncrementalSync is the everyday flow: the bot is
// already running when the invitation arrives, so it shows up in an incremental
// /sync response rather than the first one.
func TestAutoJoinAcceptsInviteFromIncrementalSync(t *testing.T) {
	fake := &inviteServer{sender: "@alice:test", inviteOn: 2}
	srv := fake.start(t)

	cfg := testConfig(srv.URL)
	cfg.AutoJoinInvites = true
	cfg.InviteAllowlist = []string{"@Alice:test"} // compared case-insensitively
	c, err := New(cfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	stop := runSync(t, c)
	defer stop()

	waitFor(t, "the join request after an incremental invite", func() bool { return fake.joinCount.Load() > 0 })
	// The invitation repeats in the response after the one that carried it and
	// the join is held open, so a second join would show up here.
	waitFor(t, "the repeated invitation to be processed", func() bool { return fake.syncCount.Load() >= 4 })
	if n := fake.joinCount.Load(); n != 1 {
		t.Fatalf("issued %d join requests for the incremental invitation, want exactly 1", n)
	}
}

// TestAutoJoinHeldOpenJoinKeepsSyncingAndCloses is the non-blocking half of the
// auto-join contract: a /join the homeserver never answers must not stall the
// sync loop, and Close must still return while the join is in flight.
func TestAutoJoinHeldOpenJoinKeepsSyncingAndCloses(t *testing.T) {
	fake := &inviteServer{sender: "@alice:test", inviteOn: 1}
	srv := fake.start(t)

	cfg := testConfig(srv.URL)
	cfg.AutoJoinInvites = true
	c, err := New(cfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	syncDone := make(chan error, 1)
	go func() { syncDone <- c.Sync(ctx) }()
	defer cancel()

	waitFor(t, "the join request", func() bool { return fake.joinCount.Load() > 0 })
	// The join is blocked in the fake: a join made on the sync goroutine would
	// stop here, so a second and third sync response can only happen when the
	// join runs off it.
	waitFor(t, "the sync loop to keep polling while the join is held open", func() bool { return fake.syncCount.Load() >= 3 })

	closed := make(chan error, 1)
	go func() { closed <- c.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return while a join request was in flight")
	}

	// Close cancels the join; the sync loop's own exit is the caller's context,
	// exactly as in the shipped binary's shutdown order.
	cancel()
	select {
	case err := <-syncDone:
		if err != nil {
			t.Fatalf("Sync returned %v, want nil after cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Sync did not return after its context was cancelled")
	}
	if n := fake.joinCount.Load(); n != 1 {
		t.Fatalf("issued %d join requests, want exactly 1", n)
	}
}

func TestSendTextAllowsRoomFromAllowlistFile(t *testing.T) {
	var sent atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/state/m.room.encryption"):
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"errcode":"M_NOT_FOUND","error":"Not found"}`)
		case strings.Contains(r.URL.Path, "/send/m.room.message/"):
			sent.Add(1)
			io.WriteString(w, `{"event_id":"$file1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "allow.txt")
	if err := os.WriteFile(path, []byte("# enrolled from the CLI\n!file-only:test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(srv.URL)
	cfg.AllowlistFile = path

	c, err := New(cfg, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := c.SendText(context.Background(), "!file-only:test", "hello", "")
	if err != nil {
		t.Fatalf("SendText to a file-allow-listed room: %v", err)
	}
	if eventID != "$file1" || sent.Load() != 1 {
		t.Fatalf("SendText = %q with %d sends, want $file1 and one send", eventID, sent.Load())
	}
}

func TestWhoAmIReportsTokenOwner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_matrix/client/v3/account/whoami" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"user_id":"@bot:test","device_id":"DEVICE"}`)
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	userID, deviceID, err := c.WhoAmI(context.Background())
	if err != nil {
		t.Fatalf("WhoAmI: %v", err)
	}
	if userID != "@bot:test" || deviceID != "DEVICE" {
		t.Fatalf("WhoAmI = %q/%q, want @bot:test/DEVICE", userID, deviceID)
	}
}

// TestMessagesPaginationRoundTrip is the whole feature in one test: a call
// without a token starts at the newest message, its next_token reads the page
// behind it, and the last page reports that there is nothing older.
func TestMessagesPaginationRoundTrip(t *testing.T) {
	srv, requests := messagesServer(t, func(n int) (string, string) {
		switch n {
		case 0:
			return textEvent("$m3", 300, "three") + "," + textEvent("$m4", 400, "four"), "page2"
		case 1:
			return textEvent("$m1", 100, "one") + "," + textEvent("$m2", 200, "two"), ""
		default:
			t.Errorf("unexpected /messages request %d", n+1)
			return "", ""
		}
	})
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	first, err := c.Messages(ctx, "!r:test", 2, "")
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if ids := eventIDs(first.Messages); strings.Join(ids, ",") != "$m3,$m4" {
		t.Fatalf("first page = %v, want the newest two messages [$m3 $m4]", ids)
	}
	if !first.HasMore {
		t.Fatal("first page has_more = false, want true while the token exists")
	}
	if first.NextToken == "" {
		t.Fatal("first page returned no next_token, want one for older history")
	}

	second, err := c.Messages(ctx, "!r:test", 2, first.NextToken)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if ids := eventIDs(second.Messages); strings.Join(ids, ",") != "$m1,$m2" {
		t.Fatalf("second page = %v, want the older two messages [$m1 $m2]", ids)
	}
	if second.HasMore || second.NextToken != "" {
		t.Fatalf("last page = has_more %t, next_token %q; want the end of history",
			second.HasMore, second.NextToken)
	}

	reqs := requests()
	if len(reqs) != 2 {
		t.Fatalf("/messages requests = %d (%+v), want 2", len(reqs), reqs)
	}
	if reqs[0].from != "" {
		t.Fatalf("first request from = %q, want the newest page (empty from)", reqs[0].from)
	}
	if reqs[1].from != "page2" {
		t.Fatalf("second request from = %q, want the token's homeserver cursor page2", reqs[1].from)
	}
}

// TestMessagesLastPageHasNoToken covers the boundary case where the newest page
// is also the oldest: exactly limit messages and no end token must not promise
// a page that does not exist.
func TestMessagesLastPageHasNoToken(t *testing.T) {
	page := textEvent("$m1", 100, "one") + "," + textEvent("$m2", 200, "two")
	srv, _ := messagesServer(t, func(int) (string, string) { return page, "" })
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Messages(context.Background(), "!r:test", 2, "")
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("Messages returned %d messages, want 2", len(got.Messages))
	}
	if got.HasMore || got.NextToken != "" {
		t.Fatalf("has_more = %t, next_token = %q; want none when the room ends at this page",
			got.HasMore, got.NextToken)
	}
}

// TestMessagesRejectsBadPageTokens proves a bad token is a typed error rather
// than a silent empty page, and that it never reaches the homeserver: a token
// is unvalidated input on the next call.
func TestMessagesRejectsBadPageTokens(t *testing.T) {
	srv, requests := messagesServer(t, func(int) (string, string) { return "", "" })
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}

	valid, err := c.pages.sign("!r:test", "cursor1")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	otherRoom, err := c.pages.sign("!other:test", "cursor1")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	// A token from a previous process: same shape, different key.
	restarted, err := newPageSigner()
	if err != nil {
		t.Fatalf("newPageSigner: %v", err)
	}
	fromRestart, err := restarted.sign("!r:test", "cursor1")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	body, sig, _ := strings.Cut(valid, ".")
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		t.Fatalf("decode signed body: %v", err)
	}
	raw[len(raw)-2] ^= 0x01
	tampered := base64.RawURLEncoding.EncodeToString(raw) + "." + sig

	cases := []struct {
		name  string
		token string
	}{
		{"garbage", "not-a-token"},
		{"base64 body with no signature", body},
		{"bad signature encoding", body + ".!!!"},
		{"other room", otherRoom},
		{"tampered payload", tampered},
		{"issued by a previous process", fromRestart},
		{"oversized", strings.Repeat("a", pageTokenMaxLen+1)},
		{"empty payload", "." + sig},
	}
	for _, tc := range cases {
		_, err := c.Messages(context.Background(), "!r:test", 5, tc.token)
		if !errors.Is(err, ErrInvalidPageToken) {
			t.Fatalf("%s: Messages = %v, want ErrInvalidPageToken", tc.name, err)
		}
	}
	if n := len(requests()); n != 0 {
		t.Fatalf("%d /messages requests were issued for invalid tokens, want 0", n)
	}

	// The same token is fine for the room it was issued for, so the refusals
	// above are about validity and not about rejecting tokens in general.
	if _, err := c.Messages(context.Background(), "!r:test", 5, valid); err != nil {
		t.Fatalf("Messages with a valid token = %v, want success", err)
	}
}

// TestMessagesStaleTokenIsTyped covers the token this process issued but the
// homeserver will not honour: the caller gets a specific error it can act on,
// while an ordinary /messages failure stays a plain error.
func TestMessagesStaleTokenIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/messages") {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("from") != "" {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"errcode":"M_INVALID_PARAM","error":"Invalid from token"}`)
			return
		}
		io.WriteString(w, `{"chunk":[`+textEvent("$m1", 100, "one")+`],"end":"gone"}`)
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.pages.sign("!r:test", "gone")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := c.Messages(context.Background(), "!r:test", 5, token); !errors.Is(err, ErrStalePageToken) {
		t.Fatalf("Messages with a rejected token = %v, want ErrStalePageToken", err)
	}
	if _, err := c.Messages(context.Background(), "!r:test", 5, token); errors.Is(err, ErrInvalidPageToken) {
		t.Fatal("a stale token was reported as invalid; the two failures need different fixes")
	}
}

// TestMessagesBoundsApplyFromAToken proves a token cannot be used to make one
// call unbounded: the page cap and the raw-event budget still apply.
func TestMessagesBoundsApplyFromAToken(t *testing.T) {
	srv, requests := messagesServer(t, func(n int) (string, string) {
		return memberEvent("$j"+itoa(int64(n)), "@alice:test"), "next" + itoa(int64(n))
	})
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.pages.sign("!r:test", "start")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	got, err := c.Messages(context.Background(), "!r:test", 2, token)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if len(got.Messages) != 0 {
		t.Fatalf("Messages = %+v, want none (only member events)", got.Messages)
	}
	reqs := requests()
	if len(reqs) != maxMessagePages {
		t.Fatalf("/messages requests = %d, want %d (page cap still applies)", len(reqs), maxMessagePages)
	}
	if reqs[0].from != "start" {
		t.Fatalf("first request from = %q, want the token's cursor", reqs[0].from)
	}
	if !got.HasMore || got.NextToken == "" {
		t.Fatalf("bounded page = has_more %t, token %q; want a token to continue from", got.HasMore, got.NextToken)
	}
}

// TestPageTokensAreOpaqueAndLoggable pins the token's outward properties: a
// log-safe alphabet, a bounded length, and no readable room ID or cursor, so a
// token can be logged and paste without leaking where it points.
func TestPageTokensAreOpaqueAndLoggable(t *testing.T) {
	c, err := New(testConfig("http://127.0.0.1:1"), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	token, err := c.pages.sign("!secret-room:test", "t123-456")
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if len(token) > 200 {
		t.Fatalf("token length = %d, want a short token", len(token))
	}
	for _, r := range token {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			t.Fatalf("token %q contains %q, want only base64url characters and '.'", token, r)
		}
	}
	for _, leaked := range []string{"secret-room", "!secret", "t123-456"} {
		if strings.Contains(token, leaked) {
			t.Fatalf("token %q carries %q in clear", token, leaked)
		}
	}
}

// TestMessagesRelations covers what a transcript needs to be read as a
// conversation: both reply shapes, an edit, a mention list, and a redacted
// message that keeps its place with no text.
func TestMessagesRelations(t *testing.T) {
	page := strings.Join([]string{
		// The shape mautrix-whatsapp sends for a reply: no rel_type, only
		// m.in_reply_to. "$parent" is deliberately not in this page.
		`{"type":"m.room.message","event_id":"$reply","sender":"@wa:test","origin_server_ts":200,` +
			`"content":{"msgtype":"m.text","body":"answering","m.relates_to":{"m.in_reply_to":{"event_id":"$parent"}}}}`,
		// The flat shape other clients send for a reply.
		`{"type":"m.room.message","event_id":"$flat","sender":"@a:test","origin_server_ts":201,` +
			`"content":{"msgtype":"m.text","body":"flat reply","m.relates_to":{"event_id":"$parent2"}}}`,
		// An edit: rel_type m.replace, with the in_reply_to some clients add.
		`{"type":"m.room.message","event_id":"$edit","sender":"@a:test","origin_server_ts":202,` +
			`"content":{"msgtype":"m.text","body":"* corrected","m.relates_to":{"rel_type":"m.replace","event_id":"$orig","m.in_reply_to":{"event_id":"$orig"}}}}`,
		// A mention list.
		`{"type":"m.room.message","event_id":"$mention","sender":"@a:test","origin_server_ts":203,` +
			`"content":{"msgtype":"m.text","body":"hey you","m.mentions":{"user_ids":["@bot:test","@b:test"]}}}`,
		// A redacted message: content stripped, redaction recorded in unsigned.
		`{"type":"m.room.message","event_id":"$gone","sender":"@a:test","origin_server_ts":204,"content":{},` +
			`"unsigned":{"redacted_because":{"type":"m.room.redaction","event_id":"$red","sender":"@a:test","origin_server_ts":205,"content":{}}}}`,
		textEvent("$plain", 206, "nothing special"),
	}, ",")
	srv, _ := messagesServer(t, func(int) (string, string) { return page, "" })
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Messages(context.Background(), "!r:test", 10, "")
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	byID := make(map[string]Message, len(got.Messages))
	for _, m := range got.Messages {
		byID[m.EventID] = m
	}
	if len(byID) != 6 {
		t.Fatalf("Messages returned %d messages, want 6: %+v", len(byID), got.Messages)
	}

	if m := byID["$reply"]; m.ReplyTo != "$parent" || m.EditOf != "" || m.Body != "answering" {
		t.Fatalf("$reply = %+v, want reply_to $parent and its body", m)
	}
	if m := byID["$flat"]; m.ReplyTo != "$parent2" {
		t.Fatalf("$flat = %+v, want reply_to $parent2 from the flat shape", m)
	}
	if m := byID["$edit"]; m.EditOf != "$orig" || m.ReplyTo != "" {
		t.Fatalf("$edit = %+v, want edit_of $orig and no reply_to (an edit is not a reply)", m)
	}
	if m := byID["$edit"]; m.Body != "* corrected" {
		t.Fatalf("$edit body = %q, want the edit kept in the stream for the client to apply", m.Body)
	}
	if m := byID["$mention"]; strings.Join(m.Mentions, ",") != "@bot:test,@b:test" {
		t.Fatalf("$mention = %+v, want both mentioned user IDs", m)
	}
	if m := byID["$gone"]; !m.Redacted || m.Body != "" || m.MsgType != "" {
		t.Fatalf("$gone = %+v, want redacted true with no body and no type", m)
	}
	if m := byID["$plain"]; m.Redacted || m.ReplyTo != "" || m.EditOf != "" || len(m.Mentions) != 0 {
		t.Fatalf("$plain = %+v, want no relations", m)
	}
}

// TestSendTextReplyCarriesInReplyTo pins the request body for a threaded send:
// the bridge renders a quote from m.relates_to.m.in_reply_to, not from the flat
// relation.
func TestSendTextReplyCarriesInReplyTo(t *testing.T) {
	var mu sync.Mutex
	var gotBody map[string]any
	var sends int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/state/m.room.encryption"):
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"errcode":"M_NOT_FOUND","error":"Not found"}`)
		case strings.Contains(r.URL.Path, "/send/m.room.message/"):
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			sends++
			mu.Unlock()
			var decoded map[string]any
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Errorf("send body is not JSON: %v", err)
			}
			mu.Lock()
			gotBody = decoded
			mu.Unlock()
			io.WriteString(w, `{"event_id":"$sent"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL, "!ok:test"), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := c.SendText(context.Background(), "!ok:test", "hello", "$parent")
	if err != nil {
		t.Fatalf("SendText reply: %v", err)
	}
	if eventID != "$sent" {
		t.Fatalf("SendText = %q, want $sent", eventID)
	}

	mu.Lock()
	defer mu.Unlock()
	if sends != 1 {
		t.Fatalf("issued %d sends, want 1", sends)
	}
	if gotBody["msgtype"] != "m.text" || gotBody["body"] != "hello" {
		t.Fatalf("send body = %+v, want m.text/hello", gotBody)
	}
	rel, ok := gotBody["m.relates_to"].(map[string]any)
	if !ok {
		t.Fatalf("send body = %+v, want an m.relates_to relation", gotBody)
	}
	if rel["event_id"] != nil {
		t.Fatalf("m.relates_to = %+v, want the reply shape and no flat event_id", rel)
	}
	reply, ok := rel["m.in_reply_to"].(map[string]any)
	if !ok || reply["event_id"] != "$parent" {
		t.Fatalf("m.relates_to = %+v, want m.in_reply_to.event_id $parent", rel)
	}
}

// TestSendTextReplyKeepsBridgeAckGuard proves the reply path does not bypass
// the guard: Matrix accepting a threaded message still does not mean the bridge
// forwarded it.
func TestSendTextReplyKeepsBridgeAckGuard(t *testing.T) {
	fake := &bridgeAckServer{}
	fake.setMessages(`{"type":"m.room.message","event_id":"$n1","sender":"@wa:test","origin_server_ts":2,` +
		`"content":{"msgtype":"m.notice","body":"not bridged","m.relates_to":{"m.in_reply_to":{"event_id":"$send1"}}}}`)
	srv := fake.start(t)

	c, err := New(testConfigWithAck(srv.URL, 250*time.Millisecond, "!ok:test"), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	eventID, err := c.SendText(context.Background(), "!ok:test", "hello", "$parent")
	if !errors.Is(err, ErrNotBridged) {
		t.Fatalf("SendText reply = %q, %v; want ErrNotBridged to still apply", eventID, err)
	}
}

// TestSendTextRejectsMalformedReplyBeforeSending covers the local validation:
// a reply target that is not an event ID must not spend a request.
func TestSendTextRejectsMalformedReplyBeforeSending(t *testing.T) {
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
	for _, bad := range []string{"not-an-event", "abc", "$", "$a b", "$a\tb", "$" + strings.Repeat("x", 255)} {
		if _, err := c.SendText(context.Background(), "!ok:test", "hi", bad); !errors.Is(err, ErrInvalidEventID) {
			t.Fatalf("SendText(reply_to=%q) = %v, want ErrInvalidEventID", bad, err)
		}
	}
	if hit.Load() {
		t.Fatal("a malformed reply target reached the homeserver")
	}
}

func TestValidEventID(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"$abc123", true},
		{"$yXXiCBRYgaKwxnPTokViHJLeaZ8gOhnk3iQIcYBX8i0", true},
		{"$143273582443PhrSn:example.org", true}, // v1 event IDs carry a server part
		{"$" + strings.Repeat("x", 254), true},   // 255 bytes inclusive
		{"", false},
		{"$", false},
		{"abc", false},
		{"!room:test", false},
		{"$a b", false},
		{"$a\nb", false},
		{"$" + strings.Repeat("x", 255), false},
	} {
		if got := ValidEventID(tc.id); got != tc.want {
			t.Fatalf("ValidEventID(%q) = %t, want %t", tc.id, got, tc.want)
		}
	}
}

// eventIDs is the event ID of every message, in order.
func eventIDs(msgs []Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.EventID)
	}
	return out
}

// TestIsFatal covers the sync loop's stop condition. A revoked or missing token
// and a token that belongs to another user stop the loop for good; everything
// else — a network blip, a rate limit, a plain cancellation — is retried.
func TestIsFatal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"M_UNKNOWN_TOKEN", mautrix.MUnknownToken, true},
		{"M_UNKNOWN_TOKEN wrapped", fmt.Errorf("matrix: whoami: %w", mautrix.MUnknownToken), true},
		{"M_MISSING_TOKEN", mautrix.MMissingToken, true},
		{"M_MISSING_TOKEN wrapped", fmt.Errorf("matrix: whoami: %w", mautrix.MMissingToken), true},
		{"ErrUserMismatch", ErrUserMismatch, true},
		{"ErrUserMismatch wrapped", fmt.Errorf("matrix: whoami: %w", ErrUserMismatch), true},
		{"M_FORBIDDEN", mautrix.MForbidden, false},
		{"M_LIMIT_EXCEEDED", mautrix.MLimitExceeded, false},
		{"network error", errors.New("dial tcp: connection refused"), false},
		{"cancelled", context.Canceled, false},
		{"nil", nil, false},
	} {
		if got := isFatal(tc.err); got != tc.want {
			t.Fatalf("%s: isFatal(%v) = %t, want %t", tc.name, tc.err, got, tc.want)
		}
	}
}
