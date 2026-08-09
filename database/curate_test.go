package database

import (
	"context"
	"database/sql"
	"slices"
	"testing"
)

// Recording curation (recording-tagsets P5): merge, appearance move /
// set-primary, whole-recording trash + hard delete, access edit, listing.

// TestMergeRecordings_UnionDedupAndPin is the headline merge: two sources fold
// into the target — the distinct appearance moves (demoted from primary), the
// duplicate-identity appearance is dropped (the target's copy wins), all
// renditions move and are pinned, the sources vanish, the target keeps its
// primary.
func TestMergeRecordings_UnionDedupAndPin(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("mg1"), "studio.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("mg2"), "bestof.mp3", "The Band", "Best Of")       // distinct → moves
	f3 := insertTaggedFile(t, db, hash64("mg3"), "reissue.mp3", "The Band", "Studio Album") // dup identity → dropped
	target := recordingIDOf(t, db, f1.ID)
	src2 := recordingIDOf(t, db, f2.ID)
	src3 := recordingIDOf(t, db, f3.ID)

	out, err := db.MergeRecordings(ctx, target, []int64{src2, src3})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	want := MergeOutcome{Found: true, SourcesMerged: 2, RenditionsMoved: 2, AppearancesMoved: 1, AppearancesDropped: 1}
	if out != want {
		t.Errorf("outcome = %+v, want %+v", out, want)
	}
	// Sources gone; every file on the target, moved ones pinned.
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id IN (?,?)`, src2, src3); n != 0 {
		t.Errorf("source recordings survived: %d", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE recording_id=?`, target); n != 3 {
		t.Errorf("target files = %d, want 3", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id IN (?,?) AND recording_pinned=1`, f2.ID, f3.ID); n != 2 {
		t.Errorf("moved renditions not pinned: %d of 2", n)
	}
	// Appearances: target's original + f2's moved one.
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=?`, target); n != 2 {
		t.Errorf("target appearances = %d, want 2", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE origin_file_id=?`, f1.ID); n != 1 {
		t.Errorf("target lost its own appearance")
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE origin_file_id=?`, f3.ID); n != 0 {
		t.Errorf("duplicate-identity appearance survived")
	}
	assertInvariants(t, db)
}

// TestMergeRecordings_StaleSelection: unknown sources, an empty set, or the
// target ticked as a source are stale selections — Found=false, nothing changes.
func TestMergeRecordings_StaleSelection(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("ms1"), "a.mp3", "A", "One")
	target := recordingIDOf(t, db, f1.ID)

	for name, srcs := range map[string][]int64{
		"unknown source": {99999},
		"empty set":      {},
		"self-merge":     {target},
	} {
		out, err := db.MergeRecordings(ctx, target, srcs)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if out.Found {
			t.Errorf("%s: Found=true, want stale-selection refusal", name)
		}
	}
	assertInvariants(t, db)
}

