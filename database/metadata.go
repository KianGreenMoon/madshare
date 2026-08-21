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

// MetadataPatch carries the descriptive tag fields editable via
// PATCH /api/files/{hash}/metadata — written onto the file's tagset (the
// descriptive columns moved there in migration 024; tech fields are blob-owned
// and read-only). A nil pointer leaves that column unchanged;
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

// UpdateFileMetadata writes the provided base fields onto the tagset of the
// file identified by hash, then returns the resulting combined row. A nil field
// in the patch is left unchanged; a non-nil field is written (empty string
// stored as NULL, matching how tags are extracted). An empty patch is a no-op
// that still returns the current row, so callers get a consistent echo. Returns
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

// FillMissingTags writes tags onto a file's appearance ONLY where the file
// itself carries none. A field the file already answered is left exactly as it
// is, and so is one somebody has edited since.
//
// It exists for content that arrives from a library that knows more about it
// than its bytes do. Metadata here is an OVERLAY — an edit is never written back
// into the file — so a node's catalogue routinely names an artist and an album
// that no tag in the blob mentions. Copy those bytes to another machine, let its
// scanner read them, and everything the first library knew is gone: an album of
// untagged WAVs (a format with no tag dialect this reader understands at all)
// arrives as Unknown artist / Other, and one tagged with only a performer loses
// the album-artist grouping it was filed under. Both were reported by madplayer,
// whose "keep on this device" is exactly that copy.
//
// "Missing" for the title means the appearance still carries the
// filename-derived default an untagged import is given (migration 016), since
// that column is required non-empty and so is never blank to test for.
//
// Returns ErrFileNotFound when no files row matches hash — which for a caller
// that has just handed the file to a scanner means the scan has not reached it
// yet, not that anything is wrong with the file.
func (db *DB) FillMissingTags(ctx context.Context, hash string, p MetadataPatch) (*MediaMetadata, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("fill missing tags: begin: %w", err)
	}
	defer tx.Rollback()

	fileID, tagsetID, err := representativeTagsetTx(ctx, tx, hash)
	if err != nil {
		return nil, err
	}

	var title, artist, albumArtist, album sql.NullString
	var track, disc, year sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT title, artist, album_artist, album, track_number, disc_number, year
		   FROM tagsets WHERE id = ?`, tagsetID,
	).Scan(&title, &artist, &albumArtist, &album, &track, &disc, &year); err != nil {
		return nil, fmt.Errorf("fill missing tags: read appearance: %w", err)
	}
	// An untagged import's title is derived from its filename (migration 016),
	// and tagsets.title is required non-empty — so "the file said nothing" is
	// not a NULL to look for but that derived value, recomputed here from the
	// same helpers the import used.
	filename, err := firstFilenameTx(ctx, tx, fileID)
	if err != nil {
		return nil, err
	}
	untitled := title.String == titleFromFilename(filename)

	keepString := func(field **string, current string) {
		if strings.TrimSpace(current) != "" {
			*field = nil
		}
	}
	keepNumber := func(field **string, current sql.NullInt64) {
		if current.Valid {
			*field = nil
		}
	}
	if !untitled {
		p.Title = nil
	}
	keepString(&p.Artist, artist.String)
	keepString(&p.AlbumArtist, albumArtist.String)
	keepString(&p.Album, album.String)
	keepNumber(&p.TrackNumber, track)
	keepNumber(&p.DiscNumber, disc)
	keepNumber(&p.Year, year)
	// A supplied value that is itself blank fills nothing — the caller knowing
	// nothing must not clear what is there, and for the title it would re-derive
	// the filename fallback that is already in place.
	for _, f := range []**string{&p.Title, &p.Artist, &p.AlbumArtist, &p.Album,
		&p.TrackNumber, &p.DiscNumber, &p.Year} {
		if *f != nil && strings.TrimSpace(**f) == "" {
			*f = nil
		}
	}

	if err := applyMetadataPatchTagsetTx(ctx, tx, tagsetID, p); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("fill missing tags: commit: %w", err)
	}
	return db.getMetadataByFileID(ctx, fileID)
}

// applyMetadataPatchTx writes one file's patch within tx (no commit), returning
// the file id. It resolves the file id first so an unknown hash is a clean
// ErrFileNotFound even for an empty patch, then writes the supplied fields and —
// when an identity field changed — re-resolves the artist/album entity FKs.
func applyMetadataPatchTx(ctx context.Context, tx *sql.Tx, hash string, p MetadataPatch) (int64, error) {
	fileID, tagsetID, err := representativeTagsetTx(ctx, tx, hash)
	if err != nil {
		return 0, err
	}
	if err := applyMetadataPatchTagsetTx(ctx, tx, tagsetID, p); err != nil {
		return 0, err
	}
	return fileID, nil
}

// representativeTagsetTx resolves a content hash to its file id and the
// appearance a *file-addressed* edit targets. Provenance first: the tagset
// actually read from this blob (a byte-dup upload may have attached extra draft
// appearances; those are edited only through the tagset-addressed review paths).
// Appearance dedup can leave a rendition with no tagset of its own, so fall back
// to the recording's representative appearance rather than 404
// (recording-tagsets P7). An unknown hash is a clean ErrFileNotFound.
func representativeTagsetTx(ctx context.Context, tx *sql.Tx, hash string) (fileID, tagsetID int64, err error) {
	err = tx.QueryRowContext(ctx, `SELECT id FROM files WHERE hash = ?`, hash).Scan(&fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrFileNotFound
	}
	if err != nil {
		return 0, 0, fmt.Errorf("update metadata: lookup file: %w", err)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT rt.id FROM tagsets rt
		   JOIN files f ON f.id = ?
		  WHERE rt.recording_id = f.recording_id
		  ORDER BY COALESCE(rt.origin_file_id = f.id, 0) DESC,
		           (rt.deleted_at IS NULL) DESC, rt.is_primary DESC, rt.id ASC
		  LIMIT 1`,
		fileID,
	).Scan(&tagsetID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, ErrFileNotFound
		}
		return 0, 0, fmt.Errorf("update metadata: representative tagset: %w", err)
	}
	return fileID, tagsetID, nil
}

