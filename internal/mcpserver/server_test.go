package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/matrix"
	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/trigger"
)

// fakeMatrix is an in-memory MatrixService. Each method records its arguments
// so handler tests can assert what was passed through the MCP boundary.
type fakeMatrix struct {
	rooms     []matrix.Room
	roomsErr  error
	msgs      []matrix.Message
	msgsErr   error
	msgsRoom  []string
	msgsLimit []int
	sentRoom  []string
	sentText  []string
	sentID    string
	sentErr   error
}

func (f *fakeMatrix) JoinedRooms(context.Context) ([]matrix.Room, error) {
	return f.rooms, f.roomsErr
}

func (f *fakeMatrix) Messages(_ context.Context, roomID string, limit int) ([]matrix.Message, error) {
	f.msgsRoom = append(f.msgsRoom, roomID)
	f.msgsLimit = append(f.msgsLimit, limit)
	return f.msgs, f.msgsErr
}

func (f *fakeMatrix) SendText(_ context.Context, roomID, text string) (string, error) {
	f.sentRoom = append(f.sentRoom, roomID)
	f.sentText = append(f.sentText, text)
	return f.sentID, f.sentErr
}

func (f *fakeMatrix) Sync(context.Context) error { return nil }
func (f *fakeMatrix) Close() error               { return nil }

// fakeProber is a RoomProber. IsBridged returns false (the documented
// best-effort fallback) for rooms whose probe "errors"; the room must still be
// listed, which is exactly the behavior under test.
type fakeProber struct {
	bridged map[string]bool
	aliases map[string]string
}

func (p *fakeProber) IsBridged(_ context.Context, roomID string) bool {
	if p.bridged == nil {
		return false
	}
	return p.bridged[roomID]
}

func (p *fakeProber) CanonicalAlias(_ context.Context, roomID string) string {
	if p.aliases == nil {
		return ""
	}
	return p.aliases[roomID]
}

// fakeRecent is a TriggerReader that records the arguments passed by the
// handler so since/limit clamping can be asserted at the MCP boundary.
type fakeRecent struct {
	mu       sync.Mutex
	gotSince int64
	gotLimit int
	out      []trigger.Trigger
}

func (f *fakeRecent) Recent(since int64, limit int) []trigger.Trigger {
	f.mu.Lock()
	f.gotSince = since
	f.gotLimit = limit
	f.mu.Unlock()
	return f.out
}

func (f *fakeRecent) lastCall() (int64, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotSince, f.gotLimit
}

// connect wires an in-memory MCP client to the given server and returns the
// client session plus a cleanup func.
func connect(t *testing.T, srv *mcp.Server) (*mcp.ClientSession, func()) {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		serverSession.Close()
		t.Fatalf("client connect: %v", err)
	}
	return clientSession, func() {
		clientSession.Close()
		serverSession.Close()
	}
}

func decodeStructured[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal structured content: %v", err)
	}
	return out
}

func resultErrorText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if !res.IsError {
		t.Fatalf("tool result is not an error: %+v", res)
	}
	if len(res.Content) == 0 {
		t.Fatal("error result has no content")
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("error content is %T, want *mcp.TextContent", res.Content[0])
	}
	return text.Text
}

func toolByName(t *testing.T, tools []*mcp.Tool, name string) *mcp.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}

func schemaMap(t *testing.T, tool *mcp.Tool) map[string]any {
	t.Helper()
	m, ok := tool.InputSchema.(map[string]any)
	if !ok {
		t.Fatalf("tool %q InputSchema is %T, want map[string]any", tool.Name, tool.InputSchema)
	}
	return m
}

func requiredFields(t *testing.T, schema map[string]any) []string {
	t.Helper()
	req, _ := schema["required"].([]any)
	out := make([]string, 0, len(req))
	for _, r := range req {
		s, ok := r.(string)
		if !ok {
			t.Fatalf("required entry is %T, want string", r)
		}
		out = append(out, s)
	}
	return out
}

