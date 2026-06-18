package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// visibleFile is the predicate every user-facing listing / access query must
// apply (aliased table f): a file is publicly visible only when it is neither
// trashed nor pending review (docs/architecture/moderation.md). Trash
// and review/staging queries intentionally do not use it.
const visibleFile = "f.deleted_at IS NULL AND f.review_state = 'approved'"

// LibraryByteSize returns the total logical byte size of stored blobs: the sum
// of byte_size over every files row. Files are content-addressed (one row per
// hash), so this is the deduplicated blob total. Trashed-but-not-yet-pruned
// rows are included — their blobs still occupy the disk until a hard delete.
// It backs the "audio" disk-usage category (files hold audio in v0); it is an
// indexed sum, so it is instant and needs no caching, unlike the image walk.
func (db *DB) LibraryByteSize(ctx context.Context) (int64, error) {
	var total int64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(byte_size), 0) FROM files`,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("sum library byte size: %w", err)
	}
	return total, nil
}

// GetFileByHash looks up a files row by content hash. Returns (nil, nil) if
// no row matches — callers treat that as "new upload". Soft-deleted files are
// returned with DeletedAt set so the upload handler can restore them.
func (db *DB) GetFileByHash(ctx context.Context, hash string) (*File, error) {
	const q = `
		SELECT id, hash, byte_size, mime_type, storage_backend, object_key, created_at, deleted_at,
		       review_state, review_note, submitted_at
		FROM files
		WHERE hash = ?`

	var f File
	err := db.QueryRowContext(ctx, q, hash).Scan(
		&f.ID, &f.Hash, &f.ByteSize, &f.MimeType, &f.StorageBackend, &f.ObjectKey, &f.CreatedAt, &f.DeletedAt,
		&f.ReviewState, &f.ReviewNote, &f.SubmittedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("query file by hash: %w", err)
	}
	return &f, nil
}

// InsertFile writes the files, file_uploads, and media_metadata rows in a
// single transaction. f.ID is populated on success. The upload and meta
// arguments may share f.ID once it is assigned.
func (db *DB) InsertFile(ctx context.Context, f *File, upload *FileUpload, meta *MediaMetadata) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// An unset ReviewState collapses to approved — the pre-moderation behavior
	// kept by auth-less embedding and existing tests.
	if f.ReviewState == "" {
		f.ReviewState = ReviewApproved
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO files (hash, byte_size, mime_type, storage_backend, object_key, created_at, uploaded_by, review_state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		f.Hash, f.ByteSize, f.MimeType, f.StorageBackend, f.ObjectKey, f.CreatedAt, f.UploadedBy, f.ReviewState,
	)
	if err != nil {
		return fmt.Errorf("insert files: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("last insert id: %w", err)
	}
	f.ID = id

	if upload != nil {
		upload.FileID = id
		if upload.UploadedAt == 0 {
			upload.UploadedAt = time.Now().Unix()
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO file_uploads (file_id, filename, uploaded_at)
			VALUES (?, ?, ?)`,
			upload.FileID, upload.Filename, upload.UploadedAt,
		); err != nil {
			return fmt.Errorf("insert file_uploads: %w", err)
		}
	}

	if meta != nil {
		meta.FileID = id
		if meta.ExtractedAt == 0 {
			meta.ExtractedAt = time.Now().Unix()
		}
		// title is required non-empty (migration 016): default it to the upload
		// filename with its extension stripped when no title tag was supplied, so
		// the CHECK is satisfied for every caller.
		if strings.TrimSpace(meta.Title) == "" {
			fname := ""
			if upload != nil {
				fname = upload.Filename
			}
			meta.Title = titleFromFilename(fname)
		}
		// Resolve the artist/album entities inline so a fresh upload carries its
		// FKs immediately, without waiting for the startup backfill. Both the
		// album-grouping artist (album_artist_id) and the track performer (artist_id)
		// are resolved; for a normal release they are the same entity.
		albumArtistID, trackArtistID, albumID, err := resolveAlbumArtistTx(ctx, tx, tagsFromMeta(meta))
		if err != nil {
			return fmt.Errorf("resolve entities: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO media_metadata (
				file_id, title, artist, album, album_artist, genre, year,
				track_number, track_total, disc_number, composer, comment,
				duration_seconds, bitrate, sample_rate, channels, codec,
				tag_format, extracted_at, album_artist_id, artist_id, album_id
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			meta.FileID, meta.Title, meta.Artist, meta.Album, meta.AlbumArtist,
			meta.Genre, meta.Year, meta.TrackNumber, meta.TrackTotal, meta.DiscNumber,
			meta.Composer, meta.Comment, meta.DurationSeconds, meta.Bitrate,
			meta.SampleRate, meta.Channels, meta.Codec, meta.TagFormat, meta.ExtractedAt,
			albumArtistID, trackArtistID, albumID,
		); err != nil {
			return fmt.Errorf("insert media_metadata: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ListFiles returns all files ordered by created_at DESC, joined with
// the first recorded filename and media_metadata tags.
func (db *DB) ListFiles(ctx context.Context) ([]*FileListEntry, error) {
	return db.listFiles(ctx, "")
}

// ListFilesGuest returns only the files an anonymous / capability-less request
// may play/download (the guest-playable / license policy). Callers holding the
// content.access permission should use the unfiltered ListFiles instead.
func (db *DB) ListFilesGuest(ctx context.Context) ([]*FileListEntry, error) {
	return db.listFiles(ctx, accessClause)
}

// listFiles is the shared query. When where is non-empty it is appended as an
// access predicate (with its bind args), restricting the result to reachable
// files.
func (db *DB) listFiles(ctx context.Context, where string, args ...any) ([]*FileListEntry, error) {
	q := `
		SELECT
			f.id, f.hash, f.mime_type, f.byte_size, f.object_key, f.created_at,
			COALESCE((SELECT filename FROM file_uploads WHERE file_id = f.id ORDER BY id LIMIT 1), f.hash) AS filename,
			COALESCE(m.title,  '') AS title,
			COALESCE(m.artist, '') AS artist,
			m.album_artist,
			COALESCE(m.album,  '') AS album,
			m.track_number,
			m.disc_number,
			COALESCE(m.year,    0) AS year,
			m.duration_seconds,
			` + guestAccessibleExpr + ` AS guest_playable,
			f.license,
			CASE WHEN aimg.artist_id IS NOT NULL THEN 1 ELSE 0 END AS artist_has_image,
			CASE WHEN alimg.album_id  IS NOT NULL THEN 1 ELSE 0 END AS album_has_image
		FROM files f
		LEFT JOIN media_metadata m ON m.file_id = f.id
		LEFT JOIN artist_images aimg ON aimg.artist_id = m.album_artist_id
		LEFT JOIN album_images  alimg ON alimg.album_id  = m.album_id`
	if where != "" {
		q += "\n\t\tWHERE " + visibleFile + " AND " + where
	} else {
		q += "\n\t\tWHERE " + visibleFile
	}
	q += "\n\t\tORDER BY f.created_at DESC"

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	defer rows.Close()

	out := make([]*FileListEntry, 0)
	for rows.Next() {
		var e FileListEntry
		var guest, artistImg, albumImg int
		if err := rows.Scan(
			&e.ID, &e.Hash, &e.MimeType, &e.ByteSize, &e.ObjectKey, &e.CreatedAt,
			&e.Filename, &e.Title, &e.Artist, &e.AlbumArtist, &e.Album, &e.TrackNumber, &e.DiscNumber, &e.Year, &e.DurationSeconds,
			&guest, &e.License, &artistImg, &albumImg,
		); err != nil {
			return nil, fmt.Errorf("scan file list entry: %w", err)
		}
		e.GuestPlayable = guest == 1
		e.ArtistHasImage = artistImg == 1
		e.AlbumHasImage = albumImg == 1
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list files rows: %w", err)
	}
	return out, nil
}

// uploadFilenamesInTx returns the recorded filenames for a file (ordered by
// upload id) within an open transaction. It is a shared helper for the two
// delete variants that need to report filenames before mutating the row.
func uploadFilenamesInTx(ctx context.Context, tx *sql.Tx, fileID int64) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT filename FROM file_uploads WHERE file_id = ? ORDER BY id`, fileID)
	if err != nil {
		return nil, fmt.Errorf("select filenames: %w", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan filename: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// SoftDeleteFileByHash marks the file as trashed by setting deleted_at. The
// blob is left on disk. file_uploads and media_metadata rows are preserved.
// Returns found=false (no error) when no live (non-trashed) row matches.
func (db *DB) SoftDeleteFileByHash(ctx context.Context, hash string) ([]string, bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM files WHERE hash = ? AND deleted_at IS NULL`, hash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("select file id: %w", err)
	}

	filenames, err := uploadFilenamesInTx(ctx, tx, id)
	if err != nil {
		return nil, false, err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE files SET deleted_at = ? WHERE id = ?`, time.Now().Unix(), id); err != nil {
		return nil, false, fmt.Errorf("soft delete file: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}
	return filenames, true, nil
}

// HardDeleteFileByHash permanently removes any files row identified by hash,
// regardless of trash state. Used by PruneDangling, which must be able to
// clean up both live and trashed rows whose blobs are gone.
// Returns found=false (no error) when no row matches.
func (db *DB) HardDeleteFileByHash(ctx context.Context, hash string) ([]string, bool, error) {
	return db.hardDelete(ctx, `SELECT id FROM files WHERE hash = ?`, hash)
}

// HardDeleteTrashedFileByHash permanently removes a trashed files row. Live
// (non-trashed) files return found=false, making the check and delete atomic
// within the same transaction so a concurrent restore cannot race the delete.
// Returns found=false (no error) when no trashed row matches.
func (db *DB) HardDeleteTrashedFileByHash(ctx context.Context, hash string) ([]string, bool, error) {
	return db.hardDelete(ctx, `SELECT id FROM files WHERE hash = ? AND deleted_at IS NOT NULL`, hash)
}

// hardDelete is the shared implementation for the two hard-delete variants.
func (db *DB) hardDelete(ctx context.Context, selectQ, hash string) ([]string, bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var id int64
	err = tx.QueryRowContext(ctx, selectQ, hash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("select file id: %w", err)
	}

	filenames, err := uploadFilenamesInTx(ctx, tx, id)
	if err != nil {
		return nil, false, err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, id); err != nil {
		return nil, false, fmt.Errorf("delete file: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit: %w", err)
	}
	return filenames, true, nil
}

// RestoreFileByHash clears deleted_at on a trashed file, returning it to the
// live library. Returns found=false (no error) when no trashed row matches.
func (db *DB) RestoreFileByHash(ctx context.Context, hash string) (bool, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE files SET deleted_at = NULL WHERE hash = ? AND deleted_at IS NOT NULL`, hash)
	if err != nil {
		return false, fmt.Errorf("restore file: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ListTrashedFiles returns all soft-deleted files ordered by deletion time
// descending, joined with the first recorded filename and media_metadata tags.
func (db *DB) ListTrashedFiles(ctx context.Context) ([]*FileListEntry, error) {
	const q = `
		SELECT
			f.id, f.hash, f.mime_type, f.byte_size, f.object_key, f.created_at,
			COALESCE((SELECT filename FROM file_uploads WHERE file_id = f.id ORDER BY id LIMIT 1), f.hash) AS filename,
			COALESCE(m.title,  '') AS title,
			COALESCE(m.artist, '') AS artist,
			m.album_artist,
			COALESCE(m.album,  '') AS album,
			m.track_number,
			COALESCE(m.year,    0) AS year,
			m.duration_seconds,
			f.guest_playable,
			f.license,
			f.deleted_at,
			f.review_state,
			CASE WHEN aimg.artist_id IS NOT NULL THEN 1 ELSE 0 END AS artist_has_image,
			CASE WHEN alimg.album_id  IS NOT NULL THEN 1 ELSE 0 END AS album_has_image
		FROM files f
		LEFT JOIN media_metadata m ON m.file_id = f.id
		LEFT JOIN artist_images aimg ON aimg.artist_id = m.album_artist_id
		LEFT JOIN album_images  alimg ON alimg.album_id  = m.album_id
		WHERE f.deleted_at IS NOT NULL
		ORDER BY f.deleted_at DESC`

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list trashed files: %w", err)
	}
	defer rows.Close()

	out := make([]*FileListEntry, 0)
	for rows.Next() {
		var e FileListEntry
		var guest, artistImg, albumImg int
		if err := rows.Scan(
			&e.ID, &e.Hash, &e.MimeType, &e.ByteSize, &e.ObjectKey, &e.CreatedAt,
			&e.Filename, &e.Title, &e.Artist, &e.AlbumArtist, &e.Album, &e.TrackNumber, &e.Year, &e.DurationSeconds,
			&guest, &e.License, &e.DeletedAt, &e.ReviewState, &artistImg, &albumImg,
		); err != nil {
			return nil, fmt.Errorf("scan trashed file: %w", err)
		}
		e.GuestPlayable = guest == 1
		e.ArtistHasImage = artistImg == 1
		e.AlbumHasImage = albumImg == 1
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list trashed files rows: %w", err)
	}
	return out, nil
}

// ListFileRefs returns one FileRef per live (non-trashed) files row with the
// filenames recorded for it, ordered by file id. Files with no upload rows
// carry an empty slice. Trashed files are excluded so PruneDangling does not
// permanently delete files the admin placed in the trash intentionally.
func (db *DB) ListFileRefs(ctx context.Context) ([]FileRef, error) {
	const q = `
		SELECT f.hash, COALESCE(GROUP_CONCAT(u.filename, char(10)), '')
		FROM files f
		LEFT JOIN file_uploads u ON u.file_id = f.id
		WHERE f.deleted_at IS NULL
		GROUP BY f.id
		ORDER BY f.id`

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list file refs: %w", err)
	}
	defer rows.Close()

	out := make([]FileRef, 0)
	for rows.Next() {
		var hash, joined string
		if err := rows.Scan(&hash, &joined); err != nil {
			return nil, fmt.Errorf("scan file ref: %w", err)
		}
		ref := FileRef{Hash: hash}
		if joined != "" {
			ref.Filenames = strings.Split(joined, "\n")
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("file ref rows: %w", err)
	}
	return out, nil
}

// RecordUpload appends an upload row for an existing file. UNIQUE conflicts
// on (file_id, filename) are silently ignored — the same file uploaded with
// the same name twice is not an error.
func (db *DB) RecordUpload(ctx context.Context, fileID int64, filename string) error {
	_, err := db.ExecContext(ctx, `
		INSERT OR IGNORE INTO file_uploads (file_id, filename, uploaded_at)
		VALUES (?, ?, ?)`,
		fileID, filename, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("record upload: %w", err)
	}
	return nil
}
