package database

import (
	"context"
	"fmt"
	"strings"

	"daemonlord.ygg/madshare/federation"
)

// The swarm page's row set (docs/architecture/swarm-admin.md §The model): every
// blob this node has bytes for, whichever half of the disk it sits in.
//
// Two arms — the library (`files`, INCLUDING drafts and trashed blobs: they
// occupy the disk, and a page claiming to say what this node holds must not omit
// them) and the download cache (`madnetwork_cache`) — unioned and grouped by
// hash, so a blob that is transiently in both is ONE row carrying both flags
// rather than two rows double-counting the bytes.
//
// Traffic is a LEFT JOIN, never a requirement: the overwhelming majority of a
// library has never moved, and absence must read as zeros rather than as a
// missing row.

// SwarmScope selects which halves the listing covers — the page's switcher.
type SwarmScope string

const (
	SwarmScopeAll     SwarmScope = "all"
	SwarmScopeLibrary SwarmScope = "library"
	SwarmScopeCache   SwarmScope = "cache"
)

// Library-state pills. They describe the library half only; picking any of them
// excludes cache rows entirely, because "in review" is not a thing a cached blob
// can be.
const (
	SwarmStateAny     = ""
	SwarmStateLive    = "live"    // approved and not trashed — the published library
	SwarmStateReview  = "review"  // staged, awaiting a moderator
	SwarmStateTrashed = "trashed" // soft-deleted, still on disk
	SwarmStatePrivate = "private" // live but scoped Local — bytes the swarm never sees
)

// SwarmFilter is what the page's switcher, pills and search box resolve to. One
// definition of "what matches", shared by the page query, the count and the
// select-all resolver — so a bulk action can never target a different set than
// the one on screen.
type SwarmFilter struct {
	Scope SwarmScope
	// State is one of the pills above. Empty means every state.
	State string
	// Q matches tag text and filename, plus the hash as a PREFIX — a hash is not
	// prose, and pasting the front of a digest is how anyone looks one up.
	Q string
	// Field scopes Q using the library's own vocabulary ("", title, artist,
	// album), so the shared filter control means the same thing here.
	Field string
}

// SwarmQuery is one page of the listing.
type SwarmQuery struct {
	SwarmFilter
	// Sort: "" / "newest" (default) · "oldest" · "name" · "largest" ·
	// "smallest" · "up" · "down" · "active".
	Sort   string
	Limit  int
	Offset int
}

// SwarmFileRow is one blob on the swarm page.
type SwarmFileRow struct {
	Hash      string `json:"hash"`
	ByteSize  int64  `json:"byte_size"`
	InLibrary bool   `json:"in_library"`
	InCache   bool   `json:"in_cache"`

	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Album    string `json:"album"`
	Filename string `json:"filename"`
	// AddedAt is when this node got the bytes: the upload for a library blob, the
	// fetch for a cached one.
	AddedAt int64 `json:"added_at"`

	// The library half's state, empty/false for a cache-only row.
	ReviewState string `json:"review_state,omitempty"`
	Trashed     bool   `json:"trashed,omitempty"`
	// ShareDepth is RESOLVED: a row that inherits the node default reports the
	// default, because the question this page answers is "does the swarm see
	// it", not "which layer decided".
	ShareDepth  int   `json:"share_depth"`
	RecordingID int64 `json:"recording_id,omitempty"`

	// All-time traffic. Zero for a blob that has never moved, which is most of
	// them.
	Up     int64 `json:"up_bytes"`
	Down   int64 `json:"down_bytes"`
	Wasted int64 `json:"wasted_bytes,omitempty"`
	LastAt int64 `json:"last_at,omitempty"`
}

// Seedable reports whether this node would serve the row to anyone at all —
// approved, live, and not scoped Local. It answers the page's "why isn't this
// seeding?" without the reader having to know the scope vocabulary. Node-level
// switches (seed_enabled, and seed_cache for the cache half) are reported once
// in the summary rather than folded in here, since they are not facts about the
// row.
func (r *SwarmFileRow) Seedable() bool {
	if r.InCache && !r.InLibrary {
		return true // a cached blob seeds under seed_cache, which has no per-row half
	}
	return !r.Trashed && r.ReviewState == "approved" && r.ShareDepth > federation.DepthPrivate
}

