//go:build !nofederation

package federation

import (
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

// TestMemberSetAgesFromItsInputs: the stamp is when the contents were READ, not
// when the walk finished. Stamping completion would let a slow computation over
// old contents pass for a fresh answer — which is exactly what the guard above
// compares, so getting this wrong would quietly disable it.
func TestMemberSetAgesFromItsInputs(t *testing.T) {
	asOf := time.Now().Add(-time.Minute)
	set := newMemberSet(k("me"), []*Peer{{PublicKey: k("a"), State: PeerFriend}}, nil, nil, asOf)
	if !set.built.Equal(asOf) {
		t.Errorf("built = %v, want the input read time %v", set.built, asOf)
	}
	if _, ok := set.keys[k("a")]; !ok {
		t.Error("a direct friend must be a member unconditionally")
	}
}
