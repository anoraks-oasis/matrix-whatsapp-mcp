package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/allowlist"
	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/matrix"
	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/trigger"
)

// fakeMatrix is an in-memory MatrixService. Each method records its arguments
// so handler tests can assert what was passed through the MCP boundary. The
// recording is mutex-guarded because the idempotency test drives concurrent
// calls through one server.
type fakeMatrix struct {
	mu         sync.Mutex
	rooms      []matrix.Room
	roomsErr   error
	msgs       []matrix.Message
	msgsNext   string
	msgsMore   bool
	msgsErr    error
	msgsRoom   []string
	msgsLimit  []int
	msgsBefore []string
	sentRoom   []string
	sentText   []string
	sentReply  []string
	sentID     string
	sentErr    error
	sendDelay  time.Duration
	// sendStarted receives one value when a send begins, and sendGate makes it
	// wait there, so a test can hold a send in flight while it does something
	// else (such as advancing the idempotency clock past its TTL).
	sendStarted chan struct{}
	sendGate    chan struct{}

	searchReqs []matrix.SearchRequest
	searchOut  matrix.SearchOutcome
	searchErr  error

	ctxRoom  []string
	ctxEvent []string
	ctxBef   []int
	ctxAft   []int
	ctxOut   matrix.ContextOutcome
	ctxErr   error

	partsRoom []string
	parts     []matrix.Participant
	partsErr  error

	// blockUntilDone makes every read method wait for its context to end and
	// return the context's error, which is how the cancellation tests observe
	// that a handler neither swallows cancellation nor retries past it.
	blockUntilDone bool
}

// waitCtx reports a cancelled context and, when blockUntilDone is set, waits
// for one. Every read method starts with it.
func (f *fakeMatrix) waitCtx(ctx context.Context) error {
	if f.blockUntilDone {
		<-ctx.Done()
		return ctx.Err()
	}
	return ctx.Err()
}

func (f *fakeMatrix) JoinedRooms(context.Context) ([]matrix.Room, error) {
	return f.rooms, f.roomsErr
}

func (f *fakeMatrix) Messages(_ context.Context, roomID string, limit int, before string) (matrix.MessagePage, error) {
	f.mu.Lock()
	f.msgsRoom = append(f.msgsRoom, roomID)
	f.msgsLimit = append(f.msgsLimit, limit)
	f.msgsBefore = append(f.msgsBefore, before)
	page := matrix.MessagePage{Messages: f.msgs, NextToken: f.msgsNext, HasMore: f.msgsMore}
	err := f.msgsErr
	f.mu.Unlock()
	return page, err
}

