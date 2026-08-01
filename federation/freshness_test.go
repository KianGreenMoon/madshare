//go:build !nofederation

package federation

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"testing"
	"time"
)

// Freshness hints (F7 item 10, docs/architecture/federation.md §Availability,
// "Two clocks, two windows"). What these pin is the honesty of the mechanism
// rather than its plumbing: a node vouches only for what it pings itself, and it
// believes only claims about its own community, about nodes it already caches.

// nodeKeyN builds a well-formed 64-hex node key from a seed byte, so a test can
// name nodes without generating real ed25519 material.
func nodeKeyN(b byte) string {
	raw := make([]byte, 32)
	for i := range raw {
		raw[i] = b
	}
	return hex.EncodeToString(raw)
}

// TestBuildFreshnessHintsVouchesOnlyForFriends: the reply carries this node's
// first-hand observations and nothing else — not a pending pairing, not a
// blocked node, not the asker's own liveness, and not a friend we have not seen
// within the relay horizon.
func TestBuildFreshnessHintsVouchesOnlyForFriends(t *testing.T) {
	now := time.Now().Unix()
	asker := nodeKeyN(0x01)
	peers := []*Peer{
		{PublicKey: asker, State: PeerFriend, LastSeen: now - 5},
		{PublicKey: nodeKeyN(0x02), State: PeerFriend, LastSeen: now - 30},
		{PublicKey: nodeKeyN(0x03), State: PeerPendingOutgoing, LastSeen: now - 5},
		{PublicKey: nodeKeyN(0x04), State: PeerBlocked, LastSeen: now - 5},
		{PublicKey: nodeKeyN(0x05), State: PeerFriend, LastSeen: 0},
		{PublicKey: nodeKeyN(0x06), State: PeerFriend, LastSeen: now - int64(2*MaxHintAge/time.Second)},
	}
	hints := buildFreshnessHints(peers, asker, now)

	if _, ok := hints[asker]; ok {
		t.Error("a node must not be told how alive it looks to us")
	}
	for _, unwanted := range []struct {
		key, why string
	}{
		{nodeKeyN(0x03), "a pending pairing is not a friendship"},
		{nodeKeyN(0x04), "a blocked node is served nothing, including a mention"},
		{nodeKeyN(0x05), "a friend we have never seen is no observation"},
		{nodeKeyN(0x06), "an observation past MaxHintAge cannot satisfy any window"},
	} {
		if _, ok := hints[unwanted.key]; ok {
			t.Errorf("hint for %s… should be absent: %s", unwanted.key[:6], unwanted.why)
		}
	}
	if age, ok := hints[nodeKeyN(0x02)]; !ok || age != 30 {
		t.Errorf("hint for the seen friend = %d, %v; want age 30", age, ok)
	}
	if len(hints) != 1 {
		t.Errorf("hints = %d entries, want exactly the one friend we saw", len(hints))
	}
}

// TestBuildFreshnessHintsCapKeepsTheFreshest: past the bound the list is cut,
// and what survives is what is worth having — a claim we made a second ago, not
// one from the edge of the horizon.
func TestBuildFreshnessHintsCapKeepsTheFreshest(t *testing.T) {
	const staleAge = 3000
	now := time.Now().Unix()
	var peers []*Peer
	// The 50 stale nodes come FIRST in key order, so a truncation that ignored
	// age would keep exactly the entries worth the least.
	for i := 0; i < MaxFreshnessHints+50; i++ {
		raw := make([]byte, 32)
		raw[0], raw[1] = byte(i>>8), byte(i)
		age := int64(staleAge)
		if i >= 50 {
			age = int64(i - 49)
		}
		peers = append(peers, &Peer{
			PublicKey: hex.EncodeToString(raw),
			State:     PeerFriend,
			LastSeen:  now - age,
		})
	}
	hints := buildFreshnessHints(peers, "", now)
	if len(hints) != MaxFreshnessHints {
		t.Fatalf("hints = %d, want the cap %d", len(hints), MaxFreshnessHints)
	}
	for key, age := range hints {
		if age >= staleAge {
			t.Fatalf("kept %s… at age %ds; the cap should drop the stalest claims first",
				key[:6], age)
		}
	}
}

