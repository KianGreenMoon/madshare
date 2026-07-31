//go:build !nofederation

package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Discovery beyond the friend ring (F7 item 5, docs/architecture/federation.md
// §Discovery beyond the friend ring) — the *pulling* half of reach.
//
// Serving our community was one line of policy, because handleCatalog already
// answered for an audience. Seeing it is this file. What stood between an admin
// and "the whole community's libraries are reachable" was never authorization:
// it was knowing that a hash exists. A node that only ever pulled from its
// direct friends could be perfectly willing to fetch from a member and still
// have no idea what that member holds.
//
// Whom to ask costs nothing — the F6 graph is a directory of node keys and a
// mesh address derives from a key. How much to ask is the whole design problem,
// and the answer is deliberately bounded rather than exhaustive:
//
//   - friends are pulled every cycle, unbudgeted (few, and chosen);
//   - members are pulled a handful per cycle, least-recently-attempted first, so
//     the frontier expands steadily instead of in one storm;
//   - foreign catalogs are capped, least-recently-seen evicted, because a cached
//     catalog is a droppable cache and always was;
//   - and an explicit pull-now jumps the rotation, so interest beats fairness.
//
// Rotating on *attempts* rather than on successes is what keeps that fair: a
// node that never answers must still lose its turn, or one dead key would be
// retried ahead of every live member forever.

// syncSources runs one round of the pulling half. It returns the keys of the
// friends whose catalog was due this round, which is the cadence the gossip pull
// rides (the sweep runs it for those, plus the Rescan button's forced round).
//
// peers is the caller's already-read peer list, so the sweep reads the table
// once — the same economy expireGraph makes with the graph store.
func (n *Node) syncSources(ctx context.Context, peers []*Peer) map[string]struct{} {
	pulledFriends := map[string]struct{}{}
	if n.store == nil {
		return pulledFriends
	}
	now := time.Now().Unix()

	// Every friend is a source. Creating the row here rather than at the moment
	// a friendship is accepted keeps one path: whatever made us friends — an
	// admin's accept, a pairing that converged, a restore from backup — the next
	// sweep notices.
	friends := map[string]*Peer{}
	for _, p := range peers {
		if p.State == PeerFriend {
			friends[p.PublicKey] = p
		}
	}
	for key := range friends {
		if _, err := n.store.EnsureCatalogSource(ctx, key, now); err != nil {
			n.logger.Printf("federation: ensure catalog source %s: %v", shortKey(key), err)
		}
	}

	sources, err := n.store.ListCatalogSources(ctx)
	if err != nil {
		n.logger.Printf("federation: list catalog sources: %v", err)
		return pulledFriends
	}
	members, err := n.community(ctx)
	if err != nil {
		n.logger.Printf("federation: community for discovery: %v", err)
		return pulledFriends
	}
	sources = n.retainSources(ctx, sources, peers, members)

	// The explicit queue is drained whether or not the node is in our community
	// yet: an admin who pasted a key is asking us to try, and a 403 answers that
	// question better than refusing to dial.
	wanted := n.takePullNow()
	for key := range wanted {
		if _, err := n.store.EnsureCatalogSource(ctx, key, now); err != nil {
			n.logger.Printf("federation: ensure requested source %s: %v", shortKey(key), err)
			delete(wanted, key)
		}
	}
	if len(wanted) > 0 {
		if sources, err = n.store.ListCatalogSources(ctx); err != nil {
			n.logger.Printf("federation: list catalog sources: %v", err)
			return pulledFriends
		}
	}

	// Phase 1 — friends and explicit requests, neither of them budgeted.
	known := map[string]struct{}{}
	for _, s := range sources {
		if ctx.Err() != nil {
			return pulledFriends
		}
		known[s.PublicKey] = struct{}{}
		_, requested := wanted[s.PublicKey]
		p, isFriend := friends[s.PublicKey]
		if !isFriend && !requested {
			continue
		}
		if isFriend {
			if !n.catalogDue(s) && !requested {
				continue
			}
			pulledFriends[s.PublicKey] = struct{}{}
		}
		n.syncSource(ctx, s, p)
	}

	// Phase 2 — the frontier, in one order: least-recently-attempted first.
	//
	// A member we have never pulled from has no source row at all, so it is not
	// in `sources` and is by definition the least-recently-attempted thing there
	// is — hence it goes first. Getting this backwards is what a live 5-node
	// chain caught: spending the budget on the members we already knew meant the
	// two nodes reached in the first round consumed every later round as well,
	// and the frontier never moved past them.
	budget := n.discovery.Budget
	for _, key := range n.frontier(members, known, budget) {
		if ctx.Err() != nil {
			return pulledFriends
		}
		src, err := n.store.EnsureCatalogSource(ctx, key, time.Now().Unix())
		if err != nil {
			n.logger.Printf("federation: ensure frontier source %s: %v", shortKey(key), err)
			continue
		}
		budget--
		n.syncSource(ctx, src, nil)
	}
	for _, s := range sources {
		if ctx.Err() != nil || budget <= 0 {
			return pulledFriends
		}
		if _, isFriend := friends[s.PublicKey]; isFriend {
			continue
		}
		if !members.member(s.PublicKey) {
			continue // not ours to pull from; retention will collect it
		}
		if !n.catalogDue(s) {
			continue
		}
		budget--
		n.syncSource(ctx, s, nil)
	}
	return pulledFriends
}

// frontier picks up to budget members we hold no catalog from yet. Sorted, so a
// round is reproducible; the pool drains rather than needing fairness of its
// own, because attempting a node gives it a source row and it joins the
// rotation.
func (n *Node) frontier(members *memberSet, known map[string]struct{}, budget int) []string {
	if members == nil || budget <= 0 {
		return nil
	}
	var fresh []string
	for key := range members.keys {
		if _, have := known[key]; have {
			continue
		}
		if key == members.self {
			continue // ourselves: our library is merged in locally, never pulled
		}
		fresh = append(fresh, key)
	}
	sort.Strings(fresh)
	if len(fresh) > budget {
		fresh = fresh[:budget]
	}
	return fresh
}

// catalogDue reports whether a source's cached catalog is stale enough to re-pull.
func (n *Node) catalogDue(s *CatalogSource) bool {
	return time.Since(time.Unix(s.CatalogSyncedAt, 0)) >= n.intervals.CatalogSync
}

// syncSource pulls one node's catalog and holdings. peer is its peer row when it
// has one, nil for a member — which is the only thing the two cases differ by:
// a friend is pinged by the refresh loop every minute, so this round already
// knows its name and that it is alive, while for a member this *is* the contact.
func (n *Node) syncSource(ctx context.Context, s *CatalogSource, peer *Peer) {
	if err := n.store.MarkCatalogSourceAttempted(ctx, s.ID, time.Now().Unix()); err != nil {
		n.logger.Printf("federation: mark source %s attempted: %v", s.Display(), err)
	}
	if peer == nil {
		n.pingSource(ctx, s)
	}
	n.syncCatalog(ctx, s)
	n.syncHoldings(ctx, s)
}

// pingSource is the friend loop's ping for a node that is not a friend: it
// records liveness and learns what the node calls itself, which is otherwise
// blank on the /madnetwork strip because nothing else on the pull path carries a
// name. Cheap enough to do per pull round — one small request per budgeted node.
func (n *Node) pingSource(ctx context.Context, s *CatalogSource) {
	addr, err := AddrForKeyHex(s.PublicKey)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://[%s]:%d/madnetwork/v0/ping", addr, MeshPort), nil)
	if err != nil {
		return
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var reply struct {
		Name string `json:"name"`
	}
	// A reply at all is the liveness; the name is additive and may be absent.
	_ = json.NewDecoder(io.LimitReader(resp.Body, 8<<10)).Decode(&reply)
	if err := n.store.TouchCatalogSourceSeen(ctx, s.ID, time.Now().Unix(), reply.Name); err != nil {
		n.logger.Printf("federation: touch source %s: %v", s.Display(), err)
	}
}

// retainSources drops what we may no longer keep and evicts what no longer fits,
// returning the survivors in the order it was given (least-recently-attempted
// first, which is the rotation order).
//
// Two rules, and they are different in kind. The first is about *admission*: we
// keep a catalog only from a node we still have a relationship with — a direct
// friend, a member of our community, or a peer we blocked. A branch a block or a
// removal cut off stops being a member (§Forgetting), so the same walk that
// un-draws it on the map collects its cached library here. The second is about
// *cost*: past the cap, the least-recently-seen foreign catalogs go, because
// nothing local references them and the network can always be asked again.
//
// Blocked peers are kept deliberately, and hidden by the browse instead — an
// unblock restores the view without a resync, which has been the rule since F2.
// A *pending* peer is not kept: nothing ever pulled from it, and there is no
// decision of an admin's to preserve.
//
// This is also where the two halves of visibility divide. Admission is decided
// here, once a minute, because membership is a graph walk that SQL cannot do.
// Blocking is decided in the browse query, because it is a local act that must
// take effect the moment an admin clicks it and not on the next sweep.
//
// Friends and blocked peers are never evicted by the cap. An admin decided
// something about those nodes, and a cache that forgets them to make room for
// strangers has its priorities backwards.
func (n *Node) retainSources(ctx context.Context, sources []*CatalogSource, peers []*Peer, members *memberSet) []*CatalogSource {
	known := map[string]struct{}{}
	for _, p := range peers {
		if p.State == PeerFriend || p.State == PeerBlocked {
			known[p.PublicKey] = struct{}{}
		}
	}

	kept := make([]*CatalogSource, 0, len(sources))
	var foreign []*CatalogSource
	var drop []int64
	for _, s := range sources {
		if _, ok := known[s.PublicKey]; ok {
			kept = append(kept, s)
			continue
		}
		if !members.member(s.PublicKey) {
			drop = append(drop, s.ID)
			continue
		}
		kept = append(kept, s)
		foreign = append(foreign, s)
	}

	if over := len(foreign) - n.discovery.Cap; over > 0 {
		stale := append([]*CatalogSource(nil), foreign...)
		sort.Slice(stale, func(i, j int) bool { return stale[i].LastSeen < stale[j].LastSeen })
		evicted := map[int64]struct{}{}
		for _, s := range stale[:over] {
			drop = append(drop, s.ID)
			evicted[s.ID] = struct{}{}
		}
		surviving := kept[:0]
		for _, s := range kept {
			if _, gone := evicted[s.ID]; !gone {
				surviving = append(surviving, s)
			}
		}
		kept = surviving
	}

	if len(drop) > 0 {
		if err := n.store.DropCatalogSources(ctx, drop); err != nil {
			n.logger.Printf("federation: drop %d catalog source(s): %v", len(drop), err)
			return sources // the store still holds them; do not pretend otherwise
		}
		n.logger.Printf("federation: dropped %d cached catalog(s) from nodes outside our community or past the cache cap", len(drop))
	}
	return kept
}

// PullFrom asks the refresh loop to fetch one node's catalog on its next round,
// ahead of the frontier rotation and outside its budget. Idempotent, and it
// wakes the loop — an admin who clicked something should not wait a minute to
// see whether it worked.
func (n *Node) PullFrom(publicKey string) error {
	key := strings.ToLower(strings.TrimSpace(publicKey))
	if !isNodeKeyHex(key) {
		return fmt.Errorf("not a node key")
	}
	n.pullMu.Lock()
	n.pullNow[key] = struct{}{}
	n.pullMu.Unlock()
	n.Nudge()
	return nil
}

// takePullNow drains the explicit queue. Draining rather than peeking is what
// makes a request cost one attempt: if the node is unreachable the rotation
// picks it up later, and an admin who wants another try can ask again.
func (n *Node) takePullNow() map[string]struct{} {
	n.pullMu.Lock()
	defer n.pullMu.Unlock()
	if len(n.pullNow) == 0 {
		return nil
	}
	out := n.pullNow
	n.pullNow = map[string]struct{}{}
	return out
}

// member reports whether a key is in this community view. Nil-safe: a node whose
// membership has not been computed yet has no members, which refuses a pull
// rather than allowing one.
func (m *memberSet) member(key string) bool {
	if m == nil {
		return false
	}
	_, ok := m.keys[key]
	return ok
}

// isNodeKeyHex reports whether s is a well-formed node public key: 64 lowercase
// hex characters, the same shape a content hash has.
func isNodeKeyHex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// shortKey abbreviates a key for a log line, for the cases that have no peer or
// source struct to ask.
func shortKey(key string) string {
	if len(key) > shortKeyRunes {
		return key[:shortKeyRunes]
	}
	return key
}
