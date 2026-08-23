//go:build !nofederation

package federation

// The size cross-check (federation-swarm.md §"Two manifest hardenings", the
// 2026-08-23 addition; madplayer's skip diagnosis is the origin — its lab grew
// a seeder holding a correct PREFIX of a blob under the full hash, whose
// self-consistent manifest streamed verified-looking bytes into playback until
// the whole-file hash ended the track mid-listen).
//
// The rule: where a copy's size claim contradicts the one the caller arrived
// with, the fetch runs with reads WITHHELD until the content hash decides. Not
// refused — the advertisement is a catalog row a member published, so it can
// contradict but never convict, and refusing would let one stale row deny the
// blob outright. Four tests, one per half of that sentence: the truncated copy
// reaches no reader; a wrong advertisement costs streaming and not the blob; an
// unknown size checks nothing; and the whole-file path carries the same check
// itself, which is where a truncated holder serving no manifest lands.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// blobHolderPair is a mesh pair where A's cache holds `keep` bytes of a blob
// under the FULL blob's hash, and A serves B (B is recorded as A's home, the
// cheapest standing that serves the cache). keep < 0 means the whole blob.
// Returned are the two nodes, the full blob's hash and its true size.
func blobHolderPair(t *testing.T, keep int) (a, b *Node, hash string, fullSize int64) {
	t.Helper()
	storeA, storeB := newMemStore(), newMemStore()
	cacheA, cacheB := t.TempDir(), t.TempDir()

	full := []byte("the whole recording, every byte of it, as the catalog knows it")
	hash = hashOf(full)
	held := full
	if keep >= 0 {
		held = full[:keep]
	}
	if err := os.WriteFile(filepath.Join(cacheA, hash), held, 0o644); err != nil {
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

// holderOf is the provider a fetch dials, once the pair has converged far
// enough for it to answer.
func holderOf(t *testing.T, from, to *Node, hash string) *BlobProvider {
	t.Helper()
	p := &BlobProvider{PublicKey: from.PublicKeyHex()}
	waitFor(t, "the mesh to converge and the holder to serve a manifest", func() bool {
		return to.fetchManifest(context.Background(), p, hash) != nil
	})
	return p
}

// awaitFetch runs a fetch of hash against a holder plan and returns the finished
// transfer. The concrete type, not the interface: these tests assert on the
// withholding flag, which no caller outside the package has business reading.
func awaitFetch(t *testing.T, to *Node, hash string, size int64, plan ...*BlobProvider) *transfer {
	t.Helper()
	tr, err := to.EnsureBlobFrom(context.Background(), hash, size, plan)
	if err != nil {
		t.Fatalf("EnsureBlobFrom: %v", err)
	}
	select {
	case <-tr.Done():
	case <-time.After(meshDeadline):
		t.Fatal("transfer did not finish")
	}
	return tr.(*transfer)
}

// withheld reports the flag under the lock. It is the deterministic pin on "no
// byte reached a reader": a failed transfer resets its progress on the way out,
// so asking Available after Done answers 0 whether or not the rule ever fired.
func withheld(tr *transfer) bool {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.withheld
}

func TestASoleTruncatedHolderNeverReachesTheReader(t *testing.T) {
	a, b, hash, fullSize := blobHolderPair(t, 20)
	tr := awaitFetch(t, b, hash, fullSize, holderOf(t, a, b, hash))

	if tr.Err() == nil {
		t.Fatal("a truncated sole copy was fetched as if complete")
	}
	if !withheld(tr) {
		t.Error("the only copy offered 20 bytes where 62 were advertised and reads were not withheld")
	}
	if avail := tr.Available(0); avail != 0 {
		t.Errorf("%d byte(s) readable from a contested transfer", avail)
	}
}

func TestAWrongAdvertisedSizeCostsStreamingNotTheBlob(t *testing.T) {
	a, b, hash, fullSize := blobHolderPair(t, -1) // A holds the WHOLE blob
	// The advertisement is the wrong record this time — a stale catalog row, or
	// a hostile one. It must cost the fetch its streaming and nothing else: a
	// refusal here would be one member's row denying a blob community-wide,
	// with no fallback and nothing to ride out.
	tr := awaitFetch(t, b, hash, fullSize+7, holderOf(t, a, b, hash))

	if err := tr.Err(); err != nil {
		t.Fatalf("an honest copy was refused over a wrong advertisement: %v", err)
	}
	if !withheld(tr) {
		t.Error("the sizes disagreed and reads were not withheld")
	}
	// Withheld through the fetch, whole once the hash spoke.
	if avail := tr.Available(0); avail != fullSize {
		t.Errorf("Available(0) = %d, want the whole blob (%d) once verified", avail, fullSize)
	}
	if info, err := os.Stat(tr.path); err != nil || info.Size() != fullSize {
		t.Errorf("cache file: %v (size %v), want the blob at %d bytes", err, info, fullSize)
	}
}

func TestAnUnknownAdvertisedSizeStillTrustsTheSoleVoice(t *testing.T) {
	a, b, hash, _ := blobHolderPair(t, 20)
	// Size 0 = the caller knew nothing, so there is no second record to
	// contradict and nothing is withheld: the sole voice is believed exactly as
	// before, and the truncation is caught the old way, by the assembled hash.
	// This is the pin that the rule keys on KNOWING — one that fired on size 0
	// would withhold every fetch whose caller simply could not name a size.
	tr := awaitFetch(t, b, hash, 0, holderOf(t, a, b, hash))

	if tr.Err() == nil {
		t.Fatal("truncated bytes passed the content hash?")
	}
	if !strings.Contains(tr.Err().Error(), "not the requested content hash") {
		t.Errorf("error = %q, want the content hash's own refusal", tr.Err())
	}
	if withheld(tr) {
		t.Error("reads were withheld with no advertised size to contradict")
	}
}

func TestTheWholeFilePathWithholdsAContestedHolder(t *testing.T) {
	a, b, hash, fullSize := blobHolderPair(t, 20)
	p := holderOf(t, a, b, hash)

	// Straight into fetchFrom, which is where a truncated holder that serves no
	// manifest lands — the manifest cross-check never runs for it, and this is
	// the path that publishes every byte as it arrives and verifies only at the
	// end. Called directly because runWhole resets progress between holders,
	// which would hide exactly what is being measured.
	dir := t.TempDir()
	tr := newTransfer(hash, filepath.Join(dir, hash), filepath.Join(dir, hash+".part"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr.ctx, tr.cancel = ctx, cancel
	tr.size, tr.advertised = fullSize, fullSize

	if err := b.fetchFrom(tr, p); err == nil {
		t.Fatal("a truncated copy passed the content hash?")
	}
	if !withheld(tr) {
		t.Error("Content-Length contradicted the advertised size and reads were not withheld")
	}
	// Deterministic: fetchFrom wrote 20 bytes through addProgress, so without
	// withholding this reads 20 — the prefix that used to reach a listener.
	if avail := tr.Available(0); avail != 0 {
		t.Errorf("%d byte(s) readable from a contested whole-file fetch", avail)
	}
}

func TestAnHonestHolderOutsideTheProbeWaveStillDelivers(t *testing.T) {
	full := []byte("the whole recording, every byte of it, as the catalog knows it")
	hash := hashOf(full)

	liarStore, honestStore, fetcherStore := newMemStore(), newMemStore(), newMemStore()
	liarCache, honestCache := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(liarCache, hash), full[:20], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(honestCache, hash), full, 0o644); err != nil {
		t.Fatal(err)
	}

	liar, honest, fetcher := startNodeTrio(t, liarStore, honestStore, fetcherStore,
		[]Option{WithCacheDir(liarCache)},
		[]Option{WithCacheDir(honestCache)},
		// The padding keys below are unreachable by construction; without this
		// each of them costs the default five-second dial, twice over.
		[]Option{WithCacheDir(t.TempDir()), WithTimeouts(Timeouts{Connect: 500 * time.Millisecond})},
	)
	liarStore.addHome(fetcher.PublicKeyHex())
	honestStore.addHome(fetcher.PublicKeyHex())
	liar.InvalidateMembers()
	honest.InvalidateMembers()

	ctx := context.Background()
	waitFor(t, "both holders to serve the fetcher a manifest", func() bool {
		return fetcher.fetchManifest(ctx, &BlobProvider{PublicKey: liar.PublicKeyHex()}, hash) != nil &&
			fetcher.fetchManifest(ctx, &BlobProvider{PublicKey: honest.PublicKeyHex()}, hash) != nil
	})

	// The liar first, then enough unreachable keys to fill the first probe wave,
	// so the honest holder sits outside it and is asked for no manifest at all.
	plan := []*BlobProvider{{PublicKey: liar.PublicKeyHex()}}
	for i := 1; i < manifestProbes; i++ {
		plan = append(plan, &BlobProvider{PublicKey: strings.Repeat(string(rune('a'+i)), 64)})
	}
	plan = append(plan, &BlobProvider{PublicKey: honest.PublicKeyHex()})

	tr := awaitFetch(t, fetcher, hash, int64(len(full)), plan...)

	if err := tr.Err(); err != nil {
		t.Fatalf("the fetch died on the liar's word with a complete copy in the plan: %v", err)
	}
	if !withheld(tr) {
		t.Error("the sole manifest contradicted the advertised size and reads were not withheld")
	}
	if avail := tr.Available(0); avail != int64(len(full)) {
		t.Errorf("Available(0) = %d, want the whole blob (%d) once verified", avail, len(full))
	}
	if got, err := os.ReadFile(tr.path); err != nil || string(got) != string(full) {
		t.Errorf("cache file: %v, want the blob verbatim", err)
	}
}
