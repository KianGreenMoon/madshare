package database

import (
	"context"
	"testing"
)

// Cascade & GC matrix for recording-tagsets P2 (the hardlink lifecycle). These
// are the highest-risk rules in the whole feature: they decide whether curated
// metadata or stored audio is silently lost. Every mutating test ends with
// assertInvariants so no operation can leave the DB in a state the model forbids.

// assertInvariants checks the hardlink invariant across the whole DB: every
// recording has ≥1 file and ≥1 tagset (and exactly one primary appearance),
// every file has a recording, every tagset has a recording. Runs after each
// cascade/GC test so a stranded row fails loudly.
func assertInvariants(t *testing.T, db *DB) {
	t.Helper()
	check := func(q, what string) {
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatalf("invariant %s: %v", what, err)
		}
		if n != 0 {
			t.Errorf("invariant violated: %s (%d offending rows)", what, n)
		}
	}
	check(`SELECT COUNT(*) FROM recordings r WHERE NOT EXISTS (SELECT 1 FROM files f WHERE f.recording_id=r.id)`,
		"recording with no files")
	check(`SELECT COUNT(*) FROM recordings r WHERE NOT EXISTS (SELECT 1 FROM tagsets t WHERE t.recording_id=r.id)`,
		"recording with no tagsets")
	check(`SELECT COUNT(*) FROM files WHERE recording_id IS NULL`, "file with no recording")
	check(`SELECT COUNT(*) FROM tagsets WHERE recording_id IS NULL`, "tagset with no recording")
	check(`SELECT COUNT(*) FROM recordings r
	         WHERE (SELECT COUNT(*) FROM tagsets t WHERE t.recording_id=r.id AND t.is_primary=1) <> 1`,
		"recording without exactly one primary tagset")
}

// insertApproved inserts a live, approved file (artist/album non-default, so its
// appearance is meaningful and library-visible) and returns it with ID +
// RecordingID populated.
func insertApproved(t *testing.T, db *DB, hash, filename string) *File {
	t.Helper()
	f := newFile(hash)
	f.ReviewState = ReviewApproved
	if err := db.InsertFile(context.Background(), f, newUpload(filename), newMeta()); err != nil {
		t.Fatalf("InsertFile %s: %v", hash, err)
	}
	return f
}

