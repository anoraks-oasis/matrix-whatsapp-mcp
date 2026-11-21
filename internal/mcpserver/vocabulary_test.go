package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/matrix"
)

// chatTool is one tool that names a chat, with the extra arguments that make a
// call valid and a reader for the chat the matrix layer was asked about.
type chatTool struct {
	name  string
	args  map[string]any
	extra func(*fakeMatrix) []string
}

func chatTools() []chatTool {
	return []chatTool{
		{"get_messages", nil, func(f *fakeMatrix) []string { return f.msgsRoom }},
		{"search_messages", map[string]any{"query": "dinner"}, func(f *fakeMatrix) []string {
			var rooms []string
			for _, req := range f.searchReqs {
				rooms = append(rooms, req.RoomID)
			}
			return rooms
		}},
		{"get_message_context", map[string]any{"message_id": "$m"}, func(f *fakeMatrix) []string { return f.ctxRoom }},
		{"list_chat_contacts", nil, func(f *fakeMatrix) []string { return f.partsRoom }},
		{"get_media", map[string]any{"message_id": "$m"}, func(f *fakeMatrix) []string { return f.mediaRoom }},
		{"send_message", map[string]any{"text": "hi"}, func(f *fakeMatrix) []string { return f.sentRoom }},
		{"send_media", map[string]any{"data_base64": "aGk=", "mimetype": "image/png"}, func(f *fakeMatrix) []string {
			var rooms []string
			for _, call := range f.mediaSends {
				rooms = append(rooms, call.roomID)
			}
			return rooms
		}},
	}
}

// TestChatIDAndRoomIDAlias proves every chat-taking tool accepts the canonical
// chat_id and the deprecated room_id for the same call, and reaches the matrix
// layer with the same chat either way.
func TestChatIDAndRoomIDAlias(t *testing.T) {
	for _, tc := range chatTools() {
		t.Run(tc.name, func(t *testing.T) {
			seen := make(map[string]string, 2)
			for _, field := range []string{"chat_id", "room_id"} {
				svc := &fakeMatrix{mediaSendID: "$media", sentID: "$sent"}
				srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!r:test"})
				session, cleanup := connect(t, srv)

				args := map[string]any{field: "!r:test"}
				for k, v := range tc.args {
					args[k] = v
				}
				res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.name, Arguments: args})
				cleanup()
				if err != nil {
					t.Fatalf("%s(%s): %v", tc.name, field, err)
				}
				if res.IsError {
					t.Fatalf("%s(%s) returned error: %s", tc.name, field, resultErrorText(t, res))
				}
				chats := tc.extra(svc)
				if len(chats) != 1 {
					t.Fatalf("%s(%s): matrix saw %d chats, want 1", tc.name, field, len(chats))
				}
				seen[field] = chats[0]
			}
			if seen["chat_id"] != "!r:test" || seen["room_id"] != "!r:test" {
				t.Fatalf("%s received chat_id=%q room_id=%q, want both !r:test", tc.name, seen["chat_id"], seen["room_id"])
			}
		})
	}
}

// TestMessageIDAndEventIDAlias is the message-side twin: both names reach the
// matrix layer as the same message ID.
func TestMessageIDAndEventIDAlias(t *testing.T) {
	for _, tool := range []struct {
		name string
		args map[string]any
		read func(*fakeMatrix) []string
	}{
		{"get_message_context", map[string]any{"chat_id": "!r:test"}, func(f *fakeMatrix) []string { return f.ctxEvent }},
		{"get_media", map[string]any{"chat_id": "!r:test"}, func(f *fakeMatrix) []string { return f.mediaEvent }},
	} {
		t.Run(tool.name, func(t *testing.T) {
			seen := make(map[string]string, 2)
			for _, field := range []string{"message_id", "event_id"} {
				svc := &fakeMatrix{}
				srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
				session, cleanup := connect(t, srv)

				args := map[string]any{field: "$m"}
				for k, v := range tool.args {
					args[k] = v
				}
				res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool.name, Arguments: args})
				cleanup()
				if err != nil {
					t.Fatalf("%s(%s): %v", tool.name, field, err)
				}
				if res.IsError {
					t.Fatalf("%s(%s) returned error: %s", tool.name, field, resultErrorText(t, res))
				}
				events := tool.read(svc)
				if len(events) != 1 {
					t.Fatalf("%s(%s): matrix saw %d messages, want 1", tool.name, field, len(events))
				}
				seen[field] = events[0]
			}
			if seen["message_id"] != "$m" || seen["event_id"] != "$m" {
				t.Fatalf("%s received message_id=%q event_id=%q, want both $m", tool.name, seen["message_id"], seen["event_id"])
			}
		})
	}
}

