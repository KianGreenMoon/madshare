package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrFileNotFound is returned when no files row matches a given content hash.
var ErrFileNotFound = errors.New("file not found")

// MetadataPatch carries the media_metadata fields editable via
// PATCH /api/files/{hash}/metadata. A nil pointer leaves that column unchanged;
// a non-nil pointer writes its value, so a pointer to "" clears the field.
//
// The numeric fields are carried as *string (the raw form input) rather than
// *int64 so they share the nil = unchanged / "" = clear / value = set trichotomy
// of the text fields; UpdateFileMetadata parses them (blank → NULL, otherwise a
// non-negative integer, else an error).
type MetadataPatch struct {
	// Base identity fields (also re-resolve the artist/album entity FKs).
	Title       *string
	Album       *string
	AlbumArtist *string
	Artist      *string
	// Extended tags — do not affect entity resolution.
	Genre       *string
	Composer    *string
	Comment     *string
	TrackNumber *string
	TrackTotal  *string
	DiscNumber  *string
	Year        *string
}

// IsEmpty reports whether the patch would change nothing (all fields nil).
func (p MetadataPatch) IsEmpty() bool {
	return p.Title == nil && p.Album == nil && p.AlbumArtist == nil && p.Artist == nil &&
		p.Genre == nil && p.Composer == nil && p.Comment == nil &&
		p.TrackNumber == nil && p.TrackTotal == nil && p.DiscNumber == nil && p.Year == nil
}

// UpdateFileMetadata writes the provided base fields onto the media_metadata row
// of the file identified by hash, then returns the resulting row. A nil field in
// the patch is left unchanged; a non-nil field is written (empty string stored
// as NULL, matching how tags are extracted). An empty patch is a no-op that
// still returns the current row, so callers get a consistent echo. Returns
// ErrFileNotFound when no files row matches hash.
//
// When the patch changes artist, album_artist, or album, the artist_id/album_id
// entity FKs are re-resolved so the track follows its new artist/album — this is
// a per-track *reclassification* (the track's own tags changed), which may move
// it to a different (or new) album entity. The album's cover, keyed by the
// album entity id, stays with the original album and does not follow the moved
// track. To rename an album/artist while keeping all its tracks and its cover
// attached, use the rename endpoints (RenameArtist/RenameAlbum) instead, which
// edit the entity in place. A reclassification that empties an album/artist may
// leave an orphan entity row; harmless (library queries JOIN through
// media_metadata) and reclaimed by the deferred merge/cleanup work.
func (db *DB) UpdateFileMetadata(ctx context.Context, hash string, p MetadataPatch) (*MediaMetadata, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("update metadata: begin: %w", err)
	}
	defer tx.Rollback()

	fileID, err := applyMetadataPatchTx(ctx, tx, hash, p)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("update metadata: commit: %w", err)
	}
	return db.getMetadataByFileID(ctx, fileID)
}

// BulkUpdateFileMetadata applies the same patch to every hash, sharing a
// transaction across a chunk of files instead of opening one (or three) per file
// — the metadata write genuinely re-resolves each file's artist/album entities so
// it can't collapse to a single UPDATE, but batching the transactions cuts the
// write-lock-acquire count that made the per-file loop a SQLITE_BUSY source over
// the "select all matching" scope. Returns the number of rows updated and the
// hashes that matched no file (skipped, not fatal — mirrors the per-file
// ErrFileNotFound the handler reported as a per-row failure). A patch-level error
// (e.g. ErrInvalidMetadata, identical for every file) or a real storage error
// aborts the current chunk and is returned.
func (db *DB) BulkUpdateFileMetadata(ctx context.Context, hashes []string, p MetadataPatch) (affected int, notFound []string, err error) {
	const chunk = 500 // bound how long one transaction holds the write lock
	for i := 0; i < len(hashes); i += chunk {
		end := i + chunk
		if end > len(hashes) {
			end = len(hashes)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return affected, notFound, fmt.Errorf("update metadata: begin: %w", err)
		}
		for _, hash := range hashes[i:end] {
			if _, e := applyMetadataPatchTx(ctx, tx, hash, p); e != nil {
				if errors.Is(e, ErrFileNotFound) {
					notFound = append(notFound, hash)
					continue
				}
				tx.Rollback()
				return affected, notFound, e
			}
			affected++
		}
		if e := tx.Commit(); e != nil {
			return affected, notFound, fmt.Errorf("update metadata: commit: %w", e)
		}
	}
	return affected, notFound, nil
}

