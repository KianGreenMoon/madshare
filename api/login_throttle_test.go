package api

import (
	"testing"
	"time"
)

func TestLoginThrottleAllowIP(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tt := newLoginThrottle()
	tt.now = func() time.Time { return now }

	// A fresh IP gets a full burst, then is denied.
	for i := 0; i < loginBurst; i++ {
		if !tt.allowIP("10.0.0.1") {
			t.Fatalf("attempt %d should be allowed within the burst", i+1)
		}
	}
	if tt.allowIP("10.0.0.1") {
		t.Fatal("attempt past the burst should be denied")
	}

	// A different IP has its own independent bucket.
	if !tt.allowIP("10.0.0.2") {
		t.Fatal("a separate IP should not be throttled by the first")
	}

	// After enough time passes, the bucket refills at least one token.
	now = now.Add(time.Minute)
	if !tt.allowIP("10.0.0.1") {
		t.Fatal("bucket should refill after a minute")
	}
}

func TestLoginThrottleExemptsLoopback(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tt := newLoginThrottle()
	tt.now = func() time.Time { return now }

	// A loopback peer (a local reverse proxy, or a local operator) is never
	// bucketed: well past the burst it is still allowed, and it allocates no
	// bucket, so it cannot throttle the many users that share that address.
	for _, ip := range []string{"127.0.0.1", "::1"} {
		for i := 0; i < loginBurst*3; i++ {
			if !tt.allowIP(ip) {
				t.Fatalf("loopback %s attempt %d should always be allowed", ip, i+1)
			}
		}
	}
	tt.mu.Lock()
	n := len(tt.buckets)
	tt.mu.Unlock()
	if n != 0 {
		t.Fatalf("loopback peers should allocate no buckets, got %d", n)
	}
}

func TestLoginThrottleAcquire(t *testing.T) {
	tt := newLoginThrottle()

	releases := make([]func(), 0, loginMaxInFlight)
	for i := 0; i < loginMaxInFlight; i++ {
		rel, ok := tt.acquire()
		if !ok {
			t.Fatalf("acquire %d should succeed up to the cap", i+1)
		}
		releases = append(releases, rel)
	}
	if _, ok := tt.acquire(); ok {
		t.Fatal("acquire past the cap should fail")
	}
	releases[0]() // free one slot
	rel, ok := tt.acquire()
	if !ok {
		t.Fatal("acquire should succeed after a release")
	}
	rel()
	for _, r := range releases[1:] {
		r()
	}
}

func TestLoginThrottleSweep(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	tt := newLoginThrottle()
	tt.now = func() time.Time { return now }

	tt.allowIP("10.0.0.9")
	now = now.Add(bucketIdleTTL + time.Minute)
	tt.mu.Lock()
	tt.sweep(now)
	_, present := tt.buckets["10.0.0.9"]
	tt.mu.Unlock()
	if present {
		t.Fatal("idle bucket should have been swept")
	}
}
