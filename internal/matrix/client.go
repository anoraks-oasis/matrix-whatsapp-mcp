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

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

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
type Message struct {
	EventID   string `json:"event_id"`
	RoomID    string `json:"room_id"`
	Sender    string `json:"sender"`
	Body      string `json:"body"`
	MsgType   string `json:"msgtype"`
	Timestamp int64  `json:"timestamp"`
	Encrypted bool   `json:"encrypted"`
}

// MatrixService is the surface the MCP layer depends on.
type MatrixService interface {
	// JoinedRooms lists the rooms the account has joined, sorted by name.
	JoinedRooms(ctx context.Context) ([]Room, error)
	// Messages returns up to limit recent messages of a room, newest first.
	Messages(ctx context.Context, roomID string, limit int) ([]Message, error)
	// SendText sends a plain-text message and returns the new event ID. It
	// fails closed with ErrSendNotAllowed unless the room is allow-listed.
	SendText(ctx context.Context, roomID, text string) (string, error)
	// Sync runs the sync loop and blocks until ctx is cancelled (returning
	// nil) or a fatal error such as an invalid access token occurs.
	Sync(ctx context.Context) error
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
	// ErrEmptyMessage is returned when the text to send is empty.
	ErrEmptyMessage = errors.New("matrix: message text is empty")
	// ErrAlreadySyncing is returned when Sync is called twice concurrently.
	ErrAlreadySyncing = errors.New("matrix: sync already running")
	// ErrUserMismatch is returned when the access token belongs to a different user.
	ErrUserMismatch = errors.New("matrix: access token does not belong to MATRIX_USER_ID")
	// ErrClosed is returned by operations on a closed client.
	ErrClosed = errors.New("matrix: client closed")
)

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
)

// Client is the mautrix-backed MatrixService.
type Client struct {
	cfg *config.Config
	log *log.Logger
	cli *mautrix.Client

	crypto atomic.Pointer[cryptohelper.CryptoHelper]

	syncing   atomic.Bool
	initMu    sync.Mutex
	whoamiOK  bool
	cryptoOK  bool
	filterOK  bool
	seeded    bool
	namesMu   sync.RWMutex
	names     map[string]string
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

	bgCtx, bgCancel := context.WithCancel(context.Background())
	c := &Client{
		cfg:      cfg,
		log:      logger,
		cli:      cli,
		names:    make(map[string]string),
		bgCtx:    bgCtx,
		bgCancel: bgCancel,
	}
	syncer.OnEventType(event.StateRoomName, func(_ context.Context, evt *event.Event) {
		if content, ok := evt.Content.Parsed.(*event.RoomNameEventContent); ok {
			c.setName(evt.RoomID.String(), content.Name)
		}
	})
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
			localpart, _, err := id.UserID(evt.GetStateKey()).Parse()
			if err == nil && strings.Contains(strings.ToLower(localpart), "whatsapp") {
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
// the room is exhausted, because the raw event stream is dominated by
// membership, edit and reaction events that convert to nothing. Paging is
// bounded by maxMessagePages and maxMessageRawFactor so a huge or noisy room
// cannot make the call unbounded. Messages are returned in the order the
// homeserver hands them out (newest first).
func (c *Client) Messages(ctx context.Context, roomID string, limit int) ([]Message, error) {
	if !config.ValidRoomID(roomID) {
		return nil, ErrInvalidRoomID
	}
	switch {
	case limit <= 0:
		limit = defaultMsgLimit
	case limit > maxMsgLimit:
		limit = maxMsgLimit
	}

	out := make([]Message, 0, limit)
	rawBudget := maxMessageRawFactor * limit
	from := ""
	for page := 0; page < maxMessagePages && len(out) < limit && rawBudget > 0; page++ {
		want := min(limit-len(out), rawBudget)
		resp, err := c.cli.Messages(ctx, id.RoomID(roomID), from, "", mautrix.DirectionBackward, nil, want)
		if err != nil {
			return nil, fmt.Errorf("matrix: fetch messages: %w", err)
		}
		rawBudget -= len(resp.Chunk)
		for _, evt := range resp.Chunk {
			evt.RoomID = id.RoomID(roomID)
			if msg, ok := c.convert(ctx, evt); ok {
				out = append(out, msg)
				if len(out) == limit {
					break
				}
			}
		}
		if len(resp.Chunk) == 0 || resp.End == "" {
			break
		}
		from = resp.End
	}
	return out, nil
}

func (c *Client) convert(ctx context.Context, evt *event.Event) (Message, bool) {
	encrypted := false
	switch evt.Type {
	case event.EventMessage:
	case event.EventEncrypted:
		encrypted = true
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
	default:
		return Message{}, false
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
	return Message{
		EventID:   evt.ID.String(),
		RoomID:    evt.RoomID.String(),
		Sender:    evt.Sender.String(),
		Body:      placeholderBody(msg.MsgType, msg.Body),
		MsgType:   string(msg.MsgType),
		Timestamp: evt.Timestamp,
		Encrypted: encrypted,
	}, true
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

// SendText implements MatrixService.
func (c *Client) SendText(ctx context.Context, roomID, text string) (string, error) {
	if !config.ValidRoomID(roomID) {
		return "", ErrInvalidRoomID
	}
	if !c.cfg.SendAllowed(roomID) {
		return "", ErrSendNotAllowed
	}
	if strings.TrimSpace(text) == "" {
		return "", ErrEmptyMessage
	}
	if err := c.prepareEncryption(ctx, id.RoomID(roomID)); err != nil {
		return "", err
	}
	resp, err := c.cli.SendText(ctx, id.RoomID(roomID), text)
	if err != nil {
		return "", fmt.Errorf("matrix: send message: %w", err)
	}
	return resp.EventID.String(), nil
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
