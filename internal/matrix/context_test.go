package matrix

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// replyEvent is a text message that answers another event.
func replyEvent(id string, ts int64, body, parent string) string {
	return `{"type":"m.room.message","event_id":"` + id + `","sender":"@bob:test","origin_server_ts":` + itoa(ts) +
		`,"content":{"msgtype":"m.text","body":"` + body + `","m.relates_to":{"m.in_reply_to":{"event_id":"` + parent + `"}}}}`
}

// contextServer serves one scripted /context response and records what it was
// asked for.
type contextServer struct {
	limit    string
	requests atomic.Int64
}

func (cs *contextServer) start(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/context/") {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		cs.requests.Add(1)
		cs.limit = r.URL.Query().Get("limit")
		if status != 0 {
			w.WriteHeader(status)
		}
		io.WriteString(w, body)
	}))
	return srv
}

func TestMessageContextUsesEndpoint(t *testing.T) {
	cs := &contextServer{}
	body := `{"start":"s","end":"e",` +
		`"event":` + replyEvent("$t", 1000, "the target", "$parent") + `,` +
		`"events_before":[` + textEvent("$b2", 900, "before two") + `,` + textEvent("$b1", 800, "before one") + `],` +
		`"events_after":[` + textEvent("$a1", 1100, "after one") + `,` + textEvent("$a2", 1200, "after two") + `]}`
	var eventFetches atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/context/"):
			cs.requests.Add(1)
			cs.limit = r.URL.Query().Get("limit")
			io.WriteString(w, body)
		case strings.Contains(r.URL.Path, "/event/"):
			eventFetches.Add(1)
			io.WriteString(w, textEvent("$parent", 700, "the parent"))
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
	out, err := c.MessageContext(context.Background(), "!r:test", "$t", 10, 10)
	if err != nil {
		t.Fatalf("MessageContext: %v", err)
	}
	if out.Source != ContextSourceEndpoint {
		t.Fatalf("source = %q, want %q", out.Source, ContextSourceEndpoint)
	}
	if out.Truncated {
		t.Fatal("truncated = true on the /context path, want false")
	}
	if cs.limit != "20" {
		t.Fatalf("limit query = %q, want 20 (before + after)", cs.limit)
	}
	if out.Event.EventID != "$t" || out.Event.Body != "the target" || out.Event.RoomID != "!r:test" {
		t.Fatalf("event = %+v, want the target message with its room", out.Event)
	}
	var before, after []string
	for _, m := range out.Before {
		before = append(before, m.EventID)
	}
	for _, m := range out.After {
		after = append(after, m.EventID)
	}
	if strings.Join(before, ",") != "$b1,$b2" {
		t.Fatalf("events_before = %v, want [$b1 $b2] oldest first", before)
	}
	if strings.Join(after, ",") != "$a1,$a2" {
		t.Fatalf("events_after = %v, want [$a1 $a2] oldest first", after)
	}
	if out.ReplyTo == nil || out.ReplyTo.EventID != "$parent" || out.ReplyTo.Body != "the parent" {
		t.Fatalf("reply_to = %+v, want the parent resolved through /event", out.ReplyTo)
	}
	if eventFetches.Load() != 1 {
		t.Fatalf("/event requests = %d, want 1", eventFetches.Load())
	}
	if out.EditOf != nil {
		t.Fatalf("edit_of = %+v, want nil for a message that is not an edit", out.EditOf)
	}
}

// TestMessageContextEditResolvedFromWindow proves the reply/edit resolution
// prefers the messages the context already read over a second request.
func TestMessageContextEditResolvedFromWindow(t *testing.T) {
	edit := `{"type":"m.room.message","event_id":"$edit","sender":"@bob:test","origin_server_ts":1000,` +
		`"content":{"msgtype":"m.text","body":"* fixed","m.relates_to":{"rel_type":"m.replace","event_id":"$orig"}}}`
	body := `{"start":"s","end":"e","event":` + edit + `,` +
		`"events_before":[` + textEvent("$orig", 900, "the original") + `],"events_after":[]}`
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if strings.Contains(r.URL.Path, "/event/") {
			t.Errorf("resolving an edit that is already in the window made a request")
		}
		io.WriteString(w, body)
	}))
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	out, err := c.MessageContext(context.Background(), "!r:test", "$edit", 5, 5)
	if err != nil {
		t.Fatalf("MessageContext: %v", err)
	}
	if out.Event.EditOf != "$orig" {
		t.Fatalf("event.edit_of = %q, want $orig", out.Event.EditOf)
	}
	if out.EditOf == nil || out.EditOf.EventID != "$orig" || out.EditOf.Body != "the original" {
		t.Fatalf("edit_of = %+v, want the replaced message resolved from the window", out.EditOf)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1 (one /context call, no /event)", requests.Load())
	}
}

