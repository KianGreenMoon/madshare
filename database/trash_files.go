package database

// Files perspective of Trash (soft-delete.md): the file-grain lens over
// soft-removed blobs (files.deleted_at IS NOT NULL — removed renditions,
// absorbed/dormant blobs). Listing reuses the shared fileListSelect/scanFileList
// row shape (it already carries storage_backend / recording_id / deleted_at);
// restore is RestoreRendition (recordings.go). Permanent delete
// (HardDeleteRemovedFile) is the only per-file purge in the system: a non-last
// file drops just its blob (live appearances repointed to a surviving
// rendition), the last file of a recording cascade-prunes the whole recording.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// removedFileFilterWhere builds the WHERE predicate (always the removed base
// f.deleted_at IS NOT NULL) + binds for a FileFilter over the removed-blob
// bucket. It reuses qFieldClause + the artist/album pins so the Files view
// filters identically to the other listings, just over soft-removed rows.
func removedFileFilterWhere(f FileFilter) (string, []any) {
	where := "f.deleted_at IS NOT NULL"
	var args []any
	if frag, fragArgs := qFieldClause(f.Q, f.QField); frag != "" {
		where += " AND " + frag
		args = append(args, fragArgs...)
	}
	if f.ArtistID != nil {
		where += " AND (m.album_artist_id = ? OR m.artist_id = ?)"
		args = append(args, *f.ArtistID, *f.ArtistID)
	}
	if f.AlbumID != nil {
		where += " AND m.album_id = ?"
		args = append(args, *f.AlbumID)
	}
	return where, args
}

// removedFileSortOrder maps a sort token to a safe ORDER BY for the Files view.
// It adds removal-time orders (the default) and otherwise delegates to the
// shared flat / grouped orders. Unknown tokens fall back to newest-removed-first.
func removedFileSortOrder(token string) string {
	switch token {
	case "removed_asc":
		return "f.deleted_at ASC, f.id ASC"
	case "removed_desc":
		return "f.deleted_at DESC, f.id DESC"
	case "created_asc", "created_desc", "title_asc", "title_desc",
		"artist_asc", "artist_desc", "size_asc", "size_desc", "untagged_first", "grouped":
		return fileSortOrder(token)
	default:
		return "f.deleted_at DESC, f.id DESC"
	}
}

// ListRemovedFilesPage returns one filtered, sorted page of the soft-removed
// blobs (Files perspective). With Limit <= 0 it returns every match. The row
// shape is the shared FileListEntry (DeletedAt carries the file's removal time,
// StorageBackend/RecordingID the physical columns).
func (db *DB) ListRemovedFilesPage(ctx context.Context, q FileListQuery) ([]*FileListEntry, error) {
	where, args := removedFileFilterWhere(q.FileFilter)
	sqlText := fileListSelect + "\n\t\tWHERE " + where + "\n\t\tORDER BY " + removedFileSortOrder(q.Sort)
	if q.Limit > 0 {
		offset := q.Offset
		if offset < 0 {
			offset = 0
		}
		sqlText += "\n\t\tLIMIT ? OFFSET ?"
		args = append(args, q.Limit, offset)
	}
	rows, err := db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("list removed files page: %w", err)
	}
	defer rows.Close()
	return scanFileList(rows)
}

