package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/anoraks-oasis/matrix-whatsapp-mcp/internal/config"
	"github.com/anoraks-oasis/matrix-whatsapp-mcp/internal/matrix"
	"github.com/anoraks-oasis/matrix-whatsapp-mcp/internal/trigger"
)

// stateRead names one state-event read the fake homeserver answered: the chat it
// was for and which state key was asked for.
type stateRead struct {
	room string
	key  string
}

const (
	stateKeyName  = "m.room.name"
	stateKeyAlias = "m.room.canonical_alias"
)

// chatNameHomeserver is a fake homeserver for a bridge account the process has
// not synced with yet. It answers only what chat-name resolution reads — the
// joined-chat list, each chat's m.room.name and its m.room.canonical_alias —
// plus the search index, and counts the reads per chat so a test can prove one
// lookup per distinct chat. A chat missing from names or aliases has no such
// state event, which the fake reports the way a homeserver does: a 404.
type chatNameHomeserver struct {
	joined     []string
	names      map[string]string
	aliases    map[string]string
	joinedErr  bool
	searchBody string

	mu          sync.Mutex
	reads       map[stateRead]int
	joinedReads int
	searches    int
}

// connect builds a real Matrix client against the fake homeserver and wires it
// into a fresh MCP server, so a test exercises the fix end to end: the client's
// sync cache is empty (Sync is never run) and the homeserver is the only source
// of a chat's name. The client satisfies both the Matrix service and the chat
// prober, as it does in the shipped server.
func (h *chatNameHomeserver) connect(t *testing.T, recent TriggerReader) (*matrix.Client, *mcp.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(srv.Close)

	c, err := matrix.New(&config.Config{
		Homeserver:  srv.URL,
		UserID:      "@bot:test",
		AccessToken: "access-token",
	}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("matrix client: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, New(c, c, recent, nil)
}

func (h *chatNameHomeserver) serve(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasSuffix(path, "/joined_rooms"):
		h.mu.Lock()
		h.joinedReads++
		h.mu.Unlock()
		if h.joinedErr {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"errcode":"M_UNKNOWN","error":"the room list is unavailable"}`)
			return
		}
		writeJSON(w, map[string]any{"joined_rooms": h.joined})
	case strings.Contains(path, "/state/"+stateKeyName):
		h.serveState(w, path, "/state/"+stateKeyName, stateKeyName, "name", h.names)
	case strings.Contains(path, "/state/"+stateKeyAlias):
		h.serveState(w, path, "/state/"+stateKeyAlias, stateKeyAlias, "alias", h.aliases)
	case strings.HasSuffix(path, "/search"):
		h.mu.Lock()
		h.searches++
		h.mu.Unlock()
		body := h.searchBody
		if body == "" {
			body = `{"search_categories":{"room_events":{"count":0,"results":[]}}}`
		}
		io.WriteString(w, body)
	default:
		http.NotFound(w, r)
	}
}

// serveState answers one state-event read and records it. An empty value means
// the chat has no such state event, so the answer is the 404 a homeserver sends.
func (h *chatNameHomeserver) serveState(w http.ResponseWriter, path, suffix, key, field string, values map[string]string) {
	roomID := strings.TrimPrefix(path, "/_matrix/client/v3/rooms/")
	// mautrix ends a state read's path with a trailing slash on some endpoints.
	if i := strings.Index(roomID, suffix); i >= 0 {
		roomID = roomID[:i]
	}
	h.mu.Lock()
	if h.reads == nil {
		h.reads = make(map[stateRead]int)
	}
	h.reads[stateRead{room: roomID, key: key}]++
	h.mu.Unlock()

	value := values[roomID]
	if value == "" {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"errcode":"M_NOT_FOUND","error":"Not found"}`)
		return
	}
	writeJSON(w, map[string]string{field: value})
}

// readCount reports how many times the fake answered one state read, which is
// how a test counts name lookups per chat.
func (h *chatNameHomeserver) readCount(roomID, key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reads[stateRead{room: roomID, key: key}]
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		io.WriteString(w, `{}`)
	}
}

// searchHit is one message the fake search index returns.
type searchHit struct {
	eventID string
	roomID  string
	body    string
}

// searchBodyFor builds the /search response for hits, each in the chat it names,
// stamped so the index returns them newest first in the order given.
func searchBodyFor(hits ...searchHit) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"search_categories":{"room_events":{"count":%d,"results":[`, len(hits))
	for i, hit := range hits {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"rank":0.9,"result":{"type":"m.room.message","event_id":%q,"room_id":%q,"sender":"@alice:test",`+
			`"origin_server_ts":%d,"content":{"msgtype":"m.text","body":%q}}}`, hit.eventID, hit.roomID, 1000-i, hit.body)
	}
	b.WriteString(`]}}}`)
	return b.String()
}

