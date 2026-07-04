package database

import (
	"context"
	"database/sql"
	"testing"
)

// TestInsertFile_CreatesRecordingAndTagset asserts the tagset invariant at the
// creation edge (recording-tagsets P0): every insert atomically produces a
// singleton recording and exactly one primary tagset seeded with the file's
// review state and owner — even for a tagless upload, whose appearance resolves
// to the Unknown artist / Other buckets ("a null tagset is just a tagset").
func TestInsertFile_CreatesRecordingAndTagset(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f := newFile(hash64("ts1"))
	f.ReviewState = ReviewDraft
	f.UploadedBy = sql.NullInt64{Int64: 0, Valid: false}
	if err := db.InsertFile(ctx, f, newUpload("bare song.mp3"), nil); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}
	if f.RecordingID == 0 {
		t.Fatal("InsertFile left RecordingID unset")
	}

	var (
		title, state       string
		isPrimary          int
		recID              int64
		artistID, albumID  int64
		artistName, aTitle string
	)
	if err := db.QueryRow(`
		SELECT t.title, t.review_state, t.is_primary, t.recording_id, t.artist_id, t.album_id
		FROM tagsets t WHERE t.origin_file_id = ?`, f.ID).
		Scan(&title, &state, &isPrimary, &recID, &artistID, &albumID); err != nil {
		t.Fatalf("read tagset: %v", err)
	}
	if title != "bare song" || state != ReviewDraft || isPrimary != 1 || recID != f.RecordingID {
		t.Errorf("tagset = (%q, %s, primary %d, rec %d), want (bare song, draft, 1, %d)",
			title, state, isPrimary, recID, f.RecordingID)
	}
	// The null appearance resolves to the reserved buckets, and is kept.
	if err := db.QueryRow(`SELECT name FROM artists WHERE id = ?`, artistID).Scan(&artistName); err != nil {
		t.Fatalf("read artist: %v", err)
	}
	if err := db.QueryRow(`SELECT title FROM albums WHERE id = ?`, albumID).Scan(&aTitle); err != nil {
		t.Fatalf("read album: %v", err)
	}
	if artistName != DefaultArtistName || aTitle != DefaultAlbumTitle {
		t.Errorf("null tagset resolved to (%q, %q), want (%q, %q)",
			artistName, aTitle, DefaultArtistName, DefaultAlbumTitle)
	}
}

// TestHardDelete_CascadesSingletonRecording asserts the P0 shape of the
// hard-delete cascade: removing a file takes its tagsets and — when it was the
// recording's last file — the recording itself, while soft delete (Trash)
// removes nothing.
func TestHardDelete_CascadesSingletonRecording(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f := newFile(hash64("ts2"))
	if err := db.InsertFile(ctx, f, newUpload("x.mp3"), newMeta()); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}

	// Trash first (permanent delete requires it), then verify nothing was lost.
	if _, found, err := db.SoftDeleteFileByHash(ctx, f.Hash); err != nil || !found {
		t.Fatalf("soft delete: found=%v err=%v", found, err)
	}
	var recCount, tagCount int
	if err := db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM recordings WHERE id = ?),
		(SELECT COUNT(*) FROM tagsets WHERE origin_file_id = ?)`, f.RecordingID, f.ID).
		Scan(&recCount, &tagCount); err != nil {
		t.Fatalf("count after trash: %v", err)
	}
	if recCount != 1 || tagCount != 1 {
		t.Fatalf("after trash: recording=%d tagset=%d, want 1/1 (soft delete never cascades)", recCount, tagCount)
	}

	if _, _, found, err := db.HardDeleteTrashedFileByHash(ctx, f.Hash); err != nil || !found {
		t.Fatalf("hard delete: found=%v err=%v", found, err)
	}
	if err := db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM recordings WHERE id = ?),
		(SELECT COUNT(*) FROM tagsets WHERE origin_file_id = ?)`, f.RecordingID, f.ID).
		Scan(&recCount, &tagCount); err != nil {
		t.Fatalf("count after hard delete: %v", err)
	}
	if recCount != 0 || tagCount != 0 {
		t.Errorf("after hard delete: recording=%d tagsets=%d, want 0/0 (cascade)", recCount, tagCount)
	}
}
