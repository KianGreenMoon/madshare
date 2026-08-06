package api

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

// The write side of the madnetwork cache index
// (docs/architecture/madnetwork-cache.md).
//
// The index lives here rather than in the federation node on purpose: fetching
// bytes does not require an index, so the transfer engine has no business
// knowing about a database. Every blob that enters the cache enters it through
// EnsureBlob, and EnsureBlob is called from exactly two places — the streaming
// relay and download-to-library — so wrapping that one call covers the whole
// write path. Anything those miss (a process killed between the rename and the
// insert) is adopted by ReconcileMadnetworkCache at the next startup, which is
// what makes it safe for this to be best-effort.

// cacheTouchInterval throttles the last-used clock: at most one write per hash
// per interval. A browser seeking through a track issues a Range request per
// drag and each one is its own relay call, so without this a scrub would write a
// row per drag. Five minutes is orders of magnitude finer than any retention
// window will be, so nothing is lost by coarsening it this far.
const cacheTouchInterval = 5 * time.Minute

// cacheTouches remembers when each hash's clock was last written. Process-global
// like mnJobs, since the per-listener handlers share one cache.
var cacheTouches = struct {
	sync.Mutex
	at map[string]time.Time
}{at: map[string]time.Time{}}

// ensureBlob is the one way the API asks for remote bytes: it joins (or starts)
// the fetch, records that a LOCAL user wanted this blob, and arranges for the
// blob to be indexed once it lands.
//
// Both of those are deliberately tied to this call rather than to a successful
// response. A stream the browser abandons still fetched the bytes and still
// filled the cache, so the entry must be indexed; and the person did ask for it,
// so it counts as use.
func (h *handler) ensureBlob(ctx context.Context, hash string) (federation.Transfer, error) {
	t, err := h.federation.EnsureBlob(ctx, hash)
	if err != nil {
		return nil, err
	}
	h.touchCache(hash)
	h.indexCachedBlob(hash, t)
	return t, nil
}

// touchCache moves a cache entry's last-used clock to now, throttled.
//
// Called only for reads by a local user. A member of the community pulling the
// blob out of our cache over the mesh deliberately does NOT reach here (decided
// 2026-08-06): seeding is a service we render with bytes we happen to hold, not
// a reason to go on holding them, and a disk kept full purely by other people's
// traffic is what the retention policy exists to prevent.
//
// A hash with no row is a silent no-op in SQL — the index describes the
// directory, so a library blob or an unknown hash simply matches nothing.
func (h *handler) touchCache(hash string) {
	if h.repo == nil {
		return
	}
	now := time.Now()
	cacheTouches.Lock()
	last, seen := cacheTouches.at[hash]
	if seen && now.Sub(last) < cacheTouchInterval {
		cacheTouches.Unlock()
		return
	}
	cacheTouches.at[hash] = now
	cacheTouches.Unlock()

	if err := h.repo.TouchMadnetworkCache(context.Background(), hash, now.Unix()); err != nil {
		// Never fatal to the read that triggered it: a missed touch costs a blob
		// some of its apparent freshness, and the caller is mid-stream.
		log.Printf("touch madnetwork cache %s: %v", hash, err)
	}
}

// indexCachedBlob records a blob in the index once its fetch finishes, in the
// background — the fetch outlives the request that started it (cache-through
// streaming keeps filling the cache after a browser disconnects), so waiting
// here would tie the row to whether someone stayed on the page.
func (h *handler) indexCachedBlob(hash string, t federation.Transfer) {
	if h.repo == nil || h.cacheDir == "" || t == nil {
		return
	}
	go func() {
		<-t.Done()
		if t.Err() != nil {
			return // nothing landed
		}
		// "local" means the transfer was born complete: the bytes were already
		// here, either as a library blob (not a cache entry at all) or as a cache
		// file that was indexed when it was first fetched. Re-putting the latter
		// would overwrite its real origin filename with the hash, since
		// completedTransfer names a finished transfer after its own path.
		if t.Stats().Mode == "local" {
			return
		}
		if err := h.indexCacheEntry(context.Background(), hash, t.Filename()); err != nil {
			log.Printf("index cached blob %s: %v", hash, err)
		}
	}()
}

// indexCacheEntry describes one cache file and writes its row. The file itself
// is the only source consulted: size from stat, and the tags out of its own
// headers with the same call the upload ingest makes. Not a source's claim about
// the bytes — that is what lets the entry still name itself, and stay
// searchable, after whoever we fetched it from has left the network.
//
// A hash with no file under cacheDir is not an error: EnsureBlob resolves the
// local library before the cache, so a blob we already held finishes as a
// completed transfer that never touched the cache at all.
func (h *handler) indexCacheEntry(ctx context.Context, hash, filename string) error {
	path := filepath.Join(h.cacheDir, hash)
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil
	}
	now := time.Now().Unix()
	e := &database.MadnetworkCacheEntry{
		Hash:     hash,
		ByteSize: info.Size(),
		Filename: sanitizeFilename(filename),
		// The fetch happened because someone here asked for these bytes, so
		// arrival IS the first use. (A re-fetch of an already-indexed hash does
		// not move the clock — PutMadnetworkCacheEntry leaves last_used alone.)
		FetchedAt:  now,
		LastUsedAt: now,
	}
	if f, oerr := os.Open(path); oerr == nil {
		tags := extractTagsOrEmpty(f, "")
		e.Title, e.Artist, e.Album = tags.Title, tags.Artist, tags.Album
		f.Close()
	}
	return h.repo.PutMadnetworkCacheEntry(ctx, e)
}

// holdsCached reports whether the download cache holds these bytes right now.
// Asked of the FILE rather than the index, because it decides whether a request
// can be served at all — and the file is what would serve it.
func (h *handler) holdsCached(hash string) bool {
	if h.cacheDir == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(h.cacheDir, hash))
	return err == nil && !info.IsDir()
}

// cachedDownloadName picks the filename a saved cache blob lands under. The
// transfer's own name is right while it is running, but a finished one is named
// after its path — which for a cache file IS the hash — so the index is what
// remembers the origin's name across a restart. A blob that never had one falls
// back to its tags, then to the hash: an extensionless digest is a poor name, but
// it is honest and the browser still saves the right bytes.
func (h *handler) cachedDownloadName(ctx context.Context, hash, live string) string {
	if name := sanitizeFilename(live); name != "" && name != hash {
		return name
	}
	if h.repo == nil {
		return hash
	}
	e, err := h.repo.GetMadnetworkCacheEntry(ctx, hash)
	if err != nil || e == nil {
		return hash
	}
	if e.Filename != "" {
		return e.Filename
	}
	if e.Title != "" {
		if e.Artist != "" {
			return sanitizeFilename(e.Artist + " - " + e.Title)
		}
		return sanitizeFilename(e.Title)
	}
	return hash
}

// dropCacheIndex makes the index agree that a cache file is gone. Best-effort
// like the eviction it follows: the file is what matters, and a row that
// outlives it is dropped by the next reconcile pass anyway.
func (h *handler) dropCacheIndex(hash string) {
	if h.repo == nil {
		return
	}
	if err := h.repo.DeleteMadnetworkCacheEntry(context.Background(), hash); err != nil {
		log.Printf("drop madnetwork cache index %s: %v", hash, err)
	}
	cacheTouches.Lock()
	delete(cacheTouches.at, hash)
	cacheTouches.Unlock()
}
