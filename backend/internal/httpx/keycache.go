package httpx

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// KeyCache removes the database round trip that authentication otherwise costs
// on EVERY request.
//
// Measured on one developer machine before this existed:
//
//	/healthz                     25,451 req/s   (no auth, no query)
//	/v1/dashboard/charges          2,661 req/s   (auth + query)
//
// Roughly 90% of the throughput was going somewhere other than the work the
// caller asked for, and the API key lookup is a fixed part of that on every
// authenticated route in the system. It is also the least interesting query in
// the codebase: a unique-index point lookup returning three small columns that
// change perhaps once a year.
//
// THE TRADEOFF, STATED PLAINLY: a revoked key keeps working for up to one TTL.
// That is a real security cost and the reason the TTL is 30 seconds rather than
// an hour. Revocation is an incident response action — someone has just
// discovered a leaked key — and "it stops working within 30 seconds" is a
// defensible answer where "within an hour" is not.
//
// Deliberately not Redis. A Redis lookup replaces a Postgres round trip with a
// cheaper round trip, and a round trip is the thing being removed. Each pod
// keeping its own copy means every pod independently converges on the truth
// within one TTL, with no shared component to fail.
type KeyCache struct {
	ttl         time.Duration
	negativeTTL time.Duration

	mu      sync.RWMutex
	entries map[string]cacheEntry
}

type cacheEntry struct {
	merchantID uuid.UUID
	mode       string
	scope      string
	// found distinguishes a cached SUCCESS from a cached FAILURE. Both are
	// cached: without negative caching, an attacker spraying invalid keys sends
	// every one of them straight to Postgres, turning a guessing attempt into a
	// database denial of service.
	found     bool
	expiresAt time.Time
}

// NewKeyCache returns a cache with a 30s positive and 5s negative TTL.
//
// The negative TTL is shorter because the cost of being wrong runs the other
// way: a newly created key that briefly reads as invalid is a confusing bug for
// a merchant following the setup guide, and a 5s window is short enough that
// nobody notices while still absorbing a spray.
func NewKeyCache() *KeyCache {
	return &KeyCache{
		ttl:         30 * time.Second,
		negativeTTL: 5 * time.Second,
		entries:     make(map[string]cacheEntry),
	}
}

func (c *KeyCache) get(hash string) (cacheEntry, bool) {
	if c == nil {
		return cacheEntry{}, false
	}
	c.mu.RLock()
	e, ok := c.entries[hash]
	c.mu.RUnlock()

	// Expiry is checked on read rather than swept by a goroutine. The map only
	// grows with DISTINCT keys seen, which for a real deployment is the number
	// of merchant keys — small and bounded. Negative entries are the unbounded
	// case, which is why put() prunes.
	if !ok || time.Now().After(e.expiresAt) {
		return cacheEntry{}, false
	}
	return e, true
}

func (c *KeyCache) put(hash string, e cacheEntry) {
	if c == nil {
		return
	}
	ttl := c.ttl
	if !e.found {
		ttl = c.negativeTTL
	}
	e.expiresAt = time.Now().Add(ttl)

	c.mu.Lock()
	defer c.mu.Unlock()

	// A key-spraying attacker inserts a negative entry per distinct guess, so
	// the map would grow without bound. Rather than carry an LRU for a cache
	// whose real working set is a few dozen entries, drop everything expired
	// once it gets large — and if that is not enough, drop the lot. Refilling
	// costs one query per live key.
	if len(c.entries) > 10_000 {
		now := time.Now()
		for k, v := range c.entries {
			if now.After(v.expiresAt) {
				delete(c.entries, k)
			}
		}
		if len(c.entries) > 10_000 {
			c.entries = make(map[string]cacheEntry, 128)
		}
	}
	c.entries[hash] = e
}

// Invalidate drops one key immediately.
//
// Called when a key is revoked through the dashboard, which closes the TTL
// window for the common case — the pod that served the revocation stops
// honouring the key at once. Other pods still take up to one TTL, because there
// is no cross-pod invalidation here and adding one would reintroduce the shared
// component this design avoids.
func (c *KeyCache) Invalidate(rawOrHash string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.entries, rawOrHash)
	c.mu.Unlock()
}
