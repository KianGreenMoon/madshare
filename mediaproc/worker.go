// Package mediaproc runs the asynchronous ingest media-analysis pool. Workers
// claim jobs from the media_analysis_jobs queue, locate the audio blob on disk,
// and run the optional decode tools — ffprobe (tech columns) and fpcalc
// (acoustic fingerprint) — persisting whatever each tool produces. Both tools
// are optional: an absent tool is simply skipped, so the pool degrades
// gracefully (docs/architecture/recordings.md, Graceful degradation). Mirrors
// imageproc.Pool / the image_processing_jobs queue.
package mediaproc

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/media"
)

// pollInterval is how often an idle worker re-checks the queue when no Notify
// has arrived (a safety net against missed wakeups).
const pollInterval = 5 * time.Second

// Repository is the persistence subset the pool needs. *database.DB satisfies
// it. Kept narrow so it does not widen the api package's Repository (and its
// fakeRepo) — only EnqueueAnalysisJob, which callers use, lives there.
type Repository interface {
	ClaimAnalysisJob(ctx context.Context) (*database.AnalysisJob, error)
	FinishAnalysisJob(ctx context.Context, id int64, jobErr error) error
	UpsertTechColumns(ctx context.Context, fileID int64, ti media.TechInfo) error
	InsertAudioFingerprint(ctx context.Context, fileID int64, fp media.Fingerprint, now int64) error
	// ResolveRecording groups the file with its matching recording (or creates a
	// new one) once its fingerprint exists. Returns 0 when there is nothing to
	// resolve (no fingerprint / pinned file).
	ResolveRecording(ctx context.Context, fileID int64) (int64, error)
}

// BlobResolver maps a content hash to its on-disk blob path across every
// configured storage, in precedence order (local before links). *storages.Registry
// satisfies it. The worker must probe all storages — not just the local audio
// dir — because a links-backed file (an external symlink import) has no blob
// under <files_dir>/audio; its bytes are reached through the links storage.
type BlobResolver interface {
	Resolve(hash string) (path, storageID string, ok bool)
}

// Pool manages a fixed-size goroutine pool that drains media_analysis_jobs.
type Pool struct {
	repo        Repository
	resolver    BlobResolver
	workers     int
	haveFFprobe bool
	haveFpcalc  bool
	notify      chan struct{}
}

// NewPool creates (but does not start) a worker pool. resolver locates each
// job's blob across all storages (local + links), so linked imports are analysed
// just like uploads. haveFFprobe / haveFpcalc reflect tool availability (see
// media.ToolStatus); a false flag skips that tool for every job. workers < 1 is
// treated as 1.
func NewPool(repo Repository, resolver BlobResolver, workers int, haveFFprobe, haveFpcalc bool) *Pool {
	if workers < 1 {
		workers = 1
	}
	return &Pool{
		repo:        repo,
		resolver:    resolver,
		workers:     workers,
		haveFFprobe: haveFFprobe,
		haveFpcalc:  haveFpcalc,
		notify:      make(chan struct{}, 1),
	}
}

// Notify signals that a new job has been enqueued so an idle worker wakes
// immediately. Safe to call from any goroutine; coalesces (never blocks).
func (p *Pool) Notify() {
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

// Start launches the worker goroutines and blocks until ctx is cancelled.
// Call it in its own goroutine: go pool.Start(ctx). When neither tool is
// present there is nothing to do, so the pool returns immediately.
func (p *Pool) Start(ctx context.Context) {
	if !p.haveFFprobe && !p.haveFpcalc {
		return
	}
	var wg sync.WaitGroup
	for i := 0; i < p.workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.run(ctx)
		}()
	}
	wg.Wait()
}

func (p *Pool) run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		// Drain the queue until it is empty or ctx is cancelled.
		for {
			if ctx.Err() != nil {
				return
			}
			job, err := p.repo.ClaimAnalysisJob(ctx)
			if err != nil {
				log.Printf("mediaproc: claim job: %v", err)
				break
			}
			if job == nil {
				break // queue empty
			}
			// Wake a sibling so multiple pending jobs process in parallel.
			p.Notify()
			p.process(ctx, job)
		}
		select {
		case <-ctx.Done():
			return
		case <-p.notify:
		case <-ticker.C:
		}
	}
}

func (p *Pool) process(ctx context.Context, job *database.AnalysisJob) {
	err := p.analyze(ctx, job)
	if err != nil {
		log.Printf("mediaproc: job %d (file=%d): %v", job.ID, job.FileID, err)
	}
	if err := p.repo.FinishAnalysisJob(ctx, job.ID, err); err != nil {
		log.Printf("mediaproc: finish job %d: %v", job.ID, err)
	}
}

// analyze locates the blob and runs each available tool. The returned error is
// reserved for conditions a retry might fix (blob unreadable, DB write failure)
// so FinishAnalysisJob re-queues them; a per-file tool failure (a corrupt file
// ffprobe/fpcalc can't decode) is logged and skipped, never retried, because the
// next attempt would fail identically.
func (p *Pool) analyze(ctx context.Context, job *database.AnalysisJob) error {
	path, _, ok := p.resolver.Resolve(job.Hash)
	if !ok {
		// No storage has the blob (gone, or only a dangling link). Surface as a
		// retryable error so a transient miss recovers; a persistent one ages out
		// to 'failed' after maxAnalysisJobRetries.
		return fmt.Errorf("no blob for hash %s", job.Hash)
	}
	now := time.Now().Unix()

	if p.haveFFprobe {
		if ti, err := media.ProbeTech(ctx, path); err != nil {
			log.Printf("mediaproc: ffprobe file=%d: %v", job.FileID, err)
		} else if err := p.repo.UpsertTechColumns(ctx, job.FileID, *ti); err != nil {
			return err
		}
	}
	if p.haveFpcalc {
		if fp, err := media.ComputeFingerprint(ctx, path); err != nil {
			log.Printf("mediaproc: fpcalc file=%d: %v", job.FileID, err)
		} else if err := p.repo.InsertAudioFingerprint(ctx, job.FileID, *fp, now); err != nil {
			return err
		} else if _, err := p.repo.ResolveRecording(ctx, job.FileID); err != nil {
			// Fingerprint is stored; only the grouping failed. A DB error is
			// worth retrying — the startup backfill would also recover it.
			return err
		}
	}
	return nil
}
