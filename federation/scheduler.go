//go:build !nofederation

package federation

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// The chunk scheduler (federation F9 item 3). Design:
// docs/architecture/federation-swarm.md §"Making it a swarm", item 3.
//
// It decides two things a fetch cannot avoid deciding: which chunk to ask for
// next, and whom to ask. Until F9 those were separate — a chunk came off a
// queue, a holder came off a round-robin cursor — and both halves of that were
// wrong for the swarm this has become.
//
// **Round-robin is speed-blind.** Every live holder took an equal share of
// dispatches however badly it was doing, so a holder that never answers was
// discovered only by burning its whole per-chunk budget, repeatedly. Measured:
// one dead entry in a plan cost ~150× the entire clean fetch
// (.issues/open-issues.md, "a fetch plan names holders that have been gone for
// days"). Dispatch now goes to whoever has the FEWEST BYTES OUTSTANDING, which
// is self-correcting and needs no decay constant anyone would have to guess: a
// holder that is not delivering keeps its bytes outstanding and stops being
// asked, without anyone having to decide it is slow.
//
// **"Pick a provider, then a chunk" stopped being true** when F9 item 1 made a
// downloader seed what it has so far. Not every holder can serve every chunk, so
// the scheduler selects the PAIR: a holder is asked for a chunk its advertised
// coverage covers (GET /madnetwork/v0/have/{hash}), and a chunk goes to the
// least-loaded holder that has it.
//
// What is deliberately NOT here: rarest-first. It only becomes meaningful with
// partial holders, and the swarms this serves are small — the design says to
// defer it to a measurement rather than ship it on principle, and
// fewest-outstanding may well be enough.

const (
	// providerFailureLimit is how many consecutive failures put a holder out of
	// rotation — relative to its peers, see worseThanPeers.
	providerFailureLimit = 4

	// maxBackoffSteps caps the doubling of Timeouts.Retry: 8× the base, after
	// which a holder is being retired anyway and a longer pause would only make
	// its last chances arrive too late to matter.
	maxBackoffSteps = 3

	// busyBackoffSteps is what a 429 buys instead of a failure. A quota refusal
	// is a fact about the holder's CURRENT load and nothing else (F7 item 6), so
	// it costs a wait rather than a mark — but a longer wait than a transient
	// error, because the condition it reports is one somebody else has to finish
	// before it clears.
	busyBackoffSteps = 2

	// rateAlpha weights the newest chunk in a holder's throughput estimate. High,
	// because a transfer completes in a handful of chunks per holder and an
	// average that needs ten samples would still be warming up when the fetch
	// ends.
	rateAlpha = 0.5

	// maxHolderRequests is how many chunks one holder may be fetching at once —
	// the pipelining half of F9 item 4, and it came out of the measurement
	// pointing the OPPOSITE way to the design's expectation.
	//
	// The design asked for "queue depth ≥ 2 per holder, so the pipe stays full
	// across the RTT". Measured over a 300 ms-RTT link capped at 512 KiB/s
	// (4 MiB, one holder), depth 1 / 2 / 4 took 12.36 s / 12.30 s / 12.80 s: the
	// dead air is real and it is not the bottleneck, because a link with any
	// queueing in it absorbs the gap. Depth 4 started costing retries and depth 8
	// **failed the transfer outright** — eight chunks sharing one capped link each
	// take eight times as long, which spends Timeouts.PerChunk rather than the
	// link, so every chunk times out at once and the swarm falls back to the
	// whole-file path.
	//
	// That is reachable in an ordinary plan: workers are capped in TOTAL, so four
	// advertised holders of which one answers put all eight workers on the
	// survivor. Hence a cap per holder rather than a floor: 2 is what the pipe can
	// use, and the third request is one the per-chunk budget pays for.
	//
	// It is the ceiling for a plan with holders to choose between. A plan with a
	// SINGLE live holder asks it for one chunk at a time — see requestCapLocked,
	// and the measurement that forced the split.
	//
	// MEASURED MULTI-HOLDER 2026-08-15 (docs/plans/maybe-to-do.md §8): in a
	// symmetric two-holder plan the second slot bought no transfer time at
	// either 4 or 16 MiB and cost the reader 4–5× the floor plus 40–80 % wire
	// overhead in endgame-hedge duplicates. Read that section before citing the
	// pipelining rationale above — the depth question is under an open decision
	// there.
	maxHolderRequests = 2

	// maxChunkCopies is how many holders may be fetching one chunk at once
	// (F9 item 4). Two, not "the last few chunks across several holders" as
	// BitTorrent's endgame does it, because a duplicate costs the chunk's bytes
	// twice over and the second copy already buys the whole claim: the tail is
	// as fast as the FASTER of two holders instead of as slow as whichever one
	// happened to be dispatched. A third copy would only narrow that further at
	// the same price again.
	maxChunkCopies = 2
)

// errChunkBusy marks a holder refusing a chunk under its own member quota (429).
// Like errChunkAbsent it is a fact about the moment rather than a fault: the
// swarm is documented to read a refusal as "ask another holder", so it must never
// build the streak that retires one. Reading it as slowness would be worse than
// harmless — it would starve a busy-but-fast peer through the very mechanism
// meant to find fast peers.
var errChunkBusy = errors.New("holder is over its serving quota")

