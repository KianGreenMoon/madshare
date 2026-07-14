package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ApproveSubmission publishes a staged appearance (recording-tagsets P4) with
// the moderator's per-piece decisions applied atomically:
//
//   - forceNew: the submission's blob is split into a new pinned recording (its
//     appearance moves with it and becomes primary) before publishing — the
//     "this is actually new audio, the fingerprint match was wrong" override.
//     Ignored when the blob already carries an approved appearance (a byte-dup
//     shared blob — splitting it would strand the other appearance's recording);
//     such a submission is byte-identical, so "actually new" cannot apply.
//   - dropBytes: after publishing the appearance, the submitted rendition is
//     soft-removed (files.deleted_at) — the "keep the appearance, drop the blob"
//     absorb-at-the-gate (case B). The recording keeps serving its other
//     renditions; dropping the only rendition makes it dormant.
//
// The tagset must be a non-trashed submitted/returned appearance; found is false
// otherwise (unknown, trashed, wrong state, or lost a concurrent race). One
// transaction.
func (db *DB) ApproveSubmission(ctx context.Context, tagsetID int64, dropBytes, forceNew bool) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("approve submission: begin: %w", err)
	}
	defer tx.Rollback()

	var (
		originFile sql.NullInt64
		recID      int64
		state      string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT origin_file_id, recording_id, review_state FROM tagsets WHERE id = ? AND deleted_at IS NULL`,
		tagsetID,
	).Scan(&originFile, &recID, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("approve submission: load tagset: %w", err)
	}
	if state != ReviewSubmitted && state != ReviewReturned {
		return false, nil // not an actionable submission
	}

	now := time.Now().Unix()

	if forceNew && originFile.Valid {
		// Only split a blob that isn't already published under another appearance —
		// otherwise the move would strand that appearance's recording.
		var sharedApproved bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM tagsets t
			  WHERE t.origin_file_id = ? AND t.id <> ? AND t.deleted_at IS NULL AND t.review_state = 'approved')`,
			originFile.Int64, tagsetID,
		).Scan(&sharedApproved); err != nil {
			return false, fmt.Errorf("approve submission: shared check: %w", err)
		}
		if !sharedApproved {
			var newRec int64
			if err := tx.QueryRowContext(ctx,
				`INSERT INTO recordings (created_at) VALUES (?) RETURNING id`, now).Scan(&newRec); err != nil {
				return false, fmt.Errorf("approve submission: new recording: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE files SET recording_id = ?, recording_pinned = 1 WHERE id = ?`, newRec, originFile.Int64); err != nil {
				return false, fmt.Errorf("approve submission: reassign file: %w", err)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE tagsets SET recording_id = ?, is_primary = 1 WHERE origin_file_id = ?`, newRec, originFile.Int64); err != nil {
				return false, fmt.Errorf("approve submission: move tagsets: %w", err)
			}
			// Multiple moved appearances can't all be primary; keep the oldest.
			if _, err := tx.ExecContext(ctx,
				`UPDATE tagsets SET is_primary = 0
				  WHERE recording_id = ? AND id <> (SELECT MIN(id) FROM tagsets WHERE recording_id = ?)`,
				newRec, newRec); err != nil {
				return false, fmt.Errorf("approve submission: primary: %w", err)
			}
			if err := reapRecordingsTx(ctx, tx, []int64{recID}); err != nil {
				return false, err
			}
		}
	}

	// Publish the appearance (guarded, so a concurrent transition can't double-apply).
	res, err := tx.ExecContext(ctx,
		`UPDATE tagsets SET review_state = 'approved', review_note = NULL
		  WHERE id = ? AND deleted_at IS NULL AND review_state IN (?, ?)`,
		tagsetID, ReviewSubmitted, ReviewReturned)
	if err != nil {
		return false, fmt.Errorf("approve submission: publish: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil // lost the race
	}

	if dropBytes && originFile.Valid {
		// Soft-remove the submitted rendition (bytes kept, restorable). The ladder
		// recomputes; no tagset repair needed (drop never touches appearances).
		if _, err := tx.ExecContext(ctx,
			`UPDATE files SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, now, originFile.Int64); err != nil {
			return false, fmt.Errorf("approve submission: drop bytes: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("approve submission: commit: %w", err)
	}
	return true, nil
}
