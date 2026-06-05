package database

import (
	"context"
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