// groupIntoRecording moves moveFileID (and its tagset) onto keepFileID's
// recording and GCs the emptied singleton — simulating the fingerprint resolver
// collapsing two same-audio uploads into one multi-rendition recording with two
// appearances (the pre-absorb duplicate case P2's cascade must handle). The kept
// file's tagset stays the primary. Returns the shared recording id.
func groupIntoRecording(t *testing.T, db *DB, keepFileID, moveFileID int64) int64 {
	t.Helper()
	var keepRec, moveRec int64
	if err := db.QueryRow(`SELECT recording_id FROM files WHERE id=?`, keepFileID).Scan(&keepRec); err != nil {
		t.Fatalf("keep recording: %v", err)
	}
	if err := db.QueryRow(`SELECT recording_id FROM files WHERE id=?`, moveFileID).Scan(&moveRec); err != nil {
		t.Fatalf("move recording: %v", err)
	}
	if _, err := db.Exec(`UPDATE files SET recording_id=? WHERE id=?`, keepRec, moveFileID); err != nil {
		t.Fatalf("regroup file: %v", err)
	}
	if _, err := db.Exec(`UPDATE tagsets SET recording_id=?, is_primary=0 WHERE origin_file_id=?`, keepRec, moveFileID); err != nil {
		t.Fatalf("regroup tagset: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM recordings WHERE id=?`, moveRec); err != nil {
		t.Fatalf("gc emptied recording: %v", err)
	}
	return keepRec
}

func countRow(t *testing.T, db *DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// visibleTagsetCount reports how many of a recording's appearances are
// library-visible (the shared visibleTagset predicate) — 0 means dormant/hidden.
func visibleTagsetCount(t *testing.T, db *DB, recID int64) int {
	t.Helper()
	return countRow(t, db,
		`SELECT COUNT(*) FROM tagsets m WHERE m.recording_id=? AND `+visibleTagset, recID)
}

// TestHardDeleteTagset_NonLastKeepsBlob is the core correctness fix: permanently
// deleting a non-last appearance of a multi-rendition recording removes only the
// tagset — the recording and every file (blob) survive, because another
// appearance may still play them.
func TestHardDeleteTagset_NonLastKeepsBlob(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertApproved(t, db, hash64("a1"), "one.flac")
	f2 := insertApproved(t, db, hash64("b2"), "two.mp3")
	rec := groupIntoRecording(t, db, f1.ID, f2.ID)

	// Trash f2's appearance, then permanently delete it.
	if _, _, err := db.SoftDeleteFileByHash(ctx, f2.Hash); err != nil {
		t.Fatalf("soft delete f2: %v", err)
	}
	_, blobs, found, err := db.HardDeleteTrashedFileByHash(ctx, f2.Hash)
	if err != nil || !found {
		t.Fatalf("hard delete f2 tagset: found=%v err=%v", found, err)
	}
	if len(blobs) != 0 {
		t.Fatalf("non-last tagset delete reclaimed %d blobs, want 0 (files must survive)", len(blobs))
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, rec); n != 1 {
		t.Errorf("recording gone after non-last tagset delete: count=%d", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id IN (?,?)`, f1.ID, f2.ID); n != 2 {
		t.Errorf("files removed after non-last tagset delete: count=%d, want 2", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE origin_file_id=?`, f2.ID); n != 0 {
		t.Errorf("f2's tagset survived: count=%d, want 0", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=?`, rec); n != 1 {
		t.Errorf("recording tagset count=%d, want 1 (f1's appearance)", n)
	}
	assertInvariants(t, db)
}

// TestHardDeleteTagset_LastCascades: deleting the LAST appearance of a recording
// takes the recording and all its files (blobs reclaimed) with it.
func TestHardDeleteTagset_LastCascades(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertApproved(t, db, hash64("c3"), "one.flac")
	f2 := insertApproved(t, db, hash64("d4"), "two.mp3")
	rec := groupIntoRecording(t, db, f1.ID, f2.ID)

	for _, h := range []string{f1.Hash, f2.Hash} {
		if _, _, err := db.SoftDeleteFileByHash(ctx, h); err != nil {
			t.Fatalf("soft delete %s: %v", h, err)
		}
	}
	deleted, blobs, err := db.BulkHardDeleteTrashedByHashes(ctx, []string{f1.Hash, f2.Hash})
	if err != nil {
		t.Fatalf("bulk hard delete: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted %d tagsets, want 2", deleted)
	}
	if len(blobs) != 2 {
		t.Errorf("reclaimed %d blobs, want 2 (recording GC took both files)", len(blobs))
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, rec); n != 0 {
		t.Errorf("recording survived last-appearance delete: count=%d", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id IN (?,?)`, f1.ID, f2.ID); n != 0 {
		t.Errorf("files survived last-appearance delete: count=%d", n)
	}
	assertInvariants(t, db)
}

// TestBulkHardDeleteTagsets_MixedLastAndNonLast runs a single bulk permanent
// delete that is a last-appearance cascade for one recording and a non-last
// tagset removal for another — in one transaction. Only the cascaded recording's
// blob comes back.
func TestBulkHardDeleteTagsets_MixedLastAndNonLast(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	fA := insertApproved(t, db, hash64("e5"), "solo.mp3") // its own recording (last)
	f1 := insertApproved(t, db, hash64("f6"), "one.flac")
	f2 := insertApproved(t, db, hash64("a7"), "two.mp3")
	recB := groupIntoRecording(t, db, f1.ID, f2.ID) // f1+f2 share recB

	for _, h := range []string{fA.Hash, f1.Hash} {
		if _, _, err := db.SoftDeleteFileByHash(ctx, h); err != nil {
			t.Fatalf("soft delete %s: %v", h, err)
		}
	}
	deleted, blobs, err := db.BulkHardDeleteTrashedByHashes(ctx, []string{fA.Hash, f1.Hash})
	if err != nil {
		t.Fatalf("bulk hard delete: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted %d tagsets, want 2", deleted)
	}
	if len(blobs) != 1 || blobs[0].Hash != fA.Hash {
		t.Errorf("blobs = %+v, want only fA (%s)", blobs, fA.Hash)
	}
	// recA (fA) fully GC'd; recB keeps both files, only f1's appearance gone.
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id=?`, fA.ID); n != 0 {
		t.Errorf("fA survived its last-appearance cascade: count=%d", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id IN (?,?)`, f1.ID, f2.ID); n != 2 {
		t.Errorf("recB files removed: count=%d, want 2", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=?`, recB); n != 1 {
		t.Errorf("recB tagset count=%d, want 1 (f2's appearance)", n)
	}
	assertInvariants(t, db)
}

// TestRestoreTagset_KeepsReviewState: soft delete never touches review_state, and
// restore returns the appearance to whatever state it was trashed from (a
// discarded submission re-enters the queue, not the library).
func TestRestoreTagset_KeepsReviewState(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f := newFile(hash64("b8"))
	f.ReviewState = ReviewSubmitted
	if err := db.InsertFile(ctx, f, newUpload("pending.mp3"), newMeta()); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}

	if _, _, err := db.SoftDeleteFileByHash(ctx, f.Hash); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	state := func() string {
		var s string
		if err := db.QueryRow(`SELECT review_state FROM tagsets WHERE origin_file_id=?`, f.ID).Scan(&s); err != nil {
			t.Fatalf("read state: %v", err)
		}
		return s
	}
	if got := state(); got != ReviewSubmitted {
		t.Errorf("trashed tagset review_state=%q, want %q", got, ReviewSubmitted)
	}
	if found, err := db.RestoreFileByHash(ctx, f.Hash); err != nil || !found {
		t.Fatalf("restore: found=%v err=%v", found, err)
	}
	if got := state(); got != ReviewSubmitted {
		t.Errorf("restored tagset review_state=%q, want %q (re-enters prior state)", got, ReviewSubmitted)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE origin_file_id=? AND deleted_at IS NULL`, f.ID); n != 1 {
		t.Errorf("restore did not clear deleted_at: live tagset count=%d", n)
	}
	assertInvariants(t, db)
}

// TestRemoveRendition_LastGoesDormantThenRestores: soft-removing the last
// surviving rendition is allowed and makes the recording dormant (hidden from
// the library, every row preserved); restoring the rendition re-surfaces it.
func TestRemoveRendition_LastGoesDormantThenRestores(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f := insertApproved(t, db, hash64("c9"), "song.mp3")
	rec := f.RecordingID
	if got := visibleTagsetCount(t, db, rec); got != 1 {
		t.Fatalf("fresh approved recording visible count=%d, want 1", got)
	}

	found, err := db.RemoveRendition(ctx, f.ID)
	if err != nil || !found {
		t.Fatalf("remove rendition: found=%v err=%v", found, err)
	}
	if got := visibleTagsetCount(t, db, rec); got != 0 {
		t.Errorf("dormant recording visible count=%d, want 0 (no surviving rendition)", got)
	}
	// Everything is preserved — dormant, not deleted.
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id=? AND deleted_at IS NOT NULL`, f.ID); n != 1 {
		t.Errorf("rendition row not soft-removed: count=%d", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=?`, rec); n != 1 {
		t.Errorf("dormant recording lost its appearance: tagset count=%d", n)
	}
	assertInvariants(t, db)

	found, err = db.RestoreRendition(ctx, f.ID)
	if err != nil || !found {
		t.Fatalf("restore rendition: found=%v err=%v", found, err)
	}
	if got := visibleTagsetCount(t, db, rec); got != 1 {
		t.Errorf("restored recording visible count=%d, want 1", got)
	}
	assertInvariants(t, db)
}

// TestRemoveRendition_NonLastLeavesRecordingVisible: removing one of several
// renditions keeps the recording live (another blob still plays).
func TestRemoveRendition_NonLastLeavesRecordingVisible(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertApproved(t, db, hash64("d1"), "one.flac")
	f2 := insertApproved(t, db, hash64("e2"), "two.mp3")
	rec := groupIntoRecording(t, db, f1.ID, f2.ID)

	if found, err := db.RemoveRendition(ctx, f2.ID); err != nil || !found {
		t.Fatalf("remove rendition f2: found=%v err=%v", found, err)
	}
	// f1 still plays → the recording stays live, so BOTH of its appearances
	// remain library-visible (they play the surviving blob). Not dormant.
	if got := visibleTagsetCount(t, db, rec); got != 2 {
		t.Errorf("recording visible count=%d after non-last rendition removal, want 2", got)
	}
	assertInvariants(t, db)
}

// TestBulkRemoveRenditions: the Files lens's "Remove selected" — one guarded
// UPDATE over the id list; already-removed and unknown ids are no-ops.
func TestBulkRemoveRenditions(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertApproved(t, db, hash64("b5"), "one.flac")
	f2 := insertApproved(t, db, hash64("c6"), "two.mp3")
	f3 := insertApproved(t, db, hash64("d7"), "three.ogg")

	// f3 is already removed; 99999 doesn't exist — neither counts.
	if found, err := db.RemoveRendition(ctx, f3.ID); err != nil || !found {
		t.Fatalf("pre-remove f3: found=%v err=%v", found, err)
	}
	// LiveFileIDs is the "select all N" target set: exactly the non-removed ids.
	if ids, err := db.LiveFileIDs(ctx); err != nil || len(ids) != 2 || ids[0] != f1.ID || ids[1] != f2.ID {
		t.Errorf("LiveFileIDs = %v (err %v), want [%d %d]", ids, err, f1.ID, f2.ID)
	}
	n, err := db.BulkRemoveRenditions(ctx, []int64{f1.ID, f2.ID, f3.ID, 99999})
	if err != nil {
		t.Fatalf("bulk remove: %v", err)
	}
	if n != 2 {
		t.Errorf("bulk remove count=%d, want 2 (already-removed + unknown are no-ops)", n)
	}
	if live := countRow(t, db, `SELECT COUNT(*) FROM files WHERE deleted_at IS NULL`); live != 0 {
		t.Errorf("live file count=%d after bulk remove, want 0", live)
	}
	assertInvariants(t, db)

	// Empty list is a no-op, not an error.
	if n, err := db.BulkRemoveRenditions(ctx, nil); err != nil || n != 0 {
		t.Errorf("empty bulk remove: n=%d err=%v, want 0/nil", n, err)
	}
}

// TestHardDeleteFile_LastFileCascades exercises the file-side (prune) direction:
// removing a non-last file leaves the recording; removing the last file cascades
// to the recording and all its appearances — symmetric with the tagset side.
func TestHardDeleteFile_LastFileCascades(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertApproved(t, db, hash64("f3"), "one.flac")
	f2 := insertApproved(t, db, hash64("a4"), "two.mp3")
	rec := groupIntoRecording(t, db, f1.ID, f2.ID)

	if _, found, err := db.HardDeleteFileByHash(ctx, f1.Hash); err != nil || !found {
		t.Fatalf("hard delete f1: found=%v err=%v", found, err)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, rec); n != 1 {
		t.Errorf("recording gone after non-last file delete: count=%d", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE recording_id=?`, rec); n != 1 {
		t.Errorf("recording file count=%d after non-last file delete, want 1", n)
	}
	assertInvariants(t, db)

	if _, found, err := db.HardDeleteFileByHash(ctx, f2.Hash); err != nil || !found {
		t.Fatalf("hard delete f2 (last): found=%v err=%v", found, err)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, rec); n != 0 {
		t.Errorf("recording survived last-file delete: count=%d", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=?`, rec); n != 0 {
		t.Errorf("appearances survived last-file delete: count=%d", n)
	}
	assertInvariants(t, db)
}

// TestSweepInvalidRecordings GCs a fileless recording (and its orphaned
// appearance) while leaving healthy recordings untouched — the prune backstop.
func TestSweepInvalidRecordings(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	healthy := insertApproved(t, db, hash64("b5"), "keep.mp3")

	// Craft a fileless recording with a dangling appearance (a crash orphan the
	// cascade paths never produce, but the backstop must clean up).
	var badRec int64
	if err := db.QueryRow(`INSERT INTO recordings (created_at) VALUES (1700000000) RETURNING id`).Scan(&badRec); err != nil {
		t.Fatalf("insert orphan recording: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO tagsets (recording_id, title, review_state, is_primary, created_at) VALUES (?, 'orphan', 'approved', 1, 1700000000)`,
		badRec); err != nil {
		t.Fatalf("insert orphan tagset: %v", err)
	}

	removed, err := db.SweepInvalidRecordings(ctx)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 1 {
		t.Errorf("swept %d recordings, want 1", removed)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, badRec); n != 0 {
		t.Errorf("orphan recording survived sweep: count=%d", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=?`, badRec); n != 0 {
		t.Errorf("orphan appearance survived sweep (FK cascade): count=%d", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, healthy.RecordingID); n != 1 {
		t.Errorf("healthy recording swept: count=%d", n)
	}
	assertInvariants(t, db)
}
