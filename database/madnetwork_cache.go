package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"daemonlord.ygg/madshare/federation"
	"daemonlord.ygg/madshare/media"
)

// The madnetwork download cache index (docs/architecture/madnetwork-cache.md).
//
// The cache itself is a flat directory of files named after their content hash;
// this table describes those files so the control page can list, search and sort
// them, and so a retention policy has a "last used" clock to read. The direction
// of authority never reverses: the FILES ARE THE TRUTH and these rows describe
// them. Every other cache path (EnsureBlob, seedableBlob, cacheHoldings, the
// startup eviction sweep) still reads the directory and knows nothing about this
// table, which is what makes a stale row harmless — ReconcileMadnetworkCache
// drops it, and no phantom entry can ever be advertised to the swarm.

// MadnetworkCacheEntry is one blob in the download cache.
//
// Title/Artist/Album come from the file's OWN embedded tags, read once when the
// row is created — not from any node's catalog claim. That is what lets a cached
// blob still name itself after whoever we fetched it from has left the network.
// All three are routinely empty (an untagged blob is not an error); the display
// falls back to Filename, then to Hash.
type MadnetworkCacheEntry struct {
	Hash       string `json:"hash"`
	ByteSize   int64  `json:"byte_size"`
	Filename   string `json:"filename"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Album      string `json:"album"`
	FetchedAt  int64  `json:"fetched_at"`
	LastUsedAt int64  `json:"last_used_at"`
}

// MadnetworkCacheFilter is what the control page's search box and sort dropdown
// resolve to. It is the single definition of "what matches", shared by the page
// query, the count/sum, and the select-all-N hash resolver — so a bulk action
// over "all matching" can never target a different set than the one on screen.
type MadnetworkCacheFilter struct {
	// Q is the search term. It matches the tag text and the filename, plus the
	// hash as a PREFIX (a hash is not prose: an infix match on 64 hex chars is
	// noise, and pasting the front of a digest is how anyone actually looks one
	// up).
	Q string
	// Field scopes the term, using the same vocabulary as the library's filter
	// dropdown ("", "title", "artist", "album"), so the shared UI control means
	// the same thing here.
	Field string
}

// MadnetworkCacheQuery is one page of the listing.
type MadnetworkCacheQuery struct {
	MadnetworkCacheFilter
	// Sort: "" / "newest" (fetched, newest first — the default), "oldest",
	// "lru" (least recently used first: a live preview of what a retention
	// daemon would delete next), "largest", "smallest".
	Sort   string
	Limit  int
	Offset int
}

// madnetworkCacheWhere builds the WHERE predicate and its binds for a filter.
// Returns ("", nil) when nothing is filtered.
func madnetworkCacheWhere(f MadnetworkCacheFilter) (string, []any) {
	q := strings.TrimSpace(f.Q)
	if q == "" {
		return "", nil
	}
	like := likeEscaped(q)
	col := func(c string) string {
		return "unicode_lower(" + c + ") LIKE unicode_lower(?) ESCAPE '\\'"
	}
	var conds []string
	var args []any
	switch f.Field {
	case "artist":
		conds = []string{col("artist")}
		args = []any{like}
	case "album":
		conds = []string{col("album")}
		args = []any{like}
	case "title":
		conds = []string{col("title"), col("filename")}
		args = []any{like, like}
	default: // "" / unknown → every field
		conds = []string{col("title"), col("artist"), col("album"), col("filename")}
		args = []any{like, like, like, like}
	}
	// The hash arm is a prefix match and is always available, whatever the field
	// scope: looking a blob up by digest is not "searching a title".
	conds = append(conds, "hash LIKE ? ESCAPE '\\'")
	args = append(args, strings.NewReplacer(`%`, `\%`, `_`, `\_`).Replace(strings.ToLower(q))+"%")
	return " WHERE (" + strings.Join(conds, " OR ") + ")", args
}

// madnetworkCacheOrder maps a sort token to an ORDER BY. Every order ends in
// `hash` so paging is stable when the leading key ties (two blobs fetched in the
// same second would otherwise be free to swap between pages and be listed twice
// or not at all).
func madnetworkCacheOrder(sort string) string {
	switch sort {
	case "oldest":
		return " ORDER BY fetched_at ASC, hash ASC"
	case "lru":
		return " ORDER BY last_used_at ASC, hash ASC"
	case "largest":
		return " ORDER BY byte_size DESC, hash ASC"
	case "smallest":
		return " ORDER BY byte_size ASC, hash ASC"
	default: // "newest"
		return " ORDER BY fetched_at DESC, hash ASC"
	}
}

// ListMadnetworkCachePage returns one page of the cache listing.
func (db *DB) ListMadnetworkCachePage(ctx context.Context, q MadnetworkCacheQuery) ([]*MadnetworkCacheEntry, error) {
	where, args := madnetworkCacheWhere(q.MadnetworkCacheFilter)
	sql := `SELECT hash, byte_size, filename, title, artist, album, fetched_at, last_used_at
	        FROM madnetwork_cache` + where + madnetworkCacheOrder(q.Sort)
	if q.Limit > 0 {
		sql += " LIMIT ?"
		args = append(args, q.Limit)
		if q.Offset > 0 {
			sql += " OFFSET ?"
			args = append(args, q.Offset)
		}
	}
	rows, err := db.QueryContext(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list madnetwork cache: %w", err)
	}
	defer rows.Close()
	var out []*MadnetworkCacheEntry
	for rows.Next() {
		var e MadnetworkCacheEntry
		if err := rows.Scan(&e.Hash, &e.ByteSize, &e.Filename, &e.Title, &e.Artist,
			&e.Album, &e.FetchedAt, &e.LastUsedAt); err != nil {
			return nil, fmt.Errorf("scan madnetwork cache row: %w", err)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// CountMadnetworkCache returns how many entries match the filter and how many
// bytes they occupy. Both come from one indexed pass, so the page's total and
// its "N files, X GB" figure can never disagree.
func (db *DB) CountMadnetworkCache(ctx context.Context, f MadnetworkCacheFilter) (int, int64, error) {
	where, args := madnetworkCacheWhere(f)
	var count int
	var bytes int64
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(byte_size), 0) FROM madnetwork_cache`+where, args...).
		Scan(&count, &bytes)
	if err != nil {
		return 0, 0, fmt.Errorf("count madnetwork cache: %w", err)
	}
	return count, bytes, nil
}

