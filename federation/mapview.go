package federation

import (
	"sort"
	"strings"
)

// Navigating the map at scale (F7 item 7, docs/architecture/federation.md §The
// network map). The community is unbounded, so the map scales by SHOWING LESS AT
// A TIME rather than by the network being smaller: a view radius, a search that
// still reaches everything outside it, and the paths between any two nodes.
//
// Everything here is a pure function over an already-built NetworkMap, for the
// same reason BuildNetworkMap is: the rules are the feature, and they should be
// testable without a mesh.
//
// **The radius is a rendering setting and nothing else.** It never limits who is
// served, never appears in a scope, never reaches the library. Said here as well
// as in the design because a radius that leaks into access is exactly the ladder
// this design threw away: this number is about what an admin LOOKS AT,
// share_depth is about whom we ANSWER, and the two must never meet.

// DefaultMapRadius is how far the map opens on: our friends, their friends, and
// one ring past that. Far enough to show the shape of a neighbourhood, close
// enough that the whole component is not the default cost.
const DefaultMapRadius = 3

// MaxPathResults / MaxPathLength bound the all-paths search. A friendship graph
// has cycles, so "every simple path" is exponential in the worst case and has to
// be bounded by something; these are bounded by what a person can read, which is
// the smaller number and therefore the right one.
const (
	MaxPathResults = 12
	MaxPathLength  = 8
)

// TrimMap returns the map as rendered at a view radius: nodes within radius hops
// of us, and the edges between them. radius <= 0 keeps everything.
//
// Nodes we hold a peer row for are ALWAYS kept, however far the graph places
// them. A pending pairing or a node this admin blocked is a direct relationship
// of ours; dropping one off the map because the gossip has not placed it nearby
// would hide the admin's own decisions behind a rendering setting.
func TrimMap(m NetworkMap, radius int) NetworkMap {
	if radius <= 0 {
		out := m
		out.Shown = len(m.Nodes)
		return out
	}
	keep := make(map[string]bool, len(m.Nodes))
	out := NetworkMap{Radius: m.Radius}
	for _, n := range m.Nodes {
		if n.Distance <= radius || n.State != "" {
			keep[n.Key] = true
			out.Nodes = append(out.Nodes, n)
		}
	}
	for _, e := range m.Edges {
		if keep[e.From] && keep[e.To] {
			out.Edges = append(out.Edges, e)
		}
	}
	if out.Nodes == nil {
		out.Nodes = []MapNode{}
	}
	if out.Edges == nil {
		out.Edges = []MapEdge{}
	}
	out.Shown = len(out.Nodes)
	out.Hidden = len(m.Nodes) - out.Shown
	return out
}

// FindNodes searches the WHOLE component — the point of a search on a map that
// shows less than everything is that it still reaches what is not shown.
//
// Three things are searchable and they are not equally trustworthy: a key and a
// mesh address are facts (the address derives from the key), while a name beyond
// our own friends is hearsay a friend passed on. Every result carries the key for
// that reason, and Matched says which field answered, so the UI can show a name
// hit as a name hit.
func FindNodes(m NetworkMap, q string, limit int) []MapHit {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return []MapHit{}
	}
	hits := []MapHit{}
	for _, n := range m.Nodes {
		matched, rank := matchNode(n, q)
		if matched == "" {
			continue
		}
		hits = append(hits, MapHit{MapNode: n, Matched: matched, rank: rank})
	}
	// Best kind of match first, then nearest — an exact key beats a name
	// fragment, and among equals the node closer to us is the more likely
	// subject. Key breaks the tie so the order is stable across requests.
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].rank != hits[j].rank {
			return hits[i].rank < hits[j].rank
		}
		if hits[i].Distance != hits[j].Distance {
			return hits[i].Distance < hits[j].Distance
		}
		return hits[i].Key < hits[j].Key
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// matchNode reports which field matched and how good the match is (lower is
// better). A prefix beats a substring because that is how someone pasting the
// front of a key expects it to behave.
func matchNode(n MapNode, q string) (string, int) {
	key := strings.ToLower(n.Key)
	switch {
	case key == q:
		return "key", 0
	case strings.HasPrefix(key, q):
		return "key", 1
	}
	if addr := strings.ToLower(n.Address); addr != "" {
		switch {
		case addr == q:
			return "address", 0
		case strings.Contains(addr, q):
			return "address", 2
		}
	}
	if name := strings.ToLower(n.Name); name != "" {
		switch {
		case name == q:
			return "name", 1
		case strings.Contains(name, q):
			return "name", 3
		}
	}
	if strings.Contains(key, q) {
		return "key", 4
	}
	return "", 0
}

// BranchNodes returns everything that reached us through one direct friend —
// the unit blocking actually operates on, since blocking a friend snips the
// whole branch behind it (§Trust graph). The friend itself is included: it is
// the root of its own branch, and it is what an admin would be blocking.
//
// A node reachable through several friends appears in each of their branches,
// which is not a flaw in the answer: it is why blocking one friend may not
// remove it, and the admin has to be able to see that before acting.
func BranchNodes(m NetworkMap, friendKey string) []MapNode {
	out := []MapNode{}
	for _, n := range m.Nodes {
		if n.Key == friendKey {
			out = append(out, n)
			continue
		}
		for _, v := range n.Via {
			if v == friendKey {
				out = append(out, n)
				break
			}
		}
	}
	return out
}

// Paths returns the simple paths joining two nodes, shortest first — the answer
// to the question an admin actually has when something looks wrong: *how is this
// node connected to me, and through whom*. Which is the same question a block is
// the answer to, so the paths are what tells them whether one block is enough.
//
// Bounded twice over (MaxPathResults, MaxPathLength) because a graph with cycles
// has exponentially many simple paths and nobody reads the thousandth one. The
// search is breadth-first so the bound cuts the LONGEST paths rather than an
// arbitrary set: a truncated list that still holds every short path is useful,
// while one missing the direct connection would be misleading.
func Paths(m NetworkMap, from, to string) [][]string {
	if from == "" || to == "" || from == to {
		return [][]string{}
	}
	adj := map[string][]string{}
	for _, e := range m.Edges {
		adj[e.From] = append(adj[e.From], e.To)
		adj[e.To] = append(adj[e.To], e.From)
	}
	for k := range adj {
		sort.Strings(adj[k]) // deterministic exploration → deterministic output
	}

	out := [][]string{}
	queue := [][]string{{from}}
	for len(queue) > 0 && len(out) < MaxPathResults {
		path := queue[0]
		queue = queue[1:]
		if len(path) > MaxPathLength {
			continue
		}
		last := path[len(path)-1]
		for _, next := range adj[last] {
			if next == to {
				found := append(append([]string{}, path...), next)
				out = append(out, found)
				if len(out) == MaxPathResults {
					break
				}
				continue
			}
			if onPath(path, next) {
				continue
			}
			queue = append(queue, append(append([]string{}, path...), next))
		}
	}
	return out
}

func onPath(path []string, key string) bool {
	for _, p := range path {
		if p == key {
			return true
		}
	}
	return false
}
