//go:build !nofederation

package federation

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// Federation F5 — sharing scope over the real mesh (docs/architecture/
// federation.md §Sharing scope). The claim these tests defend is that catalog
// and bytes read ONE rule: whatever an audience is not shown, it also cannot
// fetch, and whatever a stranger may fetch, a stranger may fetch without being
// anyone's friend.

// scopePair publishes two blobs on node A — one ordinary, one guest-playable —
// and returns their hashes plus a started, mesh-connected node pair. The
// entries' keys are "1" (ordinary) and "2" (guest), which the memStore's depths
// map is keyed by.
func scopePair(t *testing.T, storeA, storeB *memStore) (plain, guest string, a, b *Node) {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, body []byte) (string, string) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
		return hashOf(body), path
	}
	plainHash, plainPath := write("plain.mp3", []byte("ordinary library track"))
	guestHash, guestPath := write("guest.mp3", []byte("guest-playable track"))

	storeA.setPublished([]CatalogEntry{
		{Key: "1", RecordingKey: "r1", Title: "Members Only",
			Renditions: []CatalogRendition{{Hash: plainHash, Size: 22, Codec: "mp3"}}},
		{Key: "2", RecordingKey: "r2", Title: "Open Door", GuestPlayable: true,
			Renditions: []CatalogRendition{{Hash: guestHash, Size: 20, Codec: "mp3"}}},
	})
	resolve := WithBlobResolver(func(h string) (string, bool) {
		switch h {
		case plainHash:
			return plainPath, true
		case guestHash:
			return guestPath, true
		}
		return "", false
	})
	// Node A memoizes its catalog per audience; these tests change the store
	// mid-flight, so shorten the memo rather than reaching past it — the same
	// seam the mesh lab uses (docs/plans/mesh-testing.md T1).
	a, b = startNodePair(t, storeA, storeB,
		[]Option{resolve, WithIntervals(Intervals{SnapshotTTL: noMemo, MembershipTTL: noMemo})},
		[]Option{WithCacheDir(t.TempDir())})
	return plainHash, guestHash, a, b
}

// blobStatus fetches a blob from A over the mesh and reports the status code.
func blobStatus(t *testing.T, a, b *Node, hash string) int {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DialContext: b.DialContext}, Timeout: meshClientTimeout}
	var code int
	waitFor(t, "blob fetch reaches node A", func() bool {
		resp, err := client.Get(fmt.Sprintf("http://[%s]:%d/madnetwork/v0/blob/%s", a.Address(), MeshPort, hash))
		if err != nil {
			return false // mesh converging
		}
		defer resp.Body.Close()
		code = resp.StatusCode
		return true
	})
	return code
}

// fetchCatalog pulls A's catalog as B sees it.
func fetchCatalog(t *testing.T, a, b *Node) catalogMessage {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DialContext: b.DialContext}, Timeout: meshClientTimeout}
	var msg catalogMessage
	waitFor(t, "catalog reaches node A", func() bool {
		resp, err := client.Get(fmt.Sprintf("http://[%s]:%d/madnetwork/v0/catalog", a.Address(), MeshPort))
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
			return false
		}
		defer resp.Body.Close()
		return json.NewDecoder(resp.Body).Decode(&msg) == nil
	})
	return msg
}

// TestOutsiderIsServedNothingByDefault: a node outside our community gets 404 on
// every blob, guest-playable or not (F7). This is the posture — everything to our
// community, nothing outside it — and it reverses F5, where the guest-open swarm
// was always on.
func TestOutsiderIsServedNothingByDefault(t *testing.T) {
	storeA, storeB := newMemStore(), newMemStore()
	plain, guest, a, b := scopePair(t, storeA, storeB)

	if got := blobStatus(t, a, b, guest); got != http.StatusNotFound {
		t.Errorf("outsider fetching a guest-playable blob = %d, want 404 (guests off)", got)
	}
	if got := blobStatus(t, a, b, plain); got != http.StatusNotFound {
		t.Errorf("outsider fetching an ordinary blob = %d, want 404", got)
	}
}