// MadnetworkCacheHashes resolves a filter to the hashes it matches — the
// "select all N matching" set, resolved server-side so a bulk removal never has
// to materialise the rows the operator never scrolled to.
func (db *DB) MadnetworkCacheHashes(ctx context.Context, f MadnetworkCacheFilter) ([]string, error) {
	where, args := madnetworkCacheWhere(f)
	rows, err := db.QueryContext(ctx, `SELECT hash FROM madnetwork_cache`+where+` ORDER BY hash`, args...)
	if err != nil {
		return nil, fmt.Errorf("madnetwork cache hashes: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("scan madnetwork cache hash: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// MadnetworkCacheBytes is the total size of the cache — the dashboard's storage
// category. One indexed SUM rather than a directory walk, the same reasoning
// that put byte_size on `files` for the other categories.
func (db *DB) MadnetworkCacheBytes(ctx context.Context) (int64, error) {
	var bytes int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(byte_size), 0) FROM madnetwork_cache`).Scan(&bytes); err != nil {
		return 0, fmt.Errorf("madnetwork cache bytes: %w", err)
	}
	return bytes, nil
}

// GetMadnetworkCacheEntry returns one entry, or (nil, nil) on miss.
func (db *DB) GetMadnetworkCacheEntry(ctx context.Context, hash string) (*MadnetworkCacheEntry, error) {
	var e MadnetworkCacheEntry
	err := db.QueryRowContext(ctx,
		`SELECT hash, byte_size, filename, title, artist, album, fetched_at, last_used_at
		 FROM madnetwork_cache WHERE hash = ?`, hash).
		Scan(&e.Hash, &e.ByteSize, &e.Filename, &e.Title, &e.Artist, &e.Album,
			&e.FetchedAt, &e.LastUsedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get madnetwork cache entry: %w", err)
	}
	return &e, nil
}

// PutMadnetworkCacheEntry records a blob that has landed in the cache.
//
// Re-fetching a hash we already index is an update, not a conflict: the bytes
// are identical (the content hash is the name), but the row may have been
// adopted by reconciliation with no tags and an mtime for a fetch date, and the
// real fetch knows better. LastUsedAt is NOT touched here — landing in the cache
// is not a use, and letting a re-fetch reset the clock would make a blob nobody
// plays look freshly wanted.
func (db *DB) PutMadnetworkCacheEntry(ctx context.Context, e *MadnetworkCacheEntry) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO madnetwork_cache
		    (hash, byte_size, filename, title, artist, album, fetched_at, last_used_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(hash) DO UPDATE SET
		    byte_size = excluded.byte_size,
		    filename  = excluded.filename,
		    title     = excluded.title,
		    artist    = excluded.artist,
		    album     = excluded.album,
		    fetched_at = excluded.fetched_at`,
		e.Hash, e.ByteSize, e.Filename, e.Title, e.Artist, e.Album, e.FetchedAt, e.LastUsedAt)
	if err != nil {
		return fmt.Errorf("put madnetwork cache entry: %w", err)
	}
	return nil
}

// TouchMadnetworkCache records that a LOCAL user read this blob — the clock a
// retention policy keyed on last use reads. A hash with no row is silently
// ignored: the index describes the directory, and inventing a row for a file
// that may not be there would invert the direction of authority.
//
// Never called for a mesh peer fetching the blob out of our cache (decided
// 2026-08-06): seeding is a service we render with bytes we happen to hold, not
// a reason to go on holding them.
func (db *DB) TouchMadnetworkCache(ctx context.Context, hash string, at int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE madnetwork_cache SET last_used_at = ? WHERE hash = ? AND last_used_at < ?`,
		at, hash, at)
	if err != nil {
		return fmt.Errorf("touch madnetwork cache: %w", err)
	}
	return nil
}

// DeleteMadnetworkCacheEntry drops one row. Removing a hash that is not indexed
// is a success, not an error — the caller's job was to make the index agree that
// the file is gone.
func (db *DB) DeleteMadnetworkCacheEntry(ctx context.Context, hash string) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM madnetwork_cache WHERE hash = ?`, hash); err != nil {
		return fmt.Errorf("delete madnetwork cache entry: %w", err)
	}
	return nil
}