// TestMoveTagset covers the outcome matrix: a clean move (source primary
// re-promoted), the identity collision, the last-appearance refusal, the
// same-recording no-op, and the unknown target.
func TestMoveTagset(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// Source recording with two appearances (f1 primary, f2 moved in).
	f1 := insertTaggedFile(t, db, hash64("mt1"), "a.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("mt2"), "b.mp3", "The Band", "Best Of")
	src := groupIntoRecording(t, db, f1.ID, f2.ID)
	// Target: a separate recording.
	f3 := insertTaggedFile(t, db, hash64("mt3"), "c.mp3", "Other Act", "Elsewhere")
	target := recordingIDOf(t, db, f3.ID)
	moving := tagsetOfFile(t, db, f2.ID)

	// Unknown target / unknown tagset.
	if out, err := db.MoveTagset(ctx, moving, 99999); err != nil || out.Found {
		t.Errorf("unknown target: out=%+v err=%v, want not-found", out, err)
	}
	if out, err := db.MoveTagset(ctx, 99999, target); err != nil || out.Found {
		t.Errorf("unknown tagset: out=%+v err=%v, want not-found", out, err)
	}
	// Same recording: no-op outcome.
	if out, err := db.MoveTagset(ctx, moving, src); err != nil || !out.SameRecording {
		t.Errorf("same recording: out=%+v err=%v, want SameRecording", out, err)
	}

	// Clean move.
	out, err := db.MoveTagset(ctx, moving, target)
	if err != nil {
		t.Fatalf("move: %v", err)
	}
	if !out.Moved {
		t.Fatalf("move refused: %+v", out)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE id=? AND recording_id=? AND is_primary=0`, moving, target); n != 1 {
		t.Errorf("tagset not moved (or arrived primary)")
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=?`, src); n != 1 {
		t.Errorf("source appearance count after move = %d, want 1", n)
	}

	// Collision: an identical appearance (same resolved album identity) already
	// sits on the source — moving it back must refuse.
	if out, err := db.MoveTagset(ctx, moving, src); err != nil {
		t.Fatalf("collision probe: %v", err)
	} else if out.Collides {
		t.Fatalf("unexpected collision moving distinct appearance back") // sanity: distinct albums don't collide
	} else if !out.Moved {
		t.Fatalf("move back refused: %+v", out)
	}
	// The colliding tagset must not be its own recording's last appearance
	// (that refusal fires first), so pair it with a second one.
	dup := insertTaggedFile(t, db, hash64("mt4"), "dup.mp3", "The Band", "Best Of")
	sibling := insertTaggedFile(t, db, hash64("mt5"), "sib.mp3", "The Band", "Sibling Release")
	groupIntoRecording(t, db, sibling.ID, dup.ID)
	dupTagset := tagsetOfFile(t, db, dup.ID)
	if out, err := db.MoveTagset(ctx, dupTagset, src); err != nil {
		t.Fatalf("collision move: %v", err)
	} else if !out.Collides {
		t.Errorf("identical appearance move: out=%+v, want Collides", out)
	}

	// Last appearance: f3's tagset is its recording's only one.
	if out, err := db.MoveTagset(ctx, tagsetOfFile(t, db, f3.ID), src); err != nil {
		t.Fatalf("last-appearance move: %v", err)
	} else if !out.LastAppearance {
		t.Errorf("last appearance: out=%+v, want LastAppearance refusal", out)
	}
	assertInvariants(t, db)
}

// TestSetPrimaryTagset: the primary swaps in one transaction; a tagset of a
// different recording is refused.
func TestSetPrimaryTagset(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("sp1"), "a.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("sp2"), "b.mp3", "The Band", "Best Of")
	rec := groupIntoRecording(t, db, f1.ID, f2.ID)
	second := tagsetOfFile(t, db, f2.ID)

	found, err := db.SetPrimaryTagset(ctx, rec, second)
	if err != nil || !found {
		t.Fatalf("set primary: found=%v err=%v", found, err)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=? AND is_primary=1`, rec); n != 1 {
		t.Errorf("primaries = %d, want exactly 1", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE id=? AND is_primary=1`, second); n != 1 {
		t.Errorf("chosen tagset is not the primary")
	}

	other := insertTaggedFile(t, db, hash64("sp3"), "c.mp3", "X", "Y")
	if found, err := db.SetPrimaryTagset(ctx, rec, tagsetOfFile(t, db, other.ID)); err != nil || found {
		t.Errorf("foreign tagset: found=%v err=%v, want refusal", found, err)
	}
	assertInvariants(t, db)
}

// TestTrashRecording: all appearances trash (dormant in the library), fully
// restorable state; a second call reports 0 newly trashed; unknown id → found
// false. Bulk mirrors it over a set with unknown ids skipped.
func TestTrashRecording(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("tr1"), "a.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("tr2"), "b.mp3", "The Band", "Best Of")
	rec := groupIntoRecording(t, db, f1.ID, f2.ID)

	n, found, err := db.TrashRecording(ctx, rec)
	if err != nil || !found || n != 2 {
		t.Fatalf("trash: n=%d found=%v err=%v, want 2/true", n, found, err)
	}
	if got := visibleTagsetCount(t, db, rec); got != 0 {
		t.Errorf("recording still library-visible after whole-recording trash")
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE recording_id=? AND deleted_at IS NULL`, rec); n != 2 {
		t.Errorf("blobs touched by a soft trash: %d live, want 2", n)
	}
	if n, found, err := db.TrashRecording(ctx, rec); err != nil || !found || n != 0 {
		t.Errorf("re-trash: n=%d found=%v err=%v, want 0/true", n, found, err)
	}
	if _, found, err := db.TrashRecording(ctx, 99999); err != nil || found {
		t.Errorf("unknown recording: found=%v err=%v, want false", found, err)
	}

	f3 := insertTaggedFile(t, db, hash64("tr3"), "c.mp3", "X", "Y")
	recs, apps, err := db.BulkTrashRecordings(ctx, []int64{recordingIDOf(t, db, f3.ID), 99999})
	if err != nil || recs != 1 || apps != 1 {
		t.Errorf("bulk trash: recs=%d apps=%d err=%v, want 1/1 (unknown skipped)", recs, apps, err)
	}
	assertInvariants(t, db)
}

// TestHardDeleteRecording: the whole cascade in one call — appearances, file
// rows, and blobs reported; everything gone afterwards.
func TestHardDeleteRecording(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("hd1"), "a.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("hd2"), "b.mp3", "The Band", "Best Of")
	rec := groupIntoRecording(t, db, f1.ID, f2.ID)

	out, err := db.HardDeleteRecording(ctx, rec)
	if err != nil {
		t.Fatalf("hard delete: %v", err)
	}
	if !out.Found || out.Appearances != 2 || out.Files != 2 || len(out.Blobs) != 2 {
		t.Errorf("outcome = %+v, want Found with 2 appearances / 2 files / 2 blobs", out)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, rec); n != 0 {
		t.Errorf("recording survived")
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE recording_id=?`, rec); n != 0 {
		t.Errorf("files survived")
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=?`, rec); n != 0 {
		t.Errorf("tagsets survived")
	}
	if out, err := db.HardDeleteRecording(ctx, rec); err != nil || out.Found {
		t.Errorf("re-delete: out=%+v err=%v, want not-found", out, err)
	}
	assertInvariants(t, db)
}

// TestSetRecordingAccess: license and guest write recording-level, guest sets
// the manual override, nil leaves fields alone, empty license clears.
func TestSetRecordingAccess(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f := insertTaggedFile(t, db, hash64("ra1"), "a.mp3", "The Band", "Studio Album")
	rec := recordingIDOf(t, db, f.ID)

	lic := "CC-BY-4.0"
	guest := true
	if found, err := db.SetRecordingAccess(ctx, rec, &lic, &guest, ShareDepthUpdate{}); err != nil || !found {
		t.Fatalf("set access: found=%v err=%v", found, err)
	}
	if n := countRow(t, db,
		`SELECT COUNT(*) FROM recordings WHERE id=? AND license='CC-BY-4.0' AND guest_playable=1 AND guest_playable_manual=1`, rec); n != 1 {
		t.Errorf("access fields not written")
	}
	// License-only update leaves guest untouched.
	empty := ""
	if found, err := db.SetRecordingAccess(ctx, rec, &empty, nil, ShareDepthUpdate{}); err != nil || !found {
		t.Fatalf("clear license: found=%v err=%v", found, err)
	}
	if n := countRow(t, db,
		`SELECT COUNT(*) FROM recordings WHERE id=? AND license IS NULL AND guest_playable=1`, rec); n != 1 {
		t.Errorf("license clear / guest preserved failed")
	}
	if found, err := db.SetRecordingAccess(ctx, 99999, &lic, nil, ShareDepthUpdate{}); err != nil || found {
		t.Errorf("unknown recording: found=%v err=%v, want false", found, err)
	}
}

// TestListRecordings_FiltersSearchPaging: newest first, the filter pills, the
// #id / substring search, and limit/offset — with the count matching.
func TestListRecordings_FiltersSearchPaging(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("lr1"), "a.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("lr2"), "b.mp3", "The Band", "Best Of")
	multi := groupIntoRecording(t, db, f1.ID, f2.ID) // >1 rendition, >1 appearance
	f3 := insertTaggedFile(t, db, hash64("lr3"), "solo.mp3", "Solo Act", "Single")
	dormantRec := recordingIDOf(t, db, f3.ID)
	if found, err := db.RemoveRendition(ctx, f3.ID); err != nil || !found {
		t.Fatalf("remove rendition: %v", err)
	}
	f4 := insertTaggedFile(t, db, hash64("lr4"), "pin.mp3", "Pinned Act", "Pins")
	if _, err := db.Exec(`UPDATE files SET recording_pinned=1 WHERE id=?`, f4.ID); err != nil {
		t.Fatalf("pin: %v", err)
	}
	pinnedRec := recordingIDOf(t, db, f4.ID)

	all, err := db.ListRecordings(ctx, RecordingListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("recordings = %d, want 3", len(all))
	}
	for i := 1; i < len(all); i++ {
		if all[i-1].ID < all[i].ID {
			t.Errorf("not newest-first: %d before %d", all[i-1].ID, all[i].ID)
		}
	}
	byID := map[int64]RecordingRow{}
	for _, r := range all {
		byID[r.ID] = r
	}
	m := byID[multi]
	if m.LiveRenditions != 2 || m.Appearances != 2 || m.Dormant || m.Title == "" {
		t.Errorf("multi row = %+v, want 2 renditions / 2 appearances, live, titled", m)
	}
	if d := byID[dormantRec]; !d.Dormant || d.RemovedFiles != 1 {
		t.Errorf("dormant row = %+v, want Dormant with 1 removed file", d)
	}
	if p := byID[pinnedRec]; !p.Pinned {
		t.Errorf("pinned row = %+v, want Pinned", p)
	}

	cases := map[string]struct {
		opts RecordingListOptions
		want []int64
	}{
		"multi_rendition":   {RecordingListOptions{Filter: "multi_rendition"}, []int64{multi}},
		"multi_appearance":  {RecordingListOptions{Filter: "multi_appearance"}, []int64{multi}},
		"dormant":           {RecordingListOptions{Filter: "dormant"}, []int64{dormantRec}},
		"pinned":            {RecordingListOptions{Filter: "pinned"}, []int64{pinnedRec}},
		"unknown filter":    {RecordingListOptions{Filter: "nope"}, nil},
		"search id":         {RecordingListOptions{Search: "#" + itoa(multi)}, []int64{multi}},
		"search any tagset": {RecordingListOptions{Search: "best of"}, []int64{multi}},
		"search artist":     {RecordingListOptions{Search: "solo act"}, []int64{dormantRec}},
		"search none":       {RecordingListOptions{Search: "zzz-nothing"}, nil},
	}
	for name, tc := range cases {
		rows, err := db.ListRecordings(ctx, tc.opts)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := make([]int64, 0, len(rows))
		for _, r := range rows {
			got = append(got, r.ID)
		}
		if len(got) != len(tc.want) {
			t.Errorf("%s: ids = %v, want %v", name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: ids = %v, want %v", name, got, tc.want)
			}
		}
		if n, err := db.CountRecordings(ctx, tc.opts); err != nil || n != len(tc.want) {
			t.Errorf("%s: count = %d (err %v), want %d", name, n, err, len(tc.want))
		}
	}

	// Paging: page 2 of size 1 is the middle recording.
	page, err := db.ListRecordings(ctx, RecordingListOptions{Limit: 1, Offset: 1})
	if err != nil || len(page) != 1 || page[0].ID != all[1].ID {
		t.Errorf("page = %+v (err %v), want the second row (%d)", page, err, all[1].ID)
	}
}

// TestGetRecordingDetail: both arms, including a soft-removed rendition (live
// first) and a trashed appearance; unknown id → nil.
func TestGetRecordingDetail(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("rd1"), "a.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("rd2"), "b.mp3", "The Band", "Best Of")
	rec := groupIntoRecording(t, db, f1.ID, f2.ID)
	if found, err := db.RemoveRendition(ctx, f2.ID); err != nil || !found {
		t.Fatalf("remove rendition: %v", err)
	}
	trashed := tagsetOfFile(t, db, f2.ID)
	if n, err := db.BulkTrashTagsets(ctx, []int64{trashed}); err != nil || n != 1 {
		t.Fatalf("trash tagset: n=%d err=%v", n, err)
	}

	d, err := db.GetRecordingDetail(ctx, rec)
	if err != nil {
		t.Fatalf("detail: %v", err)
	}
	if d == nil {
		t.Fatal("detail nil for existing recording")
	}
	if len(d.Renditions) != 2 || len(d.Appearances) != 2 {
		t.Fatalf("arms = %d renditions / %d appearances, want 2/2", len(d.Renditions), len(d.Appearances))
	}
	if d.Renditions[0].Removed || !d.Renditions[1].Removed {
		t.Errorf("renditions not live-first: %+v", d.Renditions)
	}
	if !d.Appearances[0].IsPrimary || d.Appearances[0].Trashed {
		t.Errorf("first appearance should be the live primary: %+v", d.Appearances[0])
	}
	if !d.Appearances[1].Trashed {
		t.Errorf("second appearance should be trashed: %+v", d.Appearances[1])
	}
	if missing, err := db.GetRecordingDetail(ctx, 99999); err != nil || missing != nil {
		t.Errorf("unknown id: %+v err=%v, want nil", missing, err)
	}
}

// TestRestoreAndHardDeleteTagset covers the tagset-addressed trash inverse and
// permanent delete (the recordings view's trashed-appearance actions): restore
// un-trashes one appearance; hard delete refuses a live one, drops a trashed
// non-last one keeping the recording, and GCs the recording + reclaims the blob
// when it was the last appearance.
func TestRestoreAndHardDeleteTagset(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("ta1"), "a.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("ta2"), "b.mp3", "The Band", "Best Of")
	rec := groupIntoRecording(t, db, f1.ID, f2.ID)
	ts1, ts2 := tagsetOfFile(t, db, f1.ID), tagsetOfFile(t, db, f2.ID)

	// Restore un-trashes a trashed appearance; a live one / unknown id → false.
	if n, err := db.BulkTrashTagsets(ctx, []int64{ts2}); err != nil || n != 1 {
		t.Fatalf("trash: n=%d err=%v", n, err)
	}
	if found, err := db.RestoreTagset(ctx, ts2); err != nil || !found {
		t.Fatalf("restore trashed: found=%v err=%v", found, err)
	}
	if found, _ := db.RestoreTagset(ctx, ts1); found {
		t.Error("restore of a live appearance should report not-found")
	}
	if found, _ := db.RestoreTagset(ctx, 99999); found {
		t.Error("restore of an unknown id should report not-found")
	}
	if d, _ := db.GetRecordingDetail(ctx, rec); d == nil || d.Appearances[0].Trashed || d.Appearances[1].Trashed {
		t.Fatalf("both appearances should be live after restore: %+v", d)
	}

	// Hard delete refuses a live appearance (trash it first).
	if out, err := db.HardDeleteTrashedTagset(ctx, ts2); err != nil || !out.Found || out.Trashed {
		t.Fatalf("live hard-delete: %+v err=%v, want Found && !Trashed", out, err)
	}
	if out, _ := db.HardDeleteTrashedTagset(ctx, 99999); out.Found {
		t.Error("hard-delete of an unknown id should report not-found")
	}

	// Trashed non-last appearance: dropped, recording survives, no blob freed.
	if _, err := db.BulkTrashTagsets(ctx, []int64{ts2}); err != nil {
		t.Fatalf("trash ts2: %v", err)
	}
	out, err := db.HardDeleteTrashedTagset(ctx, ts2)
	if err != nil || !out.Found || !out.Trashed || len(out.Blobs) != 0 {
		t.Fatalf("non-last hard-delete: %+v err=%v, want Found && Trashed && no blobs", out, err)
	}
	d, _ := db.GetRecordingDetail(ctx, rec)
	if d == nil || len(d.Appearances) != 1 {
		t.Fatalf("recording should keep its one appearance: %+v", d)
	}

	// Trashing + hard-deleting the LAST appearance GCs the recording and returns
	// its files' blobs for reclamation.
	if _, err := db.BulkTrashTagsets(ctx, []int64{ts1}); err != nil {
		t.Fatalf("trash ts1: %v", err)
	}
	last, err := db.HardDeleteTrashedTagset(ctx, ts1)
	if err != nil || !last.Found || !last.Trashed {
		t.Fatalf("last hard-delete: %+v err=%v", last, err)
	}
	if len(last.Blobs) == 0 {
		t.Error("deleting the last appearance should return the recording's blobs to reclaim")
	}
	if gone, _ := db.GetRecordingDetail(ctx, rec); gone != nil {
		t.Errorf("recording should be gone after its last appearance was deleted: %+v", gone)
	}
}

// recordingIDOf returns a file's recording id.
func recordingIDOf(t *testing.T, db *DB, fileID int64) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT recording_id FROM files WHERE id=?`, fileID).Scan(&id); err != nil {
		t.Fatalf("recording of file %d: %v", fileID, err)
	}
	return id
}

// tagsetOfFile returns the tagset originated by a file.
func tagsetOfFile(t *testing.T, db *DB, fileID int64) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT id FROM tagsets WHERE origin_file_id=? ORDER BY id LIMIT 1`, fileID).Scan(&id); err != nil {
		t.Fatalf("tagset of file %d: %v", fileID, err)
	}
	return id
}

// itoa avoids strconv in the test's search-case table.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestMergeRecordings_OrphanedRenditionStaysManageable pins recording-tagsets
// P7. Appearance dedup drops the source's duplicate tagset while its blob moves
// to the target, leaving a live rendition that no tagset points at — a valid,
// by-design state. The file surfaces must still see it: they root on
// files.recording_id, not on the provenance column tagsets.origin_file_id.
func TestMergeRecordings_OrphanedRenditionStaysManageable(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// Identical artist+album ⇒ identical appearance key ⇒ the source's
	// appearance is dropped as a duplicate, its blob survives and moves.
	f1 := insertTaggedFile(t, db, hash64("p7m1"), "studio.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("p7m2"), "reissue.mp3", "The Band", "Studio Album")
	target := recordingIDOf(t, db, f1.ID)
	src := recordingIDOf(t, db, f2.ID)

	if _, err := db.MergeRecordings(ctx, target, []int64{src}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	// Precondition: f2 really is an orphaned rendition of the target.
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE origin_file_id=?`, f2.ID); n != 0 {
		t.Fatalf("setup: f2 still has %d tagset(s); the dedup did not drop it", n)
	}
	if n := countRow(t, db,
		`SELECT COUNT(*) FROM files WHERE id=? AND recording_id=? AND deleted_at IS NULL`, f2.ID, target); n != 1 {
		t.Fatalf("setup: f2 is not a live rendition of the target")
	}

	// 1. Admin·Library "All files" must list both renditions.
	rows, err := db.ListFilesPage(ctx, FileListQuery{})
	if err != nil {
		t.Fatalf("ListFilesPage: %v", err)
	}
	seen := false
	for _, r := range rows {
		if r.Hash == f2.Hash {
			seen = true
		}
	}
	if !seen || len(rows) != 2 {
		t.Errorf("ListFilesPage = %d row(s), contains orphan = %v; want 2 rows containing it", len(rows), seen)
	}

	// 2. The count must agree with the listing (bulk select-all reads it).
	if n, err := db.CountFiles(ctx, FileFilter{}); err != nil || n != 2 {
		t.Errorf("CountFiles = %d (err %v), want 2", n, err)
	}

	// 3. The analysis backfill must still fingerprint it, or the quality ladder
	//    silently degrades to the format/size fallback for that blob.
	ids, err := db.FilesNeedingAnalysis(ctx)
	if err != nil {
		t.Fatalf("FilesNeedingAnalysis: %v", err)
	}
	if !slices.Contains(ids, f2.ID) {
		t.Errorf("FilesNeedingAnalysis = %v, want it to contain the orphaned rendition %d", ids, f2.ID)
	}

	// 4. The hash-addressed access setter must reach it (it is a live rendition
	//    of a live recording), rather than silently reporting found=false.
	found, err := db.SetGuestPlayable(ctx, f2.Hash, true)
	if err != nil {
		t.Fatalf("SetGuestPlayable: %v", err)
	}
	if !found {
		t.Error("SetGuestPlayable(orphan rendition hash) = not found; want found")
	}

	// Control: serving was always recording-rooted and must stay that way.
	if ok, err := db.FileAccessibleByHash(ctx, f2.Hash); err != nil || !ok {
		t.Errorf("FileAccessibleByHash(orphan) = %v (err %v), want true", ok, err)
	}
}

