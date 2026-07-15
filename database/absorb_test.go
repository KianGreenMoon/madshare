package database

import (
	"context"
	"database/sql"
	"testing"
)

// Absorb (recording-tagsets P3) — the original motivator: keep the best blob,
// preserve every distinct appearance, drop the redundant/nameless ones.

// insertTaggedFile inserts a live approved file whose appearance resolves to
// the given artist/album (empty → the reserved buckets). Used to build
// multi-appearance recordings.
func insertTaggedFile(t *testing.T, db *DB, hash, filename, artist, album string) *File {
	t.Helper()
	f := newFile(hash)
	f.ReviewState = ReviewApproved
	m := &MediaMetadata{
		Title:       filename,
		Artist:      sql.NullString{String: artist, Valid: artist != ""},
		Album:       sql.NullString{String: album, Valid: album != ""},
		TagFormat:   sql.NullString{String: "ID3v2.4", Valid: true},
		ExtractedAt: 1700000000,
	}
	if err := db.InsertFile(context.Background(), f, newUpload(filename), m); err != nil {
		t.Fatalf("InsertFile %s: %v", hash, err)
	}
	return f
}

// TestAbsorb_DistinctAppearancesKeptOneBlob is the headline behavior: two
// same-audio renditions on different releases (distinct appearances) → absorb
// keeps one blob and BOTH appearances, each still library-visible (playing the
// kept blob).
func TestAbsorb_DistinctAppearancesKeptOneBlob(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// f1 = "Studio Album", f2 = "Best Of" — same audio, two releases.
	f1 := insertTaggedFile(t, db, hash64("ab1"), "studio.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("ab2"), "bestof.mp3", "The Band", "Best Of")
	rec := groupIntoRecording(t, db, f1.ID, f2.ID)

	out, err := db.AbsorbRenditions(ctx, rec, f1.ID, []int64{f2.ID})
	if err != nil {
		t.Fatalf("absorb: %v", err)
	}
	if !out.Found {
		t.Fatal("absorb reported not found")
	}
	if out.RenditionsRemoved != 1 || out.AppearancesDropped != 0 {
		t.Errorf("outcome = %+v, want {RenditionsRemoved:1 AppearancesDropped:0}", out)
	}
	// Both appearances survive and remain library-visible playing the one blob.
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=?`, rec); n != 2 {
		t.Errorf("appearances = %d, want 2 (both releases preserved)", n)
	}
	if got := visibleTagsetCount(t, db, rec); got != 2 {
		t.Errorf("visible appearances = %d, want 2", got)
	}
	// f1 stays live; f2's blob is soft-removed (kept, restorable).
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id=? AND deleted_at IS NULL`, f1.ID); n != 1 {
		t.Errorf("kept rendition not live: %d", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id=? AND deleted_at IS NOT NULL`, f2.ID); n != 1 {
		t.Errorf("absorbed rendition not soft-removed: %d", n)
	}
	// The one surviving rendition serves both appearances.
	var t2 int64
	if err := db.QueryRow(`SELECT id FROM tagsets WHERE origin_file_id=?`, f2.ID).Scan(&t2); err != nil {
		t.Fatalf("read f2 tagset: %v", err)
	}
	rends, err := db.RecordingRenditionsByTagsetID(ctx, t2)
	if err != nil {
		t.Fatalf("renditions: %v", err)
	}
	if len(rends) != 1 || rends[0].FileID != f1.ID {
		t.Errorf("appearance plays %+v, want the single kept rendition (file %d)", rends, f1.ID)
	}
	assertInvariants(t, db)
}

// TestAbsorb_DuplicateAppearanceDropped: two renditions with the SAME appearance
// (same release, e.g. FLAC + MP3 of one rip) → the absorbed duplicate appearance
// is dropped, the recording keeps a single appearance and the kept blob.
func TestAbsorb_DuplicateAppearanceDropped(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("ac1"), "rip.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("ac2"), "rip.mp3", "The Band", "Studio Album")
	rec := groupIntoRecording(t, db, f1.ID, f2.ID)

	out, err := db.AbsorbRenditions(ctx, rec, f1.ID, []int64{f2.ID})
	if err != nil {
		t.Fatalf("absorb: %v", err)
	}
	if !out.Found || out.RenditionsRemoved != 1 || out.AppearancesDropped != 1 {
		t.Errorf("outcome = %+v, want {Found:true RenditionsRemoved:1 AppearancesDropped:1}", out)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=?`, rec); n != 1 {
		t.Errorf("appearances = %d, want 1 (duplicate dropped)", n)
	}
	// The surviving appearance is the kept rendition's (f1); f2's was the dup.
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE origin_file_id=?`, f1.ID); n != 1 {
		t.Errorf("kept rendition's appearance missing: %d", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE origin_file_id=?`, f2.ID); n != 0 {
		t.Errorf("duplicate appearance survived: %d", n)
	}
	assertInvariants(t, db)
}

