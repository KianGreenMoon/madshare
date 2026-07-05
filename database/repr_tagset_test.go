package database

import (
	"context"
	"testing"
)

// TestReprTagset_FilesRootedStaysOnePerFile guards the representative-tagset
// pick (recording-tagsets P4): a blob that carries several appearances — as a
// byte-dup upload produces — must still appear exactly once in the files-rooted
// surfaces (admin files, storage breakdown), showing its primary appearance.
func TestReprTagset_FilesRootedStaysOnePerFile(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f := insertTaggedFile(t, db, hash64("rt1"), "song.flac", "Artist", "Album A")
	// A second (draft) appearance on the same blob — a different release.
	if _, err := db.Exec(`
		INSERT INTO tagsets (recording_id, title, album, review_state, created_by, origin_file_id, is_primary, created_at)
		SELECT recording_id, 'Song', 'Album B', 'draft', uploaded_by, id, 0, 1700000002
		  FROM files WHERE id = ?`, f.ID); err != nil {
		t.Fatalf("insert dup tagset: %v", err)
	}

	// The files-rooted listing shows the file exactly once, via its primary.
	files, err := db.ListFiles(ctx)
	if err != nil {
		t.Fatalf("list files: %v", err)
	}
	n, album := 0, ""
	for _, e := range files {
		if e.Hash == f.Hash {
			n++
			album = e.Album
		}
	}
	if n != 1 {
		t.Errorf("file appears %d times in ListFiles, want 1 (representative only)", n)
	}
	if album != "Album A" {
		t.Errorf("representative album = %q, want %q (the primary)", album, "Album A")
	}

	// The blob is counted once in the storage breakdown — as a library blob, not
	// double-counted into the review bucket by the extra draft appearance.
	b, err := db.StorageByteBreakdown(ctx)
	if err != nil {
		t.Fatalf("storage breakdown: %v", err)
	}
	if b.Library != f.ByteSize {
		t.Errorf("library bytes = %d, want %d (blob counted once)", b.Library, f.ByteSize)
	}
	if b.Review != 0 {
		t.Errorf("review bytes = %d, want 0 (draft appearance shares the library blob)", b.Review)
	}
}
