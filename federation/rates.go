//go:build !nofederation

package federation

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// The node's two rate caps (docs/architecture/swarm-admin.md §Rate limits).
//
// Until now there was one, `seed_rate_kib`, built once in Start — so changing
// what this node costs its uplink meant editing a file and restarting, which is
// exactly the wrong shape for the knob an operator reaches for *while* the link
// is saturated. And there was no inbound cap at all.
//
// Two numbers, node-wide, and no per-file ones (owner, 2026-08-06): a cap
// protects the LINE, which every transfer shares, so only the sum means
// anything. Fairness between peers is a different question and already has an
// answer — the member quotas in quota.go.
//
// Resolution is `runtime override → config → unlimited`. The override lives in
// the database, which this package deliberately knows nothing about: it arrives
// through WithRateResolver, the same shape as WithBlobResolver.

// rateMemoTTL bounds how often the override is read. Every blob request already
// reads the seeding policy; this must not add a second per-request query, and
// five seconds is far finer than any hand on a slider.
const rateMemoTTL = 5 * time.Second

// adjustableRate is one live cap. Unlimited is represented by a nil limiter
// rather than a zero-rate one, so the shipped default costs a single atomic load
// and adds no wrapper to the write path at all.
type adjustableRate struct {
	mu   sync.Mutex
	rate int64 // bytes/sec, guarded by mu; 0 = unlimited
	cur  atomic.Pointer[rateLimiter]
}

// set applies a new cap. A change between two positive rates adjusts the
// existing bucket (keeping its tokens); crossing to or from unlimited swaps the
// limiter in or out, since there is no bucket to preserve on that edge.
func (a *adjustableRate) set(bytesPerSec int64) {
	if bytesPerSec < 0 {
		bytesPerSec = 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rate == bytesPerSec {
		return
	}
	a.rate = bytesPerSec
	if bytesPerSec == 0 {
		a.cur.Store(nil)
		return
	}
	if rl := a.cur.Load(); rl != nil {
		rl.setRate(bytesPerSec)
		return
	}
	a.cur.Store(newRateLimiter(bytesPerSec))
}

// limiter is the live bucket, or nil when this direction is unlimited.
func (a *adjustableRate) limiter() *rateLimiter { return a.cur.Load() }

// bytesPerSec reports the cap in force, for diagnostics.
func (a *adjustableRate) bytesPerSec() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rate
}

// RateOverrides lives in federation.go, beside the other shapes that cross into
// `api` and main: the option that carries it is declared there too, so both must
// exist under -tags nofederation, where this file is gone.

// refreshRates re-resolves both caps if the memo has expired. Cheap enough to
// call per read: an unexpired memo is one mutex and a time comparison.
//
// The stamp is taken BEFORE the resolver runs, so a burst of concurrent chunk
// reads produces one query rather than one each.
func (n *Node) refreshRates(ctx context.Context) {
	n.rateMu.Lock()
	if !n.ratesAt.IsZero() && time.Since(n.ratesAt) < rateMemoTTL {
		n.rateMu.Unlock()
		return
	}
	n.ratesAt = time.Now()
	resolve := n.rateResolver
	n.rateMu.Unlock()

	up, down := n.cfgUpKiB, n.cfgDownKiB
	if resolve != nil {
		ov, err := resolve(ctx)
		if err != nil {
			// Keep whatever is in force and say so. Falling back to the config
			// values silently would quietly undo an operator's throttle the moment
			// the database hiccuped, which is the opposite of what a cap is for.
			n.logger.Printf("federation: read swarm rate overrides: %v", err)
			return
		}
		if ov.Up != nil {
			up = *ov.Up
		}
		if ov.Down != nil {
			down = *ov.Down
		}
	}
	n.upRate.set(int64(up) * 1024)
	n.downRate.set(int64(down) * 1024)
}

// upLimiters is the whole outbound rate policy for one blob response, resolved
// fresh on every write: the node cap (which an admin may have just changed) plus
// whatever the member budget added for this requester. Empty means nothing is
// capped, and then the response writer is not wrapped at all.
func (n *Node) upLimiters(ctx context.Context, extra []*rateLimiter) []*rateLimiter {
	n.refreshRates(ctx)
	rl := n.upRate.limiter()
	if rl == nil {
		return extra
	}
	return append([]*rateLimiter{rl}, extra...)
}

// downLimiter is the inbound cap in force, or nil when fetching is unlimited.
// There is no per-requester counterpart: the peers we fetch FROM are not
// spending our budget on their own behalf, we are.
func (n *Node) downLimiter(ctx context.Context) *rateLimiter {
	n.refreshRates(ctx)
	return n.downRate.limiter()
}

// SwarmRates reports the caps in force, in bytes/sec (0 = unlimited), for the
// admin surface — what the node is ACTUALLY limiting to right now, resolved
// through override → config, rather than what any one layer says.
//
// It resolves before answering rather than reading whatever the last blob
// request happened to leave behind. Without that the admin page reported the
// config value while a stored override said something else — and, worse, the
// override only took effect at the next transfer, because nothing else on this
// node ever asks. Memoized like every other resolution, so a page that polls
// costs one query every few seconds.
func (n *Node) SwarmRates() (up, down int64) {
	n.refreshRates(context.Background())
	return n.upRate.bytesPerSec(), n.downRate.bytesPerSec()
}

// RefreshRates re-reads the overrides NOW, past the memo. The admin surface
// calls it after writing a cap so the new value is in force immediately rather
// than up to rateMemoTTL later — a rate limit that takes effect "soon" is not
// what someone watching a saturated link is asking for.
func (n *Node) RefreshRates() {
	n.rateMu.Lock()
	n.ratesAt = time.Time{}
	n.rateMu.Unlock()
	n.refreshRates(context.Background())
}
