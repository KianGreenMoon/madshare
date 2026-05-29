package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GetFileByHash looks up a files row by content hash. Returns (nil, nil) if
// no row matches — callers treat that as "new upload".
func (db *DB) GetFileByHash(ctx context.Context, hash string) (*File, error) {
	const q = `
		SELECT id, hash, byte_size, mime_type, storage_backend, object_key, created_at
		FROM files
		WHERE hash = ?`

	var f File
	err := db.QueryRowContext(ctx, q, hash).Scan(
		&f.ID, &f.Hash, &f.ByteSize, &f.MimeType, &f.StorageBackend, &f.ObjectKey, &f.CreatedAt,
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
		INSERT INTO files (hash, byte_size, mime_type, storage_backend, object_key, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		f.Hash, f.ByteSize, f.MimeType, f.StorageBackend, f.ObjectKey, f.CreatedAt,
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
	const q = `
		SELECT
			f.id, f.hash, f.mime_type, f.byte_size, f.object_key, f.created_at,
			COALESCE((SELECT filename FROM file_uploads WHERE file_id = f.id ORDER BY id LIMIT 1), f.hash) AS filename,
			COALESCE(m.title,  '') AS title,
			COALESCE(m.artist, '') AS artist,
			m.album_artist,
			COALESCE(m.album,  '') AS album,
			COALESCE(m.year,    0) AS year,
			m.duration_seconds
		FROM files f
		LEFT JOIN media_metadata m ON m.file_id = f.id
		ORDER BY f.created_at DESC`

	rows, err := db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list files: %w", err)
	}
	defer rows.Close()

	out := make([]*FileListEntry, 0)
	for rows.Next() {
		var e FileListEntry
		if err := rows.Scan(
			&e.ID, &e.Hash, &e.MimeType, &e.ByteSize, &e.ObjectKey, &e.CreatedAt,
			&e.Filename, &e.Title, &e.Artist, &e.AlbumArtist, &e.Album, &e.Year, &e.DurationSeconds,
		); err != nil {
			return nil, fmt.Errorf("scan file list entry: %w", err)
		}
		out = append(out, &e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list files rows: %w", err)
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
