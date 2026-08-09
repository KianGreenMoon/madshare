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

	cp := newChunkPlan(man, layout, holders, false, nil)
	cp.prioritize(3)
	if idx, ok := cp.next(); !ok || idx != 3 {
		t.Fatalf("prioritized next = (%d,%v), want (3,true)", idx, ok)
	}
	for _, want := range []int{0, 1, 2, 4} { // the rest keep their order
		if idx, ok := cp.next(); !ok || idx != want {
			t.Fatalf("next = (%d,%v), want %d", idx, ok, want)
		}
	}

	cp2 := newChunkPlan(man, layout, holders, true, nil)
	if !cp2.done[0] || cp2.watermark != 1 || cp2.remaining != 4 {
		t.Fatalf("done0 plan: done[0]=%v watermark=%d remaining=%d", cp2.done[0], cp2.watermark, cp2.remaining)
	}
	if b := cp2.watermarkBytes(); b != 10 {
		t.Errorf("watermarkBytes = %d, want 10", b)
	}
	if idx, ok := cp2.next(); !ok || idx != 1 {
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

	cp := newChunkPlan(man, layout, []*BlobProvider{{Name: "only"}}, false, nil)
	for i := 1; i < providerFailureLimit; i++ {
		idx, ok := cp.next()
		if !ok {
			t.Fatalf("no chunk to dispatch at retry %d", i)
		}
		cp.fail(idx, 0, netErr, false)
		if cp.aborted {
			t.Fatalf("aborted after %d transient failures (limit %d) — should retry", i, providerFailureLimit)
		}
	}
	// A success clears the streak, so transient failures are tolerated again.
	idx, _ := cp.next()
	cp.succeed(idx, 0, newTransfer("h", "p", "p.part"))
	idx, _ = cp.next()
	cp.fail(idx, 0, netErr, false)
	if cp.aborted {
		t.Fatal("aborted right after a success reset the failure streak")
	}

	// A corrupt chunk drops the sole holder immediately → abort.
	cp2 := newChunkPlan(man, layout, []*BlobProvider{{Name: "liar"}}, false, nil)
	idx, _ = cp2.next()
	cp2.fail(idx, 0, errChunkCorrupt, true)
	if !cp2.aborted {
		t.Fatal("a corrupt chunk from the sole holder should abort")
	}
}

// wideManifest is a manifest with enough chunks that failures spread across them
// instead of exhausting one chunk's attempt budget — these tests are about who
// gets retired, not about the termination backstop.
func wideManifest(n int) (*blobManifest, *chunkLayout) {
	man := &blobManifest{ChunkSize: 10, Size: int64(10 * n), Chunks: make([]string, n)}
	for i := range man.Chunks {
		man.Chunks[i] = "h"
	}
	return man, man.layout()
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
		man, layout := wideManifest(8)
		cp := newChunkPlan(man, layout, []*BlobProvider{{Name: "bad"}, {Name: "good"}}, false, nil)
		for i := 0; i < providerFailureLimit; i++ {
			idx, ok := cp.next()
			if !ok {
				t.Fatalf("no chunk to dispatch at failure %d", i)
			}
			cp.fail(idx, 0, netErr, false) // provider 0 only; provider 1 stays at streak 0
		}
		if !cp.dead[0] {
			t.Error("a holder failing while its peer delivers should be retired")
		}
		if cp.dead[1] {
			t.Error("the delivering peer was retired")
		}
		if cp.aborted {
			t.Error("aborted while a live holder remained")
		}
	})

	t.Run("everyone is equally slow", func(t *testing.T) {
		man, layout := wideManifest(8)
		cp := newChunkPlan(man, layout, []*BlobProvider{{Name: "a"}, {Name: "b"}}, false, nil)
		// Both holders miss the same number of times, alternating.
		for i := 0; i < providerFailureLimit; i++ {
			for _, pidx := range []int{0, 1} {
				idx, ok := cp.next()
				if !ok {
					t.Fatalf("no chunk to dispatch at failure %d/%d", i, pidx)
				}
				cp.fail(idx, pidx, netErr, false)
			}
		}
		if cp.dead[0] || cp.dead[1] {
			t.Errorf("a holder was retired in a slow moment: dead=%v provFails=%v", cp.dead, cp.provFails)
		}
		if cp.aborted {
			t.Error("aborted although both holders are still worth asking")
		}
	})

	t.Run("sole holder still terminates", func(t *testing.T) {
		man, layout := wideManifest(8)
		cp := newChunkPlan(man, layout, []*BlobProvider{{Name: "only"}}, false, nil)
		for i := 0; i < providerFailureLimit; i++ {
			idx, _ := cp.next()
			cp.fail(idx, 0, netErr, false)
		}
		// Nothing to compare against, so the absolute limit stands — otherwise a
		// fetch against a single dead holder would retry forever.
		if !cp.dead[0] {
			t.Error("the sole holder was never retired")
		}
		if !cp.aborted {
			t.Error("transfer did not abort with no live holder left")
		}
	})
}

