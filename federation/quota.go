//go:build !nofederation

package federation

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// What a member may cost us (F7 item 6, docs/architecture/federation-swarm.md
// §Distribution, "What a member may cost us").
//
// `seed_rate_kib` is one bucket for every requester, which was the whole of the
// policy while a requester was always a friend. Item 3 changed who may ask: this
// node serves its entire community, and membership deliberately has no admission
// cap. So the question stopped being *who gets in* and became *what one of them
// can cost*.
//
// Two things are bounded, each twice:
//
//   - bytes, because that is what seeding spends;
//   - concurrent blob serves, because that is what a swarm client multiplies BY
//     DESIGN — our own fetcher opens parallel Range requests across holders — and
//     therefore what one member most easily costs us in goroutines, file handles
//     and netstack connections.
//
// Per requester AND across the class, and both halves are load-bearing. The
// per-requester limit is fairness *within* the class: one member cannot take the
// whole budget. The class ceiling is the actual bound on harm, because a
// per-identity limit is exactly what a sybil farm defeats — N forged keys buy N
// quotas, and §The membership rule already declared the member count not to be
// the defense.
//
// Direct friends are outside all of it. A friend is an admin's decision, served
// under the global cap alone; that split is the anti-starvation rule as much as
// the anti-abuse one, since otherwise the nodes an admin chose queue behind the
// ones the graph let in.
//
// All four are three-layer knobs — runtime override → config → unlimited — and
// resolve on the same memo as the node-wide rates (rates.go). They shipped
// config-only, which put the one limiter family an operator reaches for while
// watching a node be drained behind an edit-and-restart; the layer rule in
// docs/architecture/swarm-admin.md §"Which layers a knob gets" is what said that
// was the wrong shape.

// quotaIdleTTL is how long a per-requester entry survives with nothing in
// flight. The table is keyed by mesh address, so without expiry it would grow
// with every member that ever fetched — but dropping an entry the moment it goes
// idle would hand back a full token bucket on every reconnect, which is a rate
// limit a requester can reset at will. Outliving a fetch by minutes costs a few
// hundred bytes and closes that.
const quotaIdleTTL = 5 * time.Minute

// quotaSweepThreshold is the table size that triggers a prune of idle entries.
// Sweeping on a size trigger rather than on a timer keeps this off the clock:
// a node nobody is fetching from does no work at all.
const quotaSweepThreshold = 1024

// quotas bounds what non-friends may cost this node. A nil *quotas, or one built
// from all-zero limits, admits everything — which is the shipped default.
//
// The four bounds are LIVE: an admin edits them on /admin/swarm and setLimits
// applies the new budget to the buckets already running (rates.go, §Rate limits
// — "a node being drained by members is exactly an operator watching
// something"). That is why the rates are adjustableRate and not bare limiters:
// crossing to or from unlimited swaps the bucket in or out, and a change between
// two positive rates keeps the tokens already accumulated, so nobody buys a
// fresh burst by provoking an edit.
type quotas struct {
	classRate *adjustableRate // bytes/sec across all non-friends

	mu     sync.Mutex
	limits QuotaLimits // KiB/s and counts in force; 0 = unlimited
	active int         // class-wide serves in flight
	nodes  map[string]*quotaNode
}

// quotaNode is one requester's state. Its limiter is created lazily and kept
// across fetches so the bucket cannot be reset by reconnecting.
type quotaNode struct {
	active int
	rate   *adjustableRate
	seen   time.Time
}

// newQuotas builds the member budget from config. All-zero yields a *quotas that
// admits everything and allocates no limiters — the unlimited default costs one
// nil check per request.
func newQuotas(l QuotaLimits) *quotas {
	q := &quotas{classRate: &adjustableRate{}, nodes: map[string]*quotaNode{}}
	q.setLimits(l)
	return q
}