// errManifestSuspect ends a swarm attempt whose reference is more likely wrong
// than its sources are (see chunkPlan.fail). The whole-file fallback carries its
// own reference — the content hash — so giving way to it is the recovery.
var errManifestSuspect = errors.New("the chunk manifest is contradicted by its holders")

// providerState is one holder's standing within one transfer. None of it
// survives the fetch: a plan is built per fetchSwarm call, so a holder that had
// a bad minute starts the next transfer with a clean record. That is deliberate
// — reputation on this timescale is about scheduling, not about trust.
type providerState struct {
	dead      bool      // retired for the rest of this transfer
	fails     int       // consecutive failures (any success clears it)
	busy      int       // consecutive 429s (any success clears it) — escalates the backoff, never the streak
	inFlight  int64     // bytes dispatched to it and not yet resolved
	reqs      int       // chunk requests out to it right now (bounded by maxHolderRequests)
	lastTried time.Time // when it was last handed a chunk (zero = never)
	idleUntil time.Time // not to be asked again before this
	rate      float64   // EWMA bytes/sec over completed chunks (0 = never measured)

	// Coverage, from GET /have (F9 item 1). haveKnown false means "never asked or
	// never answered", which is read as "has everything" — the pre-F9 assumption,
	// and the only safe one, since a node too old to serve /have is a node that
	// holds blobs whole.
	haveKnown bool
	complete  bool
	have      []ByteRange
	lacks     map[int]bool // chunks it has answered 416 for
}

// canServe reports whether this holder is worth asking for chunk idx.
func (ps *providerState) canServe(l *chunkLayout, idx int) bool {
	if ps.lacks[idx] {
		return false
	}
	if !ps.haveKnown || ps.complete {
		return true
	}
	start, end := l.rangeOf(idx)
	return rangeCovered(ps.have, start, end)
}

// chunkPlan schedules chunk fetches across holders: sequential-priority dispatch
// (lowest index first, so the streaming prefix grows in order) with a
// seek-priority override, a contiguous-from-zero progress watermark, coverage-
// and load-aware pair selection, and per-chunk failover.
type chunkPlan struct {
	mu   sync.Mutex
	cond *sync.Cond

	pending   []int  // chunk indices awaiting dispatch (initially in order)
	inFlight  int    // dispatched, not yet resolved
	done      []bool // per-chunk completion
	watermark int    // count of contiguous completed chunks from 0
	remaining int    // chunks not yet done
	aborted   bool
	err       error

	layout *chunkLayout

	providers []*BlobProvider
	prov      []*providerState

	// corruptFrom records which holders have served bytes that failed against
	// THIS manifest, which is the evidence fail() weighs before condemning any of
	// them. See the note there: the accusation comes from the manifest's sender,
	// not the chunk's.
	corruptFrom  map[int]bool
	firstCorrupt int // the holder retired for the first corrupt chunk (-1 = none)

	// attempts counts every failed try at each chunk, across all holders, and
	// attemptLimit bounds it. This is what ENDS a hopeless transfer. It used to
	// be a side effect of retiring holders — the fetch stopped once the last one
	// was dropped — which forced the drop rule to double as the termination
	// rule and got healthy holders retired to make transfers finish. The two are
	// separate concerns: retirement decides who to ask, this decides when to give
	// up. See the note above fail(). A 429 spends none of it — a refusal is not
	// a fault, and the all-busy case terminates on patience instead.
	attempts     []int
	attemptLimit int

	// patience ends a plan that is making NO progress: no chunk delivered for
	// this long (Timeouts.Transfer; <=0 = no deadline) aborts the transfer.
	// It exists for the holder that keeps answering 429 when nobody else can
	// serve — the listener-node case, where the home server is the only holder
	// by construction — because a quota refusal must cost a wait, not the
	// attempt budget, and a wait with no bound would pin the transfer forever
	// if the quota never clears. lastErr carries the reason into the abort.
	patience      time.Duration
	lastDelivered time.Time
	lastErr       error

	// flight is the attempts currently out, per chunk. A chunk normally has one;
	// a hedged chunk has two, which is the whole of F9 item 4 (see hedgeLocked).
	flight map[int][]*attempt
	// wanted marks a chunk a READER is blocked on that is already in somebody's
	// hands. prioritize can reorder the queue, but it cannot reach a chunk that
	// has left it — and that is precisely the chunk a stalled stream is waiting
	// for, so a hedge is the only thing that can make it arrive sooner.
	wanted map[int]bool

	parent    context.Context // the transfer's lifetime; every attempt derives from it
	perChunk  time.Duration   // one attempt's backstop (Timeouts.PerChunk)
	retry     time.Duration   // base failure backoff (Timeouts.Retry)
	tryWindow time.Duration   // how recently a holder must have been asked to count as a benchmark (Timeouts.PerChunk)
	timerSet  bool            // a wake-up is already scheduled for the earliest backoff

	stats *transferStats // diagnostics sink; nil outside a real transfer
}

