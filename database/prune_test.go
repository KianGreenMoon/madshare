package database

import (
	"context"
	"testing"
)

// fakeProbe is an in-test blobProbe backed by a set of "present" hashes and an
// "intact" set (for the deep scan). A hash absent from intact but present in
// present models a corrupted blob. deleted records DeleteAll sweeps.
type fakeProbe struct {
	present map[string]bool
	intact  map[string]bool
	deleted map[string]bool
	err     error
}

func (p *fakeProbe) BlobPresent(hash string) (bool, error) {
	if p.err != nil {
		return false, p.err
	}
	return p.present[hash], nil
}

func (p *fakeProbe) VerifyBlob(hash string) (bool, error) {
	if p.err != nil {
		return false, p.err
	}
	// A present blob with no explicit intact map defaults to intact.
	if p.intact == nil {
		return p.present[hash], nil
	}
	return p.intact[hash], nil
}

func (p *fakeProbe) DeleteAll(hash string) (bool, error) {
	if p.deleted == nil {
		p.deleted = map[string]bool{}
	}
	p.deleted[hash] = true
	return true, nil
}

// seedFile inserts a files row (plus one upload) and returns its hash.
func seedFile(t *testing.T, db *DB, hash, filename string) {
	t.Helper()
	f := newFile(hash)
	if err := db.InsertFile(context.Background(), f, newUpload(filename), newMeta()); err != nil {
		t.Fatalf("seed %s: %v", hash, err)
	}
}

const (
	hashHealthy  = "1111000000000000000000000000000000000000000000000000000000000000"
	hashDangling = "2222000000000000000000000000000000000000000000000000000000000000"
)

// TestPruneDangling_DryRunReportsButKeeps verifies a dry run reports the
// dangling row but deletes nothing.
func TestPruneDangling_DryRunReportsButKeeps(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedFile(t, db, hashHealthy, "healthy.mp3")
	seedFile(t, db, hashDangling, "gone.mp3")

	probe := &fakeProbe{present: map[string]bool{hashHealthy: true}} // dangling blob missing

	res, err := PruneDangling(ctx, db, probe, false, false)
	if err != nil {
		t.Fatalf("PruneDangling: %v", err)
	}
	if res.Scanned != 2 {
		t.Errorf("Scanned = %d, want 2", res.Scanned)
	}
	if len(res.Dangling) != 1 || res.Dangling[0].Hash != hashDangling {
		t.Errorf("Dangling = %+v, want one entry for %s", res.Dangling, hashDangling)
	}
	if len(res.Pruned) != 0 {
		t.Errorf("Pruned = %+v, want empty on dry run", res.Pruned)
	}

	// Nothing deleted.
	var files int
	db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&files)
	if files != 2 {
		t.Errorf("files rows = %d, want 2 (dry run must not delete)", files)
	}
}

// TestPruneDangling_ConfirmDeletesDanglingOnly verifies confirm=true prunes the
// dangling row, leaves the healthy one, and is idempotent on a re-run.
func TestPruneDangling_ConfirmDeletesDanglingOnly(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedFile(t, db, hashHealthy, "healthy.mp3")
	seedFile(t, db, hashDangling, "gone.mp3")

	probe := &fakeProbe{present: map[string]bool{hashHealthy: true}}

	res, err := PruneDangling(ctx, db, probe, true, false)
	if err != nil {
		t.Fatalf("PruneDangling: %v", err)
	}
	if len(res.Pruned) != 1 || res.Pruned[0].Hash != hashDangling {
		t.Errorf("Pruned = %+v, want one entry for %s", res.Pruned, hashDangling)
	}
	if len(res.Failed) != 0 {
		t.Errorf("Failed = %+v, want empty", res.Failed)
	}

	// The healthy file survives; the dangling one is gone.
	if got, _ := db.GetFileByHash(ctx, hashHealthy); got == nil {
		t.Error("healthy file was pruned; want it kept")
	}
	if got, _ := db.GetFileByHash(ctx, hashDangling); got != nil {
		t.Error("dangling file still present after confirm prune")
	}

	// Idempotent re-run: nothing left to prune, no error.
	res2, err := PruneDangling(ctx, db, probe, true, false)
	if err != nil {
		t.Fatalf("PruneDangling re-run: %v", err)
	}
	if len(res2.Dangling) != 0 || len(res2.Pruned) != 0 {
		t.Errorf("re-run found work: dangling=%v pruned=%v", res2.Dangling, res2.Pruned)
	}
}

// TestPruneDangling_DeepDetectsCorruption verifies the opt-in deep scan flags a
// present-but-corrupted blob (Issue 1) while the cheap scan leaves it alone, and
// that the reason is reported and the blob swept on confirm.
func TestPruneDangling_DeepDetectsCorruption(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedFile(t, db, hashHealthy, "good.mp3")
	seedFile(t, db, hashDangling, "rotted.mp3")

	// Both blobs are present, but hashDangling's content no longer matches.
	probe := &fakeProbe{
		present: map[string]bool{hashHealthy: true, hashDangling: true},
		intact:  map[string]bool{hashHealthy: true}, // hashDangling corrupted
	}

	// Cheap scan: both present, nothing flagged.
	shallow, err := PruneDangling(ctx, db, probe, false, false)
	if err != nil {
		t.Fatalf("shallow PruneDangling: %v", err)
	}
	if len(shallow.Dangling) != 0 {
		t.Errorf("shallow Dangling = %+v, want none (corruption invisible to cheap scan)", shallow.Dangling)
	}

	// Deep scan (dry run): the corrupted blob is flagged with reason "corrupt".
	deep, err := PruneDangling(ctx, db, probe, false, true)
	if err != nil {
		t.Fatalf("deep PruneDangling: %v", err)
	}
	if !deep.Deep {
		t.Error("result.Deep = false, want true")
	}
	if len(deep.Dangling) != 1 || deep.Dangling[0].Hash != hashDangling || deep.Dangling[0].Reason != ReasonCorrupt {
		t.Fatalf("deep Dangling = %+v, want one corrupt entry for %s", deep.Dangling, hashDangling)
	}

	// Deep scan (confirm): prunes the row and sweeps the bad blob.
	committed, err := PruneDangling(ctx, db, probe, true, true)
	if err != nil {
		t.Fatalf("deep confirm PruneDangling: %v", err)
	}
	if len(committed.Pruned) != 1 || committed.Pruned[0].Reason != ReasonCorrupt {
		t.Fatalf("deep Pruned = %+v, want one corrupt entry", committed.Pruned)
	}
	if !probe.deleted[hashDangling] {
		t.Error("corrupt blob was not swept via DeleteAll on confirm")
	}
	if got, _ := db.GetFileByHash(ctx, hashHealthy); got == nil {
		t.Error("healthy file was pruned; want it kept")
	}
}

// TestPruneDangling_AllHealthy verifies that when every blob exists nothing is
// reported or deleted.
func TestPruneDangling_AllHealthy(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedFile(t, db, hashHealthy, "a.mp3")
	seedFile(t, db, hashDangling, "b.mp3")

	probe := &fakeProbe{present: map[string]bool{hashHealthy: true, hashDangling: true}}

	res, err := PruneDangling(ctx, db, probe, true, false)
	if err != nil {
		t.Fatalf("PruneDangling: %v", err)
	}
	if len(res.Dangling) != 0 {
		t.Errorf("Dangling = %+v, want none", res.Dangling)
	}
	if len(res.Pruned) != 0 {
		t.Errorf("Pruned = %+v, want none", res.Pruned)
	}
}
