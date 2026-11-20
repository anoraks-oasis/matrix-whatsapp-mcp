// Package matrix wraps mautrix behind the small MatrixService interface so
// that everything above this package can be tested against a mock without a
// live homeserver.
package matrix

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/allowlist"
	"github.com/ButterHost69/matrix-whatsapp-mcp/internal/config"
)

// Room is a joined Matrix room. Name falls back to the room ID when the room
// has no m.room.name state event, so it is never empty.
type Room struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Message is one m.room.message event. Timestamp is unix milliseconds. When
// Encrypted is true and the event could not be decrypted, Body is a fixed
// placeholder rather than ciphertext. A media event (m.image, m.video, ...)
// that carries no text body gets a bracketed type marker such as "[image]" so
// callers never see a silently blank message; see placeholderBody.
//
// ReplyTo, EditOf, Mentions and Redacted are the relations that let a client
// read the stream as a conversation. ReplyTo carries the parent event ID even
// when the parent is outside the page the caller asked for, so a reply is never
// orphaned. A redacted message keeps its place in the stream with Redacted true
// and an empty Body: the deleted text is never returned.
type Message struct {
	EventID   string   `json:"event_id"`
	RoomID    string   `json:"room_id"`
	Sender    string   `json:"sender"`
	Body      string   `json:"body"`
	MsgType   string   `json:"msgtype"`
	Timestamp int64    `json:"timestamp"`
	Encrypted bool     `json:"encrypted"`
	ReplyTo   string   `json:"reply_to,omitempty"`
	EditOf    string   `json:"edit_of,omitempty"`
	Mentions  []string `json:"mentions,omitempty"`
	Redacted  bool     `json:"redacted"`
}

// MessagePage is one page of a room's history, in the order the homeserver
// hands it out (newest first). NextToken is an opaque token for the next older
// page and is empty when the room's history is exhausted.
type MessagePage struct {
	Messages  []Message `json:"messages"`
	NextToken string    `json:"next_token,omitempty"`
	HasMore   bool      `json:"has_more"`
}

// MatrixService is the surface the MCP layer depends on.
type MatrixService interface {
	// JoinedRooms lists the rooms the account has joined, sorted by name.
	JoinedRooms(ctx context.Context) ([]Room, error)
	// Messages returns up to limit messages of a room, newest first. before is
	// an opaque token from an earlier page; an empty token starts at the
	// newest message.
	Messages(ctx context.Context, roomID string, limit int, before string) (MessagePage, error)
	// SendText sends a plain-text message and returns the new event ID. When
	// replyTo is set the message is sent as a reply to that event. It fails
	// closed with ErrSendNotAllowed unless the room is allow-listed.
	SendText(ctx context.Context, roomID, text, replyTo string) (string, error)
	// Sync runs the sync loop and blocks until ctx is cancelled (returning
	// nil) or a fatal error such as an invalid access token occurs.
	Sync(ctx context.Context) error
	// SearchMessages searches message bodies, using the homeserver's search
	// index when it has one and a bounded per-room scan when it does not.
	SearchMessages(ctx context.Context, req SearchRequest) (SearchOutcome, error)
	// MessageContext returns one event with the messages around it and its
	// reply and edit relations resolved.
	MessageContext(ctx context.Context, roomID, eventID string, before, after int) (ContextOutcome, error)
	// Participants lists the joined members of a room and marks the ones the
	// WhatsApp bridge owns.
	Participants(ctx context.Context, roomID string) ([]Participant, error)
	// Close releases resources. Cancel the Sync context first.
	Close() error
}

