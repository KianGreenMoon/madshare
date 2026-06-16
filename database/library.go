package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

func (db *DB) ListArtists(ctx context.Context) ([]*ArtistEntry, error) {
	return db.listArtists(ctx, false)
}

// ListArtistsGuest is ListArtists restricted to artists an anonymous /
// capability-less request can reach at least one track of (the guest-playable /
// license policy). Track counts reflect only reachable tracks.
func (db *DB) ListArtistsGuest(ctx context.Context) ([]*ArtistEntry, error) {
	return db.listArtists(ctx, true)
}

// listArtists is the shared artist listing over the entity overlay — the library
// browse list. One row per artists entity that is the ALBUM-ARTIST of at least one
// live (and, when guest, reachable) track (m.album_artist_id = a.id). A pure
// performer who only appears on a compilation (its album_artist owned by someone
// else) is intentionally NOT listed here — the library groups by album-artist —
// but it stays findable via search (which matches both roles) and browsable by id
// (ListAlbumsByArtistID unions the performer role, so a search hit isn't a dead
// end). The INNER JOIN on media_metadata means orphan entities with no tracks (e.g.
// left behind by a rename) never appear. has_image joins the now id-keyed
// artist_images directly on artist_id (exact). The unknown-artist bucket
// (norm_name = normalizeKey(DefaultArtistName)) sorts last, after the named
// artists, via the leading ORDER BY key.
func (db *DB) listArtists(ctx context.Context, guest bool) ([]*ArtistEntry, error) {
	where := "WHERE " + visibleFile
	if guest {
		where += " AND " + accessClause
	}
	q := `
		SELECT a.id, a.name, COUNT(*) AS track_count,
		       CASE WHEN ai.artist_id IS NOT NULL THEN 1 ELSE 0 END AS has_image
		FROM artists a
		JOIN media_metadata m ON m.album_artist_id = a.id
		JOIN files f ON f.id = m.file_id
		LEFT JOIN artist_images ai ON ai.artist_id = a.id
		` + where + `
		GROUP BY a.id
		ORDER BY a.norm_name = ? ASC, LOWER(a.name) ASC`

	rows, err := db.QueryContext(ctx, q, normalizeKey(DefaultArtistName))
	if err != nil {
		return nil, fmt.Errorf("list artists: %w", err)
	}
	defer rows.Close()

	var out []*ArtistEntry
	for rows.Next() {
		var e ArtistEntry
		var hasImage int
		if err := rows.Scan(&e.ID, &e.Name, &e.TrackCount, &hasImage); err != nil {
			return nil, fmt.Errorf("scan artist entry: %w", err)
		}
		e.HasImage = hasImage == 1
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list artists rows: %w", err)
	}
	return out, nil
}

func (db *DB) ListAlbumsByArtistID(ctx context.Context, artistID int64) ([]*AlbumEntry, error) {
	return db.listAlbumsByArtistID(ctx, artistID, false)
}

// ListAlbumsByArtistIDGuest is ListAlbumsByArtistID restricted to albums an
// anonymous / capability-less request can reach at least one track of.
func (db *DB) ListAlbumsByArtistIDGuest(ctx context.Context, artistID int64) ([]*AlbumEntry, error) {
	return db.listAlbumsByArtistID(ctx, artistID, true)
}

