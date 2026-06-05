package database

import (
	"context"
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

func TestFinishImageJob_SetsVariantsReady(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	const (
		artist  = "Artist"
		album   = "Album"
		baseKey = "deadbeefcafe0001"
	)
	objectKey := baseKey + "/original.jpg"
	if err := db.SetAlbumCover(ctx, artist, album, baseKey, ".jpg", objectKey, "image/jpeg", 1000); err != nil {
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

	_, _, ready, found, err := db.GetAlbumCoverStatus(ctx, artist, album)
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
