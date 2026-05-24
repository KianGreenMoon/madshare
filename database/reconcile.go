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
