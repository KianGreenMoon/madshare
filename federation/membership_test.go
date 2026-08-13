package federation

import "testing"

// The membership rule (docs/architecture/federation-access.md §The membership rule).
// MemberKeys and BuildNetworkMap read one store and answer two questions, and
// the whole of F7's perimeter is the single place they disagree: the map draws
// an edge somebody claims, membership requires both ends to claim it.

// isMember is the assertion these tests are made of.
func isMember(set map[string]struct{}, label string) bool {
	_, ok := set[k(label)]
	return ok
}

// The disagreement, stated directly. b claims a friendship with c; c says
// nothing. The map draws it — an edge somebody asserts is worth seeing — and
// membership refuses it, because agreement is what makes a key a member rather
// than a name in somebody's list.
func TestMapDrawsAOneSidedEdgeThatMembershipRefuses(t *testing.T) {
	peers := []*ExternalNode{{PublicKey: k("b"), TrustState: PeerFriend}}
	edges := []GraphEdgeClaim{edge("b", "c", "")}

	if n := nodeByLabel(t, BuildNetworkMap(k("me"), peers, edges, nil), "c"); n == nil {
		t.Error("the map should draw c: a claimed edge is worth seeing")
	}
	if members := MemberKeys(k("me"), peers, edges); isMember(members, "c") {
		t.Error("c is a member on one side's say-so — 512 invented keys per record would be members")
	}
}

// And the agreement case, so the refusal above is not simply "nothing is ever a
// member": once c publishes the same friendship, it joins.
func TestMutualEdgeAdmitsAFriendOfAFriend(t *testing.T) {
	peers := []*ExternalNode{{PublicKey: k("b"), TrustState: PeerFriend}}
	edges := []GraphEdgeClaim{edge("b", "c", ""), edge("c", "b", "")}

	members := MemberKeys(k("me"), peers, edges)
	if !isMember(members, "c") {
		t.Error("c should be a member: both ends published the friendship")
	}
	if !isMember(members, "b") {
		t.Error("our own friend must be a member unconditionally")
	}
}

// A friend of ours that publishes nothing is still a full friend — that edge is
// a local fact from federation_peers, not hearsay. Further out the same silence
// is a dead end, which is the documented "a silent node walls off its friends"
// property arriving where F7 puts it to work.
func TestSilentFriendIsStillAMemberButVouchesForNobody(t *testing.T) {
	peers := []*ExternalNode{{PublicKey: k("a"), TrustState: PeerFriend}}
	// a publishes nothing at all; d names a, but a never names d back.
	edges := []GraphEdgeClaim{edge("d", "a", "")}

	members := MemberKeys(k("me"), peers, edges)
	if !isMember(members, "a") {
		t.Error("a silent direct friend is still a member — we hold that edge ourselves")
	}
	if isMember(members, "d") {
		t.Error("d is a member on its own claim about a silent node")
	}
}

// Blocking is the revocation half of the model: the walk never traverses a
// blocked node, so one block takes the whole branch behind it out of the
// community — the same act that removes it from the map.
func TestBlockingClearsTheBranchBehindIt(t *testing.T) {
	peers := []*ExternalNode{
		{PublicKey: k("a"), TrustState: PeerFriend},
		{PublicKey: k("b"), TrustState: PeerFriend},
	}
	edges := []GraphEdgeClaim{
		edge("b", "c", ""), edge("c", "b", ""), // b and c agree
		edge("c", "d", ""), edge("d", "c", ""), // c and d agree
	}
	if members := MemberKeys(k("me"), peers, edges); !isMember(members, "d") {
		t.Fatalf("d should be a member through b—c—d before the block")
	}

	peers[1].TrustState = PeerBlocked
	members := MemberKeys(k("me"), peers, edges)
	if isMember(members, "b") {
		t.Error("a blocked peer is not a member")
	}
	for _, label := range []string{"c", "d"} {
		if isMember(members, label) {
			t.Errorf("%s survived the block of the only friend that introduced it", label)
		}
	}
	if !isMember(members, "a") {
		t.Error("blocking b must not touch our other friendship")
	}
}

// The other half of the same property: a node vouched for through a second,
// unblocked branch stays. Over-revocation would be as wrong as under-revocation.
func TestASecondBranchKeepsAMember(t *testing.T) {
	peers := []*ExternalNode{
		{PublicKey: k("a"), TrustState: PeerFriend},
		{PublicKey: k("b"), TrustState: PeerBlocked},
	}
	edges := []GraphEdgeClaim{
		edge("b", "c", ""), edge("c", "b", ""), // via the blocked friend
		edge("a", "c", ""), edge("c", "a", ""), // and via a live one
	}

	if members := MemberKeys(k("me"), peers, edges); !isMember(members, "c") {
		t.Error("c should remain a member: a still vouches for it")
	}
}

// Hearsay about our own friendships is not evidence — the F6 forgetting rule,
// which membership inherits by construction. A node we removed cannot re-admit
// itself by publishing that we are friends.
func TestRemovedPeerCannotClaimItselfBackIn(t *testing.T) {
	edges := []GraphEdgeClaim{edge("a", "me", ""), edge("me", "a", "")}

	if members := MemberKeys(k("me"), nil, edges); isMember(members, "a") {
		t.Error("a re-admitted itself by publishing a friendship we ended")
	}
}

// A pending pairing is not a member either. Membership is not a waiting room:
// until an admin accepts, the key earns whatever the community says about it and
// nothing more.
func TestPendingPeerIsNotAMemberOnItsOwn(t *testing.T) {
	peers := []*ExternalNode{{PublicKey: k("a"), TrustState: PeerPendingIncoming}}

	if members := MemberKeys(k("me"), peers, nil); isMember(members, "a") {
		t.Error("a pending peer must not be served as a member")
	}
}