// TestSinceAlias keeps the Matrix-era date bound working: `since` is canonical
// and `since_timestamp` reaches the matrix layer unchanged.
func TestSinceAlias(t *testing.T) {
	t.Run("search_messages", func(t *testing.T) {
		for _, field := range []string{"since", "since_timestamp"} {
			svc := &fakeMatrix{}
			srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
			session, cleanup := connect(t, srv)

			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "search_messages",
				Arguments: map[string]any{"query": "x", field: 50},
			})
			cleanup()
			if err != nil {
				t.Fatalf("search_messages(%s): %v", field, err)
			}
			if res.IsError {
				t.Fatalf("search_messages(%s) returned error: %s", field, resultErrorText(t, res))
			}
			calls := svc.searchCalls()
			if len(calls) != 1 || calls[0].SinceTimestamp != 50 {
				t.Fatalf("search_messages(%s) reached matrix as %+v, want the 50ms bound", field, calls)
			}
		}
	})

	t.Run("get_new_messages", func(t *testing.T) {
		for _, field := range []string{"since", "since_timestamp"} {
			recent := &fakeRecent{}
			srv := New(&fakeMatrix{}, &fakeProber{}, recent, nil)
			session, cleanup := connect(t, srv)

			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "get_new_messages",
				Arguments: map[string]any{field: 50},
			})
			cleanup()
			if err != nil {
				t.Fatalf("get_new_messages(%s): %v", field, err)
			}
			if res.IsError {
				t.Fatalf("get_new_messages(%s) returned error: %s", field, resultErrorText(t, res))
			}
			since, _ := recent.lastCall()
			if since != 50 {
				t.Fatalf("get_new_messages(%s) reached the reader with since=%d, want 50", field, since)
			}
		}
	})
}

// TestMessageShapeUsesWhatsAppFieldNames pins the JSON the caller receives: the
// message carries the WhatsApp field names and none of the Matrix-era ones.
func TestMessageShapeUsesWhatsAppFieldNames(t *testing.T) {
	svc := &fakeMatrix{msgs: []matrix.Message{{
		EventID: "$m", RoomID: "!r:test", Sender: "@alice:test",
		Body: "hello", MsgType: "m.text", Timestamp: 100,
		ReplyTo: "$parent", MXCURI: "mxc://test/media",
	}}}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_messages",
		Arguments: map[string]any{"chat_id": "!r:test"},
	})
	if err != nil {
		t.Fatalf("get_messages: %v", err)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var envelope struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
	if len(envelope.Messages) != 1 {
		t.Fatalf("message count = %d, want 1", len(envelope.Messages))
	}
	msg := envelope.Messages[0]
	for field, want := range map[string]any{
		"chat_id":    "!r:test",
		"message_id": "$m",
		"from":       "@alice:test",
		"text":       "hello",
		"type":       "text",
		"timestamp":  float64(100),
		"encrypted":  false,
		"deleted":    false,
		"reply_to":   "$parent",
		"media_uri":  "mxc://test/media",
	} {
		got, ok := msg[field]
		if !ok {
			t.Fatalf("message has no %q field: %v", field, msg)
		}
		if got != want {
			t.Fatalf("message %q = %v, want %v", field, got, want)
		}
	}
	for _, old := range []string{"room_id", "event_id", "sender", "body", "msgtype", "redacted", "mxc_uri"} {
		if _, ok := msg[old]; ok {
			t.Fatalf("message still carries the Matrix-era field %q: %v", old, msg)
		}
	}
}

