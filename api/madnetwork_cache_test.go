package api

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

// cacheTestHash is a distinct digest per test: cacheTouches is process-global
// (one cache, several listener handlers), so tests that shared a hash would
// throttle each other.
func cacheTestHash(c byte) string {
	b := make([]byte, 64)
	for i := range b {
		b[i] = c
	}
	h := string(b)
	cacheTouches.Lock()
	delete(cacheTouches.at, h)
	cacheTouches.Unlock()
	return h
}

// id3v1Blob is a blob carrying its own ID3v1 tags — the point being that the
// index reads what the FILE says about itself, not what any node claimed.
func id3v1Blob(title, artist, album string) []byte {
	pad := func(s string, n int) []byte {
		b := make([]byte, n)
		copy(b, s)
		return b
	}
	data := make([]byte, 256)
	data = append(data, []byte("TAG")...)
	data = append(data, pad(title, 30)...)
	data = append(data, pad(artist, 30)...)
	data = append(data, pad(album, 30)...)
	data = append(data, pad("1999", 4)...)
	data = append(data, pad("", 30)...)
	return append(data, 0)
}

// modedTransfer reports a fetch mode the bare fakeTransfer does not, which is
// how the indexer tells a blob it just fetched from one that was already here.
type modedTransfer struct {
	*fakeTransfer
	mode string
}

func (m *modedTransfer) Stats() federation.TransferStats {
	s := m.fakeTransfer.Stats()
	s.Mode = m.mode
	return s
}

// cachedTransfer builds a finished transfer over a real file sitting in a cache
// directory, as a completed fetch leaves behind.
func cachedTransfer(t *testing.T, cacheDir, hash, name, mode string, body []byte) federation.Transfer {
	t.Helper()
	path := filepath.Join(cacheDir, hash)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	ft := &fakeTransfer{
		hash: hash, name: name, path: path,
		size: int64(len(body)), progress: int64(len(body)),
		done: make(chan struct{}),
	}
	close(ft.done)
	return &modedTransfer{fakeTransfer: ft, mode: mode}
}

// waitForRow polls for the index row the background indexer writes: the fetch
// outlives the request that started it, so the write is deliberately not
// synchronous with the response.
func waitForRow(t *testing.T, repo *fakeRepo, hash string) *database.MadnetworkCacheEntry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		repo.mu.Lock()
		e := repo.cacheIndex[hash]
		repo.mu.Unlock()
		if e != nil {
			return e
		}
		time.Sleep(2 * time.Millisecond)
	}
	return nil
}

// TestIndexCachedBlob_ReadsTheFilesOwnTags is the write half of the cache
// index: a blob that lands is described by itself — size from the file, tags
// from its own headers — so the entry can still be named and searched long
// after whoever we fetched it from has left the network.
func TestIndexCachedBlob_ReadsTheFilesOwnTags(t *testing.T) {
	cacheDir := t.TempDir()
	hash := cacheTestHash('a')
	repo := &fakeRepo{}
	h := &handler{repo: repo, cacheDir: cacheDir}

	body := id3v1Blob("Cached Title", "Cached Artist", "Cached Album")
	tr := cachedTransfer(t, cacheDir, hash, "song.mp3", "swarm", body)

	h.indexCachedBlob(hash, tr)
	e := waitForRow(t, repo, hash)
	if e == nil {
		t.Fatal("no index row written for a completed fetch")
	}
	if e.Title != "Cached Title" || e.Artist != "Cached Artist" || e.Album != "Cached Album" {
		t.Errorf("tags = %q/%q/%q, want the file's own ID3v1 values", e.Title, e.Artist, e.Album)
	}
	if e.Filename != "song.mp3" {
		t.Errorf("filename = %q, want the origin's name (it is gone after a restart)", e.Filename)
	}
	if e.ByteSize != int64(len(body)) {
		t.Errorf("byte_size = %d, want %d (from stat)", e.ByteSize, len(body))
	}
	if e.FetchedAt == 0 || e.LastUsedAt == 0 {
		t.Errorf("timestamps = %d/%d, want both set — the fetch happened because someone here asked",
			e.FetchedAt, e.LastUsedAt)
	}
}

