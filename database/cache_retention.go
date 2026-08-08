package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// The download cache's retention ceiling
// (docs/architecture/madnetwork-cache.md §"The retention ceiling").
//
// One rule, deliberately: while the cache exceeds its ceiling, evict
// least-recently-used blobs until it fits. Age-since-last-use is the designed
// twin and is not built.
//
// Everything here follows the cache's founding rule — THE DIRECTORY IS THE
// TRUTH and the index describes it. So a sweep deletes the file first and the
// row second: a row without its file is a stale row, which reconciliation
// already drops and no code path can act on, while a file without its row is a
// blob that keeps being served and never counted. Only one of those two failure
// directions is dangerous, and this is the order that avoids it.

// SweepCacheCeiling evicts least-recently-used cached blobs until the cache fits
// under maxBytes. A ceiling of 0 or less is OFF and sweeps nothing.
//
// live names hashes being fetched right now: a running transfer writes
// `<hash>.part` and renames on success, so its finished predecessor may already
// be indexed — evicting the blob a transfer is about to publish would make the
// fetch pointless. Callers pass federation's ActiveTransfers.
//
// It reports what it removed. Removing a file another request is mid-read is
// safe on POSIX: the open descriptor survives the unlink.
func SweepCacheCeiling(ctx context.Context, db *DB, cacheDir string, maxBytes int64,
	live map[string]bool) (removed int, freed int64, err error) {

	if maxBytes <= 0 || cacheDir == "" {
		return 0, 0, nil
	}
	total, err := db.MadnetworkCacheBytes(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("cache size: %w", err)
	}
	if total <= maxBytes {
		return 0, 0, nil
	}

	// Coldest first — the same order the cache page's "least recently used" sort
	// shows, which is what makes that sort a live preview of this sweep.
	rows, err := db.ListMadnetworkCachePage(ctx, MadnetworkCacheQuery{Sort: "lru"})
	if err != nil {
		return 0, 0, fmt.Errorf("list cache: %w", err)
	}

	for _, e := range rows {
		if total <= maxBytes {
			break
		}
		if live[e.Hash] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return removed, freed, err
		}
		if rmErr := os.Remove(filepath.Join(cacheDir, e.Hash)); rmErr != nil && !os.IsNotExist(rmErr) {
			// One unremovable file must not stop the sweep: the next-coldest blob
			// frees space just as well, and refusing to evict anything because of
			// one is how a ceiling silently stops being enforced.
			continue
		}
		if err := db.DeleteMadnetworkCacheEntry(ctx, e.Hash); err != nil {
			return removed, freed, fmt.Errorf("drop cache row %s: %w", e.Hash, err)
		}
		removed++
		freed += e.ByteSize
		total -= e.ByteSize
	}
	return removed, freed, nil
}
