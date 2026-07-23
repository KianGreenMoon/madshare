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

// TestObservePeerAlive verifies the transfer-path liveness touch: it advances a
// peer's last_seen, is throttled per peer, and is a safe no-op without a store.
func TestObservePeerAlive(t *testing.T) {
	ms := newMemStore()
	id, err := ms.InsertFederationPeer(context.Background(), &Peer{
		PublicKey: "aa", State: PeerFriend, LastSeen: 0,
	})
	if err != nil {
		t.Fatalf("insert peer: %v", err)
	}
	n := &Node{
		store:       ms,
		logger:      log.New(io.Discard, "", 0),
		transferCtx: context.Background(),
		lastTouch:   map[int64]time.Time{},
	}

	// First delivery touches last_seen.
	n.observePeerAlive(&Peer{ID: id})
	p, _ := ms.GetFederationPeer(context.Background(), id)
	if p.LastSeen == 0 {
		t.Fatal("observePeerAlive should have advanced last_seen")
	}

	// A second delivery within the throttle window must not write again: reset
	// the stored value to 0 and confirm the throttled call leaves it untouched.
	ms.peers[id].LastSeen = 0
	n.observePeerAlive(&Peer{ID: id})
	if p, _ := ms.GetFederationPeer(context.Background(), id); p.LastSeen != 0 {
		t.Fatalf("throttled delivery should not touch last_seen, got %d", p.LastSeen)
	}

	// No store / nil peer must not panic.
	(&Node{}).observePeerAlive(&Peer{ID: id})
	n.observePeerAlive(nil)
}
