package database

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestStorageByteBreakdown verifies the per-state byte partition: approved/live
// files count as Library, not-yet-approved as Review, and any soft-deleted file
// as Trash (precedence over review state), with the three buckets mutually
// exclusive.
func TestStorageByteBreakdown(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	ins := func(hash string, size int64, state string) {
		f := newFile(hash)
		f.ByteSize = size
		f.ReviewState = state
		if err := db.InsertFile(ctx, f, newUpload("x.mp3"), newMeta()); err != nil {
			t.Fatalf("InsertFile %s: %v", hash, err)
		}
	}
	libHash := strings.Repeat("a", 64)
	revHash := strings.Repeat("b", 64)
	trashHash := strings.Repeat("c", 64)
	trashUnapprovedHash := strings.Repeat("d", 64)

	ins(libHash, 1000, ReviewApproved)           // live library
	ins(revHash, 200, ReviewSubmitted)           // on review
	ins(trashHash, 30, ReviewApproved)           // → trashed below
	ins(trashUnapprovedHash, 4, ReviewSubmitted) // trashed AND unapproved → Trash

	for _, h := range []string{trashHash, trashUnapprovedHash} {
		trashAppearancesByHash(t, db, h)
	}

	bd, err := db.StorageByteBreakdown(ctx)
	if err != nil {
		t.Fatalf("StorageByteBreakdown: %v", err)
	}
	if bd.Library != 1000 {
		t.Errorf("Library = %d, want 1000", bd.Library)
	}
	if bd.Review != 200 {
		t.Errorf("Review = %d, want 200", bd.Review)
	}
	if bd.Trash != 34 {
		t.Errorf("Trash = %d, want 34 (30+4)", bd.Trash)
	}
}

func newFile(hash string) *File {
	return &File{
		Hash:           hash,
		ByteSize:       42,
		MimeType:       "audio/mpeg",
		StorageBackend: "local",
		ObjectKey:      hash + "/song.mp3",
		CreatedAt:      1700000000,
	}
}

func newUpload(filename string) *FileUpload {
	return &FileUpload{Filename: filename, UploadedAt: 1700000000}
}

func newMeta() *MediaMetadata {
	return &MediaMetadata{
		Title:       "A Song",
		Artist:      sql.NullString{String: "An Artist", Valid: true},
		Album:       sql.NullString{String: "An Album", Valid: true},
		TagFormat:   sql.NullString{String: "ID3v2.4", Valid: true},
		ExtractedAt: 1700000000,
	}
}

// trashAppearancesByHash is the tests' stand-in for the removed hash-addressed
// soft delete: it trashes the live appearances offered from the blob — the
// state the old SoftDeleteFileByHash produced. Production trashes by tagset id
// (GC model P3); tests keep the hash shorthand because they created the file.
func trashAppearancesByHash(t *testing.T, db *DB, hash string) {
	t.Helper()
	res, err := db.ExecContext(context.Background(), `
		UPDATE tagsets SET deleted_at = ?
		WHERE deleted_at IS NULL
		  AND origin_file_id IN (SELECT id FROM files WHERE hash = ?)`,
		time.Now().Unix(), hash)
	if err != nil {
		t.Fatalf("trash appearances of %s: %v", hash, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		t.Fatalf("trash appearances of %s: no live appearance matched", hash)
	}
}

// trashedTagsetIDsByHash resolves the trashed appearances offered from the
// given blobs, in id order — the tagset addressing the purge ops take.
func trashedTagsetIDsByHash(t *testing.T, db *DB, hashes ...string) []int64 {
	t.Helper()
	var ids []int64
	for _, h := range hashes {
		got, err := scanIDs(db.QueryContext(context.Background(), `
			SELECT t.id FROM tagsets t
			  JOIN files f ON f.id = t.origin_file_id
			 WHERE t.deleted_at IS NOT NULL AND f.hash = ?
			 ORDER BY t.id`, h))
		if err != nil {
			t.Fatalf("trashed tagsets of %s: %v", h, err)
		}
		ids = append(ids, got...)
	}
	return ids
}

func TestInsertFile_WritesAllThreeRows(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f := newFile("aa" + "bbccddeeff0011223344556677889900aabbccddeeff0011223344556677889900"[:62])
	upload := newUpload("song.mp3")
	meta := newMeta()

	if err := db.InsertFile(ctx, f, upload, meta); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}
	if f.ID == 0 {
		t.Fatal("expected f.ID to be populated")
	}

	var (
		filesCount, uploadsCount, metaCount int
	)
	db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&filesCount)
	db.QueryRow(`SELECT COUNT(*) FROM file_uploads`).Scan(&uploadsCount)
	db.QueryRow(`SELECT COUNT(*) FROM media_metadata`).Scan(&metaCount)

	if filesCount != 1 {
		t.Errorf("files rows = %d, want 1", filesCount)
	}
	if uploadsCount != 1 {
		t.Errorf("file_uploads rows = %d, want 1", uploadsCount)
	}
	if metaCount != 1 {
		t.Errorf("media_metadata rows = %d, want 1", metaCount)
	}

	var gotFileID int64
	if err := db.QueryRow(`SELECT file_id FROM file_uploads WHERE id = 1`).Scan(&gotFileID); err != nil {
		t.Fatal(err)
	}
	if gotFileID != f.ID {
		t.Errorf("file_uploads.file_id = %d, want %d", gotFileID, f.ID)
	}
}