func hasField(t *testing.T, schema map[string]any, name string) []string {
	req := requiredFields(t, schema)
	for _, r := range req {
		if r == name {
			return req
		}
	}
	t.Fatalf("field %q not in required list %v", name, req)
	return nil
}

func TestToolsListHasFourToolsWithSchemas(t *testing.T) {
	srv := New(&fakeMatrix{}, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != 4 {
		t.Fatalf("ListTools returned %d tools, want 4", len(res.Tools))
	}
	names := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{"list_rooms", "read_messages", "get_recent_triggers", "send_message"} {
		if !names[want] {
			t.Fatalf("ListTools missing %q (got %v)", want, names)
		}
	}

	listRooms := schemaMap(t, toolByName(t, res.Tools, "list_rooms"))
	if len(requiredFields(t, listRooms)) != 0 {
		t.Fatalf("list_rooms required = %v, want none", requiredFields(t, listRooms))
	}

	read := schemaMap(t, toolByName(t, res.Tools, "read_messages"))
	hasField(t, read, "room_id")
	// read_messages must describe the type of every message it returns, so a
	// media event is never a blank body with no hint about what it was.
	for _, field := range []string{"msgtype", "encrypted", "body"} {
		if _, ok := outputProperties(t, toolByName(t, res.Tools, "read_messages"))[field]; !ok {
			t.Fatalf("read_messages output schema has no %q property", field)
		}
	}
	readProps := read["properties"].(map[string]any)
	limit := readProps["limit"].(map[string]any)
	if limit["type"] != "integer" {
		t.Fatalf("read_messages limit type = %v, want integer", limit["type"])
	}
	if desc, _ := limit["description"].(string); !strings.Contains(desc, "default 20") || !strings.Contains(desc, "100") {
		t.Fatalf("read_messages limit description = %q, want it to document default 20 and max 100", desc)
	}

	recent := schemaMap(t, toolByName(t, res.Tools, "get_recent_triggers"))
	if len(requiredFields(t, recent)) != 0 {
		t.Fatalf("get_recent_triggers required = %v, want none", requiredFields(t, recent))
	}

	send := schemaMap(t, toolByName(t, res.Tools, "send_message"))
	req := requiredFields(t, send)
	if len(req) != 2 || req[0] != "room_id" || req[1] != "text" {
		t.Fatalf("send_message required = %v, want [room_id text]", req)
	}
}

func TestListRoomsKeepsRoomsWithFailedProbes(t *testing.T) {
	svc := &fakeMatrix{rooms: []matrix.Room{
		{ID: "!a:test", Name: "Alpha"},
		{ID: "!b:test", Name: ""},
	}}
	prober := &fakeProber{
		bridged: map[string]bool{"!a:test": true}, // !b:test probe "errors" -> false
		aliases: map[string]string{"!b:test": "#beta:test"},
	}
	srv := New(svc, prober, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_rooms"})
	if err != nil {
		t.Fatalf("list_rooms: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_rooms returned error: %s", resultErrorText(t, res))
	}
	rooms := decodeStructured[[]roomResult](t, res)
	if len(rooms) != 2 {
		t.Fatalf("list_rooms returned %d rooms, want 2", len(rooms))
	}
	if rooms[0].RoomID != "!a:test" || rooms[0].Name != "Alpha" || !rooms[0].Bridged {
		t.Fatalf("rooms[0] = %+v, want !a:test/Alpha/bridged", rooms[0])
	}
	if rooms[1].RoomID != "!b:test" || rooms[1].Name != "#beta:test" || rooms[1].Bridged {
		t.Fatalf("rooms[1] = %+v, want !b:test alias fallback and bridged=false despite probe error", rooms[1])
	}
}

// outputProperties returns the properties of a tool's structured output, which
// is an array schema in every tool here.
func outputProperties(t *testing.T, tool *mcp.Tool) map[string]any {
	t.Helper()
	schema, ok := tool.OutputSchema.(map[string]any)
	if !ok {
		t.Fatalf("tool %q OutputSchema is %T, want map[string]any", tool.Name, tool.OutputSchema)
	}
	items, ok := schema["items"].(map[string]any)
	if !ok {
		t.Fatalf("tool %q output schema has no items object: %v", tool.Name, schema)
	}
	props, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("tool %q output schema has no properties: %v", tool.Name, items)
	}
	return props
}

func TestReadMessagesOrdersAndClamps(t *testing.T) {
	svc := &fakeMatrix{msgs: []matrix.Message{
		{EventID: "$2", RoomID: "!r:test", Body: "new", Timestamp: 200},
		{EventID: "$1", RoomID: "!r:test", Body: "old", Timestamp: 100},
	}}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	for _, limit := range []int{0, 1, 100, 500} {
		_, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "read_messages",
			Arguments: map[string]any{"room_id": "!r:test", "limit": limit},
		})
		if err != nil {
			t.Fatalf("read_messages(limit=%d): %v", limit, err)
		}
	}

	wantLimits := []int{20, 1, 100, 100}
	if len(svc.msgsLimit) != len(wantLimits) {
		t.Fatalf("matrix saw %d limit args %v, want %v", len(svc.msgsLimit), svc.msgsLimit, wantLimits)
	}
	for i, want := range wantLimits {
		if svc.msgsLimit[i] != want {
			t.Fatalf("limit arg %d = %d, want %d", i, svc.msgsLimit[i], want)
		}
	}

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "read_messages",
		Arguments: map[string]any{"room_id": "!r:test", "limit": 100},
	})
	if err != nil {
		t.Fatalf("read_messages: %v", err)
	}
	msgs := decodeStructured[[]messageResult](t, res)
	if len(msgs) != 2 || msgs[0].EventID != "$1" || msgs[1].EventID != "$2" {
		t.Fatalf("read_messages = %+v, want [$1 $2] oldest first", msgs)
	}
}

