package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// The purge primitives of the GC deletion model
// (docs/architecture/gc-model.md). Purge is the only destroyer in the model:
// delete-forever entry points compose these row-level primitives instead of
// each carrying its own cascade — the single place row destruction happens,
// so no code path can strand a recording or a blob by forgetting a guard.
//
// The composition sanctioned by the design doc for immediate disk reclaim
// ("purge → reap → purge") is purgeTagsetsTx: purge the trashed appearance
// rows, then reclaim every recording the purge left without any appearance —
// its file rows and blobs go too, in the same transaction, preserving the
// owner rule "nothing left to reach it = prune everything".

// inClause builds "(?,?,…)" and the matching args for one ≤400-id batch.
const idChunk = 400

func inClause(ids []int64) (string, []any) {
	ph := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		ph[i] = "?"
		args[i] = id
	}
	return "(" + strings.Join(ph, ",") + ")", args
}

// deleteTagsetRowsTx deletes the given tagset rows and returns the DISTINCT
// recording ids they belonged to. Pure row removal — no cascade, no repair;
// callers guard what may be passed (delete-forever paths pass trashed rows,
// the whole-recording destroy passes everything, appearance dedup passes the
// redundant duplicates it is folding).
func deleteTagsetRowsTx(ctx context.Context, tx *sql.Tx, ids []int64) ([]int64, error) {
	recIDs := make(map[int64]struct{})
	for i := 0; i < len(ids); i += idChunk {
		batch := ids[i:min(i+idChunk, len(ids))]
		in, args := inClause(batch)
		got, err := scanIDs(tx.QueryContext(ctx,
			`SELECT DISTINCT recording_id FROM tagsets WHERE id IN `+in, args...))
		if err != nil {
			return nil, fmt.Errorf("purge tagsets: recordings: %w", err)
		}
		for _, id := range got {
			recIDs[id] = struct{}{}
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM tagsets WHERE id IN `+in, args...); err != nil {
			return nil, fmt.Errorf("purge tagsets: delete: %w", err)
		}
	}
	out := make([]int64, 0, len(recIDs))
	for id := range recIDs {
		out = append(out, id)
	}
	return out, nil
}

// deleteFileRowsTx deletes the given file rows, returning their blobs (for
// post-commit reclamation) and the DISTINCT recording ids affected. Two GC
// rules apply inside:
//   - a draft dies with its origin blob: non-approved appearances whose
//     origin_file_id is being purged are discarded (trashed, not deleted — a
//     submission without its bytes is meaningless, but purge never destroys
//     rows that were not in Trash);
//   - approved appearances merely lose their provenance pointer
//     (origin_file_id → NULL via the FK's SET NULL) — it is inert audit data
//     after approval, never re-pointed and never a delete key.
//
// Callers guard trash state: the Trash › Files delete-forever passes only
// soft-removed rows, the prune blob-loss path passes any row whose bytes are
// gone (the row is a lie regardless of its trash mark).
func deleteFileRowsTx(ctx context.Context, tx *sql.Tx, ids []int64) ([]DeletedBlob, []int64, error) {
	var blobs []DeletedBlob
	recIDs := make(map[int64]struct{})
	now := time.Now().Unix()
	for i := 0; i < len(ids); i += idChunk {
		batch := ids[i:min(i+idChunk, len(ids))]
		in, args := inClause(batch)

		rows, err := tx.QueryContext(ctx,
			`SELECT hash, storage_backend, recording_id FROM files WHERE id IN `+in, args...)
		if err != nil {
			return nil, nil, fmt.Errorf("purge files: load: %w", err)
		}
		for rows.Next() {
			var b DeletedBlob
			var recID int64
			if err := rows.Scan(&b.Hash, &b.StorageBackend, &recID); err != nil {
				rows.Close()
				return nil, nil, fmt.Errorf("purge files: scan: %w", err)
			}
			blobs = append(blobs, b)
			recIDs[recID] = struct{}{}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("purge files: rows: %w", err)
		}
		rows.Close()

		if _, err := tx.ExecContext(ctx,
			`UPDATE tagsets SET deleted_at = ?
			  WHERE deleted_at IS NULL AND review_state <> ?
			    AND origin_file_id IN `+in,
			append([]any{now, ReviewApproved}, args...)...); err != nil {
			return nil, nil, fmt.Errorf("purge files: discard drafts: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM files WHERE id IN `+in, args...); err != nil {
			return nil, nil, fmt.Errorf("purge files: delete: %w", err)
		}
	}
	out := make([]int64, 0, len(recIDs))
	for id := range recIDs {
		out = append(out, id)
	}
	return blobs, out, nil
}

