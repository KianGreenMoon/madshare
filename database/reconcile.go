package database

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
)

// hashDirPattern matches lowercase SHA-256 hex digests — the directory naming
// convention used by api/storage.Local. Duplicated here to keep the database
// package free of an import cycle on the storage package.
var hashDirPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ReconcileOrphans walks baseDir and removes any hash-named directory that
// has no corresponding files row. This cleans up blobs left over from
// crashed uploads where storage.Put succeeded but the DB insert did not.
//
// Non-directory entries and directories whose name doesn't match the SHA-256
// pattern are skipped silently.
func ReconcileOrphans(ctx context.Context, repo Repository, baseDir string) error {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read base dir: %w", err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		hash := e.Name()
		if !hashDirPattern.MatchString(hash) {
			continue
		}

		f, err := repo.GetFileByHash(ctx, hash)
		if err != nil {
			return fmt.Errorf("lookup %s: %w", hash, err)
		}
		if f != nil {
			continue
		}

		path := filepath.Join(baseDir, hash)
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove orphan %s: %w", path, err)
		}
		log.Printf("reconciled orphan: %s", hash)
	}
	return nil
}

// imageHashDirPattern matches the full 64-char lowercase-hex image-hash
// directory names that key both image trees. Album cover variants live in such a
// directory under the variants tree (<image_hash>/<recipe>.<ext>) and the source
// original in the same-named directory under the files tree
// (<image_hash>/original<ext>). Artist covers are flat <key><ext> files (no
// variant pipeline), so a directory sweep never touches them.
var imageHashDirPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ReconcileImageOrphans removes album-cover image directories — variant dirs
// under variantsImagesDir AND the matching source-original dirs under
// sourceImagesDir — keyed by an <image_hash> that no album_images/artist_images
// row references and that has no active (pending/running) image-processing job.
// These accumulate when a cover is replaced with different bytes, or when the
// rare distinct-art loser of a concurrent same-album upload writes a cover that
// no row ends up pointing at (see .issues/open-issues.md). Sweeping BOTH trees
// matters since the split (variants.md): the source original and its variants now
// live in separate directory trees, so an orphan leaves debris in each. Returns
// the total number of directories removed.
//
// Startup-only, like ReconcileOrphans: it relies on no concurrent uploads or
// variant writes creating new references mid-sweep. Non-directory entries (artist
// flat files) and directories whose name isn't a full image hash are skipped
// silently — so it must run after db.SplitImageSources, which removes the
// pre-split 16-char directories this 64-char pattern intentionally ignores.
func (db *DB) ReconcileImageOrphans(ctx context.Context, variantsImagesDir, sourceImagesDir string) (int, error) {
	total := 0
	for _, dir := range []string{variantsImagesDir, sourceImagesDir} {
		n, err := db.reconcileImageDir(ctx, dir)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// reconcileImageDir sweeps one image tree (variants or source) for orphan
// <image_hash>/ directories; see ReconcileImageOrphans.
func (db *DB) reconcileImageDir(ctx context.Context, dir string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read images dir: %w", err)
	}

	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		imageHash := e.Name()
		if !imageHashDirPattern.MatchString(imageHash) {
			continue
		}

		referenced, err := db.imageHashReferenced(ctx, imageHash)
		if err != nil {
			return removed, err
		}
		if referenced {
			continue
		}

		path := filepath.Join(dir, imageHash)
		if err := os.RemoveAll(path); err != nil {
			return removed, fmt.Errorf("remove orphan image dir %s: %w", path, err)
		}
		log.Printf("reconciled orphan image dir: %s", path)
		removed++
	}
	return removed, nil
}

// imageHashReferenced reports whether any cover row or active image job still
// needs the given image_hash's on-disk variant directory. The active-job clause
// protects a dir whose variants are still being generated; the artist_images
// clause is belt-and-suspenders for an album/artist cover that happens to share
// an image_hash.
func (db *DB) imageHashReferenced(ctx context.Context, imageHash string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT
		   EXISTS(SELECT 1 FROM album_images  WHERE image_hash = ?) OR
		   EXISTS(SELECT 1 FROM artist_images WHERE image_hash = ?) OR
		   EXISTS(SELECT 1 FROM image_processing_jobs WHERE image_hash = ? AND status IN ('pending','running'))`,
		imageHash, imageHash, imageHash,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("image hash referenced: %w", err)
	}
	return n > 0, nil
}