// TestReadMessagesCarriesMsgType proves the live media defect is closed at the
// MCP boundary: every message reports its msgtype and encryption flag, and the
// media placeholder produced by the Matrix layer is passed through untouched.
func TestReadMessagesCarriesMsgType(t *testing.T) {
	svc := &fakeMatrix{msgs: []matrix.Message{
		{EventID: "$img", RoomID: "!r:test", Body: "[image]", MsgType: "m.image", Timestamp: 200},
		{EventID: "$txt", RoomID: "!r:test", Body: "hello", MsgType: "m.text", Timestamp: 100, Encrypted: true},
	}}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "read_messages",
		Arguments: map[string]any{"room_id": "!r:test"},
	})
	if err != nil {
		t.Fatalf("read_messages: %v", err)
	}
	if res.IsError {
		t.Fatalf("read_messages returned error: %s", resultErrorText(t, res))
	}
	msgs := decodeStructured[[]messageResult](t, res)
	if len(msgs) != 2 {
		t.Fatalf("read_messages returned %d messages, want 2", len(msgs))
	}
	if msgs[0].EventID != "$txt" || msgs[0].MsgType != "m.text" || !msgs[0].Encrypted || msgs[0].Body != "hello" {
		t.Fatalf("msgs[0] = %+v, want $txt/m.text/encrypted with body hello", msgs[0])
	}
	if msgs[1].EventID != "$img" || msgs[1].MsgType != "m.image" || msgs[1].Body != "[image]" {
		t.Fatalf("msgs[1] = %+v, want $img/m.image with body [image]", msgs[1])
	}
}

