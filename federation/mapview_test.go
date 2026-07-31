package federation

import (
	"strings"
	"testing"
)

// sampleMap is a small community with a shape worth asking questions about: two
// direct friends, a stranger reachable through both of them, a chain running out
// to four hops, and a blocked node the gossip places far away.
//
//	self ── alpha ── gamma ── delta ── epsilon
//	  └──── beta ─────┘
//	  (blocked: rogue, at distance 5)
//
// Keys and addresses are hex and names are words, as they are in the real thing
// — a fixture where the address spells the node's name would let an address
// match stand in for a name match and prove nothing.
func sampleMap() NetworkMap {
	n := func(key, addr, name, state string, distance int, via ...string) MapNode {
		return MapNode{Key: key, Name: name, State: state, Distance: distance, Via: via,
			Address: addr}
	}
	return NetworkMap{
		Radius: 5,
		Nodes: []MapNode{
			n("0a11", "200:1000::1", "us", MapSelf, 0),
			n("1b22", "200:1000::2", "Alpha", MapFriend, 1, "1b22"),
			n("2c33", "200:1000::3", "Beta", MapFriend, 1, "2c33"),
			n("3d44", "200:1000::4", "Gamma", "", 2, "1b22", "2c33"),
			n("4e55", "200:1000::5", "Delta", "", 3, "1b22", "2c33"),
			n("5f66", "200:1000::6", "Epsilon", "", 4, "1b22", "2c33"),
			n("6077", "200:1000::7", "Rogue", MapBlocked, 5, "1b22"),
		},
		Edges: []MapEdge{
			{From: "0a11", To: "1b22", Mutual: true},
			{From: "0a11", To: "2c33", Mutual: true},
			{From: "1b22", To: "3d44", Mutual: true},
			{From: "2c33", To: "3d44", Mutual: true},
			{From: "3d44", To: "4e55", Mutual: true},
			{From: "4e55", To: "5f66", Mutual: true},
			{From: "5f66", To: "6077", Mutual: true},
		},
	}
}

func keysOf(nodes []MapNode) string {
	out := make([]string, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, n.Key)
	}
	return strings.Join(out, ",")
}

func TestTrimMapKeepsTheNeighbourhoodAndOurOwnDecisions(t *testing.T) {
	m := TrimMap(sampleMap(), 2)

	// Within the radius, plus the blocked node at distance 5 — a relationship of
	// ours is never a rendering casualty.
	if got := keysOf(m.Nodes); got != "0a11,1b22,2c33,3d44,6077" {
		t.Errorf("trimmed nodes = %s", got)
	}
	// Edges survive only between kept nodes: the line to delta would have gone
	// nowhere, and so would the one into the blocked node.
	for _, e := range m.Edges {
		if e.To == "4e55" || e.From == "4e55" || e.To == "6077" || e.From == "6077" {
			t.Errorf("edge to a node not on the map: %+v", e)
		}
	}
	if m.Shown != 5 || m.Hidden != 2 {
		t.Errorf("shown/hidden = %d/%d, want 5/2", m.Shown, m.Hidden)
	}
	// Radius still describes the WHOLE component — it is what tells an admin
	// there is more out there.
	if m.Radius != 5 {
		t.Errorf("radius = %d, want the component's 5", m.Radius)
	}

	// radius 0 is the whole thing.
	if all := TrimMap(sampleMap(), 0); len(all.Nodes) != 7 || all.Hidden != 0 {
		t.Errorf("untrimmed = %d nodes, hidden %d", len(all.Nodes), all.Hidden)
	}
}

