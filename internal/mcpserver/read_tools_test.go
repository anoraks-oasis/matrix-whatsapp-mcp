package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/matrix"
	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/trigger"
)

func TestSearchMessagesTool(t *testing.T) {
	svc := &fakeMatrix{searchOut: matrix.SearchOutcome{
		Hits: []matrix.SearchHit{
			{Message: matrix.Message{EventID: "$b", RoomID: "!r:test", Sender: "@bob:test", Body: "dinner tomorrow", MsgType: "m.text", Timestamp: 200}, RoomName: "Family"},
			{Message: matrix.Message{EventID: "$a", RoomID: "!r:test", Sender: "@alice:test", Body: "dinner at eight", MsgType: "m.text", Timestamp: 100, ReplyTo: "$parent"}, RoomName: "Family"},
		},
		Truncated:     true,
		ScannedRooms:  3,
		ScannedEvents: 42,
		Source:        matrix.SearchSourceScan,
	}}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "search_messages",
		Arguments: map[string]any{"query": "  dinner  ", "chat_id": "!r:test", "limit": 5, "since": 50},
	})
	if err != nil {
		t.Fatalf("search_messages: %v", err)
	}
	if res.IsError {
		t.Fatalf("search_messages returned error: %s", resultErrorText(t, res))
	}
	got := decodeStructured[searchMessagesResult](t, res)
	if len(got.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(got.Results))
	}
	if got.Results[0].MessageID != "$b" || got.Results[1].MessageID != "$a" {
		t.Fatalf("results = %+v, want newest first as the Matrix layer returned them", got.Results)
	}
	if got.Results[0].ChatName != "Family" || got.Results[0].Text != "dinner tomorrow" || got.Results[0].Type != "text" {
		t.Fatalf("hit = %+v, want the message shape with the chat it matched in", got.Results[0])
	}
	if got.Results[1].ReplyTo != "$parent" {
		t.Fatalf("hit reply_to = %q, want $parent carried through", got.Results[1].ReplyTo)
	}
	if !got.Truncated || got.ScannedChats != 3 || got.ScannedMessages != 42 {
		t.Fatalf("envelope = %+v, want truncated and the scan counts passed through", got)
	}
	if got.Source != matrix.SearchSourceScan {
		t.Fatalf("source = %q, want %q", got.Source, matrix.SearchSourceScan)
	}

	calls := svc.searchCalls()
	if len(calls) != 1 {
		t.Fatalf("matrix saw %d searches, want 1", len(calls))
	}
	if calls[0].Query != "dinner" {
		t.Fatalf("query = %q, want it trimmed to dinner", calls[0].Query)
	}
	if calls[0].RoomID != "!r:test" || calls[0].Limit != 5 || calls[0].SinceTimestamp != 50 {
		t.Fatalf("request = %+v, want the room, limit and date bound passed through", calls[0])
	}
}

func TestSearchMessagesToolClampsLimit(t *testing.T) {
	for _, tc := range []struct {
		limit int
		want  int
	}{
		{0, 20},
		{1, 1},
		{100, 100},
		{500, 100},
	} {
		svc := &fakeMatrix{}
		srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
		session, cleanup := connect(t, srv)

		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "search_messages",
			Arguments: map[string]any{"query": "x", "limit": tc.limit},
		})
		if err != nil {
			cleanup()
			t.Fatalf("search_messages(limit=%d): %v", tc.limit, err)
		}
		if res.IsError {
			cleanup()
			t.Fatalf("search_messages(limit=%d) returned error: %s", tc.limit, resultErrorText(t, res))
		}
		calls := svc.searchCalls()
		if len(calls) != 1 || calls[0].Limit != tc.want {
			cleanup()
			t.Fatalf("limit %d reached matrix as %+v, want %d", tc.limit, calls, tc.want)
		}
		cleanup()
	}
}