// TestGuestSwarmWhenOpenedIsGuestPlayableOnly: with the node's guest switch on,
// an outsider reaches guest-playable content and only that. It still gets no
// catalog, so it must already know the hash.
func TestGuestSwarmWhenOpenedIsGuestPlayableOnly(t *testing.T) {
	storeA, storeB := newMemStore(), newMemStore()
	plain, guest, a, b := scopePair(t, storeA, storeB)
	storeA.mu.Lock()
	storeA.serveGuests = true
	storeA.mu.Unlock()

	if got := blobStatus(t, a, b, guest); got != http.StatusOK {
		t.Errorf("stranger fetching a guest-playable blob = %d, want 200 (guests opened)", got)
	}
	if got := blobStatus(t, a, b, plain); got != http.StatusNotFound {
		t.Errorf("stranger fetching an ordinary blob = %d, want 404", got)
	}

	// The catalog stays friends-only: openness is per content at the byte
	// endpoints, never a listing of the library.
	client := &http.Client{Transport: &http.Transport{DialContext: b.DialContext}, Timeout: meshClientTimeout}
	resp, err := client.Get(fmt.Sprintf("http://[%s]:%d/madnetwork/v0/catalog", a.Address(), MeshPort))
	if err != nil {
		t.Fatalf("catalog request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("stranger fetching the catalog = %d, want 403", resp.StatusCode)
	}
}

// TestPrivateDepthHidesFromFriends: a recording marked private (DepthPrivate)
// leaves the friend's catalog AND stops serving its bytes — even though the
// requester is a full friend and the blob is published locally.
func TestPrivateDepthHidesFromFriends(t *testing.T) {
	storeA, storeB := newMemStore(), newMemStore()
	plain, _, a, b := scopePair(t, storeA, storeB)
	makeFriends(t, a, b, storeA, storeB)

	if got := blobStatus(t, a, b, plain); got != http.StatusOK {
		t.Fatalf("friend fetching a published blob = %d, want 200", got)
	}
	if msg := fetchCatalog(t, a, b); len(msg.Entries) != 2 {
		t.Fatalf("friend's catalog = %d entries, want 2", len(msg.Entries))
	}

	// Mark entry 1 private. Both halves must change together.
	storeA.mu.Lock()
	storeA.depths["1"] = DepthPrivate
	storeA.mu.Unlock()

	if got := blobStatus(t, a, b, plain); got != http.StatusNotFound {
		t.Errorf("friend fetching a private blob = %d, want 404", got)
	}
	msg := fetchCatalog(t, a, b)
	if len(msg.Entries) != 1 || msg.Entries[0].Key != "2" {
		t.Errorf("friend's catalog after going private = %+v, want only entry 2", msg.Entries)
	}
}

// TestGuestOnlyFriendSeesGuestContentOnly: a friend mapped to a local account
// without content.access is a guest-only audience — the same catalog and the
// same bytes an anonymous local visitor gets, no more.
func TestGuestOnlyFriendSeesGuestContentOnly(t *testing.T) {
	storeA, storeB := newMemStore(), newMemStore()
	plain, guest, a, b := scopePair(t, storeA, storeB)
	makeFriends(t, a, b, storeA, storeB)

	// Restrict B on A's side, the way a user mapping to a content.access-less
	// account does.
	p, err := storeA.GetFederationPeerByKey(t.Context(), b.PublicKeyHex())
	if err != nil {
		t.Fatalf("look up peer B on A: %v", err)
	}
	storeA.mu.Lock()
	storeA.audiences[p.ID] = Audience{Class: ClassFriend, Distance: DepthFriends, GuestOnly: true}
	storeA.mu.Unlock()

	msg := fetchCatalog(t, a, b)
	if len(msg.Entries) != 1 || msg.Entries[0].Key != "2" {
		t.Errorf("guest-only friend's catalog = %+v, want only the guest-playable entry", msg.Entries)
	}
	if got := blobStatus(t, a, b, guest); got != http.StatusOK {
		t.Errorf("guest-only friend fetching the guest blob = %d, want 200", got)
	}
	if got := blobStatus(t, a, b, plain); got != http.StatusNotFound {
		t.Errorf("guest-only friend fetching an ordinary blob = %d, want 404", got)
	}

	// Holdings advertise the download cache, which a guest-only audience is
	// never served — so it must not be advertised either.
	client := &http.Client{Transport: &http.Transport{DialContext: b.DialContext}, Timeout: meshClientTimeout}
	resp, err := client.Get(fmt.Sprintf("http://[%s]:%d/madnetwork/v0/holdings", a.Address(), MeshPort))
	if err != nil {
		t.Fatalf("holdings request: %v", err)
	}
	defer resp.Body.Close()
	var hm holdingsMessage
	if err := json.NewDecoder(resp.Body).Decode(&hm); err != nil {
		t.Fatalf("decode holdings: %v", err)
	}
	if len(hm.Hashes) != 0 {
		t.Errorf("guest-only friend's holdings = %v, want empty", hm.Hashes)
	}
}

// TestAudienceSnapshotsAreSeparate: two audiences get two catalogs and two
// serials from the same node. A shared serial would make a restricted friend's
// next not-modified check answer about a catalog it never received.
func TestAudienceSnapshotsAreSeparate(t *testing.T) {
	storeA, storeB := newMemStore(), newMemStore()
	_, _, a, _ := scopePair(t, storeA, storeB)

	full, err := a.ownSnapshot(t.Context(), FriendAudience)
	if err != nil {
		t.Fatalf("full snapshot: %v", err)
	}
	limited, err := a.ownSnapshot(t.Context(), GuestAudience)
	if err != nil {
		t.Fatalf("guest snapshot: %v", err)
	}
	if len(full.entries) != 2 || len(limited.entries) != 1 {
		t.Fatalf("entries: full=%d guest=%d, want 2 and 1", len(full.entries), len(limited.entries))
	}
	if full.serial == limited.serial {
		t.Error("the two audiences share a serial — a restricted friend would be told the full catalog is unchanged")
	}
	// Each memo is independent: re-asking gives the same object back.
	if again, _ := a.ownSnapshot(t.Context(), FriendAudience); again.serial != full.serial {
		t.Error("the full snapshot was not memoized per audience")
	}
}