// attempt is one holder's outstanding try at one chunk. The cancel is what makes
// hedging finish a transfer rather than merely start a second copy of it: when
// one copy lands, the other is stopped, and fetchSwarm — which waits for every
// worker — is no longer held by a fetch whose result nobody needs.
type attempt struct {
	pidx   int
	cancel context.CancelFunc
}

func newChunkPlan(ctx context.Context, layout *chunkLayout, holders []*BlobProvider, st *transferStats, to Timeouts) *chunkPlan {
	nc := layout.count()
	cp := &chunkPlan{
		done:         make([]bool, nc),
		remaining:    nc,
		layout:       layout,
		providers:    holders,
		prov:         make([]*providerState, len(holders)),
		corruptFrom:  map[int]bool{},
		firstCorrupt: -1,
		attempts:     make([]int, nc),
		flight:       map[int][]*attempt{},
		wanted:       map[int]bool{},
		parent:       ctx,
		perChunk:     to.PerChunk,
		retry:        to.Retry,
		tryWindow:    to.PerChunk,
		// The transfer-scale timeout, reused as the no-progress bound: it
		// already answers "how long may one blob fetch reasonably take", and a
		// second constant would be a guess of the same quality.
		patience:      to.Transfer,
		lastDelivered: time.Now(),
		stats:         st,
	}
	if cp.parent == nil {
		cp.parent = context.Background()
	}
	for i := range cp.prov {
		cp.prov[i] = &providerState{}
	}
	// One chunk may be retried as many times as the old rule allowed in total
	// before every holder was retired — so the worst case is bounded exactly as
	// it was, while no longer requiring anyone to be retired to get there.
	cp.attemptLimit = providerFailureLimit * len(holders)
	if cp.attemptLimit < providerFailureLimit {
		cp.attemptLimit = providerFailureLimit
	}
	cp.pending = make([]int, 0, nc)
	for i := 0; i < nc; i++ {
		cp.pending = append(cp.pending, i)
	}
	cp.cond = sync.NewCond(&cp.mu)
	return cp
}

// adoptFlight registers an attempt that is ALREADY on the wire — the chunk-0
// speculation runTransfer overlaps with the manifest round trip — as an
// ordinary dispatched copy of chunk idx on provider pidx: it leaves the pending
// queue, its holder is charged the bytes, and the cancel is what a rival
// landing uses to stop it.
//
// It used to be modelled as "chunk 0 already done" behind a blocking receive
// BEFORE the plan existed, which let a holders[0] that dribbles gate the whole
// swarm start on the per-chunk backstop while holders that could serve chunk 0
// at once sat idle (.issues/open-issues.md, swarm refactor pass finding 1). As
// a flight it is covered by the same machinery as every other slow copy:
// prioritize/the endgame hedge a second copy from somebody else, and
// landedLocked cancels the loser. The caller resolves it through succeed/fail
// exactly as a worker resolves a dispatch.
func (cp *chunkPlan) adoptFlight(idx, pidx int, cancel context.CancelFunc) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	for pos, p := range cp.pending {
		if p == idx {
			cp.pending = append(cp.pending[:pos], cp.pending[pos+1:]...)
			break
		}
	}
	cp.inFlight++
	start, end := cp.layout.rangeOf(idx)
	ps := cp.prov[pidx]
	ps.inFlight += end - start
	ps.reqs++
	ps.lastTried = time.Now()
	cp.flight[idx] = append(cp.flight[idx], &attempt{pidx: pidx, cancel: cancel})
}

// watermarkBytes is the contiguous-from-zero readable length in bytes.
func (cp *chunkPlan) watermarkBytes() int64 {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.layout.offsetOf(cp.watermark)
}

// prioritize moves the chunk covering a requested offset to the front of the
// dispatch queue (if still pending), so the next free worker fetches it — this
// is what makes a streaming tail/seek read fast instead of waiting out the
// sequential prefix.
//
// **A chunk that has already left the queue gets marked instead** (F9 item 4).
// Reordering can only help a chunk still waiting to be dispatched, and the one a
// stalled reader is waiting for is very often not: it is in flight, on whichever
// holder happens to be slow. Marking it lets the next free worker fetch a second
// copy from somebody else, which is the only thing that can make it arrive
// sooner. It takes priority over starting a new chunk, because a reader blocked
// now is worth more than a chunk nobody has asked for yet.
func (cp *chunkPlan) prioritize(idx int) {
	cp.mu.Lock()
	if idx >= 0 && idx < len(cp.done) && !cp.done[idx] {
		queued := false
		for i, p := range cp.pending {
			if p == idx {
				copy(cp.pending[1:i+1], cp.pending[0:i])
				cp.pending[0] = idx
				queued = true
				break
			}
		}
		if !queued && len(cp.flight[idx]) > 0 {
			cp.wanted[idx] = true
		}
	}
	cp.cond.Broadcast()
	cp.mu.Unlock()
}

