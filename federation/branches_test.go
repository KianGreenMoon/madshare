//go:build !nofederation

package federation

import (
	"reflect"
	"testing"
)

// TestBranchesMatchTheMap is the invariant that makes the browse's weighting
// explainable: the attribution the madnetwork page counts voices by is the same
// one the admin's network map draws. A holder in the ⓘ panel links straight to
// its node on that map, so if the two walks could disagree, an admin following
// the link would be shown a different graph than the one that ranked the row.
func TestBranchesMatchTheMap(t *testing.T) {
	// me—a—b—c, plus a second friend d that also reaches b: b is corroborated
	// down two branches, c only through a.
	peers := []*ExternalNode{
		{PublicKey: k("a"), Label: "studio", TrustState: PeerFriend},
		{PublicKey: k("d"), Label: "loft", TrustState: PeerFriend},
	}
	edges := []GraphEdgeClaim{
		edge("a", "b", "kian's node"), edge("b", "a", "studio"),
		edge("b", "c", "northwind"), edge("c", "b", "kian's node"),
		edge("d", "b", "kian's node"), edge("b", "d", "loft"),
		edge("a", "e", "attic"), edge("e", "a", "studio"),
	}

	branches := branchesOf(k("me"), peers, edges)
	m := BuildNetworkMap(k("me"), peers, edges, nil)
	for i := range m.Nodes {
		n := &m.Nodes[i]
		if len(n.Via) == 0 {
			if _, ok := branches[n.Key]; ok {
				t.Errorf("%s: branch map attributes a node the map places on no branch", n.Key)
			}
			continue
		}
		if !reflect.DeepEqual(branches[n.Key], n.Via) {
			t.Errorf("%s: branches = %v, map via = %v", n.Key, branches[n.Key], n.Via)
		}
	}

	// And the facts the weighting actually depends on, stated directly rather
	// than only via the map: a node two friends reach is two voices, and a node
	// hanging off one friend is one however deep the chain behind it runs.
	if got := branches[k("b")]; len(got) != 2 {
		t.Errorf("b via = %v, want both friends", got)
	}
	if got := branches[k("e")]; len(got) != 1 || got[0] != k("a") {
		t.Errorf("e via = %v, want [a] only", got)
	}
}

// TestHopsMatchTheMap pins the other half of the same walk. /madnetwork orders
// every node list by hops, and a holder's ⓘ panel links to that node on the
// admin map — so "2 hops" on the browse and the ring the map draws it in have to
// be the same number, produced by the same BFS.
func TestHopsMatchTheMap(t *testing.T) {
	peers := []*ExternalNode{
		{PublicKey: k("a"), Label: "studio", TrustState: PeerFriend},
		{PublicKey: k("d"), Label: "loft", TrustState: PeerFriend},
	}
	edges := []GraphEdgeClaim{
		edge("a", "b", "kian's node"), edge("b", "a", "studio"),
		edge("b", "c", "northwind"), edge("c", "b", "kian's node"),
		edge("d", "b", "kian's node"), edge("b", "d", "loft"),
	}

	_, hops := graphOf(k("me"), peers, edges)
	m := BuildNetworkMap(k("me"), peers, edges, nil)
	for i := range m.Nodes {
		n := &m.Nodes[i]
		if got, ok := hops[n.Key]; !ok || got != n.Distance {
			t.Errorf("%s: hops = %d (present %v), map distance = %d", n.Key, got, ok, n.Distance)
		}
	}

	// The facts the ordering depends on, stated directly: we are 0, a friend is
	// 1, and a node reached through two friends takes the SHORTER route — a hop
	// count that grew with the number of ways to reach a node would sort the
	// best-connected members last.
	if hops[k("me")] != 0 || hops[k("a")] != 1 || hops[k("b")] != 2 || hops[k("c")] != 3 {
		t.Errorf("hops = %v, want me:0 a:1 b:2 c:3", hops)
	}
	// A node nothing places is ABSENT, not 0 — zero is us, and a stranger
	// silently promoted to the top of the list is the one wrong answer here.
	if _, ok := hops[k("stranger")]; ok {
		t.Errorf("an unreachable node was given a distance: %v", hops)
	}
}

// TestBranchesSnipWithTheBlock: blocking a node drops the branch behind it, and
// the weighting must lose those voices at the same moment the map does —
// otherwise a blocked node keeps corroborating for as long as its records live.
func TestBranchesSnipWithTheBlock(t *testing.T) {
	peers := []*ExternalNode{{PublicKey: k("a"), TrustState: PeerFriend}}
	edges := []GraphEdgeClaim{
		edge("a", "b", "b"), edge("b", "a", "a"),
		edge("b", "c", "c"), edge("c", "b", "b"),
	}
	if got := branchesOf(k("me"), peers, edges); len(got[k("c")]) != 1 {
		t.Fatalf("c via = %v, want one branch before the block", got[k("c")])
	}

	peers = append(peers, &ExternalNode{PublicKey: k("b"), TrustState: PeerBlocked})
	got := branchesOf(k("me"), peers, edges)
	if len(got[k("c")]) != 0 {
		t.Errorf("c via = %v after blocking b, want nothing — the branch is snipped", got[k("c")])
	}
}