// listAlbumsByArtistID is the shared album listing over the entity overlay,
// filtered to one artist by its stable surrogate id — in EITHER role. The artist
// filter lives in the media_metadata join condition (al.artist_id = ? OR
// m.artist_id = ?), which yields a useful hybrid track_count: an album the artist
// owns as album-artist counts all its tracks (every row matches the first branch),
// while a compilation the artist only performs on counts just their tracks on it
// (only the performer rows match). So a pure performer's drill-down lists the
// comps / features they appear on. The unknown-album bucket (norm_title =
// normalizeKey(DefaultAlbumTitle)) sorts last via the leading ORDER BY key.
func (db *DB) listAlbumsByArtistID(ctx context.Context, artistID int64, guest bool) ([]*AlbumEntry, error) {
	where := "WHERE " + visibleFile
	if guest {
		where += " AND " + accessClause
	}
	q := `
		SELECT al.id, al.artist_id, al.title, ar.name AS artist_name, COALESCE(al.year, 0) AS year,
		       COUNT(*) AS track_count,
		       CASE WHEN ali.album_id IS NOT NULL THEN 1 ELSE 0 END AS has_image
		FROM albums al
		JOIN artists ar ON ar.id = al.artist_id
		JOIN media_metadata m ON m.album_id = al.id AND (al.artist_id = ? OR m.artist_id = ?)
		JOIN files f ON f.id = m.file_id
		LEFT JOIN album_images ali ON ali.album_id = al.id
		` + where + `
		GROUP BY al.id
		ORDER BY al.norm_title = ? ASC, year ASC, LOWER(al.title) ASC`

	rows, err := db.QueryContext(ctx, q, artistID, artistID, normalizeKey(DefaultAlbumTitle))
	if err != nil {
		return nil, fmt.Errorf("list albums by artist: %w", err)
	}
	defer rows.Close()

	var out []*AlbumEntry
	for rows.Next() {
		var e AlbumEntry
		var year int64
		var hasImage int
		if err := rows.Scan(&e.ID, &e.ArtistID, &e.Title, &e.ArtistName, &year, &e.TrackCount, &hasImage); err != nil {
			return nil, fmt.Errorf("scan album entry: %w", err)
		}
		if year != 0 {
			e.Year = sql.NullInt64{Int64: year, Valid: true}
		}
		e.HasImage = hasImage == 1
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list albums rows: %w", err)
	}
	return out, nil
}

func (db *DB) ListTracksByAlbumID(ctx context.Context, albumID int64) ([]*TrackEntry, error) {
	return db.listTracksByAlbumID(ctx, albumID, false)
}

// ListTracksByAlbumIDGuest is ListTracksByAlbumID restricted to the tracks an
// anonymous / capability-less request may play/download.
func (db *DB) ListTracksByAlbumIDGuest(ctx context.Context, albumID int64) ([]*TrackEntry, error) {
	return db.listTracksByAlbumID(ctx, albumID, true)
}

// listTracksByAlbumID is the shared track listing for a single album entity,
// identified by its stable surrogate id (which already pins the album-artist,
// since an album belongs to one artist). The "Other" bucket's tracks are reached
// the same way — by passing the unknown-album entity's id. Each row carries the
// track's *performer* name (its artist_id entity), which differs from the
// album-artist on a compilation; matches the playlists page's per-track artist.
func (db *DB) listTracksByAlbumID(ctx context.Context, albumID int64, guest bool) ([]*TrackEntry, error) {
	where := "WHERE " + visibleFile + " AND m.album_id = ?"
	if guest {
		where += " AND " + accessClause
	}
	q := `
		SELECT
		    f.id,
		    m.title,
		    COALESCE(par.name, '') AS artist_name,
		    m.track_number,
		    m.disc_number,
		    m.duration_seconds,
		    f.object_key,
		    f.mime_type
		FROM files f
		JOIN media_metadata m ON m.file_id = f.id
		LEFT JOIN artists par ON par.id = m.artist_id
		` + where + `
		ORDER BY (m.disc_number IS NULL) ASC, m.disc_number ASC, m.track_number ASC, LOWER(m.title) ASC`

	rows, err := db.QueryContext(ctx, q, albumID)
	if err != nil {
		return nil, fmt.Errorf("list tracks by album: %w", err)
	}
	defer rows.Close()

	var out []*TrackEntry
	for rows.Next() {
		var e TrackEntry
		if err := rows.Scan(&e.ID, &e.Title, &e.ArtistName, &e.TrackNumber, &e.DiscNumber, &e.DurationSeconds, &e.ObjectKey, &e.MimeType); err != nil {
			return nil, fmt.Errorf("scan track entry: %w", err)
		}
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tracks rows: %w", err)
	}
	return out, nil
}

