package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
)

// legacyImageDirPattern matches the pre-split 16-char base_key directory names.
var legacyImageDirPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

// SplitImageSources is the data half of the cover source/derivative split
// (migration 022 does the schema half). For every album cover still keyed by the
// legacy 16-char base_key it:
//
//  1. reads the stored original from variantsImagesDir/<base_key>/original<ext>,
//  2. recomputes the FULL sha256 of those bytes (the base_key is only a prefix,
//     so the full hash cannot be recovered from the DB — the bytes are required),
//  3. writes the original out as the source seed at
//     filesImagesDir/<full_hash>/original<ext> (never served), and
//  4. re-keys the album_images row to the full hash with variants_ready=0, then
//     drops the old <base_key>/ directory wholesale.
//
// It deliberately does NOT relocate the old variant files: a recipe-keyed variant
// is cheap to regenerate, and resetting variants_ready=0 lets the existing
// startup recovery (RequeueStuckImageJobs + the worker pool) regenerate them into
// variantsImagesDir/<full_hash>/ — one code path instead of a fragile file move.
//
// Idempotent and crash-safe: the original is copied (not moved) before the row is
// re-keyed, so an interrupted run re-reads the same bytes and recomputes the same
// hash; a row already keyed by a 64-char hash is skipped. A final sweep removes
// any 16-char directory no album_images row references (a crash between the row
// update and the directory removal). Returns the number of covers re-keyed.
//
// Artist covers are untouched: they are flat <key><ext> files with no variant
// pipeline, so they have no <base_key>/ directory to split and keep serving their
// stored object_key from the images dir.
func (db *DB) SplitImageSources(ctx context.Context, filesImagesDir, variantsImagesDir string) (int, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT album_id, image_hash, COALESCE(source_ext, '')
		 FROM album_images
		 WHERE image_hash IS NOT NULL AND length(image_hash) = 16`)
	if err != nil {
		return 0, fmt.Errorf("split image sources: query: %w", err)
	}
	type legacy struct {
		albumID   int64
		baseKey   string
		sourceExt string
	}
	var todo []legacy
	for rows.Next() {
		var l legacy
		if err := rows.Scan(&l.albumID, &l.baseKey, &l.sourceExt); err != nil {
			rows.Close()
			return 0, fmt.Errorf("split image sources: scan: %w", err)
		}
		todo = append(todo, l)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("split image sources: rows: %w", err)
	}
	rows.Close()

	migrated := 0
	for _, l := range todo {
		if err := db.splitOneImageSource(ctx, l.albumID, l.baseKey, l.sourceExt, filesImagesDir, variantsImagesDir); err != nil {
			// A single unmigratable cover (e.g. its original is missing) must not
			// abort startup: log and leave the row as-is. Its variants won't serve
			// until re-uploaded, but the rest of the library is unaffected.
			log.Printf("split image source (base_key=%s): %v", l.baseKey, err)
			continue
		}
		migrated++
	}

	db.sweepLegacyImageDirs(ctx, variantsImagesDir)
	return migrated, nil
}

func (db *DB) splitOneImageSource(ctx context.Context, albumID int64, baseKey, sourceExt, filesImagesDir, variantsImagesDir string) error {
	oldDir := filepath.Join(variantsImagesDir, baseKey)
	origPath, ext, err := findLegacyOriginal(oldDir, sourceExt)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(origPath)
	if err != nil {
		return fmt.Errorf("read original: %w", err)
	}
	sum := sha256.Sum256(data)
	full := hex.EncodeToString(sum[:])

	// Write the source seed under <files_dir>/images (idempotent overwrite). Done
	// before the row update so a crash before the update re-reads from oldDir.
	newOrigDir := filepath.Join(filesImagesDir, full)
	if err := os.MkdirAll(newOrigDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", newOrigDir, err)
	}
	if err := os.WriteFile(filepath.Join(newOrigDir, "original"+ext), data, 0o644); err != nil {
		return fmt.Errorf("write original: %w", err)
	}

	// Re-key the row and force variant regeneration into variants/images/<full>/.
	objectKey := full + "/original" + ext
	if _, err := db.ExecContext(ctx,
		`UPDATE album_images SET image_hash=?, object_key=?, source_ext=?, variants_ready=0 WHERE album_id=?`,
		full, objectKey, ext, albumID,
	); err != nil {
		return fmt.Errorf("re-key album_images: %w", err)
	}

	// Drop the old <base_key>/ dir wholesale (legacy original + stale variants).
	if err := os.RemoveAll(oldDir); err != nil {
		return fmt.Errorf("remove old dir %s: %w", oldDir, err)
	}
	return nil
}

// findLegacyOriginal locates original.<ext> inside a pre-split <base_key>/ dir,
// trying the recorded source_ext first then the two accepted extensions.
func findLegacyOriginal(oldDir, sourceExt string) (path, ext string, err error) {
	tried := map[string]bool{}
	try := func(e string) (string, string, bool) {
		if e == "" || tried[e] {
			return "", "", false
		}
		tried[e] = true
		p := filepath.Join(oldDir, "original"+e)
		if _, statErr := os.Stat(p); statErr == nil {
			return p, e, true
		}
		return "", "", false
	}
	for _, e := range []string{sourceExt, ".jpg", ".png"} {
		if p, x, ok := try(e); ok {
			return p, x, nil
		}
	}
	return "", "", fmt.Errorf("no original image in %s", oldDir)
}

// sweepLegacyImageDirs removes any leftover 16-char base_key directory under
// variantsImagesDir that no album_images row references — crash debris from a run
// interrupted between the row re-key and the directory removal. A still-referenced
// 16-char dir (a row SplitImageSources could not migrate) is preserved. Artist
// flat files are non-directories and are never matched. Best-effort: errors are
// logged, never fatal.
func (db *DB) sweepLegacyImageDirs(ctx context.Context, variantsImagesDir string) {
	entries, err := os.ReadDir(variantsImagesDir)
	if err != nil {
		return // missing dir (fresh install) or unreadable: nothing to sweep
	}
	for _, e := range entries {
		if !e.IsDir() || !legacyImageDirPattern.MatchString(e.Name()) {
			continue
		}
		var referenced int
		if err := db.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM album_images WHERE image_hash = ?)`, e.Name(),
		).Scan(&referenced); err != nil || referenced != 0 {
			continue
		}
		path := filepath.Join(variantsImagesDir, e.Name())
		if err := os.RemoveAll(path); err != nil {
			log.Printf("split image source: remove stale dir %s: %v", path, err)
		}
	}
}
