package trigger

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func msgEvent(evID, roomID, sender string, ts int64, body string) *event.Event {
	return &event.Event{
		Type:      event.EventMessage,
		ID:        id.EventID(evID),
		RoomID:    id.RoomID(roomID),
		Sender:    id.UserID(sender),
		Timestamp: ts,
		Content:   event.Content{Parsed: &event.MessageEventContent{MsgType: event.MsgText, Body: body}},
	}
}

func TestRingBufferBeforeFill(t *testing.T) {
	d := New("", nil, discardLogger())
	for i := 1; i <= 5; i++ {
		d.Handle(context.Background(), msgEvent(fmt.Sprintf("e%d", i), "!r:test", "@alice:test", int64(i), "m"))
	}
	got := d.Recent(0, 0)
	if len(got) != 5 {
		t.Fatalf("Recent before fill = %d triggers, want 5", len(got))
	}
	for i := 0; i < 5; i++ {
		want := int64(5 - i)
		if got[i].Timestamp != want {
			t.Fatalf("Recent[%d].Timestamp = %d, want %d (newest first)", i, got[i].Timestamp, want)
		}
	}
	if limited := d.Recent(0, 3); len(limited) != 3 || limited[0].Timestamp != 5 {
		t.Fatalf("Recent(0,3) = %d items, first ts %d; want 3 items newest first", len(limited), limited[0].Timestamp)
	}
}

func TestRingBufferWrapsAtCapacity(t *testing.T) {
	d := New("", nil, discardLogger())
	for i := 1; i <= 150; i++ {
		d.Handle(context.Background(), msgEvent(fmt.Sprintf("e%03d", i), "!r:test", "@alice:test", int64(i), "m"))
	}
	got := d.Recent(0, 0)
	if len(got) != BufferSize {
		t.Fatalf("Recent after wrap = %d triggers, want %d", len(got), BufferSize)
	}
	if got[0].Timestamp != 150 || got[len(got)-1].Timestamp != 51 {
		t.Fatalf("newest=%d oldest=%d, want 150 and 51", got[0].Timestamp, got[len(got)-1].Timestamp)
	}
	// The 50 oldest (1..50) must have been evicted.
	seen := make(map[string]bool, len(got))
	for _, tr := range got {
		seen[tr.MessageID] = true
	}
	for i := 1; i <= 50; i++ {
		if seen[fmt.Sprintf("e%03d", i)] {
			t.Fatalf("evicted trigger e%03d still present after wrap", i)
		}
	}
	for i := 51; i <= 150; i++ {
		if !seen[fmt.Sprintf("e%03d", i)] {
			t.Fatalf("trigger e%03d missing after wrap", i)
		}
	}
	// Order must be strictly newest-first across the whole buffer.
	for i := 1; i < len(got); i++ {
		if got[i-1].Timestamp <= got[i].Timestamp {
			t.Fatalf("Recent not newest-first at %d: %d then %d", i, got[i-1].Timestamp, got[i].Timestamp)
		}
	}
}

func TestRecentSinceTimestampBoundary(t *testing.T) {
	d := New("", nil, discardLogger())
	d.Handle(context.Background(), msgEvent("e100", "!r:test", "@alice:test", 100, "old"))
	d.Handle(context.Background(), msgEvent("e101", "!r:test", "@alice:test", 101, "new"))

	got := d.Recent(100, 0)
	if len(got) != 1 || got[0].MessageID != "e101" {
		t.Fatalf("Recent(100) = %+v, want exactly [e101] (strictly newer than since)", got)
	}
	if both := d.Recent(99, 0); len(both) != 2 {
		t.Fatalf("Recent(99) = %d triggers, want 2", len(both))
	}
	if none := d.Recent(101, 0); len(none) != 0 {
		t.Fatalf("Recent(101) = %d triggers, want 0", len(none))
	}
	if none := d.Recent(101, 0); none == nil {
		t.Fatalf("Recent(101) returned nil, want empty non-nil slice")
	}
}

func TestHandleIgnoresNonMessageEvents(t *testing.T) {
	d := New("", nil, discardLogger())
	d.Handle(context.Background(), nil)
	d.Handle(context.Background(), &event.Event{Type: event.EventEncrypted, ID: "$x", RoomID: "!r:test"})
	if got := d.Recent(0, 0); len(got) != 0 {
		t.Fatalf("non-message events recorded %d triggers, want 0", len(got))
	}
}

