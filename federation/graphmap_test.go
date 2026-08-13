package federation

import (
	"fmt"
	"testing"
)

// k builds a distinguishable 64-hex key for a one-letter node label.
func k(label string) string {
	return fmt.Sprintf("%s%s", label, "0000000000000000000000000000000000000000000000000000000000000"[len(label)-1:])
}

// edge is a claim by `from` that it is friends with `to`.
func edge(from, to, name string) GraphEdgeClaim {
	return GraphEdgeClaim{Origin: k(from), Peer: k(to), Name: name}
}

func nodeByLabel(t *testing.T, m NetworkMap, label string) *MapNode {
	t.Helper()
	for i := range m.Nodes {
		if m.Nodes[i].Key == k(label) {
			return &m.Nodes[i]
		}
	}
	return nil
}

// A chain me—a—b—c: distances count hops, and everything past our friend is
// attributed to the branch it arrived through.
func TestNetworkMapDistanceAndAttribution(t *testing.T) {
	peers := []*ExternalNode{{PublicKey: k("a"), Label: "studio", TrustState: PeerFriend}}
	edges := []GraphEdgeClaim{
		edge("a", "b", "kian's node"),
		edge("b", "a", "studio"),
		edge("b", "c", "northwind"),
		edge("c", "b", "kian's node"),
	}
	m := BuildNetworkMap(k("me"), peers, edges, nil)

	for _, tc := range []struct {
		label string
		dist  int
		state string
	}{
		{"me", 0, MapSelf},
		{"a", 1, MapFriend},
		{"b", 2, ""},
		{"c", 3, ""},
	} {
		n := nodeByLabel(t, m, tc.label)
		if n == nil {
			t.Fatalf("node %s missing from the map", tc.label)
		}
		if n.Distance != tc.dist {
			t.Errorf("%s distance = %d, want %d", tc.label, n.Distance, tc.dist)
		}
		if n.State != tc.state {
			t.Errorf("%s state = %q, want %q", tc.label, n.State, tc.state)
		}
	}
	if m.Radius != 3 {
		t.Errorf("radius = %d, want 3", m.Radius)
	}

	// Everything behind our one friend is attributed to that branch.
	for _, label := range []string{"a", "b", "c"} {
		n := nodeByLabel(t, m, label)
		if len(n.Via) != 1 || n.Via[0] != k("a") {
			t.Errorf("%s via = %v, want [a]", label, n.Via)
		}
	}

	// A name we were told is marked as hearsay; one we chose is ours.
	if n := nodeByLabel(t, m, "a"); n.Name != "studio" || n.Named != "local" {
		t.Errorf("our friend's name = %q (%s), want our own label", n.Name, n.Named)
	}
	if n := nodeByLabel(t, m, "b"); n.Name != "kian's node" || n.Named != "heard" {
		t.Errorf("stranger's name = %q (%s), want heard", n.Name, n.Named)
	}
}

// Branch snipping: blocking a friend removes what was reachable only through
// it, and keeps what another friend also vouches for.
func TestNetworkMapSnipsBlockedBranch(t *testing.T) {
	peers := []*ExternalNode{
		{PublicKey: k("a"), Label: "studio", TrustState: PeerFriend},
		{PublicKey: k("d"), Label: "attic", TrustState: PeerFriend},
	}
	edges := []GraphEdgeClaim{
		edge("a", "b", ""), // b hangs off a only
		edge("a", "s", ""), // s is shared…
		edge("d", "s", ""), // …by both friends
		edge("b", "z", ""), // z hangs off b, so behind a too
	}

	full := BuildNetworkMap(k("me"), peers, edges, nil)
	for _, label := range []string{"b", "s", "z"} {
		if nodeByLabel(t, full, label) == nil {
			t.Fatalf("%s missing before the block", label)
		}
	}

	// Block a. Nothing is discovered through a blocked node any more.
	peers[0].TrustState = PeerBlocked
	snipped := BuildNetworkMap(k("me"), peers, edges, nil)

	if n := nodeByLabel(t, snipped, "a"); n == nil || n.State != MapBlocked {
		t.Fatal("the blocked node itself must stay visible, so it can be undone")
	}
	if nodeByLabel(t, snipped, "b") != nil {
		t.Error("b was reachable only through the blocked node and should be gone")
	}
	if nodeByLabel(t, snipped, "z") != nil {
		t.Error("z sat behind b, behind the blocked node, and should be gone")
	}
	s := nodeByLabel(t, snipped, "s")
	if s == nil {
		t.Fatal("s is also vouched for by attic and must survive the snip")
	}
	if len(s.Via) != 1 || s.Via[0] != k("d") {
		t.Errorf("s via = %v, want only the surviving branch [d]", s.Via)
	}
}