// ReconcileMadnetworkCache makes the index agree with the cache directory: a
// blob on disk with no row is adopted, a row whose file is gone is dropped. It
// returns how many of each.
//
// This is where "the files are the truth" is actually enforced, and it is what
// makes every other path in the feature simple: nothing else has to handle a
// disagreement, because a disagreement does not survive a reconcile. Three
// things produce one, and it handles all three the same way — a cache that
// predates this index, a process killed between the rename and the insert, and
// an operator deleting files by hand.
//
// Idempotent and safe to run at any time. It runs at startup (after
// EvictCachedMadnetworkBlobs, which deletes files — reconciling first would only
// re-drop those rows a moment later) and on demand from the control page's
// Rescan button.
//
// An adopted row can only be described by evidence still on disk: size and mtime
// from stat, tags from the file itself. The origin filename is not recoverable
// and stays empty. `.part` files are skipped by the digest-shape check, which is
// correct — a partial is not a cache entry, and reaping abandoned ones is a
// separate, explicitly requested act.
func ReconcileMadnetworkCache(ctx context.Context, db *DB, cacheDir string) (added, dropped int, err error) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			// Federation never ran here, or nothing was ever fetched. Any rows we
			// hold are then stale by definition, so fall through with an empty
			// on-disk set rather than returning early.
			entries = nil
		} else {
			return 0, 0, fmt.Errorf("read cache dir: %w", err)
		}
	}
	onDisk := make(map[string]bool, len(entries))
	for _, e := range entries {
		if !e.IsDir() && hashDirPattern.MatchString(e.Name()) {
			onDisk[e.Name()] = true
		}
	}

	indexed := map[string]bool{}
	rows, err := db.QueryContext(ctx, `SELECT hash FROM madnetwork_cache`)
	if err != nil {
		return 0, 0, fmt.Errorf("read cache index: %w", err)
	}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return 0, 0, fmt.Errorf("scan cache index hash: %w", err)
		}
		indexed[h] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("reconcile cache: begin: %w", err)
	}
	defer tx.Rollback()

	for hash := range onDisk {
		if indexed[hash] {
			continue
		}
		e := describeCachedBlob(filepath.Join(cacheDir, hash), hash)
		if e == nil {
			continue // vanished between the listing and the stat — not ours to mourn
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO madnetwork_cache
			    (hash, byte_size, filename, title, artist, album, fetched_at, last_used_at)
			VALUES (?, ?, '', ?, ?, ?, ?, ?)`,
			e.Hash, e.ByteSize, e.Title, e.Artist, e.Album, e.FetchedAt, e.LastUsedAt); err != nil {
			return 0, 0, fmt.Errorf("adopt cached blob %s: %w", hash, err)
		}
		added++
	}
	for hash := range indexed {
		if onDisk[hash] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM madnetwork_cache WHERE hash = ?`, hash); err != nil {
			return 0, 0, fmt.Errorf("drop stale cache row %s: %w", hash, err)
		}
		dropped++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("reconcile cache: commit: %w", err)
	}
	return added, dropped, nil
}

