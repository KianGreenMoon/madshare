//go:build !nofederation

package federation

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Tests for the T1 test seams (docs/plans/mesh-testing.md §Phase T1): the
// injectable intervals/timeouts and the per-transfer stats. The seams exist so
// the chaos suite can assert *how* a transfer went inside a sane wall-clock, so
// they need the same guarantee themselves — that an override actually reaches
// the code path, and that an unset field still gets the production default.

func TestIntervalsAndTimeoutsDefaults(t *testing.T) {
	// An empty override is exactly the production configuration.
	if got := (Intervals{}).withDefaults(defaultIntervals); got != defaultIntervals {
		t.Errorf("zero Intervals resolved to %+v, want the defaults %+v", got, defaultIntervals)
	}
	if got := (Timeouts{}).withDefaults(defaultTimeouts); got != defaultTimeouts {
		t.Errorf("zero Timeouts resolved to %+v, want the defaults %+v", got, defaultTimeouts)
	}

	// A partial override keeps every field it did not name.
	iv := Intervals{CatalogSync: 5 * time.Millisecond}.withDefaults(defaultIntervals)
	if iv.CatalogSync != 5*time.Millisecond {
		t.Errorf("CatalogSync = %v, want the override", iv.CatalogSync)
	}
	if iv.Refresh != defaultIntervals.Refresh || iv.SnapshotTTL != defaultIntervals.SnapshotTTL {
		t.Errorf("partial override leaked into %+v", iv)
	}
	to := Timeouts{ChunkStall: 7 * time.Millisecond}.withDefaults(defaultTimeouts)
	if to.ChunkStall != 7*time.Millisecond {
		t.Errorf("ChunkStall = %v, want the override", to.ChunkStall)
	}
	if to.PerChunk != defaultTimeouts.PerChunk || to.Control != defaultTimeouts.Control {
		t.Errorf("partial override leaked into %+v", to)
	}

	// A negative value is treated as unset, not as an instantly-expiring ticker.
	if got := (Intervals{Refresh: -time.Second}).withDefaults(defaultIntervals); got.Refresh != defaultIntervals.Refresh {
		t.Errorf("negative Refresh = %v, want the default", got.Refresh)
	}
}

// TestWithIntervalsDrivesSweeps proves the interval seam end to end: two real
// nodes with sub-second cadences re-pull a changed catalog on their own, with no
// Nudge to force the sweep. All three intervals are load-bearing here — the
// refresh ticker to run the sweep, the catalog interval to make the pull due,
// and the snapshot TTL to stop A serving its memoized old snapshot.
func TestWithIntervalsDrivesSweeps(t *testing.T) {
	// Split deliberately by role: only the puller (B) needs a fast sweep and a
	// short catalog interval, and only the server (A) needs a short snapshot TTL
	// — otherwise A keeps handing out its memoized old catalog however often B
	// asks. Overriding just the field each side uses keeps the ping/sync churn
	// down and shows which interval does what.
	serving := []Option{WithIntervals(Intervals{SnapshotTTL: 20 * time.Millisecond})}
	pulling := []Option{WithIntervals(Intervals{
		Refresh:     100 * time.Millisecond,
		CatalogSync: 100 * time.Millisecond,
	})}
	storeA, storeB := newMemStore(), newMemStore()
	storeA.setPublished([]CatalogEntry{{
		Key: "1", RecordingKey: "r1", Title: "First",
		Renditions: []CatalogRendition{{Hash: "h1", Size: 10}},
	}})
	a, b := startNodePair(t, storeA, storeB, serving, pulling)
	makeFriends(t, a, b, storeA, storeB)

	ctx := context.Background()
	peerA, err := storeB.GetFederationPeerByKey(ctx, a.PublicKeyHex())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "B's first catalog pull", func() bool {
		return len(storeB.cachedCatalog(peerA.ID)) == 1
	})

	// Publish a second entry on A and let the cadence carry it across — no
	// Nudge, no resetSync: only the injected intervals can make this happen.
	storeA.setPublished([]CatalogEntry{
		{Key: "1", RecordingKey: "r1", Title: "First",
			Renditions: []CatalogRendition{{Hash: "h1", Size: 10}}},
		{Key: "2", RecordingKey: "r2", Title: "Second",
			Renditions: []CatalogRendition{{Hash: "h2", Size: 20}}},
	})
	waitFor(t, "B to re-sync A's changed catalog unaided", func() bool {
		return len(storeB.cachedCatalog(peerA.ID)) == 2
	})
}

