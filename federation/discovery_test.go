//go:build !nofederation

package federation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"testing"
	"time"
)

// Federation F7 item 5 — discovery beyond the friend ring
// (docs/architecture/federation.md §Discovery beyond the friend ring). Two
// halves are tested here: the bookkeeping that decides *whom* to pull from and
// what to keep (pure, no mesh), and the property the phase exists for — that a
// node pulls a catalog from a member it never friended (over a real mesh, at the
// bottom of the file).

// discoveryNode builds a Node with a store and no mesh. The client refuses every
// dial, so a round exercises the bookkeeping — whom we try, what we keep — and
// nothing else: what a node answers is the mesh tests' subject, not this file's.
func discoveryNode(store PeerStore, d Discovery) *Node {
	return &Node{
		store:     store,
		logger:    log.New(io.Discard, "", 0),
		discovery: d.withDefaults(defaultDiscovery),
		intervals: defaultIntervals,
		pullNow:   map[string]struct{}{},
		client:    &http.Client{Transport: unreachable{}},
	}
}

// unreachable fails every request the way an unreachable mesh node does.
type unreachable struct{}

func (unreachable) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("no route to this node")
}

// seedSource creates a source row with a chosen last-seen, for the eviction
// order.
func seedSource(t *testing.T, ms *memStore, key string, lastSeen int64) *CatalogSource {
	t.Helper()
	src, err := ms.EnsureCatalogSource(context.Background(), key, 0)
	if err != nil {
		t.Fatalf("ensure source %s: %v", key, err)
	}
	if err := ms.TouchCatalogSourceSeen(context.Background(), src.ID, lastSeen, ""); err != nil {
		t.Fatalf("touch source %s: %v", key, err)
	}
	src.LastSeen = lastSeen
	return src
}

// TestRetainSourcesKeepsWhatWeStillHaveAReasonFor: the admission half of
// retention. A friend and a blocked peer are kept — the first because we pull
// from it, the second because its cache is kept-hidden so an unblock restores
// the view — a member is kept, and a node that is none of those is collected.
//
// This is where §Forgetting reaches the catalog cache: a branch a block or a
// removal cut off stops being a member, so the same walk that un-draws it on the
// map drops its cached library here.
func TestRetainSourcesKeepsWhatWeStillHaveAReasonFor(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	n := discoveryNode(ms, Discovery{})

	friend := seedSource(t, ms, k("friend"), 100)
	blocked := seedSource(t, ms, k("blocked"), 100)
	member := seedSource(t, ms, k("member"), 100)
	pending := seedSource(t, ms, k("pending"), 100)
	stranger := seedSource(t, ms, k("stranger"), 100)

	peers := []*Peer{
		{PublicKey: k("friend"), State: PeerFriend},
		{PublicKey: k("blocked"), State: PeerBlocked},
		{PublicKey: k("pending"), State: PeerPendingIncoming},
	}
	members := &memberSet{keys: map[string]struct{}{k("member"): {}}}

	sources, err := ms.ListCatalogSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kept := n.retainSources(ctx, sources, peers, members)

	keptIDs := map[int64]bool{}
	for _, s := range kept {
		keptIDs[s.ID] = true
	}
	for _, want := range []*CatalogSource{friend, blocked, member} {
		if !keptIDs[want.ID] {
			t.Errorf("source %s was dropped; it should be kept", want.PublicKey)
		}
	}
	for _, gone := range []*CatalogSource{pending, stranger} {
		if keptIDs[gone.ID] {
			t.Errorf("source %s was kept; nothing gives us a reason to cache it", gone.PublicKey)
		}
	}
	// And the drop reached the store, not just the returned slice.
	left, err := ms.ListCatalogSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 3 {
		t.Errorf("store holds %d sources after retention, want 3", len(left))
	}
}

// TestRetainSourcesEvictsTheColdestForeignCatalogs: the cost half. Past the cap
// the least-recently-seen foreign catalogs go, and a node an admin decided
// something about is never the one evicted to make room for a stranger.
func TestRetainSourcesEvictsTheColdestForeignCatalogs(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	n := discoveryNode(ms, Discovery{Cap: 2})

	friend := seedSource(t, ms, k("friend"), 1) // coldest of all, but ours
	warm := seedSource(t, ms, k("warm"), 300)
	mid := seedSource(t, ms, k("mid"), 200)
	cold := seedSource(t, ms, k("cold"), 100)

	peers := []*Peer{{PublicKey: k("friend"), State: PeerFriend}}
	members := &memberSet{keys: map[string]struct{}{
		k("warm"): {}, k("mid"): {}, k("cold"): {},
	}}

	sources, _ := ms.ListCatalogSources(ctx)
	kept := n.retainSources(ctx, sources, peers, members)

	keptIDs := map[int64]bool{}
	for _, s := range kept {
		keptIDs[s.ID] = true
	}
	if !keptIDs[friend.ID] {
		t.Error("the friend was evicted by the foreign-catalog cap; friends are not counted by it")
	}
	if !keptIDs[warm.ID] || !keptIDs[mid.ID] {
		t.Errorf("the two warmest members were not both kept: %v", keptIDs)
	}
	if keptIDs[cold.ID] {
		t.Error("the coldest member survived a cap of 2")
	}
}

