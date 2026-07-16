package database

import (
	"context"
	"testing"
)

// Recordings perspective of Trash (gc-model.md): the trashed-recording bin
// (recordings wholly out of the library), whole-recording restore (un-trash +
// un-dormant), and the bulk restore / hard-delete.

// trashAllAppearances trashes every appearance of a recording (via its two
// files' tagsets) — the "all appearances trashed" arm of a trashed recording.
func trashTagset(t *testing.T, db *DB, fileID int64) {
	t.Helper()
	ts := tagsetOfFile(t, db, fileID)
	if n, err := db.BulkTrashTagsets(context.Background(), []int64{ts}); err != nil || n != 1 {
		t.Fatalf("trash tagset of file %d: n=%d err=%v", fileID, n, err)
	}
}

// buildTrashScenario seeds: recTrashed (both appearances trashed, files live),
// recDormant (single rendition removed), recLive (normal), recDraft (never
// approved). Returns the four recording ids.
func buildTrashScenario(t *testing.T, db *DB) (recTrashed, recDormant, recLive, recDraft int64) {
	t.Helper()
	ctx := context.Background()

	// recTrashed: two appearances, both trashed (files stay live).
	f1 := insertTaggedFile(t, db, hash64("tr1"), "a.flac", "The Band", "Studio")
	f2 := insertTaggedFile(t, db, hash64("tr2"), "b.mp3", "The Band", "Best Of")
	recTrashed = groupIntoRecording(t, db, f1.ID, f2.ID)
	trashTagset(t, db, f1.ID)
	trashTagset(t, db, f2.ID)

	// recDormant: single rendition removed → no surviving file.
	f3 := insertTaggedFile(t, db, hash64("tr3"), "solo.mp3", "Solo Act", "Single")
	recDormant = recordingIDOf(t, db, f3.ID)
	if found, err := db.RemoveRendition(ctx, f3.ID); err != nil || !found {
		t.Fatalf("remove f3: %v", err)
	}

	// recLive: normal, in the library.
	f4 := insertTaggedFile(t, db, hash64("tr4"), "live.mp3", "Live Act", "Album")
	recLive = recordingIDOf(t, db, f4.ID)

	// recDraft: an appearance that was never approved (moderation, not trash).
	f5 := insertTaggedFile(t, db, hash64("tr5"), "draft.mp3", "Draft Act", "Demo")
	recDraft = recordingIDOf(t, db, f5.ID)
	if _, err := db.Exec(`UPDATE tagsets SET review_state='submitted' WHERE recording_id=?`, recDraft); err != nil {
		t.Fatalf("mark draft: %v", err)
	}
	return recTrashed, recDormant, recLive, recDraft
}

func TestListTrashedRecordings(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	recTrashed, recDormant, recLive, recDraft := buildTrashScenario(t, db)

	rows, err := db.ListTrashedRecordings(ctx, "", 0, 0)
	if err != nil {
		t.Fatalf("list trashed recordings: %v", err)
	}
	got := map[int64]bool{}
	for _, r := range rows {
		got[r.ID] = true
	}
	if !got[recTrashed] || !got[recDormant] {
		t.Errorf("trashed bin missing recTrashed/recDormant: %v", got)
	}
	if got[recLive] {
		t.Errorf("live recording %d leaked into the trashed bin", recLive)
	}
	if got[recDraft] {
		t.Errorf("pure-draft recording %d leaked into the trashed bin", recDraft)
	}
	if len(rows) != 2 {
		t.Errorf("trashed recordings = %d, want 2", len(rows))
	}
	if n, err := db.CountTrashedRecordings(ctx, ""); err != nil || n != 2 {
		t.Errorf("count trashed recordings = %d (err %v), want 2", n, err)
	}
}

func TestRestoreRecording(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	recTrashed, recDormant, _, _ := buildTrashScenario(t, db)

	// Restore the all-appearances-trashed recording → both appearances visible
	// again (files were live all along).
	if found, err := db.RestoreRecording(ctx, recTrashed); err != nil || !found {
		t.Fatalf("restore recTrashed: found=%v err=%v", found, err)
	}
	if n := visibleTagsetCount(t, db, recTrashed); n != 2 {
		t.Errorf("recTrashed visible appearances = %d, want 2", n)
	}

	// Restore the dormant recording → its removed rendition comes back, so the
	// appearance is playable/visible again.
	if found, err := db.RestoreRecording(ctx, recDormant); err != nil || !found {
		t.Fatalf("restore recDormant: found=%v err=%v", found, err)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE recording_id=? AND deleted_at IS NULL`, recDormant); n != 1 {
		t.Errorf("recDormant surviving renditions = %d, want 1", n)
	}
	if n := visibleTagsetCount(t, db, recDormant); n < 1 {
		t.Errorf("recDormant not back in the library after restore")
	}

	if found, err := db.RestoreRecording(ctx, 99999); err != nil || found {
		t.Errorf("unknown recording: found=%v err=%v, want (false, nil)", found, err)
	}
	assertInvariants(t, db)
}

func TestBulkRestoreRecordings(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	recTrashed, recDormant, _, _ := buildTrashScenario(t, db)

	restored, err := db.BulkRestoreRecordings(ctx, []int64{recTrashed, recDormant, 99999})
	if err != nil {
		t.Fatalf("bulk restore: %v", err)
	}
	if restored != 2 {
		t.Errorf("restored = %d, want 2 (unknown skipped)", restored)
	}
	if n, err := db.CountTrashedRecordings(ctx, ""); err != nil || n != 0 {
		t.Errorf("trashed bin after bulk restore = %d (err %v), want 0", n, err)
	}
	assertInvariants(t, db)
}

func TestBulkHardDeleteRecordings(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	recTrashed, recDormant, recLive, _ := buildTrashScenario(t, db)

	deleted, blobs, err := db.BulkHardDeleteRecordings(ctx, []int64{recTrashed, recDormant, 99999})
	if err != nil {
		t.Fatalf("bulk hard delete: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2 (unknown skipped)", deleted)
	}
	// recTrashed had 2 live blobs, recDormant 1 removed blob → 3 reclaimed.
	if len(blobs) != 3 {
		t.Errorf("reclaimed blobs = %d, want 3", len(blobs))
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id IN (?,?)`, recTrashed, recDormant); n != 0 {
		t.Errorf("hard-deleted recordings survived")
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, recLive); n != 1 {
		t.Errorf("live recording was collateral-deleted")
	}
	assertInvariants(t, db)
}
