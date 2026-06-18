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

// danglingReason classifies one file ref against the blob store, returning a
// prune reason ("" = healthy). The cheap check flags a missing blob; the deep
// check additionally rehashes a present blob and flags content that no longer
// matches its digest (bit-rot).
func danglingReason(probe blobProbe, hash string, deep bool) (string, error) {
	present, err := probe.BlobPresent(hash)
	if err != nil {
		return "", fmt.Errorf("probe blob %s: %w", hash, err)
	}
	if !present {
		return ReasonMissing, nil
	}
	if deep {
		intact, err := probe.VerifyBlob(hash)
		if err != nil {
			return "", fmt.Errorf("verify blob %s: %w", hash, err)
		}
		if !intact {
			return ReasonCorrupt, nil
		}
	}
	return "", nil
}

// ScanDangling is the full sweep: it walks every files row and flags those whose
// backing blob is missing (ReasonMissing) — the inverse of ReconcileOrphans,
// which removes blobs with no row — and, when deep is true, those whose present
// blob no longer hashes to its digest (ReasonCorrupt), so a library can heal from
// bit-rot. It flags only; it never deletes (that is PruneRefs).
//
// onProgress, when non-nil, is called once per row with (scanned, total) so a
// caller can report live progress on the slow deep path. ctx cancellation is
// honoured between rows: a cancelled scan returns the partial result so far plus
// ctx.Err().
func ScanDangling(ctx context.Context, repo Repository, probe blobProbe, deep bool, onProgress func(scanned, total int)) (*PruneResult, error) {
	refs, err := repo.ListFileRefs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list file refs: %w", err)
	}

	result := &PruneResult{Scanned: len(refs), Deep: deep}
	for i, ref := range refs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		reason, err := danglingReason(probe, ref.Hash, deep)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			result.Dangling = append(result.Dangling, DanglingRef{FileRef: ref, Reason: reason})
		}
		if onProgress != nil {
			onProgress(i+1, len(refs))
		}
	}
	return result, nil
}

// PruneRefs deletes exactly the given refs — the set a prior ScanDangling found
// and the operator reviewed — re-verifying each is *still* dangling immediately
// before deleting (cheap, since it touches only the flagged hashes, not the whole
// library). A ref that has since healed is skipped (neither pruned nor failed),
// so PruneRefs never deletes more than what was confirmed. Each delete removes
// the files row via HardDeleteFileByHash and then sweeps any leftover blob bytes
// via DeleteAll (best-effort); successes land in Pruned, per-row failures in
// Failed. It continues past failures so one bad row does not abort the pass, and
// a re-run is idempotent. deep must match the scan that produced refs so the
// corrupt re-check uses the same criterion. ctx cancellation is honoured between
// refs.
//
// The returned result carries Dangling = refs (the reviewed set); Scanned is left
// to the caller to fill from the originating scan.
func PruneRefs(ctx context.Context, repo Repository, probe blobProbe, deep bool, refs []DanglingRef, onProgress func(done, total int)) (*PruneResult, error) {
	result := &PruneResult{Deep: deep, Dangling: refs}
	for i, ref := range refs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		// Re-check this hash only. If it healed in the gap since the scan, leave
		// it alone — deleting it would remove a now-valid row the operator never
		// meant to prune.
		reason, err := danglingReason(probe, ref.Hash, deep)
		if err != nil {
			return nil, err
		}
		if onProgress != nil {
			onProgress(i+1, len(refs))
		}
		if reason == "" {
			continue
		}
		d := DanglingRef{FileRef: ref.FileRef, Reason: reason}
		if _, _, err := repo.HardDeleteFileByHash(ctx, ref.Hash); err != nil {
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

// PruneDangling is the combined scan-then-prune convenience: a dry run
// (confirm=false) returns the scan result; with confirm=true it prunes exactly
// what the scan flagged (via PruneRefs). The async server path drives ScanDangling
// and PruneRefs separately through the prune manager; this wrapper is retained for
// direct callers and tests.
func PruneDangling(ctx context.Context, repo Repository, probe blobProbe, confirm, deep bool) (*PruneResult, error) {
	scan, err := ScanDangling(ctx, repo, probe, deep, nil)
	if err != nil {
		return nil, err
	}
	if !confirm {
		return scan, nil
	}
	pruned, err := PruneRefs(ctx, repo, probe, deep, scan.Dangling, nil)
	if err != nil {
		return nil, err
	}
	pruned.Scanned = scan.Scanned
	return pruned, nil
}