func TestSearchMessagesToolRejectsBadQuery(t *testing.T) {
	svc := &fakeMatrix{}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	for _, query := range []string{"", "   ", "\t\n"} {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "search_messages",
			Arguments: map[string]any{"query": query},
		})
		if err != nil {
			t.Fatalf("search_messages(%q): %v", query, err)
		}
		if text := resultErrorText(t, res); !strings.Contains(text, "empty") {
			t.Fatalf("empty query error = %q, want it to say the query is empty", text)
		}
	}

	long := strings.Repeat("x", maxSearchQueryLen+1)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "search_messages",
		Arguments: map[string]any{"query": long},
	})
	if err != nil {
		t.Fatalf("search_messages(long query): %v", err)
	}
	if text := resultErrorText(t, res); !strings.Contains(text, "512") {
		t.Fatalf("over-long query error = %q, want it to name the 512-character cap", text)
	}
	if calls := svc.searchCalls(); len(calls) != 0 {
		t.Fatalf("a rejected query reached matrix: %+v", calls)
	}
}

func TestSearchMessagesToolErrorAndCancellation(t *testing.T) {
	t.Run("matrix error surfaces", func(t *testing.T) {
		svc := &fakeMatrix{searchErr: errors.New("boom from the homeserver")}
		srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
		session, cleanup := connect(t, srv)
		defer cleanup()

		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "search_messages",
			Arguments: map[string]any{"query": "dinner"},
		})
		if err != nil {
			t.Fatalf("search_messages: %v", err)
		}
		if text := resultErrorText(t, res); !strings.Contains(text, "boom") {
			t.Fatalf("error = %q, want the Matrix failure", text)
		}
	})

	t.Run("cancellation is reported, not swallowed", func(t *testing.T) {
		svc := &fakeMatrix{}
		s := &Server{matrix: svc}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := s.handleSearchMessages(ctx, nil, searchMessagesInput{Query: "dinner"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("handler error = %v, want context.Canceled", err)
		}
	})
}

func TestGetMessageContextTool(t *testing.T) {
	svc := &fakeMatrix{rooms: []matrix.Room{{ID: "!r:test", Name: "Family"}}, ctxOut: matrix.ContextOutcome{
		Before: []matrix.Message{
			{EventID: "$b1", RoomID: "!r:test", Sender: "@alice:test", Body: "before", MsgType: "m.text", Timestamp: 800},
		},
		Event: matrix.Message{EventID: "$t", RoomID: "!r:test", Sender: "@bob:test", Body: "the target", MsgType: "m.text", Timestamp: 1000, ReplyTo: "$parent"},
		After: []matrix.Message{
			{EventID: "$a1", RoomID: "!r:test", Sender: "@alice:test", Body: "after", MsgType: "m.text", Timestamp: 1100},
		},
		ReplyTo: &matrix.Message{EventID: "$parent", RoomID: "!r:test", Sender: "@alice:test", Body: "the parent", MsgType: "m.text", Timestamp: 700},
		EditOf:  &matrix.Message{EventID: "$orig", RoomID: "!r:test", Sender: "@bob:test", Body: "the original", MsgType: "m.text", Timestamp: 600},
		Source:  matrix.ContextSourceEndpoint,
	}}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_message_context",
		Arguments: map[string]any{"chat_id": "!r:test", "message_id": "$t"},
	})
	if err != nil {
		t.Fatalf("get_message_context: %v", err)
	}
	if res.IsError {
		t.Fatalf("get_message_context returned error: %s", resultErrorText(t, res))
	}
	got := decodeStructured[getMessageContextResult](t, res)
	if got.Message.MessageID != "$t" || got.Message.Text != "the target" {
		t.Fatalf("message = %+v, want the target in the message shape", got.Message)
	}
	if len(got.MessagesBefore) != 1 || got.MessagesBefore[0].MessageID != "$b1" {
		t.Fatalf("messages_before = %+v, want [$b1]", got.MessagesBefore)
	}
	if len(got.MessagesAfter) != 1 || got.MessagesAfter[0].MessageID != "$a1" {
		t.Fatalf("messages_after = %+v, want [$a1]", got.MessagesAfter)
	}
	// Every message in the answer is in the same chat, so each one names it.
	for _, msg := range append(append([]messageResult{got.Message, *got.ReplyTo, *got.EditOf}, got.MessagesBefore...), got.MessagesAfter...) {
		if msg.ChatName != "Family" {
			t.Fatalf("message %s chat_name = %q, want the chat's display name Family", msg.MessageID, msg.ChatName)
		}
	}
	if got.ReplyTo == nil || got.ReplyTo.MessageID != "$parent" || got.ReplyTo.Text != "the parent" {
		t.Fatalf("reply_to = %+v, want the parent resolved to a full message", got.ReplyTo)
	}
	if got.EditOf == nil || got.EditOf.MessageID != "$orig" {
		t.Fatalf("edit_of = %+v, want the edited message resolved", got.EditOf)
	}
	if got.Source != matrix.ContextSourceEndpoint || got.Truncated {
		t.Fatalf("source/truncated = %q/%t, want context and not truncated", got.Source, got.Truncated)
	}

	befores, afters := svc.contextCalls()
	if len(befores) != 1 || befores[0] != defaultContextLimit || afters[0] != defaultContextLimit {
		t.Fatalf("limits reached matrix as %v/%v, want 10/10 by default", befores, afters)
	}
}

