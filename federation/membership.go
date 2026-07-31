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
	keys  map[string]struct{} // member public keys (lowercase hex)
	addrs map[string]string   // mesh address string → public key
	built time.Time
}

// lookup resolves a source mesh address to the member key it derives from.
func (m *memberSet) lookup(ip net.IP) (string, bool) {
	if m == nil || ip == nil {
		return "", false
	}
	key, ok := m.addrs[ip.String()]
	return key, ok
}

// newMemberSet computes the community from raw store contents and indexes it by
// mesh address. A key whose address cannot be derived is dropped from the index
// but kept in keys: it can never be matched to a connection, and a malformed key
// in the graph should not be able to hide the rest of the branch.
func newMemberSet(selfKey string, peers []*Peer, edges []GraphEdgeClaim) *memberSet {
	keys := MemberKeys(selfKey, peers, edges)
	addrs := make(map[string]string, len(keys))
	for key := range keys {
		if addr, err := AddrForKeyHex(key); err == nil {
			addrs[addr.String()] = key
		}
	}
	return &memberSet{keys: keys, addrs: addrs, built: time.Now()}
}

// refreshMembers recomputes the community from contents the caller already has
// in hand. Called from the sweep's graph pass, where peers and edges were just
// read for the retention walk — the two answers come from the same inputs, so
// computing them together keeps them from ever disagreeing.
func (n *Node) refreshMembers(peers []*Peer, edges []GraphEdgeClaim) {
	set := newMemberSet(n.PublicKeyHex(), peers, edges)
	n.memberMu.Lock()
	n.members = set
	n.memberMu.Unlock()
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
	peers, err := n.store.ListFederationPeers(ctx)
	if err != nil {
		return nil, err
	}
	edges, err := n.store.GraphEdges(ctx, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	set := newMemberSet(n.PublicKeyHex(), peers, edges)
	n.memberMu.Lock()
	n.members = set
	n.memberMu.Unlock()
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
