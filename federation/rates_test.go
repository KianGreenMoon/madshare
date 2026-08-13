//go:build !nofederation

package federation

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// The rate knobs (docs/architecture/swarm-admin.md §Rate limits): two node-wide
// caps, resolved override → config → unlimited, adjustable while the node runs.

func intp(n int) *int { return &n }

func TestSwarmRates_ResolutionOrder(t *testing.T) {
	cases := []struct {
		name             string
		cfgUp, cfgDown   int
		override         LimitOverrides
		wantUp, wantDown int64 // bytes/sec, 0 = unlimited
	}{
		{name: "no override uses the config file", cfgUp: 100, cfgDown: 50,
			wantUp: 100 * 1024, wantDown: 50 * 1024},
		{name: "an override wins", cfgUp: 100, cfgDown: 50,
			override: LimitOverrides{Up: intp(10), Down: intp(20)},
			wantUp:   10 * 1024, wantDown: 20 * 1024},
		{
			// The case the three-valued encoding exists for: 0 is not "unset", it
			// is how one node escapes a cap its config file ships with.
			name: "an explicit zero override means unlimited", cfgUp: 100, cfgDown: 100,
			override: LimitOverrides{Up: intp(0)},
			wantUp:   0, wantDown: 100 * 1024,
		},
		{name: "nothing anywhere is unlimited"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := newRateTestNode(tc.cfgUp, tc.cfgDown, func(context.Context) (LimitOverrides, error) {
				return tc.override, nil
			})
			n.refreshLimits(context.Background())
			up, down := n.SwarmRates()
			if up != tc.wantUp || down != tc.wantDown {
				t.Errorf("rates = up %d down %d, want %d/%d", up, down, tc.wantUp, tc.wantDown)
			}
		})
	}
}

// A resolver error must leave the cap that is in force alone. Falling back to
// the config values would quietly undo an operator's throttle the moment the
// database hiccuped — the opposite of what a cap is for.
func TestSwarmRates_AResolverErrorKeepsTheCapInForce(t *testing.T) {
	fail := false
	n := newRateTestNode(1000, 0, func(context.Context) (LimitOverrides, error) {
		if fail {
			return LimitOverrides{}, errors.New("database is away")
		}
		return LimitOverrides{Up: intp(10)}, nil
	})
	n.refreshLimits(context.Background())
	if up, _ := n.SwarmRates(); up != 10*1024 {
		t.Fatalf("up = %d, want the override", up)
	}

	fail = true
	n.ratesAt = time.Time{} // expire the memo
	n.refreshLimits(context.Background())
	if up, _ := n.SwarmRates(); up != 10*1024 {
		t.Errorf("up = %d after a failed read, want the override still in force", up)
	}
}

// The memo is what keeps a per-read resolution from being a per-read query.
func TestSwarmRates_ResolverIsMemoized(t *testing.T) {
	calls := 0
	n := newRateTestNode(0, 0, func(context.Context) (LimitOverrides, error) {
		calls++
		return LimitOverrides{}, nil
	})
	for i := 0; i < 50; i++ {
		n.refreshLimits(context.Background())
	}
	if calls != 1 {
		t.Errorf("resolver called %d times, want 1 within the memo window", calls)
	}
}

// Adjusting a live cap must not hand out a fresh burst: a rate limit an operator
// could reset by fidgeting is not a rate limit.
func TestRateLimiter_SetRateKeepsItsTokens(t *testing.T) {
	rl := newRateLimiter(1000) // 1000 B/s, bucket starts full
	if err := rl.wait(context.Background(), 1000); err != nil {
		t.Fatal(err)
	}
	// Bucket is now empty. Doubling the rate must not refill it.
	rl.setRate(2000)
	rl.mu.Lock()
	tokens := rl.tokens
	rl.mu.Unlock()
	if tokens > 100 { // a hair of accrual between the two calls is expected
		t.Errorf("tokens after setRate = %.0f, want ~0 — the bucket was refilled", tokens)
	}

	start := time.Now()
	if err := rl.wait(context.Background(), 2000); err != nil {
		t.Fatal(err)
	}
	// 2000 bytes at the NEW rate is ~1s; at the old one it would be ~2s.
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("wait took %v, want ~1s — the new rate did not take effect", elapsed)
	}
}

// Crossing to unlimited removes the limiter entirely, so the shipped default
// costs one atomic load and wraps nothing.
func TestAdjustableRate_UnlimitedHasNoLimiter(t *testing.T) {
	a := &adjustableRate{}
	if a.limiter() != nil {
		t.Fatal("a fresh adjustable rate should be unlimited")
	}
	a.set(1024)
	first := a.limiter()
	if first == nil {
		t.Fatal("set(1024) produced no limiter")
	}
	a.set(2048)
	if a.limiter() != first {
		t.Error("changing between two positive rates should adjust the bucket, not replace it")
	}
	a.set(0)
	if a.limiter() != nil {
		t.Error("set(0) should remove the limiter — 0 is unlimited")
	}
}