func (db *DB) Search(ctx context.Context, q string) (*SearchResults, error) {
	return db.search(ctx, q, false)
}

func (db *DB) SearchGuest(ctx context.Context, q string) (*SearchResults, error) {
	return db.search(ctx, q, true)
}

func (db *DB) search(ctx context.Context, q string, filtered bool) (*SearchResults, error) {
	if strings.TrimSpace(q) == "" {
		return &SearchResults{}, nil
	}
	// Escape SQLite LIKE metacharacters so they are treated as literals.
	escaped := strings.NewReplacer(`%`, `\%`, `_`, `\_`).Replace(q)
	like := "%" + escaped + "%"

	// ── Artists ──────────────────────────────────────────────────────────────
	artistWhere := "WHERE " + visibleFile + " AND LOWER(a.name) LIKE LOWER(?) ESCAPE '\\'"
	artistArgs := []any{like}
	if filtered {
		artistWhere += " AND " + accessClause
	}
	artistQ := `
		SELECT a.id, a.name, COUNT(*) AS track_count,
		       CASE WHEN ai.artist_id IS NOT NULL THEN 1 ELSE 0 END AS has_image
		FROM artists a
		JOIN media_metadata m ON (m.album_artist_id = a.id OR m.artist_id = a.id)
		JOIN files f ON f.id = m.file_id
		LEFT JOIN artist_images ai ON ai.artist_id = a.id
		` + artistWhere + `
		GROUP BY a.id
		ORDER BY LOWER(a.name) ASC
		LIMIT 50`

	aRows, err := db.QueryContext(ctx, artistQ, artistArgs...)
	if err != nil {
		return nil, fmt.Errorf("search artists: %w", err)
	}
	defer aRows.Close()
	var artists []*ArtistEntry
	for aRows.Next() {
		var e ArtistEntry
		var hasImage int
		if err := aRows.Scan(&e.ID, &e.Name, &e.TrackCount, &hasImage); err != nil {
			return nil, fmt.Errorf("scan search artist: %w", err)
		}
		e.HasImage = hasImage == 1
		artists = append(artists, &e)
	}
	if err := aRows.Err(); err != nil {
		return nil, fmt.Errorf("search artists rows: %w", err)
	}

	// ── Albums ───────────────────────────────────────────────────────────────
	albumWhere := "WHERE " + visibleFile + " AND LOWER(al.title) LIKE LOWER(?) ESCAPE '\\'"
	albumArgs := []any{like}
	if filtered {
		albumWhere += " AND " + accessClause
	}
	albumQ := `
		SELECT al.id, al.artist_id, al.title, ar.name AS artist_name, COALESCE(al.year, 0) AS year,
		       COUNT(*) AS track_count,
		       CASE WHEN ali.album_id IS NOT NULL THEN 1 ELSE 0 END AS has_image
		FROM albums al
		JOIN artists ar ON ar.id = al.artist_id
		JOIN media_metadata m ON m.album_id = al.id
		JOIN files f ON f.id = m.file_id
		LEFT JOIN album_images ali ON ali.album_id = al.id
		` + albumWhere + `
		GROUP BY al.id
		ORDER BY LOWER(al.title) ASC
		LIMIT 50`

	alRows, err := db.QueryContext(ctx, albumQ, albumArgs...)
	if err != nil {
		return nil, fmt.Errorf("search albums: %w", err)
	}
	defer alRows.Close()
	var albums []*AlbumEntry
	for alRows.Next() {
		var e AlbumEntry
		var year int64
		var hasImage int
		if err := alRows.Scan(&e.ID, &e.ArtistID, &e.Title, &e.ArtistName, &year, &e.TrackCount, &hasImage); err != nil {
			return nil, fmt.Errorf("scan search album: %w", err)
		}
		if year != 0 {
			e.Year = sql.NullInt64{Int64: year, Valid: true}
		}
		e.HasImage = hasImage == 1
		albums = append(albums, &e)
	}
	if err := alRows.Err(); err != nil {
		return nil, fmt.Errorf("search albums rows: %w", err)
	}

	// ── Tracks ───────────────────────────────────────────────────────────────
	// Match the track title OR its performer name, so searching an artist surfaces
	// their tracks even on a "Various Artists" compilation. The displayed
	// artist_name is the performer (par.name, the track's artist_id entity), not the
	// album-artist — consistent with the track list and the playlists page.
	trackWhere := "WHERE " + visibleFile +
		" AND (LOWER(m.title) LIKE LOWER(?) ESCAPE '\\' OR LOWER(par.name) LIKE LOWER(?) ESCAPE '\\')"
	trackArgs := []any{like, like}
	if filtered {
		trackWhere += " AND " + accessClause
	}
	trackQ := `
		SELECT
		    f.id,
		    m.title,
		    m.track_number,
		    m.duration_seconds,
		    f.object_key,
		    f.mime_type,
		    par.name AS artist_name,
		    al.title AS album_title
		FROM files f
		JOIN media_metadata m ON m.file_id = f.id
		JOIN albums al ON al.id = m.album_id
		JOIN artists par ON par.id = m.artist_id
		` + trackWhere + `
		ORDER BY LOWER(m.title) ASC
		LIMIT 50`

	tRows, err := db.QueryContext(ctx, trackQ, trackArgs...)
	if err != nil {
		return nil, fmt.Errorf("search tracks: %w", err)
	}
	defer tRows.Close()
	var tracks []*SearchTrackEntry
	for tRows.Next() {
		var e SearchTrackEntry
		if err := tRows.Scan(&e.ID, &e.Title, &e.TrackNumber, &e.DurationSeconds, &e.ObjectKey, &e.MimeType, &e.ArtistName, &e.AlbumTitle); err != nil {
			return nil, fmt.Errorf("scan search track: %w", err)
		}
		tracks = append(tracks, &e)
	}
	if err := tRows.Err(); err != nil {
		return nil, fmt.Errorf("search tracks rows: %w", err)
	}

	return &SearchResults{Artists: artists, Albums: albums, Tracks: tracks}, nil
}

