package mediaproc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/media"
	"daemonlord.ygg/madshare/storages"
)

// 64-char lowercase-hex hashes, the only shape storages.Registry.Locate accepts.
const (
	localHash   = "1111111111111111111111111111111111111111111111111111111111111111"
	linkedHash  = "2222222222222222222222222222222222222222222222222222222222222222"
	missingHash = "3333333333333333333333333333333333333333333333333333333333333333"
)

// stubTools stands in for the analysis passes so these tests say what they mean
// on any machine. They used to declare both tools present and rely on ffprobe
// and fpcalc failing on the garbage bytes written below — true, but true by
// accident, and silently a different test on a host with neither installed.
//
// Every test here is about the POOL's behaviour around a failing pass, so the
// stub fails both. Availability is separate, because "no tool at all" and "a
// tool that errors on this file" are the two cases the pool treats differently.
type stubTools struct{ probe, fingerprint bool }

func (s stubTools) Available() (bool, bool) { return s.probe, s.fingerprint }

func (stubTools) ProbeTech(context.Context, string) (*media.TechInfo, error) {
	return nil, errNotAudio
}

func (stubTools) ComputeFingerprint(context.Context, string) (*media.Fingerprint, error) {
	return nil, errNotAudio
}

var errNotAudio = errors.New("stub: not audio")

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

// writeBlob writes a regular blob file under <root>/audio/<hash>/<name>, the
// layout storages.diskStorage probes. root is a storage root (files_dir or the
// links dir), not the audio dir itself.
func writeBlob(t *testing.T, root, hash, name string, data []byte) {
	t.Helper()
	dir := filepath.Join(root, storage.AudioSubdir, hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
}

// linkBlob places a links-storage symlink at <linksRoot>/audio/<hash>/<name>
// pointing at an external original, the way a symlink data source imports a file.
func linkBlob(t *testing.T, linksRoot, hash, name, target string) {
	t.Helper()
	dir := filepath.Join(linksRoot, storage.AudioSubdir, hash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
		t.Fatalf("symlink blob: %v", err)
	}
}

func TestPool_NeitherToolShortCircuits(t *testing.T) {
	repo := &fakeRepo{finished: make(chan struct{})}
	reg := storages.New(t.TempDir(), t.TempDir())
	pool := NewPool(repo, reg, 1, stubTools{})
	// With no tools, Start must return immediately without touching the queue.
	pool.Start(context.Background())
	if repo.claimCalls != 0 {
		t.Errorf("claimCalls = %d, want 0 (pool should short-circuit)", repo.claimCalls)
	}
}

func TestPool_FinishesJobWhenToolFails(t *testing.T) {
	filesRoot := t.TempDir()
	// The blob exists but is not audio, so both passes fail (see stubTools).
	// The worker logs and skips them, and the job still finishes cleanly (nil
	// error), proving graceful degradation rather than a retry loop.
	writeBlob(t, filesRoot, localHash, "junk.mp3", []byte("not audio"))
	reg := storages.New(filesRoot, t.TempDir())
	repo := &fakeRepo{
		jobs:     []*database.AnalysisJob{{ID: 1, FileID: 7, Hash: localHash}},
		finished: make(chan struct{}),
	}
	pool := NewPool(repo, reg, 1, stubTools{probe: true, fingerprint: true})

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

// TestPool_ResolvesLinkedBlob is the regression test for the links-storage bug:
// a file whose blob lives only in the links storage (an external symlink import,
// not under files_dir/audio) must still be located. The job finishes with a nil
// error — the passes fail on the garbage target, but the blob WAS found.
// Before the fix the worker only probed the local audio dir and returned a
// "blob dir missing for hash" error here, re-queuing the job on every restart.
func TestPool_ResolvesLinkedBlob(t *testing.T) {
	filesRoot := t.TempDir() // local storage: deliberately has no blob
	linksRoot := t.TempDir()
	external := filepath.Join(t.TempDir(), "external-original.mp3")
	if err := os.WriteFile(external, []byte("not audio"), 0o644); err != nil {
		t.Fatalf("write external original: %v", err)
	}
	linkBlob(t, linksRoot, linkedHash, "song.mp3", external)
	reg := storages.New(filesRoot, linksRoot)
	repo := &fakeRepo{
		jobs:     []*database.AnalysisJob{{ID: 1, FileID: 7, Hash: linkedHash}},
		finished: make(chan struct{}),
	}
	pool := NewPool(repo, reg, 1, stubTools{probe: true, fingerprint: true})

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
		t.Errorf("finished with err %v, want nil (linked blob must resolve, not error)", repo.finishedErr)
	}
}

func TestPool_MissingBlobIsRetryableError(t *testing.T) {
	reg := storages.New(t.TempDir(), t.TempDir())
	repo := &fakeRepo{
		jobs:     []*database.AnalysisJob{{ID: 1, FileID: 7, Hash: missingHash}},
		finished: make(chan struct{}),
	}
	pool := NewPool(repo, reg, 1, stubTools{probe: true, fingerprint: true})

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