func (f *fakeMatrix) SendText(ctx context.Context, roomID, text, replyTo string) (string, error) {
	if f.sendStarted != nil {
		select {
		case f.sendStarted <- struct{}{}:
		default:
		}
	}
	if f.sendGate != nil {
		select {
		case <-f.sendGate:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if f.sendDelay > 0 {
		time.Sleep(f.sendDelay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sentRoom = append(f.sentRoom, roomID)
	f.sentText = append(f.sentText, text)
	f.sentReply = append(f.sentReply, replyTo)
	return f.sentID, f.sentErr
}

// sends returns copies of what SendText recorded, for assertions made after the
// calls have returned.
func (f *fakeMatrix) sends() (rooms, texts, replies []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sentRoom...), append([]string(nil), f.sentText...), append([]string(nil), f.sentReply...)
}

func (f *fakeMatrix) Sync(context.Context) error { return nil }
func (f *fakeMatrix) Close() error               { return nil }

func (f *fakeMatrix) SearchMessages(ctx context.Context, req matrix.SearchRequest) (matrix.SearchOutcome, error) {
	if err := f.waitCtx(ctx); err != nil {
		return matrix.SearchOutcome{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchReqs = append(f.searchReqs, req)
	return f.searchOut, f.searchErr
}

func (f *fakeMatrix) searchCalls() []matrix.SearchRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]matrix.SearchRequest(nil), f.searchReqs...)
}

func (f *fakeMatrix) MessageContext(ctx context.Context, roomID, eventID string, before, after int) (matrix.ContextOutcome, error) {
	if err := f.waitCtx(ctx); err != nil {
		return matrix.ContextOutcome{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctxRoom = append(f.ctxRoom, roomID)
	f.ctxEvent = append(f.ctxEvent, eventID)
	f.ctxBef = append(f.ctxBef, before)
	f.ctxAft = append(f.ctxAft, after)
	return f.ctxOut, f.ctxErr
}

func (f *fakeMatrix) contextCalls() (before, after []int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.ctxBef...), append([]int(nil), f.ctxAft...)
}

func (f *fakeMatrix) Participants(ctx context.Context, roomID string) ([]matrix.Participant, error) {
	if err := f.waitCtx(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.partsRoom = append(f.partsRoom, roomID)
	return f.parts, f.partsErr
}

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

// nullableInteger reports whether a schema property's type accepts an integer
// and the absence of a value, which is the shape an optional *int input gets.
func nullableInteger(typ any) bool {
	switch v := typ.(type) {
	case string:
		return v == "integer"
	case []any:
		integer, null := false, false
		for _, entry := range v {
			switch entry {
			case "integer":
				integer = true
			case "null":
				null = true
			}
		}
		return integer && null
	}
	return false
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

func TestToolsListHasSevenToolsWithSchemas(t *testing.T) {
	srv := New(&fakeMatrix{}, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(res.Tools) != 7 {
		t.Fatalf("ListTools returned %d tools, want 7", len(res.Tools))
	}
	names := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		names[tool.Name] = true
	}
	for _, want := range []string{
		"list_rooms", "read_messages", "search_messages", "get_message_context",
		"list_participants", "get_recent_triggers", "send_message",
	} {
		if !names[want] {
			t.Fatalf("ListTools missing %q (got %v)", want, names)
		}
	}

	listRooms := schemaMap(t, toolByName(t, res.Tools, "list_rooms"))
	if len(requiredFields(t, listRooms)) != 0 {
		t.Fatalf("list_rooms required = %v, want none", requiredFields(t, listRooms))
	}

	readTool := toolByName(t, res.Tools, "read_messages")
	read := schemaMap(t, readTool)
	hasField(t, read, "room_id")
	// read_messages must describe the type of every message it returns, so a
	// media event is never a blank body with no hint about what it was, and
	// every relation that makes a reply, an edit or a deletion readable.
	for _, field := range []string{"msgtype", "encrypted", "body", "reply_to", "edit_of", "mentions", "redacted"} {
		if _, ok := messageProperties(t, readTool)[field]; !ok {
			t.Fatalf("read_messages message schema has no %q property", field)
		}
	}
	// Pagination lives in the output envelope, next to the messages it pages.
	for _, field := range []string{"messages", "next_token", "has_more"} {
		if _, ok := envelopeProperties(t, readTool)[field]; !ok {
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
	before := readProps["before"].(map[string]any)
	if before["type"] != "string" {
		t.Fatalf("read_messages before type = %v, want string", before["type"])
	}
	if desc, _ := before["description"].(string); !strings.Contains(desc, "next_token") {
		t.Fatalf("read_messages before description = %q, want it to name next_token", desc)
	}

	// search_messages: one required query, an optional room scope, limit and
	// date bound, and a result envelope that says how partial the answer is.
	searchTool := toolByName(t, res.Tools, "search_messages")
	search := schemaMap(t, searchTool)
	if req := requiredFields(t, search); len(req) != 1 || req[0] != "query" {
		t.Fatalf("search_messages required = %v, want [query]", req)
	}
	searchProps := search["properties"].(map[string]any)
	query, ok := searchProps["query"].(map[string]any)
	if !ok || query["type"] != "string" {
		t.Fatalf("search_messages query = %v, want a string", searchProps["query"])
	}
	if desc, _ := query["description"].(string); !strings.Contains(desc, "512") {
		t.Fatalf("search_messages query description = %q, want the 512-character cap", desc)
	}
	for _, field := range []string{"room_id", "limit", "since_timestamp"} {
		prop, ok := searchProps[field].(map[string]any)
		if !ok {
			t.Fatalf("search_messages schema has no %q property", field)
		}
		if desc, _ := prop["description"].(string); desc == "" {
			t.Fatalf("search_messages %s has no description", field)
		}
	}
	searchLimit := searchProps["limit"].(map[string]any)
	if searchLimit["type"] != "integer" {
		t.Fatalf("search_messages limit type = %v, want integer", searchLimit["type"])
	}
	if desc, _ := searchLimit["description"].(string); !strings.Contains(desc, "default 20") || !strings.Contains(desc, "100") {
		t.Fatalf("search_messages limit description = %q, want default 20 and max 100", desc)
	}
	if since := searchProps["since_timestamp"].(map[string]any); since["type"] != "integer" {
		t.Fatalf("search_messages since_timestamp type = %v, want integer", since["type"])
	}
	// The budget in the description must be the budget the code enforces: both
	// numbers come from the same constants.
	searchDesc := searchTool.Description
	for _, want := range []string{
		fmt.Sprint(matrix.SearchEventsPerRoom),
		fmt.Sprint(matrix.SearchMaxRooms),
		fmt.Sprint(matrix.SearchMaxRooms * matrix.SearchEventsPerRoom),
		"truncated",
	} {
		if !strings.Contains(searchDesc, want) {
			t.Fatalf("search_messages description = %q, want it to state %q", searchDesc, want)
		}
	}
	searchOut := envelopeProperties(t, searchTool)
	for _, field := range []string{"results", "truncated", "scanned_rooms", "scanned_events", "source"} {
		if _, ok := searchOut[field]; !ok {
			t.Fatalf("search_messages output schema has no %q property", field)
		}
	}
	hitItems, ok := searchOut["results"].(map[string]any)["items"].(map[string]any)
	if !ok {
		t.Fatalf("search_messages results schema has no items object")
	}
	hitProps := hitItems["properties"].(map[string]any)
	for _, field := range []string{
		"event_id", "room_id", "sender", "body", "msgtype", "timestamp", "encrypted",
		"reply_to", "edit_of", "mentions", "redacted", "room_name",
	} {
		if _, ok := hitProps[field]; !ok {
			t.Fatalf("search_messages result has no %q property (a match carries the whole message plus its room)", field)
		}
	}

	// get_message_context: room and event required, both limits optional so an
	// explicit 0 can mean "none".
	contextTool := toolByName(t, res.Tools, "get_message_context")
	contextSchema := schemaMap(t, contextTool)
	contextReq := requiredFields(t, contextSchema)
	if len(contextReq) != 2 || contextReq[0] != "room_id" || contextReq[1] != "event_id" {
		t.Fatalf("get_message_context required = %v, want [room_id event_id]", contextReq)
	}
	contextProps := contextSchema["properties"].(map[string]any)
	for _, field := range []string{"limit_before", "limit_after"} {
		prop, ok := contextProps[field].(map[string]any)
		if !ok {
			t.Fatalf("get_message_context schema has no %q property", field)
		}
		if desc, _ := prop["description"].(string); !strings.Contains(desc, "default 10") || !strings.Contains(desc, "0..50") {
			t.Fatalf("get_message_context %s description = %q, want the default and clamp", field, desc)
		}
		if !nullableInteger(prop["type"]) {
			t.Fatalf("get_message_context %s type = %v, want an optional integer", field, prop["type"])
		}
	}
	contextOut := envelopeProperties(t, contextTool)
	for _, field := range []string{"events_before", "event", "events_after", "reply_to", "edit_of", "source", "truncated"} {
		if _, ok := contextOut[field]; !ok {
			t.Fatalf("get_message_context output schema has no %q property", field)
		}
	}
	target, ok := contextOut["event"].(map[string]any)
	if !ok {
		t.Fatalf("get_message_context event schema is %T, want an object", contextOut["event"])
	}
	if _, ok := target["properties"].(map[string]any)["event_id"]; !ok {
		t.Fatal("get_message_context event schema carries no event_id: the target must use the message shape")
	}

	// list_participants: one required room, and every participant field in the
	// output schema.
	participantsTool := toolByName(t, res.Tools, "list_participants")
	participantsSchema := schemaMap(t, participantsTool)
	if req := requiredFields(t, participantsSchema); len(req) != 1 || req[0] != "room_id" {
		t.Fatalf("list_participants required = %v, want [room_id]", req)
	}
	participantsOut := envelopeProperties(t, participantsTool)
	pItems, ok := participantsOut["participants"].(map[string]any)["items"].(map[string]any)
	if !ok {
		t.Fatalf("list_participants output schema has no participants items object")
	}
	pProps := pItems["properties"].(map[string]any)
	for _, field := range []string{"user_id", "display_name", "membership", "bridged", "bridge_kind"} {
		if _, ok := pProps[field]; !ok {
			t.Fatalf("list_participants participant schema has no %q property", field)
		}
	}
	if desc, _ := pProps["bridge_kind"].(map[string]any)["description"].(string); !strings.Contains(desc, "whatsapp_contact") || !strings.Contains(desc, "whatsapp_bot") {
		t.Fatalf("bridge_kind description = %q, want it to name both bridge kinds", desc)
	}

	recent := schemaMap(t, toolByName(t, res.Tools, "get_recent_triggers"))
	if len(requiredFields(t, recent)) != 0 {
		t.Fatalf("get_recent_triggers required = %v, want none", requiredFields(t, recent))
	}

	sendTool := toolByName(t, res.Tools, "send_message")
	send := schemaMap(t, sendTool)
	req := requiredFields(t, send)
	if len(req) != 2 || req[0] != "room_id" || req[1] != "text" {
		t.Fatalf("send_message required = %v, want [room_id text]", req)
	}
	sendProps := send["properties"].(map[string]any)
	for _, field := range []string{"reply_to", "idempotency_key"} {
		prop, ok := sendProps[field].(map[string]any)
		if !ok {
			t.Fatalf("send_message schema has no %q property", field)
		}
		if prop["type"] != "string" {
			t.Fatalf("send_message %s type = %v, want string", field, prop["type"])
		}
		if desc, _ := prop["description"].(string); desc == "" {
			t.Fatalf("send_message %s has no description", field)
		}
	}
	if key := sendProps["idempotency_key"].(map[string]any); !strings.Contains(key["description"].(string), "128") {
		t.Fatalf("idempotency_key description = %q, want it to document the 128-character cap", key["description"])
	}
	sendOut := envelopeProperties(t, sendTool)
	for _, field := range []string{"event_id", "deduplicated"} {
		if _, ok := sendOut[field]; !ok {
			t.Fatalf("send_message output schema has no %q property", field)
		}
	}

	// Annotations are part of the tool contract: readOnlyHint keeps a client
	// from treating a read as a write, and destructiveHint/idempotentHint on
	// send_message keep it from retrying a send that is neither.
	for _, name := range []string{"list_rooms", "read_messages", "search_messages", "get_message_context", "list_participants", "get_recent_triggers"} {
		ann := toolByName(t, res.Tools, name).Annotations
		if ann == nil {
			t.Fatalf("%s has no annotations, want readOnlyHint true", name)
		}
		if !ann.ReadOnlyHint {
			t.Fatalf("%s readOnlyHint = false, want true", name)
		}
		if ann.DestructiveHint != nil && *ann.DestructiveHint {
			t.Fatalf("%s claims destructiveHint, want it unset", name)
		}
	}

	ann := sendTool.Annotations
	if ann == nil {
		t.Fatal("send_message has no annotations")
	}
	if ann.DestructiveHint == nil || !*ann.DestructiveHint {
		t.Fatalf("send_message destructiveHint = %v, want true", ann.DestructiveHint)
	}
	if ann.IdempotentHint {
		t.Fatal("send_message idempotentHint = true, want false (only a keyed retry is idempotent)")
	}
	if ann.ReadOnlyHint {
		t.Fatal("send_message readOnlyHint = true, want false")
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
// is an array schema for list_rooms and get_recent_triggers.
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

// envelopeProperties returns the properties of a tool's object-shaped output
// (read_messages and send_message).
func envelopeProperties(t *testing.T, tool *mcp.Tool) map[string]any {
	t.Helper()
	schema, ok := tool.OutputSchema.(map[string]any)
	if !ok {
		t.Fatalf("tool %q OutputSchema is %T, want map[string]any", tool.Name, tool.OutputSchema)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("tool %q output schema has no properties: %v", tool.Name, schema)
	}
	return props
}

// messageProperties returns the schema of one message inside read_messages'
// output envelope, so a field cannot be dropped from the message shape without
// failing the tools/list test.
func messageProperties(t *testing.T, tool *mcp.Tool) map[string]any {
	t.Helper()
	messages, ok := envelopeProperties(t, tool)["messages"].(map[string]any)
	if !ok {
		t.Fatalf("tool %q output schema has no messages object", tool.Name)
	}
	items, ok := messages["items"].(map[string]any)
	if !ok {
		t.Fatalf("tool %q messages schema has no items object: %v", tool.Name, messages)
	}
	props, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("tool %q message schema has no properties: %v", tool.Name, items)
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
	msgs := decodeStructured[readMessagesResult](t, res).Messages
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
	msgs := decodeStructured[readMessagesResult](t, res).Messages
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

// TestSendGateFollowsAllowlistFile proves the live allowlist: a room enrolled
// by writing the file (which is what `rooms allow` does) becomes sendable in
// the running server, and removing it revokes that, with no restart.
func TestSendGateFollowsAllowlistFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allow.txt")
	store := allowlist.New(nil, path, log.New(io.Discard, "", 0), allowlist.WithMinInterval(0))
	svc := &fakeMatrix{sentID: "$sent"}
	srv := NewWithAllowlist(svc, &fakeProber{}, &fakeRecent{}, store)
	session, cleanup := connect(t, srv)
	defer cleanup()

	send := func(t *testing.T) *mcp.CallToolResult {
		t.Helper()
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "send_message",
			Arguments: map[string]any{"room_id": "!enrolled:test", "text": "hello"},
		})
		if err != nil {
			t.Fatalf("send_message: %v", err)
		}
		return res
	}

	res := send(t)
	if text := resultErrorText(t, res); !strings.Contains(text, "read-only") {
		t.Fatalf("send before enrollment = %q, want a read-only error", text)
	}

	if _, err := allowlist.Add(path, "!enrolled:test"); err != nil {
		t.Fatalf("allowlist.Add: %v", err)
	}
	res = send(t)
	if res.IsError {
		t.Fatalf("send after enrollment = %q, want success", resultErrorText(t, res))
	}
	if got := decodeStructured[sendMessageResult](t, res); got.EventID != "$sent" {
		t.Fatalf("send after enrollment returned %q, want $sent", got.EventID)
	}

	if _, err := allowlist.Remove(path, "!enrolled:test"); err != nil {
		t.Fatalf("allowlist.Remove: %v", err)
	}
	res = send(t)
	if !res.IsError || !strings.Contains(resultErrorText(t, res), "read-only") {
		t.Fatalf("send after denial = %+v, want a read-only error", res)
	}
}

// TestSendGateUnionOfEnvAndFile keeps ALLOWED_SEND_ROOMS working alongside the
// file: neither source alone decides, and a room present in both is allowed.
func TestSendGateUnionOfEnvAndFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "allow.txt")
	if _, err := allowlist.Add(path, "!file:test"); err != nil {
		t.Fatalf("allowlist.Add: %v", err)
	}
	store := allowlist.New([]string{"!env:test"}, path, log.New(io.Discard, "", 0), allowlist.WithMinInterval(0))
	gate := newSendGateWith(store, 0)

	for _, room := range []string{"!env:test", "!file:test"} {
		if err := gate.check(room); err != nil {
			t.Fatalf("check(%q) = %v, want nil", room, err)
		}
	}
	if err := gate.check("!other:test"); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("check of an unlisted room = %v, want a not-allowed error", err)
	}
}

// TestReadMessagesPagination proves the tool pages history: the envelope
// carries has_more and next_token, and the next call's before reaches the
// Matrix layer untouched. Without a token the call still starts at the newest
// message.
func TestReadMessagesPagination(t *testing.T) {
	svc := &fakeMatrix{
		msgs:     []matrix.Message{{EventID: "$2", RoomID: "!r:test", Body: "new", Timestamp: 200}},
		msgsNext: "token-1",
		msgsMore: true,
	}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, nil)
	session, cleanup := connect(t, srv)
	defer cleanup()

	call := func(t *testing.T, args map[string]any) readMessagesResult {
		t.Helper()
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_messages", Arguments: args})
		if err != nil {
			t.Fatalf("read_messages: %v", err)
		}
		if res.IsError {
			t.Fatalf("read_messages returned error: %s", resultErrorText(t, res))
		}
		return decodeStructured[readMessagesResult](t, res)
	}

	first := call(t, map[string]any{"room_id": "!r:test"})
	if first.NextToken != "token-1" || !first.HasMore {
		t.Fatalf("first page = next_token %q, has_more %t; want the fake's token and more", first.NextToken, first.HasMore)
	}
	if len(first.Messages) != 1 || first.Messages[0].EventID != "$2" {
		t.Fatalf("first page messages = %+v, want [$2]", first.Messages)
	}

	second := call(t, map[string]any{"room_id": "!r:test", "before": first.NextToken})
	if second.NextToken != "token-1" {
		t.Fatalf("second page next_token = %q, want the fake's token passed through", second.NextToken)
	}

	if len(svc.msgsBefore) != 2 || svc.msgsBefore[0] != "" || svc.msgsBefore[1] != "token-1" {
		t.Fatalf("matrix saw before tokens %q, want [\"\" token-1]", svc.msgsBefore)
	}
}

// TestReadMessagesRelations proves the relation fields cross the MCP boundary:
// replies keep their parent, edits are marked rather than dropped, mentions are
// listed, and a redacted message stays in the page with no body.
func TestReadMessagesRelations(t *testing.T) {
	svc := &fakeMatrix{msgs: []matrix.Message{
		{EventID: "$edit", RoomID: "!r:test", Body: "* fixed", MsgType: "m.text", Timestamp: 300, EditOf: "$orig"},
		{EventID: "$gone", RoomID: "!r:test", MsgType: "", Timestamp: 200, Redacted: true},
		{EventID: "$reply", RoomID: "!r:test", Body: "yes", MsgType: "m.text", Timestamp: 100,
			ReplyTo: "$parent", Mentions: []string{"@bot:test"}},
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
	msgs := decodeStructured[readMessagesResult](t, res).Messages
	if len(msgs) != 3 {
		t.Fatalf("read_messages returned %d messages, want 3 (a redacted one is not dropped)", len(msgs))
	}
	// Oldest first: the reply, then the redacted message, then the edit.
	if msgs[0].EventID != "$reply" || msgs[0].ReplyTo != "$parent" || strings.Join(msgs[0].Mentions, ",") != "@bot:test" {
		t.Fatalf("msgs[0] = %+v, want the reply with its parent and mention", msgs[0])
	}
	if msgs[1].EventID != "$gone" || !msgs[1].Redacted || msgs[1].Body != "" {
		t.Fatalf("msgs[1] = %+v, want the redacted message with no body", msgs[1])
	}
	if msgs[2].EventID != "$edit" || msgs[2].EditOf != "$orig" || msgs[2].Body != "* fixed" {
		t.Fatalf("msgs[2] = %+v, want the edit marked with edit_of and kept", msgs[2])
	}
}

// TestReadMessagesErrorSurfaces proves a Matrix failure is reported as a tool
// error rather than an empty page.
func TestReadMessagesErrorSurfaces(t *testing.T) {
	svc := &fakeMatrix{msgsErr: errors.New("boom from the homeserver")}
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
	if text := resultErrorText(t, res); !strings.Contains(text, "boom") {
		t.Fatalf("read_messages error = %q, want the Matrix failure", text)
	}
}

func TestSendMessageReplyThreading(t *testing.T) {
	svc := &fakeMatrix{sentID: "$sent"}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!ok:test"})
	session, cleanup := connect(t, srv)
	defer cleanup()

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "send_message",
		Arguments: map[string]any{"room_id": "!ok:test", "text": "hello", "reply_to": "$parent"},
	})
	if err != nil {
		t.Fatalf("send_message: %v", err)
	}
	if res.IsError {
		t.Fatalf("send_message returned error: %s", resultErrorText(t, res))
	}
	if got := decodeStructured[sendMessageResult](t, res); got.EventID != "$sent" || got.Deduplicated {
		t.Fatalf("send_message = %+v, want $sent and not deduplicated", got)
	}
	rooms, texts, replies := svc.sends()
	if len(rooms) != 1 || rooms[0] != "!ok:test" || texts[0] != "hello" || replies[0] != "$parent" {
		t.Fatalf("matrix saw rooms=%v texts=%v replies=%v, want one reply to $parent", rooms, texts, replies)
	}
}

// TestSendMessageRejectsMalformedReply covers the local check: a malformed
// reply target is refused before anything is sent and before the rate-limit
// token is spent, so the corrected call goes through immediately.
func TestSendMessageRejectsMalformedReply(t *testing.T) {
	svc := &fakeMatrix{sentID: "$sent"}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!ok:test"})
	session, cleanup := connect(t, srv)
	defer cleanup()

	for _, bad := range []string{"not-an-event", "abc", "$", "$a b"} {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "send_message",
			Arguments: map[string]any{"room_id": "!ok:test", "text": "hello", "reply_to": bad},
		})
		if err != nil {
			t.Fatalf("send_message(reply_to=%q): %v", bad, err)
		}
		if text := resultErrorText(t, res); !strings.Contains(text, "event ID") {
			t.Fatalf("malformed reply error = %q, want it to name the event ID shape", text)
		}
	}
	if rooms, _, _ := svc.sends(); len(rooms) != 0 {
		t.Fatalf("a malformed reply target reached matrix: %v", rooms)
	}

	// A rejected malformed reply must not consume the rate-limit token.
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "send_message",
		Arguments: map[string]any{"room_id": "!ok:test", "text": "hello", "reply_to": "$parent"},
	})
	if err != nil {
		t.Fatalf("corrected send: %v", err)
	}
	if res.IsError {
		t.Fatalf("corrected send was rejected: %s", resultErrorText(t, res))
	}
}