// applyMetadataPatchTagsetTx writes one appearance's patch within tx, addressed
// by tagset id — the shared core of both the hash-addressed file edit (via its
// representative appearance) and the tagset-addressed review/My-uploads edits.
// An empty patch is a no-op; an identity-affecting change re-resolves the
// artist/album entity FKs.
func applyMetadataPatchTagsetTx(ctx context.Context, tx *sql.Tx, tagsetID int64, p MetadataPatch) error {
	if p.IsEmpty() {
		return nil
	}
	var sets []string
	var args []any
	if p.Title != nil {
		// Title is required non-empty (migration 016). Clearing it (or a
		// whitespace-only value) re-derives from the origin blob's filename, the
		// same default the upload path uses, rather than storing NULL/''.
		title := *p.Title
		if strings.TrimSpace(title) == "" {
			var originFile sql.NullInt64
			if err := tx.QueryRowContext(ctx,
				`SELECT origin_file_id FROM tagsets WHERE id = ?`, tagsetID,
			).Scan(&originFile); err != nil {
				return fmt.Errorf("update metadata: origin file: %w", err)
			}
			fn := ""
			if originFile.Valid {
				var e error
				if fn, e = firstFilenameTx(ctx, tx, originFile.Int64); e != nil {
					return e
				}
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
			return fmt.Errorf("update metadata: %s: %w", nf.col, err)
		}
		sets = append(sets, nf.col+" = ?")
		args = append(args, n)
	}
	args = append(args, tagsetID)
	if _, err := tx.ExecContext(ctx,
		`UPDATE tagsets SET `+strings.Join(sets, ", ")+` WHERE id = ?`,
		args...,
	); err != nil {
		return fmt.Errorf("update metadata: %w", err)
	}

	// Re-resolve entities only when an identity-affecting field changed.
	if p.Artist != nil || p.AlbumArtist != nil || p.Album != nil {
		var artist, albumArtist, album sql.NullString
		var year sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT artist, album_artist, album, year FROM tagsets WHERE id = ?`,
			tagsetID,
		).Scan(&artist, &albumArtist, &album, &year); err != nil {
			return fmt.Errorf("update metadata: reload tags: %w", err)
		}
		t := AlbumArtistTags{
			Artist:      artist.String,
			AlbumArtist: albumArtist.String,
			Album:       album.String,
			Year:        int(year.Int64),
		}
		albumArtistID, trackArtistID, albumID, err := resolveAlbumArtistTx(ctx, tx, t)
		if err != nil {
			return fmt.Errorf("update metadata: resolve entities: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tagsets SET album_artist_id = ?, artist_id = ?, album_id = ? WHERE id = ?`,
			albumArtistID, trackArtistID, albumID, tagsetID,
		); err != nil {
			return fmt.Errorf("update metadata: set entity fks: %w", err)
		}
	}
	return nil
}

