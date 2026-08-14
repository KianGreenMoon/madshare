//go:build !nofederation

package federation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestTransferChunkAvailability pins the per-chunk readiness the streaming relay
// relies on: an out-of-order (tail) chunk is readable before the front arrives,
// Available reflects only contiguously-readable runs, and WaitFor on a missing
// offset pokes the seek-priority hook and honours ctx cancellation.
func TestTransferChunkAvailability(t *testing.T) {
	tr := newTransfer("h", "final", "final.part")
	tr.size = 30
	var poked []int
	tr.beginChunks(buildLayout(30, 10, nil), func(idx int) { poked = append(poked, idx) }) // 3 uniform 10-byte chunks

	if got := tr.Available(0); got != 0 {
		t.Fatalf("Available(0) before any chunk = %d, want 0", got)
	}

	// Tail chunk (2) done out of order: readable even though the front is not.
	tr.chunkDone(2, 0) // watermark stays 0 (chunk 0 still missing)
	if got := tr.Available(20); got != 10 {
		t.Errorf("Available(20) after tail chunk = %d, want 10", got)
	}
	if got := tr.Available(25); got != 5 {
		t.Errorf("Available(25) mid tail chunk = %d, want 5", got)
	}
	if got := tr.Available(0); got != 0 {
		t.Errorf("Available(0) with only tail done = %d, want 0", got)
	}

	// WaitFor a ready offset returns at once.
	if err := tr.WaitFor(context.Background(), 25); err != nil {
		t.Errorf("WaitFor(25) on a ready chunk: %v", err)
	}
	// WaitFor a missing offset blocks: it pokes prioritize and returns on cancel.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tr.WaitFor(ctx, 0); err != context.Canceled {
		t.Errorf("WaitFor(0) missing = %v, want context.Canceled", err)
	}
	if len(poked) == 0 || poked[len(poked)-1] != 0 {
		t.Errorf("seek-priority not poked for chunk 0: %v", poked)
	}

	// Fill the gap: chunk 0 makes [0,10) readable; chunk 1 makes the file whole.
	tr.chunkDone(0, 10)
	if got := tr.Available(0); got != 10 {
		t.Errorf("Available(0) with chunks 0,2 done = %d, want 10 (chunk 1 missing)", got)
	}
	tr.chunkDone(1, 30)
	if got := tr.Available(0); got != 30 {
		t.Errorf("Available(0) fully done = %d, want 30", got)
	}
}

// TestChunkLayout pins the ramp policy and the boundary table: the lead ramp
// keeps the first chunk small (fast first byte / front seek) regardless of file
// size, offsets cover [0,size] strictly increasing, and chunkAt round-trips
// rangeOf for every chunk.
func TestChunkLayout(t *testing.T) {
	if got := leadSizes(700<<10, minChunkSize); got != nil {
		t.Errorf("leadSizes(small,floor) = %v, want nil (no ramp)", got)
	}
	if got := leadSizes(14<<20, 1<<20); len(got) != 2 || got[0] != 256<<10 || got[1] != 512<<10 {
		t.Errorf("leadSizes(14MB,1MB) = %v, want [256K 512K]", got)
	}

	for _, size := range []int64{0, 1, minChunkSize, 700 << 10, 4 << 20, 14 << 20, 50 << 20} {
		bulk := chunkSizeFor(size)
		lay := buildLayout(size, bulk, leadSizes(size, bulk))
		offs := lay.offsets
		if offs[0] != 0 || offs[len(offs)-1] != size {
			t.Fatalf("size=%d layout does not cover [0,%d]: %v", size, size, offs)
		}
		for i := 1; i < len(offs); i++ {
			if offs[i] <= offs[i-1] {
				t.Fatalf("size=%d non-increasing offsets: %v", size, offs)
			}
		}
		for i := 0; i < lay.count(); i++ {
			s, e := lay.rangeOf(i)
			if lay.chunkAt(s) != i || lay.chunkAt(e-1) != i {
				t.Errorf("size=%d chunk %d [%d,%d): chunkAt round-trip failed", size, i, s, e)
			}
		}
		if size > minChunkSize { // ramp guarantees a small first chunk
			if s, e := lay.rangeOf(0); e-s > minChunkSize {
				t.Errorf("size=%d first chunk = %d bytes, want ≤ %d", size, e-s, minChunkSize)
			}
		}
	}
}

