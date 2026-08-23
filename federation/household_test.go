//go:build !nofederation

package federation

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The household (docs/architecture/federation-access.md §"The household") — what a
// listener node can actually do, as opposed to what the access model says about
// it. A madplayer is the shape under test throughout: nobody's friend, in
// nobody's community, and unable to reach anything by a graph walk.
//
// These tests exist because that shape was declared long before it was built,
// and three of its pieces turned out to have no mechanism behind them at all.

// TestListenerNodeServesWhatItsHomeServerVouchesFor pins the third gap, which
// was a missing idea rather than missing code: a peerless node walks
// friend → member → token → guest and fails all four, so it answered every
// requester as an outsider and "seeds back what it fetched" could not happen.
//
// The rule is one sentence — a listener node serves exactly what its home server
// vouches for — and it has two halves, both here: that server, placed by the key
// the device recorded when it signed in, and any bearer of a token that server
// issued, which is the same statement from the same signer, believed in the
// other direction.
func TestListenerNodeServesWhatItsHomeServerVouchesFor(t *testing.T) {
	storeL, storeR := newMemStore(), newMemStore()
	cacheL := t.TempDir()
	content := []byte("bytes the swarm gave us")
	hash := hashOf(content)
	if err := os.WriteFile(filepath.Join(cacheL, hash), content, 0o644); err != nil {
		t.Fatal(err)
	}

	// L is the listener: a cache with one blob in it, no peers, no community, and
	// nothing of its own published. noMemo so a home node recorded mid-test is
	// placed on the next request rather than a minute later.
	l, r := startNodePair(t, storeL, storeR,
		[]Option{WithCacheDir(cacheL), WithIntervals(Intervals{MembershipTTL: noMemo})},
		nil)
	path := "/madnetwork/v0/blob/" + hash

	// Before anything is recorded, R is nobody to L — which is what every node is
	// to a device that has signed in to nothing.
	waitFor(t, "the mesh to converge on a refusal", func() bool {
		code, _ := meshGet(t, l, r, path, "")
		return code == http.StatusNotFound
	})

	// Half one: R is L's home server. One-sided — R has no peer row for L, no
	// edge, and no idea this happened.
	storeL.addHome(r.PublicKeyHex())
	waitFor(t, "L to place its home server", func() bool {
		code, _ := meshGet(t, l, r, path, "")
		return code == http.StatusOK
	})

	// Half two: a token signed by a home server L recorded, presented by a device
	// that is otherwise a stranger. This is one madplayer seeding another, and it
	// is the reason the vouching rule is worth having — the two devices have no
	// relationship at all, only a server they both signed in to.
	homePriv, homeKey := newSigner(t)
	token := mustSign(t, homePriv, homeKey, r.PublicKeyHex(), false, time.Now())
	storeL.forgetHomes()
	storeL.addHome(homeKey)
	waitFor(t, "L to honour its home server's vouch", func() bool {
		code, _ := meshGet(t, l, r, path, token)
		return code == http.StatusOK
	})
	// And R on its own, without the vouch, is back to being nobody: the token is
	// doing the work, not some standing R acquired along the way.
	if code, _ := meshGet(t, l, r, path, ""); code != http.StatusNotFound {
		t.Errorf("unvouched stranger = %d, want 404", code)
	}

	// Signing out stops it, on the next request rather than on a timer — the
	// issuer's standing is re-checked every time, which is what makes the
	// one-hour token lifetime enough (§"The capability token").
	storeL.forgetHomes()
	waitFor(t, "L to stop honouring a home server it forgot", func() bool {
		code, _ := meshGet(t, l, r, path, token)
		return code == http.StatusNotFound
	})
}