var (
	// ErrSendNotAllowed is returned when the room is not in ALLOWED_SEND_ROOMS.
	ErrSendNotAllowed = errors.New("matrix: sending to this room is not allowed")
	// ErrRoomEncrypted is returned when a room is encrypted but E2EE is disabled.
	ErrRoomEncrypted = errors.New("matrix: room is encrypted and MATRIX_E2EE_ENABLED is false")
	// ErrInvalidRoomID is returned for room IDs rejected by config.ValidRoomID.
	ErrInvalidRoomID = errors.New("matrix: invalid room ID")
	// ErrInvalidEventID is returned for reply targets that are not well-formed
	// Matrix event IDs.
	ErrInvalidEventID = errors.New("matrix: invalid event ID")
	// ErrEmptyMessage is returned when the text to send is empty.
	ErrEmptyMessage = errors.New("matrix: message text is empty")
	// ErrAlreadySyncing is returned when Sync is called twice concurrently.
	ErrAlreadySyncing = errors.New("matrix: sync already running")
	// ErrUserMismatch is returned when the access token belongs to a different user.
	ErrUserMismatch = errors.New("matrix: access token does not belong to MATRIX_USER_ID")
	// ErrClosed is returned by operations on a closed client.
	ErrClosed = errors.New("matrix: client closed")
	// ErrNotBridged is returned when the bridge reported that the message was
	// not forwarded, even though Matrix accepted the event.
	ErrNotBridged = errors.New("matrix: bridge did not bridge the message")
	// ErrInvalidPageToken is returned when a before token is malformed, was
	// modified, belongs to another room, or was issued by a previous process.
	ErrInvalidPageToken = errors.New("matrix: invalid page token")
	// ErrStalePageToken is returned when the homeserver rejects a token this
	// process issued, for example because the timeline moved past it.
	ErrStalePageToken = errors.New("matrix: page token is no longer valid")
	// ErrEmptyQuery is returned when a search has no search term.
	ErrEmptyQuery = errors.New("matrix: search query is empty")
	// ErrEventNotFound is returned when a well-formed event ID is not in the
	// room, including when the room itself is unknown.
	ErrEventNotFound = errors.New("matrix: event not found in room")
	// ErrNotAMessage is returned when an event exists but carries no message to
	// read the context of, such as a membership or state event.
	ErrNotAMessage = errors.New("matrix: event is not a message")
)

// BridgeNoticeError reports a message that Matrix accepted but the bridge
// refused to forward, carrying the bridge's own notice text. It matches
// ErrNotBridged through errors.Is.
type BridgeNoticeError struct {
	EventID string
	Notice  string
}

func (e *BridgeNoticeError) Error() string {
	return fmt.Sprintf("%v: event %s: %s", ErrNotBridged, e.EventID, e.Notice)
}

func (e *BridgeNoticeError) Unwrap() error { return ErrNotBridged }

const (
	minBackoff        = time.Second
	maxBackoff        = time.Minute
	healthyRun        = time.Minute
	defaultMsgLimit   = 20
	maxMsgLimit       = 100
	nameFetchTimeout  = 10 * time.Second
	nameFetchParallel = 8
	undecryptable     = "[encrypted message could not be decrypted]"

	// maxMessagePages bounds how many /messages pages one Messages call walks
	// backward while looking for enough convertible messages.
	maxMessagePages = 10
	// maxMessageRawFactor bounds the raw events scanned per requested message.
	// A busy room is mostly membership, edit and reaction events, so a single
	// page can yield almost no messages.
	maxMessageRawFactor = 5

	// maxEventIDLen is the Matrix spec's 255-byte cap on event IDs.
	maxEventIDLen = 255

	// bridgeAckPollInterval is how often the bridge-ack guard re-reads a room
	// while waiting for the bridge's notice.
	bridgeAckPollInterval = 100 * time.Millisecond
	// bridgeAckScanLimit is how many recent events each poll examines.
	bridgeAckScanLimit = 20

	// autoJoinTimeout bounds one automatic join. The join runs off the sync
	// loop, so a slow homeserver delays the join only.
	autoJoinTimeout = 30 * time.Second
)

// Client is the mautrix-backed MatrixService.
type Client struct {
	cfg *config.Config
	log *log.Logger
	cli *mautrix.Client

	crypto atomic.Pointer[cryptohelper.CryptoHelper]

	// searchUnavailable and contextUnavailable remember that the homeserver
	// does not implement /search or /context, so a search or a context call
	// does not pay for a doomed request every time.
	searchUnavailable  atomic.Bool
	contextUnavailable atomic.Bool

	syncing   atomic.Bool
	initMu    sync.Mutex
	whoamiOK  bool
	cryptoOK  bool
	filterOK  bool
	seeded    bool
	namesMu   sync.RWMutex
	names     map[string]string
	allow     *allowlist.Store
	pages     *pageSigner
	joinMu    sync.Mutex
	joining   map[id.RoomID]struct{}
	bgCtx     context.Context
	bgCancel  context.CancelFunc
	bgWG      sync.WaitGroup
	closeMu   sync.Mutex
	closed    bool
	closeOnce sync.Once
	closeErr  error
}

var _ MatrixService = (*Client)(nil)

