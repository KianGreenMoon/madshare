//go:build !nofederation

package federation

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// The step-1 measurement of docs/plans/maybe-to-do.md: reproduce request depth
// 1/2/4/8 on one capped 300 ms-RTT link, timing BOTH the transfer and the
// reader, and instrument which deadline actually fires on the depth-8 failure.
//
// It is a MEASUREMENT, not an assertion suite — it exists to be run and read,
// so it asserts nothing beyond "the harness itself worked" and is gated on its
// own env var (MADSHARE_MEASURE) rather than joining the chaos suite, whose
// unfiltered runs must not inherit minutes of sweep.
//
// The sweep runs the F9 item 4 scenario (4 MiB, one holder, rtt 300 ms, link
// capped 512 KiB/s — federation/scheduler.go, maxHolderRequests) on TWO clocks,
// because §4 of maybe-to-do.md found that the recorded attribution ("depth 8
// spends Timeouts.PerChunk") fails its own arithmetic — but only against the
// PRODUCTION constants (PerChunk 2 min), while the measurement that recorded it
// ran on the shrunk chaos clock (PerChunk 6 s), where eight chunks fair-sharing
// 512 KiB/s (~7.5 s each) really do outlast the budget:
//
//   - "chaos"      — the clock the original measurement used (PerChunk 6 s,
//     ChunkStall 2 s, Transfer 6 s), to reproduce the failure and name the
//     deadline that fires;
//   - "production" — the shipped defaults for every fetch deadline (PerChunk
//     2 min, ChunkStall 20 s), to find out whether the depth-8 failure exists
//     AT ALL outside the shrunk clock.
//
// Deadline attribution is read off evidence the stats already carry: a
// ChunkStall firing calls noteStall (counted in Stats.Stalls) and cancels, so
// its failures read "context canceled"; a PerChunk expiry reads "context
// deadline exceeded" with no stall counted; a connect-class failure names the
// dial. verdict() spells the rule out per cell.
func requireMeasure(t *testing.T) {
	t.Helper()
	if os.Getenv("MADSHARE_MEASURE") == "" {
		t.Skip("measurement sweep; set MADSHARE_MEASURE=1 to run")
	}
}

// measureCell is one (clock, depth) run's readout.
type measureCell struct {
	clock    string
	depth    int
	elapsed  time.Duration
	err      error
	worst    readWait
	reads    int
	wireDown int64
	stats    TransferStats
}

// measureChaosClock is the clock the original F9 measurement ran on.
func measureChaosClock() Timeouts {
	return Timeouts{
		Control:    chaosControl,
		Manifest:   chaosChunkStall,
		Connect:    chaosConnect,
		ChunkStall: chaosChunkStall,
		PerChunk:   chaosPerChunk,
		Transfer:   chaosTransfer,
		Retry:      chaosRetry,
	}
}

// measureProductionClock is the shipped defaults for every deadline a chunk
// fetch runs to (defaultTimeouts, node.go). Control/Manifest stay shrunk —
// they bound the pre-transfer probes, which are not under measurement, and the
// production 15/20 s would only slow the sweep's setup.
func measureProductionClock() Timeouts {
	return Timeouts{
		Control:    chaosControl,
		Manifest:   chaosChunkStall,
		Connect:    5 * time.Second,
		ChunkStall: 20 * time.Second,
		PerChunk:   2 * time.Minute,
		Transfer:   10 * time.Minute,
		Retry:      500 * time.Millisecond,
	}
}

// measureIntervals is the shrunk background cadence every cell runs on — the
// loops are setup, not the thing measured.
func measureIntervals() Option {
	return WithIntervals(Intervals{
		Refresh:     chaosRefresh,
		CatalogSync: chaosRefresh,
		SnapshotTTL: chaosSnapshot,
	})
}

