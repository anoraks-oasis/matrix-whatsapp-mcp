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
	"unicode"

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

	if _, err := c.Messages(context.Background(), domainlessRoomID, 10); err != nil {
		t.Fatalf("Messages(%q) = %v, want success", domainlessRoomID, err)
	}
	if _, err := c.SendText(context.Background(), domainlessRoomID, "hi"); err != nil {
		t.Fatalf("SendText(%q) = %v, want success", domainlessRoomID, err)
	}

	// The 255-byte cap is inclusive.
	boundary := "!" + strings.Repeat("x", 254)
	if _, err := c.Messages(context.Background(), boundary, 10); err != nil {
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
		if _, err := c.Messages(context.Background(), roomID, 10); !errors.Is(err, ErrInvalidRoomID) {
			t.Errorf("Messages(%q) = %v, want ErrInvalidRoomID", roomID, err)
		}
		if _, err := c.SendText(context.Background(), roomID, "hi"); !errors.Is(err, ErrInvalidRoomID) {
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
	msgs, err := c.Messages(context.Background(), "!r:test", 4)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
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
		msgs, err := c.Messages(context.Background(), "!r:test", 2)
		if err != nil {
			t.Fatalf("Messages: %v", err)
		}
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
		if _, err := c.Messages(context.Background(), "!r:test", 2); err != nil {
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
	msgs, err := c.Messages(context.Background(), "!r:test", 10)
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
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
			b.mu.Unlock()
			io.WriteString(w, `{"event_id":"$send1"}`)
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
	eventID, err := c.SendText(context.Background(), "!ok:test", "hello world")
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
	eventID, err := c.SendText(context.Background(), "!ok:test", "hello world")
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
	if _, err := c.SendText(context.Background(), "!ok:test", "hello world"); err != nil {
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
