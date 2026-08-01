//go:build !nofederation

package federation

import (
	"context"
	"sort"
	"time"
)

// Branch attribution for the browse (F7 item 10, docs/architecture/federation.md
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

// branchMemo is one computed attribution table, aged from when its inputs were
// read (the newMemberSet rule: a slow walk over old contents must not pass for a
// fresh answer).
type branchMemo struct {
	branches map[string][]string
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
	n.branchMu.Lock()
	cur := n.branches
	n.branchMu.Unlock()
	if cur != nil && time.Since(cur.built) < n.intervals.MembershipTTL {
		return cur.branches, nil
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
	branches := branchesOf(n.PublicKeyHex(), peers, edges)

	n.branchMu.Lock()
	if n.branches == nil || !n.branches.built.After(readAt) {
		n.branches = &branchMemo{branches: branches, built: readAt}
	}
	n.branchMu.Unlock()
	return branches, nil
}

// branchesOf is the walk itself, pure so the attribution the browse weights by
// can be tested — and compared against the map the admin sees — without a mesh.
// It MUST agree with BuildNetworkMap's Via for the same inputs: a holder's ⓘ
// panel links straight to that node on the map, so a ranking explained by one
// graph and a diagram drawn from another would be two answers to one question.
func branchesOf(selfKey string, peers []*Peer, edges []GraphEdgeClaim) map[string][]string {
	_, via := walkGraph(selfKey, peers, edges)
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
	return branches
}
