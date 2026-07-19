//go:build !nofederation

package federation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"daemonlord.ygg/madshare/config"
)

// fillBytes returns n deterministic, chunk-distinct bytes (so per-chunk hashes
// differ and a multi-chunk transfer is a real test).
func fillBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i*2654435761 + i/251) & 0xff)
	}
	return b
}

func hashOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestChunkSizeForAdaptive pins the adaptive-sizing contract: small files get
// the floor, mid-size files scale, huge files clamp to the ceiling, and the
// chunk count always covers the whole file.
func TestChunkSizeForAdaptive(t *testing.T) {
	cases := []struct{ size, want int64 }{
		{0, minChunkSize},
		{100 << 10, minChunkSize},    // below the floor
		{minChunkSize, minChunkSize}, // exactly the floor
		{6 << 20, 512 << 10},         // 6 MiB / 12 ≈ 512 KiB
		{15 << 20, maxChunkSize},     // 15 MiB / 12 ≈ 1.3 MiB → capped at the 1 MiB ceiling
		{100 << 20, maxChunkSize},    // clamps to the ceiling
	}
	for _, c := range cases {
		got := chunkSizeFor(c.size)
		if got != c.want {
			t.Errorf("chunkSizeFor(%d) = %d, want %d", c.size, got, c.want)
		}
		if c.size > 0 {
			nc := chunkCount(c.size, got)
			if int64(nc)*got < c.size || int64(nc-1)*got >= c.size {
				t.Errorf("chunkCount(%d,%d)=%d does not cover the file exactly", c.size, got, nc)
			}
		}
	}
}

// TestBuildManifestRamp: a file above the floor gets a lead ramp plus uniform
// bulk chunks, and every chunk hash matches a re-hash of its byte range.
func TestBuildManifestRamp(t *testing.T) {
	content := fillBytes(4 << 20) // 4 MiB → ramp + bulk chunks
	dir := t.TempDir()
	path := filepath.Join(dir, "song.flac")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	hash := hashOf(content)
	m, err := buildManifest(path, hash)
	if err != nil {
		t.Fatal(err)
	}
	if !m.valid(hash) {
		t.Fatalf("built manifest invalid: %+v", m)
	}
	if len(m.LeadSizes) == 0 {
		t.Errorf("expected a lead ramp for a 4 MiB file, got none")
	}
	lay := m.layout()
	if s, e := lay.rangeOf(0); e-s != minChunkSize {
		t.Errorf("first chunk = %d bytes, want %d (ramp floor)", e-s, minChunkSize)
	}
	for i := 0; i < lay.count(); i++ {
		s, e := lay.rangeOf(i)
		if hashOf(content[s:e]) != m.Chunks[i] {
			t.Errorf("chunk %d hash does not match its content range", i)
		}
	}
}

// TestSwarmRamp fetches a file large enough to exercise the lead ramp + variable
// bulk chunks end-to-end (manifest build → multi-chunk assembly → whole-file
// verify), byte-exact.
func TestSwarmRamp(t *testing.T) {
	content := fillBytes(4 << 20) // 4 MiB
	storeA, storeB := newMemStore(), newMemStore()
	hash, resolveA := publishBlob(t, storeA, content)
	cacheB := t.TempDir()
	a, b := startNodePair(t, storeA, storeB, []Option{resolveA}, []Option{WithCacheDir(cacheB)})
	makeFriends(t, a, b, storeA, storeB)
	seedBlobCatalog(t, storeB, a, hash, int64(len(content)))
	if !fetchAndVerify(t, b, hash, content, cacheB) {
		t.Fatal("ramped multi-chunk transfer did not assemble the blob")
	}
}

// startNodeTrio starts three connected nodes (A listens; B and C dial) without
// friending them.
func startNodeTrio(t *testing.T, sA, sB, sC *memStore, oA, oB, oC []Option) (a, b, c *Node) {
	t.Helper()
	logger := log.New(io.Discard, "", 0)
	dir := t.TempDir()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	underlay := fmt.Sprintf("tcp://%s", probe.Addr())
	probe.Close()

	start := func(name string, store *memStore, fc config.FederationConfig, opts []Option) *Node {
		fc.Name, fc.KeyFile = name, filepath.Join(dir, name+".key")
		n, err := Start(fc, store, logger, opts...)
		if err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		t.Cleanup(n.Stop)
		return n
	}
	a = start("a", sA, config.FederationConfig{Listen: []string{underlay}}, oA)
	b = start("b", sB, config.FederationConfig{Peers: []string{underlay}}, oB)
	c = start("c", sC, config.FederationConfig{Peers: []string{underlay}}, oC)
	return a, b, c
}

