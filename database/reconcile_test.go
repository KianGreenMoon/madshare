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

// mkBaseKeyDir creates an <imagesDir>/<baseKey>/original.jpg album-cover dir.
func mkBaseKeyDir(t *testing.T, imagesDir, baseKey string) string {
	t.Helper()
	dir := filepath.Join(imagesDir, baseKey)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "original.jpg"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestReconcileImageOrphans(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	imagesDir := t.TempDir()

	// 1. Orphan: a base_key dir with no row and no job → must be removed.
	orphanKey := hexHash("orphan-cover")[:16]
	orphanDir := mkBaseKeyDir(t, imagesDir, orphanKey)

	// 2. Album-referenced: dir + album_images row → kept.
	albumKey := hexHash("album-cover")[:16]
	albumDir := mkBaseKeyDir(t, imagesDir, albumKey)
	albumID, err := db.ResolveAlbumID(ctx, "Artist", "Album")
	if err != nil {
		t.Fatalf("resolve album: %v", err)
	}
	if err := db.SetAlbumCover(ctx, albumID, albumKey, ".jpg", albumKey+"/original.jpg", "image/jpeg", 1000); err != nil {
		t.Fatalf("set album cover: %v", err)
	}

	// 3. Active job: dir + pending job, no row → kept (variants in flight).
	jobKey := hexHash("job-cover")[:16]
	jobDir := mkBaseKeyDir(t, imagesDir, jobKey)
	if err := db.EnqueueImageJob(ctx, "album", "x\x1fy", jobKey, 1000); err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	// 4. Artist-referenced (shared base_key): dir + artist_images row → kept.
	artistKey := hexHash("artist-shared")[:16]
	artistDir := mkBaseKeyDir(t, imagesDir, artistKey)
	artistID, err := db.ResolveArtistID(ctx, "Some Artist")
	if err != nil {
		t.Fatalf("resolve artist: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO artist_images (artist_id, object_key, mime_type, updated_at, base_key, source_ext, variants_ready)
		 VALUES (?, ?, 'image/jpeg', 1000, ?, '.jpg', 0)`,
		artistID, artistKey+".jpg", artistKey,
	); err != nil {
		t.Fatalf("insert artist image: %v", err)
	}

	// 5. Flat artist file (not a dir) → untouched by the directory sweep.
	flatFile := filepath.Join(imagesDir, hexHash("artist-flat")[:16]+".jpg")
	if err := os.WriteFile(flatFile, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}

	// 6. Non-base_key directory name → skipped.
	miscDir := filepath.Join(imagesDir, "coverart")
	if err := os.MkdirAll(miscDir, 0755); err != nil {
		t.Fatal(err)
	}

	n, err := db.ReconcileImageOrphans(ctx, imagesDir)
	if err != nil {
		t.Fatalf("ReconcileImageOrphans: %v", err)
	}
	if n != 1 {
		t.Fatalf("removed = %d, want 1 (only the orphan)", n)
	}

	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Error("orphan dir still exists, want removed")
	}
	for _, keep := range []string{albumDir, jobDir, artistDir, flatFile, miscDir} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("expected %s kept, got %v", keep, err)
		}
	}
}

func TestReconcileImageOrphans_MissingDirOK(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	n, err := db.ReconcileImageOrphans(ctx, filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Errorf("missing imagesDir should not error, got %v", err)
	}
	if n != 0 {
		t.Errorf("removed = %d, want 0", n)
	}
}
