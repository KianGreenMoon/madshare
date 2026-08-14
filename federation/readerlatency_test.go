//go:build !nofederation

package federation

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Reader-latency scenarios (docs/plans/mesh-testing.md §Phase T2, same gating as
// chaos_test.go — requireChaos keeps them out of the default `go test ./...`).
//
// Every other swarm scenario asks how long a TRANSFER took. These ask how long
// a READER waited, which is a different number and the one a listener actually
// feels: playback stops when a single read outlasts the buffer, whatever the
// total throughput was. The two came apart under measurement on 2026-08-14 —
// request depth 2 costs the reader 2–4× and the transfer nothing (see
// docs/plans/work-queue.md §5) — and the reason nobody noticed for a month is
// that no test timed a read.
//
// So this file exists mainly as a floor to measure future swarm work against.
// Read the logged numbers; the assertions are deliberately loose, and each one
// says which of the two kinds it is:
//
//   - REGRESSION GUARDS assert behaviour that is settled and must not come back
//     (the blocked-reader hedge, and the reader never waiting a wild multiple of
//     what the link can physically deliver).
//   - MEASUREMENTS log and assert only correctness. The number they exist to
//     report is under an open decision, so pinning it would freeze the very
//     thing that is meant to change.

// readWait is one blocking read: where the reader was, and how long it waited.
type readWait struct {
	off  int64
	wait time.Duration
}

// streamWaits drives exactly the loop the streaming relay runs
// (api/madnetwork_transfer_handlers.go copyTransfer): WaitFor an offset, read
// what Available says landed, move on — and times each wait. Reading as fast as
// the bytes arrive is the honest worst case: a real player also empties its
// buffer at the front of a cold stream.
func streamWaits(ctx context.Context, t *testing.T, tr Transfer, size int64) ([]readWait, error) {
	t.Helper()
	var out []readWait
	var off int64
	for off < size {
		started := time.Now()
		if err := tr.WaitFor(ctx, off); err != nil {
			return out, err
		}
		w := time.Since(started)
		avail := tr.Available(off)
		if avail <= 0 {
			return out, fmt.Errorf("WaitFor(%d) returned with nothing readable", off)
		}
		out = append(out, readWait{off, w})
		off += avail
	}
	return out, nil
}

// worstWait returns the longest single read and where it happened.
func worstWait(waits []readWait) readWait {
	var worst readWait
	for _, w := range waits {
		if w.wait > worst.wait {
			worst = w
		}
	}
	return worst
}

// logWaits prints every read that blocked long enough to be worth seeing, plus
// the summary line. The per-read list is what shows WHERE a stream is thin —
// the summary alone hid the 768 KiB finding's refutation.
//
// ref is the time one chunk physically costs on this link, and every scenario
// states what it means for that scenario: the price the reader should be paying
// (a sole holder), or the price it is being spared (a hedged one).
func logWaits(t *testing.T, label, refName string, waits []readWait, ref time.Duration, tr Transfer) readWait {
	t.Helper()
	for _, w := range waits {
		if w.wait > ref/4 {
			t.Logf("  %s: offset %9d  wait %v", label, w.off, w.wait.Round(time.Millisecond))
		}
	}
	worst := worstWait(waits)
	t.Logf("%s: %d reads, worst %v at offset %d (%s ≈ %v → %.1f×)\n%s",
		label, len(waits), worst.wait.Round(time.Millisecond), worst.off,
		refName, ref.Round(time.Millisecond), float64(worst.wait)/float64(ref), describe(tr.Stats()))
	return worst
}

// TestChaosBlockedReaderIsRescuedByAHedge is the END-TO-END half of F9 item 4's
// reader rule — a REGRESSION GUARD, and the scenario the issue row
// ".issues/open-issues.md → the streaming reader cannot escape a stalled
// in-flight chunk" described.
//
// The unit-level half is TestBlockedReaderHedgesTheChunkItWaitsFor
// (streaming_test.go), which proves take() prefers the reader's chunk over the
// queue. This proves the whole path: a real reader on a real mesh, blocked on a
// chunk a throttled holder is sitting on, gets it from somebody else.
//
// C is throttled to 128 KiB/s — slow enough that owning any chunk of the
// reader's would be visible in seconds, fast enough that nothing times out, so
// a rescue cannot be confused with a failover. Measured over three runs before
// this was written: worst read 20/39/109 ms, hedges won every time, and the slow
// holder delivered zero bytes because it lost every race it entered.
func TestChaosBlockedReaderIsRescuedByAHedge(t *testing.T) {
	requireChaos(t)
	content := fillBytes(2 << 20) // 2 MiB → 8 chunks of 256 KiB
	storeA, storeB, storeC := newMemStore(), newMemStore(), newMemStore()
	hash, resolveA := publishBlob(t, storeA, content)
	_, resolveC := publishBlob(t, storeC, content)
	cacheB := t.TempDir()

	a, b, c, _, linkC := startFaultedTrio(t, storeA, storeB, storeC,
		chaosOpts(resolveA), chaosOpts(WithCacheDir(cacheB)), chaosOpts(resolveC), 0, 0)
	makeFriends(t, a, b, storeA, storeB)
	makeFriends(t, c, b, storeC, storeB)
	seedBlobCatalog(t, storeB, a, hash, int64(len(content)))
	seedBlobCatalog(t, storeB, c, hash, int64(len(content)))

	linkC.Set(slowDown(128 << 10))

	ctx, cancel := context.WithTimeout(context.Background(), chaosDeadline)
	defer cancel()

	tr, err := b.EnsureBlob(ctx, hash)
	if err != nil {
		t.Fatalf("EnsureBlob: %v", err)
	}
	waits, err := streamWaits(ctx, t, tr, int64(len(content)))
	if err != nil {
		t.Fatalf("stream read: %v\n%s", err, describe(tr.Stats()))
	}
	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("transfer failed: %v\n%s", err, describe(tr.Stats()))
	}
	// The reference here is what the reader is being SPARED: one 256 KiB chunk at
	// C's 128 KiB/s. A ratio far below 1 is the result — it means no read was
	// left waiting on the slow holder.
	worst := logWaits(t, "hedged", "one chunk at the slow holder's rate", waits, 2*time.Second, tr)

	st := tr.Stats()
	if st.HedgesWon == 0 {
		t.Errorf("no hedge won (hedges=%d) — nothing raced a chunk away from the slow "+
			"holder, so the reader was not rescued, it was merely lucky\n%s",
			st.Hedges, describe(st))
	}
	// Loose on purpose: the claim is that the reader never waits out the slow
	// holder, not that it hits any particular millisecond. Observed ≤ 109 ms
	// against this 2 s ceiling.
	if worst.wait >= chaosChunkStall {
		t.Errorf("a single read waited %v at offset %d (≥ ChunkStall %v) — the reader "+
			"was left on the slow holder\n%s",
			worst.wait, worst.off, chaosChunkStall, describe(st))
	}
	assertCached(t, cacheB, hash, content)
}

