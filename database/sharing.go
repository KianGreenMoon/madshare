package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"daemonlord.ygg/madshare/federation"
)

// The organize-then-share surface (docs/plans/full-node-mode.md W2): the
// queries behind an embedder's publish picker. A player pins its node default
// to Local, so the ONLY route from its disk into the catalog is the per-item
// pin these calls read and write — recordings.share_depth, the same column the
// admin UI's scope chip edits, addressed by TAGSET because an appearance is
// what a browse row carries. The visibility rule itself is not re-derived
// here: what an audience actually sees stays decided by audienceClause /
// PublishedCatalog, and SharedTracks below is the editor-side projection of
// the same predicate.

// SetShareDepthByTagsets pins (or un-pins, via Inherit) the sharing scope of
// the recordings behind the given appearances. It returns how many recordings
// changed — less than len(tagsetIDs) when several appearances share a
// recording, which is normal, or when an id names nothing, which the caller
// may treat as it likes (a picker acting on rows it just listed has no failed
// ids to report).
//
// The update must be explicit: a ShareDepthUpdate with Set=false is refused
// rather than treated as a no-op, and the depth must be one of the three legal
// scopes (ValidDepth).
func (db *DB) SetShareDepthByTagsets(ctx context.Context, tagsetIDs []int64, depth ShareDepthUpdate) (int64, error) {
	if !depth.Set {
		return 0, errors.New("set share depth: no depth given")
	}
	if !depth.Valid() {
		return 0, errors.New("set share depth: invalid share depth")
	}
	if len(tagsetIDs) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(tagsetIDs))
	args := make([]any, 0, len(tagsetIDs)+1)
	args = append(args, depth.column())
	for i, id := range tagsetIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}
	res, err := db.ExecContext(ctx, `
		UPDATE recordings SET share_depth = ?
		WHERE id IN (SELECT DISTINCT recording_id FROM tagsets
		             WHERE id IN (`+strings.Join(placeholders, ",")+`))`, args...)
	if err != nil {
		return 0, fmt.Errorf("set share depth: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("set share depth: %w", err)
	}
	return n, nil
}

// TagsetShareDepths reports the pinned scope of each appearance's recording,
// keyed by tagset id. An id absent from the result inherits the node default —
// absence IS the answer, not a miss — so a picker rendering an album asks once
// with the visible rows and marks the pinned ones.
func (db *DB) TagsetShareDepths(ctx context.Context, tagsetIDs []int64) (map[int64]int, error) {
	out := map[int64]int{}
	if len(tagsetIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(tagsetIDs))
	args := make([]any, len(tagsetIDs))
	for i, id := range tagsetIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := db.QueryContext(ctx, `
		SELECT t.id, r.share_depth FROM tagsets t
		JOIN recordings r ON r.id = t.recording_id
		WHERE t.id IN (`+strings.Join(placeholders, ",")+`) AND r.share_depth IS NOT NULL`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("tagset share depths: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, depth int64
		if err := rows.Scan(&id, &depth); err != nil {
			return nil, fmt.Errorf("scan tagset share depth: %w", err)
		}
		out[id] = int(depth)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tagset share depths: %w", err)
	}
	return out, nil
}

// SharedTrack is one row of the published listing: an appearance the network
// can currently see, and how far it travels.
type SharedTrack struct {
	TagsetID int64
	Title    string
	Artist   string // display artist (performer, falling back like the catalog)
	Album    string
	// Depth is the effective scope: DepthFriends or DepthUnlimited (a Local row
	// is by definition not in this listing).
	Depth int
	// Pinned is false when the row is published by the NODE DEFAULT rather than
	// a pin of its own — impossible on a player (default Local) but the honest
	// answer on a server whose default is open: un-pinning such a row does not
	// withdraw it.
	Pinned bool
}

// SharedTracks lists every appearance the node currently publishes — the
// editor's view of PublishedCatalog's selection: same visibleTagset predicate,
// same depth resolution against the node default, ordered for reading (artist,
// album, disc/track, title).
func (db *DB) SharedTracks(ctx context.Context) ([]*SharedTrack, error) {
	defaultDepth, err := db.nodeDefaultDepth(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.title,
		       COALESCE(par.name, m.artist, aar.name, m.album_artist, ''),
		       COALESCE(al.title, m.album, ''),
		       COALESCE(r.share_depth, ?), r.share_depth IS NOT NULL
		FROM tagsets m`+recordingJoin+`
		LEFT JOIN artists par ON par.id = m.artist_id
		LEFT JOIN artists aar ON aar.id = m.album_artist_id
		LEFT JOIN albums al   ON al.id  = m.album_id
		WHERE `+visibleTagset+`
		  AND COALESCE(r.share_depth, ?) >= ?
		ORDER BY LOWER(COALESCE(aar.name, m.album_artist, par.name, m.artist, '')),
		         LOWER(COALESCE(al.title, m.album, '')),
		         m.disc_number IS NULL, m.disc_number, m.track_number,
		         LOWER(m.title)`,
		defaultDepth, defaultDepth, federation.DepthFriends)
	if err != nil {
		return nil, fmt.Errorf("shared tracks: %w", err)
	}
	defer rows.Close()
	out := []*SharedTrack{}
	for rows.Next() {
		var t SharedTrack
		var depth sql.NullInt64
		if err := rows.Scan(&t.TagsetID, &t.Title, &t.Artist, &t.Album, &depth, &t.Pinned); err != nil {
			return nil, fmt.Errorf("scan shared track: %w", err)
		}
		t.Depth = int(depth.Int64)
		out = append(out, &t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("shared tracks: %w", err)
	}
	return out, nil
}