// TestGetMessageContextOutputSpeaksWhatsApp pins the JSON keys: the target and
// its neighbours are messages, and none of the old event/events_* names
// survives.
func TestGetMessageContextOutputSpeaksWhatsApp(t *testing.T) {
	svc := &fakeMatrix{ctxOut: matrix.ContextOutcome{
		Before: []matrix.Message{{EventID: "$b1", RoomID: "!r:test", Body: "before", MsgType: "m.text", Timestamp: 1}},
		Event:  matrix.Message{EventID: "$t", RoomID: "!r:test", Body: "the target", MsgType: "m.text", Timestamp: 2},
		After:  []matrix.Message{{EventID: "$a1", RoomID: "!r:test", Body: "after", MsgType: "m.text", Timestamp: 3}},
		Source: matrix.ContextSourceEndpoint,
	}}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_message_context",
		Arguments: map[string]any{"chat_id": "!r:test", "message_id": "$t"},
	})
	if err != nil {
		t.Fatalf("get_message_context: %v", err)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
	for _, field := range []string{"message", "messages_before", "messages_after", "source", "truncated"} {
		if _, ok := out[field]; !ok {
			t.Fatalf("result has no %q key: %s", field, raw)
		}
	}
	for _, old := range []string{"event", "events_before", "events_after"} {
		if _, ok := out[old]; ok {
			t.Fatalf("result still carries the old key %q: %s", old, raw)
		}
	}
}

// TestGetMessageContextToolLimits proves an omitted limit means 10 and an
// explicit 0 means none, which is why both knobs are optional integers.
func TestGetMessageContextToolLimits(t *testing.T) {
	for _, tc := range []struct {
		name          string
		before, after any
		wantBefore    int
		wantAfter     int
	}{
		{"omitted", nil, nil, 10, 10},
		{"explicit zero", 0, 0, 0, 0},
		{"over the cap", 51, 100, 50, 50},
		{"negative", -5, 1, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeMatrix{}
			srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
			session, cleanup := connect(t, srv)
			defer cleanup()

			args := map[string]any{"chat_id": "!r:test", "message_id": "$t"}
			if tc.before != nil {
				args["limit_before"] = tc.before
			}
			if tc.after != nil {
				args["limit_after"] = tc.after
			}
			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_message_context", Arguments: args})
			if err != nil {
				t.Fatalf("get_message_context: %v", err)
			}
			if res.IsError {
				t.Fatalf("get_message_context returned error: %s", resultErrorText(t, res))
			}
			befores, afters := svc.contextCalls()
			if len(befores) != 1 || befores[0] != tc.wantBefore || afters[0] != tc.wantAfter {
				t.Fatalf("limits reached matrix as %v/%v, want %d/%d", befores, afters, tc.wantBefore, tc.wantAfter)
			}
		})
	}
}

func TestGetMessageContextToolRejectsMalformedEvent(t *testing.T) {
	svc := &fakeMatrix{}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	for _, bad := range []string{"", "abc", "$", "$a b"} {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "get_message_context",
			Arguments: map[string]any{"chat_id": "!r:test", "message_id": bad},
		})
		if err != nil {
			t.Fatalf("get_message_context(event_id=%q): %v", bad, err)
		}
		if text := resultErrorText(t, res); !strings.Contains(text, "message ID") {
			t.Fatalf("malformed message error = %q, want it to name the message ID shape", text)
		}
	}
	if befores, _ := svc.contextCalls(); len(befores) != 0 {
		t.Fatalf("a malformed message ID reached matrix: %v", befores)
	}
}