// swarmLibraryArm builds the library half of the union and its binds.
func swarmLibraryArm(f SwarmFilter, defaultDepth int) (string, []any) {
	where := []string{"1=1"}
	var args []any
	switch f.State {
	case SwarmStateLive:
		where = append(where, visibleFile)
	case SwarmStateReview:
		where = append(where, "f.deleted_at IS NULL AND m.deleted_at IS NULL AND m.review_state <> 'approved'")
	case SwarmStateTrashed:
		where = append(where, "(f.deleted_at IS NOT NULL OR m.deleted_at IS NOT NULL)")
	case SwarmStatePrivate:
		where = append(where, visibleFile, "COALESCE(r.share_depth, ?) = ?")
		args = append(args, defaultDepth, federation.DepthPrivate)
	}
	if frag, fragArgs := qFieldClause(f.Q, f.Field); frag != "" {
		where = append(where, "("+frag+" OR f.hash LIKE ? ESCAPE '\\')")
		args = append(args, fragArgs...)
		args = append(args, hashPrefixLike(f.Q))
	}
	q := `
		SELECT f.hash AS hash, f.byte_size AS byte_size, 1 AS in_library, 0 AS in_cache,
		       COALESCE(m.title, '') AS title,
		       COALESCE(par.name, m.artist, '') AS artist,
		       COALESCE(al.title, m.album, '') AS album,
		       COALESCE((SELECT u.filename FROM file_uploads u WHERE u.file_id = f.id ORDER BY u.id LIMIT 1), '') AS filename,
		       f.created_at AS added_at,
		       COALESCE(m.review_state, '') AS review_state,
		       (CASE WHEN f.deleted_at IS NOT NULL OR m.deleted_at IS NOT NULL THEN 1 ELSE 0 END) AS trashed,
		       COALESCE(r.share_depth, ?) AS share_depth,
		       f.recording_id AS recording_id
		FROM files f` + tagsetJoin + `
		LEFT JOIN artists par ON par.id = COALESCE(m.album_artist_id, m.artist_id)
		LEFT JOIN albums al ON al.id = m.album_id
		WHERE ` + strings.Join(where, " AND ")
	// The resolved-depth bind leads the SELECT list, so it precedes the WHERE binds.
	return q, append([]any{defaultDepth}, args...)
}

// swarmCacheArm builds the cache half. A library-state pill excludes it
// entirely: "in review" is not a state a cached blob can be in, and answering
// such a filter with cache rows would quietly widen it.
func swarmCacheArm(f SwarmFilter) (string, []any) {
	if f.State != SwarmStateAny {
		return "", nil
	}
	where, args := madnetworkCacheWhere(MadnetworkCacheFilter{Q: f.Q, Field: f.Field})
	q := `
		SELECT hash, byte_size, 0 AS in_library, 1 AS in_cache,
		       title, artist, album, filename,
		       fetched_at AS added_at,
		       '' AS review_state, 0 AS trashed, ? AS share_depth, 0 AS recording_id
		FROM madnetwork_cache` + where
	// share_depth is meaningless for a cached blob — it is somebody else's
	// content, seeded under seed_cache and never under a recording's scope. The
	// unlimited value keeps Seedable() honest without inventing a per-row rule.
	return q, append([]any{federation.DepthUnlimited}, args...)
}

// hashPrefixLike turns a search term into a hash-prefix LIKE pattern.
func hashPrefixLike(q string) string {
	esc := strings.NewReplacer(`%`, `\%`, `_`, `\_`).Replace(strings.ToLower(strings.TrimSpace(q)))
	return esc + "%"
}

// swarmUnion assembles the scoped union and its binds.
func swarmUnion(f SwarmFilter, defaultDepth int) (string, []any) {
	var arms []string
	var args []any
	if f.Scope != SwarmScopeCache {
		q, a := swarmLibraryArm(f, defaultDepth)
		arms = append(arms, q)
		args = append(args, a...)
	}
	if f.Scope != SwarmScopeLibrary {
		if q, a := swarmCacheArm(f); q != "" {
			arms = append(arms, q)
			args = append(args, a...)
		}
	}
	if len(arms) == 0 {
		// A cache scope with a library-state pill selects nothing. Answering with
		// a constant-false query keeps every caller on one code path.
		return `SELECT '' AS hash, 0 AS byte_size, 0 AS in_library, 0 AS in_cache,
		        '' AS title, '' AS artist, '' AS album, '' AS filename, 0 AS added_at,
		        '' AS review_state, 0 AS trashed, 0 AS share_depth, 0 AS recording_id
		        WHERE 1=0`, nil
	}
	return strings.Join(arms, "\n\t\tUNION ALL\n"), args
}

// swarmGrouped folds the union into one row per hash. Text prefers the LIBRARY
// half when both exist — our own curated appearance beats what a cached copy's
// tags happen to say — falling back to whatever the other arm carries.
func swarmGrouped(f SwarmFilter, defaultDepth int) (string, []any) {
	union, args := swarmUnion(f, defaultDepth)
	prefer := func(col string) string {
		return "COALESCE(NULLIF(MAX(CASE WHEN in_library = 1 THEN " + col + " END), ''), MAX(" + col + "), '')"
	}
	return `
		SELECT hash,
		       MAX(byte_size) AS byte_size,
		       MAX(in_library) AS in_library,
		       MAX(in_cache) AS in_cache,
		       ` + prefer("title") + ` AS title,
		       ` + prefer("artist") + ` AS artist,
		       ` + prefer("album") + ` AS album,
		       ` + prefer("filename") + ` AS filename,
		       MAX(added_at) AS added_at,
		       COALESCE(MAX(CASE WHEN in_library = 1 THEN review_state END), '') AS review_state,
		       COALESCE(MAX(CASE WHEN in_library = 1 THEN trashed END), 0) AS trashed,
		       MAX(share_depth) AS share_depth,
		       COALESCE(MAX(CASE WHEN in_library = 1 THEN recording_id END), 0) AS recording_id
		FROM (` + union + `)
		GROUP BY hash`, args
}