// TestMessageTypeMapping exercises the whole WhatsApp type table through the
// tool boundary, including the other bucket for an unrecognized type.
func TestMessageTypeMapping(t *testing.T) {
	tests := []struct {
		msgType  string
		wantType string
	}{
		{"m.text", "text"},
		{"m.notice", "text"},
		{"m.emote", "text"},
		{"m.image", "image"},
		{"m.video", "video"},
		{"m.audio", "audio"},
		{"m.file", "document"},
		{"m.sticker", "sticker"},
		{"m.location", "location"},
		{"m.reaction", "other"},
	}
	svc := &fakeMatrix{}
	for _, tc := range tests {
		svc.msgs = append(svc.msgs, matrix.Message{
			EventID: "$" + tc.wantType, RoomID: "!r:test", Body: "x", MsgType: tc.msgType, Timestamp: 1,
		})
	}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_messages",
		Arguments: map[string]any{"chat_id": "!r:test"},
	})
	if err != nil {
		t.Fatalf("get_messages: %v", err)
	}
	msgs := decodeStructured[getMessagesResult](t, res).Messages
	if len(msgs) != len(tests) {
		t.Fatalf("got %d messages, want %d", len(msgs), len(tests))
	}
	got := make(map[string]string, len(msgs))
	for _, msg := range msgs {
		got[msg.MessageID] = msg.Type
	}
	for _, tc := range tests {
		if got["$"+tc.wantType] != tc.wantType {
			t.Errorf("%s reported type %q, want %q", tc.msgType, got["$"+tc.wantType], tc.wantType)
		}
	}
}

// TestDescriptionsSpeakWhatsApp keeps the Matrix vocabulary out of everything a
// model reads: no tool or schema description says msgtype, bridged or mxc, and
// "room" and "event" appear only where the Matrix ID format is being explained
// or the deprecated alias is being named.
func TestDescriptionsSpeakWhatsApp(t *testing.T) {
	srv := New(&fakeMatrix{}, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	allowed := []string{"matrix room id", "matrix event id", "room_id", "event_id", "events_before", "events_after"}
	for _, tool := range res.Tools {
		for _, desc := range descriptions(t, tool) {
			lower := strings.ToLower(desc)
			for _, banned := range []string{"msgtype", "bridged", "mxc"} {
				if strings.Contains(lower, banned) {
					t.Fatalf("%s description %q still says %q", tool.Name, desc, banned)
				}
			}
			rest := lower
			for _, phrase := range allowed {
				rest = strings.ReplaceAll(rest, phrase, "")
			}
			for _, banned := range []string{"room", "event"} {
				if strings.Contains(rest, banned) {
					t.Fatalf("%s description %q uses %q outside an ID-format explanation", tool.Name, desc, banned)
				}
			}
		}
	}
}

// descriptions returns a tool's own description plus every description in its
// input and output schemas.
func descriptions(t *testing.T, tool *mcp.Tool) []string {
	t.Helper()
	out := []string{tool.Description}
	for _, schema := range []any{tool.InputSchema, tool.OutputSchema} {
		b, err := json.Marshal(schema)
		if err != nil {
			t.Fatalf("marshal %s schema: %v", tool.Name, err)
		}
		var parsed map[string]any
		if err := json.Unmarshal(b, &parsed); err != nil {
			t.Fatalf("unmarshal %s schema: %v", tool.Name, err)
		}
		collectDescriptions(parsed, &out)
	}
	return out
}

func collectDescriptions(value any, out *[]string) {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			if key == "description" {
				if desc, ok := child.(string); ok {
					*out = append(*out, desc)
					continue
				}
			}
			collectDescriptions(child, out)
		}
	case []any:
		for _, child := range v {
			collectDescriptions(child, out)
		}
	}
}