// New builds a Client. It performs no network I/O; authentication, crypto
// setup and the sync loop all start in Sync.
func New(cfg *config.Config, logger *log.Logger) (*Client, error) {
	if cfg == nil {
		return nil, errors.New("matrix: nil config")
	}
	if logger == nil {
		return nil, errors.New("matrix: nil logger")
	}
	cli, err := mautrix.NewClient(cfg.Homeserver, id.UserID(cfg.UserID), cfg.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("matrix: create client: %w", err)
	}
	cli.Log = zerolog.New(os.Stderr).Level(zerolog.WarnLevel).With().Timestamp().Logger()

	syncer := mautrix.NewDefaultSyncer()
	syncer.FilterJSON = &mautrix.Filter{
		Room: &mautrix.RoomFilter{
			Timeline: &mautrix.FilterPart{Limit: 50},
			State:    &mautrix.FilterPart{LazyLoadMembers: true},
		},
	}
	cli.Syncer = syncer

	pages, err := newPageSigner()
	if err != nil {
		return nil, err
	}

	bgCtx, bgCancel := context.WithCancel(context.Background())
	c := &Client{
		cfg:      cfg,
		log:      logger,
		cli:      cli,
		names:    make(map[string]string),
		allow:    allowlist.New(cfg.AllowedSendRooms, cfg.AllowlistFile, logger),
		pages:    pages,
		joining:  make(map[id.RoomID]struct{}),
		bgCtx:    bgCtx,
		bgCancel: bgCancel,
	}
	syncer.OnEventType(event.StateRoomName, func(_ context.Context, evt *event.Event) {
		if content, ok := evt.Content.Parsed.(*event.RoomNameEventContent); ok {
			c.setName(evt.RoomID.String(), content.Name)
		}
	})
	// The invite listener is registered before init installs
	// DontProcessOldEvents, which drops the whole first sync response. Joining
	// from the first response is what makes an invitation survive a restart:
	// otherwise an invite that arrived while the bot was down would be seen by
	// no handler at all and never accepted.
	if cfg.AutoJoinInvites {
		syncer.OnSync(c.handleInvites)
	}
	return c, nil
}

// OnEventType registers a handler on the underlying syncer. Events sent by the
// account itself are filtered out so the server never reacts to its own
// messages. Register handlers before calling Sync.
func (c *Client) OnEventType(t event.Type, handler mautrix.EventHandler) {
	c.cli.Syncer.(mautrix.ExtensibleSyncer).OnEventType(t, func(ctx context.Context, evt *event.Event) {
		if evt.Sender == c.cli.UserID {
			return
		}
		handler(ctx, evt)
	})
}

// Allowlist returns the effective send allowlist: ALLOWED_SEND_ROOMS unioned
// with the allowlist file, re-read as the file changes. The MCP layer gates
// sends on the same Store so the two can never disagree.
func (c *Client) Allowlist() *allowlist.Store { return c.allow }

// WhoAmI reports the account the access token belongs to. It is one request and
// changes no client state, so `doctor` can call it without starting a sync.
func (c *Client) WhoAmI(ctx context.Context) (userID, deviceID string, err error) {
	who, err := c.cli.Whoami(ctx)
	if err != nil {
		return "", "", fmt.Errorf("matrix: whoami: %w", err)
	}
	return who.UserID.String(), who.DeviceID.String(), nil
}

// RoomName returns the cached display name of a room, or the room ID when it is
// not known. It never touches the network, so it is safe on the sync path.
func (c *Client) RoomName(roomID string) string {
	c.namesMu.RLock()
	defer c.namesMu.RUnlock()
	if name := c.names[roomID]; name != "" {
		return name
	}
	return roomID
}

func (c *Client) setName(roomID, name string) {
	c.namesMu.Lock()
	c.names[roomID] = name
	c.namesMu.Unlock()
}

func (c *Client) knownName(roomID string) (string, bool) {
	c.namesMu.RLock()
	defer c.namesMu.RUnlock()
	name, ok := c.names[roomID]
	return name, ok
}

func (c *Client) fetchName(ctx context.Context, roomID string) {
	if _, ok := c.knownName(roomID); ok {
		return
	}
	var content event.RoomNameEventContent
	err := c.cli.StateEvent(ctx, id.RoomID(roomID), event.StateRoomName, "", &content)
	if err != nil && !errors.Is(err, mautrix.MNotFound) {
		c.log.Printf("room name lookup failed for %s: %v", roomID, err)
		return
	}
	c.setName(roomID, content.Name)
}

// CanonicalAlias returns the room's canonical alias, or "" if it has none or
// the lookup fails. It is best effort so a failed lookup never fails a caller.
func (c *Client) CanonicalAlias(ctx context.Context, roomID string) string {
	ctx, cancel := context.WithTimeout(ctx, nameFetchTimeout)
	defer cancel()
	var content event.CanonicalAliasEventContent
	if err := c.cli.StateEvent(ctx, id.RoomID(roomID), event.StateCanonicalAlias, "", &content); err != nil {
		return ""
	}
	return content.Alias.String()
}