// take hands out the next (chunk, holder) pair to fetch, blocking while the work
// that remains is in somebody else's hands or behind a holder's backoff. Returns
// false when the transfer is done or aborted.
//
// One call rather than the old next()+pickProvider() pair, because with partial
// holders the two questions stopped being independent: a chunk is only worth
// dispatching to a holder that has it, and the answer to "which chunk next"
// depends on who is free.
//
// The order it tries things in is the policy:
//
//  1. a chunk a reader is BLOCKED on that somebody slow already has;
//  2. the pending queue, in its sequential/seek-priority order;
//  3. anything still in flight, once the queue is empty — the endgame.
//
// 1 and 3 are hedges, and neither needs a timing constant to decide when to
// duplicate: one asks "is anyone waiting for this right now", the other "is
// there anything better for this worker to do". "Re-dispatch a chunk in flight
// longer than k × the median" was the design's suggestion and it is not needed
// — every case it catches ends up in 3 anyway, and it would spend a duplicate
// while chunks nobody has fetched at all were still queued.
func (cp *chunkPlan) take() (dispatch, bool) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	for {
		if cp.aborted || cp.remaining == 0 {
			return dispatch{}, false
		}
		if cp.liveProvidersLocked() == 0 {
			cp.abortLocked(ErrNoHolder)
			return dispatch{}, false
		}
		for idx := range cp.wanted {
			if pidx, ok := cp.hedgeLocked(idx); ok {
				return cp.dispatchLocked(idx, pidx, true), true
			}
		}
		if len(cp.pending) > 0 {
			if pos, pidx, ok := cp.matchLocked(); ok {
				return cp.dispatchLocked(cp.unqueueLocked(pos), pidx, false), true
			}
		} else {
			for idx := range cp.flight {
				if pidx, ok := cp.hedgeLocked(idx); ok {
					return cp.dispatchLocked(idx, pidx, true), true
				}
			}
		}
		// Nothing to hand out this moment. Either somebody will bring work back
		// (a chunk in flight can fail and be re-queued), or a holder is merely
		// sitting out a backoff and will be askable again shortly.
		waiting := cp.armBackoffLocked()
		if !waiting && cp.inFlight == 0 {
			// Work remains, every holder is live, none is resting, and no pair
			// could be formed — which the two-pass match makes impossible today.
			// Aborting rather than blocking is the point: a scheduler that cannot
			// schedule must end the transfer, not hold its workers forever.
			cp.abortLocked(fmt.Errorf("chunk %d: no holder in the plan will serve it", cp.pending[0]))
			return dispatch{}, false
		}
		// The patience rule: with everything askable resting and nothing on the
		// wire, a plan that has delivered no chunk for a whole Timeouts.Transfer
		// is waited out no further. This is what ends the all-busy case — a 429
		// spends no attempt budget (fail()), so without this a holder refusing
		// under a quota that never clears would hold the transfer open forever.
		// The backoff wake-ups re-run this check every few seconds, so the
		// deadline is honoured shortly after it passes; any delivered chunk
		// resets the clock (succeed()).
		if cp.patience > 0 && cp.inFlight == 0 && time.Since(cp.lastDelivered) > cp.patience {
			err := fmt.Errorf("no chunk delivered in %s (%d chunks left)", cp.patience, cp.remaining)
			if cp.lastErr != nil {
				err = fmt.Errorf("no chunk delivered in %s (%d chunks left): %w",
					cp.patience, cp.remaining, cp.lastErr)
			}
			cp.abortLocked(err)
			return dispatch{}, false
		}
		cp.cond.Wait()
	}
}

// hedgeLocked picks a holder to fetch a SECOND copy of a chunk somebody is
// already fetching, or reports that there is no point. Caller holds cp.mu.
//
// Three conditions, and each rules out a way hedging could cost more than it
// buys: the chunk must not already have maxChunkCopies out (a swarm can spend
// its whole bandwidth on one chunk otherwise), the holder must not be one
// already fetching it (a second request to the same slow node is not a second
// chance), and it must be a holder the ordinary rules would dispatch to anyway
// — not dead, not resting, and known to hold the chunk.
func (cp *chunkPlan) hedgeLocked(idx int) (int, bool) {
	if idx < 0 || idx >= len(cp.done) || cp.done[idx] {
		return 0, false
	}
	out := cp.flight[idx]
	if len(out) == 0 || len(out) >= maxChunkCopies {
		return 0, false
	}
	for _, pi := range cp.rankLocked() {
		busy := false
		for _, a := range out {
			if a.pidx == pi {
				busy = true
				break
			}
		}
		if !busy && cp.prov[pi].canServe(cp.layout, idx) {
			return pi, true
		}
	}
	return 0, false
}

// unqueueLocked removes pending[pos] and returns the chunk index it held.
func (cp *chunkPlan) unqueueLocked(pos int) int {
	idx := cp.pending[pos]
	cp.pending = append(cp.pending[:pos], cp.pending[pos+1:]...)
	return idx
}