// setLimits puts a new budget in force, including on the requesters already
// being served: a cap an operator has just tightened must bind the transfer that
// made them tighten it, not only the next one. Concurrency changes are not
// retroactive in the other direction — a lowered ceiling refuses the next
// admission rather than cutting a response already streaming, which is the same
// rule the rate caps follow.
func (q *quotas) setLimits(l QuotaLimits) {
	if q == nil {
		return
	}
	l = QuotaLimits{
		MemberRateKiB:         max(l.MemberRateKiB, 0),
		PerMemberRateKiB:      max(l.PerMemberRateKiB, 0),
		MemberMaxTransfers:    max(l.MemberMaxTransfers, 0),
		PerMemberMaxTransfers: max(l.PerMemberMaxTransfers, 0),
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.limits = l
	q.classRate.set(int64(l.MemberRateKiB) * 1024)
	for _, n := range q.nodes {
		n.rate.set(int64(l.PerMemberRateKiB) * 1024)
	}
}

// limits reports the budget in force, for the admin readout.
func (q *quotas) current() QuotaLimits {
	if q == nil {
		return QuotaLimits{}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.limits
}

// admit reserves one concurrent serve for the requester at key, returning the
// rate limiters its bytes must pass through and a release func. ok is false when
// either concurrency cap is full; the caller answers 429 and the swarm on the
// other side fails over to another holder, which is the honest outcome.
//
// key is the requester's mesh address — self-certifying, since an address
// derives from the node key, so it is an identity and not merely a hint.
func (q *quotas) admit(key string) (rls []*rateLimiter, release func(), ok bool) {
	if q == nil {
		return nil, func() {}, true
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.limits.MemberMaxTransfers > 0 && q.active >= q.limits.MemberMaxTransfers {
		return nil, nil, false
	}
	n := q.nodes[key]
	if n == nil {
		if len(q.nodes) >= quotaSweepThreshold {
			q.pruneLocked()
		}
		n = &quotaNode{rate: &adjustableRate{}}
		n.rate.set(int64(q.limits.PerMemberRateKiB) * 1024)
		q.nodes[key] = n
	}
	if q.limits.PerMemberMaxTransfers > 0 && n.active >= q.limits.PerMemberMaxTransfers {
		return nil, nil, false
	}

	q.active++
	n.active++
	n.seen = time.Now()
	if rl := q.classRate.limiter(); rl != nil {
		rls = append(rls, rl)
	}
	if rl := n.rate.limiter(); rl != nil {
		rls = append(rls, rl)
	}

	var once sync.Once
	return rls, func() {
		once.Do(func() {
			q.mu.Lock()
			defer q.mu.Unlock()
			q.active--
			if n.active--; n.active <= 0 {
				n.seen = time.Now() // start the idle clock, keep the bucket
			}
		})
	}, true
}

// pruneLocked drops entries with nothing in flight and no recent activity.
// Callers hold q.mu.
func (q *quotas) pruneLocked() {
	cutoff := time.Now().Add(-quotaIdleTTL)
	for key, n := range q.nodes {
		if n.active == 0 && n.seen.Before(cutoff) {
			delete(q.nodes, key)
		}
	}
}

// admitServe applies the member budget to one incoming blob request. A direct
// friend bypasses it entirely and is served as it always was. The returned
// release must be called when the response is finished.
func (n *Node) admitServe(r *http.Request, aud Audience) (rls []*rateLimiter, release func(), ok bool) {
	if aud.IsFriend() {
		return nil, func() {}, true
	}
	// Resolve before admitting, not only before writing: the concurrency halves
	// of the budget are spent HERE, so an admin who has just lowered the ceiling
	// would otherwise see it bind at the first byte of the next response instead
	// of at this admission. Memoized, so this is a mutex and a clock comparison.
	n.refreshLimits(r.Context())
	ip := remoteIP(r)
	if ip == nil {
		// No source address to account against. Refusing would be safer in the
		// abstract and wrong in practice: this cannot happen over the mesh
		// listener, and a mesh request without a parseable peer address has
		// already failed the audience resolution above.
		return nil, func() {}, true
	}
	return n.quotas.admit(ip.String())
}

// throttled wraps w so the body passes through every limiter in rls. An empty
// list returns w unchanged — the unlimited default adds no layer at all.
func throttled(w http.ResponseWriter, ctx context.Context, rls []*rateLimiter) http.ResponseWriter {
	if len(rls) == 0 {
		return w
	}
	return &throttledResponseWriter{ResponseWriter: w, rls: rls, ctx: ctx}
}