// UpdateTagsetMetadata writes the patch onto the appearance with the given id
// and returns the resulting combined row (tags + tech). Addressed by tagset id
// — the review / My-uploads edit target (recording-tagsets P4). Returns
// ErrFileNotFound when no tagset matches.
func (db *DB) UpdateTagsetMetadata(ctx context.Context, tagsetID int64, p MetadataPatch) (*MediaMetadata, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("update tagset metadata: begin: %w", err)
	}
	defer tx.Rollback()

	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM tagsets WHERE id = ?`, tagsetID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrFileNotFound
		}
		return nil, fmt.Errorf("update tagset metadata: lookup: %w", err)
	}
	if err := applyMetadataPatchTagsetTx(ctx, tx, tagsetID, p); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("update tagset metadata: commit: %w", err)
	}
	return db.getMetadataByTagsetID(ctx, tagsetID)
}

// TagsetMetadataByID loads the editable metadata (tags + tech view) for one
// appearance, for the edit modal to prefill. ErrFileNotFound when no tagset
// matches.
func (db *DB) TagsetMetadataByID(ctx context.Context, tagsetID int64) (*MediaMetadata, error) {
	return db.getMetadataByTagsetID(ctx, tagsetID)
}

// getMetadataByTagsetID loads the combined metadata view for one appearance: its
// descriptive tags joined with the tech columns of its origin blob (absent for a
// blobless appearance — tech reads back NULL). FileID is the origin blob id (0
// when purged).
func (db *DB) getMetadataByTagsetID(ctx context.Context, tagsetID int64) (*MediaMetadata, error) {
	m := &MediaMetadata{}
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(t.origin_file_id, 0), t.title, t.artist, t.album, t.album_artist, t.genre, t.year,
		       t.track_number, t.track_total, t.disc_number, t.composer, t.comment,
		       mm.duration_seconds, mm.bitrate, mm.sample_rate, mm.channels, mm.codec,
		       mm.tag_format, COALESCE(mm.extracted_at, t.created_at)
		FROM tagsets t
		LEFT JOIN media_metadata mm ON mm.file_id = t.origin_file_id
		WHERE t.id = ?`, tagsetID,
	).Scan(
		&m.FileID, &m.Title, &m.Artist, &m.Album, &m.AlbumArtist, &m.Genre, &m.Year,
		&m.TrackNumber, &m.TrackTotal, &m.DiscNumber, &m.Composer, &m.Comment,
		&m.DurationSeconds, &m.Bitrate, &m.SampleRate, &m.Channels, &m.Codec,
		&m.TagFormat, &m.ExtractedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrFileNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get tagset metadata: %w", err)
	}
	return m, nil
}

// getMetadataByFileID loads the combined metadata view for a file id: the
// descriptive tags from its tagset joined with the tech columns from
// media_metadata (which may be absent for legacy rows — tech reads back NULL).
func (db *DB) getMetadataByFileID(ctx context.Context, fileID int64) (*MediaMetadata, error) {
	m := &MediaMetadata{FileID: fileID}
	err := db.QueryRowContext(ctx, `
		SELECT t.title, t.artist, t.album, t.album_artist, t.genre, t.year,
		       t.track_number, t.track_total, t.disc_number, t.composer, t.comment,
		       mm.duration_seconds, mm.bitrate, mm.sample_rate, mm.channels, mm.codec,
		       mm.tag_format, COALESCE(mm.extracted_at, t.created_at)
		FROM tagsets t
		LEFT JOIN media_metadata mm ON mm.file_id = t.origin_file_id
		WHERE t.origin_file_id = ?
		ORDER BY (t.deleted_at IS NULL) DESC, t.is_primary DESC, t.id ASC
		LIMIT 1`, fileID,
	).Scan(
		&m.Title, &m.Artist, &m.Album, &m.AlbumArtist, &m.Genre, &m.Year,
		&m.TrackNumber, &m.TrackTotal, &m.DiscNumber, &m.Composer, &m.Comment,
		&m.DurationSeconds, &m.Bitrate, &m.SampleRate, &m.Channels, &m.Codec,
		&m.TagFormat, &m.ExtractedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		// A files row always gets a tagset at insert time, so this should not
		// happen; treat it as not-found rather than a 500.
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

// FileMetadataByHash loads the editable metadata (tagset + tech view) for the
// file with the given content hash. Returns ErrFileNotFound when no files row
// matches.
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
// name with its extension stripped. It is the default tagsets.title when a
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