// TestChunkPlanPrioritizeAndAdoptedFlight covers the two scheduling entry
// points fetchSwarm uses beyond plain dispatch: prioritize jumps a pending
// chunk to the front, and adoptFlight registers the speculative chunk-0 fetch
// as an attempt already on the wire, so dispatch starts at chunk 1 and the
// speculation resolves through succeed like any worker's dispatch — including
// being charged the request slot, which on a sole holder is the whole depth.
func TestChunkPlanPrioritizeAndAdoptedFlight(t *testing.T) {
	man := &blobManifest{ChunkSize: 10, Size: 50, Chunks: []string{"a", "b", "c", "d", "e"}}
	layout := man.layout()
	holders := []*BlobProvider{{Name: "h"}}

	cp := testPlan(layout, holders)
	tr := newTransfer("h", "p", "p.part")
	cp.prioritize(3)
	// Each chunk is completed before the next is taken, because the plan's only
	// holder is asked for one chunk at a time (requestCapLocked).
	for _, want := range []int{3, 0, 1, 2, 4} { // prioritized first, the rest in order
		d, ok := cp.take()
		if !ok || d.idx != want {
			t.Fatalf("take = (%d,%v), want %d", d.idx, ok, want)
		}
		cp.succeed(d.idx, d.pidx, tr, time.Millisecond)
	}

	// Adoption on a plan with somebody else to ask: chunk 0 has left the queue
	// and is charged to holder 0, so the FIRST dispatch is chunk 1 and it goes to
	// the holder carrying nothing.
	cp2 := testPlan(layout, []*BlobProvider{{Name: "h"}, {Name: "other"}})
	cp2.adoptFlight(0, 0, func() {})
	if len(cp2.flight[0]) != 1 || cp2.inFlight != 1 || cp2.prov[0].reqs != 1 {
		t.Fatalf("adopted plan: flight[0]=%d inFlight=%d reqs=%d",
			len(cp2.flight[0]), cp2.inFlight, cp2.prov[0].reqs)
	}
	if d, ok := cp2.take(); !ok || d.idx != 1 || d.pidx != 1 {
		t.Errorf("first dispatch after adoption = (chunk %d, holder %d, %v), want (1,1,true)",
			d.idx, d.pidx, ok)
	}
	// The speculation resolving is an ordinary success: watermark, progress,
	// and the holder's slot all come back.
	cp2.succeed(0, 0, tr, time.Millisecond)
	if !cp2.done[0] || cp2.watermark != 1 || cp2.remaining != 4 {
		t.Fatalf("after speculation landed: done[0]=%v watermark=%d remaining=%d",
			cp2.done[0], cp2.watermark, cp2.remaining)
	}
	if b := cp2.watermarkBytes(); b != 10 {
		t.Errorf("watermarkBytes = %d, want 10", b)
	}

	// ...and being a plan citizen means it is charged like one. On a SOLE holder
	// the adopted speculation occupies that holder's only request slot, so the
	// plan waits for chunk 0 rather than putting chunk 1 on the same link beside
	// it — which is the point of the sole-holder depth (work-queue slot 5): the
	// reader needs chunk 0 first, so nothing else belongs on that link yet.
	cp3 := testPlan(layout, holders)
	cp3.adoptFlight(0, 0, func() {})
	waited := make(chan int, 1)
	go func() {
		d, ok := cp3.take()
		if !ok {
			waited <- -1
			return
		}
		waited <- d.idx
	}()
	select {
	case idx := <-waited:
		t.Fatalf("chunk %d was dispatched to the sole holder while it is still "+
			"fetching the speculative chunk 0", idx)
	case <-time.After(200 * time.Millisecond):
	}
	// The slot comes back with the speculation, and the wait ends.
	cp3.succeed(0, 0, tr, time.Millisecond)
	select {
	case idx := <-waited:
		if idx != 1 {
			t.Errorf("dispatch after the speculation landed = chunk %d, want 1", idx)
		}
	case <-time.After(5 * time.Second):
		t.Error("the freed slot never woke the waiting worker")
	}
}

