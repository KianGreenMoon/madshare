//go:build !nofederation

package federation

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Gossiped freshness hints (F7 item 10, docs/architecture/federation.md
// §Availability, "Two clocks, two windows").
//
// Item 5 made most of the nodes we cache catalogs from MEMBERS rather than
// friends, and a member's only liveness clock is the catalog pull — fifteen
// minutes at best. Judged by the window that was sized for the one-minute
// friendship ping, a two-hop member's tracks showed for about three minutes in
// every fifteen: discovery reached the community's libraries and availability
// hid them again.
//
// The window now has a class (reachClause), which fixes it. This file is the
// other half: making the answer accurate rather than merely generous. Our
// friends already ping THEIR friends every minute, so most of a small
// community's members are watched at ping cadence by somebody we talk to every
// minute anyway. Asking costs one query parameter on a request the refresh loop
// already makes.
//
// Three rules keep it honest, and each of them is also what keeps it cheap:
//
//   - A node vouches only for what it touches ITSELF — its direct friends,
//     never the sources it merely pulls from. Relaying second-hand liveness
//     could not satisfy a 180-second window anyway, since the first non-friend
//     relay is already on the fifteen-minute pull clock.
//   - Hints are exchanged between friends only. That is a trust boundary, but
//     it discloses nothing new: the hint list IS our friend list, which every
//     friend already holds as a signed F6 record.
//   - AGES travel, not timestamps. Two nodes need not agree on the clock, and
//     an age composes across a hop without them having to.

// buildFreshnessHints returns what this node can vouch for first-hand: for each
// direct friend, how long ago we last saw it. Freshest first, so the cap keeps
// the claims worth having; `except` is the asking node, which has no use for our
// opinion of its own liveness.
func buildFreshnessHints(peers []*Peer, except string, now int64) map[string]int64 {
	type aged struct {
		key string
		age int64
	}
	var fresh []aged
	for _, p := range peers {
		if p.State != PeerFriend || p.LastSeen <= 0 || p.PublicKey == except {
			continue
		}
		age := now - p.LastSeen
		if age < 0 {
			age = 0 // a clock that moved backwards is not evidence of the future
		}
		if age > int64(MaxHintAge/time.Second) {
			continue
		}
		fresh = append(fresh, aged{p.PublicKey, age})
	}
	if len(fresh) == 0 {
		return nil
	}
	sort.Slice(fresh, func(i, j int) bool {
		if fresh[i].age != fresh[j].age {
			return fresh[i].age < fresh[j].age
		}
		return fresh[i].key < fresh[j].key
	})
	if len(fresh) > MaxFreshnessHints {
		fresh = fresh[:MaxFreshnessHints]
	}
	out := make(map[string]int64, len(fresh))
	for _, a := range fresh {
		out[a.key] = a.age
	}
	return out
}

// freshnessHints answers a ping that asked for them. Nil for anyone but a direct
// friend, and nil when the caller did not ask — the hint list is several tens of
// kilobytes at its bound, and most pings (the discovery ping to a member, a
// health check, the mesh lab) want the four-field reply.
func (n *Node) freshnessHints(r *http.Request) map[string]int64 {
	if n.store == nil || r.URL.Query().Get("hints") != "1" {
		return nil
	}
	peers, err := n.store.ListFederationPeers(r.Context())
	if err != nil {
		n.logger.Printf("federation: freshness hints: %v", err)
		return nil
	}
	asker := matchPeerAddr(peers, remoteIP(r))
	if asker == nil || asker.State != PeerFriend {
		return nil
	}
	return buildFreshnessHints(peers, asker.PublicKey, time.Now().Unix())
}

// applyFreshnessHints records what a friend just vouched for. Everything it
// refuses, it refuses because accepting would let hearsay do something only
// first-hand contact may:
//
//   - a key that is not in our community — the perimeter decides what we cache,
//     and a friend cannot widen it by talking about a stranger;
//   - our own key — our liveness is not something anyone else observes for us;
//   - an age past MaxHintAge, or a negative one, which is a broken clock rather
//     than an observation.
//
// What survives updates only sources we ALREADY hold (the store's UPDATE is
// keyed on the node key), so a hint can refresh a cached catalog's freshness but
// can never mint the row that says we pull from that node.
func (n *Node) applyFreshnessHints(ctx context.Context, from *Peer, hints map[string]int64) {
	if n.store == nil || len(hints) == 0 {
		return
	}
	members, err := n.community(ctx)
	if err != nil {
		n.logger.Printf("federation: community for freshness hints: %v", err)
		return
	}
	// Our own key comes from the member set rather than from the core: the two
	// always agree (the set is built from it), and reading it here keeps the
	// self-check on the same snapshot as the membership check it sits beside.
	self := members.self
	now := time.Now().Unix()
	maxAge := int64(MaxHintAge / time.Second)
	seen := make(map[string]int64, len(hints))
	for key, age := range hints {
		if len(seen) >= MaxFreshnessHints {
			break
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if !isNodeKeyHex(key) || key == self || age < 0 || age > maxAge {
			continue
		}
		if !members.member(key) {
			continue
		}
		seen[key] = now - age
	}
	if len(seen) == 0 {
		return
	}
	if _, err := n.store.ApplyFreshnessHints(ctx, seen, now); err != nil {
		n.logger.Printf("federation: apply freshness hints from %s: %v", from.Display(), err)
	}
}
