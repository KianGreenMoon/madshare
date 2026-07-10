package database

// Trash · Appearances lens, tagset-rooted (recording-tagsets P7c).

import (
	"context"
	"testing"
)

// trashAppearance soft-deletes one appearance by tagset id (the sibling helper
// trashTagset in trash_recordings_test.go is keyed by *file* id).
func trashAppearance(t *testing.T, db *DB, tagsetID int64) {
	t.Helper()
	if _, err := db.ExecContext(context.Background(),
		`UPDATE tagsets SET deleted_at = 1700000000 WHERE id = ?`, tagsetID); err != nil {
		t.Fatalf("trash tagset %d: %v", tagsetID, err)
	}
}

// TestTrashLens_ListsNonRepresentativeAppearance is the blind spot the lens was
// built to close. A blob carries two appearances (the byte-dup shape); the
// non-primary one is trashed while the primary stays live. The old files-rooted
// listing joined only the blob's *representative* appearance (primary, else
// oldest), so this row was listed nowhere.
func TestTrashLens_ListsNonRepresentativeAppearance(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f := insertTaggedFile(t, db, hash64("tl-1"), "studio.flac", "The Band", "Studio Album")
	rec := recordingIDOf(t, db, f.ID)

	// A second appearance on the same blob (a different release), non-primary.
	var draftID int64
	if err := db.QueryRowContext(ctx,
		`INSERT INTO tagsets (recording_id, title, artist, album, review_state, origin_file_id, is_primary, created_at)
		 VALUES (?, 'Same Song', 'The Band', 'Best Of', 'approved', ?, 0, 1700000000) RETURNING id`,
		rec, f.ID).Scan(&draftID); err != nil {
		t.Fatalf("second appearance: %v", err)
	}
	trashAppearance(t, db, draftID)

	rows, err := db.ListTrashedAppearancesPage(ctx, FileListQuery{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].TagsetID != draftID {
		t.Fatalf("listed %d row(s) %v, want exactly the trashed non-representative appearance #%d",
			len(rows), rows, draftID)
	}
	if rows[0].Album != "Best Of" {
		t.Errorf("row album = %q, want the trashed appearance's own album %q", rows[0].Album, "Best Of")
	}
	if n, err := db.CountTrashedAppearances(ctx, FileFilter{}); err != nil || n != 1 {
		t.Errorf("count = %d (err %v), want 1", n, err)
	}

	// The live primary appearance must NOT be listed, and the blob survives.
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE deleted_at IS NULL AND recording_id=?`, rec); n != 1 {
		t.Errorf("live appearances = %d, want 1", n)
	}
}

// TestTrashLens_ListsBloblessAppearance: an appearance whose origin blob was
// purged (origin_file_id SET NULL) still belongs in Trash. The old lens joined
// FROM files, so it could not list — or address — such a row at all.
func TestTrashLens_ListsBloblessAppearance(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f := insertTaggedFile(t, db, hash64("tl-2"), "gone.flac", "The Band", "Studio Album")
	tid := tagsetOfFile(t, db, f.ID)
	if _, err := db.ExecContext(ctx, `UPDATE tagsets SET origin_file_id = NULL WHERE id = ?`, tid); err != nil {
		t.Fatal(err)
	}
	trashAppearance(t, db, tid)

	rows, err := db.ListTrashedAppearancesPage(ctx, FileListQuery{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("listed %d row(s), want the blobless trashed appearance", len(rows))
	}
	if rows[0].Hash != "" || rows[0].FileID.Valid {
		t.Errorf("blobless row carried a blob: hash=%q file_id=%v", rows[0].Hash, rows[0].FileID)
	}
	if rows[0].Title != "gone.flac" {
		t.Errorf("title = %q, want the appearance's own title", rows[0].Title)
	}
}

// TestTrashLens_BulkRestoreAndDeleteAreTagsetScoped pins the second half of the
// bug: the hash-addressed actions restored/deleted *every* trashed appearance of
// a blob while the UI showed one row. Tagset-addressed ops touch exactly one.
func TestTrashLens_BulkRestoreAndDeleteAreTagsetScoped(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f := insertTaggedFile(t, db, hash64("tl-3"), "studio.flac", "The Band", "Studio Album")
	rec := recordingIDOf(t, db, f.ID)
	primary := tagsetOfFile(t, db, f.ID)

	var second int64
	if err := db.QueryRowContext(ctx,
		`INSERT INTO tagsets (recording_id, title, artist, album, review_state, origin_file_id, is_primary, created_at)
		 VALUES (?, 'Same Song', 'The Band', 'Best Of', 'approved', ?, 0, 1700000000) RETURNING id`,
		rec, f.ID).Scan(&second); err != nil {
		t.Fatal(err)
	}
	// Both appearances of the one blob go to Trash.
	trashAppearance(t, db, primary)
	trashAppearance(t, db, second)

	// Restore only one of them.
	n, err := db.BulkRestoreTagsets(ctx, []int64{second})
	if err != nil || n != 1 {
		t.Fatalf("BulkRestoreTagsets = %d (err %v), want 1", n, err)
	}
	if countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE deleted_at IS NULL`) != 1 {
		t.Error("restore touched more than the one appearance it was given")
	}
	if countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE id=? AND deleted_at IS NOT NULL`, primary) != 1 {
		t.Error("the sibling appearance was restored too — the op is not tagset-scoped")
	}

	// Permanently delete the still-trashed one. It is not the last appearance of
	// the recording, so the blob must survive.
	deleted, blobs, err := db.BulkHardDeleteTagsets(ctx, []int64{primary})
	if err != nil || deleted != 1 {
		t.Fatalf("BulkHardDeleteTagsets = %d (err %v), want 1", deleted, err)
	}
	if len(blobs) != 0 {
		t.Errorf("reclaimed %d blob(s); a non-last appearance must keep the audio", len(blobs))
	}
	if countRow(t, db, `SELECT COUNT(*) FROM files WHERE id=?`, f.ID) != 1 {
		t.Error("the blob was deleted with a non-last appearance")
	}
	if countRow(t, db, `SELECT COUNT(*) FROM tagsets`) != 1 {
		t.Error("wrong number of appearances survived")
	}
}

// TestTrashLens_BulkHardDeleteSkipsLiveAppearances — permanent delete is
// Trash-only, mirroring the single-row HardDeleteTrashedTagset's refusal.
func TestTrashLens_BulkHardDeleteSkipsLiveAppearances(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f := insertTaggedFile(t, db, hash64("tl-4"), "live.flac", "The Band", "Studio Album")
	live := tagsetOfFile(t, db, f.ID)

	deleted, blobs, err := db.BulkHardDeleteTagsets(ctx, []int64{live, 9999})
	if err != nil {
		t.Fatalf("bulk delete: %v", err)
	}
	if deleted != 0 || len(blobs) != 0 {
		t.Errorf("deleted %d appearance(s), %d blob(s); a live appearance must be skipped", deleted, len(blobs))
	}
	if countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE id=?`, live) != 1 {
		t.Error("a live appearance was permanently deleted from the Trash bulk path")
	}
}

// TestTrashLens_BulkEditReportsUnknownIDs — applyMetadataPatchTagsetTx updates
// by primary key and reports nothing for a missing row, so the ids are checked
// up front; `affected` must not count ids that matched nothing.
func TestTrashLens_BulkEditReportsUnknownIDs(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f := insertTaggedFile(t, db, hash64("tl-5"), "typo.flac", "The Bnad", "Studio Album")
	tid := tagsetOfFile(t, db, f.ID)
	trashAppearance(t, db, tid)

	fixed := "The Band"
	affected, notFound, err := db.BulkUpdateTagsetMetadata(ctx, []int64{tid, 4242}, MetadataPatch{Artist: &fixed})
	if err != nil {
		t.Fatalf("bulk edit: %v", err)
	}
	if affected != 1 {
		t.Errorf("affected = %d, want 1 (the unknown id must not be counted)", affected)
	}
	if len(notFound) != 1 || notFound[0] != 4242 {
		t.Errorf("notFound = %v, want [4242]", notFound)
	}
	var got string
	if err := db.QueryRowContext(ctx, `SELECT artist FROM tagsets WHERE id=?`, tid).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != fixed {
		t.Errorf("artist = %q, want %q", got, fixed)
	}
}
