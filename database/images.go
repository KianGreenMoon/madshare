package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// maxImageJobRetries is the number of times a failing job is retried before it
// is marked status='failed' and abandoned.
const maxImageJobRetries = 3

// ImageJob is a claimed row from the image_processing_jobs queue.
type ImageJob struct {
	ID         int64
	CoverType  string
	SubjectKey string // "<album_artist>\x1f<album_title>" for albums
	BaseKey    string
	RetryCount int
}

// EnqueueImageJob inserts a new pending image_processing_jobs row. It is
// idempotent at the DB level: the partial unique index idx_imgproc_active (one
// active job per base_key) plus ON CONFLICT DO NOTHING means concurrent
// enqueues for the same base_key collapse to a single active job. Returns nil
// when a job already exists.
func (db *DB) EnqueueImageJob(ctx context.Context, coverType, subjectKey, baseKey string, now int64) error {
	_, err := db.ExecContext(ctx,
		`INSERT INTO image_processing_jobs (cover_type, subject_key, base_key, status, created_at)
		 VALUES (?, ?, ?, 'pending', ?)
		 ON CONFLICT DO NOTHING`,
		coverType, subjectKey, baseKey, now,
	)
	if err != nil {
		return fmt.Errorf("enqueue image job: %w", err)
	}
	return nil
}

// ClaimImageJob atomically claims the oldest pending job, flipping it to
// running, and returns it. The claim is a single UPDATE ... RETURNING so two
// workers on different pool connections cannot grab the same row. Returns
// (nil, nil) when the queue is empty.
func (db *DB) ClaimImageJob(ctx context.Context) (*ImageJob, error) {
	var j ImageJob
	err := db.QueryRowContext(ctx,
		`UPDATE image_processing_jobs SET status='running', started_at=?
		 WHERE id = (
		     SELECT id FROM image_processing_jobs
		     WHERE status='pending'
		     ORDER BY created_at, id
		     LIMIT 1
		 )
		 RETURNING id, cover_type, subject_key, base_key, retry_count`,
		time.Now().Unix(),
	).Scan(&j.ID, &j.CoverType, &j.SubjectKey, &j.BaseKey, &j.RetryCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim image job: %w", err)
	}
	return &j, nil
}