// CountRemovedFiles returns how many soft-removed blobs match the filter,
// ignoring paging — the total for the Files list and "select all N matching".
func (db *DB) CountRemovedFiles(ctx context.Context, f FileFilter) (int, error) {
	where, args := removedFileFilterWhere(f)
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files f`+tagsetJoin+` WHERE `+where, args...).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count removed files: %w", err)
	}
	return n, nil
}

// RemovedFileIDsByFilter returns the file ids of every soft-removed blob
// matching the filter (no paging), ordered by id. Backs the Files "select all N
// matching" bulk restore / delete. The Files perspective is file-id-addressed
// (like the rendition endpoints), not hash-addressed.
func (db *DB) RemovedFileIDsByFilter(ctx context.Context, f FileFilter) ([]int64, error) {
	where, args := removedFileFilterWhere(f)
	return scanIDs(db.QueryContext(ctx,
		`SELECT f.id FROM files f`+tagsetJoin+` WHERE `+where+` ORDER BY f.id`, args...))
}

// BulkRestoreRemovedFiles clears the removal mark on the given blobs in one
// guarded UPDATE — the Files "Restore selected". Non-removed / unknown ids are
// no-ops. Returns the count restored. A dormant recording whose rendition is
// restored re-enters the library automatically (visibleTagset).
func (db *DB) BulkRestoreRemovedFiles(ctx context.Context, fileIDs []int64) (int, error) {
	if len(fileIDs) == 0 {
		return 0, nil
	}
	restored := 0
	const chunk = 400
	for i := 0; i < len(fileIDs); i += chunk {
		batch := fileIDs[i:min(i+chunk, len(fileIDs))]
		ph := make([]string, len(batch))
		args := make([]any, len(batch))
		for j, id := range batch {
			ph[j] = "?"
			args[j] = id
		}
		res, err := db.ExecContext(ctx,
			`UPDATE files SET deleted_at = NULL WHERE deleted_at IS NOT NULL AND id IN (`+strings.Join(ph, ",")+`)`,
			args...)
		if err != nil {
			return 0, fmt.Errorf("bulk restore removed files: %w", err)
		}
		n, _ := res.RowsAffected()
		restored += int(n)
	}
	return restored, nil
}

// HardDeleteRemovedFile permanently removes a single soft-removed blob (the
// Files-perspective "Delete forever"). It is the only per-file purge path:
//
//   - Not the last file of its recording → reclaim just this blob + row; any
//     appearance whose origin was this file is repointed onto a surviving
//     rendition first, so a live appearance is never destroyed (unlike the
//     files-first cascade, which deletes tagsets by origin_file_id).
//   - Last file of its recording → the recording has nothing left to play, so
//     the whole recording (+ every appearance) cascades away (owner decision:
//     "last file → just prune everything").
//
// Refuses a file that is not soft-removed (a live file is not in this bucket) —
// found=false. Returns the blob(s) to reclaim after commit.
func (db *DB) HardDeleteRemovedFile(ctx context.Context, fileID int64) (blobs []DeletedBlob, found bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("hard delete removed file: begin: %w", err)
	}
	defer tx.Rollback()

	var recID int64
	var blob DeletedBlob
	err = tx.QueryRowContext(ctx,
		`SELECT recording_id, hash, storage_backend FROM files
		  WHERE id = ? AND deleted_at IS NOT NULL`, fileID).Scan(&recID, &blob.Hash, &blob.StorageBackend)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("hard delete removed file: load: %w", err)
	}

	var survivor int64
	err = tx.QueryRowContext(ctx,
		`SELECT id FROM files WHERE recording_id = ? AND id <> ?
		  ORDER BY (deleted_at IS NULL) DESC, id ASC LIMIT 1`, recID, fileID).Scan(&survivor)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Last file of the recording: nothing left to play → prune the whole
		// recording. Reclaim this file's blob, drop the file, then drop the
		// recording (its tagsets cascade via FK).
		blobs, err = deleteRecordingFilesTx(ctx, tx, recID)
		if err != nil {
			return nil, false, err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM recordings WHERE id = ?`, recID); err != nil {
			return nil, false, fmt.Errorf("hard delete removed file: drop recording: %w", err)
		}
	case err != nil:
		return nil, false, fmt.Errorf("hard delete removed file: survivor: %w", err)
	default:
		// A sibling rendition survives: repoint any appearance offered from this
		// blob onto it (provenance only), then drop just this file. FK cascade
		// clears media_metadata / file_uploads / fingerprints / source_files.
		if _, err := tx.ExecContext(ctx,
			`UPDATE tagsets SET origin_file_id = ? WHERE origin_file_id = ?`, survivor, fileID); err != nil {
			return nil, false, fmt.Errorf("hard delete removed file: repoint: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, fileID); err != nil {
			return nil, false, fmt.Errorf("hard delete removed file: drop file: %w", err)
		}
		if err := repairRecordingTx(ctx, tx, recID); err != nil {
			return nil, false, err
		}
		blobs = []DeletedBlob{blob}
	}

	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("hard delete removed file: commit: %w", err)
	}
	return blobs, true, nil
}

// BulkHardDeleteRemovedFiles permanently removes the given soft-removed blobs in
// one transaction — the Files "Delete selected". Non-removed / unknown ids are
// skipped (the removed guard mirrors HardDeleteRemovedFile). Returns the count
// purged and every blob to reclaim after commit.
func (db *DB) BulkHardDeleteRemovedFiles(ctx context.Context, fileIDs []int64) (int, []DeletedBlob, error) {
	if len(fileIDs) == 0 {
		return 0, nil, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("bulk hard delete removed files: begin: %w", err)
	}
	defer tx.Rollback()

	var blobs []DeletedBlob
	deleted := 0
	for _, fileID := range fileIDs {
		var recID int64
		var blob DeletedBlob
		err := tx.QueryRowContext(ctx,
			`SELECT recording_id, hash, storage_backend FROM files
			  WHERE id = ? AND deleted_at IS NOT NULL`, fileID).Scan(&recID, &blob.Hash, &blob.StorageBackend)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, nil, fmt.Errorf("bulk hard delete removed files: load %d: %w", fileID, err)
		}

		var survivor int64
		err = tx.QueryRowContext(ctx,
			`SELECT id FROM files WHERE recording_id = ? AND id <> ?
			  ORDER BY (deleted_at IS NULL) DESC, id ASC LIMIT 1`, recID, fileID).Scan(&survivor)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			recBlobs, err := deleteRecordingFilesTx(ctx, tx, recID)
			if err != nil {
				return 0, nil, err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM recordings WHERE id = ?`, recID); err != nil {
				return 0, nil, fmt.Errorf("bulk hard delete removed files: drop recording %d: %w", recID, err)
			}
			blobs = append(blobs, recBlobs...)
		case err != nil:
			return 0, nil, fmt.Errorf("bulk hard delete removed files: survivor %d: %w", fileID, err)
		default:
			if _, err := tx.ExecContext(ctx,
				`UPDATE tagsets SET origin_file_id = ? WHERE origin_file_id = ?`, survivor, fileID); err != nil {
				return 0, nil, fmt.Errorf("bulk hard delete removed files: repoint %d: %w", fileID, err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM files WHERE id = ?`, fileID); err != nil {
				return 0, nil, fmt.Errorf("bulk hard delete removed files: drop file %d: %w", fileID, err)
			}
			if err := repairRecordingTx(ctx, tx, recID); err != nil {
				return 0, nil, err
			}
			blobs = append(blobs, blob)
		}
		deleted++
	}

	if err := tx.Commit(); err != nil {
		return 0, nil, fmt.Errorf("bulk hard delete removed files: commit: %w", err)
	}
	return deleted, blobs, nil
}