// IsBridged reports whether a room appears to be bridged from WhatsApp. It is
// deliberately best effort: the presence of an m.bridge state event marks the
// room as bridged; failing that, a joined member whose localpart contains
// "whatsapp" does. Any probe error (including an inaccessible room or a
// timeout) is treated as not bridged, so a single room never fails the whole
// list_rooms call.
func (c *Client) IsBridged(ctx context.Context, roomID string) bool {
	ctx, cancel := context.WithTimeout(ctx, nameFetchTimeout)
	defer cancel()
	var bridge event.BridgeEventContent
	err := c.cli.StateEvent(ctx, id.RoomID(roomID), event.StateBridge, "", &bridge)
	if err == nil {
		return true
	}
	if !errors.Is(err, mautrix.MNotFound) {
		c.log.Printf("bridge probe failed for %s: %v", roomID, err)
	}

	members, err := c.cli.Members(ctx, id.RoomID(roomID))
	if err != nil {
		c.log.Printf("member probe failed for %s: %v", roomID, err)
		return false
	}
	for _, evt := range members.Chunk {
		if evt.Type != event.StateMember {
			continue
		}
		if evt.Content.Parsed == nil {
			if err := evt.Content.ParseRaw(event.StateMember); err != nil {
				continue
			}
		}
		if member := evt.Content.AsMember(); member != nil && member.Membership == event.MembershipJoin {
			if classifyBridgeUser(evt.GetStateKey()) != "" {
				return true
			}
		}
	}
	return false
}

