package database

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The cover-variant byte index (migration 043). The variants directory stays
// authoritative and this table only describes it, exactly as madnetwork_cache
// describes the download cache: the imageproc pool writes the variant files and
// the orphan sweep removes them, and neither is made transactional with a byte
// counter. A stale row is a reconciliation problem, never a phantom — nothing
// reads these rows to decide whether an image exists, only to total its bytes.

// SetImageVariantBytes records the total byte size of one image_hash's variant
// directory. Called by the imageproc pool after it writes a variant set, and by
// the startup reconcile. Upsert: regenerating a cover replaces the figure rather
// than adding to it.
func (db *DB) SetImageVariantBytes(ctx context.Context, imageHash string, bytes int64) error {
	if imageHash == "" {
		return nil
	}
	if bytes < 0 {
		bytes = 0
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO image_variants (image_hash, bytes, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(image_hash) DO UPDATE SET bytes = excluded.bytes, updated_at = excluded.updated_at`,
		imageHash, bytes, time.Now().Unix(),
	); err != nil {
		return fmt.Errorf("set image variant bytes %s: %w", imageHash, err)
	}
	return nil
}

// ImageVariantBytes totals the indexed cover-variant bytes — the storage panel's
// "images" category, and the reason this table exists: it replaces a full DirSize
// walk that ran inline on every dashboard load.
func (db *DB) ImageVariantBytes(ctx context.Context) (int64, error) {
	var bytes int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(bytes), 0) FROM image_variants`).Scan(&bytes); err != nil {
		return 0, fmt.Errorf("image variant bytes: %w", err)
	}
	return bytes, nil
}

// ReconcileImageVariants re-walks the variants tree and makes the index match
// it: every <image_hash>/ directory is re-totalled and any row whose directory
// is gone is dropped. This is the one place the expensive walk is still paid —
// once per process at startup, instead of once per dashboard load — and it is
// what keeps "the directory is authoritative" true after the pool crashed
// mid-write, the tree was edited by hand, or the orphan sweep removed a cover.
//
// Returns the number of indexed directories. A missing tree is not an error: a
// fresh install has no covers yet.
func (db *DB) ReconcileImageVariants(ctx context.Context, variantsImagesDir string) (int, error) {
	entries, err := os.ReadDir(variantsImagesDir)
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing on disk: drop any rows describing a tree that is not there.
			if _, derr := db.ExecContext(ctx, `DELETE FROM image_variants`); derr != nil {
				return 0, fmt.Errorf("reconcile image variants: clear: %w", derr)
			}
			return 0, nil
		}
		return 0, fmt.Errorf("reconcile image variants: read %q: %w", variantsImagesDir, err)
	}

	seen := make(map[string]struct{}, len(entries))
	indexed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue // artist covers are flat files, not variant directories
		}
		hash := e.Name()
		bytes, err := dirBytes(filepath.Join(variantsImagesDir, hash))
		if err != nil {
			return indexed, fmt.Errorf("reconcile image variants: size %s: %w", hash, err)
		}
		if err := db.SetImageVariantBytes(ctx, hash, bytes); err != nil {
			return indexed, err
		}
		seen[hash] = struct{}{}
		indexed++
	}

	// Drop rows whose directory no longer exists. Done by reading the keys back
	// rather than with a NOT IN over `seen`, which would build a bind list the
	// size of the library.
	rows, err := db.QueryContext(ctx, `SELECT image_hash FROM image_variants`)
	if err != nil {
		return indexed, fmt.Errorf("reconcile image variants: list: %w", err)
	}
	var stale []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return indexed, fmt.Errorf("reconcile image variants: scan: %w", err)
		}
		if _, ok := seen[h]; !ok {
			stale = append(stale, h)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return indexed, fmt.Errorf("reconcile image variants: rows: %w", err)
	}
	for _, h := range stale {
		if _, err := db.ExecContext(ctx, `DELETE FROM image_variants WHERE image_hash = ?`, h); err != nil {
			return indexed, fmt.Errorf("reconcile image variants: delete %s: %w", h, err)
		}
	}
	return indexed, nil
}

// dirBytes totals the regular files directly inside one variant directory.
func dirBytes(dir string) (int64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	var total int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			if os.IsNotExist(err) {
				continue // removed under us; the next reconcile settles it
			}
			return total, err
		}
		total += info.Size()
	}
	return total, nil
}
