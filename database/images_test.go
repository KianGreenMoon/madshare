package database

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

func countActiveJobs(t *testing.T, db *DB, baseKey string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM image_processing_jobs WHERE base_key = ?`, baseKey,
	).Scan(&n); err != nil {
		t.Fatalf("count jobs: %v", err)
	}
	return n
}

func TestEnqueueImageJob_Idempotent(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	if err := db.EnqueueImageJob(ctx, "album", "art\x1falb", "abc123", 1000); err != nil {
		t.Fatalf("first enqueue: %v", err)
	}
	if err := db.EnqueueImageJob(ctx, "album", "art\x1falb", "abc123", 1001); err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if got := countActiveJobs(t, db, "abc123"); got != 1 {
		t.Fatalf("active jobs for base_key = %d, want 1 (idempotent enqueue)", got)
	}
}

func TestSetAlbumCoverIfAbsent(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	albumID, err := db.ResolveAlbumID(ctx, "Artist", "Album")
	if err != nil {
		t.Fatalf("resolve album: %v", err)
	}

	// First insert wins.
	inserted, err := db.SetAlbumCoverIfAbsent(ctx, albumID, "key1aaaaaaaaaaaa", ".jpg", "key1aaaaaaaaaaaa/original.jpg", "image/jpeg", 1000)
	if err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if !inserted {
		t.Fatal("first SetAlbumCoverIfAbsent returned inserted=false, want true")
	}

	// Second call for the same album is a no-op and must not overwrite.
	inserted, err = db.SetAlbumCoverIfAbsent(ctx, albumID, "key2bbbbbbbbbbbb", ".png", "key2bbbbbbbbbbbb/original.png", "image/png", 2000)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if inserted {
		t.Error("second SetAlbumCoverIfAbsent returned inserted=true, want false (row already exists)")
	}

	baseKey, sourceExt, _, found, err := db.GetAlbumCoverStatus(ctx, albumID)
	if err != nil || !found {
		t.Fatalf("GetAlbumCoverStatus: found=%v err=%v", found, err)
	}
	if baseKey != "key1aaaaaaaaaaaa" || sourceExt != ".jpg" {
		t.Errorf("stored cover = (%q,%q), want the first insert (key1…, .jpg) — must not be overwritten", baseKey, sourceExt)
	}
}

// TestSetAlbumCoverIfAbsent_ConcurrentSingleWinner runs many concurrent claims
// for the same album on an on-disk DB (real multi-connection pool) and asserts
// exactly one reports inserted=true.
func TestSetAlbumCoverIfAbsent_ConcurrentSingleWinner(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "claim.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()

	albumID, err := db.ResolveAlbumID(ctx, "A", "B")
	if err != nil {
		t.Fatalf("resolve album: %v", err)
	}

	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			bk := fmt.Sprintf("base%012d", i)
			<-start
			ok, err := db.SetAlbumCoverIfAbsent(ctx, albumID, bk, ".jpg", bk+"/original.jpg", "image/jpeg", int64(i))
			if err != nil {
				t.Errorf("claim %d: %v", i, err)
				return
			}
			if ok {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Errorf("concurrent winners = %d, want exactly 1", wins)
	}
}

func TestResetStaleJobs(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	if err := db.EnqueueImageJob(ctx, "album", "art\x1falb", "stale", 1000); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	job, err := db.ClaimImageJob(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if job == nil {
		t.Fatal("claim returned nil job, want a running job")
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM image_processing_jobs WHERE id = ?`, job.ID).Scan(&status); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if status != "running" {
		t.Fatalf("claimed job status = %q, want running", status)
	}

	if err := db.ResetStaleJobs(ctx); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := db.QueryRow(`SELECT status FROM image_processing_jobs WHERE id = ?`, job.ID).Scan(&status); err != nil {
		t.Fatalf("read status after reset: %v", err)
	}
	if status != "pending" {
		t.Fatalf("status after reset = %q, want pending", status)
	}
}

// jobStatus returns the status of the single job for base_key, or "" if none.
func jobStatus(t *testing.T, db *DB, baseKey string) string {
	t.Helper()
	var s string
	err := db.QueryRow(
		`SELECT status FROM image_processing_jobs WHERE base_key = ?`, baseKey,
	).Scan(&s)
	if err == sql.ErrNoRows {
		return ""
	}
	if err != nil {
		t.Fatalf("read job status: %v", err)
	}
	return s
}

