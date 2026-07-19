//go:build !nofederation

package federation

import (
	"context"
	"testing"
)

// TestTransferChunkAvailability pins the per-chunk readiness the streaming relay
// relies on: an out-of-order (tail) chunk is readable before the front arrives,
// Available reflects only contiguously-readable runs, and WaitFor on a missing
// offset pokes the seek-priority hook and honours ctx cancellation.
func TestTransferChunkAvailability(t *testing.T) {
	tr := newTransfer("h", "final", "final.part")
	tr.size = 30
	var poked []int
	tr.beginChunks(10, 3, func(idx int) { poked = append(poked, idx) })

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

// TestChunkPlanPrioritizeAndDone0 covers the two scheduling changes: prioritize
// jumps a pending chunk to the front of dispatch, and done0 pre-completes chunk
// 0 (the prefetched-chunk-0 case) so dispatch starts at chunk 1.
func TestChunkPlanPrioritizeAndDone0(t *testing.T) {
	man := &blobManifest{ChunkSize: 10, Size: 50, Chunks: []string{"a", "b", "c", "d", "e"}}
	holders := []*Peer{{Name: "h"}}

	cp := newChunkPlan(man, holders, false)
	cp.prioritize(3)
	if idx, ok := cp.next(); !ok || idx != 3 {
		t.Fatalf("prioritized next = (%d,%v), want (3,true)", idx, ok)
	}
	for _, want := range []int{0, 1, 2, 4} { // the rest keep their order
		if idx, ok := cp.next(); !ok || idx != want {
			t.Fatalf("next = (%d,%v), want %d", idx, ok, want)
		}
	}

	cp2 := newChunkPlan(man, holders, true)
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