// TestSendMessageIdempotency covers the retry contract: an identical call
// replays the first event ID without a second send, while a different message
// under the same key is an error rather than a second post.
func TestSendMessageIdempotency(t *testing.T) {
	t.Run("same key and text replays the original", func(t *testing.T) {
		svc := &fakeMatrix{sentID: "$first"}
		srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!ok:test"})
		session, cleanup := connect(t, srv)
		defer cleanup()

		send := func(t *testing.T) sendMessageResult {
			t.Helper()
			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "send_message",
				Arguments: map[string]any{"room_id": "!ok:test", "text": "hello", "idempotency_key": "k1"},
			})
			if err != nil {
				t.Fatalf("send_message: %v", err)
			}
			if res.IsError {
				t.Fatalf("send_message returned error: %s", resultErrorText(t, res))
			}
			return decodeStructured[sendMessageResult](t, res)
		}

		first := send(t)
		if first.EventID != "$first" || first.Deduplicated {
			t.Fatalf("first send = %+v, want the new event and deduplicated false", first)
		}
		// The retry must not need a rate-limit token either, which is why the
		// dedup lookup happens before the gate.
		second := send(t)
		if second.EventID != "$first" || !second.Deduplicated {
			t.Fatalf("retry = %+v, want the original $first with deduplicated true", second)
		}
		if _, texts, _ := svc.sends(); len(texts) != 1 {
			t.Fatalf("matrix saw %d sends, want exactly 1 (the retry was deduplicated)", len(texts))
		}
	})

	t.Run("same key with different text is an error", func(t *testing.T) {
		svc := &fakeMatrix{sentID: "$first"}
		srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!ok:test"})
		session, cleanup := connect(t, srv)
		defer cleanup()

		if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "send_message",
			Arguments: map[string]any{"room_id": "!ok:test", "text": "hello", "idempotency_key": "k1"},
		}); err != nil {
			t.Fatalf("first send: %v", err)
		}
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "send_message",
			Arguments: map[string]any{"room_id": "!ok:test", "text": "something else", "idempotency_key": "k1"},
		})
		if err != nil {
			t.Fatalf("second send: %v", err)
		}
		if text := resultErrorText(t, res); !strings.Contains(text, "idempotency key already used") {
			t.Fatalf("conflict error = %q, want it to name the reused key", text)
		}
		if _, texts, _ := svc.sends(); len(texts) != 1 {
			t.Fatalf("matrix saw %d sends, want exactly 1", len(texts))
		}
	})

	t.Run("same key with a different room is an error", func(t *testing.T) {
		svc := &fakeMatrix{sentID: "$first"}
		srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!ok:test", "!ok2:test"})
		session, cleanup := connect(t, srv)
		defer cleanup()

		if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "send_message",
			Arguments: map[string]any{"room_id": "!ok:test", "text": "hello", "idempotency_key": "k1"},
		}); err != nil {
			t.Fatalf("first send: %v", err)
		}
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "send_message",
			Arguments: map[string]any{"room_id": "!ok2:test", "text": "hello", "idempotency_key": "k1"},
		})
		if err != nil {
			t.Fatalf("second send: %v", err)
		}
		if text := resultErrorText(t, res); !strings.Contains(text, "idempotency key already used") {
			t.Fatalf("conflict error = %q, want it to name the reused key", text)
		}
		if rooms, _, _ := svc.sends(); len(rooms) != 1 {
			t.Fatalf("matrix saw %d sends, want exactly 1", len(rooms))
		}
	})

	t.Run("over-long key is rejected", func(t *testing.T) {
		svc := &fakeMatrix{sentID: "$first"}
		srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!ok:test"})
		session, cleanup := connect(t, srv)
		defer cleanup()

		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name: "send_message",
			Arguments: map[string]any{"room_id": "!ok:test", "text": "hello",
				"idempotency_key": strings.Repeat("k", maxIdempotencyKeyLen+1)},
		})
		if err != nil {
			t.Fatalf("send_message: %v", err)
		}
		if text := resultErrorText(t, res); !strings.Contains(text, "128") {
			t.Fatalf("over-long key error = %q, want it to name the 128-character cap", text)
		}
		if rooms, _, _ := svc.sends(); len(rooms) != 0 {
			t.Fatalf("an over-long key still reached matrix: %v", rooms)
		}
	})
}

