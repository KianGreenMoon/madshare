//go:build !nofederation

package federation

import (
	"fmt"
	"io"
	"log"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"daemonlord.ygg/madshare/config"
)

// TestStopReleasesNetstack pins that a stopped node leaves nothing running.
//
// This is a regression guard with a specific history: Stop() used to shut the
// mesh HTTP server and stop the yggdrasil core but never touch the gVisor
// netstack, so every stopped node left a whole stack — its protocol workers plus
// the NIC's RST drain goroutine — running for the life of the process. A server
// that starts one node and stops it at exit never notices. The mesh test suite
// starts dozens, and the accumulated load made a two-node pairing that took ~30 s
// early in a -race run take over 240 s at the end, timing out three tests in
// setup on a handshake that had passed earlier in the same process.
//
// So the assertion is about the SLOPE, not an absolute count: goroutines must not
// grow with the number of start/stop cycles.
func TestStopReleasesNetstack(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()

	// A node with no peers and no listener: this test is about teardown, and a
	// bare core reaches steady state in a fraction of the time a peered one does.
	cycle := func(i int) {
		n, err := Start(config.FederationConfig{
			Name:    fmt.Sprintf("node-%d", i),
			KeyFile: filepath.Join(dir, fmt.Sprintf("%d.key", i)),
		}, newMemStore(), logger)
		if err != nil {
			t.Fatalf("start node %d: %v", i, err)
		}
		n.Stop()
	}

	// Baseline after one full cycle, so one-time allocations (package init, the
	// first stack's shared workers) are already counted.
	cycle(0)
	base := settleGoroutines(t, 0)

	const cycles = 5
	for i := 1; i <= cycles; i++ {
		cycle(i)
	}
	got := settleGoroutines(t, base)

	// Tolerance covers goroutines the runtime and the race detector keep around
	// per cycle; the leak this guards against was ~10 per node, so a real
	// regression clears this by a wide margin at 5 cycles.
	if limit := base + cycles; got > limit {
		t.Errorf("goroutines grew with start/stop cycles: %d after %d cycles (baseline %d, limit %d)\n%s",
			got, cycles, base, limit, goroutineDump())
	}
}

// settleGoroutines waits for the goroutine count to stop falling and returns it.
// Teardown is not instantaneous — the inbound reader exits only once the core
// has closed its PacketConn — so a count read straight after Stop() is noise.
// want is a target to stop early on (0 = just wait for quiet).
func settleGoroutines(t *testing.T, want int) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	last, stable := runtime.NumGoroutine(), 0
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		n := runtime.NumGoroutine()
		if want > 0 && n <= want {
			return n
		}
		if n == last {
			if stable++; stable >= 5 {
				return n
			}
			continue
		}
		last, stable = n, 0
	}
	return runtime.NumGoroutine()
}

// goroutineDump renders the live stacks, so a failure names the goroutines that
// were left behind instead of just counting them.
func goroutineDump() string {
	buf := make([]byte, 1<<16)
	return string(buf[:runtime.Stack(buf, true)])
}
