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

// TestReprTagset_OwnAppearanceWinsOverRecording guards the precedence rule that
// makes the recording-rooted lookup safe (recording-tagsets P7). reprTagset
// searches the file's whole recording so an orphaned rendition is still
// covered — but the blob's *own* offered appearance must win when it has one,
// because the per-blob lifecycle (review state, trash mark) lives there.
//
// Without the precedence, a rendition awaiting review on an already-published
// recording borrows the recording's approved primary: it leaks into the live
// All-files listing and its bytes are misfiled from Review into Library.
func TestReprTagset_OwnAppearanceWinsOverRecording(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("rt-own1"), "studio.flac", "The Band", "Studio Album")
	rec := recordingIDOf(t, db, f1.ID)

	// A second rendition of the same recording whose own appearance is still a
	// draft awaiting review (the moderation "case B" shape).
	f2 := insertTaggedFile(t, db, hash64("rt-own2"), "pending.mp3", "The Band", "Bootleg")
	if _, err := db.ExecContext(ctx, `UPDATE files SET recording_id=? WHERE id=?`, rec, f2.ID); err != nil {
		t.Fatalf("move rendition: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE tagsets SET recording_id=?, is_primary=0, review_state='draft' WHERE origin_file_id=?`,
		rec, f2.ID); err != nil {
		t.Fatalf("stage appearance: %v", err)
	}

	rows, err := db.ListFilesPage(ctx, FileListQuery{})
	if err != nil {
		t.Fatalf("ListFilesPage: %v", err)
	}
	for _, r := range rows {
		if r.Hash == f2.Hash {
			t.Error("pending-review rendition leaked into the live All-files listing")
		}
	}
	if len(rows) != 1 {
		t.Errorf("All files = %d row(s), want 1 (only the approved rendition)", len(rows))
	}

	b, err := db.StorageByteBreakdown(ctx)
	if err != nil {
		t.Fatalf("StorageByteBreakdown: %v", err)
	}
	if b.Review == 0 || b.Library == 0 {
		t.Errorf("bytes library=%d review=%d; want the staged blob counted in Review, the approved one in Library",
			b.Library, b.Review)
	}
}