func TestRequeueStuckImageJobs(t *testing.T) {
	ctx := context.Background()

	// A stuck cover (variants_ready=0) with no job is re-enqueued exactly once.
	t.Run("requeues stuck no-job row", func(t *testing.T) {
		db := openMem(t)
		const baseKey = "stuck00000000001"
		albumID, err := db.ResolveAlbumID(ctx, "Artist", "Album")
		if err != nil {
			t.Fatalf("resolve album: %v", err)
		}
		if err := db.SetAlbumCover(ctx, albumID, baseKey, ".jpg", baseKey+"/original.jpg", "image/jpeg", 1000); err != nil {
			t.Fatalf("set cover: %v", err)
		}

		n, err := db.RequeueStuckImageJobs(ctx)
		if err != nil {
			t.Fatalf("requeue: %v", err)
		}
		if n != 1 {
			t.Fatalf("requeued = %d, want 1", n)
		}
		if got := countActiveJobs(t, db, baseKey); got != 1 {
			t.Fatalf("jobs for base_key = %d, want 1", got)
		}
		if got := jobStatus(t, db, baseKey); got != "pending" {
			t.Fatalf("job status = %q, want pending", got)
		}

		// Running again is a no-op now that a pending job exists.
		if n, err := db.RequeueStuckImageJobs(ctx); err != nil || n != 0 {
			t.Fatalf("second requeue = (%d,%v), want (0,nil)", n, err)
		}
		if got := countActiveJobs(t, db, baseKey); got != 1 {
			t.Fatalf("jobs after second requeue = %d, want 1 (idempotent)", got)
		}
	})

	// variants_ready=1 covers are already done and must be left alone.
	t.Run("skips ready cover", func(t *testing.T) {
		db := openMem(t)
		const baseKey = "ready00000000001"
		albumID, err := db.ResolveAlbumID(ctx, "Artist", "Album")
		if err != nil {
			t.Fatalf("resolve album: %v", err)
		}
		if err := db.SetAlbumCover(ctx, albumID, baseKey, ".jpg", baseKey+"/original.jpg", "image/jpeg", 1000); err != nil {
			t.Fatalf("set cover: %v", err)
		}
		if _, err := db.Exec(`UPDATE album_images SET variants_ready=1 WHERE base_key=?`, baseKey); err != nil {
			t.Fatalf("mark ready: %v", err)
		}

		if n, err := db.RequeueStuckImageJobs(ctx); err != nil || n != 0 {
			t.Fatalf("requeue = (%d,%v), want (0,nil)", n, err)
		}
		if got := countActiveJobs(t, db, baseKey); got != 0 {
			t.Fatalf("jobs for ready cover = %d, want 0", got)
		}
	})

	// A terminal 'failed' job (retried out, e.g. corrupt embedded cover) must
	// stay failed — re-enqueuing it would retry corrupt images on every restart.
	t.Run("skips terminal failed job", func(t *testing.T) {
		db := openMem(t)
		const baseKey = "failed0000000001"
		albumID, err := db.ResolveAlbumID(ctx, "Artist", "Album")
		if err != nil {
			t.Fatalf("resolve album: %v", err)
		}
		if err := db.SetAlbumCover(ctx, albumID, baseKey, ".jpg", baseKey+"/original.jpg", "image/jpeg", 1000); err != nil {
			t.Fatalf("set cover: %v", err)
		}
		if _, err := db.Exec(
			`INSERT INTO image_processing_jobs (cover_type, subject_key, base_key, status, retry_count, created_at, finished_at)
			 VALUES ('album', 'Artist`+"\x1f"+`Album', ?, 'failed', 3, 1000, 1001)`, baseKey,
		); err != nil {
			t.Fatalf("seed failed job: %v", err)
		}

		if n, err := db.RequeueStuckImageJobs(ctx); err != nil || n != 0 {
			t.Fatalf("requeue = (%d,%v), want (0,nil)", n, err)
		}
		if got := countActiveJobs(t, db, baseKey); got != 1 {
			t.Fatalf("jobs for failed cover = %d, want 1 (the failed job, untouched)", got)
		}
		if got := jobStatus(t, db, baseKey); got != "failed" {
			t.Fatalf("job status = %q, want failed (terminal, untouched)", got)
		}
	})

	// Two albums sharing one cover image (same base_key) collapse to one job.
	t.Run("collapses shared base_key to one job", func(t *testing.T) {
		db := openMem(t)
		const baseKey = "shared0000000001"
		a1, err := db.ResolveAlbumID(ctx, "Artist", "Album One")
		if err != nil {
			t.Fatalf("resolve album 1: %v", err)
		}
		a2, err := db.ResolveAlbumID(ctx, "Artist", "Album Two")
		if err != nil {
			t.Fatalf("resolve album 2: %v", err)
		}
		for _, id := range []int64{a1, a2} {
			if err := db.SetAlbumCover(ctx, id, baseKey, ".jpg", baseKey+"/original.jpg", "image/jpeg", 1000); err != nil {
				t.Fatalf("set cover %d: %v", id, err)
			}
		}

		n, err := db.RequeueStuckImageJobs(ctx)
		if err != nil {
			t.Fatalf("requeue: %v", err)
		}
		if n != 1 {
			t.Fatalf("requeued = %d, want 1 (shared base_key collapses)", n)
		}
		if got := countActiveJobs(t, db, baseKey); got != 1 {
			t.Fatalf("jobs for shared base_key = %d, want 1", got)
		}
	})
}