// matchLocked picks the (pending position, provider) pair to dispatch. Caller
// holds cp.mu.
//
// Two passes, and the second one is what keeps a partial seeder useful. Coverage
// is a snapshot of a GROWING thing — a partial holder is fetching too — so
// "known to lack chunk 7" is only true until it isn't. Pass 1 respects coverage,
// which is what stops a partial from being asked for what it plainly does not
// have while a complete holder is free. Pass 2 asks anyway when nobody else
// will: the cost of being wrong is one fast 416, and the cost of never asking is
// a source lost for the rest of the transfer.
func (cp *chunkPlan) matchLocked() (int, int, bool) {
	order := cp.rankLocked()
	for _, pi := range order {
		for pos, idx := range cp.pending {
			if cp.prov[pi].canServe(cp.layout, idx) {
				return pos, pi, true
			}
		}
	}
	if len(order) > 0 {
		return 0, order[0], true
	}
	return 0, -1, false
}

// rankLocked orders the holders that may be asked right now, best first: fewest
// bytes outstanding, then fastest measured, then least recently tried.
//
// Fewest-outstanding-bytes is the whole scheduler. A holder that is not
// delivering holds onto its dispatches, so its outstanding total stays high and
// it stops being chosen — no timeout has to expire and nobody has to decide it
// is slow.
//
// The tie-breaks matter only at the start, when everyone is idle, and they are
// in this order for a reason. **Never asked comes before fastest**: an unmeasured
// holder could be the best in the swarm, and preferring a holder we happen to
// have a number for would mean the first one measured keeps the work for the
// whole transfer. It is one free sample each, not a preference. A holder that HAS
// been asked and still has no number is one that only ever failed, and it sinks
// to the bottom, which is where the two rules stop agreeing and where the
// distinction earns itself.
func (cp *chunkPlan) rankLocked() []int {
	now := time.Now()
	depth := cp.requestCapLocked()
	order := make([]int, 0, len(cp.prov))
	for i, ps := range cp.prov {
		if ps.dead || now.Before(ps.idleUntil) || ps.reqs >= depth {
			continue
		}
		order = append(order, i)
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := cp.prov[order[a]], cp.prov[order[b]]
		if x.inFlight != y.inFlight {
			return x.inFlight < y.inFlight
		}
		if xf, yf := x.lastTried.IsZero(), y.lastTried.IsZero(); xf != yf {
			return xf
		}
		if x.rate != y.rate {
			return x.rate > y.rate
		}
		return x.lastTried.Before(y.lastTried)
	})
	return order
}

// dispatch is one unit of work: fetch chunk idx from p, under ctx.
//
// The context comes from the plan rather than from the worker because the plan
// is what knows the attempt has stopped being useful — another copy landed, and
// this one should stop reading bytes nobody will keep. cancel is handed over too
// so the worker can release it and arm the idle-read watchdog with it.
type dispatch struct {
	idx    int
	p      *BlobProvider
	pidx   int
	ctx    context.Context
	cancel context.CancelFunc
	hedge  bool
}

// dispatchLocked charges chunk idx to provider pidx and opens the attempt's
// context. Caller holds cp.mu.
func (cp *chunkPlan) dispatchLocked(idx, pidx int, hedge bool) dispatch {
	cp.inFlight++
	start, end := cp.layout.rangeOf(idx)
	ps := cp.prov[pidx]
	ps.inFlight += end - start
	ps.reqs++
	ps.lastTried = time.Now()

	ctx, cancel := context.WithTimeout(cp.parent, cp.perChunk)
	cp.flight[idx] = append(cp.flight[idx], &attempt{pidx: pidx, cancel: cancel})
	if hedge {
		cp.stats.noteHedge()
	}
	return dispatch{idx: idx, p: cp.providers[pidx], pidx: pidx, ctx: ctx, cancel: cancel, hedge: hedge}
}

// landedLocked closes out one attempt at a chunk: it leaves the flight table,
// its holder gets its outstanding bytes back, and — when the chunk is now done
// — every other copy of it is cancelled. Returns whether any sibling was still
// running, which is what makes a hedge's win visible. Caller holds cp.mu.
func (cp *chunkPlan) landedLocked(idx, pidx int, won bool) bool {
	out := cp.flight[idx]
	kept := out[:0]
	dropped := false
	hadSibling := false
	for _, a := range out {
		if !dropped && a.pidx == pidx {
			dropped = true // this attempt; the same holder may hold no second one
			continue
		}
		hadSibling = true
		if won {
			a.cancel()
			continue
		}
		kept = append(kept, a)
	}
	if won || len(kept) == 0 {
		delete(cp.flight, idx)
		delete(cp.wanted, idx)
	} else {
		cp.flight[idx] = kept
	}
	if pidx >= 0 {
		start, end := cp.layout.rangeOf(idx)
		ps := cp.prov[pidx]
		if ps.inFlight -= end - start; ps.inFlight < 0 {
			ps.inFlight = 0
		}
		if ps.reqs--; ps.reqs < 0 {
			ps.reqs = 0
		}
	}
	return hadSibling
}

// armBackoffLocked schedules a wake-up at the earliest moment a resting holder
// becomes askable again, and reports whether there is one to wait for. Caller
// holds cp.mu.
//
// Without this a plan whose only live holder is backing off would sleep until
// something else happened to broadcast — and when nothing is in flight, nothing
// would.
func (cp *chunkPlan) armBackoffLocked() bool {
	now := time.Now()
	var soonest time.Time
	for _, ps := range cp.prov {
		if ps.dead || !now.Before(ps.idleUntil) {
			continue
		}
		if soonest.IsZero() || ps.idleUntil.Before(soonest) {
			soonest = ps.idleUntil
		}
	}
	if soonest.IsZero() {
		return false
	}
	if !cp.timerSet {
		cp.timerSet = true
		time.AfterFunc(soonest.Sub(now), func() {
			cp.mu.Lock()
			cp.timerSet = false
			cp.cond.Broadcast()
			cp.mu.Unlock()
		})
	}
	return true
}