// TestSendMessageIdempotencyConcurrent proves the key is claimed atomically: N
// simultaneous retries of one message produce exactly one send, and every
// caller gets that send's event ID. Run under -race, it also proves the cache
// itself is race-free.
func TestSendMessageIdempotencyConcurrent(t *testing.T) {
	svc := &fakeMatrix{sentID: "$once", sendDelay: 20 * time.Millisecond}
	srv := New(svc, &fakeProber{}, &fakeRecent{}, []string{"!ok:test"})
	session, cleanup := connect(t, srv)
	defer cleanup()

	const callers = 8
	var wg sync.WaitGroup
	results := make([]sendMessageResult, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "send_message",
				Arguments: map[string]any{"room_id": "!ok:test", "text": "hello", "idempotency_key": "same-key"},
			})
			if err != nil {
				errs[i] = err
				return
			}
			if res.IsError {
				errs[i] = errors.New("tool error")
				return
			}
			results[i] = decodeStructured[sendMessageResult](t, res)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	fresh := 0
	for i, got := range results {
		if got.EventID != "$once" {
			t.Fatalf("caller %d got event %q, want the one send's $once", i, got.EventID)
		}
		if !got.Deduplicated {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d callers saw a fresh send, want exactly 1", fresh)
	}
	if _, texts, _ := svc.sends(); len(texts) != 1 {
		t.Fatalf("matrix saw %d sends, want exactly 1 for 8 concurrent retries", len(texts))
	}
}