// TestTransferStatsAccounting pins the diagnostic bookkeeping without a mesh:
// per-provider bytes and failures, the retry/corrupt counters, and — the one
// that carries a chaos scenario — a failover being counted only when a piece is
// delivered by a holder *other* than one that failed it.
func TestTransferStatsAccounting(t *testing.T) {
	fast := &Peer{Name: "fast", PublicKey: "aa"}
	slow := &Peer{Name: "slow", PublicKey: "bb"}
	boom := errors.New("mesh stalled")

	s := newTransferStats()
	s.setMode("swarm")
	s.setChunks(3)

	// Chunk 0: delivered first time — no failover.
	s.noteSucceed(0, fast, 100)
	// Chunk 1: slow fails it, fast delivers it — one failover.
	s.noteFail(1, slow, boom, false)
	s.noteSucceed(1, fast, 100)
	// Chunk 2: slow fails it twice (the second time with corrupt bytes, which
	// drops it) and then delivers it anyway — two retries, but the same holder
	// finished the job, so still not a failover.
	s.noteFail(2, slow, boom, false)
	s.noteFail(2, slow, errChunkCorrupt, true)
	s.noteDropped(slow)
	s.noteSucceed(2, slow, 50)
	s.noteStall()

	got := s.snapshot("deadbeef", 250, 250)
	if got.Mode != "swarm" || got.Hash != "deadbeef" || got.Size != 250 || got.Progress != 250 {
		t.Errorf("snapshot header = %+v", got)
	}
	if got.Chunks != 3 || got.ChunksDone != 3 {
		t.Errorf("chunks = %d/%d, want 3/3", got.ChunksDone, got.Chunks)
	}
	if got.Failovers != 1 {
		t.Errorf("failovers = %d, want 1 (only chunk 1 changed holder)", got.Failovers)
	}
	if got.Retries != 3 || got.Corrupt != 1 || got.Stalls != 1 {
		t.Errorf("retries/corrupt/stalls = %d/%d/%d, want 3/1/1", got.Retries, got.Corrupt, got.Stalls)
	}
	if len(got.Providers) != 2 {
		t.Fatalf("providers = %+v, want two", got.Providers)
	}
	// Order is the order holders were first seen, so a scenario can talk about
	// "the first holder the tracker offered".
	pf, ps := got.Providers[0], got.Providers[1]
	if pf.Name != "fast" || pf.Bytes != 200 || pf.Chunks != 2 || pf.Failures != 0 || pf.Dropped {
		t.Errorf("fast holder = %+v", pf)
	}
	if ps.Name != "slow" || ps.Bytes != 50 || ps.Chunks != 1 || ps.Failures != 3 || !ps.Dropped {
		t.Errorf("slow holder = %+v", ps)
	}
	if ps.LastError == "" {
		t.Error("a failing holder should carry its last error")
	}

	// Elapsed keeps running until the transfer ends, then freezes.
	if got.Elapsed <= 0 {
		t.Error("Elapsed should be running")
	}
	s.finish()
	frozen := s.snapshot("deadbeef", 250, 250).Elapsed
	time.Sleep(2 * time.Millisecond)
	if again := s.snapshot("deadbeef", 250, 250).Elapsed; again != frozen {
		t.Errorf("Elapsed moved after finish: %v → %v", frozen, again)
	}
}

