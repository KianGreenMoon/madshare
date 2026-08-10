//go:build !nofederation

package federation

import (
	"context"
	"io"
	"log"
	"testing"
	"time"
)

// The digest served to friends is memoized, which is the entire rate limit on
// GET /madnetwork/v0/graph: a friend pulling too often gets the memo rather
// than a refusal, because syncGraph cannot tell a 429 from a peer that has no
// such endpoint (docs/architecture/federation-trust.md §Refreshing the graph on
// demand).
func TestGraphDigestIsMemoized(t *testing.T) {
	store := newMemStore()
	n := &Node{store: store, intervals: Intervals{GraphDigestTTL: time.Hour}, logger: log.New(io.Discard, "", 0)}
	ctx := context.Background()

	if _, _, _, err := n.ownDigest(ctx); err != nil {
		t.Fatalf("ownDigest: %v", err)
	}
	before := store.digests

	for i := 0; i < 20; i++ {
		if _, _, _, err := n.ownDigest(ctx); err != nil {
			t.Fatalf("ownDigest: %v", err)
		}
	}
	if store.digests != before {
		t.Errorf("store hit %d extra times, want 0: repeat pulls must cost a map read",
			store.digests-before)
	}
}

// Staleness is bounded by the TTL only when nothing happened. Anything that
// changes what we hold — a record learned, a branch dropped — invalidates the
// memo, so a friend never waits out the window for news.
func TestGraphDigestInvalidatesOnChange(t *testing.T) {
	store := newMemStore()
	n := &Node{store: store, intervals: Intervals{GraphDigestTTL: time.Hour}, logger: log.New(io.Discard, "", 0)}
	ctx := context.Background()

	if _, _, _, err := n.ownDigest(ctx); err != nil {
		t.Fatalf("ownDigest: %v", err)
	}
	before := store.digests

	n.invalidateGraphDigest()
	if _, _, _, err := n.ownDigest(ctx); err != nil {
		t.Fatalf("ownDigest: %v", err)
	}
	if store.digests != before+1 {
		t.Errorf("store hit %d times after invalidation, want one rebuild", store.digests-before)
	}
}

// The Rescan button is a flag the sweep consumes, so holding it down costs one
// round rather than one round per press.
func TestResyncGraphCoalesces(t *testing.T) {
	n := &Node{nudge: make(chan struct{}, 1), graphAccept: map[string]time.Time{}}

	// A record we declined a moment ago is exactly what the button is being
	// pressed for, so the accept throttle must not survive it.
	n.graphAccept["someorigin"] = time.Now()

	for i := 0; i < 5; i++ {
		n.ResyncGraph()
	}
	if !n.forceGraph.Swap(false) {
		t.Fatal("the force flag should be set after a press")
	}
	if n.forceGraph.Swap(false) {
		t.Error("the flag survived being consumed: a sweep would force twice")
	}
	if len(n.nudge) != 1 {
		t.Errorf("nudge holds %d wake-ups, want 1 coalesced", len(n.nudge))
	}
	if len(n.graphAccept) != 0 {
		t.Error("the accept throttle survived a rescan: the button would silently no-op")
	}
}