// TestIndexCachedBlob_SkipsWhatDidNotEnterTheCache covers the two ways a
// finished transfer is not a cache entry. Both matter: EnsureBlob resolves the
// local library BEFORE the cache, so a blob we already held finishes as a
// transfer that never wrote to the cache at all.
func TestIndexCachedBlob_SkipsWhatDidNotEnterTheCache(t *testing.T) {
	t.Run("born complete", func(t *testing.T) {
		cacheDir := t.TempDir()
		hash := cacheTestHash('b')
		repo := &fakeRepo{}
		h := &handler{repo: repo, cacheDir: cacheDir}

		// Mode "local" = the bytes were already here. Re-indexing a cache hit
		// would overwrite its real origin filename with the hash, because a
		// completed transfer is named after its own path.
		tr := cachedTransfer(t, cacheDir, hash, hash, "local", []byte("bytes"))
		h.indexCachedBlob(hash, tr)

		time.Sleep(50 * time.Millisecond)
		repo.mu.Lock()
		defer repo.mu.Unlock()
		if e := repo.cacheIndex[hash]; e != nil {
			t.Errorf("indexed a born-complete transfer: %+v", e)
		}
	})

	t.Run("library blob", func(t *testing.T) {
		cacheDir := t.TempDir()
		hash := cacheTestHash('c')
		repo := &fakeRepo{}
		h := &handler{repo: repo, cacheDir: cacheDir}

		// Fetched, but the file is not under cacheDir — the library short-circuit.
		libDir := t.TempDir()
		tr := cachedTransfer(t, libDir, hash, "song.mp3", "swarm", []byte("bytes"))
		h.indexCachedBlob(hash, tr)

		time.Sleep(50 * time.Millisecond)
		repo.mu.Lock()
		defer repo.mu.Unlock()
		if e := repo.cacheIndex[hash]; e != nil {
			t.Errorf("indexed a blob that never entered the cache: %+v", e)
		}
	})
}

// TestTouchCache_Throttled: a browser seeking through a track issues a Range
// request per drag and each is its own relay call, so the last-used clock must
// collapse a storm of them into one write.
func TestTouchCache_Throttled(t *testing.T) {
	hash := cacheTestHash('d')
	repo := &fakeRepo{}
	h := &handler{repo: repo}

	for i := 0; i < 25; i++ {
		h.touchCache(hash)
	}
	repo.mu.Lock()
	n := len(repo.cacheTouched)
	repo.mu.Unlock()
	if n != 1 {
		t.Errorf("touches written = %d, want 1 — a scrub must not write a row per drag", n)
	}

	// A different blob is its own clock; the throttle is per hash.
	other := cacheTestHash('e')
	h.touchCache(other)
	repo.mu.Lock()
	n = len(repo.cacheTouched)
	repo.mu.Unlock()
	if n != 2 {
		t.Errorf("touches after a second blob = %d, want 2 (the throttle is per hash)", n)
	}
}

// TestTouchCache_OnlyLocalReads pins the decision that the retention clock
// measures local demand and nothing else. Seeding is served entirely inside the
// federation node (handleBlob), which has no route into this package — so the
// guarantee is structural, and this test states it where it would be noticed if
// somebody wired a touch into the serve path.
func TestTouchCache_OnlyLocalReads(t *testing.T) {
	cacheDir := t.TempDir()
	hash := cacheTestHash('f')
	repo := &fakeRepo{}
	tr := cachedTransfer(t, cacheDir, hash, "song.mp3", "swarm", id3v1Blob("T", "A", "Al"))
	h := &handler{repo: repo, cacheDir: cacheDir, federation: &fakeBlobFederation{blob: tr}}

	// The local paths — the streaming relay and download-to-library — both go
	// through ensureBlob, and that is the only thing in this package that touches.
	if _, err := h.ensureBlob(context.Background(), hash); err != nil {
		t.Fatalf("ensureBlob: %v", err)
	}
	repo.mu.Lock()
	touched := len(repo.cacheTouched)
	repo.mu.Unlock()
	if touched != 1 {
		t.Errorf("touches after a local request = %d, want 1", touched)
	}
	if waitForRow(t, repo, hash) == nil {
		t.Error("ensureBlob did not arrange for the fetched blob to be indexed")
	}
}

// TestDropCacheIndex: removing a cache file makes the index agree, and clears
// the throttle memory with it — otherwise a hash re-fetched inside the throttle
// window would carry a stale clock into its new life.
func TestDropCacheIndex(t *testing.T) {
	hash := cacheTestHash('9')
	repo := &fakeRepo{}
	h := &handler{repo: repo}

	if err := repo.PutMadnetworkCacheEntry(context.Background(),
		&database.MadnetworkCacheEntry{Hash: hash, ByteSize: 5}); err != nil {
		t.Fatal(err)
	}
	h.touchCache(hash)
	h.dropCacheIndex(hash)

	repo.mu.Lock()
	_, indexed := repo.cacheIndex[hash]
	repo.mu.Unlock()
	if indexed {
		t.Error("index row survived the eviction of its file")
	}
	cacheTouches.Lock()
	_, remembered := cacheTouches.at[hash]
	cacheTouches.Unlock()
	if remembered {
		t.Error("throttle memory survived the eviction — a re-fetch would inherit a stale clock")
	}
}
