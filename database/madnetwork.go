package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"daemonlord.ygg/madshare/federation"
)

// Federation F2 — the catalog (docs/architecture/federation.md §Catalog):
// what this node publishes to friends (PublishedCatalog), the per-peer cached
// copies pulled from them (ReplacePeerCatalog), and the merged browse queries
// behind /api/madnetwork/* (friends only — a blocked peer's cache is kept but
// hidden). *DB satisfies the catalog half of federation.PeerStore here.

// PublishedCatalog builds this node's own catalog: every approved, live
// appearance (the visibleTagset predicate — exactly what the local library
// shows) with resolved display names and its recording's live renditions.
// Ordered by tagset id so the snapshot serial is deterministic.
func (db *DB) PublishedCatalog(ctx context.Context) ([]federation.CatalogEntry, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.recording_id, m.title,
		       COALESCE(par.name, m.artist, ''),
		       COALESCE(aar.name, m.album_artist, ''),
		       COALESCE(al.title, m.album, ''),
		       COALESCE(m.genre, ''), m.year, m.track_number, m.disc_number,
		       COALESCE(r.license, ''), r.guest_playable
		FROM tagsets m`+recordingJoin+`
		LEFT JOIN artists par ON par.id = m.artist_id
		LEFT JOIN artists aar ON aar.id = m.album_artist_id
		LEFT JOIN albums al   ON al.id  = m.album_id
		WHERE `+visibleTagset+`
		ORDER BY m.id`)
	if err != nil {
		return nil, fmt.Errorf("published catalog: %w", err)
	}
	defer rows.Close()

	var entries []federation.CatalogEntry
	recordings := map[string][]int{} // recording key -> entry indexes awaiting renditions
	for rows.Next() {
		var e federation.CatalogEntry
		var tagsetID, recordingID int64
		var year, track, disc sql.NullInt64
		if err := rows.Scan(&tagsetID, &recordingID, &e.Title, &e.Artist, &e.AlbumArtist,
			&e.Album, &e.Genre, &year, &track, &disc, &e.License, &e.GuestPlayable); err != nil {
			return nil, fmt.Errorf("scan catalog entry: %w", err)
		}
		e.Key = strconv.FormatInt(tagsetID, 10)
		e.RecordingKey = strconv.FormatInt(recordingID, 10)
		e.Year, e.TrackNumber, e.DiscNumber = nullInt(year), nullInt(track), nullInt(disc)
		e.Renditions = []federation.CatalogRendition{}
		recordings[e.RecordingKey] = append(recordings[e.RecordingKey], len(entries))
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("published catalog rows: %w", err)
	}
	if len(entries) == 0 {
		return []federation.CatalogEntry{}, nil
	}

	// Attach each recording's live renditions (hash = the future swarm id, plus
	// the quality facts the ladder ranks by).
	rrows, err := db.QueryContext(ctx, `
		SELECT f.recording_id, f.hash, f.byte_size,
		       COALESCE(mm.codec, ''), COALESCE(mm.bitrate, 0),
		       COALESCE(mm.sample_rate, 0), COALESCE(mm.bit_depth, 0),
		       COALESCE(mm.duration_seconds, 0)
		FROM files f
		LEFT JOIN media_metadata mm ON mm.file_id = f.id
		WHERE f.deleted_at IS NULL AND EXISTS (
			SELECT 1 FROM tagsets m WHERE m.recording_id = f.recording_id AND `+visibleTagset+`)
		ORDER BY f.recording_id, f.id`)
	if err != nil {
		return nil, fmt.Errorf("catalog renditions: %w", err)
	}
	defer rrows.Close()
	for rrows.Next() {
		var recordingID int64
		var rd federation.CatalogRendition
		if err := rrows.Scan(&recordingID, &rd.Hash, &rd.Size, &rd.Codec,
			&rd.Bitrate, &rd.SampleRate, &rd.BitDepth, &rd.Duration); err != nil {
			return nil, fmt.Errorf("scan catalog rendition: %w", err)
		}
		for _, i := range recordings[strconv.FormatInt(recordingID, 10)] {
			entries[i].Renditions = append(entries[i].Renditions, rd)
			if entries[i].Duration == 0 && rd.Duration > 0 {
				entries[i].Duration = rd.Duration
			}
		}
	}
	return entries, rrows.Err()
}

// ReplacePeerCatalog atomically replaces the cached copy of one friend's
// catalog with a fresh snapshot and records the snapshot serial + sync time.
func (db *DB) ReplacePeerCatalog(ctx context.Context, peerID int64, serial string, syncedAt int64, entries []federation.CatalogEntry) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace peer catalog: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM federation_catalog WHERE peer_id = ?`, peerID); err != nil {
		return fmt.Errorf("clear peer catalog: %w", err)
	}
	ins, err := tx.PrepareContext(ctx, `
		INSERT INTO federation_catalog (peer_id, entry_key, recording_key, title, artist,
			album_artist, album, genre, year, track_number, disc_number, duration,
			license, guest_playable, renditions)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare peer catalog insert: %w", err)
	}
	defer ins.Close()
	for _, e := range entries {
		if e.Key == "" || e.Title == "" {
			continue // remote input — skip rows that cannot be displayed or re-keyed
		}
		renditions, err := json.Marshal(e.Renditions)
		if err != nil {
			return fmt.Errorf("marshal renditions: %w", err)
		}
		if _, err := ins.ExecContext(ctx, peerID, e.Key, e.RecordingKey, e.Title, e.Artist,
			e.AlbumArtist, e.Album, e.Genre, e.Year, e.TrackNumber, e.DiscNumber,
			nullFloat(e.Duration), e.License, e.GuestPlayable, string(renditions)); err != nil {
			return fmt.Errorf("insert peer catalog entry: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE federation_peers SET catalog_serial = ?, catalog_synced_at = ? WHERE id = ?`,
		serial, syncedAt, peerID); err != nil {
		return fmt.Errorf("update peer sync state: %w", err)
	}
	return tx.Commit()
}