// TestFrontierBudgetLimitsMembersButNotFriends: friends are pulled every round
// they are due, members a handful — the trade that keeps the cost of seeing the
// community from growing with it.
func TestFrontierBudgetLimitsMembersButNotFriends(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	n := discoveryNode(ms, Discovery{Budget: 2})

	var peers []*Peer
	memberKeys := map[string]struct{}{}
	for _, name := range []string{"f1", "f2", "f3"} {
		seedSource(t, ms, k(name), 0)
		peers = append(peers, &Peer{PublicKey: k(name), State: PeerFriend})
	}
	for _, name := range []string{"m1", "m2", "m3", "m4", "m5"} {
		seedSource(t, ms, k(name), 0)
		memberKeys[k(name)] = struct{}{}
	}
	n.members = &memberSet{keys: memberKeys, built: time.Now()}

	pulled := n.syncSources(ctx, peers)

	// Every friend was pulled (none are reachable, so this asserts the attempt).
	if len(pulled) != 3 {
		t.Errorf("friends pulled = %d, want all 3 — friends are unbudgeted", len(pulled))
	}
	attempted := 0
	after, _ := ms.ListCatalogSources(ctx)
	for _, s := range after {
		if _, isMember := memberKeys[s.PublicKey]; isMember && s.AttemptedAt > 0 {
			attempted++
		}
	}
	if attempted != 2 {
		t.Errorf("members attempted = %d, want the budget of 2", attempted)
	}

	// The next round takes the *other* members: the rotation orders by last
	// attempt, so nobody starves behind a node that never answers.
	first := attemptedKeys(t, ms, memberKeys)
	n.syncSources(ctx, peers)
	second := attemptedKeys(t, ms, memberKeys)
	if len(second) != 4 {
		t.Errorf("members attempted after two rounds = %d, want 4 (2 per round, rotating)", len(second))
	}
	for key := range first {
		delete(second, key)
	}
	if len(second) != 2 {
		t.Errorf("the second round repeated the first round's members instead of rotating")
	}
}

// TestFrontierReachesMembersWeHoldNothingFrom is the gap a live 5-node chain
// found and the in-process tests had missed: a member we have never pulled from
// has no source row, so a rotation that only walks existing rows can never
// reach it — the frontier would spin forever over the nodes it already knew.
// Creating the row as we try it is what puts a node into the rotation.
func TestFrontierReachesMembersWeHoldNothingFrom(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	n := discoveryNode(ms, Discovery{Budget: 2})

	// Five members and not one source row: the state of a node whose gossip has
	// just converged.
	memberKeys := map[string]struct{}{}
	for _, name := range []string{"m1", "m2", "m3", "m4", "m5"} {
		memberKeys[k(name)] = struct{}{}
	}
	n.members = &memberSet{keys: memberKeys, built: time.Now()}

	n.syncSources(ctx, nil)
	after, err := ms.ListCatalogSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("sources after one round = %d, want the budget of 2 — a member with no row must still be reachable", len(after))
	}

	// And the second round takes two OTHERS: the rows just created carry an
	// attempt, so they fall behind the members still waiting for their turn.
	n.syncSources(ctx, nil)
	if after, _ = ms.ListCatalogSources(ctx); len(after) != 4 {
		t.Errorf("sources after two rounds = %d, want 4", len(after))
	}
}

func attemptedKeys(t *testing.T, ms *memStore, of map[string]struct{}) map[string]struct{} {
	t.Helper()
	out := map[string]struct{}{}
	sources, err := ms.ListCatalogSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sources {
		if _, want := of[s.PublicKey]; want && s.AttemptedAt > 0 {
			out[s.PublicKey] = struct{}{}
		}
	}
	return out
}

