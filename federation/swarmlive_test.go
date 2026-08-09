//go:build !nofederation

package federation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// F9 items 1 and 2 over a REAL mesh — genuine yggdrasil cores, a gVisor netstack,
// the actual HTTP-over-mesh path and the real audience gate.
//
// The unit tests around these two items prove the handlers decide correctly. They
// cannot prove that a partial holder is reachable, that the announce survives
// routing and authentication, or that the swarm will actually take bytes from a
// node holding only part of a blob. Those are claims about the network, and the
// stale-holder work is the precedent for measuring rather than asserting them.

// stagePartial writes the first `have` chunks of content into a node's cache as
// an in-flight download, and registers the transfer that makes them advertisable.
// This is what a node in the middle of a fetch looks like: a full-length part
// file that is mostly zero fill, plus a chunk map saying which of it is proven.
func stagePartial(t *testing.T, n *Node, content []byte, have int) string {
	t.Helper()
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])

	if err := os.MkdirAll(n.cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(n.cacheDir, hash+".part")

	size := int64(len(content))
	bulk := chunkSizeFor(size)
	layout := buildLayout(size, bulk, leadSizes(size, bulk))
	if have > layout.count() {
		t.Fatalf("asked to stage %d chunks of a %d-chunk blob", have, layout.count())
	}

	// Full length from the first moment, exactly as fetchSwarm's Truncate leaves
	// it — so anything serving off the file's extent would hand out zero fill.
	buf := make([]byte, size)
	copy(buf[:layout.offsetOf(have)], content[:layout.offsetOf(have)])
	if err := os.WriteFile(part, buf, 0o644); err != nil {
		t.Fatal(err)
	}

	tr := newTransfer(hash, filepath.Join(n.cacheDir, hash), part)
	tr.setMeta(size, "staged.mp3")
	tr.beginChunks(layout, nil)
	for i := 0; i < have; i++ {
		tr.chunkDone(i, layout.offsetOf(i+1))
	}
	n.transferMu.Lock()
	n.transfers[hash] = tr
	n.transferMu.Unlock()
	return hash
}

// One scenario, both items, three nodes — deliberately not two meshes. Starting
// real yggdrasil cores is the expensive part of this file, and an earlier split
// into two tests added five of them to the package, which tipped an unrelated
// gossip test into its timeout under the extra load. It also reads better as one
// story: the partial holder announces itself, the fetcher learns of it, and the
// fetcher then takes bytes from it.
//
// A node that has fetched half a blob is a real source for that half. Before F9
// item 1 it served nothing until it reached 100%, which is why a flash crowd left
// the origin carrying every transfer alone.
func TestPartialHolderAnnouncesItselfAndSeedsIntoALiveSwarm(t *testing.T) {
	const blobSize = 2 << 20 // 2 MiB ⇒ 8 chunks at the 256 KiB bulk size

	content := fillBytes(blobSize)
	hash := hashOf(content)

	originStore, partialStore, fetcherStore := newMemStore(), newMemStore(), newMemStore()
	hashes, resolver := publishBlobs(t, originStore, [][]byte{content})
	if hashes[0] != hash {
		t.Fatalf("staged hash %s, expected %s", hashes[0], hash)
	}

	origin, partial, fetcher := startNodeTrio(t, originStore, partialStore, fetcherStore,
		[]Option{resolver},
		[]Option{WithCacheDir(t.TempDir())},
		[]Option{WithCacheDir(t.TempDir())},
	)
	makeFriends(t, origin, fetcher, originStore, fetcherStore)
	makeFriends(t, partial, fetcher, partialStore, fetcherStore)

	// The partial holder has the first half of the blob and nothing else.
	if got := stagePartial(t, partial, content, 4); got != hash {
		t.Fatalf("staged partial hash %s, expected %s", got, hash)
	}

	ctx := context.Background()

	// ── Item 2: the announce survives real routing and the real audience gate.
	// Nothing was ever "acquired" here, so this also exercises the rule that what
	// we hold PARTIALLY is recomputed live rather than remembered.
	partialPeers, err := partialStore.ListFederationPeers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the partial holder's announce to land on the fetcher", func() bool {
		partial.announceHoldings(ctx, partialPeers)
		srcs, err := fetcherStore.ListCatalogSources(ctx)
		if err != nil {
			return false
		}
		for _, s := range srcs {
			if s.PublicKey != partial.PublicKeyHex() {
				continue
			}
			// last_seen matters as much as the row: EnsureCatalogSource sets only
			// first_seen, so an untouched source is stale on arrival and
			// StaleHolderWindow drops the holder we just learned about.
			if s.LastSeen <= 0 {
				return false
			}
			for _, h := range fetcherStore.holdings[s.ID] {
				if h == hash {
					return true
				}
			}
		}
		return false
	})

	// ── Item 1: the fetcher actually takes bytes off the partial holder.
	plan := []*BlobProvider{
		{PublicKey: origin.PublicKeyHex()},
		{PublicKey: partial.PublicKeyHex()},
	}
	waitFor(t, "the origin to serve a manifest", func() bool {
		return fetcher.fetchManifest(ctx, plan[0], hash) != nil
	})

	fctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	tr, err := fetcher.EnsureBlobFrom(fctx, hash, blobSize, plan)
	if err != nil {
		t.Fatalf("EnsureBlobFrom: %v", err)
	}
	select {
	case <-tr.Done():
	case <-fctx.Done():
		t.Fatal("fetch did not finish")
	}
	if err := tr.Err(); err != nil {
		t.Fatalf("fetch failed: %v", err)
	}

	stats := tr.Stats()
	var fromPartial ProviderStats
	for _, p := range stats.Providers {
		if p.PublicKey == partial.PublicKeyHex() {
			fromPartial = p
		}
	}
	if fromPartial.Bytes == 0 || fromPartial.Chunks == 0 {
		t.Fatalf("the partial holder delivered nothing (%d bytes, %d chunks) — "+
			"a node holding half a blob must be a source for that half; stats: %+v",
			fromPartial.Bytes, fromPartial.Chunks, stats.Providers)
	}
	// It must also not have been thrown out for the chunks it legitimately lacks:
	// a 416 is a fact about the chunk, not a fault of the holder's.
	if fromPartial.Dropped {
		t.Error("the partial holder was retired for not having the chunks it does not have")
	}
}
