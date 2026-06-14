package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"daemonlord.ygg/madshare/media"
)

// maxAnalysisJobRetries is the number of times a failing analysis job is retried
// before it is marked status='failed' and abandoned. Matches the image queue.
const maxAnalysisJobRetries = 3

// AnalysisJob is a claimed row from the media_analysis_jobs queue, joined with
// the file's content hash so the worker can locate the blob on disk.
type AnalysisJob struct {
	ID         int64
	FileID     int64
	Hash       string
	RetryCount int
}

// EnqueueAnalysisJob inserts a new pending media_analysis_jobs row. It is
// idempotent at the DB level: the partial unique index idx_media_analysis_active
// (one active job per file_id) plus ON CONFLICT DO NOTHING means concurrent or
// repeated enqueues for the same file collapse to a single active job.
func (db *DB) EnqueueAnalysisJob(ctx context.Context, fileID, now int64) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO media_analysis_jobs (file_id, status, created_at)
		 VALUES (?, 'pending', ?)
		 ON CONFLICT DO NOTHING`,
		fileID, now,
	)
	if err != nil {
		return fmt.Errorf("enqueue analysis job: %w", err)
	}
	return nil
}

// ClaimAnalysisJob atomically claims the oldest pending job (flipping it to
// running) and returns it joined with the file's hash, or (nil, nil) when the
// queue is empty. The claim is a single UPDATE ... RETURNING so two workers
// cannot grab the same row; the hash is fetched in a follow-up read.
func (db *DB) ClaimAnalysisJob(ctx context.Context) (*AnalysisJob, error) {
	var j AnalysisJob
	err := db.QueryRowContext(ctx,
		`UPDATE media_analysis_jobs SET status='running', started_at=?
		 WHERE id = (
		     SELECT id FROM media_analysis_jobs
		     WHERE status='pending'
		     ORDER BY created_at, id
		     LIMIT 1
		 )
		 RETURNING id, file_id, retry_count`,
		time.Now().Unix(),
	).Scan(&j.ID, &j.FileID, &j.RetryCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim analysis job: %w", err)
	}
	// Resolve the blob hash for the claimed file so the worker can find it on
	// disk. A racing hard-delete (FK cascade removed the job) cannot happen here
	// because the claim already holds the row, but the file row could in theory
	// be gone; treat that as an empty queue rather than a hard error.
	if err := db.QueryRowContext(ctx,
		`SELECT hash FROM files WHERE id = ?`, j.FileID,
	).Scan(&j.Hash); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("claim analysis job: resolve hash: %w", err)
	}
	return &j, nil
}

// FinishAnalysisJob records the outcome of a claimed job, owning the
// done/retry/failed decision (the worker never touches retry_count). Mirrors
// FinishImageJob: on success status='done'; on error retry_count is incremented
// and the job re-queues until maxAnalysisJobRetries, then 'failed'.
func (db *DB) FinishAnalysisJob(ctx context.Context, id int64, jobErr error) error {
	now := time.Now().Unix()
	if jobErr == nil {
		if _, err := db.ExecContext(ctx,
			`UPDATE media_analysis_jobs SET status='done', error=NULL, finished_at=? WHERE id=?`,
			now, id,
		); err != nil {
			return fmt.Errorf("finish analysis job: mark done: %w", err)
		}
		return nil
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE media_analysis_jobs
		 SET retry_count = retry_count + 1,
		     error = ?,
		     status = CASE WHEN retry_count + 1 >= ? THEN 'failed' ELSE 'pending' END,
		     started_at = CASE WHEN retry_count + 1 >= ? THEN started_at ELSE NULL END,
		     finished_at = CASE WHEN retry_count + 1 >= ? THEN ? ELSE NULL END
		 WHERE id = ?`,
		jobErr.Error(), maxAnalysisJobRetries, maxAnalysisJobRetries, maxAnalysisJobRetries, now, id,
	); err != nil {
		return fmt.Errorf("finish analysis job: record failure: %w", err)
	}
	return nil
}

// ResetStaleAnalysisJobs returns all status='running' analysis jobs to 'pending'
// (clearing started_at). Called once at startup, before workers launch, to
// recover jobs that were in flight when the process died.
func (db *DB) ResetStaleAnalysisJobs(ctx context.Context) error {
	_, err := db.ExecContext(ctx,
		`UPDATE media_analysis_jobs SET status='pending', started_at=NULL WHERE status='running'`,
	)
	if err != nil {
		return fmt.Errorf("reset stale analysis jobs: %w", err)
	}
	return nil
}

// UpsertTechColumns writes ffprobe-derived tech columns onto the file's
// media_metadata row. Zero-valued fields are stored as NULL (ffprobe didn't
// report them), so a partial probe never clobbers a known value with a bogus
// zero. No-op-safe: a file with no media_metadata row affects 0 rows.
func (db *DB) UpsertTechColumns(ctx context.Context, fileID int64, ti media.TechInfo) error {
	_, err := db.ExecContext(ctx,
		`UPDATE media_metadata SET
		     duration_seconds = ?,
		     bitrate          = ?,
		     sample_rate      = ?,
		     channels         = ?,
		     bit_depth        = ?,
		     codec            = ?
		 WHERE file_id = ?`,
		nullF64(ti.DurationSeconds),
		nullI64(int64(ti.Bitrate)),
		nullI64(int64(ti.SampleRate)),
		nullI64(int64(ti.Channels)),
		nullI64(int64(ti.BitDepth)),
		nullStr(ti.Codec),
		fileID,
	)
	if err != nil {
		return fmt.Errorf("upsert tech columns: %w", err)
	}
	return nil
}

// InsertAudioFingerprint stores (or replaces) the fingerprint row for a file.
// The raw sub-fingerprints are packed little-endian for compact storage and
// re-decoded by the resolver.
func (db *DB) InsertAudioFingerprint(ctx context.Context, fileID int64, fp media.Fingerprint, now int64) error {
	_, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO audio_fingerprints
		     (file_id, algo, algo_version, duration, fingerprint, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		fileID, fp.Algo, nullStr(fp.AlgoVersion), fp.Duration, fp.Packed(), now,
	)
	if err != nil {
		return fmt.Errorf("insert audio fingerprint: %w", err)
	}
	return nil
}

// FilesNeedingAnalysis returns the ids of non-trashed files that still lack
// analysis — no fingerprint row, or NULL tech columns (codec unset is the
// proxy). Drives the idempotent startup backfill; new uploads enqueue inline.
func (db *DB) FilesNeedingAnalysis(ctx context.Context) ([]int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT f.id
		   FROM files f
		   LEFT JOIN audio_fingerprints af ON af.file_id = f.id
		   LEFT JOIN media_metadata    mm ON mm.file_id = f.id
		  WHERE f.deleted_at IS NULL
		    AND (af.file_id IS NULL OR mm.codec IS NULL)
		  ORDER BY f.id`,
	)
	if err != nil {
		return nil, fmt.Errorf("files needing analysis: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("files needing analysis: scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// nullStr / nullI64 / nullF64 wrap a value as its sql.Null* with Valid set only
// for non-zero input — the "0/empty means unknown → NULL" rule for tech columns.
func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
func nullI64(i int64) sql.NullInt64   { return sql.NullInt64{Int64: i, Valid: i != 0} }
func nullF64(f float64) sql.NullFloat64 {
	return sql.NullFloat64{Float64: f, Valid: f != 0}
}