func TestFindNodesReachesPastTheViewRadius(t *testing.T) {
	full := sampleMap()

	// The point of the search: epsilon sits at four hops and would not be drawn
	// at the default radius, and is still findable by name.
	hits := FindNodes(full, "epsilon", 10)
	if len(hits) != 1 || hits[0].Key != "5f66" || hits[0].Matched != "name" {
		t.Fatalf("name search = %+v", hits)
	}

	// A key prefix — what someone pasting the front of a key expects.
	if hits := FindNodes(full, "3d4", 10); len(hits) != 1 || hits[0].Matched != "key" {
		t.Errorf("key-prefix search = %+v", hits)
	}
	// A mesh address is a fact about the key, so it is searchable too.
	if hits := FindNodes(full, "200:1000::3", 10); len(hits) != 1 || hits[0].Matched != "address" {
		t.Errorf("address search = %+v", hits)
	}
	// An exact key outranks a name that merely contains the query.
	mixed := sampleMap()
	mixed.Nodes = append(mixed.Nodes, MapNode{Key: "99aa", Name: "a node called 1b22 by its admin", Distance: 2})
	if hits := FindNodes(mixed, "1b22", 10); hits[0].Key != "1b22" {
		t.Errorf("ranking put %q first, want the exact key", hits[0].Key)
	}
	// Empty query finds nothing rather than everything.
	if hits := FindNodes(full, "  ", 10); len(hits) != 0 {
		t.Errorf("empty query returned %d hits", len(hits))
	}
	if hits := FindNodes(full, "200:1000", 2); len(hits) > 2 {
		t.Errorf("limit ignored: %d hits", len(hits))
	}
}

func TestBranchNodesAnswersWhatOneBlockWouldTake(t *testing.T) {
	full := sampleMap()

	branch := BranchNodes(full, "1b22")
	if got := keysOf(branch); got != "1b22,3d44,4e55,5f66,6077" {
		t.Errorf("alpha's branch = %s (the friend itself is its own root)", got)
	}

	// Gamma reached us through BOTH friends, so it is in both branches — which
	// is exactly why blocking one of them may not remove it, and the admin has
	// to be able to see that before acting.
	beta := BranchNodes(full, "2c33")
	var inBoth bool
	for _, n := range beta {
		if n.Key == "3d44" {
			inBoth = true
		}
	}
	if !inBoth {
		t.Error("a node reachable through two friends is missing from the second branch")
	}
}

func TestPathsShowsEveryWayTwoNodesAreJoined(t *testing.T) {
	full := sampleMap()

	paths := Paths(full, "0a11", "4e55")
	if len(paths) != 2 {
		t.Fatalf("paths self→delta = %v, want both routes", paths)
	}
	// Shortest first, and both run through gamma — which is the actual answer:
	// one block on gamma cuts delta off, one on either friend does not.
	for _, p := range paths {
		if p[0] != "0a11" || p[len(p)-1] != "4e55" {
			t.Errorf("path does not join the endpoints: %v", p)
		}
		if len(p) != 4 {
			t.Errorf("unexpected path length: %v", p)
		}
	}

	// No self-paths, no paths to nowhere.
	if got := Paths(full, "0a11", "0a11"); len(got) != 0 {
		t.Errorf("self→self = %v", got)
	}
	if got := Paths(full, "0a11", "nosuchnode"); len(got) != 0 {
		t.Errorf("path to an unknown key = %v", got)
	}

	// A long chain is found, and the result is deterministic across runs.
	long := Paths(full, "0a11", "5f66")
	if len(long) != 2 {
		t.Fatalf("paths self→epsilon = %v", long)
	}
	again := Paths(full, "0a11", "5f66")
	if keysJoin(long) != keysJoin(again) {
		t.Error("path order is not deterministic")
	}
}

// TestPathsStaysBounded: a graph with cycles has exponentially many simple
// paths, so the search has to stop — and it has to stop by dropping the LONGEST
// ones, since a truncated list missing the direct connection would mislead.
func TestPathsStaysBounded(t *testing.T) {
	// A dense blob: self plus eight nodes, all joined to each other.
	m := NetworkMap{Nodes: []MapNode{{Key: "self"}}}
	keys := []string{"self"}
	for _, k := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		m.Nodes = append(m.Nodes, MapNode{Key: k})
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			m.Edges = append(m.Edges, MapEdge{From: keys[i], To: keys[j], Mutual: true})
		}
	}

	paths := Paths(m, "self", "h")
	if len(paths) != MaxPathResults {
		t.Fatalf("dense graph produced %d paths, want the cap %d", len(paths), MaxPathResults)
	}
	// The direct edge is the first answer, because the search is breadth-first.
	if len(paths[0]) != 2 {
		t.Errorf("first path = %v, want the direct connection", paths[0])
	}
	for _, p := range paths {
		if len(p) > MaxPathLength+1 {
			t.Errorf("path longer than the bound: %v", p)
		}
	}
}

func keysJoin(paths [][]string) string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, strings.Join(p, ">"))
	}
	return strings.Join(out, "|")
}
