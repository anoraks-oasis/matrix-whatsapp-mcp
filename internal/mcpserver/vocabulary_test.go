package mcpserver

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/anoraks-oasis/matrix-whatsapp-mcp/internal/matrix"
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

// TestVocabularySpeaksWhatsApp is the leak scan: every error string the tool
// packages can produce, every tool and schema description, and the server's
// handshake instructions must read like a WhatsApp tool. Matrix vocabulary is
// allowed only where it names a real identifier: an environment variable such
// as MATRIX_E2EE_ENABLED, the ALLOWED_SEND_ROOMS variable, or the matrix_user
// contact type. The ID formats themselves stay (`!abc:server`, `$abc123`) but
// are described as chat and message references, so they carry no banned word.
func TestVocabularySpeaksWhatsApp(t *testing.T) {
	texts := errorTexts(t)
	srv := New(&fakeMatrix{}, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	texts = append(texts, instructions)
	for _, tool := range res.Tools {
		texts = append(texts, descriptions(t, tool)...)
	}

	// Prove the scan sees all three sources, so a parser drift cannot turn
	// this test into a silent pass.
	for _, want := range []string{"could not read chat history", "this message has no attachment", "Tools for an agent working a WhatsApp account"} {
		found := false
		for _, text := range texts {
			if strings.Contains(text, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("scan did not collect %q; it is not looking at the right source", want)
		}
	}

	for _, text := range texts {
		if leak := matrixWord(text); leak != "" {
			t.Errorf("tool-visible text %q still says %q", text, leak)
		}
	}
}

// allowedVocabulary strips the identifiers that legitimately carry Matrix
// vocabulary: configuration names and the non-WhatsApp contact type.
var allowedVocabulary = []*regexp.Regexp{
	regexp.MustCompile(`(?i)MATRIX_[A-Z0-9_]+`),
	regexp.MustCompile(`(?i)ALLOWED_SEND_ROOMS`),
}

// matrixWord returns the first banned word in text, or "" when the text speaks
// WhatsApp. The match is whole-word and case-insensitive, so "prevent" and
// "eventual" are not flagged while "event" is.
func matrixWord(text string) string {
	for _, allowed := range allowedVocabulary {
		text = allowed.ReplaceAllString(text, "")
	}
	for _, word := range []string{"matrix", "room", "event", "mxc"} {
		if regexp.MustCompile(`(?i)\b` + word + `\b`).MatchString(text) {
			return word
		}
	}
	return ""
}

// errorTexts parses the packages behind the tool surface and returns every
// string literal passed to errors.New or fmt.Errorf, so the scan covers errors
// that are not exported constants too.
func errorTexts(t *testing.T) []string {
	t.Helper()
	dirs := []string{"../matrix", "../mcpserver", "../trigger", "../allowlist"}
	var out []string
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				switch fn := call.Fun.(type) {
				case *ast.SelectorExpr:
					pkg, ok := fn.X.(*ast.Ident)
					if !ok {
						return true
					}
					// failedText carries the tool-facing text of a failed
					// transport call, with the raw cause kept out of it.
					if fn.Sel.Name == "failedText" {
						break
					}
					if (pkg.Name != "errors" || fn.Sel.Name != "New") && (pkg.Name != "fmt" || fn.Sel.Name != "Errorf") {
						return true
					}
				case *ast.Ident:
					// invalidPageToken attaches a short reason to the sentinel;
					// that reason reaches the caller too.
					if fn.Name != "invalidPageToken" {
						return true
					}
				default:
					return true
				}
				if lit, ok := call.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if text, err := strconv.Unquote(lit.Value); err == nil {
						out = append(out, text)
					}
				}
				return true
			})
		}
	}
	if len(out) == 0 {
		t.Fatal("parsed no error strings; the scan is not looking at the source")
	}
	return out
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