// The hazard the discount exists for: a throttled read looks exactly like a
// silent holder to the stall watchdog, and would get a healthy peer retired.
func TestThrottledReadIsNotCountedAsAStall(t *testing.T) {
	n := newRateTestNode(0, 4, nil) // 4 KiB/s inbound
	n.refreshLimits(context.Background())
	tr := newTransfer("hh", "", "")

	const size = 8 << 10 // 8 KiB at 4 KiB/s ≈ 2s of deliberate pausing
	body := io.NopCloser(strings.NewReader(strings.Repeat("x", size)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stalls := 0
	// A stall window far shorter than the throttling: without the discount the
	// watchdog fires, cancels the fetch and counts a stall against the holder.
	got, err := readStall(cancel, n.wire(ctx, tr, &BlobProvider{PublicKey: "aa"}, body),
		size, true, 300*time.Millisecond, func() { stalls++ })
	if err != nil {
		t.Fatalf("throttled read failed: %v", err)
	}
	if len(got) != size {
		t.Errorf("read %d bytes, want %d", len(got), size)
	}
	if stalls != 0 {
		t.Errorf("watchdog fired %d times — a limit we imposed is not evidence against a peer", stalls)
	}
	if tr.Received() != size {
		t.Errorf("received = %d, want %d — the throttle must not lose the count", tr.Received(), size)
	}
}

// The inbound cap binds the SUM of the parallel chunk workers, not each of
// them — a swarm fetch opens several Range requests by design, and a per-read
// bucket would multiply the operator's number by however many holders answered.
func TestInboundCapBindsTheSumOfParallelReads(t *testing.T) {
	n := newRateTestNode(0, 8, nil) // 8 KiB/s inbound; the bucket starts full
	n.refreshLimits(context.Background())

	const each = 8 << 10 // two workers, 16 KiB total: 8 KiB of burst + 8 KiB earned ≈ 1s
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr := newTransfer("hh", "", "")
			r := n.wire(context.Background(), tr, &BlobProvider{PublicKey: "aa"},
				strings.NewReader(strings.Repeat("x", each)))
			if _, err := io.ReadAll(r); err != nil {
				t.Errorf("read: %v", err)
			}
		}()
	}
	wg.Wait()

	elapsed := time.Since(start)
	if elapsed < 700*time.Millisecond {
		t.Errorf("two workers finished in %v — each appears to have its own bucket", elapsed)
	}
	if elapsed > 4*time.Second {
		t.Errorf("two workers took %v, want ~1s", elapsed)
	}
}

// The node's own cap applies to everyone, friends included: it is a statement
// about this node's pipe, not about who deserves what. (The member quotas, which
// friends DO bypass, are a separate chain — see quota.go.)
func TestNodeCapAppliesWithNoMemberBudget(t *testing.T) {
	n := newRateTestNode(64, 0, nil)
	n.refreshLimits(context.Background())
	if got := n.upLimiters(context.Background(), nil); len(got) != 1 {
		t.Errorf("outbound limiters = %d, want the node cap even with no member budget", len(got))
	}
	// And nothing is wrapped when the node is unlimited, which is the default.
	free := newRateTestNode(0, 0, nil)
	free.refreshLimits(context.Background())
	if got := free.upLimiters(context.Background(), nil); len(got) != 0 {
		t.Errorf("unlimited node produced %d limiters, want none", len(got))
	}
}

// newRateTestNode builds the minimum Node the rate machinery needs: no mesh, no
// store, no listener.
func newRateTestNode(cfgUpKiB, cfgDownKiB int, resolve func(context.Context) (LimitOverrides, error)) *Node {
	return &Node{
		upRate:        &adjustableRate{},
		downRate:      &adjustableRate{},
		cfgUpKiB:      cfgUpKiB,
		cfgDownKiB:    cfgDownKiB,
		limitResolver: resolve,
		traffic:       newTrafficTable(),
		logger:        log.New(io.Discard, "", 0),
	}
}

// TestAbortedThrottledWriteRefundsTokens: a wait cut short by the requester's
// context must give the tokens back — the write they were debited for never
// happens. The buckets include SHARED ones (global cap, member-class cap), so
// without the refund every aborted response depressed everyone's throughput by
// up to one write's worth per bucket (.issues/open-issues.md, swarm refactor
// pass finding 3). The second limiter is the one that blocks, so the test also
// pins the multi-limiter half: the first bucket already paid and must be
// refunded by the caller, not just the one whose wait failed.
func TestAbortedThrottledWriteRefundsTokens(t *testing.T) {
	shared := newRateLimiter(1 << 10) // burst = 1 KiB, starts full — pays instantly
	own := newRateLimiter(16)         // cannot cover the write, so its wait sleeps

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the requester is already gone

	w := throttled(&headerOnlyWriter{}, ctx, []*rateLimiter{shared, own})
	n, err := w.Write(make([]byte, 1<<10))
	if n != 0 || err == nil {
		t.Fatalf("Write = (%d, %v), want (0, the context error)", n, err)
	}

	if got := tokensOf(shared); got != 1<<10 {
		t.Errorf("shared bucket holds %.0f tokens after the aborted write, want its full %d back", got, 1<<10)
	}
	if got := tokensOf(own); got != 16 {
		t.Errorf("own bucket holds %.0f tokens after the aborted write, want its full 16 back", got)
	}
}

func tokensOf(rl *rateLimiter) float64 {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.tokens
}

// headerOnlyWriter is the least http.ResponseWriter a throttle test needs; a
// refunded write must never reach it, so Write failing loudly is a feature.
type headerOnlyWriter struct{ hdr http.Header }

func (h *headerOnlyWriter) Header() http.Header {
	if h.hdr == nil {
		h.hdr = http.Header{}
	}
	return h.hdr
}
func (h *headerOnlyWriter) WriteHeader(int) {}
func (h *headerOnlyWriter) Write(p []byte) (int, error) {
	return 0, errors.New("a throttled write reached the wire after its tokens were refunded")
}
