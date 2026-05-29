package database

import (
	"context"
	"fmt"
	"log"
)

// PruneFailure records a hash whose prune delete failed, with the error text.
type PruneFailure struct {
	Hash string
	Err  string
}

// PruneResult reports the outcome of a PruneDangling pass.
//
// Scanned is the number of files rows examined. Dangling lists every file row
// whose backing blob directory is missing. On a confirmed run Pruned lists the
// rows that were deleted and Failed lists the rows whose delete errored.
type PruneResult struct {
	Scanned  int
	Dangling []FileRef
	Pruned   []FileRef
	Failed   []PruneFailure
}

// blobProbe is the slice of a storage backend PruneDangling needs: a way to
// ask whether a blob directory exists. Declared here so the database package
// does not import api/storage (the dependency arrow runs api → database and
// api → storage, never database → storage).
type blobProbe interface {
	HashDirExists(hash string) (bool, error)
}

// PruneDangling finds files rows whose backing blob directory no longer exists
// — the inverse of ReconcileOrphans, which removes blobs with no row. On a dry
// run (confirm=false) it only reports the dangling rows. With confirm=true it
// also deletes each dangling row via DeleteFileByHash, recording successes in
// Pruned and per-row failures in Failed (it continues past failures so one bad
// row does not abort the pass; a re-run is idempotent).
func PruneDangling(ctx context.Context, repo Repository, probe blobProbe, confirm bool) (*PruneResult, error) {
	refs, err := repo.ListFileRefs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list file refs: %w", err)
	}

	result := &PruneResult{Scanned: len(refs)}
	for _, ref := range refs {
		exists, err := probe.HashDirExists(ref.Hash)
		if err != nil {
			return nil, fmt.Errorf("probe blob %s: %w", ref.Hash, err)
		}
		if exists {
			continue
		}
		result.Dangling = append(result.Dangling, ref)

		if !confirm {
			continue
		}
		if _, _, err := repo.DeleteFileByHash(ctx, ref.Hash); err != nil {
			log.Printf("prune dangling: hash=%s err=%v", ref.Hash, err)
			result.Failed = append(result.Failed, PruneFailure{Hash: ref.Hash, Err: err.Error()})
			continue
		}
		result.Pruned = append(result.Pruned, ref)
	}
	return result, nil
}