// MarkPeerCatalogChecked records a sync round that confirmed the cached copy
// is still fresh (the not-modified path).
func (db *DB) MarkPeerCatalogChecked(ctx context.Context, peerID int64, serial string, syncedAt int64) error {
	_, err := db.ExecContext(ctx, `
		UPDATE federation_peers SET catalog_serial = ?, catalog_synced_at = ? WHERE id = ?`,
		serial, syncedAt, peerID)
	if err != nil {
		return fmt.Errorf("mark peer catalog checked: %w", err)
	}
	return nil
}

// ── Merged browse (the /madnetwork drill-down) ───────────────────────────────

// The browse queries group by DISPLAY identity — the grouping artist is the
// album artist, falling back to the performer, falling back to the unknown
// bucket, mirroring the local library's album-artist-only artist list; albums
// fall back to the shared "Other" bucket. Only friends' catalogs are visible.
const fedcatBase = `
	FROM (SELECT COALESCE(NULLIF(c.album_artist, ''), NULLIF(c.artist, ''), 'Unknown artist') AS akey,
	             COALESCE(NULLIF(c.album, ''), 'Other') AS alb,
	             c.*
	      FROM federation_catalog c
	      JOIN federation_peers p ON p.id = c.peer_id AND p.state = 'friend')`

// trackIdent is the merged logical-track identity inside one artist bucket:
// album + disc + track + title (case-insensitive) — the same text offered by
// several friends is ONE row.
const trackIdent = `lower(alb) || char(31) || COALESCE(disc_number, -1) || char(31) ||
	COALESCE(track_number, -1) || char(31) || lower(title)`

// MadnetworkArtist is one row of the merged artist list.
type MadnetworkArtist struct {
	Name   string `json:"name"`
	Albums int64  `json:"albums"`
	Tracks int64  `json:"tracks"`
}

