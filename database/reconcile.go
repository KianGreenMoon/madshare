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

// baseKeyDirPattern matches the 16-char lowercase-hex base_key directory names
// under imagesDir. Album covers live in such a directory
// (<base_key>/original.<ext> + variants); artist covers are flat <base_key><ext>
// files (no variant pipeline), so a directory sweep never touches them.
var baseKeyDirPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// ReconcileImageOrphans removes album-cover variant directories under imagesDir
// (<base_key>/) that no album_images or artist_images row references and that
// have no active (pending/running) image-processing job. These accumulate when a
// cover is replaced with different bytes, or when the rare distinct-art loser of
// a concurrent same-album upload writes an original that no row ends up pointing
// at (see .issues/open-issues.md). Returns the number of directories removed.
//
// Startup-only, like ReconcileOrphans: it relies on no concurrent uploads or
// variant writes creating new references mid-sweep. Non-directory entries (artist
// flat files) and directories whose name isn't a base_key are skipped silently.
func (db *DB) ReconcileImageOrphans(ctx context.Context, imagesDir string) (int, error) {
	entries, err := os.ReadDir(imagesDir)
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
		baseKey := e.Name()
		if !baseKeyDirPattern.MatchString(baseKey) {
			continue
		}

		referenced, err := db.imageBaseKeyReferenced(ctx, baseKey)
		if err != nil {
			return removed, err
		}
		if referenced {
			continue
		}

		path := filepath.Join(imagesDir, baseKey)
		if err := os.RemoveAll(path); err != nil {
			return removed, fmt.Errorf("remove orphan image dir %s: %w", path, err)
		}
		log.Printf("reconciled orphan image dir: %s", baseKey)
		removed++
	}
	return removed, nil
}

// imageBaseKeyReferenced reports whether any cover row or active image job still
// needs the given base_key's on-disk directory. The active-job clause protects a
// dir whose variants are still being generated; the artist_images clause is
// belt-and-suspenders for an album/artist cover that happens to share base_key.
func (db *DB) imageBaseKeyReferenced(ctx context.Context, baseKey string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT
		   EXISTS(SELECT 1 FROM album_images  WHERE base_key = ?) OR
		   EXISTS(SELECT 1 FROM artist_images WHERE base_key = ?) OR
		   EXISTS(SELECT 1 FROM image_processing_jobs WHERE base_key = ? AND status IN ('pending','running'))`,
		baseKey, baseKey, baseKey,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("image base_key referenced: %w", err)
	}
	return n > 0, nil
}
