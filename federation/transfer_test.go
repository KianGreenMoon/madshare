//go:build !nofederation

package federation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"daemonlord.ygg/madshare/config"
)

// startNodePair starts two connected nodes (A listens, B dials) without
// friending them.
func startNodePair(t *testing.T, storeA, storeB *memStore, optsA, optsB []Option) (*Node, *Node) {
	t.Helper()
	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	underlay := fmt.Sprintf("tcp://%s", probe.Addr())
	probe.Close()

	a, err := Start(config.FederationConfig{
		Name: "node-a", KeyFile: filepath.Join(dir, "a.key"), Listen: []string{underlay},
	}, storeA, logger, optsA...)
	if err != nil {
		t.Fatalf("start node A: %v", err)
	}
	t.Cleanup(a.Stop)

	b, err := Start(config.FederationConfig{
		Name: "node-b", KeyFile: filepath.Join(dir, "b.key"), Peers: []string{underlay},
	}, storeB, logger, optsB...)
	if err != nil {
		t.Fatalf("start node B: %v", err)
	}
	t.Cleanup(b.Stop)
	return a, b
}

// makeFriends walks the pairing handshake until both sides are friends.
func makeFriends(t *testing.T, a, b *Node, storeA, storeB *memStore) {
	t.Helper()
	ctx := context.Background()
	if _, err := b.ImportCard(ctx, a.Info().Card); err != nil {
		t.Fatalf("import card on B: %v", err)
	}
	var incomingID int64
	waitFor(t, "A to see B's pairing request", func() bool {
		b.Nudge()
		p, err := storeA.GetFederationPeerByKey(ctx, b.PublicKeyHex())
		if err != nil {
			return false
		}
		incomingID = p.ID
		return p.State == PeerPendingIncoming
	})
	if err := a.AcceptPeer(ctx, incomingID); err != nil {
		t.Fatalf("accept on A: %v", err)
	}
	waitFor(t, "the friendship to converge on B", func() bool {
		b.Nudge()
		p, err := storeB.GetFederationPeerByKey(ctx, a.PublicKeyHex())
		return err == nil && p.State == PeerFriend
	})
}