// MadnetworkArtists lists the merged catalog's artists (display-identity
// grouping, case-insensitive), optionally filtered by a substring.
func (db *DB) MadnetworkArtists(ctx context.Context, q string) ([]*MadnetworkArtist, error) {
	where, args := "", []any{}
	if s := strings.TrimSpace(q); s != "" {
		escaped := strings.NewReplacer(`%`, `\%`, `_`, `\_`).Replace(s)
		where = ` WHERE lower(akey) LIKE lower(?) ESCAPE '\'`
		args = append(args, "%"+escaped+"%")
	}
	rows, err := db.QueryContext(ctx, `
		SELECT MIN(akey), COUNT(DISTINCT lower(alb)), COUNT(DISTINCT `+trackIdent+`)
		`+fedcatBase+where+`
		GROUP BY lower(akey)
		ORDER BY lower(akey)`, args...)
	if err != nil {
		return nil, fmt.Errorf("madnetwork artists: %w", err)
	}
	defer rows.Close()
	var out []*MadnetworkArtist
	for rows.Next() {
		var a MadnetworkArtist
		if err := rows.Scan(&a.Name, &a.Albums, &a.Tracks); err != nil {
			return nil, fmt.Errorf("scan madnetwork artist: %w", err)
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// MadnetworkAlbum is one row of an artist's merged album list.
type MadnetworkAlbum struct {
	Title  string `json:"title"`
	Tracks int64  `json:"tracks"`
	Year   *int64 `json:"year,omitempty"`
}

// MadnetworkAlbums lists one artist's albums in the merged catalog.
func (db *DB) MadnetworkAlbums(ctx context.Context, artist string) ([]*MadnetworkAlbum, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT MIN(alb), COUNT(DISTINCT `+trackIdent+`), MAX(year)
		`+fedcatBase+`
		WHERE lower(akey) = lower(?)
		GROUP BY lower(alb)
		ORDER BY year IS NULL, year, lower(alb)`, artist)
	if err != nil {
		return nil, fmt.Errorf("madnetwork albums: %w", err)
	}
	defer rows.Close()
	var out []*MadnetworkAlbum
	for rows.Next() {
		var a MadnetworkAlbum
		var year sql.NullInt64
		if err := rows.Scan(&a.Title, &a.Tracks, &year); err != nil {
			return nil, fmt.Errorf("scan madnetwork album: %w", err)
		}
		a.Year = nullInt(year)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// MadnetworkTrackRow is one RAW cached row of an album's tracks — one per
// (peer, appearance). The handler merges rows into logical tracks and their
// "N versions" (distinct claimed recordings) — set logic that reads better in
// Go than SQL at album scale.
type MadnetworkTrackRow struct {
	PeerID       int64
	PeerName     string
	PeerLastSeen int64
	Entry        federation.CatalogEntry
}

// MadnetworkTracks returns every friend's cached rows for one artist+album, in
// display order.
func (db *DB) MadnetworkTracks(ctx context.Context, artist, album string) ([]*MadnetworkTrackRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT peer_id, p2.name, p2.last_seen,
		       entry_key, recording_key, title, artist, album_artist, alb,
		       COALESCE(genre, ''), year, track_number, disc_number,
		       COALESCE(duration, 0), COALESCE(license, ''), guest_playable, renditions
		`+fedcatBase+`
		JOIN federation_peers p2 ON p2.id = peer_id
		WHERE lower(akey) = lower(?) AND lower(alb) = lower(?)
		ORDER BY (disc_number IS NULL) ASC, disc_number ASC, track_number ASC, lower(title) ASC, peer_id ASC`,
		artist, album)
	if err != nil {
		return nil, fmt.Errorf("madnetwork tracks: %w", err)
	}
	defer rows.Close()
	var out []*MadnetworkTrackRow
	for rows.Next() {
		var r MadnetworkTrackRow
		var year, track, disc sql.NullInt64
		var renditions string
		if err := rows.Scan(&r.PeerID, &r.PeerName, &r.PeerLastSeen,
			&r.Entry.Key, &r.Entry.RecordingKey, &r.Entry.Title, &r.Entry.Artist,
			&r.Entry.AlbumArtist, &r.Entry.Album, &r.Entry.Genre, &year, &track, &disc,
			&r.Entry.Duration, &r.Entry.License, &r.Entry.GuestPlayable, &renditions); err != nil {
			return nil, fmt.Errorf("scan madnetwork track: %w", err)
		}
		r.Entry.Year, r.Entry.TrackNumber, r.Entry.DiscNumber = nullInt(year), nullInt(track), nullInt(disc)
		if err := json.Unmarshal([]byte(renditions), &r.Entry.Renditions); err != nil {
			r.Entry.Renditions = nil // tolerate a damaged cache row rather than failing the album
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// MadnetworkFriend is one friend's sync status on the /madnetwork page strip.
type MadnetworkFriend struct {
	Name     string `json:"name"`
	LastSeen int64  `json:"last_seen"`
	SyncedAt int64  `json:"synced_at"`
	Entries  int64  `json:"entries"`
}

// MadnetworkSummary reports the merged catalog's shape: each friend with sync
// state and the merged distinct track count.
func (db *DB) MadnetworkSummary(ctx context.Context) ([]*MadnetworkFriend, int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT p.name, p.last_seen, p.catalog_synced_at,
		       (SELECT COUNT(*) FROM federation_catalog c WHERE c.peer_id = p.id)
		FROM federation_peers p
		WHERE p.state = 'friend'
		ORDER BY lower(p.name), p.id`)
	if err != nil {
		return nil, 0, fmt.Errorf("madnetwork summary: %w", err)
	}
	defer rows.Close()
	var friends []*MadnetworkFriend
	for rows.Next() {
		var f MadnetworkFriend
		if err := rows.Scan(&f.Name, &f.LastSeen, &f.SyncedAt, &f.Entries); err != nil {
			return nil, 0, fmt.Errorf("scan madnetwork friend: %w", err)
		}
		friends = append(friends, &f)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	var tracks int64
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT lower(akey) || char(31) || `+trackIdent+`)
		`+fedcatBase).Scan(&tracks); err != nil {
		return nil, 0, fmt.Errorf("madnetwork track count: %w", err)
	}
	return friends, tracks, nil
}

func nullInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func nullFloat(v float64) any {
	if v == 0 {
		return nil
	}
	return v
}
