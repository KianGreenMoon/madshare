package database

import (
	"context"
	"testing"
)

// Files perspective of Trash (soft-delete.md): removed-blob listing + the
// per-file permanent delete (non-last repoints live appearances and keeps the
// recording; last file cascade-prunes the whole recording).

// TestListRemovedFiles: the listing/count/id-resolver return only soft-removed
// blobs (files.deleted_at), never live ones, and honour the search filter.
func TestListRemovedFiles(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("rf1"), "a.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("rf2"), "b.mp3", "The Band", "Best Of")
	groupIntoRecording(t, db, f1.ID, f2.ID) // f1 live, f2 a second rendition
	if found, err := db.RemoveRendition(ctx, f2.ID); err != nil || !found {
		t.Fatalf("remove f2: found=%v err=%v", found, err)
	}
	f3 := insertTaggedFile(t, db, hash64("rf3"), "solo.mp3", "Solo Act", "Single")
	if found, err := db.RemoveRendition(ctx, f3.ID); err != nil || !found {
		t.Fatalf("remove f3: found=%v err=%v", found, err)
	}

	page, err := db.ListRemovedFilesPage(ctx, FileListQuery{})
	if err != nil {
		t.Fatalf("list removed: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("removed files = %d, want 2 (f2, f3); f1 is live", len(page))
	}
	if n, err := db.CountRemovedFiles(ctx, FileFilter{}); err != nil || n != 2 {
		t.Errorf("count removed = %d (err %v), want 2", n, err)
	}
	ids, err := db.RemovedFileIDsByFilter(ctx, FileFilter{})
	if err != nil {
		t.Fatalf("removed ids: %v", err)
	}
	if len(ids) != 2 || ids[0] != f2.ID || ids[1] != f3.ID {
		t.Errorf("removed ids = %v, want [%d %d]", ids, f2.ID, f3.ID)
	}

	// Filter narrows to the matching removed blob only.
	if n, err := db.CountRemovedFiles(ctx, FileFilter{Q: "solo act", QField: "artist"}); err != nil || n != 1 {
		t.Errorf("filtered count = %d (err %v), want 1", n, err)
	}
}

// TestHardDeleteRemovedFile_NonLastKeepsAppearance: purging a non-last removed
// rendition drops only its blob; its (live, approved) appearance survives with
// a NULL provenance pointer — nothing that was playable is destroyed.
func TestHardDeleteRemovedFile_NonLastKeepsAppearance(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	h2 := hash64("nl2")
	f1 := insertTaggedFile(t, db, hash64("nl1"), "studio.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, h2, "bestof.mp3", "The Band", "Best Of")
	rec := groupIntoRecording(t, db, f1.ID, f2.ID)
	if found, err := db.RemoveRendition(ctx, f2.ID); err != nil || !found {
		t.Fatalf("remove f2: found=%v err=%v", found, err)
	}
	t2 := tagsetOfFile(t, db, f2.ID) // f2's appearance (origin_file_id = f2), still live

	blobs, found, err := db.HardDeleteRemovedFile(ctx, f2.ID)
	if err != nil || !found {
		t.Fatalf("hard delete f2: found=%v err=%v", found, err)
	}
	if len(blobs) != 1 || blobs[0].Hash != h2 {
		t.Errorf("blobs = %+v, want one with hash %s", blobs, h2)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id=?`, f2.ID); n != 0 {
		t.Errorf("f2 row survived")
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id=?`, f1.ID); n != 1 {
		t.Errorf("surviving rendition f1 was removed")
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, rec); n != 1 {
		t.Errorf("recording was pruned but a rendition survives")
	}
	// The appearance survives live, its provenance pointer cleared.
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE id=? AND origin_file_id IS NULL AND deleted_at IS NULL`, t2); n != 1 {
		t.Errorf("appearance not preserved live with NULL origin")
	}
	if n := visibleTagsetCount(t, db, rec); n != 2 {
		t.Errorf("visible appearances = %d, want 2 (both play f1)", n)
	}
	assertInvariants(t, db)
}

// TestHardDeleteRemovedFile_LastTrashesAppearance: purging the last file of a
// recording destroys only the bytes; the recording's appearance is TRASHED by
// the scoped reap (GC model: demote, never destroy) and stays reachable in
// Trash › Appearances, restorable or purgeable from there.
func TestHardDeleteRemovedFile_LastTrashesAppearance(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	h := hash64("lc1")
	f := insertTaggedFile(t, db, h, "solo.mp3", "Solo Act", "Single")
	rec := recordingIDOf(t, db, f.ID)
	if found, err := db.RemoveRendition(ctx, f.ID); err != nil || !found {
		t.Fatalf("remove f: found=%v err=%v", found, err)
	}

	blobs, found, err := db.HardDeleteRemovedFile(ctx, f.ID)
	if err != nil || !found {
		t.Fatalf("hard delete last file: found=%v err=%v", found, err)
	}
	if len(blobs) != 1 || blobs[0].Hash != h {
		t.Errorf("blobs = %+v, want one with hash %s", blobs, h)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id=?`, f.ID); n != 0 {
		t.Errorf("file row survived")
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, rec); n != 1 {
		t.Errorf("recording destroyed by last-file delete, want it kept (its appearance references it)")
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=? AND deleted_at IS NOT NULL`, rec); n != 1 {
		t.Errorf("appearance not trashed by the scoped reap")
	}
	assertInvariants(t, db)
}

// TestHardDeleteRemovedFile_RefusesLiveOrUnknown: a live file is not in the
// Files bucket (found=false, untouched); an unknown id is likewise found=false.
func TestHardDeleteRemovedFile_RefusesLiveOrUnknown(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f := insertTaggedFile(t, db, hash64("lv1"), "live.mp3", "Live Act", "Album")

	blobs, found, err := db.HardDeleteRemovedFile(ctx, f.ID)
	if err != nil || found || blobs != nil {
		t.Errorf("live file: found=%v blobs=%v err=%v, want (false, nil, nil)", found, blobs, err)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id=?`, f.ID); n != 1 {
		t.Errorf("live file was deleted")
	}
	if _, found, err := db.HardDeleteRemovedFile(ctx, 99999); err != nil || found {
		t.Errorf("unknown id: found=%v err=%v, want (false, nil)", found, err)
	}
	assertInvariants(t, db)
}

// TestBulkHardDeleteRemovedFiles: one transaction over a mix — a non-last purge,
// a last-file purge (appearance demoted to Trash), plus a live and an unknown
// id (both skipped).
func TestBulkHardDeleteRemovedFiles(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// Recording A: two renditions, f2 removed (non-last purge).
	f1 := insertTaggedFile(t, db, hash64("ba1"), "a.flac", "The Band", "Studio")
	f2 := insertTaggedFile(t, db, hash64("ba2"), "b.mp3", "The Band", "Best Of")
	recA := groupIntoRecording(t, db, f1.ID, f2.ID)
	if found, err := db.RemoveRendition(ctx, f2.ID); err != nil || !found {
		t.Fatalf("remove f2: %v", err)
	}
	// Recording B: single rendition f3 removed (last-file cascade).
	f3 := insertTaggedFile(t, db, hash64("ba3"), "solo.mp3", "Solo", "Single")
	recB := recordingIDOf(t, db, f3.ID)
	if found, err := db.RemoveRendition(ctx, f3.ID); err != nil || !found {
		t.Fatalf("remove f3: %v", err)
	}
	// f4 live (skipped).
	f4 := insertTaggedFile(t, db, hash64("ba4"), "live.mp3", "Live", "Album")

	deleted, blobs, err := db.BulkHardDeleteRemovedFiles(ctx, []int64{f2.ID, f3.ID, f4.ID, 99999})
	if err != nil {
		t.Fatalf("bulk: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2 (f2, f3; live + unknown skipped)", deleted)
	}
	if len(blobs) != 2 {
		t.Errorf("blobs = %d, want 2", len(blobs))
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id IN (?,?)`, f2.ID, f3.ID); n != 0 {
		t.Errorf("removed files survived bulk purge")
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, recB); n != 1 {
		t.Errorf("recording B destroyed, want it kept with its appearance in Trash")
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=? AND deleted_at IS NOT NULL`, recB); n != 1 {
		t.Errorf("recording B's appearance not trashed by the scoped reap")
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, recA); n != 1 {
		t.Errorf("recording A pruned but f1 survives")
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id=?`, f4.ID); n != 1 {
		t.Errorf("live f4 was purged")
	}
	assertInvariants(t, db)
}
