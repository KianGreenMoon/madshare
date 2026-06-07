package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// GetFileByHash looks up a files row by content hash. Returns (nil, nil) if
// no row matches — callers treat that as "new upload". Soft-deleted files are
// returned with DeletedAt set so the upload handler can restore them.
func (db *DB) GetFileByHash(ctx context.Context, hash string) (*File, error) {
	const q = `
		SELECT id, hash, byte_size, mime_type, storage_backend, object_key, created_at, deleted_at
		FROM files
		WHERE hash = ?`

	var f File
	err := db.QueryRowContext(ctx, q, hash).Scan(
		&f.ID, &f.Hash, &f.ByteSize, &f.MimeType, &f.StorageBackend, &f.ObjectKey, &f.CreatedAt, &f.DeletedAt,
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

	res, err := tx.ExecContext(ctx, `
		INSERT INTO files (hash, byte_size, mime_type, storage_backend, object_key, created_at, uploaded_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		f.Hash, f.ByteSize, f.MimeType, f.StorageBackend, f.ObjectKey, f.CreatedAt, f.UploadedBy,
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
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO media_metadata (
				file_id, title, artist, album, album_artist, genre, year,
				track_number, track_total, disc_number, composer, comment,
				duration_seconds, bitrate, sample_rate, channels, codec,
				tag_format, extracted_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			meta.FileID, meta.Title, meta.Artist, meta.Album, meta.AlbumArtist,
			meta.Genre, meta.Year, meta.TrackNumber, meta.TrackTotal, meta.DiscNumber,
			meta.Composer, meta.Comment, meta.DurationSeconds, meta.Bitrate,
			meta.SampleRate, meta.Channels, meta.Codec, meta.TagFormat, meta.ExtractedAt,
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
			COALESCE(m.year,    0) AS year,
			m.duration_seconds,
			` + guestAccessibleExpr + ` AS guest_playable,
			f.license
		FROM files f
		LEFT JOIN media_metadata m ON m.file_id = f.id`
	if where != "" {
		q += "\n\t\tWHERE f.deleted_at IS NULL AND " + where
	} else {
		q += "\n\t\tWHERE f.deleted_at IS NULL"
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
		var guest int
		if err := rows.Scan(
			&e.ID, &e.Hash, &e.MimeType, &e.ByteSize, &e.ObjectKey, &e.CreatedAt,
			&e.Filename, &e.Title, &e.Artist, &e.AlbumArtist, &e.Album, &e.Year, &e.DurationSeconds,
			&guest, &e.License,
		); err != nil {
			return nil, fmt.Errorf("scan file list entry: %w", err)
		}
		e.GuestPlayable = guest == 1
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
			COALESCE(m.year,    0) AS year,
			m.duration_seconds,
			f.guest_playable,
			f.license,
			f.deleted_at
		FROM files f
		LEFT JOIN media_metadata m ON m.file_id = f.id
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
		var guest int
		if err := rows.Scan(
			&e.ID, &e.Hash, &e.MimeType, &e.ByteSize, &e.ObjectKey, &e.CreatedAt,
			&e.Filename, &e.Title, &e.Artist, &e.AlbumArtist, &e.Album, &e.Year, &e.DurationSeconds,
			&guest, &e.License, &e.DeletedAt,
		); err != nil {
			return nil, fmt.Errorf("scan trashed file: %w", err)
		}
		e.GuestPlayable = guest == 1
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