// TestFreshnessHintsServedToFriendsOnly: the list is our friend list, so it goes
// to friends and only when asked. A member pulling our catalog pings the same
// endpoint on the discovery path and must get the small reply.
func TestFreshnessHintsServedToFriendsOnly(t *testing.T) {
	ms := newMemStore()
	ctx := context.Background()
	friend, stranger := nodeKeyN(0x11), nodeKeyN(0x22)
	if _, err := ms.InsertFederationPeer(ctx, &Peer{PublicKey: friend, State: PeerFriend, LastSeen: time.Now().Unix()}); err != nil {
		t.Fatalf("insert friend: %v", err)
	}
	if _, err := ms.InsertFederationPeer(ctx, &Peer{PublicKey: nodeKeyN(0x33), State: PeerFriend, LastSeen: time.Now().Unix() - 10}); err != nil {
		t.Fatalf("insert second friend: %v", err)
	}
	n := &Node{store: ms, logger: log.New(io.Discard, "", 0)}

	ask := func(key string, query string) map[string]int64 {
		t.Helper()
		addr, err := AddrForKeyHex(key)
		if err != nil {
			t.Fatalf("derive address: %v", err)
		}
		r := httptest.NewRequest("GET", "/madnetwork/v0/ping"+query, nil)
		r.RemoteAddr = fmt.Sprintf("[%s]:40000", addr)
		return n.freshnessHints(r)
	}

	if got := ask(friend, "?hints=1"); len(got) == 0 {
		t.Error("a friend that asked should receive hints")
	}
	if got := ask(friend, ""); got != nil {
		t.Errorf("an unasked ping should stay small, got %d hints", len(got))
	}
	if got := ask(stranger, "?hints=1"); got != nil {
		t.Errorf("a node that is not our friend should receive no hints, got %d", len(got))
	}
}

// TestApplyFreshnessHintsRefusesWhatItCannotVerify: a friend's word moves the
// freshness of a source we already hold and of a node inside our community —
// nothing else. In particular it can never create a source row, because a source
// row means the sweep pulled from that node.
func TestApplyFreshnessHintsRefusesWhatItCannotVerify(t *testing.T) {
	ms := newMemStore()
	ctx := context.Background()
	self := nodeKeyN(0xff)
	member, outsider, uncached := nodeKeyN(0x41), nodeKeyN(0x42), nodeKeyN(0x43)

	memberSrc, err := ms.EnsureCatalogSource(ctx, member, 0)
	if err != nil {
		t.Fatalf("ensure member source: %v", err)
	}
	outsiderSrc, err := ms.EnsureCatalogSource(ctx, outsider, 0)
	if err != nil {
		t.Fatalf("ensure outsider source: %v", err)
	}
	selfSrc, err := ms.EnsureCatalogSource(ctx, self, 0)
	if err != nil {
		t.Fatalf("ensure self source: %v", err)
	}

	n := &Node{
		store:     ms,
		logger:    log.New(io.Discard, "", 0),
		intervals: Intervals{MembershipTTL: time.Minute},
		members: &memberSet{
			self:  self,
			keys:  map[string]struct{}{member: {}, uncached: {}},
			addrs: map[string]string{},
			built: time.Now(),
		},
	}

	now := time.Now().Unix()
	n.applyFreshnessHints(ctx, &Peer{PublicKey: nodeKeyN(0x01)}, map[string]int64{
		member:         20,
		outsider:       1, // in the cache, outside the community: a friend cannot widen the perimeter
		uncached:       1, // in the community, no source row: hearsay must not mint one
		self:           1, // our own liveness is not someone else's observation
		"not-a-key":    1,
		nodeKeyN(0x44): -5,                                  // a clock that ran backwards
		nodeKeyN(0x45): int64(2 * MaxHintAge / time.Second), // past the horizon
	})

	ms.mu.Lock()
	defer ms.mu.Unlock()
	if got := ms.sources[memberSrc.ID]; got.LastSeen < now-25 || got.LastSeen > now {
		t.Errorf("member last_seen = %d, want ≈ now−20 (%d)", got.LastSeen, now-20)
	} else if got.HintedAt < now {
		t.Errorf("member hinted_at = %d, want the receipt time %d", got.HintedAt, now)
	}
	if got := ms.sources[outsiderSrc.ID]; got.LastSeen != 0 || got.HintedAt != 0 {
		t.Errorf("a node outside our community was refreshed: last_seen=%d hinted_at=%d",
			got.LastSeen, got.HintedAt)
	}
	if got := ms.sources[selfSrc.ID]; got.LastSeen != 0 {
		t.Errorf("our own row was refreshed by a friend's claim: last_seen=%d", got.LastSeen)
	}
	for _, s := range ms.sources {
		if s.PublicKey == uncached {
			t.Error("a hint created a source row for a node we never pulled from")
		}
	}
}
