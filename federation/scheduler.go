//go:build !nofederation

package federation

import (
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
	inFlight  int64     // bytes dispatched to it and not yet resolved
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
	// up. See the note above fail().
	attempts     []int
	attemptLimit int

	retry     time.Duration // base failure backoff (Timeouts.Retry)
	tryWindow time.Duration // how recently a holder must have been asked to count as a benchmark (Timeouts.PerChunk)
	timerSet  bool          // a wake-up is already scheduled for the earliest backoff

	stats *transferStats // diagnostics sink; nil outside a real transfer
}

func newChunkPlan(layout *chunkLayout, holders []*BlobProvider, done0 bool, st *transferStats, to Timeouts) *chunkPlan {
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
		retry:        to.Retry,
		tryWindow:    to.PerChunk,
		stats:        st,
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
	start := 0
	if done0 && nc > 0 {
		cp.done[0] = true
		cp.watermark = 1
		cp.remaining = nc - 1
		start = 1
	}
	cp.pending = make([]int, 0, nc-start)
	for i := start; i < nc; i++ {
		cp.pending = append(cp.pending, i)
	}
	cp.cond = sync.NewCond(&cp.mu)
	return cp
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
func (cp *chunkPlan) prioritize(idx int) {
	cp.mu.Lock()
	if idx >= 0 && idx < len(cp.done) && !cp.done[idx] {
		for i, p := range cp.pending {
			if p == idx {
				copy(cp.pending[1:i+1], cp.pending[0:i])
				cp.pending[0] = idx
				break
			}
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
func (cp *chunkPlan) take() (int, *BlobProvider, int, bool) {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	for {
		if cp.aborted || cp.remaining == 0 {
			return 0, nil, -1, false
		}
		if cp.liveProvidersLocked() == 0 {
			cp.abortLocked(ErrNoHolder)
			return 0, nil, -1, false
		}
		if len(cp.pending) > 0 {
			if pos, pidx, ok := cp.matchLocked(); ok {
				idx, p := cp.dispatchLocked(pos, pidx)
				return idx, p, pidx, true
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
			return 0, nil, -1, false
		}
		cp.cond.Wait()
	}
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
	order := make([]int, 0, len(cp.prov))
	for i, ps := range cp.prov {
		if ps.dead || now.Before(ps.idleUntil) {
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

// dispatchLocked removes pending[pos] from the queue and charges it to provider
// pidx. Caller holds cp.mu.
func (cp *chunkPlan) dispatchLocked(pos, pidx int) (int, *BlobProvider) {
	idx := cp.pending[pos]
	cp.pending = append(cp.pending[:pos], cp.pending[pos+1:]...)
	cp.inFlight++
	start, end := cp.layout.rangeOf(idx)
	ps := cp.prov[pidx]
	ps.inFlight += end - start
	ps.lastTried = time.Now()
	return idx, cp.providers[pidx]
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
	if pidx >= 0 {
		ps := cp.prov[pidx]
		ps.fails = 0
		ps.idleUntil = time.Time{}
		ps.inFlight -= end - start
		if ps.inFlight < 0 {
			ps.inFlight = 0
		}
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
	fresh := !cp.done[idx]
	if fresh {
		cp.done[idx] = true
		cp.remaining--
		for cp.watermark < len(cp.done) && cp.done[cp.watermark] {
			cp.watermark++
		}
	}
	progress := cp.layout.offsetOf(cp.watermark)
	cp.cond.Broadcast()
	cp.mu.Unlock()
	if fresh {
		cp.stats.noteSucceed(idx, from, end-start)
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
	var from, reinstated *BlobProvider
	dropped := false
	if pidx >= 0 {
		ps := cp.prov[pidx]
		from = cp.providers[pidx]
		start, end := cp.layout.rangeOf(idx)
		ps.inFlight -= end - start
		if ps.inFlight < 0 {
			ps.inFlight = 0
		}
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
			ps.idleUntil = time.Now().Add(backoffFor(cp.retry, busyBackoffSteps))
		default:
			ps.fails++
			ps.idleUntil = time.Now().Add(backoffFor(cp.retry, ps.fails))
			if ps.fails >= providerFailureLimit && cp.worseThanPeers(pidx) {
				ps.dead = true
			}
		}
		dropped = ps.dead
	}
	cp.attempts[idx]++
	switch {
	case cp.aborted:
		// already given up (a suspect manifest); the chunk needs no re-queue
	case cp.liveProvidersLocked() == 0:
		cp.abortLocked(err)
	case cp.attempts[idx] >= cp.attemptLimit:
		cp.abortLocked(fmt.Errorf(
			"chunk %d unfetchable after %d attempts: %w", idx, cp.attempts[idx], err))
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
