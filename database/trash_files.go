package database

// Files perspective of Trash (soft-delete.md): the file-grain lens over
// soft-removed blobs (files.deleted_at IS NOT NULL — removed renditions,
// absorbed/dormant blobs). Listing reuses the shared fileListSelect/scanFileList
// row shape (it already carries storage_backend / recording_id / deleted_at);
// restore is RestoreRendition (recordings.go). Permanent delete
// (HardDeleteRemovedFile) purges the trashed row + blob and lets the scoped
// reap converge the recording (GC model): losing the last file row trashes
// the recording's appearances — catalog entries survive blobless in Trash,
// only the bytes are destroyed.

import (
	"context"
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
// Files-perspective "Delete forever") through the GC purge primitives: the
// trashed row and its bytes go; drafts whose origin this blob was are
// discarded, approved appearances just lose their (inert) provenance pointer.
// The scoped reap then converges the recording — if this was its last file
// row, its appearances are TRASHED (Trash › Appearances), never destroyed:
// the catalog entry survives blobless and restorable, only the bytes are
// gone. Refuses a file that is not soft-removed (a live file is not in this
// bucket) — found=false. Returns the blob(s) to reclaim after commit.
func (db *DB) HardDeleteRemovedFile(ctx context.Context, fileID int64) (blobs []DeletedBlob, found bool, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("hard delete removed file: begin: %w", err)
	}
	defer tx.Rollback()

	var trashed bool
	err = tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM files WHERE id = ? AND deleted_at IS NOT NULL)`,
		fileID).Scan(&trashed)
	if err != nil {
		return nil, false, fmt.Errorf("hard delete removed file: load: %w", err)
	}
	if !trashed {
		return nil, false, nil
	}

	blobs, recIDs, err := deleteFileRowsTx(ctx, tx, []int64{fileID})
	if err != nil {
		return nil, false, err
	}
	if err := reapRecordingsTx(ctx, tx, recIDs); err != nil {
		return nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("hard delete removed file: commit: %w", err)
	}
	return blobs, true, nil
}

// BulkHardDeleteRemovedFiles permanently removes the given soft-removed blobs in
// one transaction — the Files "Delete selected". Non-removed / unknown ids are
// skipped (the removed guard mirrors HardDeleteRemovedFile); the same purge +
// scoped-reap semantics apply. Returns the count purged and every blob to
// reclaim after commit.
func (db *DB) BulkHardDeleteRemovedFiles(ctx context.Context, fileIDs []int64) (int, []DeletedBlob, error) {
	if len(fileIDs) == 0 {
		return 0, nil, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("bulk hard delete removed files: begin: %w", err)
	}
	defer tx.Rollback()

	// Keep only the ids that are actually soft-removed.
	trashed := make([]int64, 0, len(fileIDs))
	for i := 0; i < len(fileIDs); i += idChunk {
		batch := fileIDs[i:min(i+idChunk, len(fileIDs))]
		in, args := inClause(batch)
		ids, err := scanIDs(tx.QueryContext(ctx,
			`SELECT id FROM files WHERE deleted_at IS NOT NULL AND id IN `+in, args...))
		if err != nil {
			return 0, nil, fmt.Errorf("bulk hard delete removed files: filter: %w", err)
		}
		trashed = append(trashed, ids...)
	}
	if len(trashed) == 0 {
		return 0, nil, nil
	}

	blobs, recIDs, err := deleteFileRowsTx(ctx, tx, trashed)
	if err != nil {
		return 0, nil, err
	}
	if err := reapRecordingsTx(ctx, tx, recIDs); err != nil {
		return 0, nil, err
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, fmt.Errorf("bulk hard delete removed files: commit: %w", err)
	}
	return len(trashed), blobs, nil
}
