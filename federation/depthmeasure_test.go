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

func TestMeasureDepthSweep(t *testing.T) {
	requireMeasure(t)

	chaosClock := Timeouts{
		Control:    chaosControl,
		Manifest:   chaosChunkStall,
		Connect:    chaosConnect,
		ChunkStall: chaosChunkStall,
		PerChunk:   chaosPerChunk,
		Transfer:   chaosTransfer,
		Retry:      chaosRetry,
	}
	// The shipped defaults for every deadline a chunk fetch runs to
	// (defaultTimeouts, node.go). Control/Manifest stay shrunk — they bound the
	// pre-transfer probes, which are not under measurement, and the production
	// 15/20 s would only slow the sweep's setup.
	productionClock := Timeouts{
		Control:    chaosControl,
		Manifest:   chaosChunkStall,
		Connect:    5 * time.Second,
		ChunkStall: 20 * time.Second,
		PerChunk:   2 * time.Minute,
		Transfer:   10 * time.Minute,
		Retry:      500 * time.Millisecond,
	}

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

	intervals := WithIntervals(Intervals{
		Refresh:     chaosRefresh,
		CatalogSync: chaosRefresh,
		SnapshotTTL: chaosSnapshot,
	})
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