// TestSendMessageIdempotencyInFlightPastTTL proves R1's invariant end to end: a
// send that is still in flight when the idempotency TTL elapses keeps its key,
// so the retry waits for it and replays its event instead of posting the same
// message to the homeserver a second time.
func TestSendMessageIdempotencyInFlightPastTTL(t *testing.T) {
	cache, advance := testCache(idempotencyTTL, 8)
	gate := make(chan struct{})
	started := make(chan struct{}, 1)
	svc := &fakeMatrix{sentID: "$once", sendStarted: started, sendGate: gate}
	srv := newServer(svc, &fakeProber{}, &fakeRecent{}, allowlist.FromSlice([]string{"!ok:test"}), cache)
	session, cleanup := connect(t, srv)
	defer cleanup()

	send := func() (sendMessageResult, error) {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "send_message",
			Arguments: map[string]any{"room_id": "!ok:test", "text": "hello", "idempotency_key": "k"},
		})
		if err != nil {
			return sendMessageResult{}, err
		}
		if res.IsError {
			return sendMessageResult{}, errors.New(resultErrorText(t, res))
		}
		return decodeStructured[sendMessageResult](t, res), nil
	}

	type outcome struct {
		result sendMessageResult
		err    error
	}
	first := make(chan outcome, 1)
	go func() {
		result, err := send()
		first <- outcome{result, err}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		close(gate)
		t.Fatal("the first send never reached the homeserver")
	}

	// The send is now in flight and its recorded entry is older than the TTL.
	advance(idempotencyTTL + time.Second)

	second := make(chan outcome, 1)
	go func() {
		result, err := send()
		second <- outcome{result, err}
	}()

	select {
	case got := <-second:
		close(gate)
		t.Fatalf("the retry returned %+v while the first send was still in flight, want it to wait", got.result)
	case <-time.After(100 * time.Millisecond):
	}

	close(gate)

	gotFirst := <-first
	if gotFirst.err != nil {
		t.Fatalf("first send: %v", gotFirst.err)
	}
	if gotFirst.result.EventID != "$once" || gotFirst.result.Deduplicated {
		t.Fatalf("first send = %+v, want the new event $once with deduplicated false", gotFirst.result)
	}

	gotSecond := <-second
	if gotSecond.err != nil {
		t.Fatalf("retry: %v", gotSecond.err)
	}
	if gotSecond.result.EventID != "$once" || !gotSecond.result.Deduplicated {
		t.Fatalf("retry = %+v, want $once replayed with deduplicated true", gotSecond.result)
	}

	// A later retry replays the same event too: one send, two identical replays.
	third, err := send()
	if err != nil {
		t.Fatalf("third call: %v", err)
	}
	if third.EventID != "$once" || !third.Deduplicated {
		t.Fatalf("third call = %+v, want $once replayed", third)
	}

	if _, texts, _ := svc.sends(); len(texts) != 1 {
		t.Fatalf("matrix saw %d sends, want exactly 1 for one key across the ttl boundary", len(texts))
	}
}