// TestBlockedReaderHedgesTheChunkItWaitsFor pins take()'s FIRST rule — the one
// F9 item 4 added for a stalled stream, and the one nothing else covers.
//
// Every other hedge test here and in the chaos suite is the ENDGAME: the queue
// is empty, so a free worker has nothing better to do than duplicate. That is
// not the case .issues/open-issues.md reported ("the streaming reader cannot
// escape a stalled in-flight chunk"). There the queue is full — the reader is
// waiting on chunk 0 while chunks 1..4 are still unfetched — and the old
// behaviour was for the free worker to start chunk 1, because prioritize could
// only reorder a queue the reader's chunk had already left.
//
// So the assertion is that the hedge BEATS the pending queue, and it is driven
// through the real reader entry point (transfer.WaitFor, what the relay's
// copyTransfer calls) rather than by poking prioritize directly: the wiring
// from a blocked read to a second copy is the thing that was missing.
func TestBlockedReaderHedgesTheChunkItWaitsFor(t *testing.T) {
	layout := buildLayout(50, 10, nil) // 5 chunks of 10 bytes
	cp := testPlan(layout, []*BlobProvider{{Name: "slow"}, {Name: "healthy"}})
	tr := newTransfer("h", "p", "p.part")
	tr.size = 50
	tr.beginChunks(layout, cp.prioritize)

	// The slow holder is handed chunk 0 and never resolves it. Chunks 1..4 stay
	// queued, which is what keeps take() out of its endgame branch.
	if idx := askHolder(t, cp, 0); idx != 0 {
		t.Fatalf("dispatched chunk %d, want 0", idx)
	}
	if len(cp.pending) == 0 {
		t.Fatal("pending queue empty — that is the endgame case, not this one")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readerDone := make(chan error, 1)
	go func() { readerDone <- tr.WaitFor(ctx, 5) }() // offset 5 = inside chunk 0

	// The reader's poke leaves its only possible trace on an in-flight chunk.
	deadline := time.Now().Add(2 * time.Second * testTimeoutScale)
	marked := false
	for !marked && time.Now().Before(deadline) {
		cp.mu.Lock()
		marked = cp.wanted[0]
		cp.mu.Unlock()
		if !marked {
			time.Sleep(time.Millisecond)
		}
	}
	if !marked {
		t.Fatal("a blocked reader's prioritize() left no mark on the in-flight chunk")
	}

	d, ok := cp.take()
	if !ok {
		t.Fatal("take() gave the free worker nothing")
	}
	if d.idx != 0 || !d.hedge {
		t.Fatalf("free worker got (idx=%d hedge=%v), want a hedge of chunk 0 — the "+
			"reader is blocked on it and chunk %d is merely next in the queue", d.idx, d.hedge, d.idx)
	}
	if d.pidx == 0 {
		t.Error("the hedge went back to the holder already sitting on chunk 0")
	}

	// And it has to actually wake the reader, not merely be dispatched.
	cp.succeed(0, d.pidx, tr, time.Millisecond)
	select {
	case err := <-readerDone:
		if err != nil {
			t.Fatalf("reader returned %v", err)
		}
	case <-time.After(2 * time.Second * testTimeoutScale):
		t.Fatal("the hedge landed and the blocked reader was not woken")
	}
}

// TestAdoptedFlightIsRacedAndItsLoserForgiven pins what adoption buys: the
// speculative chunk-0 attempt is hedged like any slow in-flight copy (the fix
// for the swarm start being gated on holders[0] — .issues/open-issues.md,
// swarm refactor pass finding 1), the rival landing cancels it, and its
// subsequent failure is a cancelled loser — blamed on nobody, requeued nowhere.
func TestAdoptedFlightIsRacedAndItsLoserForgiven(t *testing.T) {
	layout := wideLayout(1) // one chunk: the speculation holds the whole plan
	cp := testPlan(layout, []*BlobProvider{{Name: "dribbler"}, {Name: "healthy"}})
	tr := newTransfer("h", "p", "p.part")

	cancelled := false
	cp.adoptFlight(0, 0, func() { cancelled = true })

	// The queue is empty and chunk 0 is in the dribbler's hands: the endgame
	// must hand a second copy to the healthy holder rather than wait.
	d, ok := cp.take()
	if !ok || d.idx != 0 || d.pidx != 1 || !d.hedge {
		t.Fatalf("take = (idx=%d pidx=%d hedge=%v ok=%v), want a hedge of chunk 0 on the healthy holder",
			d.idx, d.pidx, d.hedge, ok)
	}

	// The healthy copy lands: the adopted attempt must be cancelled with it.
	cp.succeed(0, 1, tr, time.Millisecond)
	if !cancelled {
		t.Fatal("the rival landed and the adopted speculation was not cancelled")
	}
	if cp.remaining != 0 {
		t.Fatalf("remaining = %d after the rival landed, want 0", cp.remaining)
	}

	// The cancelled speculation resolves as a failure — on the done-chunk path,
	// so the dribbler collects no streak and nothing is requeued.
	cp.fail(0, 0, context.Canceled, false)
	if cp.prov[0].fails != 0 {
		t.Errorf("the losing speculation cost its holder a failure streak (%d)", cp.prov[0].fails)
	}
	if len(cp.pending) != 0 || cp.aborted {
		t.Errorf("pending=%d aborted=%v after a forgiven loser, want a finished plan",
			len(cp.pending), cp.aborted)
	}
}

// TestAdoptedFlightFailureRequeues: a speculation that fails while the chunk is
// still nobody else's is an ordinary failure — the chunk goes back in the
// queue, the attempt is counted, and the holder collects its streak.
func TestAdoptedFlightFailureRequeues(t *testing.T) {
	cp := testPlan(wideLayout(1), []*BlobProvider{{Name: "dribbler"}, {Name: "healthy"}})
	cp.adoptFlight(0, 0, func() {})

	cp.fail(0, 0, errors.New("mesh stalled"), false)
	if len(cp.pending) != 1 || cp.pending[0] != 0 {
		t.Fatalf("pending = %v after the speculation failed, want chunk 0 requeued", cp.pending)
	}
	if cp.attempts[0] != 1 || cp.prov[0].fails != 1 {
		t.Errorf("attempts[0]=%d fails=%d, want the failure counted like any dispatch",
			cp.attempts[0], cp.prov[0].fails)
	}
	if d, ok := cp.take(); !ok || d.idx != 0 || d.pidx != 1 {
		t.Errorf("take = (idx=%d pidx=%d ok=%v), want chunk 0 handed to the healthy holder",
			d.idx, d.pidx, ok)
	}
}

// TestChunkPlanFailover: a transient error on the sole holder retries (never
// fatal until the consecutive-failure limit, reset on success), while a corrupt
// chunk drops the holder immediately.
func TestChunkPlanFailover(t *testing.T) {
	man := &blobManifest{ChunkSize: 10, Size: 30, Chunks: []string{"a", "b", "c"}}
	layout := man.layout()
	netErr := errors.New("mesh stalled")

	cp := testPlan(layout, []*BlobProvider{{Name: "only"}})
	for i := 1; i < providerFailureLimit; i++ {
		d, ok := cp.take()
		if !ok {
			t.Fatalf("no chunk to dispatch at retry %d", i)
		}
		cp.fail(d.idx, d.pidx, netErr, false)
		if cp.aborted {
			t.Fatalf("aborted after %d transient failures (limit %d) — should retry", i, providerFailureLimit)
		}
	}
	// A success clears the streak, so transient failures are tolerated again.
	d, _ := cp.take()
	cp.succeed(d.idx, d.pidx, newTransfer("h", "p", "p.part"), time.Millisecond)
	d, _ = cp.take()
	cp.fail(d.idx, d.pidx, netErr, false)
	if cp.aborted {
		t.Fatal("aborted right after a success reset the failure streak")
	}

	// A corrupt chunk drops the sole holder immediately → abort.
	cp2 := testPlan(layout, []*BlobProvider{{Name: "liar"}})
	d, _ = cp2.take()
	cp2.fail(d.idx, d.pidx, errChunkCorrupt, true)
	if !cp2.aborted {
		t.Fatal("a corrupt chunk from the sole holder should abort")
	}
}

// wideLayout is a chunk layout with enough chunks that failures spread across
// them instead of exhausting one chunk's attempt budget — these tests are about
// who gets retired, not about the termination backstop.
func wideLayout(n int) *chunkLayout { return buildLayout(int64(10*n), 10, nil) }

// testPlan builds a scheduler on a clock a unit test does not have to wait out:
// the failure backoff is a millisecond (production is half a second), and the
// benchmark window is long enough that every holder asked during the test still
// counts as recently asked.
func testPlan(layout *chunkLayout, holders []*BlobProvider) *chunkPlan {
	return newChunkPlan(context.Background(), layout, holders, newTransferStats(),
		Timeouts{Retry: time.Millisecond, PerChunk: time.Minute})
}

// askHolder hands the next pending chunk to a NAMED holder, with the same
// bookkeeping take() does. The scheduler picks the holder itself, which is the
// point of it — these tests are about what happens after a holder was asked, so
// they choose.
func askHolder(t *testing.T, cp *chunkPlan, pidx int) int {
	t.Helper()
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if len(cp.pending) == 0 {
		t.Fatal("no chunk left to dispatch")
	}
	return cp.dispatchLocked(cp.unqueueLocked(0), pidx, false).idx
}

// TestChunkPlanRetirementIsRelative pins the fix for a healthy holder being
// dropped as if faulty (.issues/open-issues.md, -race findings, item 3).
//
// Retirement asks whether a holder is failing out of line with the others, not
// whether it crossed a fixed count. When some peer is still delivering, that is
// the old absolute rule exactly. When every holder is missing, the moment is bad
// rather than any one holder, and retiring them all is how a fetch used to kill
// itself with a perfectly good source in hand.
func TestChunkPlanRetirementIsRelative(t *testing.T) {
	netErr := errors.New("mesh stalled")

	t.Run("out of line with a delivering peer", func(t *testing.T) {
		cp := testPlan(wideLayout(12), []*BlobProvider{{Name: "bad"}, {Name: "good"}})
		tr := newTransfer("h", "p", "p.part")
		for i := 0; i < providerFailureLimit; i++ {
			cp.fail(askHolder(t, cp, 0), 0, netErr, false)
			// The peer has to be actually delivering, not merely present: since F9
			// item 3 a holder nobody has asked is no longer a benchmark.
			cp.succeed(askHolder(t, cp, 1), 1, tr, time.Millisecond)
		}
		if !cp.prov[0].dead {
			t.Error("a holder failing while its peer delivers should be retired")
		}
		if cp.prov[1].dead {
			t.Error("the delivering peer was retired")
		}
		if cp.aborted {
			t.Error("aborted while a live holder remained")
		}
	})

	t.Run("everyone is equally slow", func(t *testing.T) {
		cp := testPlan(wideLayout(12), []*BlobProvider{{Name: "a"}, {Name: "b"}})
		// Both holders miss the same number of times, alternating.
		for i := 0; i < providerFailureLimit; i++ {
			for _, pidx := range []int{0, 1} {
				cp.fail(askHolder(t, cp, pidx), pidx, netErr, false)
			}
		}
		if cp.prov[0].dead || cp.prov[1].dead {
			t.Errorf("a holder was retired in a slow moment: fails=%d/%d",
				cp.prov[0].fails, cp.prov[1].fails)
		}
		if cp.aborted {
			t.Error("aborted although both holders are still worth asking")
		}
	})

	t.Run("sole holder still terminates", func(t *testing.T) {
		cp := testPlan(wideLayout(12), []*BlobProvider{{Name: "only"}})
		for i := 0; i < providerFailureLimit; i++ {
			cp.fail(askHolder(t, cp, 0), 0, netErr, false)
		}
		// Nothing to compare against, so the absolute limit stands — otherwise a
		// fetch against a single dead holder would retry forever.
		if !cp.prov[0].dead {
			t.Error("the sole holder was never retired")
		}
		if !cp.aborted {
			t.Error("transfer did not abort with no live holder left")
		}
	})

	t.Run("an unasked holder is not a benchmark", func(t *testing.T) {
		// The rule load-aware dispatch made necessary. Under round-robin a streak
		// of 0 meant "this peer is delivering", because every live holder was
		// handed work in rotation. Now a holder can be deprioritised and sit at 0
		// without having earned it — and reading that as a clean record would keep
		// the strict absolute rule in force against a holder having the same bad
		// moment as everybody else.
		cp := testPlan(wideLayout(12), []*BlobProvider{{Name: "asked"}, {Name: "idle"}})
		cp.tryWindow = time.Millisecond // "recently" ends immediately
		for i := 0; i < providerFailureLimit; i++ {
			cp.fail(askHolder(t, cp, 0), 0, netErr, false)
			time.Sleep(2 * time.Millisecond)
		}
		if !cp.prov[0].dead {
			t.Error("the sole holder anyone asked was not retired, so nothing ends the fetch")
		}
		// And the reverse: a peer asked WITHIN the window does hold the rule back.
		cp2 := testPlan(wideLayout(12), []*BlobProvider{{Name: "asked"}, {Name: "also asked"}})
		askHolder(t, cp2, 1)
		for i := 0; i < providerFailureLimit; i++ {
			cp2.fail(askHolder(t, cp2, 0), 0, netErr, false)
		}
		if !cp2.prov[0].dead {
			t.Error("a holder failing beside a peer at streak 0 should still be retired")
		}
	})
}

// TestChunkPlanStopsDispatchingToAHolderThatNeverAnswers is the headline of F9
// item 3, and it is the same test that used to pin the defect.
//
// The measurement behind it: a madplayer fetching a 20 MB track was handed
// holders last seen 21 and 54 hours earlier, and the fetch took 4m12s–4m25s
// against 1m43s for the same server with one live holder and none stale.
// Dispatch was plain round-robin, so a holder that had never delivered a single
// byte kept being handed chunks until it had failed providerFailureLimit times
// — and each of those cost Timeouts.PerChunk, not ChunkStall, because a stale
// holder's dial never connects and the idle-read watchdog is never armed.
//
// Both halves of that are gone. A holder that fails is put to the back of the
// queue by the load rule and to the back of the tie-break by having no measured
// throughput, so ONE dispatch is all it gets while a live holder is delivering;
// and that one dispatch is now bounded by Timeouts.Connect rather than PerChunk.
func TestChunkPlanStopsDispatchingToAHolderThatNeverAnswers(t *testing.T) {
	// Index 0 is the ghost — advertised, dialled, never there. Index 1 is real.
	cp := testPlan(wideLayout(24), []*BlobProvider{{Name: "ghost"}, {Name: "live"}})
	tr := newTransfer("h", "p", "p.part")
	netErr := errors.New("mesh stalled")

	wasted := 0
	for {
		d, ok := cp.take()
		if !ok {
			break
		}
		if d.p.Name == "ghost" {
			wasted++
			cp.fail(d.idx, d.pidx, netErr, false)
			continue
		}
		cp.succeed(d.idx, d.pidx, tr, time.Millisecond)
	}

	if cp.remaining != 0 {
		t.Fatalf("the live holder did not finish the transfer: %d chunks left", cp.remaining)
	}
	if wasted != 1 {
		t.Errorf("the ghost absorbed %d dispatches, want 1 — the load rule should have "+
			"stopped asking it after the first", wasted)
	}
	if cp.prov[1].dead {
		t.Error("the holder that was actually delivering got retired")
	}
	t.Logf("a never-present holder absorbs %d dispatch, %s at the shipped Timeouts.Connect "+
		"— it was %d × PerChunk = %s", wasted, defaultTimeouts.Connect,
		providerFailureLimit, time.Duration(providerFailureLimit)*defaultTimeouts.PerChunk)
}

// TestChunkPlanAttemptLimit pins the other half of that change: retiring holders
// is no longer what ends a hopeless transfer, so something else must. A chunk
// that cannot be fetched aborts on its own attempt budget, with every holder
// still live — the case the relative rule deliberately refuses to resolve by
// killing sources.
func TestChunkPlanAttemptLimit(t *testing.T) {
	holders := []*BlobProvider{{Name: "a"}, {Name: "b"}}
	cp := testPlan(wideLayout(1), holders) // one chunk, so every failure lands on it
	netErr := errors.New("mesh stalled")

	if cp.attemptLimit != providerFailureLimit*len(holders) {
		t.Fatalf("attemptLimit = %d, want %d", cp.attemptLimit, providerFailureLimit*len(holders))
	}
	for i := 0; !cp.aborted; i++ {
		if i > cp.attemptLimit*2 {
			t.Fatal("transfer never gave up on an unfetchable chunk")
		}
		if len(cp.pending) == 0 {
			break
		}
		cp.fail(askHolder(t, cp, i%len(holders)), i%len(holders), netErr, false) // alternate, so streaks stay level
	}
	if !cp.aborted {
		t.Fatal("an unfetchable chunk must abort the transfer")
	}
	if cp.prov[0].dead || cp.prov[1].dead {
		t.Error("holders were retired to reach termination")
	}
	if cp.err == nil || !strings.Contains(cp.err.Error(), "unfetchable") {
		t.Errorf("abort error = %v, want it to name the unfetchable chunk", cp.err)
	}
	if !errors.Is(cp.err, netErr) {
		t.Errorf("abort error = %v, want the holder's error wrapped", cp.err)
	}
}
