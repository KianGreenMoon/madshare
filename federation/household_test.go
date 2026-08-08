//go:build !nofederation

package federation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// The household (docs/architecture/federation.md §"The household") — what a
// listener node can actually do, as opposed to what the access model says about
// it. A madplayer is the shape under test throughout: nobody's friend, in
// nobody's community, and unable to reach anything by a graph walk.
//
// These tests exist because that shape was declared long before it was built,
// and three of its pieces turned out to have no mechanism behind them at all.

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