// TestSigningInIsHonouredWithoutWaitingOutTheMembershipTTL is the 2026-08-23
// defect over the real mesh: the household is a membership input, the household
// write goes straight to the store, and a memo primed before the sign-in kept
// answering "outsider" for up to a full MembershipTTL — a listener heard it as a
// minute of skipped tracks right after signing in, because the seeder refused
// its home server's perfectly valid tokens. The TTL here is an hour, so the
// assertion is airtight: only the invalidation can make the very next request
// see the home server, and before the fix this test fails.
func TestSigningInIsHonouredWithoutWaitingOutTheMembershipTTL(t *testing.T) {
	storeL, storeR := newMemStore(), newMemStore()
	cacheL := t.TempDir()
	content := []byte("bytes the swarm gave us")
	hash := hashOf(content)
	if err := os.WriteFile(filepath.Join(cacheL, hash), content, 0o644); err != nil {
		t.Fatal(err)
	}

	l, r := startNodePair(t, storeL, storeR,
		[]Option{WithCacheDir(cacheL), WithIntervals(Intervals{MembershipTTL: time.Hour})},
		nil)
	path := "/madnetwork/v0/blob/" + hash

	// Prime the memo: R asks and is refused, so L now holds a computed
	// perimeter that, at this TTL, outlives the whole test.
	waitFor(t, "the mesh to converge on a refusal", func() bool {
		code, _ := meshGet(t, l, r, path, "")
		return code == http.StatusNotFound
	})

	// The sign-in: the store learns, and the node is told its inputs moved —
	// exactly what the app facade's AddHome does (app/network.go).
	storeL.addHome(r.PublicKeyHex())
	l.InvalidateMembers()

	// The VERY NEXT request, not a poll: the rebuild happens in-request.
	if code, _ := meshGet(t, l, r, path, ""); code != http.StatusOK {
		t.Errorf("the first request after signing in = %d, want 200 — the memo "+
			"kept serving the pre-sign-in perimeter", code)
	}

	// And signing out is the same contract in the other direction.
	storeL.forgetHomes()
	l.InvalidateMembers()
	if code, _ := meshGet(t, l, r, path, ""); code != http.StatusNotFound {
		t.Errorf("the first request after signing out = %d, want 404", code)
	}
}

// TestListenerNodeFetchesFromHoldersItWasHanded pins the second of those gaps.
//
// EnsureBlob discovers its holders from this node's own cached catalogs, which
// syncSources fills from friends and members. A node with neither has empty
// tables forever, so the discovery step cannot answer a single hash in the
// network — which is why the holder list has to arrive from outside, from the
// home server whose rows the device is already rendering.
func TestListenerNodeFetchesFromHoldersItWasHanded(t *testing.T) {
	storeA, storeB := newMemStore(), newMemStore()
	cacheB := t.TempDir()

	var mu sync.Mutex
	token := ""
	plain, _, a, b := scopePair(t, storeA, storeB,
		WithCacheDir(cacheB),
		WithCapabilityToken(func() string {
			mu.Lock()
			defer mu.Unlock()
			return token
		}))

	homePriv, homeKey := newSigner(t)
	vouchFor(t, storeA, k("voucher"), homeKey)
	mu.Lock()
	token = mustSign(t, homePriv, homeKey, b.PublicKeyHex(), false, time.Now())
	mu.Unlock()

	ctx := context.Background()
	holder := &BlobProvider{PublicKey: a.PublicKeyHex()}
	waitFor(t, "the mesh to converge and A to place the issuer", func() bool {
		return b.fetchManifest(ctx, holder, plain) != nil
	})

	// B is a listener node, so its own tables know nothing and never will. This
	// is the state every madplayer is permanently in, not a fixture that has not
	// finished setting up.
	if _, err := b.EnsureBlob(ctx, plain); !errors.Is(err, ErrNoHolder) {
		t.Fatalf("EnsureBlob on a node with no catalogs = %v, want ErrNoHolder", err)
	}

	// Handed the holder its home server named, the same fetch runs: same dedupe
	// map, same cache directory, same swarm path, same whole-file verification.
	// Size 0 because the caller may not know it — the manifest does.
	tr, err := b.EnsureBlobFrom(ctx, plain, 0, []*BlobProvider{holder})
	if err != nil {
		t.Fatalf("EnsureBlobFrom: %v", err)
	}
	select {
	case <-tr.Done():
	case <-time.After(meshDeadline):
		t.Fatal("transfer did not finish")
	}
	if err := tr.Err(); err != nil {
		t.Fatalf("transfer failed: %v", err)
	}
	if tr.Size() != 22 {
		t.Errorf("size = %d, want 22 — an unknown size is answered by the manifest", tr.Size())
	}
	got, err := os.ReadFile(filepath.Join(cacheB, plain))
	if err != nil {
		t.Fatalf("cache file: %v", err)
	}
	if string(got) != "ordinary library track" {
		t.Errorf("cached bytes = %q, want the origin's", got)
	}
}