// TestWebhookPayloadSpeaksWhatsApp pins the JSON a webhook consumer parses: the
// WhatsApp field names and no Matrix-era key, with the attachment fields
// present only when the message carries an attachment.
func TestWebhookPayloadSpeaksWhatsApp(t *testing.T) {
	tests := []struct {
		name      string
		content   *event.MessageEventContent
		wantKeys  []string
		wantType  string
		wantText  string
		wantMedia string
	}{
		{
			name:     "text",
			content:  &event.MessageEventContent{MsgType: event.MsgText, Body: "hello"},
			wantKeys: []string{"chat_id", "chat_name", "from", "message_id", "text", "timestamp", "type"},
			wantType: "text",
			wantText: "hello",
		},
		{
			name: "media",
			content: &event.MessageEventContent{
				MsgType: event.MsgImage, FileName: "dinner.jpg", URL: "mxc://example.com/abc",
				Info: &event.FileInfo{MimeType: "image/jpeg", Size: 2048},
			},
			wantKeys:  []string{"chat_id", "chat_name", "filename", "from", "media_uri", "message_id", "mimetype", "size_bytes", "text", "timestamp", "type"},
			wantType:  "image",
			wantText:  "[image]",
			wantMedia: "mxc://example.com/abc",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := make(chan map[string]any, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var decoded map[string]any
				if err := json.NewDecoder(r.Body).Decode(&decoded); err != nil {
					t.Errorf("decode webhook payload: %v", err)
				}
				got <- decoded
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()

			d := New(srv.URL, nil, discardLogger())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			d.Start(ctx)
			d.Handle(ctx, &event.Event{
				Type:      event.EventMessage,
				ID:        id.EventID("$evt1"),
				RoomID:    id.RoomID("!cafe:example.com"),
				Sender:    id.UserID("@wa_alice:example.com"),
				Timestamp: 1763600000000,
				Content:   event.Content{Parsed: tc.content},
			})

			var payload map[string]any
			select {
			case payload = <-got:
			case <-time.After(2 * time.Second):
				t.Fatal("webhook never received the trigger")
			}
			if err := d.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			keys := make([]string, 0, len(payload))
			for key := range payload {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			if strings.Join(keys, ",") != strings.Join(tc.wantKeys, ",") {
				t.Fatalf("payload keys = %v, want %v", keys, tc.wantKeys)
			}
			for _, old := range []string{"room_id", "event_id", "sender", "body", "msgtype"} {
				if _, ok := payload[old]; ok {
					t.Fatalf("payload still carries the Matrix-era key %q: %v", old, payload)
				}
			}
			if payload["message_id"] != "$evt1" {
				t.Fatalf("message_id = %v, want $evt1", payload["message_id"])
			}
			if payload["chat_id"] != "!cafe:example.com" {
				t.Fatalf("chat_id = %v, want the chat ID", payload["chat_id"])
			}
			if payload["chat_name"] != "!cafe:example.com" {
				t.Fatalf("chat_name = %v, want the chat ID when no namer is set", payload["chat_name"])
			}
			if payload["from"] != "@wa_alice:example.com" {
				t.Fatalf("from = %v, want the sender", payload["from"])
			}
			if payload["type"] != tc.wantType {
				t.Fatalf("type = %v, want %q", payload["type"], tc.wantType)
			}
			if payload["text"] != tc.wantText {
				t.Fatalf("text = %v, want %q", payload["text"], tc.wantText)
			}
			if payload["timestamp"] != float64(1763600000000) {
				t.Fatalf("timestamp = %v, want 1763600000000", payload["timestamp"])
			}
			if tc.wantMedia != "" {
				if payload["media_uri"] != tc.wantMedia {
					t.Fatalf("media_uri = %v, want %q", payload["media_uri"], tc.wantMedia)
				}
				if payload["mimetype"] != "image/jpeg" || payload["size_bytes"] != float64(2048) || payload["filename"] != "dinner.jpg" {
					t.Fatalf("attachment fields = %v, want mimetype/filename/size_bytes carried", payload)
				}
			}
		})
	}
}

func TestWebhookDispatchIsAsync(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var bodies atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodies.Add(1)
		close(entered)
		<-release
	}))
	defer srv.Close()

	d := New(srv.URL, nil, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)

	start := time.Now()
	d.Handle(ctx, msgEvent("$async", "!r:test", "@alice:test", 1, "hello"))
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Handle blocked for %s; dispatch must be async", elapsed)
	}

	// The worker must actually reach the webhook while the response is held open.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("webhook never received the trigger")
	}
	close(release)
	time.Sleep(50 * time.Millisecond) // let the worker drain the response
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if bodies.Load() != 1 {
		t.Fatalf("webhook received %d requests, want 1", bodies.Load())
	}
}

