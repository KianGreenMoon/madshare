package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func hexHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestReconcileOrphans_RemovesUnknownHash(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	base := t.TempDir()

	orphan := hexHash("orphan")
	orphanDir := filepath.Join(base, orphan)
	if err := os.MkdirAll(orphanDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, "file.mp3"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := ReconcileOrphans(ctx, db, base); err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Errorf("orphan dir still exists after reconciliation")
	}
}

func TestReconcileOrphans_KeepsKnownHash(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	base := t.TempDir()

	known := hexHash("known")
	knownDir := filepath.Join(base, known)
	if err := os.MkdirAll(knownDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Insert a matching files row.
	f := &File{
		Hash: known, ByteSize: 1, MimeType: "audio/mpeg",
		StorageBackend: "local", ObjectKey: known + "/x", CreatedAt: 1,
	}
	if err := db.InsertFile(ctx, f, &FileUpload{Filename: "x", UploadedAt: 1}, &MediaMetadata{ExtractedAt: 1}); err != nil {
		t.Fatal(err)
	}

	if err := ReconcileOrphans(ctx, db, base); err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	if _, err := os.Stat(knownDir); err != nil {
		t.Errorf("known dir was removed: %v", err)
	}
}

func TestReconcileOrphans_SkipsNonHashEntries(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	base := t.TempDir()

	// A loose file and a non-hash directory — both must survive.
	if err := os.WriteFile(filepath.Join(base, "readme.txt"), []byte("hi"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(base, "not-a-hash"), 0755); err != nil {
		t.Fatal(err)
	}

	if err := ReconcileOrphans(ctx, db, base); err != nil {
		t.Fatalf("ReconcileOrphans: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "readme.txt")); err != nil {
		t.Error("loose file was removed")
	}
	if _, err := os.Stat(filepath.Join(base, "not-a-hash")); err != nil {
		t.Error("non-hash dir was removed")
	}
}

func TestReconcileOrphans_MissingBaseDirOK(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	if err := ReconcileOrphans(ctx, db, filepath.Join(t.TempDir(), "does-not-exist")); err != nil {
		t.Errorf("missing baseDir should not error, got %v", err)
	}
}