func TestFinishImageJob_SetsVariantsReady(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	const (
		artist  = "Artist"
		album   = "Album"
		baseKey = "deadbeefcafe0001"
	)
	objectKey := baseKey + "/original.jpg"
	albumID, err := db.ResolveAlbumID(ctx, artist, album)
	if err != nil {
		t.Fatalf("resolve album: %v", err)
	}
	if err := db.SetAlbumCover(ctx, albumID, baseKey, ".jpg", objectKey, "image/jpeg", 1000); err != nil {
		t.Fatalf("set album cover: %v", err)
	}
	if err := db.EnqueueImageJob(ctx, "album", artist+"\x1f"+album, baseKey, 1000); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	job, err := db.ClaimImageJob(ctx)
	if err != nil || job == nil {
		t.Fatalf("claim: job=%v err=%v", job, err)
	}

	if err := db.FinishImageJob(ctx, job.ID, nil); err != nil {
		t.Fatalf("finish: %v", err)
	}

	_, _, ready, found, err := db.GetAlbumCoverStatus(ctx, albumID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !found {
		t.Fatal("album cover not found after finish")
	}
	if !ready {
		t.Fatal("variants_ready = false after successful finish, want true")
	}

	var status string
	if err := db.QueryRow(`SELECT status FROM image_processing_jobs WHERE id = ?`, job.ID).Scan(&status); err != nil {
		t.Fatalf("read job status: %v", err)
	}
	if status != "done" {
		t.Fatalf("job status = %q, want done", status)
	}
}

// TestClaimImageJob_Concurrent seeds a queue and has many goroutines drain it on
// a real on-disk DB (multi-connection pool). It asserts no job is claimed twice
// and all are claimed exactly once — locking in ClaimImageJob's atomic-claim
// guarantee under genuine connection-level concurrency.
func TestClaimImageJob_Concurrent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "claim.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()

	const nJobs = 200
	for i := 0; i < nJobs; i++ {
		// Distinct base_key per job so the active-job unique index never collapses them.
		if err := db.EnqueueImageJob(ctx, "album", fmt.Sprintf("a\x1f%d", i), fmt.Sprintf("key%04d", i), int64(i)); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}

	const nWorkers = 8
	var (
		mu      sync.Mutex
		claimed = make(map[int64]int)
		wg      sync.WaitGroup
	)
	for w := 0; w < nWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				job, err := db.ClaimImageJob(ctx)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if job == nil {
					return // queue drained
				}
				mu.Lock()
				claimed[job.ID]++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(claimed) != nJobs {
		t.Errorf("claimed %d distinct jobs, want %d", len(claimed), nJobs)
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("job %d claimed %d times, want exactly 1", id, n)
		}
	}
}

func TestFinishImageJob_RequeuesOnError(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	if err := db.EnqueueImageJob(ctx, "album", "a\x1fb", "key", 1000); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	job, err := db.ClaimImageJob(ctx)
	if err != nil || job == nil {
		t.Fatalf("claim: job=%v err=%v", job, err)
	}

	// First two failures requeue; the third marks the job failed.
	for i := 0; i < 2; i++ {
		if err := db.FinishImageJob(ctx, job.ID, context.DeadlineExceeded); err != nil {
			t.Fatalf("finish (failure %d): %v", i, err)
		}
		var status string
		if err := db.QueryRow(`SELECT status FROM image_processing_jobs WHERE id = ?`, job.ID).Scan(&status); err != nil {
			t.Fatalf("status: %v", err)
		}
		if status != "pending" {
			t.Fatalf("after failure %d status = %q, want pending", i, status)
		}
		// Re-claim for the next round.
		if job, err = db.ClaimImageJob(ctx); err != nil || job == nil {
			t.Fatalf("re-claim: job=%v err=%v", job, err)
		}
	}
	if err := db.FinishImageJob(ctx, job.ID, context.DeadlineExceeded); err != nil {
		t.Fatalf("finish (final failure): %v", err)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM image_processing_jobs WHERE id = ?`, job.ID).Scan(&status); err != nil {
		t.Fatalf("status: %v", err)
	}
	if status != "failed" {
		t.Fatalf("after 3 failures status = %q, want failed", status)
	}
}
