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

// TestChunkPlanPrioritizeAndDone0 covers the two scheduling changes: prioritize
// jumps a pending chunk to the front of dispatch, and done0 pre-completes chunk
// 0 (the prefetched-chunk-0 case) so dispatch starts at chunk 1.
func TestChunkPlanPrioritizeAndDone0(t *testing.T) {
	man := &blobManifest{ChunkSize: 10, Size: 50, Chunks: []string{"a", "b", "c", "d", "e"}}
	layout := man.layout()
	holders := []*BlobProvider{{Name: "h"}}

	cp := testPlan(layout, holders, false)
	cp.prioritize(3)
	if idx, _, _, ok := cp.take(); !ok || idx != 3 {
		t.Fatalf("prioritized take = (%d,%v), want (3,true)", idx, ok)
	}
	for _, want := range []int{0, 1, 2, 4} { // the rest keep their order
		if idx, _, _, ok := cp.take(); !ok || idx != want {
			t.Fatalf("take = (%d,%v), want %d", idx, ok, want)
		}
	}

	cp2 := testPlan(layout, holders, true)
	if !cp2.done[0] || cp2.watermark != 1 || cp2.remaining != 4 {
		t.Fatalf("done0 plan: done[0]=%v watermark=%d remaining=%d", cp2.done[0], cp2.watermark, cp2.remaining)
	}
	if b := cp2.watermarkBytes(); b != 10 {
		t.Errorf("watermarkBytes = %d, want 10", b)
	}
	if idx, _, _, ok := cp2.take(); !ok || idx != 1 {
		t.Errorf("done0 first dispatch = (%d,%v), want (1,true)", idx, ok)
	}
}

// TestChunkPlanFailover: a transient error on the sole holder retries (never
// fatal until the consecutive-failure limit, reset on success), while a corrupt
// chunk drops the holder immediately.
func TestChunkPlanFailover(t *testing.T) {
	man := &blobManifest{ChunkSize: 10, Size: 30, Chunks: []string{"a", "b", "c"}}
	layout := man.layout()
	netErr := errors.New("mesh stalled")

	cp := testPlan(layout, []*BlobProvider{{Name: "only"}}, false)
	for i := 1; i < providerFailureLimit; i++ {
		idx, _, pidx, ok := cp.take()
		if !ok {
			t.Fatalf("no chunk to dispatch at retry %d", i)
		}
		cp.fail(idx, pidx, netErr, false)
		if cp.aborted {
			t.Fatalf("aborted after %d transient failures (limit %d) — should retry", i, providerFailureLimit)
		}
	}
	// A success clears the streak, so transient failures are tolerated again.
	idx, _, pidx, _ := cp.take()
	cp.succeed(idx, pidx, newTransfer("h", "p", "p.part"), time.Millisecond)
	idx, _, pidx, _ = cp.take()
	cp.fail(idx, pidx, netErr, false)
	if cp.aborted {
		t.Fatal("aborted right after a success reset the failure streak")
	}

	// A corrupt chunk drops the sole holder immediately → abort.
	cp2 := testPlan(layout, []*BlobProvider{{Name: "liar"}}, false)
	idx, _, pidx, _ = cp2.take()
	cp2.fail(idx, pidx, errChunkCorrupt, true)
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
func testPlan(layout *chunkLayout, holders []*BlobProvider, done0 bool) *chunkPlan {
	return newChunkPlan(layout, holders, done0, nil,
		Timeouts{Retry: time.Millisecond, PerChunk: time.Minute})
}

// dispatch hands the next pending chunk to a NAMED holder, with the same
// bookkeeping take() does. The scheduler picks the holder itself, which is the
// point of it — these tests are about what happens after a holder was asked, so
// they choose.
func dispatch(t *testing.T, cp *chunkPlan, pidx int) int {
	t.Helper()
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if len(cp.pending) == 0 {
		t.Fatal("no chunk left to dispatch")
	}
	idx, _ := cp.dispatchLocked(0, pidx)
	return idx
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
		cp := testPlan(wideLayout(12), []*BlobProvider{{Name: "bad"}, {Name: "good"}}, false)
		tr := newTransfer("h", "p", "p.part")
		for i := 0; i < providerFailureLimit; i++ {
			cp.fail(dispatch(t, cp, 0), 0, netErr, false)
			// The peer has to be actually delivering, not merely present: since F9
			// item 3 a holder nobody has asked is no longer a benchmark.
			cp.succeed(dispatch(t, cp, 1), 1, tr, time.Millisecond)
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
		cp := testPlan(wideLayout(12), []*BlobProvider{{Name: "a"}, {Name: "b"}}, false)
		// Both holders miss the same number of times, alternating.
		for i := 0; i < providerFailureLimit; i++ {
			for _, pidx := range []int{0, 1} {
				cp.fail(dispatch(t, cp, pidx), pidx, netErr, false)
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
		cp := testPlan(wideLayout(12), []*BlobProvider{{Name: "only"}}, false)
		for i := 0; i < providerFailureLimit; i++ {
			cp.fail(dispatch(t, cp, 0), 0, netErr, false)
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
		cp := testPlan(wideLayout(12), []*BlobProvider{{Name: "asked"}, {Name: "idle"}}, false)
		cp.tryWindow = time.Millisecond // "recently" ends immediately
		for i := 0; i < providerFailureLimit; i++ {
			cp.fail(dispatch(t, cp, 0), 0, netErr, false)
			time.Sleep(2 * time.Millisecond)
		}
		if !cp.prov[0].dead {
			t.Error("the sole holder anyone asked was not retired, so nothing ends the fetch")
		}
		// And the reverse: a peer asked WITHIN the window does hold the rule back.
		cp2 := testPlan(wideLayout(12), []*BlobProvider{{Name: "asked"}, {Name: "also asked"}}, false)
		dispatch(t, cp2, 1)
		for i := 0; i < providerFailureLimit; i++ {
			cp2.fail(dispatch(t, cp2, 0), 0, netErr, false)
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
	cp := testPlan(wideLayout(24), []*BlobProvider{{Name: "ghost"}, {Name: "live"}}, false)
	tr := newTransfer("h", "p", "p.part")
	netErr := errors.New("mesh stalled")

	wasted := 0
	for {
		idx, p, pidx, ok := cp.take()
		if !ok {
			break
		}
		if p.Name == "ghost" {
			wasted++
			cp.fail(idx, pidx, netErr, false)
			continue
		}
		cp.succeed(idx, pidx, tr, time.Millisecond)
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
	cp := testPlan(wideLayout(1), holders, false) // one chunk, so every failure lands on it
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
		cp.fail(dispatch(t, cp, i%len(holders)), i%len(holders), netErr, false) // alternate, so streaks stay level
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
