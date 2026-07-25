//go:build tests && !nofederation

package main

// Topology presets. Each names the nodes and the UNDERLAY edges between them;
// the friendship graph is chosen separately (-friends), for the reasons in
// lab.go. A preset ships a sensible default friendship graph but never implies
// one — that separation is what lets the same shape express both "friends
// several hops apart" and "underlay-adjacent strangers".

import (
	"fmt"
	"sort"
	"strings"
)

// edge is one underlay peering: from dials to.
type edge struct{ from, to string }

type topology struct {
	name    string
	nodes   []string
	edges   []edge
	friends [][2]string
}

// presets, by name. nodeCount is how many nodes the shape wants by default;
// most accept more.
var presets = map[string]func(n int) (topology, error){
	"pair":     pairTopology,
	"triangle": triangleTopology,
	"hub":      hubTopology,
	"chain":    chainTopology,
}

func presetNames() []string {
	names := make([]string, 0, len(presets))
	for k := range presets {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func nodeNames(n int) []string {
	names := make([]string, n)
	for i := range n {
		names[i] = string(rune('a' + i))
	}
	return names
}

// pair: a <- b. The smallest useful lab and the one a walkthrough starts with.
func pairTopology(n int) (topology, error) {
	if n != 0 && n != 2 {
		return topology{}, fmt.Errorf("pair is a 2-node shape (got -nodes %d)", n)
	}
	names := nodeNames(2)
	return topology{
		name: "pair", nodes: names,
		edges:   []edge{{"b", "a"}},
		friends: allPairs(names),
	}, nil
}

// triangle: every node dials every earlier one, so all three are adjacent.
// Useful as the swarm shape — two holders reachable independently.
func triangleTopology(n int) (topology, error) {
	if n != 0 && n != 3 {
		return topology{}, fmt.Errorf("triangle is a 3-node shape (got -nodes %d)", n)
	}
	names := nodeNames(3)
	return topology{
		name: "triangle", nodes: names,
		edges:   []edge{{"b", "a"}, {"c", "a"}, {"c", "b"}},
		friends: allPairs(names),
	}, nil
}

// hub: a is the backbone, every other node dials it and nothing else. Spokes
// are therefore two hops from each other — friendship between two spokes is
// carried entirely by yggdrasil's routing through a, which is the interesting
// part, and cutting a's links isolates the whole mesh at once.
func hubTopology(n int) (topology, error) {
	if n == 0 {
		n = 4
	}
	if n < 3 {
		return topology{}, fmt.Errorf("hub needs at least 3 nodes (got %d)", n)
	}
	names := nodeNames(n)
	edges := make([]edge, 0, n-1)
	for _, s := range names[1:] {
		edges = append(edges, edge{s, names[0]})
	}
	return topology{name: "hub", nodes: names, edges: edges, friends: allPairs(names)}, nil
}

// chain: a — b — c — …, each node dialing only its predecessor. The only shape
// that exercises yggdrasil's multi-hop routing rather than a single link, and
// the one where degrading a middle link degrades a friendship neither endpoint
// has a link to.
func chainTopology(n int) (topology, error) {
	if n == 0 {
		n = 3
	}
	if n < 3 {
		return topology{}, fmt.Errorf("chain needs at least 3 nodes to have a middle (got %d)", n)
	}
	names := nodeNames(n)
	edges := make([]edge, 0, n-1)
	for i := 1; i < n; i++ {
		edges = append(edges, edge{names[i], names[i-1]})
	}
	return topology{name: "chain", nodes: names, edges: edges, friends: allPairs(names)}, nil
}

func allPairs(names []string) [][2]string {
	var out [][2]string
	for i := range names {
		for j := i + 1; j < len(names); j++ {
			out = append(out, [2]string{names[i], names[j]})
		}
	}
	return out
}

// adjacentPairs is the friendship graph that mirrors the underlay exactly —
// every peering is a friendship and nothing else is.
func adjacentPairs(edges []edge) [][2]string {
	seen := map[string]bool{}
	var out [][2]string
	for _, e := range edges {
		a, b := e.from, e.to
		if a > b {
			a, b = b, a
		}
		key := a + "-" + b
		if !seen[key] {
			seen[key] = true
			out = append(out, [2]string{a, b})
		}
	}
	return out
}

// resolveFriends turns the -friends value into a graph.
//
//	all       every pair (the default)
//	adjacent  only underlay-peered pairs — the rest are strangers that can route
//	          to each other but must see nothing
//	none      nobody; every node is isolated at the madshare layer
//	a-c,b-c   an explicit list
func resolveFriends(spec string, top topology) ([][2]string, error) {
	switch strings.TrimSpace(spec) {
	case "", "all":
		return allPairs(top.nodes), nil
	case "adjacent":
		return adjacentPairs(top.edges), nil
	case "none":
		return nil, nil
	}
	known := map[string]bool{}
	for _, n := range top.nodes {
		known[n] = true
	}
	var out [][2]string
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		a, b, ok := strings.Cut(pair, "-")
		if !ok || a == "" || b == "" {
			return nil, fmt.Errorf("bad -friends entry %q, want two node names joined by a dash (a-b)", pair)
		}
		if !known[a] || !known[b] {
			return nil, fmt.Errorf("-friends %q names a node not in this topology (have: %s)",
				pair, strings.Join(top.nodes, ", "))
		}
		if a == b {
			return nil, fmt.Errorf("-friends %q befriends a node with itself", pair)
		}
		out = append(out, [2]string{a, b})
	}
	return out, nil
}
