package database

import (
	"context"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/federation"
)

// Per-counterparty traffic (docs/architecture/swarm-admin.md §Migration 042):
// what a node has cost us, all time — the companion to the member quotas, which
// bound only what one may cost us.

// nodeKey is a stand-in public key: 64 hex characters, the shape the store
// normalizes and the panel abbreviates.
func nodeKey(c byte) string { return strings.Repeat(string(c), 64) }

func TestSwarmPeerTraffic_WritesLandBesideTheBlobLedger(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	friend := nodeKey('a')

	// One drain, both halves — the flusher's actual call shape.
	if err := db.AddSwarmTraffic(ctx,
		[]SwarmTrafficDelta{{Hash: "aa11", Up: 100}},
		[]SwarmPeerTrafficDelta{{Key: friend, Up: 100}}, 1000); err != nil {
		t.Fatal(err)
	}
	if err := db.AddSwarmTraffic(ctx,
		[]SwarmTrafficDelta{{Hash: "aa11", Up: 40, Down: 7}},
		[]SwarmPeerTrafficDelta{{Key: friend, Up: 40, Down: 7}}, 2000); err != nil {
		t.Fatal(err)
	}

	peers, err := db.ListSwarmPeerTraffic(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 {
		t.Fatalf("got %d rows, want 1", len(peers))
	}
	// Increments, never assignments — the same contract the blob table has.
	if peers[0].Up != 140 || peers[0].Down != 7 {
		t.Errorf("peer = ▲%d ▼%d, want ▲140 ▼7", peers[0].Up, peers[0].Down)
	}
	if peers[0].FirstAt != 1000 || peers[0].LastAt != 2000 {
		t.Errorf("clocks = %d..%d, want 1000..2000", peers[0].FirstAt, peers[0].LastAt)
	}
	// The two ledgers count the same bytes, and the flush that wrote one wrote
	// the other. Drift between them means a commit went missing.
	total, err := db.SwarmTrafficTotals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total.Up != peers[0].Up || total.Down != peers[0].Down {
		t.Errorf("blob ledger ▲%d ▼%d vs peer ledger ▲%d ▼%d — they count the same bytes",
			total.Up, total.Down, peers[0].Up, peers[0].Down)
	}
}

// Everything we could not place shares ONE row. A row per mesh address would be
// a table sized by whoever chooses to talk to us — N forged keys, N rows.
func TestSwarmPeerTraffic_UnplacedShareOneBucket(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	if err := db.AddSwarmTraffic(ctx, nil, []SwarmPeerTrafficDelta{
		{Key: "", Up: 5}, {Key: "", Up: 7}, {Key: nodeKey('b'), Up: 1},
	}, 100); err != nil {
		t.Fatal(err)
	}

	peers, err := db.ListSwarmPeerTraffic(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 2 {
		t.Fatalf("got %d rows, want 2 (one node + one bucket)", len(peers))
	}
	// The bucket sorts LAST whatever its size: it is a summary of strangers, and
	// reading it as the busiest node would be wrong.
	last := peers[len(peers)-1]
	if last.Key != "" || last.Kind != "unplaced" {
		t.Fatalf("last row = %+v, want the unplaced bucket", last)
	}
	if last.Up != 12 {
		t.Errorf("bucket = %d bytes, want the 12 both requesters moved", last.Up)
	}
	if last.Placed() {
		t.Error("the bucket reports itself as a placed node")
	}
}

// The name and the class are joined at read time, never stored: a heard name is
// a claim that changes, and a node can leave our community without its history
// ceasing to be true.
func TestSwarmPeerTraffic_IdentityIsResolvedNotStored(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	friend, member, gone := nodeKey('a'), nodeKey('b'), nodeKey('c')

	if _, err := db.InsertFederationPeer(ctx, &federation.ExternalNode{
		PublicKey: friend, Label: "fiona's box", HeardName: "some node",
		TrustState: federation.PeerFriend, TrustedAt: 1}); err != nil {
		t.Fatal(err)
	}
	src, err := db.EnsureCatalogSource(ctx, member, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.TouchCatalogSourceSeen(ctx, src.ID, 2, "a node we pull from"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddSwarmTraffic(ctx, nil, []SwarmPeerTrafficDelta{
		{Key: friend, Up: 30}, {Key: member, Up: 20}, {Key: gone, Up: 10},
	}, 100); err != nil {
		t.Fatal(err)
	}

	byKey := map[string]SwarmPeerTraffic{}
	peers, err := db.ListSwarmPeerTraffic(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range peers {
		byKey[p.Key] = p
	}
	// The admin's own label beats the node's claim about itself.
	if got := byKey[friend]; got.Name != "fiona's box" || got.Kind != "friend" {
		t.Errorf("friend = %q/%q, want the local name and 'friend'", got.Name, got.Kind)
	}
	// A node only the discovery sweep knows is a member, named by its own claim.
	if got := byKey[member]; got.Name != "a node we pull from" || got.Kind != "member" {
		t.Errorf("member = %q/%q, want the heard name and 'member'", got.Name, got.Kind)
	}
	// Nothing knows this key any more — and it keeps every byte.
	if got := byKey[gone]; got.Kind != "gone" || got.Up != 10 || got.Name != "" {
		t.Errorf("unknown key = %+v, want kind 'gone' with its bytes intact", got)
	}

	// Busiest first among the named nodes.
	if peers[0].Key != friend {
		t.Errorf("first row = %q, want the busiest (%q)", peers[0].Key, friend)
	}
}

// A blocked peer says so rather than reading as a plain friend: the class is
// where that node stands NOW, which is the question an admin reading a byte
// count is usually about to ask.
func TestSwarmPeerTraffic_BlockedNodeKeepsItsHistoryAndSaysSo(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	key := nodeKey('d')
	id, err := db.InsertFederationPeer(ctx, &federation.ExternalNode{
		PublicKey: key, Label: "noisy", TrustState: federation.PeerFriend, TrustedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AddSwarmTraffic(ctx, nil,
		[]SwarmPeerTrafficDelta{{Key: key, Up: 900}}, 100); err != nil {
		t.Fatal(err)
	}
	if err := db.BlockFederationPeer(ctx, id, federation.PeerFriend, "too much", 2); err != nil {
		t.Fatal(err)
	}

	peers, err := db.ListSwarmPeerTraffic(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].Kind != "blocked" || peers[0].Up != 900 {
		t.Errorf("blocked peer = %+v, want its 900 bytes and kind 'blocked'", peers)
	}
}

// ResolveSwarmPeers names keys with no stored row yet — a counterparty this
// process has traded with since the last flush. Without it the panel would list
// nobody while the summary strip says two nodes are pulling.
func TestSwarmPeerTraffic_ResolveNamesUnflushedKeys(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	friend, stranger := nodeKey('a'), nodeKey('e')
	if _, err := db.InsertFederationPeer(ctx, &federation.ExternalNode{
		PublicKey: friend, Label: "fiona's box", TrustState: federation.PeerFriend, TrustedAt: 1}); err != nil {
		t.Fatal(err)
	}

	got, err := db.ResolveSwarmPeers(ctx, []string{friend, stranger})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want one per key asked for", len(got))
	}
	if got[0].Key != friend || got[0].Name != "fiona's box" || got[0].Kind != "friend" {
		t.Errorf("resolved[0] = %+v", got[0])
	}
	if got[1].Kind != "gone" || got[1].Name != "" {
		t.Errorf("resolved[1] = %+v, want an unplaceable key answered rather than dropped", got[1])
	}
	// Counters stay zero: this resolves identity, it does not invent history.
	if got[0].Up != 0 || got[0].Down != 0 {
		t.Errorf("resolution carried bytes: %+v", got[0])
	}
	if none, err := db.ResolveSwarmPeers(ctx, nil); err != nil || none != nil {
		t.Errorf("ResolveSwarmPeers(nil) = %v, %v", none, err)
	}
}

// Forgetting a counterparty does not rewrite any blob's history, and forgetting
// a blob's does not debit its counterparties. Two ledgers of the same bytes,
// neither derived from the other.
func TestSwarmPeerTraffic_ForgetIsIndependentOfTheBlobLedger(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	key := nodeKey('a')
	if err := db.AddSwarmTraffic(ctx,
		[]SwarmTrafficDelta{{Hash: "aa11", Up: 100}},
		[]SwarmPeerTrafficDelta{{Key: key, Up: 100}, {Key: "", Up: 5}}, 100); err != nil {
		t.Fatal(err)
	}

	n, err := db.ForgetSwarmPeerTraffic(ctx, []string{key})
	if err != nil || n != 1 {
		t.Fatalf("forget = %d, %v", n, err)
	}
	if row, _ := db.GetSwarmTraffic(ctx, "aa11"); row == nil || row.Up != 100 {
		t.Error("forgetting a peer changed what the blob had moved")
	}
	// The bucket survives being unnamed: it was not asked for.
	peers, _ := db.ListSwarmPeerTraffic(ctx)
	if len(peers) != 1 || peers[0].Key != "" {
		t.Errorf("after forget: %+v, want only the bucket", peers)
	}

	// And the reverse direction.
	if _, err := db.ForgetSwarmTraffic(ctx, []string{"aa11"}); err != nil {
		t.Fatal(err)
	}
	if peers, _ := db.ListSwarmPeerTraffic(ctx); len(peers) != 1 {
		t.Error("forgetting a blob's traffic deleted a counterparty's history")
	}

	all, err := db.ForgetAllSwarmPeerTraffic(ctx)
	if err != nil || all != 1 {
		t.Fatalf("forget all = %d, %v", all, err)
	}
	if peers, _ := db.ListSwarmPeerTraffic(ctx); len(peers) != 0 {
		t.Errorf("forget all left %+v", peers)
	}
	if n, err := db.ForgetSwarmPeerTraffic(ctx, nil); err != nil || n != 0 {
		t.Errorf("ForgetSwarmPeerTraffic(nil) = %d, %v — an empty set is not everything", n, err)
	}
}

// A quiet counterparty's clock must not move: a node that pulled nothing in this
// interval has not been active, and the panel sorts and reads by that stamp.
func TestSwarmPeerTraffic_ZeroDeltaLeavesTheClockAlone(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	key := nodeKey('a')
	if err := db.AddSwarmTraffic(ctx, nil,
		[]SwarmPeerTrafficDelta{{Key: key, Up: 10}}, 1000); err != nil {
		t.Fatal(err)
	}
	if err := db.AddSwarmTraffic(ctx, nil,
		[]SwarmPeerTrafficDelta{{Key: key}}, 9999); err != nil {
		t.Fatal(err)
	}
	peers, _ := db.ListSwarmPeerTraffic(ctx)
	if len(peers) != 1 || peers[0].LastAt != 1000 {
		t.Errorf("last_at = %+v, want the row untouched at 1000", peers)
	}
}