// TestChaosReaderLatencyOnASoleCappedHolder is the household shape, where a
// listener node's only holder is its home server and the link is the bottleneck.
// Hedging cannot reach it (hedgeLocked needs a holder that is not already
// fetching the chunk) and reordering cannot reach a dispatched chunk, so
// whatever the reader waits here, it waits.
//
// It was a MEASUREMENT until 2026-08-14 and is now an ASSERTION, because the
// question it was measuring got answered: with a single live holder the plan
// asks for one chunk at a time (requestCapLocked, work-queue slot 5), so the
// reader waits one chunk and no more. The number to read is still the multiple
// of the floor in the summary line.
//
// Measured over three runs each. At the old maxHolderRequests=2 the worst read
// was 2–4× the floor, because the second slot put a chunk nobody had asked for
// on the same capped link as the chunk the reader was blocked on. At depth 1 it
// is the floor: on this link, 8 uniform reads of ~595 ms against a 500 ms floor
// — 1.2×, identical in all three runs — and on the 128 KiB/s link the decision
// was taken from, 2.396/2.405/2.415 s against 2 s. Total transfer time is
// unchanged either way, which is why only a reader-timed test can see this.
func TestChaosReaderLatencyOnASoleCappedHolder(t *testing.T) {
	requireChaos(t)
	content := fillBytes(2 << 20) // 8 chunks of 256 KiB
	storeA, storeB := newMemStore(), newMemStore()
	_, resolveA := publishBlob(t, storeA, content)
	cacheB := t.TempDir()

	a, b, link := startFaultedPair(t, storeA, storeB,
		chaosOpts(resolveA), chaosOpts(WithCacheDir(cacheB)))
	hash := friendsHolding(t, a, b, storeA, storeB, content)
	warmMesh(t, a, b)

	const rate = 512 << 10 // 512 KiB/s → one 256 KiB chunk ≈ 500 ms
	link.Set(slowDown(rate))

	ctx, cancel := context.WithTimeout(context.Background(), chaosDeadline)
	defer cancel()

	tr, err := b.EnsureBlob(ctx, hash)
	if err != nil {
		t.Fatalf("EnsureBlob: %v", err)
	}
	waits, err := streamWaits(ctx, t, tr, int64(len(content)))
	if err != nil {
		t.Fatalf("stream read: %v\n%s", err, describe(tr.Stats()))
	}
	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("transfer failed: %v\n%s", err, describe(tr.Stats()))
	}
	floor := time.Duration(float64(minChunkSize) / rate * float64(time.Second))
	worst := logWaits(t, "sole capped", "one chunk at this link's rate", waits, floor, tr)

	// The claim: a reader on a sole capped holder waits ONE chunk, because that
	// holder is only asked for one. Observed 1.2× the floor over three runs, so
	// 2× is the whole handshake-and-scheduling budget and still less than half
	// what depth 2 cost (2–4×) — a regression to sharing the link fails here.
	if limit := 2 * floor * testTimeoutScale; worst.wait > limit {
		t.Errorf("worst read %v at offset %d is over %v (2× the link's own floor for one "+
			"chunk) — the reader is sharing its link with a chunk nobody asked for\n%s",
			worst.wait, worst.off, limit, describe(tr.Stats()))
	}
	assertCached(t, cacheB, hash, content)
}