// staleTriggers builds the ring-buffer rows a process that has not synced holds:
// the chat name the recorder had at the time, which is the chat ID.
func staleTriggers(messages ...[2]string) *fakeRecent {
	out := make([]trigger.Trigger, 0, len(messages))
	for i, m := range messages {
		out = append(out, trigger.Trigger{
			MessageID: m[0],
			ChatID:    m[1],
			ChatName:  m[1],
			From:      "@alice:test",
			Text:      "dinner",
			Type:      "text",
			Timestamp: int64(1000 - i),
		})
	}
	return &fakeRecent{out: out}
}

// TestChatNamesResolveWithoutSync is the bug: a result's chat_name must be the
// name the chat has, not the ID the sync-seeded cache happens to hold, and the
// same chain must answer for search hits and for the new-message buffer. Each
// case starts with an empty cache, which the client reports as the raw chat ID,
// and the homeserver is the only place a name can come from.
func TestChatNamesResolveWithoutSync(t *testing.T) {
	const chatID = "!a:test"
	cases := []struct {
		name      string
		names     map[string]string
		aliases   map[string]string
		joinedErr bool
		want      string
	}{
		{
			name:  "display name from the room list",
			names: map[string]string{chatID: "mcp-selftest"},
			want:  "mcp-selftest",
		},
		{
			name:    "canonical alias when the chat has no name",
			aliases: map[string]string{chatID: "#selftest:test"},
			want:    "#selftest:test",
		},
		{
			name: "chat ID when it has neither",
			want: chatID,
		},
		{
			name:      "chat ID when the lookup fails",
			joinedErr: true,
			want:      chatID,
		},
	}
	for _, tc := range cases {
		for _, tool := range []string{"search_messages", "get_new_messages"} {
			t.Run(tc.name+"/"+tool, func(t *testing.T) {
				h := &chatNameHomeserver{
					joined:     []string{chatID},
					names:      tc.names,
					aliases:    tc.aliases,
					joinedErr:  tc.joinedErr,
					searchBody: searchBodyFor(searchHit{eventID: "$m", roomID: chatID, body: "dinner tomorrow"}),
				}
				client, srv := h.connect(t, staleTriggers([2]string{"$m", chatID}))
				session, cleanup := connect(t, srv)
				defer cleanup()

				// The bug's precondition: nothing has synced, so the client's own
				// name for the chat is the ID.
				if got := client.RoomName(chatID); got != chatID {
					t.Fatalf("client name before the call = %q, want the raw chat ID: the cache must start empty", got)
				}

				rows := callChatRows(t, session, tool, map[string]any{"query": "dinner"})
				if len(rows) != 1 {
					t.Fatalf("%s returned %d rows, want 1", tool, len(rows))
				}
				if rows[0].ChatID != chatID || rows[0].ChatName != tc.want {
					t.Fatalf("%s row = %+v, want chat_id %q with chat_name %q", tool, rows[0], chatID, tc.want)
				}
			})
		}
	}
}

// TestChatNameLookupIsOncePerDistinctChat proves a page spanning two chats pays
// one lookup per chat, not one per result: the third row repeats a chat the page
// already named.
func TestChatNameLookupIsOncePerDistinctChat(t *testing.T) {
	const (
		first  = "!a:test"
		second = "!b:test"
	)
	for _, tool := range []string{"search_messages", "get_new_messages"} {
		t.Run(tool, func(t *testing.T) {
			h := &chatNameHomeserver{
				joined: []string{first, second},
				names:  map[string]string{first: "Alpha", second: "Bravo"},
				searchBody: searchBodyFor(
					searchHit{eventID: "$1", roomID: first, body: "one"},
					searchHit{eventID: "$2", roomID: second, body: "one"},
					searchHit{eventID: "$3", roomID: first, body: "one"},
				),
			}
			_, srv := h.connect(t, staleTriggers(
				[2]string{"$1", first},
				[2]string{"$2", second},
				[2]string{"$3", first},
			))
			session, cleanup := connect(t, srv)
			defer cleanup()

			want := map[string]string{"$1": "Alpha", "$2": "Bravo", "$3": "Alpha"}
			got := make(map[string]string, len(want))
			for _, row := range callChatRows(t, session, tool, map[string]any{"query": "one"}) {
				got[row.MessageID] = row.ChatName
			}
			if len(got) != len(want) {
				t.Fatalf("%s returned %d rows, want %d", tool, len(got), len(want))
			}
			for messageID, name := range want {
				if got[messageID] != name {
					t.Fatalf("%s chat_name for %s = %q, want %q (rows: %v)", tool, messageID, got[messageID], name, got)
				}
			}

			for _, roomID := range []string{first, second} {
				if reads := h.readCount(roomID, stateKeyName); reads != 1 {
					t.Fatalf("%s read the display name of %s %d times, want exactly 1 for one call", tool, roomID, reads)
				}
			}
			if h.joinedReads != 1 {
				t.Fatalf("%s read the joined-chat list %d times, want 1 for one call", tool, h.joinedReads)
			}
		})
	}
}

