package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The download cache's retention policy
// (docs/architecture/madnetwork-cache.md §"The retention ceiling").
//
// Two rules, either usable alone: evict what nothing has read in N days, and
// while the cache still exceeds its ceiling, evict least-recently-used blobs
// until it fits. Age runs first — it asks an absolute question ("does anyone
// here still want this?") and the ceiling then takes the coldest of whatever
// survived, which is a question about how much room is left.
//
// Everything here follows the cache's founding rule — THE DIRECTORY IS THE
// TRUTH and the index describes it. So a sweep deletes the file first and the
// row second: a row without its file is a stale row, which reconciliation
// already drops and no code path can act on, while a file without its row is a
// blob that keeps being served and never counted. Only one of those two failure
// directions is dangerous, and this is the order that avoids it.
//
// Both sweeps evict through evictCacheEntry, so the two policies cannot drift on
// the parts that are about the cache rather than about the policy: skipping a
// blob a transfer is about to publish, tolerating one unremovable file, and the
// file-then-row order.

// SweepCacheAge evicts cached blobs nothing has read for maxAgeDays. Zero or
// less is OFF and sweeps nothing.
//
// The clock is last_used_at, which moves on LOCAL reads only — a mesh peer
// pulling from our cache never counts (docs/architecture/madnetwork-cache.md).
// That is deliberate and it is what this knob means: "we have stopped using
// this", not "nobody wants it". Withdrawing a seed the community still fetches
// is the cost, and it is the operator's call to make, which is why the default
// is off.
func SweepCacheAge(ctx context.Context, db *DB, cacheDir string, maxAgeDays int64,
	live map[string]bool) (removed int, freed int64, err error) {

	if maxAgeDays <= 0 || cacheDir == "" {
		return 0, 0, nil
	}
	cutoff := time.Now().Add(-time.Duration(maxAgeDays) * 24 * time.Hour).Unix()

	// Coldest first, so every candidate is a PREFIX of this list and the walk
	// stops at the first row inside the window. Same query and same order the
	// ceiling uses — and the same one the cache page's "least recently used" sort
	// shows, which is what makes that sort a live preview of both sweeps.
	rows, err := db.ListMadnetworkCachePage(ctx, MadnetworkCacheQuery{Sort: "lru"})
	if err != nil {
		return 0, 0, fmt.Errorf("list cache: %w", err)
	}
	for _, e := range rows {
		if e.LastUsedAt >= cutoff {
			break
		}
		if err := ctx.Err(); err != nil {
			return removed, freed, err
		}
		gone, err := evictCacheEntry(ctx, db, cacheDir, e, live)
		if err != nil {
			return removed, freed, err
		}
		if gone {
			removed++
			freed += e.ByteSize
		}
	}
	return removed, freed, nil
}

// evictCacheEntry removes one cached blob, file first and row second. It reports
// false — without an error — for an entry it declined to touch: one being
// fetched right now, or one whose file will not go away.
//
// live names hashes being fetched right now: a running transfer writes
// `<hash>.part` and renames on success, so its finished predecessor may already
// be indexed — evicting the blob a transfer is about to publish would make the
// fetch pointless. Callers pass federation's ActiveTransfers.
//
// Removing a file another request is mid-read is safe on POSIX: the open
// descriptor survives the unlink.
func evictCacheEntry(ctx context.Context, db *DB, cacheDir string,
	e *MadnetworkCacheEntry, live map[string]bool) (bool, error) {

	if live[e.Hash] {
		return false, nil
	}
	if err := os.Remove(filepath.Join(cacheDir, e.Hash)); err != nil && !os.IsNotExist(err) {
		// One unremovable file must not stop the sweep: the next candidate frees
		// space just as well, and refusing to evict anything because of one is how
		// a retention policy silently stops being enforced.
		return false, nil
	}
	if err := db.DeleteMadnetworkCacheEntry(ctx, e.Hash); err != nil {
		return false, fmt.Errorf("drop cache row %s: %w", e.Hash, err)
	}
	return true, nil
}

// SweepCacheCeiling evicts least-recently-used cached blobs until the cache fits
// under maxBytes. A ceiling of 0 or less is OFF and sweeps nothing. It reports
// what it removed; see evictCacheEntry for what it declines to touch.
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
		if err := ctx.Err(); err != nil {
			return removed, freed, err
		}
		gone, err := evictCacheEntry(ctx, db, cacheDir, e, live)
		if err != nil {
			return removed, freed, err
		}
		if gone {
			removed++
			freed += e.ByteSize
			total -= e.ByteSize
		}
	}
	return removed, freed, nil
}
