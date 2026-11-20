package mcpserver

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

// testCache returns a cache with a controllable clock and the given ttl/size.
// The clock starts at a fixed instant so expiry and eviction are deterministic
// and no test has to sleep.
func testCache(ttl time.Duration, max int) (*idempotencyCache, func(time.Duration)) {
	now := time.Unix(1_700_000_000, 0)
	cache := &idempotencyCache{now: func() time.Time { return now }, ttl: ttl, max: max, entries: make(map[string]*idempotentSend)}
	return cache, func(d time.Duration) { now = now.Add(d) }
}

// mustClaim claims key and fails the test if the claim is refused.
func mustClaim(t *testing.T, cache *idempotencyCache, key, roomID, text string) *sendClaim {
	t.Helper()
	claim, err := cache.claim(context.Background(), key, roomID, text)
	if err != nil {
		t.Fatalf("claim(%q): %v", key, err)
	}
	if claim.replayed {
		t.Fatalf("claim(%q) replayed %q, want a fresh reservation", key, claim.eventID)
	}
	return claim
}

func TestIdempotencyCacheReplaysWithinTTL(t *testing.T) {
	cache, advance := testCache(10*time.Minute, 8)

	cache.settle(mustClaim(t, cache, "k", "!r:test", "hello"), "$e1")
	advance(9 * time.Minute)

	claim, err := cache.claim(context.Background(), "k", "!r:test", "hello")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !claim.replayed || claim.eventID != "$e1" {
		t.Fatalf("claim = %+v, want a replay of $e1", claim)
	}

	advance(2 * time.Minute)
	claim, err = cache.claim(context.Background(), "k", "!r:test", "hello")
	if err != nil {
		t.Fatalf("claim after ttl: %v", err)
	}
	if claim.replayed {
		t.Fatalf("claim after the ttl replayed %q, want a new send", claim.eventID)
	}
}

func TestIdempotencyCacheConflict(t *testing.T) {
	cache, _ := testCache(10*time.Minute, 8)
	cache.settle(mustClaim(t, cache, "k", "!r:test", "hello"), "$e1")

	for _, tc := range []struct{ name, roomID, text string }{
		{"different text", "!r:test", "something else"},
		{"different room", "!other:test", "hello"},
	} {
		claim, err := cache.claim(context.Background(), "k", tc.roomID, tc.text)
		if !errors.Is(err, errIdempotencyConflict) {
			t.Fatalf("%s: claim = %+v, %v; want errIdempotencyConflict", tc.name, claim, err)
		}
	}
}

// TestIdempotencyCacheReleaseAllowsRetry covers a send that never happened: the
// key is free again, so the caller's retry is a send and not a conflict.
func TestIdempotencyCacheReleaseAllowsRetry(t *testing.T) {
	cache, _ := testCache(10*time.Minute, 8)

	claim := mustClaim(t, cache, "k", "!r:test", "hello")
	cache.release(claim)
	cache.release(claim) // the handler's deferred release after a failed send is idempotent

	retry := mustClaim(t, cache, "k", "!r:test", "hello")
	cache.settle(retry, "$e2")

	replay, err := cache.claim(context.Background(), "k", "!r:test", "hello")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !replay.replayed || replay.eventID != "$e2" {
		t.Fatalf("claim = %+v, want the retried send replayed", replay)
	}
}