// setCoverage records what a holder answered to GET /have: which byte extents of
// this blob it will serve, and whether it holds the whole thing (F9 item 1).
//
// A holder that answers is believed about what it HAS — the bytes are verified
// against the manifest either way, so a lie here costs a 416 or a failed chunk,
// not correctness. What it is not believed about is what it lacks *later*: see
// matchLocked's second pass.
func (cp *chunkPlan) setCoverage(pidx int, msg *haveMessage) {
	if msg == nil || pidx < 0 || pidx >= len(cp.prov) {
		return
	}
	cp.mu.Lock()
	ps := cp.prov[pidx]
	ps.haveKnown = true
	ps.complete = msg.Complete
	ps.have = msg.Ranges
	ps.lacks = nil // this reply supersedes what individual 416s taught us
	cp.cond.Broadcast()
	cp.mu.Unlock()
}

// probeUnanswered records that a holder did not answer the coverage probe.
//
// It rests briefly and is NOT marked against: a probe failure is weaker evidence
// than a failed chunk, and the endpoint is young enough that a node may simply
// not serve it (in which case the reply is a 404 and never reaches here — but
// only condemning on bytes keeps that distinction from mattering). Retirement
// stays the business of holders that were asked for something and did not
// deliver it.
func (cp *chunkPlan) probeUnanswered(pidx int) {
	if pidx < 0 || pidx >= len(cp.prov) {
		return
	}
	cp.mu.Lock()
	cp.prov[pidx].idleUntil = time.Now().Add(cp.retry)
	cp.mu.Unlock()
}

// succeed records a completed chunk, advancing the contiguous progress watermark
// and publishing it to the transfer. A success clears the provider's failure
// streak — an intermittently-stalling holder is forgiven so it is not dropped
// over transient hiccups — and feeds its throughput estimate, which is the only
// place that estimate comes from.
func (cp *chunkPlan) succeed(idx, pidx int, t *transfer, took time.Duration) {
	cp.mu.Lock()
	cp.inFlight--
	var from *BlobProvider
	start, end := cp.layout.rangeOf(idx)
	var rate float64
	fresh := !cp.done[idx]
	// Close the attempt out FIRST: it is what stops the copy that lost, and the
	// answer decides whether this was a hedge winning or an ordinary fetch.
	raced := cp.landedLocked(idx, pidx, fresh)
	if pidx >= 0 {
		ps := cp.prov[pidx]
		ps.fails = 0
		ps.busy = 0
		ps.idleUntil = time.Time{}
		if took > 0 {
			sample := float64(end-start) / took.Seconds()
			if ps.rate == 0 {
				ps.rate = sample
			} else {
				ps.rate = rateAlpha*sample + (1-rateAlpha)*ps.rate
			}
			rate = ps.rate
		}
		from = cp.providers[pidx]
	}
	if fresh {
		cp.done[idx] = true
		cp.remaining--
		cp.lastDelivered = time.Now() // progress resets the patience clock
		for cp.watermark < len(cp.done) && cp.done[cp.watermark] {
			cp.watermark++
		}
	}
	progress := cp.layout.offsetOf(cp.watermark)
	cp.cond.Broadcast()
	cp.mu.Unlock()
	if fresh {
		cp.stats.noteSucceed(idx, from, end-start)
		if raced {
			cp.stats.noteHedgeWon()
		}
	}
	if rate > 0 {
		cp.stats.noteRate(from, rate)
	}
	t.chunkDone(idx, progress)
}

