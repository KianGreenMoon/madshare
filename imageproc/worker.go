// Package imageproc runs the asynchronous cover-image variant generation pool.
// Workers claim jobs from the image_processing_jobs queue (see database.DB),
// read the stored original off disk, generate every variant via media.ProcessImage,
// and write them under <imagesDir>/<base_key>/.
package imageproc

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/media"
)

// pollInterval is how often an idle worker re-checks the queue when no Notify
// has arrived (a safety net against missed wakeups).
const pollInterval = 5 * time.Second

// Pool manages a fixed-size goroutine pool that drains image_processing_jobs.
type Pool struct {
	repo      database.Repository
	imagesDir string
	workers   int
	notify    chan struct{}
}

// NewPool creates (but does not start) a worker pool. imagesDir is the absolute
// path to the images directory. workers < 1 is treated as 1.
func NewPool(repo database.Repository, imagesDir string, workers int) *Pool {
	if workers < 1 {
		workers = 1
	}
	return &Pool{
		repo:      repo,
		imagesDir: imagesDir,
		workers:   workers,
		notify:    make(chan struct{}, 1),
	}
}

// Notify signals that a new job has been enqueued so an idle worker wakes
// immediately rather than waiting for its next poll. Safe to call from any
// goroutine; coalesces (never blocks).
func (p *Pool) Notify() {
	select {
	case p.notify <- struct{}{}:
	default:
	}
}

// Start launches the worker goroutines and blocks until ctx is cancelled.
// Call it in its own goroutine: go pool.Start(ctx).
func (p *Pool) Start(ctx context.Context) {
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
			job, err := p.repo.ClaimImageJob(ctx)
			if err != nil {
				log.Printf("imageproc: claim job: %v", err)
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

func (p *Pool) process(ctx context.Context, job *database.ImageJob) {
	genErr := p.generate(job)
	if genErr != nil {
		log.Printf("imageproc: job %d (base_key=%s): %v", job.ID, job.BaseKey, genErr)
	}
	if err := p.repo.FinishImageJob(ctx, job.ID, genErr); err != nil {
		log.Printf("imageproc: finish job %d: %v", job.ID, err)
	}
}

// generate reads the original for the job's base_key, produces all variants,
// and writes them to disk. It returns any error so the caller can hand it to
// FinishImageJob, which owns the retry/fail decision.
func (p *Pool) generate(job *database.ImageJob) error {
	origPath, mimeType, err := p.resolveOriginal(job.BaseKey)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(origPath)
	if err != nil {
		return fmt.Errorf("read original: %w", err)
	}
	set, ext, err := media.ProcessImage(data, mimeType)
	if err != nil {
		return err
	}
	dir := filepath.Join(p.imagesDir, job.BaseKey)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	for _, name := range media.AllVariants {
		if name == media.VariantOriginal {
			continue // already on disk; never rewrite the source
		}
		rel := media.VariantPath(job.BaseKey, name, ext)
		dest := filepath.Join(p.imagesDir, filepath.FromSlash(rel))
		if err := os.WriteFile(dest, set[name], 0o644); err != nil {
			return fmt.Errorf("write variant %s: %w", name, err)
		}
	}
	return nil
}

// resolveOriginal finds the stored original for a base_key by probing the two
// accepted extensions, returning its path and MIME type. This keys off the
// base_key directory directly, so it processes exactly the claimed image even
// if the album's cover was since replaced with different bytes.
func (p *Pool) resolveOriginal(baseKey string) (path, mimeType string, err error) {
	for _, c := range []struct{ ext, mime string }{
		{".jpg", "image/jpeg"},
		{".png", "image/png"},
	} {
		candidate := filepath.Join(p.imagesDir, baseKey, media.VariantOriginal+c.ext)
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, c.mime, nil
		}
	}
	return "", "", fmt.Errorf("no original image found for base_key %s", baseKey)
}
