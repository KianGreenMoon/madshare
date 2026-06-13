package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrFileNotFound is returned when no files row matches a given content hash.
var ErrFileNotFound = errors.New("file not found")

// MetadataPatch carries the base media_metadata fields editable via
// PATCH /api/files/{hash}/metadata. A nil pointer leaves that column unchanged;
// a non-nil pointer writes its value, so a pointer to "" clears the field.
// Only these base fields are writable in this round — richer tag editing
// (track #, disc, year, genre, …) is deferred (see
// .issues/open-issues.md).
type MetadataPatch struct {
	Title       *string
	Album       *string
	AlbumArtist *string
	Artist      *string
}

// IsEmpty reports whether the patch would change nothing (all fields nil).
func (p MetadataPatch) IsEmpty() bool {
	return p.Title == nil && p.Album == nil && p.AlbumArtist == nil && p.Artist == nil
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

	// Resolve the file id first so an unknown hash is a clean ErrFileNotFound
	// even for an empty patch (where no UPDATE would otherwise run).
	var fileID int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM files WHERE hash = ?`, hash).Scan(&fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrFileNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update metadata: lookup file: %w", err)
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
					return nil, err
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
		args = append(args, fileID)
		if _, err := tx.ExecContext(ctx,
			`UPDATE media_metadata SET `+strings.Join(sets, ", ")+` WHERE file_id = ?`,
			args...,
		); err != nil {
			return nil, fmt.Errorf("update metadata: %w", err)
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
				return nil, fmt.Errorf("update metadata: reload tags: %w", err)
			}
			t = AlbumArtistTags{
				Artist:      artist.String,
				AlbumArtist: albumArtist.String,
				Album:       album.String,
				Year:        int(year.Int64),
			}
			artistID, albumID, err := resolveAlbumArtistTx(ctx, tx, t)
			if err != nil {
				return nil, fmt.Errorf("update metadata: resolve entities: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE media_metadata SET artist_id = ?, album_id = ? WHERE file_id = ?`,
				artistID, albumID, fileID,
			); err != nil {
				return nil, fmt.Errorf("update metadata: set entity fks: %w", err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("update metadata: commit: %w", err)
	}
	return db.getMetadataByFileID(ctx, fileID)
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