// TestTransferStatsFirstByte: the time-to-first-byte is what scenario 4 (the
// lead-ramp + chunk-0 prefetch claim) measures, so it must be set by the
// progress path itself — and cleared when a failed attempt takes the bytes back.
func TestTransferStatsFirstByte(t *testing.T) {
	tr := newTransfer("h", "p", "p.part")
	if ttfb := tr.Stats().FirstByte; ttfb != 0 {
		t.Errorf("FirstByte before any bytes = %v, want 0", ttfb)
	}
	tr.addProgress(64)
	first := tr.Stats().FirstByte
	if first == 0 {
		t.Fatal("FirstByte still 0 after the front of the file became readable")
	}
	tr.addProgress(64)
	if again := tr.Stats().FirstByte; again != first {
		t.Errorf("FirstByte moved on later bytes: %v → %v", first, again)
	}
	// A failed attempt restarts from zero: readers lost the prefix, so the
	// measurement must describe the live attempt, not the abandoned one.
	tr.resetProgress()
	if ttfb := tr.Stats().FirstByte; ttfb != 0 {
		t.Errorf("FirstByte after a reset = %v, want 0", ttfb)
	}
	// Cumulative history survives the reset.
	tr.stats.noteFail(wholePiece, &Peer{Name: "gone", PublicKey: "cc"}, errors.New("dead"), false)
	tr.resetProgress()
	if r := tr.Stats().Retries; r != 1 {
		t.Errorf("retries after a reset = %d, want the history kept (1)", r)
	}
}

// TestTransferStatsPriorAttempt: what a reset clears is archived, not lost. A
// transfer that fetched half its chunks and then fell back to the whole-file
// path used to report mode=whole chunks=0/0 — the failed phase erased itself
// from the only readout anyone looks at.
func TestTransferStatsPriorAttempt(t *testing.T) {
	st := newTransferStats()
	st.setMode("swarm")
	st.setChunks(8)
	st.noteFirstByte()
	st.noteSucceed(0, &Peer{Name: "a", PublicKey: "aa"}, 4096)

	st.resetAttempt() // the swarm→whole fallback
	st.setMode("whole")

	snap := st.snapshot("h", 0, 0)
	if snap.Mode != "whole" || snap.Chunks != 0 || snap.ChunksDone != 0 || snap.FirstByte != 0 {
		t.Errorf("live attempt not clean: mode=%s chunks=%d/%d ttfb=%v",
			snap.Mode, snap.ChunksDone, snap.Chunks, snap.FirstByte)
	}
	if len(snap.Prior) != 1 {
		t.Fatalf("prior attempts = %d, want 1", len(snap.Prior))
	}
	if p := snap.Prior[0]; p.Mode != "swarm" || p.Chunks != 8 || p.ChunksDone != 1 || p.FirstByte == 0 {
		t.Errorf("archived attempt = %+v, want the swarm phase's 1/8 chunks and its ttfb", p)
	}
	// Transfer-wide history is not split per attempt.
	if len(snap.Providers) != 1 || snap.Providers[0].Bytes != 4096 {
		t.Errorf("per-provider bytes should survive the reset, got %+v", snap.Providers)
	}

	// An attempt that got nowhere is not archived, even though it has a mode:
	// runWhole names the mode once and then walks its holders, resetting between
	// each, so a fetch against three dead holders would otherwise report three
	// blank abandoned attempts.
	st2 := newTransferStats()
	st2.setMode("whole")
	st2.resetAttempt()
	st2.resetAttempt()
	if p := st2.snapshot("h", 0, 0).Prior; len(p) != 0 {
		t.Errorf("attempts that reached nothing were archived: %+v", p)
	}

	// A whole-file holder that delivered some bytes before dying IS worth
	// recording — it says the fetch was working and then lost its source.
	st3 := newTransferStats()
	st3.setMode("whole")
	st3.noteFirstByte()
	st3.resetAttempt()
	if p := st3.snapshot("h", 0, 0).Prior; len(p) != 1 || p[0].Mode != "whole" || p[0].FirstByte == 0 {
		t.Errorf("partial whole-file attempt not archived: %+v", p)
	}
}
