package mediaproc

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/media"
)

// fakeRepo implements mediaproc.Repository for the worker tests. It hands out a
// single queued job, then reports the queue empty, and records outcomes.
type fakeRepo struct {
	mu           sync.Mutex
	jobs         []*database.AnalysisJob
	claimCalls   int
	finishedErr  error
	finished     chan struct{}
	techCalls    int
	fpCalls      int
	resolveCalls int
}

func (f *fakeRepo) ClaimAnalysisJob(context.Context) (*database.AnalysisJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimCalls++
	if len(f.jobs) == 0 {
		return nil, nil
	}
	j := f.jobs[0]
	f.jobs = f.jobs[1:]
	return j, nil
}

func (f *fakeRepo) FinishAnalysisJob(_ context.Context, _ int64, jobErr error) error {
	f.mu.Lock()
	f.finishedErr = jobErr
	f.mu.Unlock()
	close(f.finished)
	return nil
}

func (f *fakeRepo) UpsertTechColumns(context.Context, int64, media.TechInfo) error {
	f.mu.Lock()
	f.techCalls++
	f.mu.Unlock()
	return nil
}

func (f *fakeRepo) InsertAudioFingerprint(context.Context, int64, media.Fingerprint, int64) error {
	f.mu.Lock()
	f.fpCalls++
	f.mu.Unlock()
	return nil
}

func (f *fakeRepo) ResolveRecording(context.Context, int64) (int64, error) {
	f.mu.Lock()
	f.resolveCalls++
	f.mu.Unlock()
	return 0, nil
}

func writeBlob(t *testing.T, audioDir, hash, name string, data []byte) {
	t.Helper()
	dir := filepath.Join(audioDir, hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
}

func TestResolveBlobPath_FindsRegularFile(t *testing.T) {
	audioDir := t.TempDir()
	writeBlob(t, audioDir, "deadbeef", "song.mp3", []byte("x"))
	got, err := resolveBlobPath(audioDir, "deadbeef")
	if err != nil {
		t.Fatalf("resolveBlobPath: %v", err)
	}
	if want := filepath.Join(audioDir, "deadbeef", "song.mp3"); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveBlobPath_MissingDir(t *testing.T) {
	if _, err := resolveBlobPath(t.TempDir(), "nope"); err == nil {
		t.Error("expected error for missing blob dir")
	}
}

func TestPool_NeitherToolShortCircuits(t *testing.T) {
	repo := &fakeRepo{finished: make(chan struct{})}
	pool := NewPool(repo, t.TempDir(), 1, false, false)
	// With no tools, Start must return immediately without touching the queue.
	pool.Start(context.Background())
	if repo.claimCalls != 0 {
		t.Errorf("claimCalls = %d, want 0 (pool should short-circuit)", repo.claimCalls)
	}
}

func TestPool_FinishesJobWhenToolFails(t *testing.T) {
	audioDir := t.TempDir()
	hash := "abcd1234"
	// Garbage bytes: whether or not ffprobe/fpcalc are installed, the tool fails
	// on this "file", which the worker logs and skips — the job still finishes
	// cleanly (nil error), proving graceful degradation rather than a retry loop.
	writeBlob(t, audioDir, hash, "junk.mp3", []byte("not audio"))
	repo := &fakeRepo{
		jobs:     []*database.AnalysisJob{{ID: 1, FileID: 7, Hash: hash}},
		finished: make(chan struct{}),
	}
	pool := NewPool(repo, audioDir, 1, true, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pool.Start(ctx)

	select {
	case <-repo.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("job never finished")
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.finishedErr != nil {
		t.Errorf("finished with err %v, want nil (tool failures are skipped, not retried)", repo.finishedErr)
	}
}

func TestPool_MissingBlobIsRetryableError(t *testing.T) {
	repo := &fakeRepo{
		jobs:     []*database.AnalysisJob{{ID: 1, FileID: 7, Hash: "missinghash"}},
		finished: make(chan struct{}),
	}
	pool := NewPool(repo, t.TempDir(), 1, true, true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pool.Start(ctx)

	select {
	case <-repo.finished:
	case <-time.After(5 * time.Second):
		t.Fatal("job never finished")
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if repo.finishedErr == nil {
		t.Error("missing blob should surface as a (retryable) job error, got nil")
	}
}