// applyMetadataPatchTx writes one file's patch within tx (no commit), returning
// the file id. It resolves the file id first so an unknown hash is a clean
// ErrFileNotFound even for an empty patch, then writes the supplied fields and —
// when an identity field changed — re-resolves the artist/album entity FKs.
func applyMetadataPatchTx(ctx context.Context, tx *sql.Tx, hash string, p MetadataPatch) (int64, error) {
	var fileID int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM files WHERE hash = ?`, hash).Scan(&fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrFileNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("update metadata: lookup file: %w", err)
	}

	if !p.IsEmpty() {
		var sets []string
		var args []any
		if p.Title != nil {
			// Title is required non-empty (migration 016). Clearing it (or a
			// whitespace-only value) re-derives from the filename, the same default
			// the upload path uses, rather than storing NULL/''.
			title := *p.Title
			if strings.TrimSpace(title) == "" {
				fn, err := firstFilenameTx(ctx, tx, fileID)
				if err != nil {
					return 0, err
				}
				title = titleFromFilename(fn)
			}
			sets = append(sets, "title = ?")
			args = append(args, title)
		}
		if p.Album != nil {
			sets = append(sets, "album = ?")
			args = append(args, metaNullString(*p.Album))
		}
		if p.AlbumArtist != nil {
			sets = append(sets, "album_artist = ?")
			args = append(args, metaNullString(*p.AlbumArtist))
		}
		if p.Artist != nil {
			sets = append(sets, "artist = ?")
			args = append(args, metaNullString(*p.Artist))
		}
		// Extended string tags.
		if p.Genre != nil {
			sets = append(sets, "genre = ?")
			args = append(args, metaNullString(*p.Genre))
		}
		if p.Composer != nil {
			sets = append(sets, "composer = ?")
			args = append(args, metaNullString(*p.Composer))
		}
		if p.Comment != nil {
			sets = append(sets, "comment = ?")
			args = append(args, metaNullString(*p.Comment))
		}
		// Extended numeric tags (blank → NULL, else a non-negative integer).
		for _, nf := range []struct {
			col string
			val *string
		}{
			{"track_number", p.TrackNumber},
			{"track_total", p.TrackTotal},
			{"disc_number", p.DiscNumber},
			{"year", p.Year},
		} {
			if nf.val == nil {
				continue
			}
			n, err := metaNullInt(*nf.val)
			if err != nil {
				return 0, fmt.Errorf("update metadata: %s: %w", nf.col, err)
			}
			sets = append(sets, nf.col+" = ?")
			args = append(args, n)
		}
		args = append(args, fileID)
		if _, err := tx.ExecContext(ctx,
			`UPDATE media_metadata SET `+strings.Join(sets, ", ")+` WHERE file_id = ?`,
			args...,
		); err != nil {
			return 0, fmt.Errorf("update metadata: %w", err)
		}

		// Re-resolve entities only when an identity-affecting field changed.
		if p.Artist != nil || p.AlbumArtist != nil || p.Album != nil {
			var t AlbumArtistTags
			var artist, albumArtist, album sql.NullString
			var year sql.NullInt64
			if err := tx.QueryRowContext(ctx,
				`SELECT artist, album_artist, album, year FROM media_metadata WHERE file_id = ?`,
				fileID,
			).Scan(&artist, &albumArtist, &album, &year); err != nil {
				return 0, fmt.Errorf("update metadata: reload tags: %w", err)
			}
			t = AlbumArtistTags{
				Artist:      artist.String,
				AlbumArtist: albumArtist.String,
				Album:       album.String,
				Year:        int(year.Int64),
			}
			albumArtistID, trackArtistID, albumID, err := resolveAlbumArtistTx(ctx, tx, t)
			if err != nil {
				return 0, fmt.Errorf("update metadata: resolve entities: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE media_metadata SET album_artist_id = ?, artist_id = ?, album_id = ? WHERE file_id = ?`,
				albumArtistID, trackArtistID, albumID, fileID,
			); err != nil {
				return 0, fmt.Errorf("update metadata: set entity fks: %w", err)
			}
		}
	}

	return fileID, nil
}

