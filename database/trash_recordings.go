package database

// Recordings perspective of Trash (gc-model.md): the recording-grain lens
// over recordings wholly out of the library (all appearances trashed and/or the
// recording gone dormant). Listing reuses ListRecordings with the "trashed"
// filter (recordingFilterClause, curate.go); restore un-trashes every appearance
// AND, if dormant, restores the ladder-best removed rendition so it plays again;
// permanent delete is the existing HardDeleteRecording (single) plus the bulk
// variant here. All hard deletes are Trash-only (recordings page keeps soft ops).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ListTrashedRecordings returns one page of the trashed-recording bin, newest
// first — ListRecordings forced to the "trashed" membership. Search narrows the
// same way as the recordings page.
func (db *DB) ListTrashedRecordings(ctx context.Context, search string, limit, offset int) ([]RecordingRow, error) {
	return db.ListRecordings(ctx, RecordingListOptions{
		Filter: "trashed", Search: search, Limit: limit, Offset: offset,
	})
}

// CountTrashedRecordings returns how many recordings are wholly out of the
// library (the Trash Recordings total).
func (db *DB) CountTrashedRecordings(ctx context.Context, search string) (int, error) {
	return db.CountRecordings(ctx, RecordingListOptions{Filter: "trashed", Search: search})
}

// TrashedRecordingIDs returns the id of every recording in the trashed bin (no
// paging), ordered by id — the Recordings bin's "select all N" bulk target
// (body {action, all:true}).
func (db *DB) TrashedRecordingIDs(ctx context.Context) ([]int64, error) {
	return scanIDs(db.QueryContext(ctx,
		`SELECT r.id FROM recordings r WHERE `+recordingFilterClause("trashed")+` ORDER BY r.id`))
}

// restoreRecordingTx brings one recording back into the library: it un-trashes
// every trashed appearance and, if the recording is dormant (no surviving
// rendition), restores its ladder-best removed blob so the appearances can play.
// Returns exists=false (no change) when the id is unknown.
func restoreRecordingTx(ctx context.Context, tx *sql.Tx, recordingID int64) (bool, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM recordings WHERE id = ?)`, recordingID).Scan(&exists); err != nil {
		return false, fmt.Errorf("restore recording: check: %w", err)
	}
	if !exists {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE tagsets SET deleted_at = NULL WHERE recording_id = ? AND deleted_at IS NOT NULL`,
		recordingID); err != nil {
		return false, fmt.Errorf("restore recording: un-trash appearances: %w", err)
	}
	var live int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files WHERE recording_id = ? AND deleted_at IS NULL`,
		recordingID).Scan(&live); err != nil {
		return false, fmt.Errorf("restore recording: count live: %w", err)
	}
	if live == 0 {
		// Dormant: restore the ladder-best removed rendition (ignoring the manual
		// preference, which may itself point at a removed blob) so the recording
		// plays again.
		var bestID int64
		err := tx.QueryRowContext(ctx, `
			SELECT f.id FROM files f
			LEFT JOIN media_metadata mm ON mm.file_id = f.id
			WHERE f.recording_id = ? AND f.deleted_at IS NOT NULL
			ORDER BY `+renditionLadderOrder("f", "mm", "NULL")+`
			LIMIT 1`, recordingID).Scan(&bestID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("restore recording: best removed: %w", err)
		}
		if bestID != 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE files SET deleted_at = NULL WHERE id = ?`, bestID); err != nil {
				return false, fmt.Errorf("restore recording: restore rendition: %w", err)
			}
		}
	}
	return true, nil
}

// RestoreRecording brings a whole recording back into the library (the Trash
// Recordings "Restore"). Returns found=false (no change) for an unknown id.
func (db *DB) RestoreRecording(ctx context.Context, recordingID int64) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("restore recording: begin: %w", err)
	}
	defer tx.Rollback()

	found, err := restoreRecordingTx(ctx, tx, recordingID)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("restore recording: commit: %w", err)
	}
	return found, nil
}

// BulkRestoreRecordings restores every listed recording in one transaction — the
// Recordings bin's "Restore selected". Unknown ids are skipped. Returns the
// count restored.
func (db *DB) BulkRestoreRecordings(ctx context.Context, recordingIDs []int64) (int, error) {
	if len(recordingIDs) == 0 {
		return 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("bulk restore recordings: begin: %w", err)
	}
	defer tx.Rollback()

	restored := 0
	for _, id := range recordingIDs {
		found, err := restoreRecordingTx(ctx, tx, id)
		if err != nil {
			return 0, err
		}
		if found {
			restored++
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("bulk restore recordings: commit: %w", err)
	}
	return restored, nil
}

// BulkHardDeleteRecordings permanently removes every listed recording (with all
// appearances + files) in one transaction — the Recordings bin's "Delete
// selected". Unknown ids are skipped. Returns recordings deleted + every blob to
// reclaim after commit. Routes through the shared tagset-first cascade.
func (db *DB) BulkHardDeleteRecordings(ctx context.Context, recordingIDs []int64) (int, []DeletedBlob, error) {
	if len(recordingIDs) == 0 {
		return 0, nil, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("bulk hard delete recordings: begin: %w", err)
	}
	defer tx.Rollback()

	var blobs []DeletedBlob
	deleted := 0
	for _, id := range recordingIDs {
		var exists bool
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM recordings WHERE id = ?)`, id).Scan(&exists); err != nil {
			return 0, nil, fmt.Errorf("bulk hard delete recordings: check %d: %w", id, err)
		}
		if !exists {
			continue
		}
		tagsetIDs, err := scanIDs(tx.QueryContext(ctx,
			`SELECT id FROM tagsets WHERE recording_id = ?`, id))
		if err != nil {
			return 0, nil, fmt.Errorf("bulk hard delete recordings: tagsets %d: %w", id, err)
		}
		if len(tagsetIDs) > 0 {
			recBlobs, err := purgeTagsetsTx(ctx, tx, tagsetIDs)
			if err != nil {
				return 0, nil, err
			}
			blobs = append(blobs, recBlobs...)
		} else {
			// Already appearance-less (garbage): reclaim files + husk directly.
			recBlobs, err := reclaimAbandonedRecordingsTx(ctx, tx, []int64{id})
			if err != nil {
				return 0, nil, err
			}
			blobs = append(blobs, recBlobs...)
		}
		deleted++
	}
	if err := tx.Commit(); err != nil {
		return 0, nil, fmt.Errorf("bulk hard delete recordings: commit: %w", err)
	}
	return deleted, blobs, nil
}