// TestMessageContextClampsLimits proves the 0..50 clamp, that an explicit zero
// is respected, and that only the requested side is trimmed.
func TestMessageContextClampsLimits(t *testing.T) {
	cs := &contextServer{}
	var beforeEvents []string
	for i := 0; i < 60; i++ {
		beforeEvents = append(beforeEvents, textEvent("$b"+itoa(int64(i)), int64(100+i), "old "+itoa(int64(i))))
	}
	body := `{"start":"s","end":"e","event":` + textEvent("$t", 1000, "target") + `,` +
		`"events_before":[` + strings.Join(beforeEvents, ",") + `],"events_after":[` + textEvent("$a", 1100, "after") + `]}`
	srv := cs.start(t, 0, body)
	defer srv.Close()

	c, err := New(testConfig(srv.URL), testLogger())
	if err != nil {
		t.Fatal(err)
	}

	out, err := c.MessageContext(context.Background(), "!r:test", "$t", 100, -3)
	if err != nil {
		t.Fatalf("MessageContext: %v", err)
	}
	if cs.limit != "50" {
		t.Fatalf("limit query = %q, want 50 (100 clamped, -3 clamped to 0)", cs.limit)
	}
	if len(out.Before) != 50 {
		t.Fatalf("events_before = %d, want the 50 closest messages", len(out.Before))
	}
	if len(out.After) != 0 {
		t.Fatalf("events_after = %d, want 0 for an explicit limit_after of -3", len(out.After))
	}
	// The messages kept must be the ones closest to the target, i.e. the newest.
	if out.Before[len(out.Before)-1].Timestamp != 159 {
		t.Fatalf("last kept message ts = %d, want 159 (the closest before the target)", out.Before[len(out.Before)-1].Timestamp)
	}
}

func TestMessageContextErrors(t *testing.T) {
	t.Run("unknown event", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"errcode":"M_NOT_FOUND","error":"Event not found"}`)
		}))
		defer srv.Close()

		c, err := New(testConfig(srv.URL), testLogger())
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.MessageContext(context.Background(), "!r:test", "$missing", 5, 5)
		if !errors.Is(err, ErrEventNotFound) {
			t.Fatalf("error = %v, want ErrEventNotFound (not an empty context)", err)
		}
	})

	t.Run("not a message", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"start":"s","end":"e","event":`+memberEvent("$m", "@alice:test")+`,"events_before":[],"events_after":[]}`)
		}))
		defer srv.Close()

		c, err := New(testConfig(srv.URL), testLogger())
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.MessageContext(context.Background(), "!r:test", "$m", 5, 5)
		if !errors.Is(err, ErrNotAMessage) {
			t.Fatalf("error = %v, want ErrNotAMessage", err)
		}
	})

	t.Run("malformed ids never reach the server", func(t *testing.T) {
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
		if _, err := c.MessageContext(context.Background(), "!r:test", "not-an-event", 5, 5); !errors.Is(err, ErrInvalidEventID) {
			t.Fatalf("bad event ID error = %v, want ErrInvalidEventID", err)
		}
		if _, err := c.MessageContext(context.Background(), "not-a-room", "$e", 5, 5); !errors.Is(err, ErrInvalidRoomID) {
			t.Fatalf("bad room ID error = %v, want ErrInvalidRoomID", err)
		}
		if requests.Load() != 0 {
			t.Fatalf("a rejected context call made %d requests, want 0", requests.Load())
		}
	})
}