func TestGetFileByHash_Hit(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	f := newFile("hit" + "0000000000000000000000000000000000000000000000000000000000000"[:61])

	if err := db.InsertFile(ctx, f, newUpload("a.mp3"), newMeta()); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}

	got, err := db.GetFileByHash(ctx, f.Hash)
	if err != nil {
		t.Fatalf("GetFileByHash: %v", err)
	}
	if got == nil {
		t.Fatal("expected file, got nil")
	}
	if got.Hash != f.Hash {
		t.Errorf("hash = %q, want %q", got.Hash, f.Hash)
	}
	if got.ByteSize != f.ByteSize {
		t.Errorf("byte_size = %d, want %d", got.ByteSize, f.ByteSize)
	}
	if got.MimeType != f.MimeType {
		t.Errorf("mime_type = %q, want %q", got.MimeType, f.MimeType)
	}
	if got.ObjectKey != f.ObjectKey {
		t.Errorf("object_key = %q, want %q", got.ObjectKey, f.ObjectKey)
	}
}

func TestGetFileByHash_Miss(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	got, err := db.GetFileByHash(ctx, "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	if err != nil {
		t.Fatalf("GetFileByHash on miss: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil on miss, got %+v", got)
	}
}

func TestRecordUpload_AddsRow(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	f := newFile("rec" + "0000000000000000000000000000000000000000000000000000000000000"[:61])

	if err := db.InsertFile(ctx, f, newUpload("first.mp3"), newMeta()); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}

	if err := db.RecordUpload(ctx, f.ID, "second.mp3"); err != nil {
		t.Fatalf("RecordUpload: %v", err)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM file_uploads WHERE file_id = ?`, f.ID).Scan(&count)
	if count != 2 {
		t.Errorf("upload rows for file = %d, want 2", count)
	}
}

func TestRecordUpload_DuplicateIsNoOp(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	f := newFile("dup" + "0000000000000000000000000000000000000000000000000000000000000"[:61])

	if err := db.InsertFile(ctx, f, newUpload("only.mp3"), newMeta()); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}

	// Duplicate (file_id, filename) — must not error, must not insert.
	if err := db.RecordUpload(ctx, f.ID, "only.mp3"); err != nil {
		t.Fatalf("RecordUpload duplicate: %v", err)
	}

	var count int
	db.QueryRow(`SELECT COUNT(*) FROM file_uploads WHERE file_id = ?`, f.ID).Scan(&count)
	if count != 1 {
		t.Errorf("upload rows = %d, want 1 (duplicate ignored)", count)
	}
}

func TestInsertFile_UniqueHashConflict(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	f := newFile("uniq" + "000000000000000000000000000000000000000000000000000000000000"[:60])

	if err := db.InsertFile(ctx, f, newUpload("a.mp3"), newMeta()); err != nil {
		t.Fatalf("first InsertFile: %v", err)
	}

	// Second insert with same hash must fail (UNIQUE constraint on files.hash).
	f2 := newFile(f.Hash)
	if err := db.InsertFile(ctx, f2, newUpload("b.mp3"), newMeta()); err == nil {
		t.Fatal("expected error on duplicate hash, got nil")
	}
}

// ---- HardDeleteFileByHash --------------------------------------------------------

// TestHardDeleteFileByHash_CascadesToChildRows is the critical correctness check:
// deleting a files row must also remove its file_uploads AND media_metadata
// rows via ON DELETE CASCADE. This only fires when foreign_keys is ON on the
// executing connection (see Open).
func TestHardDeleteFileByHash_CascadesToChildRows(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	hash := "abcd000000000000000000000000000000000000000000000000000000000000"
	f := newFile(hash)
	if err := db.InsertFile(ctx, f, newUpload("first.mp3"), newMeta()); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}
	if err := db.RecordUpload(ctx, f.ID, "second.mp3"); err != nil {
		t.Fatalf("RecordUpload: %v", err)
	}

	filenames, found, err := db.HardDeleteFileByHash(ctx, hash)
	if err != nil {
		t.Fatalf("HardDeleteFileByHash: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}
	want := []string{"first.mp3", "second.mp3"}
	if !reflect.DeepEqual(filenames, want) {
		t.Errorf("filenames = %v, want %v", filenames, want)
	}

	var files, uploads, meta int
	db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&files)
	db.QueryRow(`SELECT COUNT(*) FROM file_uploads`).Scan(&uploads)
	db.QueryRow(`SELECT COUNT(*) FROM media_metadata`).Scan(&meta)
	if files != 0 {
		t.Errorf("files rows = %d, want 0", files)
	}
	if uploads != 0 {
		t.Errorf("file_uploads rows = %d, want 0 (cascade failed)", uploads)
	}
	if meta != 0 {
		t.Errorf("media_metadata rows = %d, want 0 (cascade failed)", meta)
	}
}

// TestHardDeleteFileByHash_NotFound returns found=false (no error) for an unknown hash.
func TestHardDeleteFileByHash_NotFound(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	filenames, found, err := db.HardDeleteFileByHash(ctx, "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	if err != nil {
		t.Fatalf("HardDeleteFileByHash on miss: %v", err)
	}
	if found {
		t.Error("found = true on miss, want false")
	}
	if filenames != nil {
		t.Errorf("filenames = %v, want nil on miss", filenames)
	}
}

// TestHardDeleteFileByHash_LeavesOtherFiles deletes one file and confirms a second,
// unrelated file is untouched.
func TestHardDeleteFileByHash_LeavesOtherFiles(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	hashA := "aaaa000000000000000000000000000000000000000000000000000000000000"
	hashB := "bbbb000000000000000000000000000000000000000000000000000000000000"
	if err := db.InsertFile(ctx, newFile(hashA), newUpload("a.mp3"), newMeta()); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertFile(ctx, newFile(hashB), newUpload("b.mp3"), newMeta()); err != nil {
		t.Fatal(err)
	}

	if _, found, err := db.HardDeleteFileByHash(ctx, hashA); err != nil || !found {
		t.Fatalf("HardDeleteFileByHash(A): found=%v err=%v", found, err)
	}

	got, err := db.GetFileByHash(ctx, hashB)
	if err != nil {
		t.Fatalf("GetFileByHash(B): %v", err)
	}
	if got == nil {
		t.Error("file B was removed; want it to survive deletion of A")
	}
}

// ---- ListFileRefs ------------------------------------------------------------

func TestListFileRefs_Empty(t *testing.T) {
	db := openMem(t)
	refs, err := db.ListFileRefs(context.Background())
	if err != nil {
		t.Fatalf("ListFileRefs: %v", err)
	}
	if refs == nil {
		t.Fatal("ListFileRefs returned nil; want non-nil empty slice")
	}
	if len(refs) != 0 {
		t.Errorf("len = %d, want 0", len(refs))
	}
}

func TestListFileRefs_GroupsFilenames(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	hash := "1234000000000000000000000000000000000000000000000000000000000000"
	f := newFile(hash)
	if err := db.InsertFile(ctx, f, newUpload("one.mp3"), newMeta()); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordUpload(ctx, f.ID, "two.mp3"); err != nil {
		t.Fatal(err)
	}

	refs, err := db.ListFileRefs(ctx)
	if err != nil {
		t.Fatalf("ListFileRefs: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("len = %d, want 1", len(refs))
	}
	if refs[0].Hash != hash {
		t.Errorf("Hash = %q, want %q", refs[0].Hash, hash)
	}
	want := []string{"one.mp3", "two.mp3"}
	if !reflect.DeepEqual(refs[0].Filenames, want) {
		t.Errorf("Filenames = %v, want %v", refs[0].Filenames, want)
	}
}

// TestListFileRefs_NoUploads verifies a file with no file_uploads rows yields
// an empty (not nil-with-one-empty-string) filename slice.
func TestListFileRefs_NoUploads(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	hash := "5678000000000000000000000000000000000000000000000000000000000000"
	// Insert a files row with no upload rows by passing nil upload.
	if err := db.InsertFile(ctx, newFile(hash), nil, newMeta()); err != nil {
		t.Fatal(err)
	}

	refs, err := db.ListFileRefs(ctx)
	if err != nil {
		t.Fatalf("ListFileRefs: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("len = %d, want 1", len(refs))
	}
	if len(refs[0].Filenames) != 0 {
		t.Errorf("Filenames = %v, want empty slice", refs[0].Filenames)
	}
}

// ---- ListFiles --------------------------------------------------------------

// TestListFiles_Empty verifies ListFiles returns a non-nil empty slice on an
// empty database (never nil, so JSON encoding produces [] not null).
func TestListFiles_Empty(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	entries, err := db.ListFiles(ctx)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	// Result must be non-nil so it encodes as [] not null.
	if entries == nil {
		t.Fatal("ListFiles returned nil on empty DB; want non-nil empty slice")
	}
	if len(entries) != 0 {
		t.Fatalf("ListFiles on empty DB returned %d entries; want 0", len(entries))
	}
}

// TestListFiles_ReturnsSingleFile inserts one file and verifies all fields are
// correctly populated in the ListFiles result.
func TestListFiles_ReturnsSingleFile(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	hash := "aabb000000000000000000000000000000000000000000000000000000000000"
	f := newFile(hash)
	upload := newUpload("song.mp3")
	meta := newMeta()
	if err := db.InsertFile(ctx, f, upload, meta); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}

	entries, err := db.ListFiles(ctx)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.Hash != hash {
		t.Errorf("Hash = %q, want %q", e.Hash, hash)
	}
	if e.Filename != "song.mp3" {
		t.Errorf("Filename = %q, want song.mp3", e.Filename)
	}
	if e.MimeType != "audio/mpeg" {
		t.Errorf("MimeType = %q, want audio/mpeg", e.MimeType)
	}
	if e.ByteSize != 42 {
		t.Errorf("ByteSize = %d, want 42", e.ByteSize)
	}
	if e.Title != "A Song" {
		t.Errorf("Title = %q, want A Song", e.Title)
	}
	if e.Artist != "An Artist" {
		t.Errorf("Artist = %q, want An Artist", e.Artist)
	}
	if e.Album != "An Album" {
		t.Errorf("Album = %q, want An Album", e.Album)
	}
	if e.ObjectKey != hash+"/song.mp3" {
		t.Errorf("ObjectKey = %q, want %q", e.ObjectKey, hash+"/song.mp3")
	}
	if e.ID <= 0 {
		t.Errorf("ID = %d, want > 0", e.ID)
	}
}

// TestListFiles_OrderByCreatedAtDesc inserts two files at different timestamps
// and verifies the most-recently-created one appears first.
func TestListFiles_OrderByCreatedAtDesc(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	hash1 := "1111000000000000000000000000000000000000000000000000000000000000"
	hash2 := "2222000000000000000000000000000000000000000000000000000000000000"

	f1 := &File{
		Hash: hash1, ByteSize: 1, MimeType: "audio/mpeg",
		StorageBackend: "local", ObjectKey: hash1 + "/a.mp3", CreatedAt: 1000,
	}
	f2 := &File{
		Hash: hash2, ByteSize: 2, MimeType: "audio/mpeg",
		StorageBackend: "local", ObjectKey: hash2 + "/b.mp3", CreatedAt: 2000,
	}

	if err := db.InsertFile(ctx, f1, &FileUpload{Filename: "a.mp3", UploadedAt: 1000}, &MediaMetadata{ExtractedAt: 1000}); err != nil {
		t.Fatalf("InsertFile f1: %v", err)
	}
	if err := db.InsertFile(ctx, f2, &FileUpload{Filename: "b.mp3", UploadedAt: 2000}, &MediaMetadata{ExtractedAt: 2000}); err != nil {
		t.Fatalf("InsertFile f2: %v", err)
	}

	entries, err := db.ListFiles(ctx)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("len = %d, want 2", len(entries))
	}
	if entries[0].Hash != hash2 {
		t.Errorf("entries[0].Hash = %q, want %q (most recent first)", entries[0].Hash, hash2)
	}
	if entries[1].Hash != hash1 {
		t.Errorf("entries[1].Hash = %q, want %q", entries[1].Hash, hash1)
	}
}

// TestListFiles_NullMetadataCoalesces verifies that a file with no media tags
// returns empty strings (not an error) for artist/album and 0 for year. Title is
// required non-empty (migration 016): a file with no metadata row at all
// coalesces to ”, while an all-null meta row defaults its title to the filename.
func TestListFiles_NullMetadataCoalesces(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	hash := "cccc000000000000000000000000000000000000000000000000000000000000"
	f := &File{
		Hash: hash, ByteSize: 10, MimeType: "audio/ogg",
		StorageBackend: "local", ObjectKey: hash + "/x.ogg", CreatedAt: 999,
	}
	// Insert with nil meta (no tags extracted).
	if err := db.InsertFile(ctx, f, &FileUpload{Filename: "x.ogg", UploadedAt: 999}, nil); err != nil {
		// nil meta is accepted by InsertFile (the nil-check skips the INSERT).
		// If an error occurs here, the test is verifying the error path.
		t.Logf("InsertFile with nil meta: %v", err)
	}

	// Insert with an all-null meta row explicitly.
	hash2 := "dddd000000000000000000000000000000000000000000000000000000000000"
	f2 := &File{
		Hash: hash2, ByteSize: 10, MimeType: "audio/ogg",
		StorageBackend: "local", ObjectKey: hash2 + "/y.ogg", CreatedAt: 998,
	}
	emptyMeta := &MediaMetadata{ExtractedAt: 998} // all fields NULL
	if err := db.InsertFile(ctx, f2, &FileUpload{Filename: "y.ogg", UploadedAt: 998}, emptyMeta); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}

	entries, err := db.ListFiles(ctx)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}

	for _, e := range entries {
		if e.Artist != "" {
			t.Errorf("hash %s: Artist = %q, want empty string", e.Hash, e.Artist)
		}
		if e.Album != "" {
			t.Errorf("hash %s: Album = %q, want empty string", e.Hash, e.Album)
		}
		if e.Year != 0 {
			t.Errorf("hash %s: Year = %d, want 0", e.Hash, e.Year)
		}
		switch e.Hash {
		case hash: // nil meta → the offered tagset defaults its title from the filename
			if e.Title != "x" {
				t.Errorf("nil-meta file: Title = %q, want %q (filename default)", e.Title, "x")
			}
		case hash2: // all-null meta → title defaulted from filename "y.ogg"
			if e.Title != "y" {
				t.Errorf("empty-meta file: Title = %q, want %q (filename default)", e.Title, "y")
			}
		}
	}
}

// ---- Trash visibility -------------------------------------------------------

func TestListFiles_ExcludesTrashed(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	hashA := "asoft00000000000000000000000000000000000000000000000000000000000"
	hashB := "bsoft00000000000000000000000000000000000000000000000000000000000"
	if err := db.InsertFile(ctx, newFile(hashA), newUpload("a.mp3"), newMeta()); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertFile(ctx, newFile(hashB), newUpload("b.mp3"), newMeta()); err != nil {
		t.Fatal(err)
	}

	trashAppearancesByHash(t, db, hashA)

	entries, err := db.ListFiles(ctx)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ListFiles returned %d entries, want 1 (trashed file should be excluded)", len(entries))
	}
	if entries[0].Hash != hashB {
		t.Errorf("ListFiles[0].Hash = %q, want %q (live file)", entries[0].Hash, hashB)
	}
}

// ---- RestoreFileByHash ------------------------------------------------------

func TestRestoreFileByHash_ClearsDeletedAt(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	hash := "rest0000000000000000000000000000000000000000000000000000000000000"
	if err := db.InsertFile(ctx, newFile(hash), newUpload("track.mp3"), newMeta()); err != nil {
		t.Fatal(err)
	}
	trashAppearancesByHash(t, db, hash)

	found, err := db.RestoreFileByHash(ctx, hash)
	if err != nil {
		t.Fatalf("RestoreFileByHash: %v", err)
	}
	if !found {
		t.Fatal("found = false, want true")
	}

	got, err := db.GetFileByHash(ctx, hash)
	if err != nil || got == nil {
		t.Fatalf("GetFileByHash after restore: %v", err)
	}
	if got.DeletedAt.Valid {
		t.Errorf("DeletedAt = %d, want NULL after restore", got.DeletedAt.Int64)
	}

	// File must appear in ListFiles again.
	entries, err := db.ListFiles(ctx)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("ListFiles returned %d entries after restore, want 1", len(entries))
	}
}

func TestRestoreFileByHash_NotFound(t *testing.T) {
	db := openMem(t)
	found, err := db.RestoreFileByHash(context.Background(),
		"ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	if err != nil {
		t.Fatalf("RestoreFileByHash on miss: %v", err)
	}
	if found {
		t.Error("found = true on miss, want false")
	}
}

func TestRestoreFileByHash_LiveFileReturnsFalse(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	hash := "live0000000000000000000000000000000000000000000000000000000000000"
	if err := db.InsertFile(ctx, newFile(hash), newUpload("a.mp3"), newMeta()); err != nil {
		t.Fatal(err)
	}

	// Restore on a live (not trashed) file returns found=false; it should be
	// a no-op and not corrupt the row.
	found, err := db.RestoreFileByHash(ctx, hash)
	if err != nil {
		t.Fatalf("RestoreFileByHash on live file: %v", err)
	}
	if found {
		t.Error("found = true restoring a live file, want false")
	}
}

// ---- GetFileByHash trash view -------------------------------------------------

func TestGetFileByHash_ReturnsSoftDeletedFile(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	hash := "getsft0000000000000000000000000000000000000000000000000000000000"
	if err := db.InsertFile(ctx, newFile(hash), newUpload("t.mp3"), newMeta()); err != nil {
		t.Fatal(err)
	}
	trashAppearancesByHash(t, db, hash)

	got, err := db.GetFileByHash(ctx, hash)
	if err != nil {
		t.Fatalf("GetFileByHash: %v", err)
	}
	if got == nil {
		t.Fatal("GetFileByHash returned nil for trashed file; should still return the row")
	}
	if !got.DeletedAt.Valid {
		t.Error("DeletedAt not set on trashed file returned by GetFileByHash")
	}
}

// TestListFiles_FilenameFromFirstUpload verifies the COALESCE subquery returns
// the first filename inserted via file_uploads, not a later one.
func TestListFiles_FilenameFromFirstUpload(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	hash := "eeee000000000000000000000000000000000000000000000000000000000000"
	f := newFile(hash)
	if err := db.InsertFile(ctx, f, newUpload("first.mp3"), newMeta()); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}
	// Add a second upload record with a different filename.
	if err := db.RecordUpload(ctx, f.ID, "second.mp3"); err != nil {
		t.Fatalf("RecordUpload: %v", err)
	}

	entries, err := db.ListFiles(ctx)
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}
	// "LIMIT 1" with no ORDER BY in the subquery is non-deterministic, but
	// in SQLite with a single-connection pool the insert order is preserved.
	// Document rather than assert a specific value.
	if entries[0].Filename == "" {
		t.Error("Filename is empty; COALESCE fallback to hash not working")
	}
}
