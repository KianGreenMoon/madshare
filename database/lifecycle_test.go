package database

import (
	"context"
	"database/sql"
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
	// Structural edges (NOT NULL, trigger-backed since 024).
	check(`SELECT COUNT(*) FROM files WHERE recording_id IS NULL`, "file with no recording")
	check(`SELECT COUNT(*) FROM tagsets WHERE recording_id IS NULL`, "tagset with no recording")
	// GC-model convergence (docs/architecture/gc-model.md): no garbage awaits
	// collection. A zero-tagset recording may only retain trashed (quarantined)
	// files, a zero-file recording only trashed appearances, and an empty husk
	// may not exist. Call after ops that keep the library converged (the
	// cascade-era write paths do; otherwise Reap first).
	check(`SELECT COUNT(*) FROM recordings r
	         WHERE NOT EXISTS (SELECT 1 FROM tagsets t WHERE t.recording_id=r.id)
	           AND EXISTS (SELECT 1 FROM files f WHERE f.recording_id=r.id AND f.deleted_at IS NULL)`,
		"appearance-less recording retaining live files (unreaped)")
	check(`SELECT COUNT(*) FROM recordings r
	         WHERE NOT EXISTS (SELECT 1 FROM files f WHERE f.recording_id=r.id)
	           AND EXISTS (SELECT 1 FROM tagsets t WHERE t.recording_id=r.id AND t.deleted_at IS NULL)`,
		"file-less recording retaining live appearances (unreaped)")
	check(`SELECT COUNT(*) FROM recordings r
	         WHERE NOT EXISTS (SELECT 1 FROM tagsets t WHERE t.recording_id=r.id)
	           AND NOT EXISTS (SELECT 1 FROM files f WHERE f.recording_id=r.id)`,
		"empty recording husk (unreaped)")
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
	trashAppearancesByHash(t, db, f2.Hash)
	ids := trashedTagsetIDsByHash(t, db, f2.Hash)
	if len(ids) != 1 {
		t.Fatalf("trashed tagsets of f2 = %d, want 1", len(ids))
	}
	out, err := db.HardDeleteTrashedTagset(ctx, ids[0])
	if err != nil || !out.Found || !out.Trashed {
		t.Fatalf("hard delete f2 tagset: out=%+v err=%v", out, err)
	}
	if len(out.Blobs) != 0 {
		t.Fatalf("non-last tagset delete reclaimed %d blobs, want 0 (files must survive)", len(out.Blobs))
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
		trashAppearancesByHash(t, db, h)
	}
	deleted, blobs, err := db.BulkHardDeleteTagsets(ctx, trashedTagsetIDsByHash(t, db, f1.Hash, f2.Hash))
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
		trashAppearancesByHash(t, db, h)
	}
	deleted, blobs, err := db.BulkHardDeleteTagsets(ctx, trashedTagsetIDsByHash(t, db, fA.Hash, f1.Hash))
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

	trashAppearancesByHash(t, db, f.Hash)
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

// TestHardDeleteFile_LastFileTrashesAppearances exercises the file-side
// (prune blob-loss) direction under the GC model: removing a non-last file
// leaves the recording; removing the last file leaves the recording file-less,
// so the scoped reap TRASHES its appearances (demote, never destroy) — the
// catalog entries survive blobless in Trash › Appearances.
func TestHardDeleteFile_LastFileTrashesAppearances(t *testing.T) {
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
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, rec); n != 1 {
		t.Errorf("recording destroyed by last-file delete: count=%d, want 1 (appearances still reference it)", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=? AND deleted_at IS NOT NULL`, rec); n != 2 {
		t.Errorf("trashed appearances = %d after last-file delete, want 2 (demoted, not destroyed)", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=? AND deleted_at IS NULL`, rec); n != 0 {
		t.Errorf("live appearances = %d after last-file delete, want 0", n)
	}
	assertInvariants(t, db)
}