// JoinedRooms implements MatrixService.
func (c *Client) JoinedRooms(ctx context.Context) ([]Room, error) {
	resp, err := c.cli.JoinedRooms(ctx)
	if err != nil {
		return nil, fmt.Errorf("matrix: list joined rooms: %w", err)
	}
	sem := make(chan struct{}, nameFetchParallel)
	var wg sync.WaitGroup
	for _, rid := range resp.JoinedRooms {
		if _, ok := c.knownName(rid.String()); ok {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(rid string) {
			defer wg.Done()
			defer func() { <-sem }()
			fctx, cancel := context.WithTimeout(ctx, nameFetchTimeout)
			defer cancel()
			c.fetchName(fctx, rid)
		}(rid.String())
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rooms := make([]Room, 0, len(resp.JoinedRooms))
	for _, rid := range resp.JoinedRooms {
		rooms = append(rooms, Room{ID: rid.String(), Name: c.RoomName(rid.String())})
	}
	sort.Slice(rooms, func(i, j int) bool {
		if rooms[i].Name != rooms[j].Name {
			return rooms[i].Name < rooms[j].Name
		}
		return rooms[i].ID < rooms[j].ID
	})
	return rooms, nil
}

// Messages implements MatrixService. A limit outside 1..100 is clamped. It
// pages backward through /messages until it has limit convertible messages or
// the page it started from is exhausted, because the raw event stream is
// dominated by membership, edit and reaction events that convert to nothing.
// Paging is bounded by maxMessagePages and maxMessageRawFactor so a huge or
// noisy room cannot make the call unbounded, whether the caller supplies a
// token or not.
//
// before is a token from an earlier page. Without one the call starts at the
// newest message, which is the pre-pagination behaviour. A token is bound to
// the room it was issued for and signed by this process, so a malformed,
// foreign or hand-edited token is an ErrInvalidPageToken rather than an empty
// page; a token the homeserver no longer accepts is ErrStalePageToken.
//
// Messages are returned in the order the homeserver hands them out (newest
// first). NextToken is set when there is more history behind this page, and is
// exactly what the caller passes back as before.
func (c *Client) Messages(ctx context.Context, roomID string, limit int, before string) (MessagePage, error) {
	if !config.ValidRoomID(roomID) {
		return MessagePage{}, ErrInvalidRoomID
	}
	switch {
	case limit <= 0:
		limit = defaultMsgLimit
	case limit > maxMsgLimit:
		limit = maxMsgLimit
	}

	from := ""
	if before != "" {
		var err error
		from, err = c.pages.verify(roomID, before)
		if err != nil {
			return MessagePage{}, err
		}
	}

	out := make([]Message, 0, limit)
	rawBudget := maxMessageRawFactor * limit
	next := ""
	for page := 0; ; page++ {
		// Stop without consuming the token of the page we have not read yet:
		// from is the start of the next older page.
		if page >= maxMessagePages || rawBudget <= 0 || len(out) >= limit {
			next = from
			break
		}
		want := min(limit-len(out), rawBudget)
		resp, err := c.cli.Messages(ctx, id.RoomID(roomID), from, "", mautrix.DirectionBackward, nil, want)
		if err != nil {
			if from != "" && isStalePageError(err) {
				return MessagePage{}, fmt.Errorf("%w: the homeserver rejected it; call again without before", ErrStalePageToken)
			}
			return MessagePage{}, fmt.Errorf("matrix: fetch messages: %w", err)
		}
		rawBudget -= len(resp.Chunk)
		for _, evt := range resp.Chunk {
			evt.RoomID = id.RoomID(roomID)
			if msg, ok := c.convert(ctx, evt); ok {
				out = append(out, msg)
			}
		}
		// The homeserver omits end at the start of the timeline, and an empty
		// chunk means it has nothing older to give.
		if len(resp.Chunk) == 0 || resp.End == "" {
			next = ""
			break
		}
		from = resp.End
	}

	token, err := c.pages.sign(roomID, next)
	if err != nil {
		return MessagePage{}, err
	}
	return MessagePage{Messages: out, NextToken: token, HasMore: token != ""}, nil
}

func (c *Client) convert(ctx context.Context, evt *event.Event) (Message, bool) {
	encrypted := false
	switch evt.Type {
	case event.EventMessage:
	case event.EventEncrypted:
		encrypted = true
	default:
		return Message{}, false
	}
	if evt.Unsigned.RedactedBecause != nil {
		return redactedMessage(evt, encrypted), true
	}
	if encrypted {
		decrypted := c.decrypt(ctx, evt)
		if decrypted == nil {
			return Message{
				EventID: evt.ID.String(), RoomID: evt.RoomID.String(), Sender: evt.Sender.String(),
				Body: undecryptable, Timestamp: evt.Timestamp, Encrypted: true,
			}, true
		}
		evt = decrypted
		if evt.Type != event.EventMessage {
			return Message{}, false
		}
	}
	if evt.Content.Parsed == nil {
		if err := evt.Content.ParseRaw(evt.Type); err != nil {
			return Message{}, false
		}
	}
	msg := evt.Content.AsMessage()
	if msg == nil {
		return Message{}, false
	}
	replyTo, editOf, mentions := relationFields(msg)
	return Message{
		EventID:   evt.ID.String(),
		RoomID:    evt.RoomID.String(),
		Sender:    evt.Sender.String(),
		Body:      placeholderBody(msg.MsgType, msg.Body),
		MsgType:   string(msg.MsgType),
		Timestamp: evt.Timestamp,
		Encrypted: encrypted,
		ReplyTo:   replyTo,
		EditOf:    editOf,
		Mentions:  mentions,
	}, true
}

// redactedMessage reports a message the room has deleted. It keeps its place in
// the stream — the event ID, sender and timestamp are still true — and carries
// no body: the server has already stripped the content, and returning anything
// from it would hand back text a participant chose to delete.
func redactedMessage(evt *event.Event, encrypted bool) Message {
	return Message{
		EventID:   evt.ID.String(),
		RoomID:    evt.RoomID.String(),
		Sender:    evt.Sender.String(),
		Timestamp: evt.Timestamp,
		Encrypted: encrypted,
		Redacted:  true,
	}
}

// relationFields extracts what a message answers, replaces, or mentions. Both
// reply shapes count as a reply: the reply form m.relates_to
// .m.in_reply_to.event_id that mautrix-whatsapp emits, and the flat
// m.relates_to.event_id that other clients send. An m.replace relation is an
// edit, so it fills editOf instead of replyTo and the edit stays in the stream
// for a client to apply; any other relation names its parent in the flat field,
// so a thread reply that carries no m.in_reply_to still points at its root.
func relationFields(msg *event.MessageEventContent) (replyTo, editOf string, mentions []string) {
	if rel := msg.RelatesTo; rel != nil {
		switch {
		case rel.Type == event.RelReplace:
			editOf = rel.EventID.String()
		case rel.InReplyTo != nil && rel.InReplyTo.EventID != "":
			replyTo = rel.InReplyTo.EventID.String()
		default:
			replyTo = rel.EventID.String()
		}
	}
	if msg.Mentions != nil {
		for _, userID := range msg.Mentions.UserIDs {
			mentions = append(mentions, userID.String())
		}
	}
	return replyTo, editOf, mentions
}

// placeholderBody substitutes a short bracketed marker for a media event that
// carries no text body: bridges commonly send m.image/m.video/m.file events
// with body "" (the caption, if any, is elsewhere), and an empty string reads
// to an AI client as a blank message. The marker is derived from the msgtype
// alone and never from the event's content; text-like bodies are returned
// verbatim.
func placeholderBody(msgtype event.MessageType, body string) string {
	if body != "" {
		return body
	}
	switch msgtype {
	case event.MsgText, event.MsgNotice, event.MsgEmote, "":
		return body
	}
	return "[" + strings.TrimPrefix(string(msgtype), "m.") + "]"
}

func (c *Client) decrypt(ctx context.Context, evt *event.Event) *event.Event {
	helper := c.crypto.Load()
	if helper == nil {
		return nil
	}
	if evt.Content.Parsed == nil {
		if err := evt.Content.ParseRaw(event.EventEncrypted); err != nil {
			return nil
		}
	}
	decrypted, err := helper.Decrypt(ctx, evt)
	if err != nil {
		return nil
	}
	decrypted.RoomID = evt.RoomID
	decrypted.ID = evt.ID
	return decrypted
}

// ValidEventID reports whether eventID is a well-formed Matrix event ID: a "$"
// sigil, at most 255 bytes, and no whitespace or control characters. A server
// suffix is not required — bridges hand out event IDs without one — and nothing
// weaker is accepted, because a reply target that is not an event ID would be
// sent to the homeserver and fail there instead of here.
func ValidEventID(eventID string) bool {
	if len(eventID) < 2 || len(eventID) > maxEventIDLen || eventID[0] != '$' {
		return false
	}
	for _, r := range eventID {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// SendText implements MatrixService. When replyTo is set the message is sent
// with m.relates_to.m.in_reply_to, the shape WhatsApp's bridge renders as a
// quote; an event ID that is not a well-formed Matrix event ID is rejected
// before anything reaches the homeserver. Everything after the send — the
// allowlist, the encryption check and the bridge-ack guard — is identical for
// a reply and a plain send.
func (c *Client) SendText(ctx context.Context, roomID, text, replyTo string) (string, error) {
	if !config.ValidRoomID(roomID) {
		return "", ErrInvalidRoomID
	}
	if !c.allow.Allowed(roomID) {
		return "", ErrSendNotAllowed
	}
	if strings.TrimSpace(text) == "" {
		return "", ErrEmptyMessage
	}
	if replyTo != "" && !ValidEventID(replyTo) {
		return "", ErrInvalidEventID
	}
	if err := c.prepareEncryption(ctx, id.RoomID(roomID)); err != nil {
		return "", err
	}
	content := &event.MessageEventContent{MsgType: event.MsgText, Body: text}
	if replyTo != "" {
		content.RelatesTo = &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: id.EventID(replyTo)}}
	}
	resp, err := c.cli.SendMessageEvent(ctx, id.RoomID(roomID), event.EventMessage, content)
	if err != nil {
		return "", fmt.Errorf("matrix: send message: %w", err)
	}
	eventID := resp.EventID.String()
	// Matrix accepting the event does not mean the bridge forwarded it: a
	// bridge whose relay/login is misconfigured posts an m.notice into the room
	// that relates to the sent event. Reporting success there would let a
	// caller believe a WhatsApp message was delivered when nothing was.
	if c.cfg.BridgeAckTimeout > 0 {
		if notice, ok := c.awaitBridgeNotice(ctx, roomID, resp.EventID); ok {
			return "", &BridgeNoticeError{EventID: eventID, Notice: notice}
		}
	}
	return eventID, nil
}

// awaitBridgeNotice polls the newest events of a room until a bridge notice
// about eventID shows up or cfg.BridgeAckTimeout elapses. It issues no request
// at all when the guard is disabled (the caller checks), honours ctx, and
// starts no goroutine, so nothing can outlive the send.
func (c *Client) awaitBridgeNotice(ctx context.Context, roomID string, eventID id.EventID) (string, bool) {
	waitCtx, cancel := context.WithTimeout(ctx, c.cfg.BridgeAckTimeout)
	defer cancel()
	ticker := time.NewTicker(bridgeAckPollInterval)
	defer ticker.Stop()
	var logged bool
	for {
		notice, ok, err := c.findBridgeNotice(waitCtx, roomID, eventID)
		if ok {
			return notice, true
		}
		// A failing probe leaves the send a success, but it also silently
		// disables the guard, so report it once per send rather than never.
		if err != nil && !logged {
			logged = true
			c.log.Printf("bridge-ack probe failed for %s: %v", roomID, err)
		}
		select {
		case <-waitCtx.Done():
			return "", false
		case <-ticker.C:
		}
	}
}

// findBridgeNotice looks for the bridge's rejection notice among the newest
// events of a room. A notice counts when its m.relates_to points at the sent
// event and it was not sent by this account. A bridge may express that relation
// either as m.relates_to.event_id or as a reply, m.relates_to.m.in_reply_to
// .event_id; mautrix-whatsapp emits the reply shape, which is why matching only
// the flat field misses every live rejection. The rule deliberately does not
// depend on the bridge bot's user ID, which differs per bridge deployment;
// a false positive needs another client to reply with an m.notice to this exact
// event, and even then the caller only gets a quoted notice, never a lost send.
func (c *Client) findBridgeNotice(ctx context.Context, roomID string, eventID id.EventID) (string, bool, error) {
	resp, err := c.cli.Messages(ctx, id.RoomID(roomID), "", "", mautrix.DirectionBackward, nil, bridgeAckScanLimit)
	if err != nil {
		// A probe failure must not turn a delivered send into an error.
		return "", false, err
	}
	for _, evt := range resp.Chunk {
		if evt.Type != event.EventMessage || evt.Sender == c.cli.UserID {
			continue
		}
		if evt.Content.Parsed == nil {
			if err := evt.Content.ParseRaw(evt.Type); err != nil {
				continue
			}
		}
		msg := evt.Content.AsMessage()
		if msg == nil || msg.MsgType != event.MsgNotice {
			continue
		}
		if relatesToEventID(msg.RelatesTo) != eventID {
			continue
		}
		return sanitizeNotice(msg.Body), true, nil
	}
	return "", false, nil
}

// relatesToEventID returns the event a relation points at, whether the sender
// used the flat m.relates_to.event_id or the reply form m.relates_to
// .m.in_reply_to.event_id. Mautrix keeps the two apart: RelatesTo.EventID stays
// empty for a reply.
func relatesToEventID(rel *event.RelatesTo) id.EventID {
	if rel == nil {
		return ""
	}
	if rel.EventID != "" {
		return rel.EventID
	}
	if rel.InReplyTo != nil {
		return rel.InReplyTo.EventID
	}
	return ""
}

// sanitizeNotice renders a bridge notice as one trimmed line with no control
// characters and a sane upper bound, so an error message stays readable and
// cannot smuggle terminal escapes or an unbounded body into a log or a tool
// result.
func sanitizeNotice(body string) string {
	body = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, body)
	body = strings.Join(strings.Fields(body), " ")
	const maxNotice = 300
	if runes := []rune(body); len(runes) > maxNotice {
		body = string(runes[:maxNotice]) + "..."
	}
	if body == "" {
		body = "(empty notice)"
	}
	return body
}

// prepareEncryption makes sure a message is never sent in plaintext into an
// encrypted room. The initial sync is deliberately not processed (so history
// never triggers anything), which means the state store does not know about
// room encryption or membership until it is fetched here.
func (c *Client) prepareEncryption(ctx context.Context, roomID id.RoomID) error {
	var content event.EncryptionEventContent
	err := c.cli.StateEvent(ctx, roomID, event.StateEncryption, "", &content)
	if errors.Is(err, mautrix.MNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("matrix: check room encryption: %w", err)
	}
	if c.crypto.Load() == nil || c.cli.StateStore == nil {
		return ErrRoomEncrypted
	}
	if err := c.cli.StateStore.SetEncryptionEvent(ctx, roomID, &content); err != nil {
		return fmt.Errorf("matrix: record room encryption: %w", err)
	}
	fetched, err := c.cli.StateStore.HasFetchedMembers(ctx, roomID)
	if err != nil {
		return fmt.Errorf("matrix: check member cache: %w", err)
	}
	if !fetched {
		if _, err := c.cli.Members(ctx, roomID); err != nil {
			return fmt.Errorf("matrix: fetch room members: %w", err)
		}
	}
	return nil
}

// handleInvites accepts pending room invitations. It runs as a sync listener,
// not an event handler, for two reasons: invitation state arrives in
// rooms.invite rather than a timeline, and this listener is installed before
// DontProcessOldEvents, so the invitations carried by the first sync response
// are seen too.
func (c *Client) handleInvites(_ context.Context, resp *mautrix.RespSync, _ string) bool {
	for roomID, invited := range resp.Rooms.Invite {
		inviter, ok := inviteSender(invited, c.cfg.UserID)
		if !ok {
			continue
		}
		c.autoJoin(roomID, inviter)
	}
	return true
}

// inviteSender returns the user who invited us to room, which is the sender of
// our own invite membership event. The event may be stripped state from
// rooms.invite, so its content is parsed here instead of relying on the syncer
// having done it.
func inviteSender(room *mautrix.SyncInvitedRoom, self string) (id.UserID, bool) {
	if room == nil {
		return "", false
	}
	for _, evt := range room.BestState() {
		if evt.Type.Type != event.StateMember.Type || evt.GetStateKey() != self {
			continue
		}
		if evt.Content.Parsed == nil {
			if err := evt.Content.ParseRaw(event.StateMember); err != nil {
				continue
			}
		}
		member := evt.Content.AsMember()
		if member == nil || member.Membership != event.MembershipInvite || evt.Sender == "" {
			continue
		}
		return evt.Sender, true
	}
	return "", false
}

// autoJoin joins a room in the background. The decision is made on the sync
// goroutine but the request is not issued there, so a slow or hanging join
// never stalls the sync loop; in-flight rooms are deduplicated because an
// invitation can be repeated by consecutive sync responses, and every join
// goroutine is joined by Close.
func (c *Client) autoJoin(roomID id.RoomID, inviter id.UserID) {
	if !c.cfg.InviteAllowed(inviter.String()) {
		c.log.Printf("auto-join: refusing invitation to %s from %s: not in MATRIX_INVITE_ALLOWLIST", roomID, inviter)
		return
	}

	c.joinMu.Lock()
	_, busy := c.joining[roomID]
	c.closeMu.Lock()
	closed := c.closed
	if !busy && !closed {
		c.joining[roomID] = struct{}{}
		c.bgWG.Add(1)
	}
	c.closeMu.Unlock()
	c.joinMu.Unlock()
	if busy || closed {
		return
	}

	go func() {
		defer c.bgWG.Done()
		defer func() {
			c.joinMu.Lock()
			delete(c.joining, roomID)
			c.joinMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(c.bgCtx, autoJoinTimeout)
		defer cancel()
		if _, err := c.cli.JoinRoomByID(ctx, roomID); err != nil {
			c.log.Printf("auto-join: joining %s failed: %v", roomID, err)
			return
		}
		c.log.Printf("auto-join: joined %s (invited by %s)", roomID, inviter)
	}()
}

// Sync implements MatrixService.
func (c *Client) Sync(ctx context.Context) error {
	if !c.syncing.CompareAndSwap(false, true) {
		return ErrAlreadySyncing
	}
	defer c.syncing.Store(false)

	backoff := minBackoff
	for {
		start := time.Now()
		err := c.syncOnce(ctx)
		if ctx.Err() != nil || c.isClosed() {
			return nil
		}
		if isFatal(err) {
			return err
		}
		if time.Since(start) >= healthyRun {
			backoff = minBackoff
		}
		wait := backoff/2 + rand.N(backoff/2+1)
		c.log.Printf("sync stopped (%v); retrying in %s", err, wait.Round(time.Millisecond))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-c.bgCtx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

func isFatal(err error) bool {
	return errors.Is(err, mautrix.MUnknownToken) ||
		errors.Is(err, mautrix.MMissingToken) ||
		errors.Is(err, ErrUserMismatch)
}

func (c *Client) syncOnce(ctx context.Context) error {
	if err := c.init(ctx); err != nil {
		return err
	}
	return c.cli.SyncWithContext(ctx)
}

// init performs the network-dependent setup. Each step records completion so a
// retry after a transient failure resumes where it stopped.
func (c *Client) init(ctx context.Context) error {
	c.initMu.Lock()
	defer c.initMu.Unlock()

	if !c.whoamiOK {
		who, err := c.cli.Whoami(ctx)
		if err != nil {
			return fmt.Errorf("matrix: whoami: %w", err)
		}
		if who.UserID != c.cli.UserID {
			return ErrUserMismatch
		}
		c.cli.DeviceID = who.DeviceID
		c.whoamiOK = true
	}

	if c.cfg.E2EEEnabled && !c.cryptoOK {
		helper, err := cryptohelper.NewCryptoHelper(c.cli, []byte(c.cfg.PickleKey), c.cfg.CryptoDBPath)
		if err != nil {
			return fmt.Errorf("matrix: create crypto helper: %w", err)
		}
		if err := helper.Init(ctx); err != nil {
			_ = helper.Close()
			return fmt.Errorf("matrix: init crypto helper: %w", err)
		}
		c.cli.Crypto = helper
		c.crypto.Store(helper)
		c.cryptoOK = true
	}

	if !c.filterOK {
		c.cli.Syncer.(mautrix.ExtensibleSyncer).OnSync(c.cli.DontProcessOldEvents)
		c.filterOK = true
	}

	if !c.seeded {
		c.closeMu.Lock()
		if !c.closed {
			c.bgWG.Add(1)
			go c.seedNames()
			c.seeded = true
		}
		c.closeMu.Unlock()
	}
	return nil
}

func (c *Client) seedNames() {
	defer c.bgWG.Done()
	resp, err := c.cli.JoinedRooms(c.bgCtx)
	if err != nil {
		c.log.Printf("room name seeding skipped: %v", err)
		return
	}
	for _, rid := range resp.JoinedRooms {
		if c.bgCtx.Err() != nil {
			return
		}
		ctx, cancel := context.WithTimeout(c.bgCtx, nameFetchTimeout)
		c.fetchName(ctx, rid.String())
		cancel()
	}
}

func (c *Client) isClosed() bool {
	c.closeMu.Lock()
	defer c.closeMu.Unlock()
	return c.closed
}

// Close stops background work and releases the crypto store. It is idempotent.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.closeMu.Lock()
		c.closed = true
		c.closeMu.Unlock()
		c.bgCancel()
		c.cli.StopSync()
		c.bgWG.Wait()
		if helper := c.crypto.Swap(nil); helper != nil {
			c.closeErr = helper.Close()
		}
	})
	return c.closeErr
}