// TestIdempotencyCacheWaitsForInFlightSend proves two concurrent retries cannot
// both reach the homeserver: the second waits for the first and then replays it.
func TestIdempotencyCacheWaitsForInFlightSend(t *testing.T) {
	cache, _ := testCache(10*time.Minute, 8)
	first := mustClaim(t, cache, "k", "!r:test", "hello")

	type result struct {
		claim *sendClaim
		err   error
	}
	waiting := make(chan result, 1)
	go func() {
		claim, err := cache.claim(context.Background(), "k", "!r:test", "hello")
		waiting <- result{claim, err}
	}()

	select {
	case got := <-waiting:
		t.Fatalf("the second claim returned %+v before the first send finished", got.claim)
	case <-time.After(20 * time.Millisecond):
	}

	cache.settle(first, "$e1")

	select {
	case got := <-waiting:
		if got.err != nil {
			t.Fatalf("waiting claim: %v", got.err)
		}
		if !got.claim.replayed || got.claim.eventID != "$e1" {
			t.Fatalf("waiting claim = %+v, want a replay of $e1", got.claim)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting claim never returned after the send settled")
	}
}

// TestIdempotencyCacheInFlightEntrySurvivesTTL is the expiry twin of
// TestIdempotencyCacheEvictionSkipsInFlightEntries: a send can sit in flight
// past the ten-minute TTL on a hung connection, and the retry that arrives
// during that window must wait for it and replay its event instead of claiming
// the key and posting the message a second time.
func TestIdempotencyCacheInFlightEntrySurvivesTTL(t *testing.T) {
	cache, advance := testCache(10*time.Minute, 8)
	inFlight := mustClaim(t, cache, "k", "!r:test", "hello")

	// The send is still in flight when its entry would otherwise expire.
	advance(10*time.Minute + time.Second)

	type result struct {
		claim *sendClaim
		err   error
	}
	waited := make(chan result, 1)
	go func() {
		claim, err := cache.claim(context.Background(), "k", "!r:test", "hello")
		waited <- result{claim, err}
	}()

	select {
	case got := <-waited:
		t.Fatalf("the second claim returned %+v while the first send was still in flight, want it to wait", got.claim)
	case <-time.After(50 * time.Millisecond):
	}

	cache.settle(inFlight, "$e1")

	select {
	case got := <-waited:
		if got.err != nil {
			t.Fatalf("the waiting claim errored: %v", got.err)
		}
		if !got.claim.replayed || got.claim.eventID != "$e1" {
			t.Fatalf("waiting claim = %+v, want a replay of $e1", got.claim)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the waiting claim never returned after the send settled")
	}

	// The retry that waited and the one after it both replay the single send.
	replay, err := cache.claim(context.Background(), "k", "!r:test", "hello")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if !replay.replayed || replay.eventID != "$e1" {
		t.Fatalf("third claim = %+v, want a replay of $e1", replay)
	}
}

func TestIdempotencyCacheBounds(t *testing.T) {
	cache, advance := testCache(10*time.Minute, 2)

	for _, key := range []string{"a", "b", "c"} {
		cache.settle(mustClaim(t, cache, key, "!r:test", "hello"), "$"+key)
		advance(time.Second)
	}
	if len(cache.entries) != 2 {
		t.Fatalf("cache holds %d entries, want the cap of 2", len(cache.entries))
	}

	// "b" and "c" are still replayable; "a" was evicted.
	for _, key := range []string{"b", "c"} {
		claim, err := cache.claim(context.Background(), key, "!r:test", "hello")
		if err != nil {
			t.Fatalf("claim(%q): %v", key, err)
		}
		if !claim.replayed || claim.eventID != "$"+key {
			t.Fatalf("claim(%q) = %+v, want it replayed", key, claim)
		}
	}
	claim, err := cache.claim(context.Background(), "a", "!r:test", "hello")
	if err != nil {
		t.Fatalf("claim(a): %v", err)
	}
	if claim.replayed {
		t.Fatalf("claim(a) replayed %q, want a new send after eviction", claim.eventID)
	}
}

// TestIdempotencyCacheEvictionSkipsInFlightEntries makes sure the cap never
// evicts a key whose caller is still waiting on it.
func TestIdempotencyCacheEvictionSkipsInFlightEntries(t *testing.T) {
	cache, advance := testCache(10*time.Minute, 1)
	inFlight := mustClaim(t, cache, "busy", "!r:test", "hello")
	advance(time.Second)

	for i := 0; i < 3; i++ {
		cache.settle(mustClaim(t, cache, "k"+strconv.Itoa(i), "!r:test", "hello"), "$e")
		advance(time.Second)
	}

	cache.mu.Lock()
	kept := cache.entries["busy"] == inFlight.entry
	cache.mu.Unlock()
	if !kept {
		t.Fatal("the in-flight entry was evicted while its caller was waiting on it")
	}

	cache.settle(inFlight, "$busy")
	claim, err := cache.claim(context.Background(), "busy", "!r:test", "hello")
	if err != nil {
		t.Fatalf("claim(busy): %v", err)
	}
	if !claim.replayed || claim.eventID != "$busy" {
		t.Fatalf("claim(busy) = %+v, want $busy replayed", claim)
	}
}