// TestReap_FilelessRecordingIsQuarantinedNotDestroyed: the reaper's safety
// invariant (GC model) — a crash-orphaned fileless recording has its dangling
// appearance TRASHED (restorable), never deleted; the recording husk only
// falls once its last row is purged. Healthy recordings are untouched.
func TestReap_FilelessRecordingIsQuarantinedNotDestroyed(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	healthy := insertApproved(t, db, hash64("b5"), "keep.mp3")

	// Craft a fileless recording with a dangling appearance (a crash orphan the
	// cascade paths never produce, but the backstop must collect).
	var badRec int64
	if err := db.QueryRow(`INSERT INTO recordings (created_at) VALUES (1700000000) RETURNING id`).Scan(&badRec); err != nil {
		t.Fatalf("insert orphan recording: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO tagsets (recording_id, title, review_state, is_primary, created_at) VALUES (?, 'orphan', 'approved', 1, 1700000000)`,
		badRec); err != nil {
		t.Fatalf("insert orphan tagset: %v", err)
	}

	stats, err := db.Reap(ctx)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if stats.TrashedTagsets != 1 || stats.QuarantinedFiles != 0 || stats.DeletedHusks != 0 {
		t.Errorf("reap stats = %+v, want exactly 1 trashed tagset", stats)
	}
	// Demoted, not destroyed: the appearance sits in Trash, the husk remains.
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=? AND deleted_at IS NOT NULL`, badRec); n != 1 {
		t.Errorf("orphan appearance not quarantined: trashed count=%d, want 1", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, badRec); n != 1 {
		t.Errorf("recording husk deleted while a trashed row still references it: count=%d", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, healthy.RecordingID); n != 1 {
		t.Errorf("healthy recording reaped: count=%d", n)
	}
	// Idempotent: a converged library is a no-op.
	if stats, _ := db.Reap(ctx); stats.Total() != 0 {
		t.Errorf("second reap collected %+v, want nothing (idempotent)", stats)
	}

	// Purge the trashed appearance (raw row delete = what purge does) → the
	// empty husk is collected on the next reap.
	if _, err := db.Exec(`DELETE FROM tagsets WHERE recording_id=?`, badRec); err != nil {
		t.Fatalf("purge trashed tagset: %v", err)
	}
	stats, err = db.Reap(ctx)
	if err != nil {
		t.Fatalf("reap after purge: %v", err)
	}
	if stats.DeletedHusks != 1 {
		t.Errorf("reap stats after purge = %+v, want 1 deleted husk", stats)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, badRec); n != 0 {
		t.Errorf("empty husk survived reap: count=%d", n)
	}
	assertInvariants(t, db)
}

// originOf reads a tagset's origin_file_id (0 when NULL).
func originOf(t *testing.T, db *DB, tagsetID int64) int64 {
	t.Helper()
	var origin sql.NullInt64
	if err := db.QueryRow(`SELECT origin_file_id FROM tagsets WHERE id=?`, tagsetID).Scan(&origin); err != nil {
		t.Fatalf("origin of tagset %d: %v", tagsetID, err)
	}
	return origin.Int64
}

// TestHardDeleteFile_PreservesAbsorbedAppearance: prune of an absorbed blob
// must not destroy the appearance absorb preserved. Two releases of the same
// audio are absorbed down to one blob; the absorbed (soft-removed) blob then
// goes corrupt and prune hard-deletes it — its appearance survives with a NULL
// provenance pointer (GC model: origin_file_id is inert after approval, never
// re-pointed and never a delete key).
func TestHardDeleteFile_PreservesAbsorbedAppearance(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("rp1"), "studio.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("rp2"), "bestof.mp3", "The Band", "Best Of")
	rec := groupIntoRecording(t, db, f1.ID, f2.ID)
	if _, err := db.AbsorbRenditions(ctx, rec, f1.ID, []int64{f2.ID}); err != nil {
		t.Fatalf("absorb: %v", err)
	}

	if _, found, err := db.HardDeleteFileByHash(ctx, f2.Hash); err != nil || !found {
		t.Fatalf("hard delete absorbed blob: found=%v err=%v", found, err)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=?`, rec); n != 2 {
		t.Errorf("appearances = %d after pruning the absorbed blob, want 2 (both releases preserved)", n)
	}
	if got := visibleTagsetCount(t, db, rec); got != 2 {
		t.Errorf("visible appearances = %d, want 2", got)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=? AND origin_file_id IS NULL`, rec); n != 1 {
		t.Errorf("appearances with NULL origin = %d, want 1 (provenance cleared, not re-pointed)", n)
	}
	assertInvariants(t, db)
}

// TestHardDeleteFile_OrphanSurvivorKeepsAppearance: identical-identity absorb
// leaves the surviving rendition an orphan (its own appearance was deduped);
// pruning the origin blob of the recording's ONLY appearance must keep that
// appearance alive (origin NULL — blobless is legal), not strand the recording
// with a file and zero tagsets.
func TestHardDeleteFile_OrphanSurvivorKeepsAppearance(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("os1"), "track.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("os2"), "track.mp3", "The Band", "Studio Album")
	rec := groupIntoRecording(t, db, f1.ID, f2.ID)
	out, err := db.AbsorbRenditions(ctx, rec, f1.ID, []int64{f2.ID})
	if err != nil {
		t.Fatalf("absorb: %v", err)
	}
	if out.AppearancesDropped != 1 {
		t.Fatalf("absorb dropped %d appearances, want 1 (identical identity)", out.AppearancesDropped)
	}

	// f1 now hosts the only appearance; f2 is a soft-removed orphan rendition.
	// Prune f1 (blob loss): the appearance survives blobless (origin NULL); the
	// recording keeps 1 file row + 1 tagset.
	if _, found, err := db.HardDeleteFileByHash(ctx, f1.Hash); err != nil || !found {
		t.Fatalf("hard delete appearance origin: found=%v err=%v", found, err)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=?`, rec); n != 1 {
		t.Errorf("appearances = %d after pruning the origin, want 1 (preserved, not destroyed)", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=? AND origin_file_id IS NULL AND deleted_at IS NULL`, rec); n != 1 {
		t.Errorf("appearance not preserved live with NULL origin")
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE recording_id=?`, rec); n != 1 {
		t.Errorf("recording file count=%d, want 1", n)
	}
	assertInvariants(t, db)
}

// TestHardDeleteFile_MovedAppearanceSurvives: an appearance moved to another
// recording (MoveTagset) whose origin file stays behind must survive that
// file's hard delete — provenance cleared to NULL, the appearance itself
// untouched (it belongs to its recording, not to its origin blob).
func TestHardDeleteFile_MovedAppearanceSurvives(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	fA1 := insertTaggedFile(t, db, hash64("mv1"), "a1.flac", "Artist A", "Album A")
	fA2 := insertTaggedFile(t, db, hash64("mv2"), "a2.mp3", "Artist A", "Live Album")
	recA := groupIntoRecording(t, db, fA1.ID, fA2.ID)
	fB := insertTaggedFile(t, db, hash64("mv3"), "b1.flac", "Artist B", "Album B")
	recB := fB.RecordingID

	// Move fA2's appearance onto recording B (its origin file stays on A).
	var movedTagset int64
	if err := db.QueryRow(`SELECT id FROM tagsets WHERE origin_file_id=?`, fA2.ID).Scan(&movedTagset); err != nil {
		t.Fatalf("moved tagset id: %v", err)
	}
	if _, err := db.Exec(`UPDATE tagsets SET recording_id=?, is_primary=0 WHERE id=?`, recB, movedTagset); err != nil {
		t.Fatalf("move tagset: %v", err)
	}

	if _, found, err := db.HardDeleteFileByHash(ctx, fA2.Hash); err != nil || !found {
		t.Fatalf("hard delete fA2: found=%v err=%v", found, err)
	}
	// The moved appearance lives on, its provenance pointer cleared.
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE id=? AND deleted_at IS NULL`, movedTagset); n != 1 {
		t.Fatalf("moved appearance destroyed or trashed by its origin file's hard delete")
	}
	if got := originOf(t, db, movedTagset); got != 0 {
		t.Errorf("moved appearance origin=%d, want NULL (provenance inert, never re-pointed)", got)
	}
	// Recording A keeps its remaining rendition + appearance.
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE recording_id=?`, recA); n != 1 {
		t.Errorf("recording A file count=%d, want 1", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=?`, recA); n != 1 {
		t.Errorf("recording A appearance count=%d, want 1", n)
	}
	assertInvariants(t, db)
}