// FinishImageJob records the outcome of a claimed job. It owns the full
// done/retry/failed decision so the worker never touches retry_count:
//   - jobErr == nil: status='done', finished_at=now, and variants_ready=1 on
//     every album_images row sharing the job's base_key.
//   - jobErr != nil: retry_count is incremented; if it then reaches
//     maxImageJobRetries the job is marked 'failed' (finished_at=now);
//     otherwise it returns to 'pending' with started_at=NULL so another worker
//     re-claims it.
func (db *DB) FinishImageJob(ctx context.Context, id int64, jobErr error) error {
	now := time.Now().Unix()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("finish image job: begin: %w", err)
	}
	defer tx.Rollback()

	if jobErr == nil {
		if _, err := tx.ExecContext(ctx,
			`UPDATE image_processing_jobs SET status='done', error=NULL, finished_at=? WHERE id=?`,
			now, id,
		); err != nil {
			return fmt.Errorf("finish image job: mark done: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE album_images SET variants_ready=1
			 WHERE base_key = (SELECT base_key FROM image_processing_jobs WHERE id=?)`,
			id,
		); err != nil {
			return fmt.Errorf("finish image job: set variants_ready: %w", err)
		}
	} else {
		// Increment retry_count and branch on the new value. started_at is
		// cleared on requeue so a re-claim resets the running clock.
		if _, err := tx.ExecContext(ctx,
			`UPDATE image_processing_jobs
			 SET retry_count = retry_count + 1,
			     error = ?,
			     status = CASE WHEN retry_count + 1 >= ? THEN 'failed' ELSE 'pending' END,
			     started_at = CASE WHEN retry_count + 1 >= ? THEN started_at ELSE NULL END,
			     finished_at = CASE WHEN retry_count + 1 >= ? THEN ? ELSE NULL END
			 WHERE id = ?`,
			jobErr.Error(), maxImageJobRetries, maxImageJobRetries, maxImageJobRetries, now, id,
		); err != nil {
			return fmt.Errorf("finish image job: record failure: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("finish image job: commit: %w", err)
	}
	return nil
}

// RequeueStuckImageJobs re-enqueues image jobs for album covers stuck at
// variants_ready=0 with no job in the queue. This recovers the row whose claim
// succeeded but whose EnqueueImageJob then errored (a genuine DB failure), which
// would otherwise leave the cover "processing" forever — the original blob is on
// disk (written before the row is claimed), so a fresh job is all that's needed.
// Called once at startup, after the worker pool launches. Returns the number of
// jobs created.
//
// Only base_keys with no job row at all are requeued: a base_key whose job is
// already pending/running is in flight, and one marked 'failed' is terminal (it
// was retried maxImageJobRetries times — typically a corrupt/mislabelled embedded
// cover) and must not be retried on every restart. Multiple albums sharing one
// cover (same base_key) collapse to a single job via GROUP BY, matching the
// queue's per-base_key grain and the idx_imgproc_active unique index. Scope is
// album_images only — artist_images has no worker pipeline. subject_key is
// reconstructed for debuggability only; the worker keys off base_key.
func (db *DB) RequeueStuckImageJobs(ctx context.Context) (int, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO image_processing_jobs (cover_type, subject_key, base_key, status, created_at)
		 SELECT 'album',
		        COALESCE(ar.name, '') || char(31) || COALESCE(al.title, ''),
		        ai.base_key, 'pending', ?
		 FROM album_images ai
		 JOIN albums  al ON al.id = ai.album_id
		 JOIN artists ar ON ar.id = al.artist_id
		 WHERE ai.variants_ready = 0
		   AND ai.base_key IS NOT NULL AND ai.base_key <> ''
		   AND NOT EXISTS (SELECT 1 FROM image_processing_jobs j WHERE j.base_key = ai.base_key)
		 GROUP BY ai.base_key
		 ON CONFLICT DO NOTHING`,
		time.Now().Unix(),
	)
	if err != nil {
		return 0, fmt.Errorf("requeue stuck image jobs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("requeue stuck image jobs: rows affected: %w", err)
	}
	return int(n), nil
}

// ResetStaleJobs returns all status='running' jobs to 'pending' (clearing
// started_at). Called once at startup, before workers launch, to recover jobs
// that were in flight when the process died.
func (db *DB) ResetStaleJobs(ctx context.Context) error {
	_, err := db.ExecContext(ctx,
		`UPDATE image_processing_jobs SET status='pending', started_at=NULL WHERE status='running'`,
	)
	if err != nil {
		return fmt.Errorf("reset stale image jobs: %w", err)
	}
	return nil
}

// SetAlbumCover inserts or replaces the album_images row for the album entity
// with the given variant-tracking fields. variants_ready is reset to 0 — the
// async worker flips it to 1 via FinishImageJob once variants are generated.
func (db *DB) SetAlbumCover(ctx context.Context, albumID int64, baseKey, sourceExt, objectKey, mimeType string, now int64) error {
	_, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO album_images
		     (album_id, object_key, mime_type, updated_at, base_key, source_ext, variants_ready)
		 VALUES (?, ?, ?, ?, ?, ?, 0)`,
		albumID, objectKey, mimeType, now, baseKey, sourceExt,
	)
	if err != nil {
		return fmt.Errorf("set album cover: %w", err)
	}
	return nil
}

// SetAlbumCoverIfAbsent atomically inserts an album_images row for the album
// entity only when none exists yet, returning inserted=true exactly when this
// call created the row. It is the race-free form of the fill-if-missing rule:
// when several tracks of the same album are uploaded concurrently they all see
// "no cover" via HasAlbumCover, but only the single caller that wins this
// INSERT ... ON CONFLICT DO NOTHING gets inserted=true and proceeds to enqueue
// variant processing — the rest are no-ops. Unlike SetAlbumCover it never
// overwrites an existing cover, so a previously stored (e.g. manually uploaded)
// cover always wins over embedded art. variants_ready starts at 0.
func (db *DB) SetAlbumCoverIfAbsent(ctx context.Context, albumID int64, baseKey, sourceExt, objectKey, mimeType string, now int64) (bool, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO album_images
		     (album_id, object_key, mime_type, updated_at, base_key, source_ext, variants_ready)
		 VALUES (?, ?, ?, ?, ?, ?, 0)
		 ON CONFLICT(album_id) DO NOTHING`,
		albumID, objectKey, mimeType, now, baseKey, sourceExt,
	)
	if err != nil {
		return false, fmt.Errorf("set album cover if absent: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("set album cover if absent: rows affected: %w", err)
	}
	return n > 0, nil
}

// GetAlbumCoverStatus returns the variant-tracking state for an album cover.
// found is false (no error) when no album_images row exists. base_key and
// source_ext may be empty for legacy rows written before variants existed.
func (db *DB) GetAlbumCoverStatus(ctx context.Context, albumID int64) (baseKey, sourceExt string, variantsReady, found bool, err error) {
	var (
		bk, se sql.NullString
		ready  int
	)
	row := db.QueryRowContext(ctx,
		`SELECT base_key, source_ext, variants_ready FROM album_images WHERE album_id = ?`,
		albumID,
	)
	if err = row.Scan(&bk, &se, &ready); errors.Is(err, sql.ErrNoRows) {
		return "", "", false, false, nil
	}
	if err != nil {
		return "", "", false, false, fmt.Errorf("get album cover status: %w", err)
	}
	return bk.String, se.String, ready == 1, true, nil
}

// HasAlbumCover reports whether an album_images row exists for the album entity,
// regardless of variants_ready. Used by the fill-if-missing logic.
func (db *DB) HasAlbumCover(ctx context.Context, albumID int64) (bool, error) {
	var one int
	row := db.QueryRowContext(ctx,
		`SELECT 1 FROM album_images WHERE album_id = ?`, albumID,
	)
	if err := row.Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("has album cover: %w", err)
	}
	return true, nil
}
