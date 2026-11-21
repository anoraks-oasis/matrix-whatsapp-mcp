package mcpserver

import (
	"context"
	"errors"
	"fmt"
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
		t.Fatalf("hit = %+v, want the message shape plus chat_name", got.Results[0])
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
	svc := &fakeMatrix{ctxOut: matrix.ContextOutcome{
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
	if got.Event.MessageID != "$t" || got.Event.Text != "the target" {
		t.Fatalf("event = %+v, want the target in the message shape", got.Event)
	}
	if len(got.EventsBefore) != 1 || got.EventsBefore[0].MessageID != "$b1" {
		t.Fatalf("events_before = %+v, want [$b1]", got.EventsBefore)
	}
	if len(got.EventsAfter) != 1 || got.EventsAfter[0].MessageID != "$a1" {
		t.Fatalf("events_after = %+v, want [$a1]", got.EventsAfter)
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
		if text := resultErrorText(t, res); !strings.Contains(text, "event ID") {
			t.Fatalf("malformed event error = %q, want it to name the event ID shape", text)
		}
	}
	if befores, _ := svc.contextCalls(); len(befores) != 0 {
		t.Fatalf("a malformed event ID reached matrix: %v", befores)
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
		if text := resultErrorText(t, res); !strings.Contains(text, "invalid room ID") {
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