// getMetadataByFileID loads the full media_metadata row for a file id.
func (db *DB) getMetadataByFileID(ctx context.Context, fileID int64) (*MediaMetadata, error) {
	m := &MediaMetadata{FileID: fileID}
	err := db.QueryRowContext(ctx, `
		SELECT title, artist, album, album_artist, genre, year,
		       track_number, track_total, disc_number, composer, comment,
		       duration_seconds, bitrate, sample_rate, channels, codec,
		       tag_format, extracted_at
		FROM media_metadata WHERE file_id = ?`, fileID,
	).Scan(
		&m.Title, &m.Artist, &m.Album, &m.AlbumArtist, &m.Genre, &m.Year,
		&m.TrackNumber, &m.TrackTotal, &m.DiscNumber, &m.Composer, &m.Comment,
		&m.DurationSeconds, &m.Bitrate, &m.SampleRate, &m.Channels, &m.Codec,
		&m.TagFormat, &m.ExtractedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		// A files row always gets a media_metadata row at insert time, so this
		// should not happen; treat it as not-found rather than a 500.
		return nil, ErrFileNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get metadata: %w", err)
	}
	return m, nil
}

// metaNullString stores "" as NULL, matching the tag-extraction convention so an
// edited-then-cleared field is indistinguishable from a never-set one.
func metaNullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// ErrInvalidMetadata flags a client-supplied value the patch can't accept (e.g.
// a non-numeric track number), so the API layer can map it to 400 rather than 500.
var ErrInvalidMetadata = errors.New("invalid metadata value")

// metaNullInt parses a numeric tag field carried as raw text: blank/whitespace
// clears the column (NULL); otherwise it must be a non-negative integer. A
// malformed or negative value is an ErrInvalidMetadata.
func metaNullInt(s string) (sql.NullInt64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return sql.NullInt64{}, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return sql.NullInt64{}, fmt.Errorf("%w: %q is not a non-negative integer", ErrInvalidMetadata, s)
	}
	return sql.NullInt64{Int64: n, Valid: true}, nil
}

// FileMetadataByHash loads the editable media_metadata row for the file with the
// given content hash. Returns ErrFileNotFound when no files row matches.
func (db *DB) FileMetadataByHash(ctx context.Context, hash string) (*MediaMetadata, error) {
	var fileID int64
	err := db.QueryRowContext(ctx, `SELECT id FROM files WHERE hash = ?`, hash).Scan(&fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrFileNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("file metadata by hash: %w", err)
	}
	return db.getMetadataByFileID(ctx, fileID)
}

// titleFromFilename derives a display title from an upload filename: the base
// name with its extension stripped. It is the default media_metadata.title when a
// file carries no title tag (upload) or its title is cleared (PATCH), so the
// column is never empty. Falls back to "Untitled" for an all-extension/blank
// name, matching migration 016's backfill.
func titleFromFilename(name string) string {
	base := strings.TrimSpace(strings.TrimSuffix(name, filepath.Ext(name)))
	if base == "" {
		base = strings.TrimSpace(name)
	}
	if base == "" {
		base = "Untitled"
	}
	return base
}

// firstFilenameTx returns the earliest-recorded filename for a file within an
// open transaction, or "" when none was recorded.
func firstFilenameTx(ctx context.Context, tx *sql.Tx, fileID int64) (string, error) {
	var name sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT filename FROM file_uploads WHERE file_id = ? ORDER BY id LIMIT 1`, fileID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("first filename: %w", err)
	}
	return name.String, nil
}