// TestChunkPlanKeepsDispatchingToAHolderThatNeverAnswers puts a number on what a
// stale holder in the fetch plan costs, measured against a live server on
// 2026-08-09: a madplayer fetching a 20 MB track was handed holders last seen 21
// and 54 hours earlier, and the fetch took 4m12s–4m25s against 1m43s for the
// same server with one live holder and none stale.
//
// This is where that time comes from. Dispatch is plain round-robin
// (pickProvider), so a holder that has never delivered a single byte keeps being
// handed chunks until it has failed providerFailureLimit times.
//
// What each of those failures COSTS is measured in TestStaleHoldersCostAFetch,
// and it is not what this comment first claimed. The obvious answer is
// Timeouts.ChunkStall (20 s), the idle-read watchdog — but a stale holder's dial
// never connects, so no response header ever arrives and that watchdog is never
// armed. The binding deadline is the per-chunk backstop, **PerChunk, 2 minutes**.
//
// The waste is bounded and the transfer is correct, which is why this is a cost
// rather than a bug: the live holder carries everything and the fetch completes.
// It is pinned because the fix has two halves in two repos — a freshness cutoff
// on the plan (database.MadnetworkBlobProviders, see
// TestMadnetworkBlobProvidersKeepStaleCatalogHolders) and, if dispatch ever
// becomes speed-aware, worseThanPeers has to be re-read with it
// (.issues/open-issues.md §"swarm provider selection is speed-blind").
func TestChunkPlanKeepsDispatchingToAHolderThatNeverAnswers(t *testing.T) {
	man, layout := wideManifest(24)
	// Index 0 is the ghost — advertised, dialled, never there. Index 1 is real.
	cp := newChunkPlan(man, layout, []*BlobProvider{{Name: "ghost"}, {Name: "live"}}, false, nil)
	netErr := errors.New("mesh stalled")

	wasted := 0
	for !cp.dead[0] {
		idx, ok := cp.next()
		if !ok {
			t.Fatal("ran out of chunks before the ghost was retired")
		}
		p, pidx, ok := cp.pickProvider()
		if !ok {
			t.Fatal("no live provider left")
		}
		if p.Name == "ghost" {
			wasted++
			cp.fail(idx, pidx, netErr, false)
			continue
		}
		cp.succeed(idx, pidx, newTransfer("h", "p", "p.part"))
	}

	if wasted != providerFailureLimit {
		t.Errorf("the ghost absorbed %d dispatches before retirement, want %d",
			wasted, providerFailureLimit)
	}
	if cp.dead[1] {
		t.Error("the holder that was actually delivering got retired")
	}
	if cp.aborted {
		t.Error("aborted with a live holder still in hand")
	}
	// The claim in production terms, so the number above means something.
	t.Logf("a never-present holder absorbs %d dispatches before retirement — %s at "+
		"the shipped 2-minute PerChunk (NOT ChunkStall: the dial never connects, so "+
		"the idle-read watchdog never arms)", wasted, time.Duration(wasted)*2*time.Minute)
}

// TestChunkPlanAttemptLimit pins the other half of that change: retiring holders
// is no longer what ends a hopeless transfer, so something else must. A chunk
// that cannot be fetched aborts on its own attempt budget, with every holder
// still live — the case the relative rule deliberately refuses to resolve by
// killing sources.
func TestChunkPlanAttemptLimit(t *testing.T) {
	man, layout := wideManifest(1) // one chunk, so every failure lands on it
	holders := []*BlobProvider{{Name: "a"}, {Name: "b"}}
	cp := newChunkPlan(man, layout, holders, false, nil)
	netErr := errors.New("mesh stalled")

	if cp.attemptLimit != providerFailureLimit*len(holders) {
		t.Fatalf("attemptLimit = %d, want %d", cp.attemptLimit, providerFailureLimit*len(holders))
	}
	for i := 0; !cp.aborted; i++ {
		if i > cp.attemptLimit*2 {
			t.Fatal("transfer never gave up on an unfetchable chunk")
		}
		idx, ok := cp.next()
		if !ok {
			break
		}
		cp.fail(idx, i%len(holders), netErr, false) // alternate, so streaks stay level
	}
	if !cp.aborted {
		t.Fatal("an unfetchable chunk must abort the transfer")
	}
	if cp.dead[0] || cp.dead[1] {
		t.Errorf("holders were retired to reach termination: dead=%v", cp.dead)
	}
	if cp.err == nil || !strings.Contains(cp.err.Error(), "unfetchable") {
		t.Errorf("abort error = %v, want it to name the unfetchable chunk", cp.err)
	}
	if !errors.Is(cp.err, netErr) {
		t.Errorf("abort error = %v, want the holder's error wrapped", cp.err)
	}
}