// fail re-queues the chunk for another attempt and decides what the failure says
// about the holder that missed it.
//
// Four answers, because four different things are being reported:
//
//   - **Corrupt bytes** are evidence about the holder itself, and no amount of
//     environmental bad luck produces them — so the first one retires its sender
//     outright. The second one from a DIFFERENT sender says something else
//     entirely; see below.
//   - **A 416** (errChunkAbsent) is a fact about the CHUNK: a partial seeder has
//     not reached it yet. It told us something true about the blob and nothing
//     bad about itself, so its streak is left alone — condemning it would retire
//     exactly the nodes F9 item 1 exists to recruit. Not reset either: a holder
//     already in trouble should not launder it by happening to lack a chunk.
//   - **A 429** (errChunkBusy) is a deliberate refusal under the member quota,
//     which the swarm is documented to read as "ask another holder". It costs a
//     wait and nothing else.
//   - **Anything else** — a stall, a timeout, an unreachable node — is weaker
//     evidence than corruption, because it describes the holder AND the moment.
//     The rule is therefore relative: a holder is retired once it is
//     providerFailureLimit consecutive failures worse than the best live holder
//     (worseThanPeers). It also rests, doubling per consecutive failure, which
//     under load-based dispatch is load-bearing in its own right: a holder that
//     fails INSTANTLY has no bytes outstanding, so without the pause the fastest
//     way to look idle would be to keep refusing.
//
// **The manifest is a suspect too, and it is the one the code cannot see.**
// errChunkCorrupt blames the chunk's sender, but the accusation comes from the
// MANIFEST's sender, and those are different nodes — so one lying manifest would
// retire every honest holder in the swarm (.issues/open-issues.md, "a lying
// manifest retires every honest holder"). When a second, distinct holder fails
// against the same reference, the reference is the likelier liar: the attempt
// ends with errManifestSuspect, the holder retired for the first corrupt chunk is
// reinstated, and the whole-file fallback — which carries its own reference, the
// content hash — takes over. Two holders colluding still defeat this, and it is
// not meant to catch that.
//
// Retiring holders is not how a hopeless transfer stops — see attempts /
// attemptLimit, which bound each chunk individually. That separation is the
// point: the old code could only end a fetch by killing every holder, so a
// healthy one had to be declared faulty for the transfer to finish at all
// (.issues/open-issues.md, -race run findings, item 3).
func (cp *chunkPlan) fail(idx, pidx int, err error, corrupt bool) {
	cp.mu.Lock()
	cp.inFlight--
	cp.landedLocked(idx, pidx, false)
	// A copy that was still running when another one landed is not a failure at
	// all — it was CANCELLED, by us, on purpose (F9 item 4). Blaming its holder
	// would punish the losing half of every hedge, which is the one thing that
	// could make hedging worse than not hedging: the second-fastest holder in the
	// swarm would collect a streak for being second. Nothing is counted, nothing
	// is re-queued, and the chunk is already done.
	if cp.done[idx] {
		var lost *BlobProvider
		if pidx >= 0 {
			lost = cp.providers[pidx]
		}
		cp.cond.Broadcast()
		cp.mu.Unlock()
		cp.stats.noteLost(lost)
		return
	}
	var from, reinstated *BlobProvider
	dropped := false
	if pidx >= 0 {
		ps := cp.prov[pidx]
		from = cp.providers[pidx]
		switch {
		case corrupt:
			cp.corruptFrom[pidx] = true
			if len(cp.corruptFrom) > 1 {
				if cp.firstCorrupt >= 0 {
					cp.prov[cp.firstCorrupt].dead = false
					reinstated = cp.providers[cp.firstCorrupt]
					cp.firstCorrupt = -1
				}
				if !cp.aborted {
					cp.aborted, cp.err = true, fmt.Errorf(
						"chunk %d failed against a manifest %d holders disagree with: %w",
						idx, len(cp.corruptFrom), errManifestSuspect)
				}
			} else {
				ps.dead = true
				cp.firstCorrupt = pidx
			}
		case errors.Is(err, errChunkAbsent):
			ps.lacks = markLacking(ps.lacks, idx)
		case errors.Is(err, errChunkBusy):
			// Consecutive refusals double the pause (through backoffFor's cap):
			// the swarm may be waiting a quota out for minutes (see patience in
			// take()), and re-asking a node that keeps saying no at the base
			// cadence would be a poll, not a retry.
			ps.busy++
			ps.idleUntil = time.Now().Add(backoffFor(cp.retry, busyBackoffSteps+ps.busy-1))
		default:
			ps.fails++
			ps.idleUntil = time.Now().Add(backoffFor(cp.retry, ps.fails))
			if ps.fails >= providerFailureLimit && cp.worseThanPeers(pidx) {
				ps.dead = true
			}
		}
		dropped = ps.dead
	}
	cp.lastErr = err
	// A 429 spends no attempt budget (decided 2026-08-13): the budget is the
	// termination rule for FAULTS, and a refusal is not one — counting it meant
	// a sole busy holder failed the transfer in four polite refusals, which is
	// the household's normal shape (a listener node has exactly one holder, its
	// home server, and draws on the member budget). The all-busy case
	// terminates on the patience deadline in take() instead. A 416 keeps
	// counting: that is what makes a swarm of partials that collectively lack a
	// chunk terminate rather than loop.
	if !errors.Is(err, errChunkBusy) {
		cp.attempts[idx]++
	}
	switch {
	case cp.aborted:
		// already given up (a suspect manifest); the chunk needs no re-queue
	case cp.liveProvidersLocked() == 0:
		cp.abortLocked(err)
	case cp.attempts[idx] >= cp.attemptLimit:
		cp.abortLocked(fmt.Errorf(
			"chunk %d unfetchable after %d attempts: %w", idx, cp.attempts[idx], err))
	case len(cp.flight[idx]) > 0:
		// A hedge failed while the copy it was racing is still running. The chunk
		// is not lost and must not be queued a second time, or one failure would
		// turn into two dispatches.
	default:
		cp.pending = append(cp.pending, idx)
	}
	cp.cond.Broadcast()
	cp.mu.Unlock()
	cp.stats.noteFail(idx, from, err, corrupt)
	if dropped {
		cp.stats.noteDropped(from)
	}
	if reinstated != nil {
		cp.stats.noteReinstated(reinstated)
	}
}

