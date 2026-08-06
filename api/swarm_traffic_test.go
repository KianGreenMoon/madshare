package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

// The flusher's contract (docs/architecture/swarm-admin.md): drain once, add
// once, and never read-modify-write — every database write is an increment, so
// nothing can be lost to a racing fetch.

func TestTrafficFlusher_DrainsOnceAndAdds(t *testing.T) {
	node := &fakeFederation{pending: []federation.TrafficDelta{
		{Hash: "aa", TrafficCounters: federation.TrafficCounters{Up: 100, Down: 40}},
		{Hash: "bb", TrafficCounters: federation.TrafficCounters{Wasted: 9}},
	}}
	repo := &fakeRepo{}
	f := NewTrafficFlusher(node, repo)

	if n := f.Flush(context.Background()); n != 2 {
		t.Fatalf("first flush wrote %d hashes, want 2", n)
	}
	row, _ := repo.GetSwarmTraffic(context.Background(), "aa")
	if row == nil || row.Up != 100 || row.Down != 40 {
		t.Errorf("persisted aa = %+v, want up 100 down 40", row)
	}

	// Nothing new: no drain result, so no write at all — an empty flush must not
	// touch rows, or every idle tick would move the "last active" clock.
	before := repo.swarmFlushes
	if n := f.Flush(context.Background()); n != 0 {
		t.Errorf("second flush wrote %d, want 0", n)
	}
	if repo.swarmFlushes != before {
		t.Errorf("an empty flush still called AddSwarmTraffic (%d → %d)", before, repo.swarmFlushes)
	}
	if node.drained != 2 {
		t.Errorf("DrainTraffic called %d times, want 2 (once per flush)", node.drained)
	}
}

// A storage error must not panic or spin — the figure is a cumulative total
// whose value does not depend on any one interval.
func TestTrafficFlusher_SurvivesAStorageError(t *testing.T) {
	node := &fakeFederation{pending: []federation.TrafficDelta{
		{Hash: "aa", TrafficCounters: federation.TrafficCounters{Up: 1}},
	}}
	repo := &failingSwarmRepo{fakeRepo: &fakeRepo{}}
	f := NewTrafficFlusher(node, repo)
	if n := f.Flush(context.Background()); n != 0 {
		t.Errorf("flush reported %d written despite the error", n)
	}
}

// Run flushes once more on the way out, so a graceful shutdown is not a small
// data loss. The final flush must not ride the cancelled context.
func TestTrafficFlusher_FlushesOnShutdown(t *testing.T) {
	node := &fakeFederation{pending: []federation.TrafficDelta{
		{Hash: "aa", TrafficCounters: federation.TrafficCounters{Up: 500}},
	}}
	repo := &fakeRepo{}
	f := NewTrafficFlusher(node, repo)
	f.Interval = time.Hour // only the shutdown path may fire

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context ended")
	}

	row, _ := repo.GetSwarmTraffic(context.Background(), "aa")
	if row == nil || row.Up != 500 {
		t.Errorf("shutdown flush persisted %+v, want up 500", row)
	}
}

// No node, no flusher: a server with federation off moves nothing, and the
// caller's nil check is the whole of the gate.
func TestNewTrafficFlusher_NilWithoutANode(t *testing.T) {
	if f := NewTrafficFlusher(nil, &fakeRepo{}); f != nil {
		t.Error("a flusher was built without a node")
	}
	var f *TrafficFlusher
	if n := f.Flush(context.Background()); n != 0 {
		t.Error("a nil flusher should flush nothing rather than panic")
	}
	f.Run(context.Background()) // must return immediately
}

// failingSwarmRepo is a fakeRepo whose traffic writes always fail.
type failingSwarmRepo struct{ *fakeRepo }

func (r *failingSwarmRepo) AddSwarmTraffic(context.Context, []database.SwarmTrafficDelta,
	[]database.SwarmPeerTrafficDelta, int64) error {
	return errors.New("disk on fire")
}

// Both ledgers come off one drain and land in one call. Written as a test
// because the failure it guards against is silent: a flusher that persisted only
// the blob half would leave the peer panel permanently empty while every other
// figure on the page kept moving.
func TestTrafficFlusher_PersistsBothLedgersTogether(t *testing.T) {
	key := "abc123"
	node := &fakeFederation{
		pending: []federation.TrafficDelta{
			{Hash: "aa", TrafficCounters: federation.TrafficCounters{Up: 100}},
		},
		pendingPeers: []federation.PeerTrafficDelta{
			{Key: key, Up: 100},
			{Key: "", Up: 7}, // could not be placed: the bucket
		},
	}
	repo := &fakeRepo{}
	f := NewTrafficFlusher(node, repo)
	if n := f.Flush(context.Background()); n != 1 {
		t.Fatalf("flush wrote %d hashes, want 1", n)
	}
	if node.drainedPeers != 1 {
		t.Errorf("DrainPeerTraffic called %d times, want 1 per flush", node.drainedPeers)
	}

	peers, _ := repo.ListSwarmPeerTraffic(context.Background())
	if len(peers) != 2 {
		t.Fatalf("persisted %d counterparties, want 2 (one node + the bucket)", len(peers))
	}
	byKey := map[string]database.SwarmPeerTraffic{}
	for _, p := range peers {
		byKey[p.Key] = p
	}
	if byKey[key].Up != 100 {
		t.Errorf("node row = %+v, want 100 bytes", byKey[key])
	}
	// The empty key travels as the empty key: which requesters could not be
	// placed is the store's business, not the flusher's.
	if byKey[""].Up != 7 {
		t.Errorf("bucket row = %+v, want the 7 unplaced bytes", byKey[""])
	}

	// A drain that finds only peer bytes still writes — the return value counts
	// hashes, and reading it as "nothing happened" would drop the peer half.
	node.pendingPeers = []federation.PeerTrafficDelta{{Key: key, Up: 5}}
	if n := f.Flush(context.Background()); n != 0 {
		t.Fatalf("peer-only flush reported %d hashes", n)
	}
	peers, _ = repo.ListSwarmPeerTraffic(context.Background())
	for _, p := range peers {
		if p.Key == key && p.Up != 105 {
			t.Errorf("node row = %+v, want the peer-only flush added to 105", p)
		}
	}
}