func TestMeasureDepthSweep(t *testing.T) {
	requireMeasure(t)

	chaosClock := measureChaosClock()
	productionClock := measureProductionClock()

	var cells []measureCell
	for _, clock := range []struct {
		name string
		to   Timeouts
	}{{"chaos", chaosClock}, {"production", productionClock}} {
		for _, depth := range []int{1, 2, 4, 8} {
			name := fmt.Sprintf("%s/depth%d", clock.name, depth)
			t.Run(name, func(t *testing.T) {
				cells = append(cells, measureDepthCell(t, clock.name, clock.to, depth))
			})
		}
	}

	t.Log("sweep summary (4 MiB, one holder, rtt 300 ms, link 512 KiB/s):")
	for _, c := range cells {
		outcome := "ok"
		if c.err != nil {
			outcome = "FAILED: " + c.err.Error()
		}
		t.Logf("  %-10s depth %d: transfer %8v  worst read %8v (%d reads)  "+
			"payload %3.0f KiB/s  wire %3.0f KiB/s  %s  %s",
			c.clock, c.depth, c.elapsed.Round(time.Millisecond),
			c.worst.wait.Round(time.Millisecond), c.reads,
			rateKiB(4<<20, c.elapsed), rateKiB(c.wireDown, c.elapsed),
			verdict(c.stats, c.err), outcome)
	}
}

// measureDepthCell runs one cell of the sweep and reports it. A failed transfer
// is a DATA POINT, not a test failure — the depth-8 failure is the thing being
// measured — so only a harness breakage (mesh never came up, transfer never
// ended) fails the test.
func measureDepthCell(t *testing.T, clockName string, to Timeouts, depth int) measureCell {
	t.Helper()
	content := fillBytes(4 << 20) // 9 chunks: lead 256 KiB + 8 × 512 KiB
	storeA, storeB := newMemStore(), newMemStore()
	_, resolveA := publishBlob(t, storeA, content)
	cacheB := t.TempDir()

	intervals := measureIntervals()
	a, b, link := startFaultedPair(t, storeA, storeB,
		[]Option{intervals, WithTimeouts(to), resolveA},
		[]Option{intervals, WithTimeouts(to), WithCacheDir(cacheB)})
	hash := friendsHolding(t, a, b, storeA, storeB, content)
	warmMesh(t, a, b)

	f := rtt(300 * time.Millisecond)
	f.Down.Bandwidth = 512 << 10
	link.Set(f)
	wireBase := link.Stats().BytesDown

	measureRequestDepth = depth
	defer func() { measureRequestDepth = 0 }()

	// Bounded by the cell's own generous deadline, not chaosDeadline: the
	// production clock is entitled to spend minutes failing, and how long it
	// spends is part of the answer.
	deadline := 5 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	started := time.Now()
	tr, err := b.EnsureBlob(ctx, hash)
	if err != nil {
		t.Fatalf("EnsureBlob: %v", err)
	}

	// The reader half, concurrent with the transfer exactly as the streaming
	// relay is: its waits are the number slot 5 was invisible without.
	type readResult struct {
		waits []readWait
		err   error
	}
	readCh := make(chan readResult, 1)
	go func() {
		waits, rerr := streamWaits(ctx, t, tr, int64(len(content)))
		readCh <- readResult{waits, rerr}
	}()

	var terr error
	select {
	case <-tr.Done():
		terr = tr.Err()
	case <-time.After(deadline):
		t.Fatalf("transfer neither finished nor failed within %v\n%s", deadline, describe(tr.Stats()))
	}
	elapsed := time.Since(started)
	read := <-readCh

	st := tr.Stats()
	cell := measureCell{
		clock: clockName, depth: depth, elapsed: elapsed, err: terr,
		worst: worstWait(read.waits), reads: len(read.waits),
		wireDown: link.Stats().BytesDown - wireBase, stats: st,
	}
	floor := time.Duration(float64(512<<10) / (512 << 10) * float64(time.Second)) // one 512 KiB bulk chunk at 512 KiB/s
	t.Logf("depth %d on the %s clock: transfer %v (err=%v), reader: %d reads, "+
		"worst %v at offset %d (floor ≈ %v), reader err=%v\nwire down: %d bytes for a %d-byte payload (%.1f%% overhead)\n%s\nverdict: %s",
		depth, clockName, elapsed.Round(time.Millisecond), terr,
		len(read.waits), cell.worst.wait.Round(time.Millisecond), cell.worst.off, floor,
		read.err, cell.wireDown, len(content),
		100*(float64(cell.wireDown)/float64(len(content))-1),
		describe(st), verdict(st, terr))
	return cell
}