// libraryNode wires a memStore + resolver so a node publishes and serves one
// blob from a temp library file.
func publishBlob(t *testing.T, store *memStore, content []byte) (hash string, resolver Option) {
	t.Helper()
	hash = hashOf(content)
	dir := t.TempDir()
	path := filepath.Join(dir, "track.mp3")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	store.setPublished([]CatalogEntry{{
		Key: "1", RecordingKey: "r1", Title: "Swarm Song",
		Renditions: []CatalogRendition{{Hash: hash, Size: int64(len(content)), Codec: "mp3"}},
	}})
	return hash, WithBlobResolver(func(h string) (string, bool) { return path, h == hash })
}

// TestManifestEndpoint: a friend's manifest matches a locally built one, and it
// is refused before friendship.
func TestManifestEndpoint(t *testing.T) {
	content := fillBytes(700 << 10) // ~700 KiB → several 256 KiB chunks
	storeA, storeB := newMemStore(), newMemStore()
	hash, resolveA := publishBlob(t, storeA, content)

	a, b := startNodePair(t, storeA, storeB, []Option{resolveA}, []Option{WithCacheDir(t.TempDir())})
	manURL := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/manifest/%s", a.Address(), MeshPort, hash)
	client := &http.Client{Transport: &http.Transport{DialContext: b.DialContext}, Timeout: meshClientTimeout}

	waitFor(t, "pre-friendship manifest refusal", func() bool {
		resp, err := client.Get(manURL)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("pre-friendship manifest = %d, want 403", resp.StatusCode)
		}
		return true
	})

	makeFriends(t, a, b, storeA, storeB)

	var got blobManifest
	waitFor(t, "manifest served to a friend", func() bool {
		resp, err := client.Get(manURL)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		return json.NewDecoder(resp.Body).Decode(&got) == nil
	})
	if !got.valid(hash) {
		t.Fatalf("served manifest invalid: %+v", got)
	}
	if got.Size != int64(len(content)) || len(got.Chunks) != chunkCount(got.Size, got.ChunkSize) {
		t.Errorf("manifest size/chunks off: size=%d chunks=%d chunkSize=%d", got.Size, len(got.Chunks), got.ChunkSize)
	}
	if len(got.Chunks) < 2 {
		t.Errorf("expected a multi-chunk manifest, got %d chunk(s)", len(got.Chunks))
	}
}

// TestSwarmMultiSource: two honest seeders both hold the blob; the fetcher
// assembles it byte-exact across them.
func TestSwarmMultiSource(t *testing.T) {
	content := fillBytes(700 << 10)
	storeA, storeB, storeC := newMemStore(), newMemStore(), newMemStore()
	hash, resolveA := publishBlob(t, storeA, content)
	_, resolveC := publishBlob(t, storeC, content)
	cacheB := t.TempDir()

	a, b, c := startNodeTrio(t, storeA, storeB, storeC,
		[]Option{resolveA}, []Option{WithCacheDir(cacheB)}, []Option{resolveC})
	makeFriends(t, a, b, storeA, storeB)
	makeFriends(t, b, c, storeB, storeC)
	seedBlobCatalog(t, storeB, a, hash, int64(len(content)))
	seedBlobCatalog(t, storeB, c, hash, int64(len(content)))

	assembled := fetchAndVerify(t, b, hash, content, cacheB)
	if !assembled {
		t.Fatal("multi-source transfer did not assemble the blob")
	}
}

// TestSwarmFailover: one holder advertises the blob but cannot serve it (no
// resolver, empty cache); the honest holder covers every chunk.
func TestSwarmFailover(t *testing.T) {
	content := fillBytes(700 << 10)
	storeA, storeB, storeC := newMemStore(), newMemStore(), newMemStore()
	hash, resolveA := publishBlob(t, storeA, content)
	cacheB := t.TempDir()

	// C is a friend advertised as a holder but serves nothing (404s manifest +
	// blob), so every chunk must fail over to A.
	a, b, c := startNodeTrio(t, storeA, storeB, storeC,
		[]Option{resolveA}, []Option{WithCacheDir(cacheB)}, nil)
	makeFriends(t, a, b, storeA, storeB)
	makeFriends(t, b, c, storeB, storeC)
	seedBlobCatalog(t, storeB, a, hash, int64(len(content)))
	seedBlobCatalog(t, storeB, c, hash, int64(len(content)))

	if !fetchAndVerify(t, b, hash, content, cacheB) {
		t.Fatal("failover transfer did not assemble the blob")
	}
}