func TestWebhookExactlyOneRetry(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := New(srv.URL, nil, discardLogger())
	d.retryDelay = 10 * time.Millisecond
	d.retries = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	d.Handle(ctx, msgEvent("$retry", "!r:test", "@alice:test", 1, "hello"))

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("webhook received %d requests, want exactly 2 (initial + 1 retry)", got)
	}
	// No third attempt may follow the retry.
	time.Sleep(3 * d.retryDelay)
	if got := calls.Load(); got != 2 {
		t.Fatalf("webhook received %d requests after settling, want exactly 2", got)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestWebhookTimesOut(t *testing.T) {
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(300 * time.Millisecond)
	}))
	defer srv.Close()

	d := New(srv.URL, nil, discardLogger())
	d.timeout = 30 * time.Millisecond
	d.retryDelay = 10 * time.Millisecond
	d.retries = 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.Start(ctx)
	d.Handle(ctx, msgEvent("$slow", "!r:test", "@alice:test", 1, "hello"))

	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("webhook received %d requests, want 2 (both timed out)", got)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestConcurrentBufferAccess(t *testing.T) {
	d := New("", nil, discardLogger())
	const writers = 8
	const readers = 8
	const iters = 500
	var wg sync.WaitGroup
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				evID := fmt.Sprintf("e-%d-%d", g, i)
				d.Handle(context.Background(), msgEvent(evID, "!r:test", "@alice:test", int64(i), "m"))
			}
		}(g)
	}
	for g := 0; g < readers; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				_ = d.Recent(0, 50)
				_ = d.Recent(0, 0)
			}
		}()
	}
	wg.Wait()
	if got := d.Recent(0, 0); len(got) != BufferSize {
		t.Fatalf("Recent after concurrent access = %d, want %d", len(got), BufferSize)
	}
}

// TestNewDispatcherDefaults locks the documented production defaults. These
// are a public contract (5s webhook timeout, exactly one retry), so any drift
// here is a production bug, not a test bug.
func TestNewDispatcherDefaults(t *testing.T) {
	d := New("http://example.invalid/hook", nil, discardLogger())
	if d.timeout != 5*time.Second {
		t.Fatalf("default timeout = %v, want exactly 5s", d.timeout)
	}
	if d.retries != 1 {
		t.Fatalf("default retries = %d, want exactly 1", d.retries)
	}
	if d.retryDelay != 500*time.Millisecond {
		t.Fatalf("default retryDelay = %v, want exactly 500ms", d.retryDelay)
	}
}

// mediaEvent builds an m.room.message event of an arbitrary msgtype.
func mediaEvent(evID, roomID, sender string, ts int64, msgtype event.MessageType, body string) *event.Event {
	return &event.Event{
		Type:      event.EventMessage,
		ID:        id.EventID(evID),
		RoomID:    id.RoomID(roomID),
		Sender:    id.UserID(sender),
		Timestamp: ts,
		Content:   event.Content{Parsed: &event.MessageEventContent{MsgType: msgtype, Body: body}},
	}
}

// TestHandleMediaPlaceholderAndType closes the live media defect on the
// trigger path: a photo with an empty body must not reach a webhook consumer as
// a blank string, and the WhatsApp type must be carried through.
func TestHandleMediaPlaceholderAndType(t *testing.T) {
	tests := []struct {
		name     string
		msgtype  event.MessageType
		body     string
		wantBody string
		wantType string
	}{
		{"image with empty body", event.MsgImage, "", "[image]", "image"},
		{"video with empty body", event.MsgVideo, "", "[video]", "video"},
		{"audio with empty body", event.MsgAudio, "", "[audio]", "audio"},
		{"file with empty body", event.MsgFile, "", "[document]", "document"},
		{"image with caption", event.MsgImage, "a caption", "a caption", "image"},
		{"text keeps its body", event.MsgText, "hello there", "hello there", "text"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := New("", nil, discardLogger())
			d.Handle(context.Background(), mediaEvent("$e", "!r:test", "@wa:test", int64(i+1), tc.msgtype, tc.body))
			got := d.Recent(0, 0)
			if len(got) != 1 {
				t.Fatalf("Recent = %d triggers, want 1", len(got))
			}
			if got[0].Text != tc.wantBody {
				t.Fatalf("Text = %q, want %q", got[0].Text, tc.wantBody)
			}
			if got[0].Type != tc.wantType {
				t.Fatalf("Type = %q, want %q", got[0].Type, tc.wantType)
			}
			payload, err := json.Marshal(got[0])
			if err != nil {
				t.Fatalf("marshal trigger: %v", err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(payload, &decoded); err != nil {
				t.Fatalf("unmarshal trigger: %v", err)
			}
			if decoded["type"] != tc.wantType {
				t.Fatalf("trigger JSON type = %v, want %q", decoded["type"], tc.wantType)
			}
			if decoded["text"] != tc.wantBody {
				t.Fatalf("trigger JSON text = %v, want %q", decoded["text"], tc.wantBody)
			}
		})
	}
}
