//go:build !nofederation

package federation

// The size cross-check on a sole manifest (federation-swarm.md §"Two manifest
// hardenings", the 2026-08-23 addition; madplayer's skip diagnosis is the
// origin — its lab grew a seeder holding a correct PREFIX of a blob under the
// full hash, whose self-consistent manifest streamed verified-looking bytes
// into playback until the whole-file hash ended the track mid-listen).
//
// The rule: a sole manifest whose total contradicts an advertised size ends
// the transfer at once, with no whole-file fallback — the fallback would fetch
// the same bytes from the same proven-wrong source while a streaming reader
// consumed them. A quorum outranks the advertisement, and an unknown size
// checks nothing; the second test pins that the check keys on KNOWING, because
// a refusal that fires on size 0 would refuse every fetch whose caller simply
// could not name one.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// truncatedHolderPair is a mesh pair where A's cache holds only a PREFIX of a
// blob under its full hash, and A serves B (B is recorded as A's home, the
// cheapest standing that serves the cache). Returned are the two nodes, the
// full blob's hash and its true size.
func truncatedHolderPair(t *testing.T) (a, b *Node, hash string, fullSize int64) {
	t.Helper()
	storeA, storeB := newMemStore(), newMemStore()
	cacheA, cacheB := t.TempDir(), t.TempDir()

	full := []byte("the whole recording, every byte of it, as the catalog knows it")
	hash = hashOf(full)
	if err := os.WriteFile(filepath.Join(cacheA, hash), full[:20], 0o644); err != nil {
		t.Fatal(err)
	}

	a, b = startNodePair(t, storeA, storeB,
		[]Option{WithCacheDir(cacheA)},
		[]Option{WithCacheDir(cacheB)})
	storeA.addHome(b.PublicKeyHex())
	// The store learned after the node's first memo; tell the node, exactly as
	// the app facade's AddHome does (app/network.go).
	a.InvalidateMembers()
	return a, b, hash, int64(len(full))
}

func TestASoleManifestContradictingTheAdvertisedSizeEndsTheFetchBeforeAByte(t *testing.T) {
	a, b, hash, fullSize := truncatedHolderPair(t)
	ctx := context.Background()
	holder := &BlobProvider{PublicKey: a.PublicKeyHex()}

	waitFor(t, "the mesh to converge and A to serve B a manifest", func() bool {
		return b.fetchManifest(ctx, holder, hash) != nil
	})

	tr, err := b.EnsureBlobFrom(ctx, hash, fullSize, []*BlobProvider{holder})
	if err != nil {
		t.Fatalf("EnsureBlobFrom: %v", err)
	}
	select {
	case <-tr.Done():
	case <-time.After(meshDeadline):
		t.Fatal("transfer did not finish")
	}
	terr := tr.Err()
	if terr == nil {
		t.Fatal("a truncated sole copy was fetched as if complete")
	}
	if !strings.Contains(terr.Error(), "advertised") {
		t.Errorf("error = %q, want the size cross-check's sentence naming the advertised size", terr)
	}
	// Before a byte: nothing was ever readable, which is the whole point — the
	// old shape streamed the prefix into a listener's ears first.
	if avail := tr.Available(0); avail != 0 {
		t.Errorf("%d byte(s) were readable from a refused transfer", avail)
	}
}

func TestAnUnknownAdvertisedSizeStillTrustsTheSoleVoice(t *testing.T) {
	a, b, hash, _ := truncatedHolderPair(t)
	ctx := context.Background()
	holder := &BlobProvider{PublicKey: a.PublicKeyHex()}

	waitFor(t, "the mesh to converge and A to serve B a manifest", func() bool {
		return b.fetchManifest(ctx, holder, hash) != nil
	})

	// Size 0 = the caller knew nothing, so the sole voice is believed exactly
	// as before — the fetch runs, and the truncation is caught the old way,
	// by the assembled hash. The error must NOT be the cross-check's: this is
	// the pin that the refusal keys on knowledge.
	tr, err := b.EnsureBlobFrom(ctx, hash, 0, []*BlobProvider{holder})
	if err != nil {
		t.Fatalf("EnsureBlobFrom: %v", err)
	}
	select {
	case <-tr.Done():
	case <-time.After(meshDeadline):
		t.Fatal("transfer did not finish")
	}
	terr := tr.Err()
	if terr == nil {
		t.Fatal("truncated bytes passed the content hash?")
	}
	if strings.Contains(terr.Error(), "advertised") {
		t.Errorf("error = %q — the size cross-check fired with no size known", terr)
	}
}
