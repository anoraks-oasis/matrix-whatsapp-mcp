package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

const (
	// idempotencyTTL is how long a recorded send stays replayable. Ten minutes
	// covers an agent's retry loop and a client reconnect, without letting a
	// stale key shadow a message the caller deliberately repeats later.
	idempotencyTTL = 10 * time.Minute
	// maxIdempotencyKeyLen bounds the caller's key. A key only has to be
	// unique inside one retry loop, so a short cap keeps both the map and its
	// error messages predictable.
	maxIdempotencyKeyLen = 128
	// idempotencyEntries caps the map. At one send per three seconds this is
	// hours of traffic; the cap exists so a caller that invents a fresh key per
	// call still cannot grow the map without limit.
	idempotencyEntries = 256
)

// errIdempotencyConflict is returned when one key is used for two different
// messages. Sending the second would defeat the reason for the key: a caller
// reusing a key has a bug, and silently posting is the one outcome that costs
// a real person a duplicate message.
var errIdempotencyConflict = errors.New("idempotency key already used for a different message")

// idempotentSend is one claimed key. It is guarded by idempotencyCache.mu: an
// unsettled entry is a send in flight, a settled one is a result to replay.
type idempotentSend struct {
	key     string
	roomID  string
	text    string
	at      time.Time
	done    chan struct{}
	settled bool
	eventID string
}

// sendClaim is the result of claiming a key: either a replay of an earlier
// send, which must not be sent again, or a reservation the caller owns until it
// settles or releases it.
type sendClaim struct {
	replayed bool
	eventID  string
	entry    *idempotentSend
}

// idempotencyCache remembers the event ID of recent sends so a retried
// send_message returns the original event instead of posting a second message.
// It is in-process and bounded with no persistence: a restart forgets every
// key, and the oldest recorded send is evicted once the cache is full.
type idempotencyCache struct {
	mu      sync.Mutex
	now     func() time.Time
	ttl     time.Duration
	max     int
	entries map[string]*idempotentSend
}

// newIdempotencyCache returns a cache with the shipped limits. Tests shorten
// ttl, max or replace now to exercise expiry and eviction without sleeping.
func newIdempotencyCache() *idempotencyCache {
	return &idempotencyCache{
		now:     time.Now,
		ttl:     idempotencyTTL,
		max:     idempotencyEntries,
		entries: make(map[string]*idempotentSend),
	}
}

// claim reserves key for one send of roomID+text. An identical send recorded
// within the TTL is returned as a replay; an identical send still in flight is
// waited for (so two concurrent retries cannot both reach the homeserver) and
// then replayed. Only a settled entry expires: an in-flight send stays
// claimable for as long as it takes, so a hung connection cannot turn a retry
// into a second message. A key already used for different content is
// errIdempotencyConflict rather than a second send.
func (c *idempotencyCache) claim(ctx context.Context, key, roomID, text string) (*sendClaim, error) {
	for {
		c.mu.Lock()
		entry, ok := c.entries[key]
		if ok && c.expired(entry) && entry.settled {
			// Only a settled entry may expire. An entry whose send is still
			// in flight is kept, because the caller waiting on it must replay
			// that send's event: dropping it here would let a retry reserve
			// the key a second time and post the message twice.
			delete(c.entries, key)
			ok = false
		}
		if ok {
			if entry.roomID != roomID || entry.text != text {
				c.mu.Unlock()
				return nil, fmt.Errorf("%w: pass the original chat and text to get its message ID back, or use a new key", errIdempotencyConflict)
			}
			if entry.settled {
				claim := &sendClaim{replayed: true, eventID: entry.eventID}
				c.mu.Unlock()
				return claim, nil
			}
			done := entry.done
			c.mu.Unlock()
			select {
			case <-done:
				// The send finished: re-read the entry, which is now settled
				// (or gone, if it failed and released the key).
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		entry = &idempotentSend{key: key, roomID: roomID, text: text, at: c.now(), done: make(chan struct{})}
		c.entries[key] = entry
		c.trimLocked()
		c.mu.Unlock()
		return &sendClaim{entry: entry}, nil
	}
}

// settle records the event ID a claimed send produced and wakes any waiter.
func (c *idempotencyCache) settle(claim *sendClaim, eventID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := claim.entry
	if entry.settled {
		return
	}
	entry.settled = true
	entry.eventID = eventID
	entry.at = c.now()
	// A key reclaimed while this send was still in flight belongs to the newer
	// send now; record nothing rather than overwrite a message that may itself
	// have been delivered.
	if current, ok := c.entries[entry.key]; !ok || current == entry {
		c.entries[entry.key] = entry
		c.trimLocked()
	}
	close(entry.done)
}

// release gives a claimed key back after a send that never happened, so the
// caller can retry the same key instead of being told its own retry is a
// different message. A settled claim is left alone, which makes the deferred
// release after a successful send a no-op.
func (c *idempotencyCache) release(claim *sendClaim) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := claim.entry
	if entry.settled {
		return
	}
	entry.settled = true
	if c.entries[entry.key] == entry {
		delete(c.entries, entry.key)
	}
	close(entry.done)
}

func (c *idempotencyCache) expired(entry *idempotentSend) bool {
	return c.now().Sub(entry.at) >= c.ttl
}

// trimLocked evicts the oldest settled entries until the map fits. An entry
// whose send is still in flight is never evicted: its caller is waiting on it.
func (c *idempotencyCache) trimLocked() {
	for len(c.entries) > c.max {
		var oldest *idempotentSend
		for _, entry := range c.entries {
			if !entry.settled {
				continue
			}
			if oldest == nil || entry.at.Before(oldest.at) {
				oldest = entry
			}
		}
		if oldest == nil {
			return
		}
		delete(c.entries, oldest.key)
	}
}