// TestChaosReaderLatencyAcrossTheRamp is a MEASUREMENT of the front of a cold
// stream, where a player's buffer is thinnest and where the adaptive layout does
// its work: chunk 0 is fetched speculatively before the manifest, the lead ramp
// keeps the first chunks small, and bulk chunks follow.
//
// 16 MiB is not arbitrary — it is the smallest convenient size that reaches the
// 1 MiB bulk cap (chunkSizeFor: size/targetChunks), which puts the lead ramp at
// [256 KiB, 512 KiB] and the first full-size chunk at exactly 768 KiB. That
// boundary was reported as a stall; measured 2026-08-14 it is not one — at
// 1 MiB/s the reader did not block there in four runs (the parallel workers land
// the bulk chunks while it is still stuck at the front) and its worst read was
// at 256 KiB, the handover from the speculative chunk 0 to chunk 1. This test
// runs a faster link than that measurement, so the boundary sometimes IS a
// blocking read here — cheaply, well inside one chunk time, which is the same
// conclusion by a different route: the cost is the chunk size, not the seam.
//
// Kept as a measurement rather than an assertion because WHERE the front is
// thinnest is a property of the layout and the scheduler together, and both are
// things a rewrite is entitled to change — but it should change them knowingly,
// and the per-read log is what makes that visible.
func TestChaosReaderLatencyAcrossTheRamp(t *testing.T) {
	requireChaos(t)
	content := fillBytes(16 << 20) // ≥ 12 MiB → bulk = 1 MiB, lead = [256K, 512K]
	storeA, storeB := newMemStore(), newMemStore()
	_, resolveA := publishBlob(t, storeA, content)
	cacheB := t.TempDir()

	a, b, link := startFaultedPair(t, storeA, storeB,
		chaosOpts(resolveA), chaosOpts(WithCacheDir(cacheB)))
	hash := friendsHolding(t, a, b, storeA, storeB, content)
	warmMesh(t, a, b)

	const rate = 4 << 20 // 4 MiB/s → one 1 MiB bulk chunk ≈ 250 ms
	link.Set(slowDown(rate))

	ctx, cancel := context.WithTimeout(context.Background(), chaosDeadline)
	defer cancel()

	tr, err := b.EnsureBlob(ctx, hash)
	if err != nil {
		t.Fatalf("EnsureBlob: %v", err)
	}
	waits, err := streamWaits(ctx, t, tr, int64(len(content)))
	if err != nil {
		t.Fatalf("stream read: %v\n%s", err, describe(tr.Stats()))
	}
	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("transfer failed: %v\n%s", err, describe(tr.Stats()))
	}
	floor := time.Duration(float64(maxChunkSize) / rate * float64(time.Second))
	worst := logWaits(t, "ramp", "one bulk chunk at this link's rate", waits, floor, tr)

	// Report the boundary explicitly — it is the number the issue row is about,
	// and "the reader never blocked there" is itself the finding.
	blocked := false
	for _, w := range waits {
		if w.off == 768<<10 {
			blocked = true
			t.Logf("lead→bulk boundary (768 KiB) was a blocking read: %v", w.wait.Round(time.Millisecond))
		}
	}
	if !blocked {
		t.Logf("lead→bulk boundary (768 KiB) was not a blocking read at all — the bulk " +
			"chunks landed while the reader was still at the front")
	}

	// Same guard as above, same reason.
	if limit := 8 * floor * testTimeoutScale; worst.wait > limit {
		t.Errorf("worst read %v at offset %d is over %v (8× one bulk chunk at this rate) "+
			"— the front of the stream is being starved\n%s",
			worst.wait, worst.off, limit, describe(tr.Stats()))
	}
	assertCached(t, cacheB, hash, content)
}
