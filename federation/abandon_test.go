//go:build !nofederation

package federation

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"path/filepath"
	"testing"
	"time"

	"daemonlord.ygg/madshare/config"
)

// Transfers run on the node's lifetime on purpose (cache-through), so until
// Abandon existed there was NO way to stop one: a fetch a listener node gave
// up on ran to completion in the background, in parallel with the relay
// fallback for the very same bytes (madplayer .issues/open-issues.md row 1,
// 2026-08-15). This pins the surface that fixes it: an abandoned mid-flight
// transfer ends promptly with an error, leaves the node's active list, and a
// second Abandon is a no-op.
func TestAbandonStopsARunningTransfer(t *testing.T) {
	content := fillBytes(2 << 20) // 2 MiB
	storeA, storeB := newMemStore(), newMemStore()
	hash, resolveA := publishBlob(t, storeA, content)
	cacheB := t.TempDir()

	// startNodePair, except the seeder is capped at 64 KiB/s: left alone this
	// fetch takes ~32s, so a transfer that is gone within the short wait below
	// stopped because it was told to, not because it finished.
	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	underlay := fmt.Sprintf("tcp://%s", probe.Addr())
	probe.Close()

	a, err := Start(config.FederationConfig{
		Name: "node-a", KeyFile: filepath.Join(dir, "a.key"),
		Listen: []string{underlay}, SeedRateKiB: 64,
	}, storeA, logger, resolveA)
	if err != nil {
		t.Fatalf("start node A: %v", err)
	}
	t.Cleanup(a.Stop)
	b, err := Start(config.FederationConfig{
		Name: "node-b", KeyFile: filepath.Join(dir, "b.key"), Peers: []string{underlay},
	}, storeB, logger, WithCacheDir(cacheB))
	if err != nil {
		t.Fatalf("start node B: %v", err)
	}
	t.Cleanup(b.Stop)

	makeFriends(t, a, b, storeA, storeB)
	seedBlobCatalog(t, storeB, a, hash, int64(len(content)))

	tr, err := b.EnsureBlob(context.Background(), hash)
	if err != nil {
		t.Fatalf("EnsureBlob: %v", err)
	}

	// Abandon a transfer that is demonstrably mid-flight: first byte landed,
	// far more still to come.
	wctx, cancel := context.WithTimeout(context.Background(), meshDeadline)
	defer cancel()
	if err := tr.WaitFor(wctx, 0); err != nil {
		t.Fatalf("waiting for the first byte: %v", err)
	}
	abandoned := time.Now()
	tr.Abandon()

	select {
	case <-tr.Done():
	case <-time.After(10 * time.Second * testTimeoutScale):
		t.Fatal("an abandoned transfer kept running")
	}
	if took := time.Since(abandoned); took > 10*time.Second*testTimeoutScale {
		t.Fatalf("abandoned transfer took %s to stop", took)
	}
	if tr.Err() == nil {
		t.Fatal("an abandoned transfer reported success without the bytes")
	}
	waitFor(t, "the transfer to leave the active list", func() bool {
		return len(b.ActiveTransfers()) == 0
	})
	tr.Abandon() // idempotent, including after the run is gone
}