// TestMessageContextFallsBackWithoutEndpoint proves the M_UNRECOGNIZED path:
// the event is fetched directly, its neighbourhood comes from /messages, and
// the refusal is remembered.
func TestMessageContextFallsBackWithoutEndpoint(t *testing.T) {
	var contextCalls, eventCalls, messagePages atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/context/"):
			contextCalls.Add(1)
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"errcode":"M_UNRECOGNIZED","error":"Unrecognized request"}`)
		case strings.Contains(r.URL.Path, "/event/"):
			eventCalls.Add(1)
			io.WriteString(w, textEvent("$t", 1000, "the target"))
		case strings.Contains(r.URL.Path, "/messages"):
			messagePages.Add(1)
			io.WriteString(w, `{"chunk":[`+
				textEvent("$a2", 1200, "after two")+`,`+
				textEvent("$a1", 1100, "after one")+`,`+
				textEvent("$t", 1000, "the target")+`,`+
				textEvent("$b2", 900, "before two")+`,`+
				textEvent("$b1", 800, "before one")+`],"end":""}`)
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
	for attempt := 0; attempt < 2; attempt++ {
		out, err := c.MessageContext(context.Background(), "!r:test", "$t", 2, 2)
		if err != nil {
			t.Fatalf("MessageContext: %v", err)
		}
		if out.Source != ContextSourceScan {
			t.Fatalf("source = %q, want %q", out.Source, ContextSourceScan)
		}
		if out.Truncated {
			t.Fatal("truncated = true although the whole window was covered")
		}
		var before, after []string
		for _, m := range out.Before {
			before = append(before, m.EventID)
		}
		for _, m := range out.After {
			after = append(after, m.EventID)
		}
		if strings.Join(before, ",") != "$b1,$b2" {
			t.Fatalf("events_before = %v, want [$b1 $b2] oldest first", before)
		}
		if strings.Join(after, ",") != "$a1,$a2" {
			t.Fatalf("events_after = %v, want [$a1 $a2] oldest first", after)
		}
		if out.Event.EventID != "$t" {
			t.Fatalf("event = %+v, want $t", out.Event)
		}
	}
	if contextCalls.Load() != 1 {
		t.Fatalf("/context requests = %d, want 1 (the refusal is cached per process)", contextCalls.Load())
	}
	if eventCalls.Load() != 2 {
		t.Fatalf("/event requests = %d, want 2 (one per call)", eventCalls.Load())
	}
	if messagePages.Load() != 2 {
		t.Fatalf("/messages requests = %d, want 2 (one per call)", messagePages.Load())
	}
}

// TestMessageContextFallbackIsBounded proves the fallback cannot walk a room
// forever: when the event is older than the budget reaches, the answer says it
// is incomplete instead of pretending the event has no neighbourhood.
func TestMessageContextFallbackIsBounded(t *testing.T) {
	var pages atomic.Int64
	var chunk []string
	for i := 0; i < ContextScanPageLimit; i++ {
		chunk = append(chunk, textEvent("$n"+itoa(int64(i)), int64(2000-i), "newer"))
	}
	page := `{"chunk":[` + strings.Join(chunk, ",") + `],"end":"next"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/context/"):
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"errcode":"M_UNRECOGNIZED"}`)
		case strings.Contains(r.URL.Path, "/event/"):
			io.WriteString(w, textEvent("$old", 100, "very old target"))
		case strings.Contains(r.URL.Path, "/messages"):
			pages.Add(1)
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
	out, err := c.MessageContext(context.Background(), "!r:test", "$old", 2, 2)
	if err != nil {
		t.Fatalf("MessageContext: %v", err)
	}
	if !out.Truncated {
		t.Fatal("truncated = false after the scan hit its budget, want true")
	}
	if len(out.Before) != 0 || len(out.After) != 0 {
		t.Fatalf("context = %d before / %d after, want none: the budget ended before the event", len(out.Before), len(out.After))
	}
	if out.Event.EventID != "$old" {
		t.Fatalf("event = %+v, want the fetched target even without a neighbourhood", out.Event)
	}
	if got := pages.Load(); got != ContextScanPages {
		t.Fatalf("/messages pages = %d, want %d (the scan budget)", got, ContextScanPages)
	}
}

func TestMessageContextHonoursCancellation(t *testing.T) {
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

	_, err = c.MessageContext(ctx, "!r:test", "$t", 5, 5)
	if err == nil {
		t.Fatal("MessageContext returned no error after cancellation")
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("MessageContext error = %v, want the context error", err)
	}
}