// TestAbsorb_NamelessAppearanceDropped: an absorbed rendition whose tags resolve
// to Unknown artist / Other adds nothing — its appearance is dropped even though
// its identity key differs from the kept one.
func TestAbsorb_NamelessAppearanceDropped(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("ad1"), "named.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("ad2"), "junk.mp3", "", "") // → Unknown / Other
	rec := groupIntoRecording(t, db, f1.ID, f2.ID)

	out, err := db.AbsorbRenditions(ctx, rec, f1.ID, []int64{f2.ID})
	if err != nil {
		t.Fatalf("absorb: %v", err)
	}
	if !out.Found || out.AppearancesDropped != 1 {
		t.Errorf("outcome = %+v, want AppearancesDropped:1 (nameless)", out)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=?`, rec); n != 1 {
		t.Errorf("appearances = %d, want 1 (nameless dropped)", n)
	}
	assertInvariants(t, db)
}

// TestAbsorb_StaleSelectionNotFound: a keep or absorb id that is not a live
// rendition of the recording yields found=false with no mutation.
func TestAbsorb_StaleSelectionNotFound(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("ae1"), "a.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("ae2"), "b.mp3", "The Band", "Best Of")
	rec := groupIntoRecording(t, db, f1.ID, f2.ID)

	// keep == an absorb id → stale.
	if out, err := db.AbsorbRenditions(ctx, rec, f1.ID, []int64{f1.ID}); err != nil || out.Found {
		t.Errorf("keep-in-absorb: out=%+v err=%v, want not found", out, err)
	}
	// absorb id not on this recording.
	other := insertTaggedFile(t, db, hash64("ae9"), "z.mp3", "Other", "Elsewhere")
	if out, err := db.AbsorbRenditions(ctx, rec, f1.ID, []int64{other.ID}); err != nil || out.Found {
		t.Errorf("foreign absorb id: out=%+v err=%v, want not found", out, err)
	}
	// Nothing changed.
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE recording_id=? AND deleted_at IS NULL`, rec); n != 2 {
		t.Errorf("live renditions = %d, want 2 (no mutation on stale selection)", n)
	}
	assertInvariants(t, db)
}

