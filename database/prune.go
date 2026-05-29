package database

import (
	"context"
	"fmt"
	"log"
)

// Prune reasons reported per dangling record.
const (
	// ReasonMissing: the backing blob file is gone (the hash directory is absent
	// or has been emptied). Detected by the cheap presence scan.
	ReasonMissing = "missing"
	// ReasonCorrupt: the blob exists but its content no longer hashes to the
	// recorded digest. Detected only by the deep (rehash) scan.
	ReasonCorrupt = "corrupt"
)

// PruneFailure records a hash whose prune delete failed, with the error text.
type PruneFailure struct {
	Hash string
	Err  string
}

// DanglingRef is a file row flagged for pruning, with why it was flagged.
type DanglingRef struct {
	FileRef
	Reason string // ReasonMissing | ReasonCorrupt
}

// PruneResult reports the outcome of a PruneDangling pass.
//
// Scanned is the number of files rows examined. Dangling lists every flagged
// row (missing blob, or — in a deep scan — a corrupted one). On a confirmed run
// Pruned lists the rows that were deleted and Failed lists the rows whose delete
// errored. Deep reports whether the integrity (rehash) scan was run.
type PruneResult struct {
	Scanned  int
	Deep     bool
	Dangling []DanglingRef
	Pruned   []DanglingRef
	Failed   []PruneFailure
}

// blobProbe is the slice of a storage backend PruneDangling needs. Declared
// here so the database package does not import api/storage (the dependency arrow
// runs api → database and api → storage, never database → storage).
type blobProbe interface {
	// BlobPresent reports whether a backing blob file exists for the hash.
	BlobPresent(hash string) (bool, error)
	// VerifyBlob reports whether the stored blob still hashes to hash (deep).
	VerifyBlob(hash string) (bool, error)
	// DeleteAll removes the hash directory and its contents (idempotent).
	DeleteAll(hash string) (bool, error)
}

// PruneDangling finds files rows whose backing blob is missing — the inverse of
// ReconcileOrphans, which removes blobs with no row. The cheap scan flags rows
// whose blob file is gone (ReasonMissing); when deep is true it additionally
// rehashes every present blob and flags content that no longer matches its
// digest (ReasonCorrupt), so a library can heal from bit-rot.
//
// On a dry run (confirm=false) it only reports the flagged rows. With
// confirm=true it deletes each flagged row via DeleteFileByHash and then sweeps
// any leftover blob bytes via DeleteAll (best-effort), recording successes in
// Pruned and per-row failures in Failed. It continues past failures so one bad
// row does not abort the pass; a re-run is idempotent.
func PruneDangling(ctx context.Context, repo Repository, probe blobProbe, confirm, deep bool) (*PruneResult, error) {
	refs, err := repo.ListFileRefs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list file refs: %w", err)
	}

	result := &PruneResult{Scanned: len(refs), Deep: deep}
	for _, ref := range refs {
		present, err := probe.BlobPresent(ref.Hash)
		if err != nil {
			return nil, fmt.Errorf("probe blob %s: %w", ref.Hash, err)
		}

		reason := ""
		switch {
		case !present:
			reason = ReasonMissing
		case deep:
			intact, err := probe.VerifyBlob(ref.Hash)
			if err != nil {
				return nil, fmt.Errorf("verify blob %s: %w", ref.Hash, err)
			}
			if !intact {
				reason = ReasonCorrupt
			}
		}
		if reason == "" {
			continue
		}

		d := DanglingRef{FileRef: ref, Reason: reason}
		result.Dangling = append(result.Dangling, d)
		if !confirm {
			continue
		}
		if _, _, err := repo.DeleteFileByHash(ctx, ref.Hash); err != nil {
			log.Printf("prune dangling: hash=%s err=%v", ref.Hash, err)
			result.Failed = append(result.Failed, PruneFailure{Hash: ref.Hash, Err: err.Error()})
			continue
		}
		// Sweep any leftover bytes (an emptied dir, or a corrupted blob) so the
		// hash directory does not linger as an orphan. Best-effort: a failure
		// here is logged but the row is already gone, so the prune still counts.
		if _, err := probe.DeleteAll(ref.Hash); err != nil {
			log.Printf("prune sweep: hash=%s err=%v", ref.Hash, err)
		}
		result.Pruned = append(result.Pruned, d)
	}
	return result, nil
}
