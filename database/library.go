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

// listArtists is the shared artist listing over the entity overlay. One row per
// artists entity that has at least one live (and, when guest, reachable) track;
// the INNER JOIN on media_metadata means orphan entities with no tracks (e.g.
// left behind by a rename) never appear. has_image still matches the
// string-keyed artist_images by display name until the Phase 4 cover re-key.
func (db *DB) listArtists(ctx context.Context, guest bool) ([]*ArtistEntry, error) {
	where := "WHERE f.deleted_at IS NULL"
	if guest {
		where += " AND " + accessClause
	}
	q := `
		SELECT a.id, a.name, COUNT(*) AS track_count,
		       CASE WHEN ai.artist_name IS NOT NULL THEN 1 ELSE 0 END AS has_image
		FROM artists a
		JOIN media_metadata m ON m.artist_id = a.id
		JOIN files f ON f.id = m.file_id
		LEFT JOIN artist_images ai ON ai.artist_name = a.name
		` + where + `
		GROUP BY a.id
		ORDER BY LOWER(a.name) ASC`

	rows, err := db.QueryContext(ctx, q)
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

func (db *DB) ListAlbumsByArtist(ctx context.Context, artist string) ([]*AlbumEntry, error) {
	return db.listAlbumsByArtist(ctx, artist, false)
}

// ListAlbumsByArtistGuest is ListAlbumsByArtist restricted to albums an
// anonymous / capability-less request can reach at least one track of.
func (db *DB) ListAlbumsByArtistGuest(ctx context.Context, artist string) ([]*AlbumEntry, error) {
	return db.listAlbumsByArtist(ctx, artist, true)
}

// listAlbumsByArtist is the shared album listing over the entity overlay. An
// empty artist returns every album; otherwise it filters by the artist entity
// resolved from the name (normalized the same way the resolver keys it). One row
// per albums entity with at least one live (and, when guest, reachable) track.
func (db *DB) listAlbumsByArtist(ctx context.Context, artist string, guest bool) ([]*AlbumEntry, error) {
	where := "WHERE f.deleted_at IS NULL AND (? = '' OR ar.norm_name = ?)"
	if guest {
		where += " AND " + accessClause
	}
	q := `
		SELECT al.id, al.title, ar.name AS artist_name, COALESCE(al.year, 0) AS year,
		       COUNT(*) AS track_count,
		       CASE WHEN ali.album_title IS NOT NULL THEN 1 ELSE 0 END AS has_image
		FROM albums al
		JOIN artists ar ON ar.id = al.artist_id
		JOIN media_metadata m ON m.album_id = al.id
		JOIN files f ON f.id = m.file_id
		LEFT JOIN album_images ali ON ali.album_artist = ar.name AND ali.album_title = al.title
		` + where + `
		GROUP BY al.id
		ORDER BY year ASC, LOWER(al.title) ASC`

	rows, err := db.QueryContext(ctx, q, artist, normalizeKey(artist))
	if err != nil {
		return nil, fmt.Errorf("list albums by artist: %w", err)
	}
	defer rows.Close()

	var out []*AlbumEntry
	for rows.Next() {
		var e AlbumEntry
		var year int64
		var hasImage int
		if err := rows.Scan(&e.ID, &e.Title, &e.ArtistName, &year, &e.TrackCount, &hasImage); err != nil {
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

func (db *DB) ListTracksByAlbumArtist(ctx context.Context, artist, album string) ([]*TrackEntry, error) {
	return db.listTracksByAlbumArtist(ctx, artist, album, false)
}

// ListTracksByAlbumArtistGuest is ListTracksByAlbumArtist restricted to the
// tracks an anonymous / capability-less request may play/download.
func (db *DB) ListTracksByAlbumArtistGuest(ctx context.Context, artist, album string) ([]*TrackEntry, error) {
	return db.listTracksByAlbumArtist(ctx, artist, album, true)
}

// listTracksByAlbumArtist is the shared track listing for a single (artist,
// album) entity, identified by the normalized name keys. An empty artist returns
// nil — the no-artist bucket is not browsable as a drill-down (unchanged
// behavior). The empty-string album resolves to the unknown-album entity, so the
// "Other" bucket's tracks remain reachable under a named artist.
func (db *DB) listTracksByAlbumArtist(ctx context.Context, artist, album string, guest bool) ([]*TrackEntry, error) {
	if artist == "" {
		return nil, nil
	}
	where := "WHERE f.deleted_at IS NULL AND ar.norm_name = ? AND al.norm_title = ?"
	if guest {
		where += " AND " + accessClause
	}
	q := `
		SELECT
		    f.id,
		    COALESCE(NULLIF(m.title, ''), fu.filename, '') AS title,
		    m.track_number,
		    m.duration_seconds,
		    f.object_key,
		    f.mime_type
		FROM files f
		JOIN media_metadata m ON m.file_id = f.id
		JOIN albums al ON al.id = m.album_id
		JOIN artists ar ON ar.id = al.artist_id
		LEFT JOIN (
		    SELECT file_id, MIN(filename) AS filename
		    FROM file_uploads
		    GROUP BY file_id
		) fu ON fu.file_id = f.id
		` + where + `
		ORDER BY m.track_number ASC, LOWER(COALESCE(NULLIF(m.title, ''), fu.filename, '')) ASC`

	rows, err := db.QueryContext(ctx, q, normalizeKey(artist), normalizeKey(album))
	if err != nil {
		return nil, fmt.Errorf("list tracks by album artist: %w", err)
	}
	defer rows.Close()

	var out []*TrackEntry
	for rows.Next() {
		var e TrackEntry
		if err := rows.Scan(&e.ID, &e.Title, &e.TrackNumber, &e.DurationSeconds, &e.ObjectKey, &e.MimeType); err != nil {
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
	artistWhere := "WHERE f.deleted_at IS NULL AND LOWER(a.name) LIKE LOWER(?) ESCAPE '\\'"
	artistArgs := []any{like}
	if filtered {
		artistWhere += " AND " + accessClause
	}
	artistQ := `
		SELECT a.id, a.name, COUNT(*) AS track_count,
		       CASE WHEN ai.artist_name IS NOT NULL THEN 1 ELSE 0 END AS has_image
		FROM artists a
		JOIN media_metadata m ON m.artist_id = a.id
		JOIN files f ON f.id = m.file_id
		LEFT JOIN artist_images ai ON ai.artist_name = a.name
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
	albumWhere := "WHERE f.deleted_at IS NULL AND al.title != '' AND LOWER(al.title) LIKE LOWER(?) ESCAPE '\\'"
	albumArgs := []any{like}
	if filtered {
		albumWhere += " AND " + accessClause
	}
	albumQ := `
		SELECT al.id, al.title, ar.name AS artist_name, COALESCE(al.year, 0) AS year,
		       COUNT(*) AS track_count,
		       CASE WHEN ali.album_title IS NOT NULL THEN 1 ELSE 0 END AS has_image
		FROM albums al
		JOIN artists ar ON ar.id = al.artist_id
		JOIN media_metadata m ON m.album_id = al.id
		JOIN files f ON f.id = m.file_id
		LEFT JOIN album_images ali ON ali.album_artist = ar.name AND ali.album_title = al.title
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
		if err := alRows.Scan(&e.ID, &e.Title, &e.ArtistName, &year, &e.TrackCount, &hasImage); err != nil {
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
	trackWhere := "WHERE f.deleted_at IS NULL AND LOWER(COALESCE(NULLIF(m.title, ''), fu.filename, '')) LIKE LOWER(?) ESCAPE '\\'"
	trackArgs := []any{like}
	if filtered {
		trackWhere += " AND " + accessClause
	}
	trackQ := `
		SELECT
		    f.id,
		    COALESCE(NULLIF(m.title, ''), fu.filename, '') AS title,
		    m.track_number,
		    m.duration_seconds,
		    f.object_key,
		    f.mime_type,
		    ar.name AS artist_name,
		    al.title AS album_title
		FROM files f
		JOIN media_metadata m ON m.file_id = f.id
		JOIN albums al ON al.id = m.album_id
		JOIN artists ar ON ar.id = al.artist_id
		LEFT JOIN (
		    SELECT file_id, MIN(filename) AS filename
		    FROM file_uploads
		    GROUP BY file_id
		) fu ON fu.file_id = f.id
		` + trackWhere + `
		ORDER BY LOWER(COALESCE(NULLIF(m.title, ''), fu.filename, '')) ASC
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

func (db *DB) UpsertArtistImage(ctx context.Context, artist, objectKey, mimeType string, updatedAt int64) error {
	_, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO artist_images (artist_name, object_key, mime_type, updated_at) VALUES (?, ?, ?, ?)`,
		artist, objectKey, mimeType, updatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert artist image: %w", err)
	}
	return nil
}

func (db *DB) UpsertAlbumImage(ctx context.Context, artist, album, objectKey, mimeType string, updatedAt int64) error {
	_, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO album_images (album_artist, album_title, object_key, mime_type, updated_at) VALUES (?, ?, ?, ?, ?)`,
		artist, album, objectKey, mimeType, updatedAt,
	)
	if err != nil {
		return fmt.Errorf("upsert album image: %w", err)
	}
	return nil
}

func (db *DB) GetArtistImage(ctx context.Context, artist string) (objectKey, mimeType string, found bool, err error) {
	row := db.QueryRowContext(ctx,
		`SELECT object_key, mime_type FROM artist_images WHERE artist_name = ?`,
		artist,
	)
	if err = row.Scan(&objectKey, &mimeType); errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("get artist image: %w", err)
	}
	return objectKey, mimeType, true, nil
}

func (db *DB) GetAlbumImage(ctx context.Context, artist, album string) (objectKey, mimeType string, found bool, err error) {
	row := db.QueryRowContext(ctx,
		`SELECT object_key, mime_type FROM album_images WHERE album_artist = ? AND album_title = ?`,
		artist, album,
	)
	if err = row.Scan(&objectKey, &mimeType); errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, fmt.Errorf("get album image: %w", err)
	}
	return objectKey, mimeType, true, nil
}
