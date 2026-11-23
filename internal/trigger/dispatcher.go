// Package trigger turns incoming Matrix message events into Trigger records,
// keeps the most recent ones in memory, and optionally forwards them to a
// webhook without ever blocking the sync loop.
package trigger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"

	"github.com/anoraks-oasis/matrix-whatsapp-mcp/internal/whatsapp"
)

const (
	// BufferSize is the number of triggers kept for Recent.
	BufferSize = 100

	webhookTimeout    = 5 * time.Second
	webhookRetryDelay = 500 * time.Millisecond
	webhookRetries    = 1
	queueSize         = 256
	workerCount       = 4
)

// Trigger is a normalized incoming message, in the WhatsApp vocabulary every
// other surface speaks. Timestamp is unix milliseconds. A media event that
// carries no caption gets a bracketed type marker such as "[image]" instead of
// an empty string, so a webhook consumer never sees a silently blank text.
// Mimetype, Filename, SizeBytes and MediaURI are present only when the message
// carries an attachment.
type Trigger struct {
	MessageID string `json:"message_id"`
	ChatID    string `json:"chat_id"`
	ChatName  string `json:"chat_name"`
	From      string `json:"from"`
	Text      string `json:"text"`
	Type      string `json:"type"`
	Timestamp int64  `json:"timestamp"`
	Mimetype  string `json:"mimetype,omitempty"`
	Filename  string `json:"filename,omitempty"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	MediaURI  string `json:"media_uri,omitempty"`
}

// RoomNamer resolves a room's display name without network access.
type RoomNamer interface {
	RoomName(roomID string) string
}

// EventSource is satisfied by *matrix.Client.
type EventSource interface {
	OnEventType(t event.Type, handler mautrix.EventHandler)
}

// Dispatcher records message events and forwards them to a webhook.
type Dispatcher struct {
	log        *log.Logger
	namer      RoomNamer
	webhookURL string
	httpClient *http.Client
	timeout    time.Duration
	retryDelay time.Duration
	retries    int

	mu    sync.RWMutex
	ring  [BufferSize]Trigger
	next  int
	count int

	lifeMu  sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc
	queue   chan Trigger
	wg      sync.WaitGroup
}

// New creates a Dispatcher. An empty webhookURL disables webhook delivery.
// namer may be nil, in which case the room ID is used as the room name.
func New(webhookURL string, namer RoomNamer, logger *log.Logger) *Dispatcher {
	return &Dispatcher{
		log:        logger,
		namer:      namer,
		webhookURL: webhookURL,
		httpClient: &http.Client{},
		timeout:    webhookTimeout,
		retryDelay: webhookRetryDelay,
		retries:    webhookRetries,
		queue:      make(chan Trigger, queueSize),
	}
}

// Attach subscribes the dispatcher to m.room.message events.
func (d *Dispatcher) Attach(src EventSource) {
	src.OnEventType(event.EventMessage, d.Handle)
}

// Handle normalizes a message event, records it, and queues webhook delivery.
// It never blocks and performs no network I/O.
func (d *Dispatcher) Handle(_ context.Context, evt *event.Event) {
	if evt == nil || evt.Type != event.EventMessage {
		return
	}
	msg := evt.Content.AsMessage()
	if msg == nil {
		return
	}
	roomID := evt.RoomID.String()
	name := roomID
	if d.namer != nil {
		name = d.namer.RoomName(roomID)
	}
	t := Trigger{
		MessageID: evt.ID.String(),
		ChatID:    roomID,
		ChatName:  name,
		From:      evt.Sender.String(),
		Text:      bodyOrPlaceholder(msg.MsgType, msg.Body),
		Type:      whatsapp.Type(string(msg.MsgType)),
		Timestamp: evt.Timestamp,
	}
	attachMedia(&t, msg)
	if !d.record(t) {
		return
	}
	d.enqueue(t)
}

// bodyOrPlaceholder substitutes a short bracketed marker for a media event
// with no text body, derived from the msgtype alone. Text-like bodies are
// returned verbatim. It is internal/whatsapp's rule, the same one
// internal/matrix applies, so a trigger and the corresponding get_messages
// entry describe the same message identically.
func bodyOrPlaceholder(msgtype event.MessageType, body string) string {
	return whatsapp.Body(string(msgtype), body)
}

// attachMedia records the attachment an event carries, in the same fields and
// from the same content the tools report. A message with no attachment leaves
// all four fields out of the payload.
func attachMedia(t *Trigger, msg *event.MessageEventContent) {
	if msg.Info != nil {
		t.Mimetype = msg.Info.MimeType
		t.SizeBytes = int64(msg.Info.Size)
	}
	t.Filename = msg.FileName
	switch {
	case msg.File != nil && msg.File.URL != "":
		t.MediaURI = string(msg.File.URL)
	case msg.URL != "":
		t.MediaURI = string(msg.URL)
	}
}

func (d *Dispatcher) record(t Trigger) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t.MessageID != "" {
		for i := 0; i < d.count; i++ {
			if d.ring[i].MessageID == t.MessageID {
				return false
			}
		}
	}
	d.ring[d.next] = t
	d.next = (d.next + 1) % BufferSize
	if d.count < BufferSize {
		d.count++
	}
	return true
}

func (d *Dispatcher) enqueue(t Trigger) {
	if d.webhookURL == "" {
		return
	}
	d.lifeMu.Lock()
	defer d.lifeMu.Unlock()
	if !d.started || d.closed {
		return
	}
	select {
	case d.queue <- t:
	default:
		d.log.Printf("webhook queue full; dropping trigger %s", t.MessageID)
	}
}

// Recent returns triggers with Timestamp greater than since, newest first, at
// most limit of them (limit <= 0 or above BufferSize means BufferSize). The
// result is a copy and is never nil.
func (d *Dispatcher) Recent(since int64, limit int) []Trigger {
	if limit <= 0 || limit > BufferSize {
		limit = BufferSize
	}
	d.mu.RLock()
	out := make([]Trigger, 0, d.count)
	for i := 1; i <= d.count; i++ {
		t := d.ring[(d.next-i+BufferSize)%BufferSize]
		if t.Timestamp > since {
			out = append(out, t)
		}
	}
	d.mu.RUnlock()
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp > out[j].Timestamp })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Start launches the webhook workers. They exit when ctx is cancelled or Close
// is called. Start is a no-op after the first call or after Close.
func (d *Dispatcher) Start(ctx context.Context) {
	d.lifeMu.Lock()
	defer d.lifeMu.Unlock()
	if d.started || d.closed {
		return
	}
	d.started = true
	if d.webhookURL == "" {
		return
	}
	ctx, d.cancel = context.WithCancel(ctx)
	for range workerCount {
		d.wg.Add(1)
		go d.worker(ctx)
	}
}

// Close stops the workers and waits for them to exit. It is idempotent.
func (d *Dispatcher) Close() error {
	d.lifeMu.Lock()
	d.closed = true
	cancel := d.cancel
	d.lifeMu.Unlock()
	if cancel != nil {
		cancel()
	}
	d.wg.Wait()
	return nil
}

func (d *Dispatcher) worker(ctx context.Context) {
	defer d.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-d.queue:
			d.deliver(ctx, t)
		}
	}
}

func (d *Dispatcher) deliver(ctx context.Context, t Trigger) {
	body, err := json.Marshal(t)
	if err != nil {
		d.log.Printf("webhook encode failed for %s: %v", t.MessageID, err)
		return
	}
	for attempt := 0; ; attempt++ {
		err = d.post(ctx, body)
		if err == nil || ctx.Err() != nil {
			return
		}
		if attempt >= d.retries {
			d.log.Printf("webhook delivery failed for %s after %d retries: %v", t.MessageID, d.retries, err)
			return
		}
		timer := time.NewTimer(d.retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (d *Dispatcher) post(ctx context.Context, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return redactURLError(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("webhook returned status %d", resp.StatusCode)
	}
	return nil
}

// redactURLError strips the URL from a url.Error so the webhook URL (which may
// carry credentials in a query string or path) never appears in logs.
func redactURLError(err error) error {
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return fmt.Errorf("%s: %w", uerr.Op, uerr.Err)
	}
	return err
}
