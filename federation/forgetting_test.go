package federation

import "testing"

// Forgetting (docs/architecture/federation-trust.md §Forgetting). Three properties,
// all of them consequences of one walk: an edge with our own key comes from our
// peer rows and nowhere else, reachability decides retention and not merely
// drawing, and removal therefore needs no machinery of its own.

// A node whose admin never removed us keeps publishing "friends with you". That
// is not dishonest — it is what they hold — but it is a claim about a
// relationship we are in a position to know the truth of, and we do not draw it.
func TestOwnEdgesIgnoreTheirClaims(t *testing.T) {
	// We removed a: no peer row at all. They still publish the friendship.
	edges := []GraphEdgeClaim{edge("a", "me", "our node")}
	m := BuildNetworkMap(k("me"), nil, edges, nil)

	if len(m.Edges) != 0 {
		t.Errorf("edges = %+v, want none: a friendship we ended is not theirs to publish", m.Edges)
	}
	if n := nodeByLabel(t, m, "a"); n != nil {
		t.Errorf("a is on the map at distance %d, want gone with the edge we cut", n.Distance)
	}
}

// The same hole seen from the other side, and the reason this is an integrity
// fix rather than only a staleness one: a node already in our view could put
// ITSELF on our inner ring by claiming a friendship that never existed.
func TestStrangerCannotClaimItselfOntoOurRing(t *testing.T) {
	peers := []*Peer{{PublicKey: k("a"), State: PeerFriend}}
	edges := []GraphEdgeClaim{
		edge("a", "b", ""),    // a vouches for b — b is admissible
		edge("b", "me", "hi"), // b claims to be OUR friend
	}
	m := BuildNetworkMap(k("me"), peers, edges, nil)

	n := nodeByLabel(t, m, "b")
	if n == nil {
		t.Fatal("b should still be on the map — a vouched for it")
	}
	if n.Distance != 2 {
		t.Errorf("b sits at distance %d, want 2: its own claim must not promote it", n.Distance)
	}
}

// Other nodes' edges stay single-claim. The asymmetry is deliberate: an edge
// somebody claims is worth seeing, but our own edges are not claims at all.
func TestOthersEdgesRemainSingleClaim(t *testing.T) {
	peers := []*Peer{{PublicKey: k("a"), State: PeerFriend}}
	m := BuildNetworkMap(k("me"), peers, []GraphEdgeClaim{edge("a", "b", "")}, nil)

	if n := nodeByLabel(t, m, "b"); n == nil || n.Distance != 2 {
		t.Fatalf("b is missing: one node's claim about a third party still draws an edge (%+v)", m.Nodes)
	}
}

// ReachableKeys is what the sweep keeps. Removing a friend severs the edge,
// which makes the branch behind them unreachable — the whole of part 3.
func TestReachableKeysDropARemovedFriendsBranch(t *testing.T) {
	edges := []GraphEdgeClaim{
		edge("a", "b", ""), // a introduced b
		edge("b", "c", ""), // and b introduced c
		edge("a", "me", ""),
	}

	friends := []*Peer{{PublicKey: k("a"), State: PeerFriend}}
	before := ReachableKeys(k("me"), friends, edges)
	for _, label := range []string{"a", "b", "c"} {
		if _, ok := before[k(label)]; !ok {
			t.Fatalf("%s should be reachable while a is our friend", label)
		}
	}

	// The admin removes a. No peer row, and their claim about us is not ours.
	after := ReachableKeys(k("me"), nil, edges)
	for _, label := range []string{"a", "b", "c"} {
		if _, ok := after[k(label)]; ok {
			t.Errorf("%s survived the removal of the only friend it was seen through", label)
		}
	}
	if _, ok := after[k("me")]; !ok {
		t.Error("our own key must always be kept — a caller deleting the complement would empty the store")
	}
}

// "Unless we have another connection to them": a node a second friend also
// vouches for survives losing the first. Unchanged behaviour, now governing
// retention rather than only drawing.
func TestReachableKeysKeepWhatASecondFriendVouchesFor(t *testing.T) {
	edges := []GraphEdgeClaim{
		edge("a", "b", ""), // both our friends know b
		edge("d", "b", ""),
	}
	// a is blocked, so nothing is discovered through it; d still vouches for b.
	peers := []*Peer{
		{PublicKey: k("a"), State: PeerBlocked},
		{PublicKey: k("d"), State: PeerFriend},
	}
	if _, ok := ReachableKeys(k("me"), peers, edges)[k("b")]; !ok {
		t.Error("b is vouched for by d too and must survive a being blocked")
	}

	// With d gone as well, b has no path to us left and is collected.
	only := []*Peer{{PublicKey: k("a"), State: PeerBlocked}}
	if _, ok := ReachableKeys(k("me"), only, edges)[k("b")]; ok {
		t.Error("b was reachable only behind a blocked node and should be collected")
	}
}

// A blocked node stays: an admin who cannot see a block cannot lift it. What
// goes is everything discovered only THROUGH it.
func TestReachableKeysKeepBlockedPeersButNotTheirBranch(t *testing.T) {
	peers := []*Peer{{PublicKey: k("a"), State: PeerBlocked}}
	edges := []GraphEdgeClaim{edge("a", "b", "")}
	keep := ReachableKeys(k("me"), peers, edges)

	if _, ok := keep[k("a")]; !ok {
		t.Error("a blocked peer's own record must stay — it is a direct relationship of ours")
	}
	if _, ok := keep[k("b")]; ok {
		t.Error("b was reachable only through a blocked node and should be collected")
	}
}

// A pending pairing has no gossiped edge yet. It is still a relationship of
// ours, so its record is not collected out from under the admin mid-handshake.
func TestReachableKeysKeepPendingPeers(t *testing.T) {
	peers := []*Peer{{PublicKey: k("a"), State: PeerPendingOutgoing}}
	if _, ok := ReachableKeys(k("me"), peers, nil)[k("a")]; !ok {
		t.Error("a pending peer's record should be kept while the pairing is in flight")
	}
}
