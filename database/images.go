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

// SetAlbumCover inserts or replaces the album_images row for (artist, album)
// with the given variant-tracking fields. variants_ready is reset to 0 — the
// async worker flips it to 1 via FinishImageJob once variants are generated.
func (db *DB) SetAlbumCover(ctx context.Context, artist, album, baseKey, sourceExt, objectKey, mimeType string, now int64) error {
	_, err := db.ExecContext(ctx,
		`INSERT OR REPLACE INTO album_images
		     (album_artist, album_title, object_key, mime_type, updated_at, base_key, source_ext, variants_ready)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
		artist, album, objectKey, mimeType, now, baseKey, sourceExt,
	)
	if err != nil {
		return fmt.Errorf("set album cover: %w", err)
	}
	return nil
}

// SetAlbumCoverIfAbsent atomically inserts an album_images row for (artist,
// album) only when none exists yet, returning inserted=true exactly when this
// call created the row. It is the race-free form of the fill-if-missing rule:
// when several tracks of the same album are uploaded concurrently they all see
// "no cover" via HasAlbumCover, but only the single caller that wins this
// INSERT ... ON CONFLICT DO NOTHING gets inserted=true and proceeds to enqueue
// variant processing — the rest are no-ops. Unlike SetAlbumCover it never
// overwrites an existing cover, so a previously stored (e.g. manually uploaded)
// cover always wins over embedded art. variants_ready starts at 0.
func (db *DB) SetAlbumCoverIfAbsent(ctx context.Context, artist, album, baseKey, sourceExt, objectKey, mimeType string, now int64) (bool, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO album_images
		     (album_artist, album_title, object_key, mime_type, updated_at, base_key, source_ext, variants_ready)
		 VALUES (?, ?, ?, ?, ?, ?, ?, 0)
		 ON CONFLICT(album_artist, album_title) DO NOTHING`,
		artist, album, objectKey, mimeType, now, baseKey, sourceExt,
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
func (db *DB) GetAlbumCoverStatus(ctx context.Context, artist, album string) (baseKey, sourceExt string, variantsReady, found bool, err error) {
	var (
		bk, se sql.NullString
		ready  int
	)
	row := db.QueryRowContext(ctx,
		`SELECT base_key, source_ext, variants_ready FROM album_images
		 WHERE album_artist = ? AND album_title = ?`,
		artist, album,
	)
	if err = row.Scan(&bk, &se, &ready); errors.Is(err, sql.ErrNoRows) {
		return "", "", false, false, nil
	}
	if err != nil {
		return "", "", false, false, fmt.Errorf("get album cover status: %w", err)
	}
	return bk.String, se.String, ready == 1, true, nil
}

// HasAlbumCover reports whether an album_images row exists for the album,
// regardless of variants_ready. Used by the fill-if-missing logic.
func (db *DB) HasAlbumCover(ctx context.Context, artist, album string) (bool, error) {
	var one int
	row := db.QueryRowContext(ctx,
		`SELECT 1 FROM album_images WHERE album_artist = ? AND album_title = ?`,
		artist, album,
	)
	if err := row.Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("has album cover: %w", err)
	}
	return true, nil
}
