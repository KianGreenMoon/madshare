//go:build !nofederation

package federation

import (
	"io"
	"log"
	"testing"
	"time"
)

// The membership MEMO, as opposed to the rule it caches (membership_test.go).
// Two producers write it — the sweep, and a request that found it stale — and
// they race by construction, so arrival order must not decide the answer.

// memberSetAt builds a set "as of" a moment, containing exactly the named keys.
func memberSetAt(asOf time.Time, labels ...string) *memberSet {
	keys := map[string]struct{}{}
	for _, l := range labels {
		keys[k(l)] = struct{}{}
	}
	return &memberSet{keys: keys, addrs: map[string]string{}, self: k("me"), built: asOf}
}

// TestInstallMembersRefusesAStaleClobber is the bug found on 2026-08-01 while
// verifying F7 item 6. The sweep reads its peer list at the TOP of a round and
// only turns it into the memo some real work later — publishing records,
// expiring the store, walking the graph. A request that rebuilt the set in
// between holds a newer answer, and the sweep used to overwrite it and stamp the
// result with the write time, so an accepted friendship could stop being a
// membership for a whole TTL. On the mesh tests, where the TTL is a millisecond,
// it showed up as a member being served as an outsider.
func TestInstallMembersRefusesAStaleClobber(t *testing.T) {
	n := &Node{}
	now := time.Now()

	// A request rebuilt the set from a fresh read: `a` was just accepted.
	n.installMembers(memberSetAt(now, "a"))

	// The sweep now finishes a round whose peer list predates that accept.
	n.installMembers(memberSetAt(now.Add(-time.Second)))

	n.memberMu.Lock()
	got := n.members
	n.memberMu.Unlock()
	if _, ok := got.keys[k("a")]; !ok {
		t.Error("a sweep working from an older read overwrote a newer membership — " +
			"the accepted friend stopped being a member, and the stale answer was " +
			"stamped fresh")
	}

	// Newer inputs do win, or the memo would never advance at all.
	n.installMembers(memberSetAt(now.Add(time.Second), "a", "b"))
	n.memberMu.Lock()
	got = n.members
	n.memberMu.Unlock()
	if _, ok := got.keys[k("b")]; !ok {
		t.Error("a set computed from newer inputs was refused")
	}
}

// TestInvalidateMembersDropsTheMemoAndItsPast is the defect found on 2026-08-23
// reproducing madplayer's "madnetwork tracks skip": the memo's inputs can change
// LOCALLY — a home node added, a friendship accepted or removed — and nothing
// told the memo, so the node refused perfectly valid capability tokens for up to
// a full MembershipTTL after AddHome. Invalidation has two halves: the memo is
// gone so the next request recomputes, and a sweep that read the store BEFORE
// the local action cannot re-install the perimeter that action changed — with
// the memo nil there is no built stamp left to compare, which is what the floor
// is for.
func TestInvalidateMembersDropsTheMemoAndItsPast(t *testing.T) {
	n := &Node{}
	before := time.Now().Add(-time.Second)
	n.installMembers(memberSetAt(before, "a"))

	n.InvalidateMembers()
	n.memberMu.Lock()
	dropped := n.members == nil
	n.memberMu.Unlock()
	if !dropped {
		t.Fatal("InvalidateMembers left the memo standing")
	}

	// The sweep finishes a round whose inputs predate the local action.
	n.installMembers(memberSetAt(before, "a"))
	n.memberMu.Lock()
	resurrected := n.members != nil
	n.memberMu.Unlock()
	if resurrected {
		t.Error("a set computed from pre-invalidation inputs was installed — the " +
			"sweep resurrected the perimeter a local action just changed, stamped fresh")
	}

	// A set read after the invalidation is exactly the answer it asked for.
	n.installMembers(memberSetAt(time.Now().Add(time.Second), "a", "b"))
	n.memberMu.Lock()
	got := n.members
	n.memberMu.Unlock()
	if got == nil {
		t.Fatal("a set computed from post-invalidation inputs was refused")
	}
	if _, ok := got.keys[k("b")]; !ok {
		t.Error("the post-invalidation set is not the one installed")
	}

	// And a nil node ignores the call, mirroring memberSet's own receivers.
	var nilNode *Node
	nilNode.InvalidateMembers()
}

// TestLocalPeerMutationsInvalidateTheMembershipMemo pins the wiring: every
// local action that turns a stranger into a member or a member into a
// non-member must drop the memo on its way out. Narrow nodes (no mesh, no
// sweep loop), so "the memo is gone" is deterministic — nothing races to heal
// it, and with the sweep's Nudge healing it in production these one-liners are
// exactly the code path a request between the mutation and the sweep depends
// on.
func TestLocalPeerMutationsInvalidateTheMembershipMemo(t *testing.T) {
	ctx := t.Context()
	cases := []struct {
		name  string
		state string // the peer row the mutation starts from
		act   func(t *testing.T, n *Node, id int64)
	}{
		{"AcceptPeer", PeerPendingIncoming, func(t *testing.T, n *Node, id int64) {
			if err := n.AcceptPeer(ctx, id); err != nil {
				t.Fatalf("AcceptPeer: %v", err)
			}
		}},
		// ImportCard's accept arm, handlePair's flip and pairWith's flip carry the
		// same one-line invalidation; they need a mesh identity to call, so the
		// wiring is read rather than driven here and the mesh suites exercise it.
		{"BlockPeer", PeerFriend, func(t *testing.T, n *Node, id int64) {
			if err := n.BlockPeer(ctx, id, "test"); err != nil {
				t.Fatalf("BlockPeer: %v", err)
			}
		}},
		{"UnblockPeer", PeerBlocked, func(t *testing.T, n *Node, id int64) {
			if err := n.UnblockPeer(ctx, id); err != nil {
				t.Fatalf("UnblockPeer: %v", err)
			}
		}},
		{"RemovePeer", PeerFriend, func(t *testing.T, n *Node, id int64) {
			if err := n.RemovePeer(ctx, id); err != nil {
				t.Fatalf("RemovePeer: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms := newMemStore()
			n := &Node{store: ms, logger: log.New(io.Discard, "", 0), intervals: Intervals{MembershipTTL: time.Hour}}
			id, err := ms.InsertFederationPeer(ctx, &ExternalNode{PublicKey: k("peer"), TrustState: tc.state})
			if err != nil {
				t.Fatal(err)
			}
			// The memo exists and, at an hour's TTL, would outlive the whole test.
			n.installMembers(memberSetAt(time.Now().Add(-time.Second), "somebody"))

			tc.act(t, n, id)

			n.memberMu.Lock()
			stale := n.members
			n.memberMu.Unlock()
			if stale != nil {
				t.Errorf("%s left the membership memo standing — the perimeter it changed "+
					"would be served for up to a full MembershipTTL", tc.name)
			}
		})
	}
}

// TestMemberSetAgesFromItsInputs: the stamp is when the contents were READ, not
// when the walk finished. Stamping completion would let a slow computation over
// old contents pass for a fresh answer — which is exactly what the guard above
// compares, so getting this wrong would quietly disable it.
func TestMemberSetAgesFromItsInputs(t *testing.T) {
	asOf := time.Now().Add(-time.Minute)
	set := newMemberSet(k("me"), []*ExternalNode{{PublicKey: k("a"), TrustState: PeerFriend}}, nil, nil, asOf)
	if !set.built.Equal(asOf) {
		t.Errorf("built = %v, want the input read time %v", set.built, asOf)
	}
	if _, ok := set.keys[k("a")]; !ok {
		t.Error("a direct friend must be a member unconditionally")
	}
}