// TestMeasureMultiHolderDepth is maybe-to-do.md §7 question 3: does per-holder
// depth 2 buy anything at all in a MULTI-holder plan? The constant's own
// justification ("the second slot is what keeps the pipe full across the RTT",
// scheduler.go maxHolderRequests) was only ever measured on a sole holder, where
// slot 5 later took the second slot away again.
//
// The scenario is the knob's BEST case, on purpose: two holders, each behind its
// own 300 ms-RTT link capped at 512 KiB/s — symmetric, so the scheduler's
// load-balancing is not the variable, and latent, so per-holder dead air exists
// for a second slot to hide. If depth 2 buys nothing here it buys nothing
// anywhere, and the knob is decoration; if it buys something, this is the shape
// it earns its keep in. Production clock only — the chaos clock's deadline
// artifacts are established (§8) and would only muddy a throughput question.
func TestMeasureMultiHolderDepth(t *testing.T) {
	requireMeasure(t)
	to := measureProductionClock()

	// Two sizes, because a 9-chunk plan is mostly ramp + endgame and the
	// pipelining claim is a STEADY-STATE claim: 16 MiB (18 chunks, bulk 1 MiB)
	// gives the mid-transfer state room to show a depth benefit if one exists.
	type cellSpec struct {
		size  int
		depth int
	}
	specs := []cellSpec{
		{4 << 20, 1}, {4 << 20, 2}, {4 << 20, 4},
		{16 << 20, 1}, {16 << 20, 2},
	}
	type line struct {
		spec  cellSpec
		run   int
		cell  measureCell
		split string
	}
	var lines []line
	for _, spec := range specs {
		for run := 1; run <= 2; run++ {
			t.Run(fmt.Sprintf("%dMiB/depth%d/run%d", spec.size>>20, spec.depth, run), func(t *testing.T) {
				cell, split := measureTrioCell(t, to, spec.size, spec.depth)
				lines = append(lines, line{spec, run, cell, split})
			})
		}
	}

	t.Log("multi-holder sweep summary (TWO holders, each rtt 300 ms + 512 KiB/s, production clock):")
	for _, l := range lines {
		outcome := "ok"
		if l.cell.err != nil {
			outcome = "FAILED: " + l.cell.err.Error()
		}
		t.Logf("  %2d MiB depth %d run %d: transfer %8v  worst read %8v (%d reads)  "+
			"payload %3.0f KiB/s  split %s  %s  %s",
			l.spec.size>>20, l.spec.depth, l.run, l.cell.elapsed.Round(time.Millisecond),
			l.cell.worst.wait.Round(time.Millisecond), l.cell.reads,
			rateKiB(int64(l.spec.size), l.cell.elapsed), l.split, verdict(l.cell.stats, l.cell.err), outcome)
	}
}