func TestGetRecentTriggersSinceAndLimit(t *testing.T) {
	recent := &fakeRecent{out: []trigger.Trigger{
		{EventID: "$t2", Timestamp: 200, RoomID: "!r:test", Body: "b"},
		{EventID: "$t1", Timestamp: 100, RoomID: "!r:test", Body: "a"},
	}}
	srv := New(&fakeMatrix{}, &fakeProber{}, recent, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "get_recent_triggers",
		Arguments: map[string]any{"since_timestamp": 50, "limit": 500},
	})
	if err != nil {
		t.Fatalf("get_recent_triggers: %v", err)
	}
	triggers := decodeStructured[[]triggerResult](t, res)
	if len(triggers) != 2 || triggers[0].EventID != "$t2" || triggers[1].EventID != "$t1" {
		t.Fatalf("get_recent_triggers = %+v, want [$t2 $t1]", triggers)
	}
	since, limit := recent.lastCall()
	if since != 50 || limit != 100 {
		t.Fatalf("reader got since=%d limit=%d, want since=50 limit=100 (clamped)", since, limit)
	}
}

func TestSendMessageSuccess(t *testing.T) {
	svc := &fakeMatrix{sentID: "$sent"}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!ok:test"})
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "send_message",
		Arguments: map[string]any{"room_id": "!ok:test", "text": "hello"},
	})
	if err != nil {
		t.Fatalf("send_message: %v", err)
	}
	if res.IsError {
		t.Fatalf("send_message returned error: %s", resultErrorText(t, res))
	}
	got := decodeStructured[sendMessageResult](t, res)
	if got.EventID != "$sent" {
		t.Fatalf("send_message event id = %q, want $sent", got.EventID)
	}
	if len(svc.sentRoom) != 1 || svc.sentRoom[0] != "!ok:test" || len(svc.sentText) != 1 || svc.sentText[0] != "hello" {
		t.Fatalf("matrix saw sends room=%v text=%v, want [!ok:test] [hello]", svc.sentRoom, svc.sentText)
	}
}

func TestSendMessageRejections(t *testing.T) {
	t.Run("empty allowlist", func(t *testing.T) {
		svc := &fakeMatrix{sentID: "$sent"}
		srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
		session, cleanup := connect(t, srv)
		defer cleanup()

		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "send_message",
			Arguments: map[string]any{"room_id": "!ok:test", "text": "hello"},
		})
		if err != nil {
			t.Fatalf("send_message: %v", err)
		}
		if text := resultErrorText(t, res); !strings.Contains(text, "read-only") {
			t.Fatalf("empty allowlist error = %q, want read-only message", text)
		}
		if len(svc.sentRoom) != 0 {
			t.Fatalf("empty allowlist still reached matrix: %v", svc.sentRoom)
		}
	})

	t.Run("non allowlisted room", func(t *testing.T) {
		svc := &fakeMatrix{sentID: "$sent"}
		srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!ok:test"})
		session, cleanup := connect(t, srv)
		defer cleanup()

		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "send_message",
			Arguments: map[string]any{"room_id": "!other:test", "text": "hello"},
		})
		if err != nil {
			t.Fatalf("send_message: %v", err)
		}
		if text := resultErrorText(t, res); !strings.Contains(text, "not allowed") {
			t.Fatalf("non-allowlisted error = %q, want not-allowed message", text)
		}
		if len(svc.sentRoom) != 0 {
			t.Fatalf("non-allowlisted room still reached matrix: %v", svc.sentRoom)
		}
	})

	t.Run("empty and whitespace text", func(t *testing.T) {
		svc := &fakeMatrix{sentID: "$sent"}
		srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!ok:test"})
		session, cleanup := connect(t, srv)
		defer cleanup()

		for _, text := range []string{"", "   ", "\t\n"} {
			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "send_message",
				Arguments: map[string]any{"room_id": "!ok:test", "text": text},
			})
			if err != nil {
				t.Fatalf("send_message(%q): %v", text, err)
			}
			if got := resultErrorText(t, res); !strings.Contains(got, "empty") {
				t.Fatalf("whitespace text error = %q, want empty-text message", got)
			}
		}
		if len(svc.sentRoom) != 0 {
			t.Fatalf("whitespace text still reached matrix: %v", svc.sentRoom)
		}
	})

	t.Run("rate limited on immediate second send", func(t *testing.T) {
		svc := &fakeMatrix{sentID: "$sent"}
		srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!ok:test"})
		session, cleanup := connect(t, srv)
		defer cleanup()

		first, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "send_message",
			Arguments: map[string]any{"room_id": "!ok:test", "text": "one"},
		})
		if err != nil {
			t.Fatalf("first send: %v", err)
		}
		if first.IsError {
			t.Fatalf("first send rejected: %s", resultErrorText(t, first))
		}
		second, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "send_message",
			Arguments: map[string]any{"room_id": "!ok:test", "text": "two"},
		})
		if err != nil {
			t.Fatalf("second send: %v", err)
		}
		if text := resultErrorText(t, second); !strings.Contains(text, "rate limited") {
			t.Fatalf("second send error = %q, want rate-limited", text)
		}
		if len(svc.sentText) != 1 {
			t.Fatalf("matrix saw %d sends, want exactly 1 (the second was rate-limited)", len(svc.sentText))
		}
	})

	t.Run("rejected send does not consume token", func(t *testing.T) {
		svc := &fakeMatrix{sentID: "$sent"}
		srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!ok:test"})
		session, cleanup := connect(t, srv)
		defer cleanup()

		rejected, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "send_message",
			Arguments: map[string]any{"room_id": "!other:test", "text": "nope"},
		})
		if err != nil {
			t.Fatalf("rejected send: %v", err)
		}
		if !rejected.IsError {
			t.Fatal("rejected send was not an error")
		}
		allowed, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "send_message",
			Arguments: map[string]any{"room_id": "!ok:test", "text": "ok"},
		})
		if err != nil {
			t.Fatalf("allowed send after rejection: %v", err)
		}
		if allowed.IsError {
			t.Fatalf("allowed send after rejection was rejected: %s", resultErrorText(t, allowed))
		}
		if len(svc.sentRoom) != 1 || svc.sentRoom[0] != "!ok:test" {
			t.Fatalf("matrix saw sends %v, want exactly one allowed send", svc.sentRoom)
		}
	})
}

