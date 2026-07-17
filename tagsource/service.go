package tagsource

import (
	"context"
	"sync"
	"time"
)

// Shared plumbing for the external lookup clients (AcoustID, MusicBrainz):
// a serializing per-provider rate limiter and a small TTL cache. Both
// services enforce ~1 req/s per IP, shared with every other app on the host,
// so requests are spaced — not just capped — and rapid repeats are served
// from memory without going out at all.

// svcLimiter reserves spaced start slots for outbound requests. Callers whose
// slot would begin beyond maxWait get ErrBusy immediately instead of queueing
// unboundedly against a shared per-IP limit.
type svcLimiter struct {
	interval time.Duration // spacing between request starts
	maxWait  time.Duration // queue budget before ErrBusy

	mu   sync.Mutex
	next time.Time
}

// acquire blocks until the caller's reserved slot opens, or returns ErrBusy /
// the context error.
func (l *svcLimiter) acquire(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	start := l.next
	if start.Before(now) {
		start = now
	}
	wait := start.Sub(now)
	if wait > l.maxWait {
		l.mu.Unlock()
		return ErrBusy
	}
	l.next = start.Add(l.interval)
	l.mu.Unlock()

	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// svcCache is a mutex-guarded TTL cache for successful lookups, capped by
// entry count (expired entries are evicted first, then the soonest-to-expire).
type svcCache struct {
	ttl time.Duration
	cap int

	mu sync.Mutex
	m  map[string]svcCacheEntry
}

type svcCacheEntry struct {
	suggestions []Suggestion
	expires     time.Time
}

// get returns a fresh cache hit.
func (c *svcCache) get(key string) ([]Suggestion, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return e.suggestions, true
}

// put caches a successful lookup.
func (c *svcCache) put(key string, s []Suggestion) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string]svcCacheEntry)
	}
	now := time.Now()
	if len(c.m) >= c.cap {
		for k, e := range c.m {
			if now.After(e.expires) {
				delete(c.m, k)
			}
		}
	}
	if len(c.m) >= c.cap {
		var oldest string
		var oldestExp time.Time
		for k, e := range c.m {
			if oldest == "" || e.expires.Before(oldestExp) {
				oldest, oldestExp = k, e.expires
			}
		}
		delete(c.m, oldest)
	}
	c.m[key] = svcCacheEntry{suggestions: s, expires: now.Add(c.ttl)}
}