// TestBulkAbsorbKeepBest absorbs a recording's non-best renditions into its
// ladder-best in one call; a single-rendition recording is untouched.
func TestBulkAbsorbKeepBest(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// rec1: FLAC (best) + MP3, distinct appearances → keep FLAC, absorb MP3.
	flac := insertTaggedFile(t, db, hash64("af1"), "s.flac", "The Band", "Studio Album")
	mp3 := insertTaggedFile(t, db, hash64("af2"), "s.mp3", "The Band", "Best Of")
	setCodec(t, db, flac.ID, "flac", 44100, 16, 900000)
	setCodec(t, db, mp3.ID, "mp3", 44100, 0, 320000)
	rec1 := groupIntoRecording(t, db, flac.ID, mp3.ID)
	// rec2: a lone rendition — must be skipped.
	solo := insertTaggedFile(t, db, hash64("af3"), "solo.mp3", "Solo", "Album")

	recs, rends, err := db.BulkAbsorbKeepBest(ctx, []int64{rec1, solo.RecordingID})
	if err != nil {
		t.Fatalf("bulk absorb: %v", err)
	}
	if recs != 1 || rends != 1 {
		t.Errorf("bulk absorb = (recordings %d, renditions %d), want (1, 1)", recs, rends)
	}
	// The FLAC (best) is kept live; the MP3 is soft-removed.
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id=? AND deleted_at IS NULL`, flac.ID); n != 1 {
		t.Errorf("best (FLAC) not kept live: %d", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id=? AND deleted_at IS NOT NULL`, mp3.ID); n != 1 {
		t.Errorf("MP3 not absorbed: %d", n)
	}
	// The solo recording is untouched.
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id=? AND deleted_at IS NULL`, solo.ID); n != 1 {
		t.Errorf("solo rendition disturbed: %d", n)
	}
	assertInvariants(t, db)
}

// TestSplitRendition_TagsetLessCopiesRepresentative: splitting a live rendition that
// carries no appearance of its own (e.g. a non-last tagset was hard-deleted in
// P2) gives the new recording a copy of the source's primary so it stays
// browsable, instead of leaving an invalid tagset-less recording.
func TestSplitRendition_TagsetLessCopiesRepresentative(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("ag1"), "one.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("ag2"), "two.mp3", "The Band", "Best Of")
	rec := groupIntoRecording(t, db, f1.ID, f2.ID) // f1 primary, f2 secondary

	// Strip f2's own appearance → f2 is a live, tagset-less rendition of rec.
	if _, err := db.Exec(`DELETE FROM tagsets WHERE origin_file_id=?`, f2.ID); err != nil {
		t.Fatalf("strip f2 tagset: %v", err)
	}

	newRec, found, err := db.SplitRendition(ctx, f2.ID)
	if err != nil || !found {
		t.Fatalf("split: found=%v err=%v", found, err)
	}
	if newRec == rec {
		t.Fatal("split did not create a new recording")
	}
	// The new recording has exactly one appearance, copied from the source's
	// derived representative (title "one.flac"), origin_file_id = the split
	// file. The is_primary flag stays 0 — it is a manual preference (GC P3).
	var (
		cnt          int
		title        string
		originFileID int64
		isPrimary    int
	)
	if err := db.QueryRow(`SELECT COUNT(*) FROM tagsets WHERE recording_id=?`, newRec).Scan(&cnt); err != nil {
		t.Fatalf("count new tagsets: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("new recording appearance count = %d, want 1 (copied primary)", cnt)
	}
	if err := db.QueryRow(
		`SELECT title, origin_file_id, is_primary FROM tagsets WHERE recording_id=?`, newRec,
	).Scan(&title, &originFileID, &isPrimary); err != nil {
		t.Fatalf("read copied tagset: %v", err)
	}
	if title != "one.flac" || originFileID != f2.ID || isPrimary != 0 {
		t.Errorf("copied tagset = (title %q, origin %d, primary %d), want (one.flac, %d, 0)",
			title, originFileID, isPrimary, f2.ID)
	}
	// The new recording is browsable (its copied appearance plays f2).
	if got := visibleTagsetCount(t, db, newRec); got != 1 {
		t.Errorf("split-off recording visible count = %d, want 1", got)
	}
	assertInvariants(t, db)
}

// setCodec fills a file's media_metadata tech columns so the quality ladder has
// something to rank on.
func setCodec(t *testing.T, db *DB, fileID int64, codec string, sampleRate, bitDepth, bitrate int) {
	t.Helper()
	if _, err := db.Exec(
		`UPDATE media_metadata SET codec=?, sample_rate=?, bit_depth=?, bitrate=? WHERE file_id=?`,
		codec, sampleRate, bitDepth, bitrate, fileID); err != nil {
		t.Fatalf("set codec on file %d: %v", fileID, err)
	}
}
