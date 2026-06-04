package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func (db *DB) ListArtists(ctx context.Context) ([]*ArtistEntry, error) {
	const q = `
		SELECT
		    COALESCE(NULLIF(m.album_artist, ''), NULLIF(m.artist, ''), '') AS name,
		    COUNT(*) AS track_count,
		    CASE WHEN ai.artist_name IS NOT NULL THEN 1 ELSE 0 END AS has_image
		FROM media_metadata m
		JOIN files f ON f.id = m.file_id
		LEFT JOIN artist_images ai
		    ON ai.artist_name = COALESCE(NULLIF(m.album_artist, ''), NULLIF(m.artist, ''), '')
		WHERE f.deleted_at IS NULL
		GROUP BY name
		ORDER BY LOWER(name) ASC`

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list artists: %w", err)
	}
	defer rows.Close()

	var out []*ArtistEntry
	for rows.Next() {
		var e ArtistEntry
		var hasImage int
		if err := rows.Scan(&e.Name, &e.TrackCount, &hasImage); err != nil {
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

// ListArtistsFiltered is ListArtists restricted to artists the user (invalid
// userID = anonymous) can reach at least one track of, per the §5.3 access
// predicate. Track counts reflect only reachable tracks.
func (db *DB) ListArtistsFiltered(ctx context.Context, userID sql.NullInt64) ([]*ArtistEntry, error) {
	const q = `
		SELECT
		    COALESCE(NULLIF(m.album_artist, ''), NULLIF(m.artist, ''), '') AS name,
		    COUNT(*) AS track_count,
		    CASE WHEN ai.artist_name IS NOT NULL THEN 1 ELSE 0 END AS has_image
		FROM media_metadata m
		JOIN files f ON f.id = m.file_id
		LEFT JOIN artist_images ai
		    ON ai.artist_name = COALESCE(NULLIF(m.album_artist, ''), NULLIF(m.artist, ''), '')
		WHERE f.deleted_at IS NULL AND ` + accessClause + `
		GROUP BY name
		ORDER BY LOWER(name) ASC`

	rows, err := db.QueryContext(ctx, q, userID)
	if err != nil {
		return nil, fmt.Errorf("list artists filtered: %w", err)
	}
	defer rows.Close()

	var out []*ArtistEntry
	for rows.Next() {
		var e ArtistEntry
		var hasImage int
		if err := rows.Scan(&e.Name, &e.TrackCount, &hasImage); err != nil {
			return nil, fmt.Errorf("scan artist entry: %w", err)
		}
		e.HasImage = hasImage == 1
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list artists filtered rows: %w", err)
	}
	return out, nil
}

func (db *DB) ListAlbumsByArtist(ctx context.Context, artist string) ([]*AlbumEntry, error) {
	const q = `
		SELECT
		    COALESCE(NULLIF(m.album, ''), '') AS title,
		    COALESCE(NULLIF(m.album_artist, ''), NULLIF(m.artist, ''), '') AS artist_name,
		    COALESCE(m.year, 0) AS year,
		    COUNT(*) AS track_count,
		    CASE WHEN ali.album_title IS NOT NULL THEN 1 ELSE 0 END AS has_image
		FROM media_metadata m
		JOIN files f ON f.id = m.file_id
		LEFT JOIN album_images ali
		    ON ali.album_artist = COALESCE(NULLIF(m.album_artist, ''), NULLIF(m.artist, ''), '')
		    AND ali.album_title = COALESCE(NULLIF(m.album, ''), '')
		WHERE f.deleted_at IS NULL
		  AND (? = '' OR COALESCE(NULLIF(m.album_artist, ''), NULLIF(m.artist, ''), '') = ?)
		GROUP BY COALESCE(NULLIF(m.album, ''), ''), COALESCE(NULLIF(m.album_artist, ''), NULLIF(m.artist, ''), '')
		ORDER BY year ASC, LOWER(COALESCE(NULLIF(m.album, ''), '')) ASC`

	rows, err := db.QueryContext(ctx, q, artist, artist)
	if err != nil {
		return nil, fmt.Errorf("list albums by artist: %w", err)
	}
	defer rows.Close()

	var out []*AlbumEntry
	for rows.Next() {
		var e AlbumEntry
		var year int64
		var hasImage int
		if err := rows.Scan(&e.Title, &e.ArtistName, &year, &e.TrackCount, &hasImage); err != nil {
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

// ListAlbumsByArtistFiltered is ListAlbumsByArtist restricted to albums the
// user (invalid userID = anonymous) can reach at least one track of.
func (db *DB) ListAlbumsByArtistFiltered(ctx context.Context, artist string, userID sql.NullInt64) ([]*AlbumEntry, error) {
	const q = `
		SELECT
		    COALESCE(NULLIF(m.album, ''), '') AS title,
		    COALESCE(NULLIF(m.album_artist, ''), NULLIF(m.artist, ''), '') AS artist_name,
		    COALESCE(m.year, 0) AS year,
		    COUNT(*) AS track_count,
		    CASE WHEN ali.album_title IS NOT NULL THEN 1 ELSE 0 END AS has_image
		FROM media_metadata m
		JOIN files f ON f.id = m.file_id
		LEFT JOIN album_images ali
		    ON ali.album_artist = COALESCE(NULLIF(m.album_artist, ''), NULLIF(m.artist, ''), '')
		    AND ali.album_title = COALESCE(NULLIF(m.album, ''), '')
		WHERE f.deleted_at IS NULL
		  AND (? = '' OR COALESCE(NULLIF(m.album_artist, ''), NULLIF(m.artist, ''), '') = ?)
		  AND ` + accessClause + `
		GROUP BY COALESCE(NULLIF(m.album, ''), ''), COALESCE(NULLIF(m.album_artist, ''), NULLIF(m.artist, ''), '')
		ORDER BY year ASC, LOWER(COALESCE(NULLIF(m.album, ''), '')) ASC`

	rows, err := db.QueryContext(ctx, q, artist, artist, userID)
	if err != nil {
		return nil, fmt.Errorf("list albums by artist filtered: %w", err)
	}
	defer rows.Close()

	var out []*AlbumEntry
	for rows.Next() {
		var e AlbumEntry
		var year int64
		var hasImage int
		if err := rows.Scan(&e.Title, &e.ArtistName, &year, &e.TrackCount, &hasImage); err != nil {
			return nil, fmt.Errorf("scan album entry: %w", err)
		}
		if year != 0 {
			e.Year = sql.NullInt64{Int64: year, Valid: true}
		}
		e.HasImage = hasImage == 1
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list albums filtered rows: %w", err)
	}
	return out, nil
}

func (db *DB) ListTracksByAlbumArtist(ctx context.Context, artist, album string) ([]*TrackEntry, error) {
	if artist == "" {
		return nil, nil
	}

	const q = `
		SELECT
		    f.id,
		    COALESCE(NULLIF(m.title, ''), fu.filename, '') AS title,
		    m.track_number,
		    m.duration_seconds,
		    f.object_key,
		    f.mime_type
		FROM files f
		JOIN media_metadata m ON m.file_id = f.id
		LEFT JOIN (
		    SELECT file_id, MIN(filename) AS filename
		    FROM file_uploads
		    GROUP BY file_id
		) fu ON fu.file_id = f.id
		WHERE f.deleted_at IS NULL
		  AND COALESCE(NULLIF(m.album_artist, ''), NULLIF(m.artist, ''), '') = ?
		  AND COALESCE(NULLIF(m.album, ''), '') = ?
		ORDER BY m.track_number ASC, LOWER(COALESCE(NULLIF(m.title, ''), fu.filename, '')) ASC`

	rows, err := db.QueryContext(ctx, q, artist, album)
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

// ListTracksByAlbumArtistFiltered is ListTracksByAlbumArtist restricted to the
// tracks the user (invalid userID = anonymous) may play/download.
func (db *DB) ListTracksByAlbumArtistFiltered(ctx context.Context, artist, album string, userID sql.NullInt64) ([]*TrackEntry, error) {
	if artist == "" {
		return nil, nil
	}

	const q = `
		SELECT
		    f.id,
		    COALESCE(NULLIF(m.title, ''), fu.filename, '') AS title,
		    m.track_number,
		    m.duration_seconds,
		    f.object_key,
		    f.mime_type
		FROM files f
		JOIN media_metadata m ON m.file_id = f.id
		LEFT JOIN (
		    SELECT file_id, MIN(filename) AS filename
		    FROM file_uploads
		    GROUP BY file_id
		) fu ON fu.file_id = f.id
		WHERE f.deleted_at IS NULL
		  AND COALESCE(NULLIF(m.album_artist, ''), NULLIF(m.artist, ''), '') = ?
		  AND COALESCE(NULLIF(m.album, ''), '') = ?
		  AND ` + accessClause + `
		ORDER BY m.track_number ASC, LOWER(COALESCE(NULLIF(m.title, ''), fu.filename, '')) ASC`

	rows, err := db.QueryContext(ctx, q, artist, album, userID)
	if err != nil {
		return nil, fmt.Errorf("list tracks by album artist filtered: %w", err)
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
		return nil, fmt.Errorf("list tracks filtered rows: %w", err)
	}
	return out, nil
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