// One branch is one voice: a sybil farm behind a single friendship counts once,
// however many keys it mints.
func TestNetworkMapWeightsMarksByBranch(t *testing.T) {
	peers := []*ExternalNode{
		{PublicKey: k("a"), Label: "studio", TrustState: PeerFriend},
		{PublicKey: k("d"), Label: "attic", TrustState: PeerFriend},
	}
	edges := []GraphEdgeClaim{
		edge("a", "1", ""), // three sybils, all behind friend a
		edge("a", "2", ""),
		edge("a", "3", ""),
		edge("d", "9", ""), // one node behind the other friend
	}
	target := k("t")
	marks := []StoredMark{
		{Origin: k("1"), Target: target, Reason: "spam"},
		{Origin: k("2"), Target: target, Reason: "spam"},
		{Origin: k("3"), Target: target, Reason: "spam"},
	}
	// The target has to be on the map to carry marks.
	edges = append(edges, edge("9", "t", "accused"))

	m := BuildNetworkMap(k("me"), peers, edges, marks)
	n := nodeByLabel(t, m, "t")
	if n == nil {
		t.Fatal("the marked node is not on the map")
	}
	if len(n.Marks) != 3 {
		t.Fatalf("marks = %d, want all 3 kept for display", len(n.Marks))
	}
	if n.MarkBranches != 1 {
		t.Errorf("mark branches = %d, want 1 — a farm behind one friendship is one voice", n.MarkBranches)
	}

	// A second, independent accuser makes it genuinely two voices.
	marks = append(marks, StoredMark{Origin: k("9"), Target: target, Reason: "same"})
	m = BuildNetworkMap(k("me"), peers, edges, marks)
	if n := nodeByLabel(t, m, "t"); n.MarkBranches != 2 {
		t.Errorf("mark branches = %d, want 2 after an independent branch agrees", n.MarkBranches)
	}
}

// An accusation from a node we cannot place is not evidence we can weigh, so it
// is left off rather than shown without provenance.
func TestNetworkMapDropsUnreachableAccusers(t *testing.T) {
	peers := []*ExternalNode{{PublicKey: k("a"), TrustState: PeerFriend}}
	edges := []GraphEdgeClaim{edge("a", "b", "")}
	marks := []StoredMark{{Origin: k("x"), Target: k("b"), Reason: "from nowhere"}}

	m := BuildNetworkMap(k("me"), peers, edges, marks)
	if n := nodeByLabel(t, m, "b"); len(n.Marks) != 0 {
		t.Errorf("marks = %+v, want none from an unplaceable accuser", n.Marks)
	}
}

// A friendship we just made is a fact we hold directly, so it is drawn before
// any record carries it — and drawn solid, because our own edges are not claims
// to be weighed for mutuality (§Forgetting).
func TestNetworkMapIncludesOwnFriendshipsWithoutRecords(t *testing.T) {
	peers := []*ExternalNode{{PublicKey: k("a"), Label: "fresh", TrustState: PeerFriend}}
	m := BuildNetworkMap(k("me"), peers, nil, nil)

	if n := nodeByLabel(t, m, "a"); n == nil || n.Distance != 1 {
		t.Fatalf("a brand-new friend is missing from the map: %+v", m.Nodes)
	}
	if len(m.Edges) != 1 || !m.Edges[0].Mutual {
		t.Errorf("edges = %+v, want one edge drawn as a fact of ours", m.Edges)
	}
}

// An edge both ends published is stronger evidence than one only one end
// claims, and the map distinguishes them.
func TestNetworkMapMarksMutualEdges(t *testing.T) {
	peers := []*ExternalNode{{PublicKey: k("a"), TrustState: PeerFriend}}
	edges := []GraphEdgeClaim{
		edge("a", "b", ""),
		edge("b", "a", ""), // both directions claimed
		edge("b", "c", ""), // only b says so
	}
	m := BuildNetworkMap(k("me"), peers, edges, nil)

	for _, e := range m.Edges {
		switch {
		case e.From == k("a") && e.To == k("b"), e.From == k("b") && e.To == k("a"):
			if !e.Mutual {
				t.Error("a—b was claimed by both ends and should be mutual")
			}
		case e.From == k("b") && e.To == k("c"), e.From == k("c") && e.To == k("b"):
			if e.Mutual {
				t.Error("b—c was claimed by one end only")
			}
		}
	}
}

// A node we blocked that the graph does not mention still belongs on the map:
// an admin must be able to see and undo a block where they made it, not only in
// the peer list. The same goes for a pairing still awaiting an answer.
func TestNetworkMapKeepsUnlinkedPeers(t *testing.T) {
	peers := []*ExternalNode{
		{PublicKey: k("b"), Label: "spammer", TrustState: PeerBlocked},
		{PublicKey: k("p"), Label: "waiting", TrustState: PeerPendingOutgoing},
	}
	m := BuildNetworkMap(k("me"), peers, nil, nil)

	blocked := nodeByLabel(t, m, "b")
	if blocked == nil || blocked.State != MapBlocked {
		t.Fatalf("a blocked key with no graph edges fell off the map: %+v", m.Nodes)
	}
	if blocked.Distance != 1 {
		t.Errorf("blocked distance = %d, want 1 (a direct relationship of ours)", blocked.Distance)
	}
	if pending := nodeByLabel(t, m, "p"); pending == nil || pending.State != MapPending {
		t.Errorf("pending peer missing from the map: %+v", m.Nodes)
	}
	if len(m.Edges) != 0 {
		t.Errorf("edges = %+v, want none — nobody vouches for either", m.Edges)
	}
}
