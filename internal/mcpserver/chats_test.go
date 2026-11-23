package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/anoraks-oasis/matrix-whatsapp-mcp/internal/matrix"
)

// resolutionRooms is the joined-chat list every chat-by-name test resolves
// against: one exact name (Cafe), one unique substring (roup in Family Group),
// and two names that share "Family" so the substring is ambiguous.
func resolutionRooms() []matrix.Room {
	return []matrix.Room{
		{ID: "!cafe:test", Name: "Cafe"},
		{ID: "!family:test", Name: "Family Group"},
		{ID: "!family2:test", Name: "Family Chat"},
	}
}

// TestChatByNameResolves runs the same resolution contract through every
// chat-scoped tool: exact ID, exact name, a unique substring, an ambiguous
// substring, no match, and chat together with chat_id.
func TestChatByNameResolves(t *testing.T) {
	for _, tool := range chatTools() {
		t.Run(tool.name, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				chat    string
				chatID  string
				want    string
				wantErr []string
			}{
				{name: "exact ID", chat: "!family:test", want: "!family:test"},
				{name: "exact name case-insensitive", chat: "cafe", want: "!cafe:test"},
				{name: "unique substring", chat: "roup", want: "!family:test"},
				{name: "ambiguous substring", chat: "family", wantErr: []string{"matches 2 chats", "Family Group", "Family Chat"}},
				{name: "no match lists the joined chats", chat: "Nowhere", wantErr: []string{`no chat matches "Nowhere"`, "Cafe"}},
				{name: "chat and chat_id together", chat: "Cafe", chatID: "!cafe:test", wantErr: []string{"either chat or chat_id"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					svc := &fakeMatrix{rooms: resolutionRooms(), sentID: "$sent", mediaSendID: "$media"}
					srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!cafe:test", "!family:test"})
					session, cleanup := connect(t, srv)
					defer cleanup()

					args := map[string]any{"chat": tc.chat}
					if tc.chatID != "" {
						args["chat_id"] = tc.chatID
					}
					for k, v := range tool.args {
						args[k] = v
					}
					res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tool.name, Arguments: args})
					if err != nil {
						t.Fatalf("%s: %v", tool.name, err)
					}
					if len(tc.wantErr) > 0 {
						if !res.IsError {
							t.Fatalf("%s(chat=%q) succeeded, want an error", tool.name, tc.chat)
						}
						text := resultErrorText(t, res)
						for _, want := range tc.wantErr {
							if !strings.Contains(text, want) {
								t.Fatalf("%s error = %q, want it to contain %q", tool.name, text, want)
							}
						}
						if chats := tool.extra(svc); len(chats) != 0 {
							t.Fatalf("%s reached the matrix layer with %v despite the resolution error", tool.name, chats)
						}
						return
					}
					if res.IsError {
						t.Fatalf("%s(chat=%q) returned error: %s", tool.name, tc.chat, resultErrorText(t, res))
					}
					chats := tool.extra(svc)
					if len(chats) != 1 || chats[0] != tc.want {
						t.Fatalf("%s(chat=%q) reached the matrix layer as %v, want [%s]", tool.name, tc.chat, chats, tc.want)
					}
				})
			}
		})
	}
}

// TestChatByNameNeverGrantsSendPermission proves a name is only a way to find a
// chat: the resolved ID still has to pass the send allowlist.
func TestChatByNameNeverGrantsSendPermission(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"send_message", map[string]any{"text": "hi"}},
		{"send_media", map[string]any{"data_base64": "aGk=", "mimetype": "image/png"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeMatrix{rooms: resolutionRooms(), sentID: "$sent", mediaSendID: "$media"}
			srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!other:test"})
			session, cleanup := connect(t, srv)
			defer cleanup()

			args := map[string]any{"chat": "Cafe"}
			for k, v := range tc.args {
				args[k] = v
			}
			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: tc.name, Arguments: args})
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if !res.IsError {
				t.Fatalf("%s to a non-allowlisted chat named by chat succeeded", tc.name)
			}
			if text := resultErrorText(t, res); !strings.Contains(text, "sending to this chat is not allowed") {
				t.Fatalf("%s error = %q, want the allowlist refusal", tc.name, text)
			}
			if rooms, _, _ := svc.sends(); len(rooms) != 0 {
				t.Fatalf("%s reached SendText despite the allowlist: %v", tc.name, rooms)
			}
			if sends := svc.mediaSent(); len(sends) != 0 {
				t.Fatalf("%s reached SendMedia despite the allowlist: %v", tc.name, sends)
			}
		})
	}
}

// TestChatByNameResolvesTheNameListChatsShows covers the display-name fallback:
// when the joined list carries no name, list_chats shows the canonical alias, so
// a name resolved that way has to find the same chat.
func TestChatByNameResolvesTheNameListChatsShows(t *testing.T) {
	svc := &fakeMatrix{rooms: []matrix.Room{{ID: "!x:test", Name: "!x:test"}}}
	prober := &fakeProber{aliases: map[string]string{"!x:test": "#cafe:test"}}
	srv := New(svc, prober, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_messages",
		Arguments: map[string]any{"chat": "#cafe:test"},
	})
	if err != nil {
		t.Fatalf("get_messages: %v", err)
	}
	if res.IsError {
		t.Fatalf("get_messages returned error: %s", resultErrorText(t, res))
	}
	if len(svc.msgsRoom) != 1 || svc.msgsRoom[0] != "!x:test" {
		t.Fatalf("get_messages reached the matrix layer as %v, want [!x:test]", svc.msgsRoom)
	}
}