func TestGetMessageContextToolErrorAndCancellation(t *testing.T) {
	t.Run("unknown event surfaces as a typed error", func(t *testing.T) {
		svc := &fakeMatrix{ctxErr: matrix.ErrEventNotFound}
		srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
		session, cleanup := connect(t, srv)
		defer cleanup()

		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "get_message_context",
			Arguments: map[string]any{"chat_id": "!r:test", "message_id": "$missing"},
		})
		if err != nil {
			t.Fatalf("get_message_context: %v", err)
		}
		if text := resultErrorText(t, res); !strings.Contains(text, "not found") {
			t.Fatalf("error = %q, want the not-found error rather than an empty context", text)
		}
	})

	t.Run("cancellation is reported, not swallowed", func(t *testing.T) {
		svc := &fakeMatrix{}
		s := &Server{matrix: svc}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := s.handleMessageContext(ctx, nil, getMessageContextInput{chatRef: chatRef{ChatID: "!r:test"}, messageRef: messageRef{MessageID: "$t"}}); !errors.Is(err, context.Canceled) {
			t.Fatalf("handler error = %v, want context.Canceled", err)
		}
	})
}

func TestListChatContactsTool(t *testing.T) {
	svc := &fakeMatrix{parts: []matrix.Participant{
		{UserID: "@alice:test", DisplayName: "Alice", Membership: "join"},
		{UserID: "@whatsapp_15551234567:test", DisplayName: "Bob (WA)", Membership: "join", Bridged: true, BridgeKind: matrix.BridgeKindContact},
		{UserID: "@whatsappbot:test", DisplayName: "WhatsApp bridge", Membership: "join", Bridged: true, BridgeKind: matrix.BridgeKindBot},
	}}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "list_chat_contacts",
		Arguments: map[string]any{"chat_id": "!r:test"},
	})
	if err != nil {
		t.Fatalf("list_chat_contacts: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_chat_contacts returned error: %s", resultErrorText(t, res))
	}
	got := decodeStructured[listChatContactsResult](t, res)
	if len(got.Contacts) != 3 {
		t.Fatalf("contacts = %d, want 3", len(got.Contacts))
	}
	if got.Contacts[0].UserID != "@alice:test" || got.Contacts[0].WhatsApp || got.Contacts[0].ContactType != "matrix_user" {
		t.Fatalf("plain member = %+v, want matrix_user", got.Contacts[0])
	}
	if got.Contacts[1].ContactType != matrix.BridgeKindContact || !got.Contacts[1].WhatsApp {
		t.Fatalf("contact = %+v, want a whatsapp_contact", got.Contacts[1])
	}
	if got.Contacts[2].ContactType != matrix.BridgeKindBot {
		t.Fatalf("bridge bot = %+v, want bridge_bot", got.Contacts[2])
	}
	if got.Contacts[0].Membership != "join" || got.Contacts[0].DisplayName != "Alice" {
		t.Fatalf("plain member = %+v, want membership and display name carried", got.Contacts[0])
	}
	if len(svc.partsRoom) != 1 || svc.partsRoom[0] != "!r:test" {
		t.Fatalf("matrix saw chats %v, want [!r:test]", svc.partsRoom)
	}
}

func TestListChatContactsToolErrorAndCancellation(t *testing.T) {
	t.Run("matrix error surfaces", func(t *testing.T) {
		svc := &fakeMatrix{partsErr: matrix.ErrInvalidRoomID}
		srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
		session, cleanup := connect(t, srv)
		defer cleanup()

		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "list_chat_contacts",
			Arguments: map[string]any{"chat_id": "not-a-room"},
		})
		if err != nil {
			t.Fatalf("list_chat_contacts: %v", err)
		}
		if text := resultErrorText(t, res); !strings.Contains(text, "invalid chat ID") {
			t.Fatalf("error = %q, want the invalid-room error", text)
		}
	})

	t.Run("cancellation is reported, not swallowed", func(t *testing.T) {
		svc := &fakeMatrix{}
		s := &Server{matrix: svc}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := s.handleListChatContacts(ctx, nil, listChatContactsInput{chatRef: chatRef{ChatID: "!r:test"}}); !errors.Is(err, context.Canceled) {
			t.Fatalf("handler error = %v, want context.Canceled", err)
		}
	})
}

