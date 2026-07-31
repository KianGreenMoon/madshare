//go:build !nofederation

package federation

import (
	"context"
	"io"
	"log"
	"testing"
	"time"
)

// TestInboundHealthy covers the self-health signal the availability predicate
// reads (docs/plans/availability.md Phase 1): a nil reader signal is treated as
// healthy, and a wired signal is reported verbatim.
func TestInboundHealthy(t *testing.T) {
	if !(&Node{}).InboundHealthy() {
		t.Fatal("a Node with no reader signal should default healthy")
	}
	healthy := &Node{readerAlive: func() bool { return true }}
	if !healthy.InboundHealthy() {
		t.Fatal("readerAlive true → healthy")
	}
	dead := &Node{readerAlive: func() bool { return false }}
	if dead.InboundHealthy() {
		t.Fatal("readerAlive false → unhealthy")
	}
}

// TestObservePeerAlive verifies the transfer-path liveness touch: it advances
// the holder's last_seen, is throttled per holder, and is a safe no-op without a
// store. Since F7 item 5 it writes to the SOURCE row — most holders are members
// with no peer row at all, and the freshness window reads the later of the two
// clocks anyway.
func TestObservePeerAlive(t *testing.T) {
	ms := newMemStore()
	src, err := ms.EnsureCatalogSource(context.Background(), "aa", 0)
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}
	n := &Node{
		store:       ms,
		logger:      log.New(io.Discard, "", 0),
		transferCtx: context.Background(),
		lastTouch:   map[int64]time.Time{},
	}
	lastSeen := func() int64 {
		ms.mu.Lock()
		defer ms.mu.Unlock()
		return ms.sources[src.ID].LastSeen
	}

	// First delivery touches last_seen.
	n.observePeerAlive(&BlobProvider{SourceID: src.ID})
	if lastSeen() == 0 {
		t.Fatal("observePeerAlive should have advanced last_seen")
	}

	// A second delivery within the throttle window must not write again: reset
	// the stored value to 0 and confirm the throttled call leaves it untouched.
	ms.mu.Lock()
	ms.sources[src.ID].LastSeen = 0
	ms.mu.Unlock()
	n.observePeerAlive(&BlobProvider{SourceID: src.ID})
	if got := lastSeen(); got != 0 {
		t.Fatalf("throttled delivery should not touch last_seen, got %d", got)
	}

	// No store / nil holder / a holder with no source row must not panic.
	(&Node{}).observePeerAlive(&BlobProvider{SourceID: src.ID})
	n.observePeerAlive(nil)
	n.observePeerAlive(&BlobProvider{})
}
