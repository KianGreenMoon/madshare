package database

import (
	"context"
	"testing"
)

// fakeProbe is an in-test blobProbe backed by a set of "present" hashes.
type fakeProbe struct {
	present map[string]bool
	err     error
}

func (p *fakeProbe) HashDirExists(hash string) (bool, error) {
	if p.err != nil {
		return false, p.err
	}
	return p.present[hash], nil
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

	res, err := PruneDangling(ctx, db, probe, false)
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

	res, err := PruneDangling(ctx, db, probe, true)
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
	res2, err := PruneDangling(ctx, db, probe, true)
	if err != nil {
		t.Fatalf("PruneDangling re-run: %v", err)
	}
	if len(res2.Dangling) != 0 || len(res2.Pruned) != 0 {
		t.Errorf("re-run found work: dangling=%v pruned=%v", res2.Dangling, res2.Pruned)
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

	res, err := PruneDangling(ctx, db, probe, true)
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
