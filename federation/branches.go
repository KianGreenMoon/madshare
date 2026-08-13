//go:build !nofederation

package federation

import (
	"context"
	"sort"
	"time"
)

// Branch attribution for the browse (F7 item 10, docs/architecture/federation-trust.md
// §Trust graph). The network map already computes "which direct friends does this
// node reach us through" for the admin diagram; this is the same walk exposed as
// a plain lookup table, because the madnetwork page needs it on every request
// that orders anything by how many nodes hold something.
//
// It is a separate entry point rather than a call to NetworkMap for one reason:
// NetworkMap also groups the distrust marks and derives a mesh address for every
// node in the component — ed25519 work per node, wanted on a diagram an admin
// opens occasionally and not on a search-as-you-type. Branch attribution is the
// BFS alone.

// branchMemo is one walk's answers, aged from when its inputs were read (the
// newMemberSet rule: a slow walk over old contents must not pass for a fresh
// answer). It holds BOTH halves the walk produces — the branch attribution and
// the hop distances — because they come out of one BFS and a page load that
// orders versions by branches and nodes by hops would otherwise walk the graph
// twice for one answer.
type branchMemo struct {
	branches map[string][]string
	hops     map[string]int
	built    time.Time
}

// BranchMap returns each reachable node key mapped to the direct friends it
// reaches us through — the input to database.BranchMap's sybil rule. Nodes we
// cannot place are simply absent, which the weighting reads as "its own voice".
//
// Memoized for Intervals.MembershipTTL: the answer changes only when the graph
// or our peer list does, both of which are sweep-paced, and the browse asks for
// it far more often than that. The TTL bounds staleness in the harmless
// direction — a branch that just appeared is at worst counted as its own voice
// for a minute, which understates corroboration rather than inventing it.
func (n *Node) BranchMap(ctx context.Context) (map[string][]string, error) {
	memo, err := n.graphMemo(ctx)
	if memo == nil || err != nil {
		return nil, err
	}
	return memo.branches, nil
}

// HopMap returns each reachable node key mapped to its friendship distance from
// us: 0 is this node, 1 a direct friend, 2 a friend's friend. Nodes we cannot
// place are absent, which every caller reads as "distance unknown" and none as
// zero — zero is us (docs/ui/madnetwork-nodes.md §Ordering).
//
// It is the other half of BranchMap's walk and shares its memo, so asking for
// both costs one BFS. What it is FOR is ordering: the nodes an admin chose
// personally first, then the ones they chose, outward. That is a rendering
// order and nothing else — the same warning mapview.go carries about the view
// radius applies here, and more sharply, because this number reaches a page
// ordinary users see: it never limits who is served and never appears in a scope.
func (n *Node) HopMap(ctx context.Context) (map[string]int, error) {
	memo, err := n.graphMemo(ctx)
	if memo == nil || err != nil {
		return nil, err
	}
	return memo.hops, nil
}

// graphMemo returns the current walk, computing it when the memo has aged out.
// Nil (with no error) when there is no store to walk — the degenerate world in
// which every node is its own voice and none can be placed.
func (n *Node) graphMemo(ctx context.Context) (*branchMemo, error) {
	n.branchMu.Lock()
	cur := n.branches
	n.branchMu.Unlock()
	if cur != nil && time.Since(cur.built) < n.intervals.MembershipTTL {
		return cur, nil
	}
	if n.store == nil {
		return nil, nil
	}
	readAt := time.Now()
	peers, err := n.store.ListFederationPeers(ctx)
	if err != nil {
		return nil, err
	}
	edges, err := n.store.GraphEdges(ctx, readAt.Unix())
	if err != nil {
		return nil, err
	}
	branches, hops := graphOf(n.PublicKeyHex(), peers, edges)
	memo := &branchMemo{branches: branches, hops: hops, built: readAt}

	n.branchMu.Lock()
	if n.branches == nil || !n.branches.built.After(readAt) {
		n.branches = memo
	}
	n.branchMu.Unlock()
	return memo, nil
}

// graphOf is the walk itself, pure so the attribution the browse weights by and
// the distances it orders nodes by can be tested — and compared against the map
// the admin sees — without a mesh. It MUST agree with BuildNetworkMap for the
// same inputs: a holder's ⓘ panel links straight to that node on the map, so a
// ranking explained by one graph and a diagram drawn from another would be two
// answers to one question.
func graphOf(selfKey string, peers []*ExternalNode, edges []GraphEdgeClaim) (map[string][]string, map[string]int) {
	dist, via := walkGraph(selfKey, peers, edges)
	branches := make(map[string][]string, len(via))
	for key, labels := range via {
		if len(labels) == 0 {
			continue
		}
		out := make([]string, 0, len(labels))
		for l := range labels {
			out = append(out, l)
		}
		sort.Strings(out) // map order is not an answer; two identical graphs must agree
		branches[key] = out
	}
	return branches, dist
}

// branchesOf is graphOf's attribution half, kept for the tests that pin the
// branch rule on its own.
func branchesOf(selfKey string, peers []*ExternalNode, edges []GraphEdgeClaim) map[string][]string {
	branches, _ := graphOf(selfKey, peers, edges)
	return branches
}