// MadnetworkCacheClaim is one source's current description of a cached blob.
type MadnetworkCacheClaim struct {
	SourceKey  string `json:"source_key"`
	SourceName string `json:"source_name"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	Album      string `json:"album"`
	LastSeen   int64  `json:"last_seen"`
}

// MadnetworkCacheClaims lists what the network currently says about a hash: one
// row per source advertising it, most recently seen first.
//
// The cache page's rare "what did people call this" view. Deliberately computed
// live rather than recorded (decided 2026-08-06): no growing table for a button
// nobody opens twice, and a claim that disappeared with its node is not history
// worth keeping. The blob's own description — the one the listing shows — comes
// from the file itself and needs none of this.
//
// One hash at a time on purpose. This walks the cached catalogs, which is fine
// for a click and would be ruinous for a page of a hundred rows.
func (db *DB) MadnetworkCacheClaims(ctx context.Context, hash string) ([]*MadnetworkCacheClaim, error) {
	var out []*MadnetworkCacheClaim
	err := db.madnetworkRowsForHash(ctx, hash,
		func(p *federation.BlobProvider, e *federation.CatalogEntry, _ *federation.CatalogRendition) bool {
			name := p.Name
			if name == "" {
				name = p.HeardName
			}
			out = append(out, &MadnetworkCacheClaim{
				SourceKey: p.PublicKey, SourceName: name,
				Title: e.Title, Artist: e.Artist, Album: e.Album,
				LastSeen: p.LastSeen,
			})
			return true
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ReapAbandonedPartials deletes `<hash>.part` scratch files that no transfer is
// writing, returning how many and how many bytes were freed.
//
// These are the one thing in the cache that is pure waste. A failed fetch cleans
// up after itself, but a KILLED PROCESS cannot, and nothing swept them
// afterwards: the eviction sweep and the holdings listing both skip non-digest
// names on purpose, so an abandoned partial was permanent dead disk.
//
// `live` is the set of hashes with a running transfer, whose partials must
// survive. **At startup it is correctly nil** — a process that has just started
// is writing nothing, so every partial it finds is abandoned by definition. That
// is what makes the startup sweep unconditional and safe: no age heuristic, no
// policy, no knob. At runtime the caller must pass the live set.
//
// Deliberately NOT folded into ReconcileMadnetworkCache, which runs from the
// page's Rescan button while transfers may be in flight.
func ReapAbandonedPartials(cacheDir string, live map[string]bool) (int, int64, error) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("read cache dir: %w", err)
	}
	var removed int
	var freed int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), partialSuffix) {
			continue
		}
		hash := strings.TrimSuffix(e.Name(), partialSuffix)
		if !hashDirPattern.MatchString(hash) || live[hash] {
			continue
		}
		size := int64(0)
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		if err := os.Remove(filepath.Join(cacheDir, e.Name())); err != nil && !os.IsNotExist(err) {
			return removed, freed, fmt.Errorf("reap partial %s: %w", e.Name(), err)
		}
		removed++
		freed += size
	}
	return removed, freed, nil
}

// CountAbandonedPartials reports what ReapAbandonedPartials would free, without
// freeing it — the cache page's "N abandoned partials" line.
func CountAbandonedPartials(cacheDir string, live map[string]bool) (int, int64, error) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("read cache dir: %w", err)
	}
	var count int
	var bytes int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), partialSuffix) {
			continue
		}
		hash := strings.TrimSuffix(e.Name(), partialSuffix)
		if !hashDirPattern.MatchString(hash) || live[hash] {
			continue
		}
		count++
		if info, err := e.Info(); err == nil {
			bytes += info.Size()
		}
	}
	return count, bytes, nil
}

// partialSuffix is what federation names an in-progress fetch's scratch file.
// Duplicated here rather than imported: the sweeps must keep working when
// federation is compiled out, and the whole point is that they run on a node
// which is no longer fetching anything.
const partialSuffix = ".part"

// describeCachedBlob builds the index row for a cache file from the file alone:
// size and mtime from stat, tags from its own headers. mtime stands in for both
// timestamps because it is the only evidence a directory can offer about when a
// blob arrived, and there is none at all about when it was last read.
//
// Tag extraction failing is not an error — an untagged or unreadable-header blob
// is still a cache entry, it just has less to say about itself. Returns nil only
// when the file itself is gone.
func describeCachedBlob(path, hash string) *MadnetworkCacheEntry {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil
	}
	e := &MadnetworkCacheEntry{
		Hash:       hash,
		ByteSize:   info.Size(),
		FetchedAt:  info.ModTime().Unix(),
		LastUsedAt: info.ModTime().Unix(),
	}
	f, err := os.Open(path)
	if err != nil {
		return e
	}
	defer f.Close()
	tags, err := media.ExtractTags(f, "")
	if err != nil || tags == nil {
		return e
	}
	e.Title, e.Artist, e.Album = tags.Title, tags.Artist, tags.Album
	return e
}