// TestInitializeInstructionsNameBothAllowlistSources is R4's regression: the
// instructions are the first thing an MCP client reads, and a client told that
// only ALLOWED_SEND_ROOMS permits sending concludes that a room enrolled with
// `chats allow` is unsendable — the workflow this release added.
func TestInitializeInstructionsNameBothAllowlistSources(t *testing.T) {
	srv := New(&fakeMatrix{}, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	init := session.InitializeResult()
	if init == nil || init.Instructions == "" {
		t.Fatal("initialize returned no instructions")
	}
	for _, want := range []string{"ALLOWED_SEND_ROOMS", "allowlist file", "3 seconds"} {
		if !strings.Contains(init.Instructions, want) {
			t.Fatalf("instructions = %q, want them to mention %q", init.Instructions, want)
		}
	}
	if strings.Contains(init.Instructions, "only rooms listed in ALLOWED_SEND_ROOMS") {
		t.Fatalf("instructions still say only ALLOWED_SEND_ROOMS permits sending: %q", init.Instructions)
	}
}

// TestTriggerDescriptionStatesTheBuffer pins get_new_messages' description to
// the ring buffer it describes: the capacity comes from trigger.BufferSize, and
// the in-memory, restart-clearing nature of the buffer is what a caller needs
// to know before treating the list as history.
func TestTriggerDescriptionStatesTheBuffer(t *testing.T) {
	srv := New(&fakeMatrix{}, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	desc := toolByName(t, res.Tools, "get_new_messages").Description
	for _, want := range []string{fmt.Sprint(trigger.BufferSize), "in-memory", "restart", "since_timestamp"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("get_new_messages description = %q, want it to state %q", desc, want)
		}
	}
}

// TestGetMessagesCarriesChatName proves the message shape names its chat on the
// same chain search_messages reports: the joined chat's display name, then its
// canonical alias, then the chat ID. The lookup happens once per call, so a page
// of messages costs one name resolution rather than one per message.
func TestGetMessagesCarriesChatName(t *testing.T) {
	tests := []struct {
		name  string
		rooms []matrix.Room
		alias string
		want  string
	}{
		{
			name:  "display name",
			rooms: []matrix.Room{{ID: "!r:test", Name: "Family"}},
			want:  "Family",
		},
		{
			name:  "canonical alias when the chat has no name",
			rooms: []matrix.Room{{ID: "!r:test"}},
			alias: "#family:test",
			want:  "#family:test",
		},
		{
			name:  "chat ID when the name is not known",
			rooms: []matrix.Room{{ID: "!r:test"}},
			want:  "!r:test",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeMatrix{
				rooms: tc.rooms,
				msgs: []matrix.Message{
					{EventID: "$2", RoomID: "!r:test", Body: "new", MsgType: "m.text", Timestamp: 200},
					{EventID: "$1", RoomID: "!r:test", Body: "old", MsgType: "m.text", Timestamp: 100},
				},
			}
			prober := &fakeProber{}
			if tc.alias != "" {
				prober.aliases = map[string]string{"!r:test": tc.alias}
			}
			srv := New(svc, prober, &fakeRecent{}, nil)
			session, cleanup := connect(t, srv)
			defer cleanup()

			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "get_messages",
				Arguments: map[string]any{"chat_id": "!r:test"},
			})
			if err != nil {
				t.Fatalf("get_messages: %v", err)
			}
			if res.IsError {
				t.Fatalf("get_messages returned error: %s", resultErrorText(t, res))
			}
			msgs := decodeStructured[getMessagesResult](t, res).Messages
			if len(msgs) != 2 {
				t.Fatalf("got %d messages, want 2", len(msgs))
			}
			for _, msg := range msgs {
				if msg.ChatName != tc.want {
					t.Fatalf("message %s chat_name = %q, want %q", msg.MessageID, msg.ChatName, tc.want)
				}
			}
			if got := svc.joinedCalls(); got != 1 {
				t.Fatalf("joined-chat lookups = %d, want 1: the chat name is resolved once per call, not once per message", got)
			}
		})
	}
}