// The gate tests use an injected clock so the rate limiter is deterministic
// and never sleeps.
func TestSendGateRateLimitWithInjectedClock(t *testing.T) {
	g := newSendGate([]string{"!ok:test"}, time.Second)
	now := time.Unix(1000, 0)
	g.now = func() time.Time { return now }

	if err := g.check("!ok:test"); err != nil {
		t.Fatalf("first send: %v", err)
	}
	if err := g.check("!ok:test"); err == nil {
		t.Fatal("second immediate send succeeded, want rate limited")
	} else if !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("second send error = %v, want rate limited", err)
	}
	now = now.Add(time.Second)
	if err := g.check("!ok:test"); err != nil {
		t.Fatalf("send after interval: %v", err)
	}
}

func TestSendGateRejectedSendDoesNotConsumeToken(t *testing.T) {
	g := newSendGate([]string{"!ok:test"}, time.Second)
	now := time.Unix(1000, 0)
	g.now = func() time.Time { return now }

	if err := g.check("!other:test"); err == nil {
		t.Fatal("non-allowlisted send succeeded")
	}
	// The rejection must not have consumed the token: an immediate allowed
	// send still succeeds.
	if err := g.check("!ok:test"); err != nil {
		t.Fatalf("allowed send after rejected send: %v", err)
	}
	// And the token it just consumed is now held.
	if err := g.check("!ok:test"); err == nil {
		t.Fatal("third send succeeded, want rate limited")
	}
}

func TestSendGateEmptyAllowlistRejects(t *testing.T) {
	g := newSendGate(nil, time.Second)
	if err := g.check("!ok:test"); err == nil {
		t.Fatal("send with empty allowlist succeeded, want read-only error")
	} else if !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("empty allowlist error = %v, want read-only", err)
	}
}
