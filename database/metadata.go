package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ErrFileNotFound is returned when no files row matches a given content hash.
var ErrFileNotFound = errors.New("file not found")

// MetadataPatch carries the base media_metadata fields editable via
// PATCH /api/files/{hash}/metadata. A nil pointer leaves that column unchanged;
// a non-nil pointer writes its value, so a pointer to "" clears the field.
// Only these base fields are writable in this round — richer tag editing
// (track #, disc, year, genre, …) is deferred (see
// docs/plans/upload-and-covers.md §5h).
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
// It deliberately does not touch album_images: albums are keyed by their
// (album_artist, album) strings, so renaming a track here can orphan a cover.
// The caller (the upload page) re-POSTs the cover to the new identity — see
// docs/plans/upload-and-covers.md §5d/§5e.
func (db *DB) UpdateFileMetadata(ctx context.Context, hash string, p MetadataPatch) (*MediaMetadata, error) {
	// Resolve the file id first so an unknown hash is a clean ErrFileNotFound
	// even for an empty patch (where no UPDATE would otherwise run).
	var fileID int64
	err := db.QueryRowContext(ctx, `SELECT id FROM files WHERE hash = ?`, hash).Scan(&fileID)
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
			sets = append(sets, "title = ?")
			args = append(args, metaNullString(*p.Title))
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
		if _, err := db.ExecContext(ctx,
			`UPDATE media_metadata SET `+strings.Join(sets, ", ")+` WHERE file_id = ?`,
			args...,
		); err != nil {
			return nil, fmt.Errorf("update metadata: %w", err)
		}
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
