//go:build !nofederation

package federation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Federation F7 — serving our community over the real mesh
// (docs/architecture/federation-access.md §Principals & access, §The membership rule).
// The claim under test is the phase's whole point: a node that is nobody's
// friend here, but that our community vouches for mutually, is served the
// Madnetwork scope — catalog, bytes and cache alike — while a node outside the
// community is served nothing at all.

// vouchFor makes `subject` a member of the node whose store this is, without any
// friendship to it: an (unreachable) friend row for a vouching node, plus the
// two signed records that would have arrived by gossip naming each other. That
// is the smallest shape the rule accepts — one mutually declared edge behind one
// friend of ours — and it needs no third process.
func vouchFor(t *testing.T, store *memStore, voucherKey, subjectKey string) {
	t.Helper()
	if _, err := store.InsertFederationPeer(context.Background(), &ExternalNode{
		PublicKey: voucherKey, TrustState: PeerFriend, Label: "voucher",
	}); err != nil {
		t.Fatalf("insert vouching friend: %v", err)
	}
	expires := time.Now().Add(time.Hour).Unix()
	store.mu.Lock()
	defer store.mu.Unlock()
	store.graph[voucherKey] = &memRecord{
		seq: 1, expiresAt: expires, edges: []GraphEdge{{Key: subjectKey}},
	}
	store.graph[subjectKey] = &memRecord{
		seq: 1, expiresAt: expires, edges: []GraphEdge{{Key: voucherKey}},
	}
}

// unvouch drops the subject's own record, leaving the voucher's claim standing
// alone — the one-sided edge the map draws and membership refuses.
func unvouch(store *memStore, subjectKey string) {
	store.mu.Lock()
	defer store.mu.Unlock()
	delete(store.graph, subjectKey)
}

// TestMemberIsServedTheMadnetworkScope: B is not A's friend, but A's community
// vouches for it mutually. It reaches everything A publishes to the madnetwork —
// and nothing A restricted to the nodes it friended itself.
func TestMemberIsServedTheMadnetworkScope(t *testing.T) {
	storeA, storeB := newMemStore(), newMemStore()
	plain, _, a, b := scopePair(t, storeA, storeB)
	vouchFor(t, storeA, k("voucher"), b.PublicKeyHex())

	if got := blobStatus(t, a, b, plain); got != http.StatusOK {
		t.Errorf("member fetching a madnetwork-scoped blob = %d, want 200", got)
	}
	if msg := fetchCatalog(t, a, b); len(msg.Entries) != 2 {
		t.Errorf("member's catalog = %d entries, want 2 — a member may discover what it may fetch", len(msg.Entries))
	}

	// Restricted to hand-picked nodes: the one thing membership does not buy.
	storeA.mu.Lock()
	storeA.depths["1"] = DepthFriends
	storeA.mu.Unlock()

	if got := blobStatus(t, a, b, plain); got != http.StatusNotFound {
		t.Errorf("member fetching a Direct-friends blob = %d, want 404", got)
	}
	if msg := fetchCatalog(t, a, b); len(msg.Entries) != 1 || msg.Entries[0].Key != "2" {
		t.Errorf("member's catalog after restricting entry 1 = %+v, want only entry 2", msg.Entries)
	}
}

// TestOneSidedEdgeDoesNotMakeAMember is the perimeter, over the mesh: the same
// setup with the subject's own record removed. One friend's unanswered claim
// about a third party must buy that party nothing, or a single record naming 512
// invented keys would mint 512 authorized fetchers.
func TestOneSidedEdgeDoesNotMakeAMember(t *testing.T) {
	storeA, storeB := newMemStore(), newMemStore()
	plain, _, a, b := scopePair(t, storeA, storeB)
	vouchFor(t, storeA, k("voucher"), b.PublicKeyHex())
	if got := blobStatus(t, a, b, plain); got != http.StatusOK {
		t.Fatalf("member fetch before the edge is one-sided = %d, want 200", got)
	}

	unvouch(storeA, b.PublicKeyHex())
	// The membership memo is refreshed on the sweep; nudge rather than wait it out.
	a.Nudge()
	waitFor(t, "A to stop counting B as a member", func() bool {
		return blobStatus(t, a, b, plain) == http.StatusNotFound
	})

	resp, err := (&http.Client{
		Transport: &http.Transport{DialContext: b.DialContext}, Timeout: meshClientTimeout,
	}).Get(fmt.Sprintf("http://[%s]:%d/madnetwork/v0/catalog", a.Address(), MeshPort))
	if err != nil {
		t.Fatalf("catalog request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("outsider fetching the catalog = %d, want 403", resp.StatusCode)
	}
}

// TestCacheSeedsToTheCommunityNotOutside: the swarm's boundary is the
// madnetwork, so a member re-seeds from our download cache exactly as a direct
// friend does — this REVERSES F5's "cache blobs never leave the friend ring".
// An outsider still gets nothing, even with the guest switch on: a cached blob
// is content we merely hold and can vouch for no license of.
func TestCacheSeedsToTheCommunityNotOutside(t *testing.T) {
	storeA, storeB := newMemStore(), newMemStore()
	cacheDir := t.TempDir()
	body := []byte("a blob A downloaded from somebody else")
	cached := hashOf(body)
	if err := os.WriteFile(filepath.Join(cacheDir, cached), body, 0o644); err != nil {
		t.Fatal(err)
	}
	a, b := startNodePair(t, storeA, storeB,
		[]Option{WithCacheDir(cacheDir), WithIntervals(Intervals{SnapshotTTL: time.Millisecond, MembershipTTL: noMemo})},
		[]Option{WithCacheDir(t.TempDir())})

	// With the guest switch open — the most permissive an outsider can ever be —
	// a cache blob is still refused.
	storeA.mu.Lock()
	storeA.serveGuests = true
	storeA.mu.Unlock()
	if got := blobStatus(t, a, b, cached); got != http.StatusNotFound {
		t.Errorf("outsider fetching a cache blob = %d, want 404 even with guests open", got)
	}

	vouchFor(t, storeA, k("voucher"), b.PublicKeyHex())
	if got := blobStatus(t, a, b, cached); got != http.StatusOK {
		t.Errorf("member fetching a cache blob = %d, want 200 — the swarm's boundary is the community", got)
	}

	// And holdings advertise it to the same set: advertising and serving are one
	// rule, so a member must be told what a member may fetch.
	client := &http.Client{Transport: &http.Transport{DialContext: b.DialContext}, Timeout: meshClientTimeout}
	resp, err := client.Get(fmt.Sprintf("http://[%s]:%d/madnetwork/v0/holdings", a.Address(), MeshPort))
	if err != nil {
		t.Fatalf("holdings request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("member fetching holdings = %d, want 200", resp.StatusCode)
	}
	var msg holdingsMessage
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatalf("decode holdings: %v", err)
	}
	if len(msg.Hashes) != 1 || msg.Hashes[0] != cached {
		t.Errorf("member's holdings = %v, want [%s]", msg.Hashes, cached)
	}
}