// TestChatNameResolutionIsBounded proves the lookup chain runs under one overall
// deadline per call, so a chat whose name cannot be read costs a short wait
// rather than a hang, and that one call reads the joined-chat list once.
func TestChatNameResolutionIsBounded(t *testing.T) {
	if chatNameTimeout <= 0 {
		t.Fatalf("chatNameTimeout = %s, want a positive bound", chatNameTimeout)
	}
	svc := &fakeMatrix{
		rooms: []matrix.Room{{ID: "!a:test"}, {ID: "!b:test"}},
		searchOut: matrix.SearchOutcome{Hits: []matrix.SearchHit{
			{Message: matrix.Message{EventID: "$1", RoomID: "!a:test", Body: "x", MsgType: "m.text", Timestamp: 200}},
			{Message: matrix.Message{EventID: "$2", RoomID: "!b:test", Body: "x", MsgType: "m.text", Timestamp: 100}},
		}},
	}
	prober := &deadlineProber{}
	srv := New(svc, prober, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	if rows := callChatRows(t, session, "search_messages", map[string]any{"query": "x"}); len(rows) != 2 {
		t.Fatalf("search returned %d rows, want 2", len(rows))
	}
	seen := prober.seen()
	if len(seen) != 2 {
		t.Fatalf("recorded %d alias probes, want one per unnamed chat", len(seen))
	}
	for i, remaining := range seen {
		if remaining <= 0 {
			t.Fatalf("alias probe %d ran without a deadline: the lookup is unbounded", i)
		}
		if remaining > chatNameTimeout {
			t.Fatalf("alias probe %d had %s left, want at most the %s bound", i, remaining, chatNameTimeout)
		}
	}
	if got := svc.joinedCalls(); got != 1 {
		t.Fatalf("joined-chat reads = %d, want 1 per call", got)
	}
}

// TestChatNameResolutionSkipsWorkItAlreadyDid proves the resolver is cached for
// the call and the chain stops at the first name it finds: a chat with a display
// name never costs an alias probe, and a second call reuses the name the client
// already knows.
func TestChatNameResolutionSkipsWorkItAlreadyDid(t *testing.T) {
	h := &chatNameHomeserver{
		joined:     []string{"!a:test"},
		names:      map[string]string{"!a:test": "mcp-selftest"},
		searchBody: searchBodyFor(searchHit{eventID: "$m", roomID: "!a:test", body: "dinner"}),
	}
	client, srv := h.connect(t, &fakeRecent{})
	session, cleanup := connect(t, srv)
	defer cleanup()

	for i := 0; i < 2; i++ {
		rows := callChatRows(t, session, "search_messages", map[string]any{"query": "dinner"})
		if len(rows) != 1 || rows[0].ChatName != "mcp-selftest" {
			t.Fatalf("call %d: rows = %+v, want one row named mcp-selftest", i+1, rows)
		}
	}
	if reads := h.readCount("!a:test", stateKeyName); reads != 1 {
		t.Fatalf("read the display name %d times across two calls, want 1: the second call reuses it", reads)
	}
	if reads := h.readCount("!a:test", stateKeyAlias); reads != 0 {
		t.Fatalf("probed the canonical alias %d times for a chat with a display name, want 0", reads)
	}
	if h.joinedReads != 2 {
		t.Fatalf("joined-chat reads = %d across two calls, want 2", h.joinedReads)
	}
	if got := client.RoomName("!a:test"); got != "mcp-selftest" {
		t.Fatalf("client name after the call = %q, want the name the homeserver carries", got)
	}
}

// chatNameRow is the field set these tests read from either message tool.
type chatNameRow struct {
	MessageID string
	ChatID    string
	ChatName  string
}

// callChatRows calls a message tool and returns its rows' chat fields, reading
// either envelope the same way so a test can assert both tools at once.
func callChatRows(t *testing.T, session *mcp.ClientSession, tool string, args map[string]any) []chatNameRow {
	t.Helper()
	if tool != "search_messages" {
		args = nil
	}
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("%s returned error: %s", tool, resultErrorText(t, res))
	}
	switch tool {
	case "search_messages":
		results := decodeStructured[searchMessagesResult](t, res).Results
		rows := make([]chatNameRow, 0, len(results))
		for _, row := range results {
			rows = append(rows, chatNameRow{MessageID: row.MessageID, ChatID: row.ChatID, ChatName: row.ChatName})
		}
		return rows
	case "get_new_messages":
		messages := decodeStructured[getNewMessagesResult](t, res).Messages
		rows := make([]chatNameRow, 0, len(messages))
		for _, row := range messages {
			rows = append(rows, chatNameRow{MessageID: row.MessageID, ChatID: row.ChatID, ChatName: row.ChatName})
		}
		return rows
	default:
		t.Fatalf("unknown tool %q", tool)
		return nil
	}
}