// seedBlobCatalog gives B a cached catalog row for A advertising hash, so
// EnsureBlob finds a provider without waiting out a catalog sync. Addressed by
// node key: a cached catalog hangs off a source row, and A need not be a peer at
// all for B to hold one (F7 item 5).
func seedBlobCatalog(t *testing.T, storeB *memStore, a *Node, hash string, size int64) {
	t.Helper()
	ctx := context.Background()
	src, err := storeB.EnsureCatalogSource(ctx, a.PublicKeyHex(), time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	err = storeB.ReplaceSourceCatalog(ctx, src.ID, "s1", time.Now().Unix(), []CatalogEntry{{
		Key: "1", RecordingKey: "r1", Title: "Blob Song",
		Renditions: []CatalogRendition{{Hash: hash, Size: size, Codec: "mp3"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
}

// TestBlobTransfer walks the F3 direct-transfer flow: the blob is refused
// before friendship, then fetched, verified, and cached once the nodes are
// friends — including the origin filename via Content-Disposition and the
// cache-hit path on a second EnsureBlob.
func TestBlobTransfer(t *testing.T) {
	content := []byte("madnetwork direct transfer test payload — the bytes must survive verbatim")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])

	// Node A serves the blob from a temp "library".
	blobDir := t.TempDir()
	blobPath := filepath.Join(blobDir, "blob song.mp3")
	if err := os.WriteFile(blobPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	storeA, storeB := newMemStore(), newMemStore()
	storeA.setPublished([]CatalogEntry{{
		Key: "1", RecordingKey: "r1", Title: "Blob Song",
		Renditions: []CatalogRendition{{Hash: hash, Size: int64(len(content)), Codec: "mp3"}},
	}})
	resolveA := func(h string) (string, bool) {
		if h == hash {
			return blobPath, true
		}
		return "", false
	}
	cacheB := t.TempDir()

	a, b := startNodePair(t, storeA, storeB,
		[]Option{WithBlobResolver(resolveA)},
		[]Option{WithCacheDir(cacheB)})

	ctx := context.Background()
	blobURL := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/blob/%s", a.Address(), MeshPort, hash)
	client := &http.Client{Transport: &http.Transport{DialContext: b.DialContext}, Timeout: meshClientTimeout}

	// Pre-friendship: default-deny. Since F5 a non-friend is answered as the
	// open swarm's guest audience, so a blob that is not guest-playable is 404
	// (nothing to find) rather than 403 (which would confirm the hash exists).
	waitFor(t, "pre-friendship blob refusal", func() bool {
		resp, err := client.Get(blobURL)
		if err != nil {
			return false // mesh converging
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("pre-friendship blob = %d, want 404", resp.StatusCode)
		}
		return true
	})

	makeFriends(t, a, b, storeA, storeB)
	seedBlobCatalog(t, storeB, a, hash, int64(len(content)))

	tr, err := b.EnsureBlob(ctx, hash)
	if err != nil {
		t.Fatalf("EnsureBlob: %v", err)
	}
	select {
	case <-tr.Done():
	case <-time.After(meshDeadline):
		t.Fatal("transfer did not finish")
	}
	if err := tr.Err(); err != nil {
		t.Fatalf("transfer failed: %v", err)
	}
	if tr.Filename() != "blob song.mp3" {
		t.Errorf("origin filename = %q, want the served base name", tr.Filename())
	}
	if tr.Size() != int64(len(content)) {
		t.Errorf("size = %d, want %d", tr.Size(), len(content))
	}
	got, err := os.ReadFile(filepath.Join(cacheB, hash))
	if err != nil {
		t.Fatalf("cache file: %v", err)
	}
	if string(got) != string(content) {
		t.Error("cached bytes differ from the origin")
	}

	// The stats seam (T1) describes the fetch that just happened: which path it
	// took, who carried the bytes, when the first one became readable.
	st := tr.Stats()
	if st.Mode != "swarm" && st.Mode != "whole" {
		t.Errorf("stats mode = %q, want the swarm or whole-file path", st.Mode)
	}
	if st.FirstByte <= 0 || st.Elapsed <= 0 {
		t.Errorf("stats timing = first byte %v, elapsed %v — both should be set", st.FirstByte, st.Elapsed)
	}
	if len(st.Providers) != 1 || st.Providers[0].PublicKey != a.PublicKeyHex() {
		t.Fatalf("stats providers = %+v, want only node A", st.Providers)
	}
	if st.Providers[0].Bytes != int64(len(content)) || st.Providers[0].Failures != 0 {
		t.Errorf("A's contribution = %+v, want all %d bytes and no failures", st.Providers[0], len(content))
	}
	if st.Failovers != 0 || st.Corrupt != 0 {
		t.Errorf("clean single-holder fetch reported failovers=%d corrupt=%d", st.Failovers, st.Corrupt)
	}

	// Second EnsureBlob is a cache hit: complete immediately, same bytes.
	tr2, err := b.EnsureBlob(ctx, hash)
	if err != nil {
		t.Fatalf("EnsureBlob (cached): %v", err)
	}
	select {
	case <-tr2.Done():
	default:
		t.Error("cached EnsureBlob not complete immediately")
	}
	if m := tr2.Stats().Mode; m != "local" {
		t.Errorf("cache-hit stats mode = %q, want %q", m, "local")
	}

	// A published-but-unadvertised hash has no provider.
	other := sha256.Sum256([]byte("no one holds this"))
	if _, err := b.EnsureBlob(ctx, hex.EncodeToString(other[:])); err != ErrNoHolder {
		t.Errorf("unknown hash err = %v, want ErrNoHolder", err)
	}
}

// TestBlobTransfer_VerificationFailure: an origin serving bytes that do not
// match the requested hash must fail the transfer (never land in the cache).
func TestBlobTransfer_VerificationFailure(t *testing.T) {
	content := []byte("the true bytes")
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])

	blobDir := t.TempDir()
	liar := filepath.Join(blobDir, "liar.mp3")
	if err := os.WriteFile(liar, []byte("entirely different bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	storeA, storeB := newMemStore(), newMemStore()
	storeA.setPublished([]CatalogEntry{{
		Key: "1", RecordingKey: "r1", Title: "Liar Song",
		Renditions: []CatalogRendition{{Hash: hash, Size: int64(len(content)), Codec: "mp3"}},
	}})
	cacheB := t.TempDir()
	a, b := startNodePair(t, storeA, storeB,
		[]Option{WithBlobResolver(func(h string) (string, bool) { return liar, h == hash })},
		[]Option{WithCacheDir(cacheB)})
	makeFriends(t, a, b, storeA, storeB)
	seedBlobCatalog(t, storeB, a, hash, int64(len(content)))

	tr, err := b.EnsureBlob(context.Background(), hash)
	if err != nil {
		t.Fatalf("EnsureBlob: %v", err)
	}
	select {
	case <-tr.Done():
	case <-time.After(meshDeadline):
		t.Fatal("transfer did not finish")
	}
	if tr.Err() == nil {
		t.Fatal("mismatching bytes verified successfully — want an error")
	}
	if _, err := os.Stat(filepath.Join(cacheB, hash)); !os.IsNotExist(err) {
		t.Error("unverified bytes landed in the cache")
	}
}

// TestEvictCachedBlobDropsTheDuplicate pins the fix for the scope leak found by
// the F8 mesh verification (.issues/open-issues.md). It also pins what eviction
// must NOT touch: an in-flight transfer's `.part`, and a hash never cached.
func TestEvictCachedBlobDropsTheDuplicate(t *testing.T) {
	dir := t.TempDir()
	n := &Node{cacheDir: dir}

	hash := strings.Repeat("ab", 32)
	final := filepath.Join(dir, hash)
	part := final + ".part"
	for _, p := range []string{final, part} {
		if err := os.WriteFile(p, []byte("bytes"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	if err := n.EvictCachedBlob(hash); err != nil {
		t.Fatalf("EvictCachedBlob: %v", err)
	}
	if _, err := os.Stat(final); !os.IsNotExist(err) {
		t.Error("the cached duplicate survived eviction")
	}
	if _, err := os.Stat(part); err != nil {
		t.Error("evicting the cached blob also took an in-flight transfer's .part")
	}

	// Idempotent, tolerant of nonsense, and a no-op without a cache dir — all
	// three matter because every caller ignores the error.
	if err := n.EvictCachedBlob(hash); err != nil {
		t.Errorf("second eviction: %v", err)
	}
	if err := n.EvictCachedBlob("not-a-hash"); err != nil {
		t.Errorf("non-hash: %v", err)
	}
	if err := (&Node{}).EvictCachedBlob(hash); err != nil {
		t.Errorf("no cache dir configured: %v", err)
	}
}