// TestCreateAppearance covers the hand-authored appearance (recording-tagsets
// P7d): a blobless, approved, non-primary appearance on an existing recording,
// plus the meaningful-rule / dedup / empty-title / unknown-recording refusals.
func TestCreateAppearance(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f := insertTaggedFile(t, db, hash64("ca1"), "studio.flac", "The Band", "Studio Album")
	rec := recordingIDOf(t, db, f.ID)
	if _, err := db.ExecContext(ctx,
		`INSERT INTO users (id, username, password_hash, created_at) VALUES (7, 'mod', 'x', 1)`); err != nil {
		t.Fatal(err)
	}
	actor := sql.NullInt64{Int64: 7, Valid: true}

	// Happy path: a distinct release on the same recording.
	out, err := db.CreateAppearance(ctx, rec, AppearanceInput{
		Title: "Same Song", Artist: "The Band", AlbumArtist: "The Band", Album: "Best Of",
	}, actor)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if out.TagsetID == 0 {
		t.Fatalf("outcome = %+v, want a created tagset", out)
	}
	var (
		album, state        string
		primary             int
		origin, createdBy   sql.NullInt64
		albumID, albumArtID sql.NullInt64
	)
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(album,''), review_state, is_primary, origin_file_id, created_by, album_id, album_artist_id
		   FROM tagsets WHERE id = ?`, out.TagsetID).Scan(
		&album, &state, &primary, &origin, &createdBy, &albumID, &albumArtID); err != nil {
		t.Fatal(err)
	}
	if album != "Best Of" || state != "approved" || primary != 0 || origin.Valid {
		t.Errorf("created row: album=%q state=%q primary=%d originValid=%v; want Best Of/approved/0/false",
			album, state, primary, origin.Valid)
	}
	if createdBy.Int64 != 7 {
		t.Errorf("created_by = %v, want 7", createdBy)
	}
	if !albumID.Valid || !albumArtID.Valid {
		t.Error("entity FKs not resolved on the created appearance")
	}

	// It is now a library-visible track of the recording (plays the recording's
	// rendition, though it carries no blob of its own).
	tracks, err := db.ListTracksByAlbumID(ctx, albumID.Int64)
	if err != nil {
		t.Fatalf("list tracks: %v", err)
	}
	if len(tracks) != 1 || tracks[0].TagsetID != out.TagsetID {
		t.Errorf("blobless appearance not browsable: got %d track(s)", len(tracks))
	}

	// Dedup: the identical release is refused.
	dup, err := db.CreateAppearance(ctx, rec, AppearanceInput{
		Title: "Whatever", Artist: "The Band", AlbumArtist: "The Band", Album: "Best Of",
	}, actor)
	if err != nil {
		t.Fatalf("dup: %v", err)
	}
	if !dup.Collides || dup.TagsetID != 0 {
		t.Errorf("dup outcome = %+v, want Collides", dup)
	}

	// Meaningful rule: an all-blank appearance resolves to Unknown/Other → refused.
	nameless, err := db.CreateAppearance(ctx, rec, AppearanceInput{Title: "Untitled"}, actor)
	if err != nil {
		t.Fatalf("nameless: %v", err)
	}
	if !nameless.Nameless || nameless.TagsetID != 0 {
		t.Errorf("nameless outcome = %+v, want Nameless", nameless)
	}

	// Empty title is refused (the CHECK would abort anyway).
	empty, err := db.CreateAppearance(ctx, rec, AppearanceInput{Title: "   ", Artist: "X"}, actor)
	if err != nil {
		t.Fatalf("empty: %v", err)
	}
	if !empty.EmptyTitle {
		t.Errorf("empty-title outcome = %+v, want EmptyTitle", empty)
	}

	// Unknown recording → NotFound.
	nf, err := db.CreateAppearance(ctx, 99999, AppearanceInput{Title: "T", Artist: "A"}, actor)
	if err != nil {
		t.Fatalf("notfound: %v", err)
	}
	if !nf.NotFound {
		t.Errorf("unknown-recording outcome = %+v, want NotFound", nf)
	}

	// The recording still has exactly its two live appearances (original + the
	// one successful add) — the refusals inserted nothing.
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=? AND deleted_at IS NULL`, rec); n != 2 {
		t.Errorf("live appearances = %d, want 2", n)
	}
}