// reclaimAbandonedRecordingsTx finishes a delete-forever: every recording in
// scope that was left without ANY appearance row is reclaimed — all its file
// rows are deleted (blobs returned; FK cascade clears media_metadata /
// file_uploads / fingerprints / source_files, sibling appearances on other
// recordings just lose their provenance pointer via SET NULL) and the
// recording row is dropped. Recordings that still hold appearances (live or
// trashed) are untouched: they remain reachable, at least through Trash.
func reclaimAbandonedRecordingsTx(ctx context.Context, tx *sql.Tx, recIDs []int64) ([]DeletedBlob, error) {
	var blobs []DeletedBlob
	for _, recID := range recIDs {
		var remaining int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM tagsets WHERE recording_id = ?`, recID).Scan(&remaining); err != nil {
			return nil, fmt.Errorf("reclaim recording %d: count: %w", recID, err)
		}
		if remaining > 0 {
			continue
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT hash, storage_backend FROM files WHERE recording_id = ?`, recID)
		if err != nil {
			return nil, fmt.Errorf("reclaim recording %d: files: %w", recID, err)
		}
		for rows.Next() {
			var b DeletedBlob
			if err := rows.Scan(&b.Hash, &b.StorageBackend); err != nil {
				rows.Close()
				return nil, fmt.Errorf("reclaim recording %d: scan: %w", recID, err)
			}
			blobs = append(blobs, b)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("reclaim recording %d: rows: %w", recID, err)
		}
		rows.Close()
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM files WHERE recording_id = ?`, recID); err != nil {
			return nil, fmt.Errorf("reclaim recording %d: delete files: %w", recID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM recordings WHERE id = ?`, recID); err != nil {
			return nil, fmt.Errorf("reclaim recording %d: delete: %w", recID, err)
		}
	}
	return blobs, nil
}

// purgeTagsetsTx is the appearance delete-forever composition
// (purge → reap → purge): drop the given tagset rows, then reclaim every
// recording that lost its last appearance — files, blobs, and husk included,
// in this same transaction, so "delete forever" frees the bytes immediately.
// Every tagset-addressed permanent-delete entry point goes through here.
func purgeTagsetsTx(ctx context.Context, tx *sql.Tx, ids []int64) ([]DeletedBlob, error) {
	recIDs, err := deleteTagsetRowsTx(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	return reclaimAbandonedRecordingsTx(ctx, tx, recIDs)
}

// reapRecordingsTx runs the reaper's demote-only passes over the given
// recordings inside the caller's transaction — the in-tx, scoped equivalent
// of db.Reap that every op which may leave a recording unreferenced (file
// purge, move, merge, split, dedup) calls on the recordings it touched.
// Demote only, mirroring the global reaper: files of appearance-less
// recordings are quarantined, appearances of file-less recordings are
// trashed, and only bare husks (no rows of either kind) are removed.
func reapRecordingsTx(ctx context.Context, tx *sql.Tx, recIDs []int64) error {
	now := time.Now().Unix()
	for i := 0; i < len(recIDs); i += idChunk {
		batch := recIDs[i:min(i+idChunk, len(recIDs))]
		in, args := inClause(batch)
		if _, err := tx.ExecContext(ctx,
			`UPDATE files SET deleted_at = ?
			  WHERE deleted_at IS NULL AND recording_id IN `+in+`
			    AND NOT EXISTS (SELECT 1 FROM tagsets t WHERE t.recording_id = files.recording_id)`,
			append([]any{now}, args...)...); err != nil {
			return fmt.Errorf("reap recordings: quarantine files: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE tagsets SET deleted_at = ?
			  WHERE deleted_at IS NULL AND recording_id IN `+in+`
			    AND NOT EXISTS (SELECT 1 FROM files f WHERE f.recording_id = tagsets.recording_id)`,
			append([]any{now}, args...)...); err != nil {
			return fmt.Errorf("reap recordings: trash tagsets: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM recordings WHERE id IN `+in+`
			    AND NOT EXISTS (SELECT 1 FROM tagsets t WHERE t.recording_id = recordings.id)
			    AND NOT EXISTS (SELECT 1 FROM files f WHERE f.recording_id = recordings.id)`,
			args...); err != nil {
			return fmt.Errorf("reap recordings: delete husks: %w", err)
		}
	}
	return nil
}
