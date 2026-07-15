package database

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"daemonlord.ygg/madshare/media"
)

// insertAnalysisFile inserts a file with the standard metadata row and returns
// its id and hash.
func insertAnalysisFile(t *testing.T, db *DB, seed string) (int64, string) {
	t.Helper()
	hash := hash64(seed)
	f := newFile(hash)
	if err := db.InsertFile(context.Background(), f, newUpload(seed+".mp3"), newMeta()); err != nil {
		t.Fatalf("InsertFile(%s): %v", seed, err)
	}
	return f.ID, hash
}

func activeJobCount(t *testing.T, db *DB, fileID int64) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM media_analysis_jobs WHERE file_id=? AND status IN ('pending','running')`,
		fileID,
	).Scan(&n); err != nil {
		t.Fatalf("count active jobs: %v", err)
	}
	return n
}

func TestEnqueueAnalysisJob_Idempotent(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fileID, _ := insertAnalysisFile(t, db, "aa")

	if err := db.EnqueueAnalysisJob(ctx, fileID, 1000); err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	if err := db.EnqueueAnalysisJob(ctx, fileID, 1001); err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}
	if got := activeJobCount(t, db, fileID); got != 1 {
		t.Errorf("active jobs after double enqueue = %d, want 1", got)
	}
}

func TestClaimAnalysisJob_ReturnsHashThenEmpty(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fileID, hash := insertAnalysisFile(t, db, "bb")
	if err := db.EnqueueAnalysisJob(ctx, fileID, 1000); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	job, err := db.ClaimAnalysisJob(ctx)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if job == nil {
		t.Fatal("claim returned nil, want a job")
	}
	if job.FileID != fileID || job.Hash != hash {
		t.Errorf("claimed job file=%d hash=%s, want %d / %s", job.FileID, job.Hash, fileID, hash)
	}
	// The claim flips it to running, so a second claim sees an empty queue.
	again, err := db.ClaimAnalysisJob(ctx)
	if err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if again != nil {
		t.Errorf("second claim = %+v, want nil (job already running)", again)
	}
}

func TestFinishAnalysisJob_Success(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fileID, _ := insertAnalysisFile(t, db, "cc")
	db.EnqueueAnalysisJob(ctx, fileID, 1000)
	job, _ := db.ClaimAnalysisJob(ctx)

	if err := db.FinishAnalysisJob(ctx, job.ID, nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	var status string
	db.QueryRow(`SELECT status FROM media_analysis_jobs WHERE id=?`, job.ID).Scan(&status)
	if status != "done" {
		t.Errorf("status = %q, want done", status)
	}
}

func TestFinishAnalysisJob_RetryThenFail(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fileID, _ := insertAnalysisFile(t, db, "dd")
	db.EnqueueAnalysisJob(ctx, fileID, 1000)

	jobErr := errors.New("boom")
	// First two failures requeue (back to pending); the third trips the retry
	// ceiling and the job is marked failed.
	for i := range maxAnalysisJobRetries {
		job, err := db.ClaimAnalysisJob(ctx)
		if err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		if job == nil {
			t.Fatalf("claim %d returned nil; expected requeue", i)
		}
		if err := db.FinishAnalysisJob(ctx, job.ID, jobErr); err != nil {
			t.Fatalf("finish %d: %v", i, err)
		}
	}
	var status string
	var retry int
	db.QueryRow(`SELECT status, retry_count FROM media_analysis_jobs WHERE file_id=?`, fileID).Scan(&status, &retry)
	if status != "failed" || retry != maxAnalysisJobRetries {
		t.Errorf("status=%q retry=%d, want failed / %d", status, retry, maxAnalysisJobRetries)
	}
}

func TestResetStaleAnalysisJobs(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fileID, _ := insertAnalysisFile(t, db, "ee")
	db.EnqueueAnalysisJob(ctx, fileID, 1000)
	if _, err := db.ClaimAnalysisJob(ctx); err != nil { // -> running
		t.Fatalf("claim: %v", err)
	}
	if err := db.ResetStaleAnalysisJobs(ctx); err != nil {
		t.Fatalf("reset: %v", err)
	}
	// After reset the running job is pending again and can be re-claimed.
	job, err := db.ClaimAnalysisJob(ctx)
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if job == nil {
		t.Error("re-claim returned nil; reset should have requeued the running job")
	}
}

func TestUpsertTechColumns(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fileID, _ := insertAnalysisFile(t, db, "ff")

	ti := media.TechInfo{
		DurationSeconds: 212.48,
		Bitrate:         889000,
		SampleRate:      44100,
		Channels:        2,
		BitDepth:        16,
		Codec:           "flac",
	}
	if err := db.UpsertTechColumns(ctx, fileID, ti); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var (
		dur        float64
		br, sr, ch int
		bd         int
		codec      string
	)
	if err := db.QueryRow(
		`SELECT duration_seconds, bitrate, sample_rate, channels, bit_depth, codec
		   FROM media_metadata WHERE file_id=?`, fileID,
	).Scan(&dur, &br, &sr, &ch, &bd, &codec); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if dur != 212.48 || br != 889000 || sr != 44100 || ch != 2 || bd != 16 || codec != "flac" {
		t.Errorf("tech columns = dur=%v br=%d sr=%d ch=%d bd=%d codec=%s", dur, br, sr, ch, bd, codec)
	}
}

func TestUpsertTechColumns_ZeroIsNull(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fileID, _ := insertAnalysisFile(t, db, "a1")

	// Lossy file: ffprobe reported no bit_depth (0) — it must persist as NULL,
	// not 0, so the quality ladder can tell "unknown" from a real value.
	ti := media.TechInfo{Bitrate: 320000, SampleRate: 44100, Channels: 2, Codec: "mp3"}
	if err := db.UpsertTechColumns(ctx, fileID, ti); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var bd any
	db.QueryRow(`SELECT bit_depth FROM media_metadata WHERE file_id=?`, fileID).Scan(&bd)
	if bd != nil {
		t.Errorf("bit_depth = %v, want NULL", bd)
	}
}

func TestInsertAudioFingerprint_RoundTrip(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fileID, _ := insertAnalysisFile(t, db, "a2")

	fp := media.Fingerprint{
		Algo:        "chromaprint",
		AlgoVersion: "1.5.1",
		Duration:    180.5,
		Raw:         []uint32{1, 2, 4294967295, 7},
	}
	if err := db.InsertAudioFingerprint(ctx, fileID, fp, 2000); err != nil {
		t.Fatalf("insert fingerprint: %v", err)
	}
	var (
		algo, ver string
		dur       float64
		blob      []byte
	)
	if err := db.QueryRow(
		`SELECT algo, algo_version, duration, fingerprint FROM audio_fingerprints WHERE file_id=?`, fileID,
	).Scan(&algo, &ver, &dur, &blob); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if algo != "chromaprint" || ver != "1.5.1" || dur != 180.5 {
		t.Errorf("got algo=%s ver=%s dur=%v", algo, ver, dur)
	}
	if got := media.DecodeFingerprint(blob); !reflect.DeepEqual(got, fp.Raw) {
		t.Errorf("decoded raw = %v, want %v", got, fp.Raw)
	}
}

func TestFilesNeedingAnalysis(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// File 1: nothing yet → needs analysis.
	id1, _ := insertAnalysisFile(t, db, "b1")
	// File 2: fully analyzed (tech + fingerprint) → excluded.
	id2, _ := insertAnalysisFile(t, db, "b2")
	db.UpsertTechColumns(ctx, id2, media.TechInfo{Codec: "flac"})
	db.InsertAudioFingerprint(ctx, id2, media.Fingerprint{Algo: "chromaprint", Raw: []uint32{1}}, 2000)
	// File 3: trashed → excluded even though unanalyzed.
	id3, hash3 := insertAnalysisFile(t, db, "b3")
	trashAppearancesByHash(t, db, hash3)

	ids, err := db.FilesNeedingAnalysis(ctx)
	if err != nil {
		t.Fatalf("FilesNeedingAnalysis: %v", err)
	}
	if !reflect.DeepEqual(ids, []int64{id1}) {
		t.Errorf("needing analysis = %v, want [%d] (excludes analyzed %d and trashed %d)", ids, id1, id2, id3)
	}
}