// TestHoldingsTracker: A holds a blob only in its download cache (not its
// library). B pulls A's holdings, then fetches the blob from A's cache.
func TestHoldingsTracker(t *testing.T) {
	content := fillBytes(300 << 10)
	hash := hashOf(content)
	storeA, storeB := newMemStore(), newMemStore()

	// A's cache holds the blob; A publishes nothing (library empty).
	cacheA := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheA, hash), content, 0o644); err != nil {
		t.Fatal(err)
	}
	cacheB := t.TempDir()
	a, b := startNodePair(t, storeA, storeB,
		[]Option{WithCacheDir(cacheA)}, []Option{WithCacheDir(cacheB)})
	makeFriends(t, a, b, storeA, storeB)

	// A advertises the cache blob in holdings; B syncs it → it becomes a provider.
	pA, err := storeB.GetFederationPeerByKey(context.Background(), a.PublicKeyHex())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "B to learn A's holdings", func() bool {
		b.syncHoldings(context.Background(), pA)
		size, holders, _ := storeB.MadnetworkBlobProviders(context.Background(), hash)
		return len(holders) == 1 && size == 0 // cache-only holder → no advertised size
	})

	if !fetchAndVerify(t, b, hash, content, cacheB) {
		t.Fatal("cache-holder transfer did not assemble the blob")
	}
}

// TestSeedToggles: the master seed switch refuses all service; disabling
// cache-seeding hides the cache blob and empties holdings while the library
// stays served.
func TestSeedToggles(t *testing.T) {
	libContent := fillBytes(120 << 10)
	cacheContent := fillBytes(90 << 10)
	cacheHash := hashOf(cacheContent)
	storeA, storeB := newMemStore(), newMemStore()
	libHash, resolveA := publishBlob(t, storeA, libContent)

	cacheA := t.TempDir()
	if err := os.WriteFile(filepath.Join(cacheA, cacheHash), cacheContent, 0o644); err != nil {
		t.Fatal(err)
	}
	a, b := startNodePair(t, storeA, storeB,
		[]Option{resolveA, WithCacheDir(cacheA)}, []Option{WithCacheDir(t.TempDir())})
	makeFriends(t, a, b, storeA, storeB)
	client := &http.Client{Transport: &http.Transport{DialContext: b.DialContext}, Timeout: meshClientTimeout}
	blobURL := func(h string) string {
		return fmt.Sprintf("http://[%s]:%d/madnetwork/v0/blob/%s", a.Address(), MeshPort, h)
	}
	status := func(h string) int {
		resp, err := client.Get(blobURL(h))
		if err != nil {
			return -1
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	// Defaults on: both served.
	waitFor(t, "library blob served", func() bool { return status(libHash) == http.StatusOK })
	if code := status(cacheHash); code != http.StatusOK {
		t.Errorf("cache blob = %d, want 200 with cache-seeding on", code)
	}

	// Cache-seeding off: cache blob 404, library still 200, holdings empty.
	storeA.setSeeding(true, false)
	if code := status(cacheHash); code != http.StatusNotFound {
		t.Errorf("cache blob (seed_cache off) = %d, want 404", code)
	}
	if code := status(libHash); code != http.StatusOK {
		t.Errorf("library blob (seed_cache off) = %d, want 200", code)
	}
	if hs := holdingsOf(t, client, a); len(hs) != 0 {
		t.Errorf("holdings with cache-seeding off = %v, want empty", hs)
	}

	// Seeding off entirely: nothing served.
	storeA.setSeeding(false, false)
	if code := status(libHash); code != http.StatusNotFound {
		t.Errorf("library blob (seed off) = %d, want 404", code)
	}
}

// holdingsOf fetches a node's advertised cache holdings over the mesh.
func holdingsOf(t *testing.T, client *http.Client, a *Node) []string {
	t.Helper()
	url := fmt.Sprintf("http://[%s]:%d/madnetwork/v0/holdings", a.Address(), MeshPort)
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var msg holdingsMessage
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		t.Fatal(err)
	}
	return msg.Hashes
}

// fetchAndVerify runs EnsureBlob to completion and checks the cached bytes.
func fetchAndVerify(t *testing.T, b *Node, hash string, want []byte, cacheDir string) bool {
	t.Helper()
	tr, err := b.EnsureBlob(context.Background(), hash)
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
	got, err := os.ReadFile(filepath.Join(cacheDir, hash))
	if err != nil {
		t.Fatalf("cache file: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("cached bytes differ from origin (%d vs %d)", len(got), len(want))
		return false
	}
	return true
}