// measureTrioCell is one (depth, run) cell of the multi-holder sweep: the
// faulted-trio topology (two seeders, each behind its own proxy), both links
// identically degraded, and the same two instruments as the sole-holder cells.
// The extra return is the byte split across the holders — the number that shows
// whether both links were actually pulling their weight.
func measureTrioCell(t *testing.T, to Timeouts, size, depth int) (measureCell, string) {
	t.Helper()
	content := fillBytes(size)
	storeA, storeB, storeC := newMemStore(), newMemStore(), newMemStore()
	hash, resolveA := publishBlob(t, storeA, content)
	_, resolveC := publishBlob(t, storeC, content)
	cacheB := t.TempDir()

	intervals := measureIntervals()
	a, b, c, linkA, linkC := startFaultedTrio(t, storeA, storeB, storeC,
		[]Option{intervals, WithTimeouts(to), resolveA},
		[]Option{intervals, WithTimeouts(to), WithCacheDir(cacheB)},
		[]Option{intervals, WithTimeouts(to), resolveC}, 0, 0)
	makeFriends(t, a, b, storeA, storeB)
	makeFriends(t, c, b, storeC, storeB)
	seedBlobCatalog(t, storeB, a, hash, int64(len(content)))
	seedBlobCatalog(t, storeB, c, hash, int64(len(content)))
	warmMesh(t, a, b)
	warmMesh(t, c, b)

	f := rtt(300 * time.Millisecond)
	f.Down.Bandwidth = 512 << 10
	linkA.Set(f)
	linkC.Set(f)
	wireBase := linkA.Stats().BytesDown + linkC.Stats().BytesDown

	measureRequestDepth = depth
	defer func() { measureRequestDepth = 0 }()

	deadline := 5 * time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()

	started := time.Now()
	tr, err := b.EnsureBlob(ctx, hash)
	if err != nil {
		t.Fatalf("EnsureBlob: %v", err)
	}
	type readResult struct {
		waits []readWait
		err   error
	}
	readCh := make(chan readResult, 1)
	go func() {
		waits, rerr := streamWaits(ctx, t, tr, int64(len(content)))
		readCh <- readResult{waits, rerr}
	}()

	var terr error
	select {
	case <-tr.Done():
		terr = tr.Err()
	case <-time.After(deadline):
		t.Fatalf("transfer neither finished nor failed within %v\n%s", deadline, describe(tr.Stats()))
	}
	elapsed := time.Since(started)
	read := <-readCh

	st := tr.Stats()
	split := ""
	for _, p := range st.Providers {
		if split != "" {
			split += " + "
		}
		split += fmt.Sprintf("%dK", p.Bytes>>10)
	}
	cell := measureCell{
		clock: "production", depth: depth, elapsed: elapsed, err: terr,
		worst: worstWait(read.waits), reads: len(read.waits),
		wireDown: linkA.Stats().BytesDown + linkC.Stats().BytesDown - wireBase,
		stats:    st,
	}
	t.Logf("depth %d, two holders: transfer %v (err=%v), reader: %d reads, worst %v at "+
		"offset %d, reader err=%v\nwire down both links: %d bytes for a %d-byte payload, "+
		"holder split %s\n%s\nverdict: %s",
		depth, elapsed.Round(time.Millisecond), terr,
		len(read.waits), cell.worst.wait.Round(time.Millisecond), cell.worst.off,
		read.err, cell.wireDown, len(content), split,
		describe(st), verdict(st, terr))
	return cell, split
}

// rateKiB is bytes over a duration as KiB/s.
func rateKiB(bytes int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(bytes) / 1024 / d.Seconds()
}

// verdict names the deadline the evidence points at, from the stats a transfer
// already keeps: every ChunkStall firing is counted in Stalls before it cancels
// its fetch, a PerChunk expiry is a bare "context deadline exceeded", and a
// refused/failed dial names itself.
func verdict(st TransferStats, terr error) string {
	fails := 0
	deadline, canceled, dial := false, false, false
	for _, p := range st.Providers {
		fails += p.Failures
		e := p.LastError
		deadline = deadline || strings.Contains(e, "deadline exceeded")
		canceled = canceled || strings.Contains(e, "context canceled")
		dial = dial || strings.Contains(e, "dial") || strings.Contains(e, "connect")
	}
	switch {
	case terr == nil && st.Stalls == 0 && fails == 0:
		return "clean (no deadline fired)"
	case st.Stalls > 0 && deadline:
		return fmt.Sprintf("BOTH fired: ChunkStall ×%d and PerChunk (deadline exceeded)", st.Stalls)
	case st.Stalls > 0:
		return fmt.Sprintf("ChunkStall fired ×%d (idle-read watchdog)", st.Stalls)
	case deadline:
		return "PerChunk fired (deadline exceeded, no stall counted)"
	case dial:
		return "connect-class failure"
	case canceled:
		return "cancellations only (hedge losers or shutdown)"
	case fails > 0:
		return "failures without a deadline signature"
	default:
		return "no failures recorded"
	}
}