// swarmSortOrder maps a sort token to an ORDER BY over the joined row. Every
// order ends in hash so paging is stable when the leading key ties — two blobs
// added in the same second would otherwise be free to swap between pages and be
// listed twice or not at all.
func swarmSortOrder(sort string) string {
	switch sort {
	case "oldest":
		return "g.added_at ASC, g.hash ASC"
	case "name":
		return "unicode_lower(g.title) ASC, g.hash ASC"
	case "largest":
		return "g.byte_size DESC, g.hash ASC"
	case "smallest":
		return "g.byte_size ASC, g.hash ASC"
	case "up":
		return "up_bytes DESC, g.hash ASC"
	case "down":
		return "down_bytes DESC, g.hash ASC"
	case "active":
		return "last_at DESC, g.hash ASC"
	default: // "newest"
		return "g.added_at DESC, g.hash ASC"
	}
}

// ListSwarmFiles returns one page of the swarm listing.
func (db *DB) ListSwarmFiles(ctx context.Context, q SwarmQuery) ([]*SwarmFileRow, error) {
	depth, err := db.nodeDefaultDepth(ctx)
	if err != nil {
		return nil, err
	}
	grouped, args := swarmGrouped(q.SwarmFilter, depth)
	sqlText := `
		SELECT g.hash, g.byte_size, g.in_library, g.in_cache, g.title, g.artist, g.album,
		       g.filename, g.added_at, g.review_state, g.trashed, g.share_depth, g.recording_id,
		       COALESCE(t.up_bytes, 0), COALESCE(t.down_bytes, 0),
		       COALESCE(t.wasted_bytes, 0), COALESCE(t.last_at, 0)
		FROM (` + grouped + `) g
		LEFT JOIN swarm_traffic t ON t.hash = g.hash
		ORDER BY ` + swarmSortOrder(q.Sort)
	if q.Limit > 0 {
		sqlText += "\n\t\tLIMIT ? OFFSET ?"
		offset := q.Offset
		if offset < 0 {
			offset = 0
		}
		args = append(args, q.Limit, offset)
	}
	rows, err := db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("list swarm files: %w", err)
	}
	defer rows.Close()

	var out []*SwarmFileRow
	for rows.Next() {
		r := &SwarmFileRow{}
		var inLib, inCache, trashed int
		if err := rows.Scan(&r.Hash, &r.ByteSize, &inLib, &inCache, &r.Title, &r.Artist,
			&r.Album, &r.Filename, &r.AddedAt, &r.ReviewState, &trashed, &r.ShareDepth,
			&r.RecordingID, &r.Up, &r.Down, &r.Wasted, &r.LastAt); err != nil {
			return nil, fmt.Errorf("scan swarm file: %w", err)
		}
		r.InLibrary, r.InCache, r.Trashed = inLib == 1, inCache == 1, trashed == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// CountSwarmFiles returns how many blobs match, and their total bytes — the
// figures behind "select all N" and the listing's byte line. Same filter, same
// grouping, so it can never disagree with the page.
func (db *DB) CountSwarmFiles(ctx context.Context, f SwarmFilter) (int, int64, error) {
	depth, err := db.nodeDefaultDepth(ctx)
	if err != nil {
		return 0, 0, err
	}
	grouped, args := swarmGrouped(f, depth)
	var n int
	var bytes int64
	err = db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(byte_size), 0) FROM (`+grouped+`) g`, args...).
		Scan(&n, &bytes)
	if err != nil {
		return 0, 0, fmt.Errorf("count swarm files: %w", err)
	}
	return n, bytes, nil
}

// SwarmFileHashes resolves a filter to the hashes it matches — the select-all-N
// path, over the same predicate the page uses.
func (db *DB) SwarmFileHashes(ctx context.Context, f SwarmFilter) ([]string, error) {
	depth, err := db.nodeDefaultDepth(ctx)
	if err != nil {
		return nil, err
	}
	grouped, args := swarmGrouped(f, depth)
	rows, err := db.QueryContext(ctx, `SELECT hash FROM (`+grouped+`) g`, args...)
	if err != nil {
		return nil, fmt.Errorf("swarm file hashes: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// GetSwarmFile returns one blob's row, or nil when this node has no bytes for
// that hash at all.
func (db *DB) GetSwarmFile(ctx context.Context, hash string) (*SwarmFileRow, error) {
	rows, err := db.ListSwarmFiles(ctx, SwarmQuery{
		SwarmFilter: SwarmFilter{Scope: SwarmScopeAll, Q: hash},
		Limit:       2,
	})
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.Hash == hash {
			return r, nil
		}
	}
	return nil, nil
}
