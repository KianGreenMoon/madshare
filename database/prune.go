package database

import (
	"context"
	"fmt"
	"log"
)

// Prune reasons reported per dangling record. The first two apply to a local
// blob; the next two are links-storage broken-link states (data-sources P5). A
// links row whose symlink entry is entirely gone reports ReasonMissing (like a
// missing local blob), and a deep links integrity failure reports ReasonCorrupt
// (the target no longer hashes to the digest) — so the reason vocabulary stays
// small and the prune summary's Dangling count covers both storages.
const (
	// ReasonMissing: the backing blob is gone (local: the hash dir is absent or
	// emptied; links: the symlink entry is gone). Detected by the cheap scan.
	ReasonMissing = "missing"
	// ReasonCorrupt: the blob/target exists but no longer hashes to the recorded
	// digest. Detected only by the deep (rehash) scan.
	ReasonCorrupt = "corrupt"
	// ReasonDangling: a links symlink is present but its external target is gone.
	ReasonDangling = "dangling"
	// ReasonRetargeted: a links symlink resolves but points somewhere other than
	// the recorded link_target (the external file was swapped under us).
	ReasonRetargeted = "retargeted"
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
	// InvalidRecordings is how many fileless recordings the post-prune
	// invariant sweep garbage-collected (recording-tagsets P2). Set only on a
	// confirmed prune (PruneRefs); a healthy library reports 0.
	InvalidRecordings int
}

// blobProbe is the slice of a local storage backend the prune needs. Declared
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

// linkProbe is the slice of the links storage the prune needs to classify and
// reclaim a symlink import (data-sources P5). Same import-arrow reasoning as
// blobProbe; satisfied by *storages.Linker. It is never asked to touch a target.
// May be nil (no links storage wired), in which case links rows are skipped.
type linkProbe interface {
	// LinkInfo returns the symlink's recorded target, whether a link entry
	// exists, and whether its target currently stats as a regular file.
	LinkInfo(hash string) (target string, exists, targetPresent bool, err error)
	// VerifyLink reports whether the link target still hashes to hash (deep).
	VerifyLink(hash string) (bool, error)
	// Remove unlinks the symlink for hash, never following it to the target.
	Remove(hash string) error
}

// linkReason classifies one links-storage ref against the recorded target,
// returning a prune reason ("" = healthy). A links row with no symlink entry is
// missing; a present link whose target is gone is dangling; a link pointing
// somewhere other than its recorded target is retargeted; and (deep) a target
// that no longer hashes to the digest is corrupt. The external target is never
// modified. A nil probe cannot check, so every links ref reads as healthy.
func linkReason(probe linkProbe, hash, expectedTarget string, deep bool) (string, error) {
	if probe == nil {
		return "", nil
	}
	target, exists, present, err := probe.LinkInfo(hash)
	if err != nil {
		return "", fmt.Errorf("probe link %s: %w", hash, err)
	}
	if !exists {
		return ReasonMissing, nil
	}
	if !present {
		return ReasonDangling, nil
	}
	if expectedTarget != "" && target != expectedTarget {
		return ReasonRetargeted, nil
	}
	if deep {
		intact, err := probe.VerifyLink(hash)
		if err != nil {
			return "", fmt.Errorf("verify link %s: %w", hash, err)
		}
		if !intact {
			return ReasonCorrupt, nil
		}
	}
	return "", nil
}

// refReason classifies one file ref against the storage it lives in, dispatching
// on its storage_backend: a links symlink (linkReason) or a local blob
// (danglingReason). It is the single per-row entry point both ScanDangling and
// PruneRefs use so the local/links split is decided in exactly one place.
func refReason(blob blobProbe, link linkProbe, ref FileRef, deep bool) (string, error) {
	if ref.StorageBackend == StorageBackendLinks {
		return linkReason(link, ref.Hash, ref.LinkTarget, deep)
	}
	return danglingReason(blob, ref.Hash, deep)
}

// reclaimStorage removes a pruned ref's storage-specific bytes after its row is
// deleted: unlink the symlink for a links import (never the external target), or
// os.RemoveAll the hash dir for a local blob. Best-effort; the row is already
// gone, so a sweep failure only leaves reclaimable debris.
func reclaimStorage(blob blobProbe, link linkProbe, ref FileRef) error {
	if ref.StorageBackend == StorageBackendLinks {
		if link == nil {
			return nil
		}
		return link.Remove(ref.Hash)
	}
	_, err := blob.DeleteAll(ref.Hash)
	return err
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
func ScanDangling(ctx context.Context, repo Repository, probe blobProbe, link linkProbe, deep bool, onProgress func(scanned, total int)) (*PruneResult, error) {
	refs, err := repo.ListFileRefs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list file refs: %w", err)
	}

	result := &PruneResult{Scanned: len(refs), Deep: deep}
	for i, ref := range refs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		reason, err := refReason(probe, link, ref, deep)
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
func PruneRefs(ctx context.Context, repo Repository, probe blobProbe, link linkProbe, deep bool, refs []DanglingRef, onProgress func(done, total int)) (*PruneResult, error) {
	result := &PruneResult{Deep: deep, Dangling: refs}
	for i, ref := range refs {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		// Re-check this hash only. If it healed in the gap since the scan, leave
		// it alone — deleting it would remove a now-valid row the operator never
		// meant to prune.
		reason, err := refReason(probe, link, ref.FileRef, deep)
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
		// Reclaim the storage-specific bytes so the entry does not linger as an
		// orphan: unlink the symlink for a links import (the external target is
		// NEVER touched), or os.RemoveAll the hash dir for a local blob.
		// Best-effort: a failure here is logged but the row is already gone.
		if err := reclaimStorage(probe, link, ref.FileRef); err != nil {
			log.Printf("prune sweep: hash=%s err=%v", ref.Hash, err)
		}
		result.Pruned = append(result.Pruned, d)
	}

	// Standing invariant backstop (recording-tagsets P2): GC any recording left
	// with no files — the per-row cascade above already removes a recording when
	// it prunes that recording's last file, so this only catches violations a bug
	// or crash slipped through. Best-effort: a sweep failure must not fail the
	// prune whose deletes already committed.
	if removed, err := repo.SweepInvalidRecordings(ctx); err != nil {
		log.Printf("prune: invalid-recording sweep: %v", err)
	} else {
		result.InvalidRecordings = removed
	}
	return result, nil
}

// PruneDangling is the combined scan-then-prune convenience: a dry run
// (confirm=false) returns the scan result; with confirm=true it prunes exactly
// what the scan flagged (via PruneRefs). The async server path drives ScanDangling
// and PruneRefs separately through the prune manager; this wrapper is retained for
// direct callers and tests.
func PruneDangling(ctx context.Context, repo Repository, probe blobProbe, link linkProbe, confirm, deep bool) (*PruneResult, error) {
	scan, err := ScanDangling(ctx, repo, probe, link, deep, nil)
	if err != nil {
		return nil, err
	}
	if !confirm {
		return scan, nil
	}
	pruned, err := PruneRefs(ctx, repo, probe, link, deep, scan.Dangling, nil)
	if err != nil {
		return nil, err
	}
	pruned.Scanned = scan.Scanned
	return pruned, nil
}
