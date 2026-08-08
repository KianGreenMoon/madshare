//go:build !nofederation

package federation

import (
	"context"
	"net"
	"net/http"
	"time"
)

// Membership — who belongs to our community, resolved for the running node (F7,
// docs/architecture/federation.md §The membership rule). The rule itself is
// [MemberKeys], which is pure and lives in gossip.go; this file is the part that
// needs a node: the store reads, the memo, and the address lookup a mesh request
// arrives with.
//
// It sits on every mesh request's path, which is why it is memoized. The set
// changes only when the graph or our peer list does — both of which happen on
// the sweep — so the cache is refreshed there and the TTL below is a backstop
// for the case where the sweep is slow or not running at all.

// memberSet is one computed view of our community, plus the index a request
// actually needs. Mesh address derivation is one-way (address = f(key)), so a
// connection's source address can only be matched by deriving every candidate's
// address — the same walk peerFromRemote does over the peer table. Doing that
// per request over a community of thousands would be silly, so it is done once
// per refresh and kept as a map.
type memberSet struct {
	keys map[string]struct{} // member public keys (lowercase hex)
	// homes are the servers this node signs in to (§"The household"). Kept apart
	// from keys rather than folded in, because they are not members and the
	// difference is visible: MemberCount reports a community, and a device whose
	// community is empty has one all the same.
	homes map[string]struct{}
	addrs map[string]string // mesh address string → public key (members and homes)
	self  string            // our own key, so callers can exclude us without a live core
	built time.Time
}

// lookup resolves a source mesh address to the member key it derives from.
//
// A home node resolves here too, which is what makes the server a device signed
// in to able to pull from it: one relationship, deliberately asymmetric, and the
// only node a listener can place without a graph.
func (m *memberSet) lookup(ip net.IP) (string, bool) {
	if m == nil || ip == nil {
		return "", false
	}
	key, ok := m.addrs[ip.String()]
	return key, ok
}

// vouches reports whether key may issue capability tokens this node honours
// (F7 item 9, token.go): a node we place in our own community by our own walk,
// ourselves — our own users' devices present tokens we signed, and a madplayer
// fetching chunks from its own home server is the ordinary case, not an edge one
// — or a home server we have signed in to.
//
// Deliberately the community and not the friend list. "Verified by that server's
// friends" was written when direct friendship was the access boundary; item 3
// moved that boundary, and leaving the token behind would make a device reach
// strictly less than the server vouching for it (§"The capability token").
//
// The home arm is what gives a listener node an audience at all
// (§"The household"). On a server it is always empty, so nothing changes there;
// on a device it means the population that can reach it is exactly the
// population its home server authenticated — that server, and its other devices,
// carrying tokens it signed.
func (m *memberSet) vouches(key string) bool {
	if m == nil || key == "" {
		return false
	}
	if key == m.self {
		return true
	}
	if _, ok := m.keys[key]; ok {
		return true
	}
	_, ok := m.homes[key]
	return ok
}

// newMemberSet computes the community from raw store contents and indexes it by
// mesh address. A key whose address cannot be derived is dropped from the index
// but kept in keys: it can never be matched to a connection, and a malformed key
// in the graph should not be able to hide the rest of the branch.
//
// asOf is when the INPUTS were read, not when the walk finished. The difference
// matters because the set is memoized and compared by age: stamping it with the
// completion time would let a slow computation over old contents pass for a
// fresh answer.
func newMemberSet(selfKey string, peers []*Peer, edges []GraphEdgeClaim, homes []HomeNode, asOf time.Time) *memberSet {
	keys := MemberKeys(selfKey, peers, edges)
	addrs := make(map[string]string, len(keys)+len(homes))
	for key := range keys {
		if addr, err := AddrForKeyHex(key); err == nil {
			addrs[addr.String()] = key
		}
	}
	homeKeys := make(map[string]struct{}, len(homes))
	for _, h := range homes {
		key, err := NormalizeKey(h.PublicKey)
		if err != nil || key == selfKey {
			// A malformed key places nobody, and a node cannot be its own home
			// server — SignCapabilityToken already refuses to issue that token, so
			// honouring one would be believing something nobody can say.
			continue
		}
		homeKeys[key] = struct{}{}
		// A member key wins the address index: it was established by our own walk,
		// while this is a decision we recorded about somebody else.
		if _, taken := keys[key]; taken {
			continue
		}
		if addr, err := AddrForKeyHex(key); err == nil {
			addrs[addr.String()] = key
		}
	}
	return &memberSet{keys: keys, homes: homeKeys, addrs: addrs, self: selfKey, built: asOf}
}

// refreshMembers recomputes the community from contents the caller already has
// in hand. Called from the sweep's graph pass, where peers and edges were just
// read for the retention walk — the two answers come from the same inputs, so
// computing them together keeps them from ever disagreeing.
//
// readAt is when the sweep read those contents, which is NOT now: the sweep
// reads its peer list at the top of a round and then does real work — publishing
// records, expiring the store, walking the graph — so by the time this runs, a
// request may already have rebuilt the set from a later state of the store. It
// therefore refuses to replace a memo computed from newer inputs. Overwriting
// one would serve the older perimeter *stamped as fresh*, which is how an
// accepted friendship could briefly stop being a membership.
func (n *Node) refreshMembers(peers []*Peer, edges []GraphEdgeClaim, homes []HomeNode, readAt time.Time) {
	n.installMembers(newMemberSet(n.PublicKeyHex(), peers, edges, homes, readAt))
}

// installMembers publishes a computed community, unless the memo already there
// was computed from NEWER inputs. Both producers go through it — the sweep and
// a request that found the memo stale — because they race by construction and
// the older answer must never win on arrival order.
func (n *Node) installMembers(set *memberSet) {
	n.memberMu.Lock()
	defer n.memberMu.Unlock()
	if n.members != nil && n.members.built.After(set.built) {
		return
	}
	n.members = set
}

// community returns the member set, rebuilding it if the memo is missing or
// older than Intervals.MembershipTTL.
func (n *Node) community(ctx context.Context) (*memberSet, error) {
	n.memberMu.Lock()
	cur := n.members
	n.memberMu.Unlock()
	if cur != nil && time.Since(cur.built) < n.intervals.MembershipTTL {
		return cur, nil
	}
	if n.store == nil {
		return &memberSet{}, nil
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
	homes, err := n.store.ListHomeNodes(ctx)
	if err != nil {
		return nil, err
	}
	// Stamped with the read time, so this answer ages from when it was true
	// rather than from when it finished being computed.
	set := newMemberSet(n.PublicKeyHex(), peers, edges, homes, readAt)
	n.installMembers(set)
	// The caller gets what it just computed, not whatever won the memo: this
	// answer is at least as new, and a request must never be served a perimeter
	// older than the one it read the store for.
	return set, nil
}

// memberFromRemote resolves a mesh request's source address to a member key.
// ok is false for a node outside our community — and for a direct friend, which
// callers resolve from the peer table first, since a friend is a local fact and
// this is hearsay.
func (n *Node) memberFromRemote(r *http.Request) (string, bool, error) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", false, nil
	}
	set, err := n.community(r.Context())
	if err != nil {
		return "", false, err
	}
	key, ok := set.lookup(ip)
	return key, ok, nil
}

// MemberCount is how many nodes this node currently counts as its community, for
// the admin surface. Direct friends included; ourselves not.
func (n *Node) MemberCount(ctx context.Context) (int, error) {
	set, err := n.community(ctx)
	if err != nil {
		return 0, err
	}
	return len(set.keys), nil
}