// TestCreateAppearance_DiscTrackDistinguish: two appearances that differ only by
// disc/track are NOT duplicates (NULL-safe identity — untagged stays distinct
// from a numbered one, disc-numbering.md).
func TestCreateAppearance_DiscTrackDistinguish(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	f := insertTaggedFile(t, db, hash64("ca2"), "a.flac", "The Band", "Anthology")
	rec := recordingIDOf(t, db, f.ID)
	one := int64(1)

	a, err := db.CreateAppearance(ctx, rec, AppearanceInput{Title: "T", Artist: "The Band", Album: "Anthology", TrackNumber: &one}, sql.NullInt64{})
	if err != nil || a.TagsetID == 0 {
		t.Fatalf("first: %+v err=%v", a, err)
	}
	two := int64(2)
	b, err := db.CreateAppearance(ctx, rec, AppearanceInput{Title: "T2", Artist: "The Band", Album: "Anthology", TrackNumber: &two}, sql.NullInt64{})
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if b.TagsetID == 0 || b.Collides {
		t.Errorf("track 2 outcome = %+v, want a distinct create (not a collision)", b)
	}
}

// Merge shares the dedup rule with absorb (loadAppearances), and shares its bug:
// a TRASHED appearance on the target claimed an identity, so the source's LIVE
// approved twin was hard-deleted instead of moved.
func TestMergeRecordings_TrashedTargetAppearanceIsNotAKeptKey(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("mt1"), "studio.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("mt2"), "reissue.mp3", "The Band", "Studio Album")
	target := recordingOfFile(t, db, f1.ID)
	source := recordingOfFile(t, db, f2.ID)

	if _, err := db.Exec(`UPDATE tagsets SET deleted_at=1700000000 WHERE id=?`,
		tagsetOfFile(t, db, f1.ID)); err != nil {
		t.Fatalf("trash target appearance: %v", err)
	}
	live := tagsetOfFile(t, db, f2.ID)

	out, err := db.MergeRecordings(ctx, target, []int64{source})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !out.Found {
		t.Fatal("merge reported not found")
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE id=? AND recording_id=?`, live, target); n != 1 {
		t.Errorf("the live appearance did not survive onto the target (outcome %+v)", out)
	}
	if got := visibleTagsetCount(t, db, target); got != 1 {
		t.Errorf("library-visible appearances = %d, want 1", got)
	}
}

// A non-live appearance takes no part in merge's dedup, but it must still MOVE:
// the source recording is going away, and a row left behind is reaped with it.
func TestMergeRecordings_NonLiveAppearancesMoveWithTheSource(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("mv1"), "studio.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("mv2"), "reissue.mp3", "The Band", "Studio Album")
	target := recordingOfFile(t, db, f1.ID)
	source := recordingOfFile(t, db, f2.ID)

	// The source's only appearance is trashed — same identity as the target's
	// live one, so the old rule would have dropped it as a duplicate.
	trashed := tagsetOfFile(t, db, f2.ID)
	if _, err := db.Exec(`UPDATE tagsets SET deleted_at=1700000000 WHERE id=?`, trashed); err != nil {
		t.Fatalf("trash source appearance: %v", err)
	}

	if _, err := db.MergeRecordings(ctx, target, []int64{source}); err != nil {
		t.Fatalf("merge: %v", err)
	}
	var rec int64
	if err := db.QueryRow(`SELECT recording_id FROM tagsets WHERE id=?`, trashed).Scan(&rec); err != nil {
		t.Fatalf("the trashed appearance was destroyed by the merge: %v", err)
	}
	if rec != target {
		t.Errorf("trashed appearance is on recording %d, want the merge target %d", rec, target)
	}
}

// recordingOfFile returns the recording a file is a rendition of.
func recordingOfFile(t *testing.T, db *DB, fileID int64) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT recording_id FROM files WHERE id=?`, fileID).Scan(&id); err != nil {
		t.Fatalf("recording of file %d: %v", fileID, err)
	}
	return id
}