// markLacking records that a holder answered 416 for a chunk, allocating the set
// on first use (most holders never need one).
func markLacking(set map[int]bool, idx int) map[int]bool {
	if set == nil {
		set = map[int]bool{}
	}
	set[idx] = true
	return set
}

// backoffFor is the base delay doubled per consecutive failure, capped.
func backoffFor(base time.Duration, step int) time.Duration {
	if base <= 0 {
		return 0
	}
	if step > maxBackoffSteps {
		step = maxBackoffSteps
	}
	for ; step > 1; step-- {
		base *= 2
	}
	return base
}

// abortLocked ends the transfer with the first reason offered. Caller holds cp.mu.
func (cp *chunkPlan) abortLocked(err error) {
	if !cp.aborted {
		cp.aborted, cp.err = true, err
	}
}

// worseThanPeers reports whether provider i is failing out of line with the
// other live holders — the relative half of the retirement rule above. Caller
// holds cp.mu.
//
// It compares consecutive-failure streaks, which are already maintained and are
// reset by any success: a holder that keeps delivering sits at 0, so anything
// providerFailureLimit above it is demonstrably the odd one out. With no live
// peer left to compare against it returns true, leaving the absolute limit in
// force so a fetch against a single dead holder still terminates.
//
// **Only holders that have been ASKED recently are a benchmark**, and that
// condition is what load-aware dispatch cost. Under round-robin every live
// holder was handed work in rotation, so a streak of 0 could be read as "this
// peer is doing fine" — the reading the whole rule rests on. It is now possible
// for a holder to be deprioritised and hold a clean 0 it never earned, which
// would silently make the comparison meaningless and, worse, keep the strict
// absolute rule in force against a holder that is merely having the same bad
// moment as everybody else. A holder not asked within the time one ask may take
// (Timeouts.PerChunk) has no record worth comparing against.
func (cp *chunkPlan) worseThanPeers(i int) bool {
	cutoff := time.Now().Add(-cp.tryWindow)
	best := -1
	for j, ps := range cp.prov {
		if j == i || ps.dead {
			continue
		}
		if ps.lastTried.IsZero() || ps.lastTried.Before(cutoff) {
			continue
		}
		if best < 0 || ps.fails < best {
			best = ps.fails
		}
	}
	if best < 0 {
		return true
	}
	return cp.prov[i].fails >= best+providerFailureLimit
}

// requestCapLocked is how many chunks one holder may be fetching at once, right
// now. Caller holds cp.mu.
//
// maxHolderRequests everywhere except the one shape where the second slot cannot
// buy anything: a plan with a SINGLE live holder. Then both requests share one
// link, so the chunk a reader is blocked on is competing with a chunk nobody has
// asked for — and neither of the rules that normally reclaim a chunk applies,
// because prioritize cannot reorder a dispatch that has left the queue and a
// hedge needs a second holder to send.
//
// That is the household by construction: a madplayer's only holder is its home
// server, which is also the deployment the mid-track stall was reported from.
// Measured 2026-08-14, 2 MiB over a 128 KiB/s link, three runs each — the worst
// single blocking read a streaming reader paid:
//
//	depth 2: 4.86 / 5.54 / 9.23 s      depth 1: 2.396 / 2.405 / 2.415 s
//
// A 256 KiB chunk at 128 KiB/s is ~2 s, so depth 1 is the floor and depth 2 was
// costing up to 4× it. **Total elapsed is identical either way** (19.0 s), which
// is exactly why the F9 measurement that shipped depth 2 scored it free: it timed
// the TRANSFER, and the whole cost of request depth lands on reader tail latency.
// Anyone measuring a swarm change after this should time transfer.WaitFor, not
// the fetch.
//
// Narrow on purpose. A blanket depth 1 would give up pipelining on healthy
// multi-holder swarms, where the second slot is what keeps a fast holder busy
// across the RTT — the finding the constant above records. This condition costs
// nothing in any plan that has an alternative, and it lifts the moment one
// appears: "live" is not-dead, so retiring the last of four ghosts narrows the
// survivor to depth 1 by itself. Narrowing mid-transfer drains rather than
// preempts — a holder already at two requests keeps both and is simply not asked
// again until it is down to none, which costs at most one chunk once.
func (cp *chunkPlan) requestCapLocked() int {
	if measureRequestDepth > 0 {
		return measureRequestDepth
	}
	if cp.liveProvidersLocked() <= 1 {
		return 1
	}
	return maxHolderRequests
}

// measureRequestDepth is a MEASUREMENT SEAM (docs/plans/maybe-to-do.md step 1):
// when > 0 it forces the per-holder request depth, bypassing both the
// maxHolderRequests ceiling and the sole-holder narrowing, and fetchSwarm
// raises its worker count to match. Set only by depthmeasure_test.go, never by
// the server, and read without synchronization — the measurement sets it before
// a transfer starts and clears it after the transfer ends.
var measureRequestDepth = 0

// liveProvidersLocked counts non-dead holders; caller holds cp.mu.
func (cp *chunkPlan) liveProvidersLocked() int {
	live := 0
	for _, ps := range cp.prov {
		if !ps.dead {
			live++
		}
	}
	return live
}
