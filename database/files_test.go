package database

import (
	"context"
	"database/sql"
	"testing"
)

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
		Title:       sql.NullString{String: "A Song", Valid: true},
		Artist:      sql.NullString{String: "An Artist", Valid: true},
		Album:       sql.NullString{String: "An Album", Valid: true},
		TagFormat:   sql.NullString{String: "ID3v2.4", Valid: true},
		ExtractedAt: 1700000000,
	}
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