// TestPullNowBeatsRotationAndBudget: an admin looking at one node should not
// wait for its turn. The request also creates the source row, so a node we have
// never cached anything from can be asked.
func TestPullNowBeatsRotationAndBudget(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	// Budget -1 is "friends only": with discovery off, the ask is the only pull
	// that may happen, which is what makes the assertion below sharp.
	n := discoveryNode(ms, Discovery{Budget: -1})

	// A real 64-hex key here, not the tests' shorter k(): PullFrom is a boundary,
	// and refusing anything that is not a node key is half of what it does.
	wanted := realKey("wanted")
	if err := n.PullFrom("not a key"); err == nil {
		t.Error("PullFrom accepted something that is not a node key")
	}
	if err := n.PullFrom(k("wanted")); err == nil {
		t.Error("PullFrom accepted a 62-character key; a node key is 64 hex")
	}
	if err := n.PullFrom(wanted); err != nil {
		t.Fatalf("PullFrom: %v", err)
	}
	// A member we did not ask for, to prove the budget still binds around it.
	seedSource(t, ms, k("other"), 0)
	n.members = &memberSet{keys: map[string]struct{}{
		wanted: {}, k("other"): {},
	}, built: time.Now()}

	n.syncSources(ctx, nil)

	got := attemptedKeys(t, ms, map[string]struct{}{wanted: {}, k("other"): {}})
	if _, ok := got[wanted]; !ok {
		t.Error("the explicitly requested node was not pulled")
	}
	if _, ok := got[k("other")]; ok {
		t.Error("a member was pulled although the budget is 0; only the ask should have run")
	}

	// The queue is drained, not remembered: a second round does not re-ask.
	if err := ms.MarkCatalogSourceAttempted(ctx, mustSource(t, ms, wanted).ID, 0); err != nil {
		t.Fatal(err)
	}
	n.syncSources(ctx, nil)
	if got := attemptedKeys(t, ms, map[string]struct{}{wanted: {}}); len(got) != 0 {
		t.Error("the pull-now request survived its round; it should cost exactly one attempt")
	}
}

// realKey builds a full 64-hex node key from a label, for the paths that check
// the shape rather than merely carrying it around.
func realKey(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])
}

func mustSource(t *testing.T, ms *memStore, key string) *CatalogSource {
	t.Helper()
	src, err := ms.EnsureCatalogSource(context.Background(), key, 0)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

// TestPullsFromAMemberItNeverFriended is the phase's acceptance property, over a
// real mesh: B holds A's catalog although the two are not friends, because each
// counts the other a member of its community. Before item 5 the sweep pulled
// from `state = 'friend'` alone, so this was the gap that made items 1–4 a
// one-sided opening — every node served its community and no node could see it.
func TestPullsFromAMemberItNeverFriended(t *testing.T) {
	ctx := context.Background()
	storeA, storeB := newMemStore(), newMemStore()
	storeA.setPublished([]CatalogEntry{
		{Key: "1", RecordingKey: "r1", Title: "Heard Of You", Artist: "Stranger",
			Renditions: []CatalogRendition{{Hash: "hash-member", Size: 5}}},
	})
	fast := WithIntervals(Intervals{
		SnapshotTTL: time.Millisecond, MembershipTTL: noMemo,
		CatalogSync: time.Millisecond, // every sweep is a due round
	})
	a, b := startNodePair(t, storeA, storeB, []Option{fast}, []Option{fast})

	// Mutual membership, no friendship: each side's community vouches for the
	// other. A must count B a member to serve it, B must count A a member to pull.
	vouchFor(t, storeA, k("a-voucher"), b.PublicKeyHex())
	vouchFor(t, storeB, k("b-voucher"), a.PublicKeyHex())

	if _, err := storeB.GetFederationPeerByKey(ctx, a.PublicKeyHex()); err == nil {
		t.Fatal("B has a peer row for A; the point of this test is that it does not")
	}

	// Read-only: creating the source row here would be creating the very thing
	// under test — the sweep has to reach a member it holds nothing from yet.
	waitFor(t, "B to pull a member's catalog", func() bool {
		b.Nudge()
		return len(storeB.cachedFrom(a.PublicKeyHex())) == 1
	})

	// And the cached rows are usable as a swarm tracker: the member is a holder.
	_, holders, err := storeB.MadnetworkBlobProviders(ctx, "hash-member")
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 1 || holders[0].PublicKey != a.PublicKeyHex() {
		t.Fatalf("providers for the member's hash = %+v, want the member itself", holders)
	}
	if holders[0].PeerID != 0 {
		t.Errorf("the holder carries a peer id (%d); we never friended it", holders[0].PeerID)
	}
}
