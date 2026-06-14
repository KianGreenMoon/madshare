package api

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Login throttle tuning. The login endpoint is unauthenticated and every
// attempt triggers a deliberately expensive argon2id verification, so it is
// both an online brute-force target and a resource-exhaustion vector (each
// verify costs ~64 MiB). loginThrottle caps both axes.
const (
	loginBurst        = 10            // attempts an idle IP may make immediately
	loginRefillPerSec = 10.0 / 60.0   // sustained rate: ~10 attempts / minute / IP
	loginMaxInFlight  = 8             // concurrent password verifications, server-wide
	bucketIdleTTL     = 10 * time.Minute
	bucketSweepAt     = 4096          // sweep idle IP buckets once the map grows past this
)

// loginThrottle defends the login endpoint against brute-force and flood
// attacks. It combines a per-IP token bucket (slows guessing from any single
// source) with a global semaphore that bounds how many argon2 verifications run
// at once (so a flood cannot exhaust memory/CPU). It is keyed on the request's
// source IP: effective for direct and Yggdrasil binds; behind a reverse proxy,
// the proxy reports its own address, so rate-limit at the proxy as well.
type loginThrottle struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	sem     chan struct{}
	now     func() time.Time // injected in tests
}

type tokenBucket struct {
	tokens   float64
	lastFill time.Time
	lastSeen time.Time
}

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{
		buckets: map[string]*tokenBucket{},
		sem:     make(chan struct{}, loginMaxInFlight),
		now:     time.Now,
	}
}

// allowIP reports whether ip may make another login attempt now, consuming one
// token when it does. A previously unseen IP starts with a full burst.
func (t *loginThrottle) allowIP(ip string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	b := t.buckets[ip]
	if b == nil {
		if len(t.buckets) >= bucketSweepAt {
			t.sweep(now)
		}
		b = &tokenBucket{tokens: loginBurst, lastFill: now, lastSeen: now}
		t.buckets[ip] = b
	}
	if elapsed := now.Sub(b.lastFill).Seconds(); elapsed > 0 {
		b.tokens += elapsed * loginRefillPerSec
		if b.tokens > loginBurst {
			b.tokens = loginBurst
		}
		b.lastFill = now
	}
	b.lastSeen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets that have not been touched within bucketIdleTTL, bounding
// memory growth under a high-cardinality source-IP flood. Caller holds t.mu.
func (t *loginThrottle) sweep(now time.Time) {
	for ip, b := range t.buckets {
		if now.Sub(b.lastSeen) > bucketIdleTTL {
			delete(t.buckets, ip)
		}
	}
}

// acquire reserves a verification slot, returning a release func and true, or
// (nil, false) when the server is already at loginMaxInFlight verifications.
func (t *loginThrottle) acquire() (release func(), ok bool) {
	select {
	case t.sem <- struct{}{}:
		return func() { <-t.sem }, true
	default:
		return nil, false
	}
}

// clientIP returns the source IP of r, stripping the port. Mirrors the address
// handling in the allow_from filter (madshare.go).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
