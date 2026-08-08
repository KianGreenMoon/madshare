package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func writeBytes(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReconcileImageVariants is the rule migration 043 rests on: the directory
// is authoritative and the index describes it. Whatever the rows said before,
// after a reconcile they match the tree — including dropping rows whose
// directory has gone, which is how the orphan sweep's removals reach the index.
func TestReconcileImageVariants(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	dir := t.TempDir()

	writeBytes(t, filepath.Join(dir, "aaa", "small_crop.jpg"), 1000)
	writeBytes(t, filepath.Join(dir, "aaa", "small_fit.jpg"), 500)
	writeBytes(t, filepath.Join(dir, "bbb", "small_crop.jpg"), 250)
	// A stale row for a directory that is not there.
	if err := db.SetImageVariantBytes(ctx, "ccc", 999999); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}

	n, err := db.ReconcileImageVariants(ctx, dir)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 2 {
		t.Errorf("indexed %d dir(s), want 2", n)
	}
	total, err := db.ImageVariantBytes(ctx)
	if err != nil {
		t.Fatalf("total: %v", err)
	}
	if total != 1750 {
		t.Errorf("total = %d, want 1750 (1000+500+250; the stale row must be gone)", total)
	}

	// Re-running changes nothing, and a shrunk directory is re-totalled rather
	// than added to — the index tracks the tree, it does not accumulate.
	if err := os.Remove(filepath.Join(dir, "aaa", "small_fit.jpg")); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ReconcileImageVariants(ctx, dir); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if total, _ := db.ImageVariantBytes(ctx); total != 1250 {
		t.Errorf("after shrinking a variant set, total = %d, want 1250", total)
	}
}

// TestReconcileImageVariants_MissingTreeIsNotAnError: a fresh install has no
// covers yet, and the panel must still answer.
func TestReconcileImageVariants_MissingTreeIsNotAnError(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	if err := db.SetImageVariantBytes(ctx, "ddd", 4242); err != nil {
		t.Fatalf("seed: %v", err)
	}
	n, err := db.ReconcileImageVariants(ctx, filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("reconcile on a missing tree: %v", err)
	}
	if n != 0 {
		t.Errorf("indexed %d, want 0", n)
	}
	if total, _ := db.ImageVariantBytes(ctx); total != 0 {
		t.Errorf("total = %d, want 0 — rows describing an absent tree must not survive", total)
	}
}