// TestResultShapesAreExact pins the key sets a model sees, because a model
// reads the shape and a field that appears in one tool but not its neighbour is
// a trap. The envelopes are objects (not bare arrays) so a later count or
// cursor has somewhere to go, and the rows carry chat_name, the one word every
// tool uses for a chat's name.
func TestResultShapesAreExact(t *testing.T) {
	svc := &fakeMatrix{
		rooms: []matrix.Room{{ID: "!r:test", Name: "Family"}},
		msgs: []matrix.Message{{
			EventID: "$m", RoomID: "!r:test", Sender: "@alice:test",
			Body: "hi", MsgType: "m.text", Timestamp: 100,
		}},
	}
	recent := &fakeRecent{out: []trigger.Trigger{{
		MessageID: "$m", ChatID: "!r:test", ChatName: "Family",
		From: "@alice:test", Text: "hi", Type: "text", Timestamp: 100,
	}}}
	srv := New(svc, &fakeProber{}, recent, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	envelopes := []struct {
		tool string
		args map[string]any
		want []string
	}{
		{tool: "list_chats", want: []string{"chats"}},
		{tool: "get_messages", args: map[string]any{"chat_id": "!r:test"}, want: []string{"has_more", "messages"}},
		{tool: "get_new_messages", want: []string{"messages"}},
	}
	objects := make(map[string]map[string]any, len(envelopes))
	for _, tc := range envelopes {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.tool, Arguments: tc.args})
		if err != nil {
			t.Fatalf("%s: %v", tc.tool, err)
		}
		if res.IsError {
			t.Fatalf("%s returned error: %s", tc.tool, resultErrorText(t, res))
		}
		obj := structuredObject(t, res)
		assertExactKeys(t, tc.tool+" result", obj, tc.want)
		objects[tc.tool] = obj
	}

	chatRow := firstRow(t, objects["list_chats"], "chats")
	assertExactKeys(t, "list_chats row", chatRow, []string{"chat_id", "chat_name", "whatsapp"})
	if chatRow["chat_name"] != "Family" {
		t.Fatalf("list_chats row chat_name = %v, want Family", chatRow["chat_name"])
	}

	messageRow := firstRow(t, objects["get_messages"], "messages")
	assertExactKeys(t, "get_messages message", messageRow, []string{
		"chat_id", "chat_name", "deleted", "encrypted", "from", "message_id", "text", "timestamp", "type",
	})
	if messageRow["chat_name"] != "Family" {
		t.Fatalf("get_messages message chat_name = %v, want Family", messageRow["chat_name"])
	}

	triggerRow := firstRow(t, objects["get_new_messages"], "messages")
	assertExactKeys(t, "get_new_messages row", triggerRow, []string{
		"chat_id", "chat_name", "from", "message_id", "text", "timestamp", "type",
	})
}

// structuredObject returns a tool result's structured content as a generic
// object, so a shape assertion sees the keys that were actually sent rather
// than the keys a struct would decode.
func structuredObject(t *testing.T, res *mcp.CallToolResult) map[string]any {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("structured content is not an object: %s", raw)
	}
	return obj
}

// firstRow returns the first element of an envelope's array field.
func firstRow(t *testing.T, obj map[string]any, field string) map[string]any {
	t.Helper()
	rows, ok := obj[field].([]any)
	if !ok {
		t.Fatalf("envelope has no %q array: %v", field, obj)
	}
	if len(rows) == 0 {
		t.Fatalf("envelope %q is empty, want one row", field)
	}
	row, ok := rows[0].(map[string]any)
	if !ok {
		t.Fatalf("%s[0] is %T, want an object", field, rows[0])
	}
	return row
}

// assertExactKeys compares a value's key set to want exactly, so an added,
// dropped or renamed field fails here instead of drifting between tools.
func assertExactKeys(t *testing.T, what string, obj map[string]any, want []string) {
	t.Helper()
	got := make([]string, 0, len(obj))
	for key := range obj {
		got = append(got, key)
	}
	sort.Strings(got)
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	if !slices.Equal(got, sorted) {
		t.Fatalf("%s keys = %v, want exactly %v", what, got, sorted)
	}
}
