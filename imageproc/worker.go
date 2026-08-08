// Package imageproc runs the asynchronous cover-image variant generation pool.
// Workers claim jobs from the image_processing_jobs queue (see database.DB), read
// the stored source original off disk (sourceImagesDir/<image_hash>/original<ext>,
// under files_dir — never served), generate every derived variant via
// media.ProcessImage, and write them under
// variantsImagesDir/<image_hash>/<recipe><ext> (under variants_dir — served at
// /images). See docs/architecture/variants.md.
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
	repo database.Repository
	// sourceImagesDir is where source originals are read from
	// (<files_dir>/images/<image_hash>/original<ext>); variantsImagesDir is where
	// derived variants are written (<variants_dir>/images/<image_hash>/<recipe><ext>).
	sourceImagesDir   string
	variantsImagesDir string
	workers           int
	notify            chan struct{}
}

// NewPool creates (but does not start) a worker pool. sourceImagesDir and
// variantsImagesDir are absolute paths (see Pool). workers < 1 is treated as 1.
func NewPool(repo database.Repository, sourceImagesDir, variantsImagesDir string, workers int) *Pool {
	if workers < 1 {
		workers = 1
	}
	return &Pool{
		repo:              repo,
		sourceImagesDir:   sourceImagesDir,
		variantsImagesDir: variantsImagesDir,
		workers:           workers,
		notify:            make(chan struct{}, 1),
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
		log.Printf("imageproc: job %d (image_hash=%s): %v", job.ID, job.ImageHash, genErr)
	} else if err := p.indexBytes(ctx, job.ImageHash); err != nil {
		// Best-effort: the variants are on disk and serving, and the startup
		// reconcile re-walks the tree, so a missed byte figure is a stale total
		// until the next restart rather than a broken cover.
		log.Printf("imageproc: index variant bytes for %s: %v", job.ImageHash, err)
	}
	if err := p.repo.FinishImageJob(ctx, job.ID, genErr); err != nil {
		log.Printf("imageproc: finish job %d: %v", job.ID, err)
	}
}

// indexBytes totals the variant directory this job just wrote and records it in
// the cover-variant byte index (migration 043), so the storage panel can sum
// images instead of walking them. Called only after a successful generate: a
// failed attempt cleans up its own partial output and leaves no set to size.
func (p *Pool) indexBytes(ctx context.Context, imageHash string) error {
	dir := filepath.Join(p.variantsImagesDir, imageHash)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		total += info.Size()
	}
	return p.repo.SetImageVariantBytes(ctx, imageHash, total)
}

// generate reads the source original for the job's image_hash, produces every
// derived variant, and writes them under variantsImagesDir/<image_hash>/. It
// returns any error so the caller can hand it to FinishImageJob, which owns the
// retry/fail decision.
func (p *Pool) generate(job *database.ImageJob) error {
	origPath, mimeType, err := p.resolveOriginal(job.ImageHash)
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
	dir := filepath.Join(p.variantsImagesDir, job.ImageHash)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	// Track what this attempt wrote so a mid-loop failure (e.g. disk full) does
	// not leave a partial set of variants behind. The source original lives under
	// sourceImagesDir and is never touched here — retries re-read it via
	// resolveOriginal. Only derived variants are written (never the original).
	var written []string
	for _, name := range media.DerivedVariants {
		rel := media.VariantPath(job.ImageHash, name, ext)
		dest := filepath.Join(p.variantsImagesDir, filepath.FromSlash(rel))
		if err := os.WriteFile(dest, set[name], 0o644); err != nil {
			for _, w := range written {
				os.Remove(w) // best-effort cleanup of this attempt's partial output
			}
			return fmt.Errorf("write variant %s: %w", name, err)
		}
		written = append(written, dest)
	}
	return nil
}

// resolveOriginal finds the stored source original for an image_hash by probing
// the two accepted extensions under sourceImagesDir, returning its path and MIME
// type. This keys off the image_hash directory directly, so it processes exactly
// the claimed image even if the album's cover was since replaced with other bytes.
func (p *Pool) resolveOriginal(imageHash string) (path, mimeType string, err error) {
	for _, c := range []struct{ ext, mime string }{
		{".jpg", "image/jpeg"},
		{".png", "image/png"},
	} {
		candidate := filepath.Join(p.sourceImagesDir, imageHash, media.VariantOriginal+c.ext)
		if _, statErr := os.Stat(candidate); statErr == nil {
			return candidate, c.mime, nil
		}
	}
	return "", "", fmt.Errorf("no source original found for image_hash %s", imageHash)
}