func (db *DB) UpsertArtistImage(ctx context.Context, artistID int64, objectKey, mimeType string, updatedAt int64) error {
	_, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO artist_images (artist_id, object_key, mime_type, updated_at) VALUES (?, ?, ?, ?)`,
		artistID, objectKey, mimeType, updatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert artist image: %w", err)
	}
	return nil
}

func (db *DB) UpsertAlbumImage(ctx context.Context, albumID int64, objectKey, mimeType string, updatedAt int64) error {
	_, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO album_images (album_id, object_key, mime_type, updated_at) VALUES (?, ?, ?, ?)`,
		albumID, objectKey, mimeType, updatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert album image: %w", err)
	}
	return nil
}

func (db *DB) GetArtistImage(ctx context.Context, artistID int64) (objectKey, mimeType string, found bool, err error) {
	row := db.QueryRowContext(ctx,
		`SELECT object_key, mime_type FROM artist_images WHERE artist_id = ?`,
		artistID,
	)
	if err = row.Scan(&objectKey, &mimeType); errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("get artist image: %w", err)
	}
	return objectKey, mimeType, true, nil
}

func (db *DB) GetAlbumImage(ctx context.Context, albumID int64) (objectKey, mimeType string, found bool, err error) {
	row := db.QueryRowContext(ctx,
		`SELECT object_key, mime_type FROM album_images WHERE album_id = ?`,
		albumID,
	)
	if err = row.Scan(&objectKey, &mimeType); errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("get album image: %w", err)
	}
	return objectKey, mimeType, true, nil
}
