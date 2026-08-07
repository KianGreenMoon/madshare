package database

import (
	"context"
	"database/sql"
	"testing"

	"daemonlord.ygg/madshare/media"
)

func insertFP(t *testing.T, db *DB, seed string, dur float64, raw []uint32) int64 {
	t.Helper()
	id, _ := insertAnalysisFile(t, db, seed)
	fp := media.Fingerprint{Algo: "chromaprint", Duration: dur, Raw: raw}
	if err := db.InsertAudioFingerprint(context.Background(), id, fp, 1000); err != nil {
		t.Fatalf("insert fingerprint(%s): %v", seed, err)
	}
	return id
}

func recordingOf(t *testing.T, db *DB, fileID int64) (int64, bool) {
	t.Helper()
	var r sql.NullInt64
	if err := db.QueryRow(`SELECT recording_id FROM files WHERE id=?`, fileID).Scan(&r); err != nil {
		t.Fatalf("read recording_id: %v", err)
	}
	return r.Int64, r.Valid
}

// repeated returns a raw fingerprint of n copies of v (a controllable input for
// BitErrorRate: equal slices → BER 0, complementary slices → BER 1).
func repeated(v uint32, n int) []uint32 {
	out := make([]uint32, n)
	for i := range out {
		out[i] = v
	}
	return out
}

func TestResolveRecording_GroupsIdenticalFingerprints(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fp := repeated(0xABCD1234, 120)

	a := insertFP(t, db, "c1", 200.0, fp)
	recA, err := db.ResolveRecording(ctx, a)
	if err != nil || recA == 0 {
		t.Fatalf("resolve A: rec=%d err=%v", recA, err)
	}
	b := insertFP(t, db, "c2", 200.4, fp) // same audio, near-identical duration
	recB, err := db.ResolveRecording(ctx, b)
	if err != nil {
		t.Fatalf("resolve B: %v", err)
	}
	if recB != recA {
		t.Errorf("recording B = %d, want same as A = %d (identical fingerprint)", recB, recA)
	}
}

func TestResolveRecording_SeparatesDifferentFingerprints(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	a := insertFP(t, db, "d1", 200, repeated(0x00000000, 120))
	recA, _ := db.ResolveRecording(ctx, a)
	b := insertFP(t, db, "d2", 200, repeated(0xFFFFFFFF, 120)) // BER 1.0 vs A
	recB, _ := db.ResolveRecording(ctx, b)
	if recA == 0 || recB == 0 || recA == recB {
		t.Errorf("different fingerprints grouped: recA=%d recB=%d", recA, recB)
	}
}

func TestResolveRecording_DurationFilterSeparates(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fp := repeated(0x12345678, 120)

	a := insertFP(t, db, "e1", 100, fp)
	recA, _ := db.ResolveRecording(ctx, a)
	// Same bytes-identical fingerprint but a wildly different duration: the
	// shortlist excludes A, so B lands as its own recording (structural edit).
	b := insertFP(t, db, "e2", 400, fp)
	recB, _ := db.ResolveRecording(ctx, b)
	if recA == recB {
		t.Errorf("duration-distant files grouped: recA=%d recB=%d", recA, recB)
	}
}

func TestResolveRecording_NoFingerprintNoOp(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	id, _ := insertAnalysisFile(t, db, "f1") // no fingerprint inserted

	rec, err := db.ResolveRecording(ctx, id)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if rec != 0 {
		t.Errorf("resolve returned %d, want 0 (no fingerprint)", rec)
	}
	// Every file owns a (singleton) recording from insert; without a
	// fingerprint the resolver simply leaves it there.
	if _, ok := recordingOf(t, db, id); !ok {
		t.Error("file should keep its insert-time singleton recording")
	}
}

func TestResolveRecording_SkipsPinned(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fp := repeated(0x0F0F0F0F, 120)

	a := insertFP(t, db, "g1", 200, fp)
	recA, _ := db.ResolveRecording(ctx, a)

	// B is the same audio but pinned (a human split it off). The resolver must
	// not auto-merge it back into A's recording.
	b := insertFP(t, db, "g2", 200, fp)
	if _, err := db.ExecContext(ctx, `UPDATE files SET recording_pinned=1 WHERE id=?`, b); err != nil {
		t.Fatalf("pin: %v", err)
	}
	rec, err := db.ResolveRecording(ctx, b)
	if err != nil {
		t.Fatalf("resolve pinned: %v", err)
	}
	if rec != 0 {
		t.Errorf("resolve pinned returned %d, want 0 (skip)", rec)
	}
	if got, ok := recordingOf(t, db, b); !ok || got == recA {
		t.Errorf("pinned file recording = %d (ok=%v), want its own singleton, never A's %d", got, ok, recA)
	}
}

// TestReap_CollectsGarbageStates seeds the three garbage shapes of the GC
// model (docs/architecture/gc-model.md) and checks each converges by
// demotion, never destruction or manufacture:
//   - appearance-less recording with a live file → the file is quarantined
//     (soft-removed), NOT given a manufactured filename appearance;
//   - empty husk (no tagsets, no files) → the recording row is dropped;
//   - a lost is_primary flag is NOT repaired — primary is a preference, the
//     representative appearance derives as oldest-live.
func TestReap_CollectsGarbageStates(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// (a) A file stripped of its appearance leaves its recording unreachable.
	bare, _ := insertAnalysisFile(t, db, "h1")
	if _, err := db.ExecContext(ctx, `DELETE FROM tagsets WHERE origin_file_id=?`, bare); err != nil {
		t.Fatalf("strip tagset: %v", err)
	}
	// (b) An empty husk (raw DELETEs bypassing everything).
	orphanFile, _ := insertAnalysisFile(t, db, "h2")
	var orphanRec int64
	if err := db.QueryRow(`SELECT recording_id FROM files WHERE id=?`, orphanFile).Scan(&orphanRec); err != nil {
		t.Fatalf("read recording: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM tagsets WHERE origin_file_id=?`, orphanFile); err != nil {
		t.Fatalf("strip tagsets: %v", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM files WHERE id=?`, orphanFile); err != nil {
		t.Fatalf("raw file delete: %v", err)
	}
	// (c) A recording whose primary flag was lost.
	demoted, _ := insertAnalysisFile(t, db, "h3")
	if _, err := db.ExecContext(ctx, `UPDATE tagsets SET is_primary=0 WHERE origin_file_id=?`, demoted); err != nil {
		t.Fatalf("demote primary: %v", err)
	}

	stats, err := db.Reap(ctx)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if stats.QuarantinedFiles != 1 || stats.DeletedHusks != 1 || stats.TrashedTagsets != 0 {
		t.Errorf("reap stats = %+v, want 1 quarantined file + 1 deleted husk", stats)
	}

	// (a) quarantined, not healed: no appearance was manufactured, the blob
	// row survives soft-removed (Trash › Files), its recording remains.
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE origin_file_id=?`, bare); n != 0 {
		t.Errorf("bare file has %d tagsets after reap, want 0 (nothing manufactured)", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE id=? AND deleted_at IS NOT NULL`, bare); n != 1 {
		t.Errorf("bare file not quarantined: soft-removed count=%d, want 1", n)
	}
	// (b) the husk is gone.
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, orphanRec); n != 0 {
		t.Errorf("empty husk survived reap: count=%d", n)
	}
	// (c) untouched: primary is a preference, not an invariant.
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets t JOIN files f ON f.id=t.origin_file_id
		WHERE f.id=? AND t.is_primary=1`, demoted); n != 0 {
		t.Errorf("reap promoted a primary flag (%d), want 0 — primary is not repaired", n)
	}

	// Idempotent: a second run finds nothing to do.
	if stats2, _ := db.Reap(ctx); stats2.Total() != 0 {
		t.Errorf("second reap collected %+v, want nothing (idempotent)", stats2)
	}
	assertInvariants(t, db)
}

func TestListDuplicateRecordings(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fp := repeated(0x33CC33CC, 120)

	// Two same-audio files → one duplicate recording.
	a := insertFP(t, db, "p1", 200, fp)
	recA, _ := db.ResolveRecording(ctx, a)
	b := insertFP(t, db, "p2", 200.2, fp)
	if rec, _ := db.ResolveRecording(ctx, b); rec != recA {
		t.Fatalf("setup: b grouped into %d, want %d", rec, recA)
	}
	// A lone recording must NOT appear.
	c := insertFP(t, db, "p3", 90, repeated(0x11111111, 120))
	db.ResolveRecording(ctx, c)

	dups, err := db.ListDuplicateRecordings(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(dups) != 1 {
		t.Fatalf("duplicate recordings = %d, want 1", len(dups))
	}
	if dups[0].RecordingID != recA || len(dups[0].Renditions) != 2 {
		t.Errorf("got recording %d with %d renditions, want %d with 2", dups[0].RecordingID, len(dups[0].Renditions), recA)
	}

	// Trashing an APPEARANCE does not remove a rendition: the reaper deliberately
	// leaves a blob alone while its tagsets are merely trashed (reap pass 1), so
	// both blobs are still on disk and still need reconciling here. This is the
	// one behaviour the P7 re-rooting changed — before it, trashing the appearance
	// hid the duplicate while both blobs remained.
	trashAppearancesByHash(t, db, hash64("p1"))
	if dups, _ := db.ListDuplicateRecordings(ctx); len(dups) != 1 {
		t.Errorf("after trashing one APPEARANCE, duplicates = %d, want 1 (both blobs are still renditions)", len(dups))
	}

	// Removing the RENDITION is what drops the recording below the >1 threshold.
	if found, err := db.RemoveRendition(ctx, a); err != nil || !found {
		t.Fatalf("remove rendition: found=%v err=%v", found, err)
	}
	if dups, _ := db.ListDuplicateRecordings(ctx); len(dups) != 0 {
		t.Errorf("after removing one rendition, duplicates = %d, want 0", len(dups))
	}
}

// liveFilesOf counts the rendition rows of a recording — the canonical
// definition RecordingRenditionsByTagsetID uses (a live file row), with no
// appearance involved.
func liveFilesOf(t *testing.T, db *DB, recID int64) int {
	t.Helper()
	return countRow(t, db, `SELECT COUNT(*) FROM files WHERE recording_id=? AND deleted_at IS NULL`, recID)
}

// TestListDuplicateRecordings_ListsAnOrphanedRendition pins the shape the page
// exists to fix and could not see. Appearance dedup (merge, absorb) keeps the
// blob and drops the redundant tagset, so the second rendition has no appearance
// of its own — and a listing rooted on `t.origin_file_id = f.id` counted
// provenance links rather than renditions, dropping the whole recording.
func TestListDuplicateRecordings_ListsAnOrphanedRendition(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// Two blobs the resolver keeps apart (different fingerprints), each with its
	// own appearance. The appearances collide on identity because the shared test
	// metadata is identical — which is what makes merge drop one of them.
	a := insertFP(t, db, "orph-a", 200, repeated(0x41414141, 120))
	recA, _ := db.ResolveRecording(ctx, a)
	b := insertFP(t, db, "orph-b", 90, repeated(0x52525252, 120))
	recB, _ := db.ResolveRecording(ctx, b)
	if recA == recB {
		t.Fatalf("setup: both files landed on recording %d; the fixtures must stay apart", recA)
	}

	out, err := db.MergeRecordings(ctx, recA, []int64{recB})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if out.AppearancesDropped != 1 || out.RenditionsMoved != 1 {
		t.Fatalf("merge outcome = %+v, want 1 appearance dropped and 1 rendition moved", out)
	}
	if n := liveFilesOf(t, db, recA); n != 2 {
		t.Fatalf("setup: recording has %d live renditions, want 2", n)
	}

	dups, err := db.ListDuplicateRecordings(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(dups) != 1 {
		t.Fatalf("duplicate recordings = %d, want 1 (a recording with two live blobs is a duplicate "+
			"whether or not both carry an appearance)", len(dups))
	}
	if got := len(dups[0].Renditions); got != 2 {
		t.Errorf("renditions listed = %d, want 2 (the orphaned blob must be reconcilable here)", got)
	}
}

// TestListDuplicateRecordings_ByteDupDraftIsOneRendition is the same defect from
// the other side: counting provenance links let a SINGLE blob carrying two live
// appearances (a byte-dup draft, AttachDraftTagset) pass the >1 test and be
// listed as two renditions of itself. One blob is one rendition.
func TestListDuplicateRecordings_ByteDupDraftIsOneRendition(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	a := insertFP(t, db, "bdup-a", 200, repeated(0x63636363, 120))
	recA, _ := db.ResolveRecording(ctx, a)

	// A second upload of the same bytes attaches another appearance to the very
	// same blob rather than storing it twice. It has to be a genuinely different
	// appearance — the attach dedups on album/album-artist/disc/track — so give
	// it another album, which is also the real case: the same recording turning
	// up on a compilation.
	second := newMeta()
	second.Album = sql.NullString{String: "A Compilation", Valid: true}
	if _, created, err := db.AttachDraftTagset(ctx, a, sql.NullInt64{}, second, "again.mp3"); err != nil || !created {
		t.Fatalf("attach draft tagset: created=%v err=%v", created, err)
	}
	if n := liveFilesOf(t, db, recA); n != 1 {
		t.Fatalf("setup: recording has %d live renditions, want 1", n)
	}

	dups, err := db.ListDuplicateRecordings(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(dups) != 0 {
		t.Fatalf("duplicate recordings = %d, want 0; one blob with two appearances is not two "+
			"renditions (listed %d of them)", len(dups), len(dups[0].Renditions))
	}
}

func TestSplitRendition(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fp := repeated(0x77777777, 120)

	a := insertFP(t, db, "q1", 200, fp)
	recA, _ := db.ResolveRecording(ctx, a)
	b := insertFP(t, db, "q2", 200, fp)
	db.ResolveRecording(ctx, b)

	sp, err := db.SplitRendition(ctx, a)
	newRec, found := sp.NewRecordingID, sp.Found
	if err != nil || !found {
		t.Fatalf("split: found=%v err=%v", found, err)
	}
	if newRec == recA || newRec == 0 {
		t.Errorf("split made recording %d, want a new id (not %d)", newRec, recA)
	}
	gotRec, _ := recordingOf(t, db, a)
	var pinned int
	db.QueryRow(`SELECT recording_pinned FROM files WHERE id=?`, a).Scan(&pinned)
	if gotRec != newRec || pinned != 1 {
		t.Errorf("after split: recording=%d pinned=%d, want %d / 1", gotRec, pinned, newRec)
	}

	// The pinned split must survive a re-resolve (resolver never re-merges it).
	if rec, _ := db.ResolveRecording(ctx, a); rec != 0 {
		t.Errorf("resolve after split returned %d, want 0 (pinned, skipped)", rec)
	}

	// Splitting an unknown file id is a clean not-found.
	if sp, _ := db.SplitRendition(ctx, 999999); sp.Found {
		t.Error("split of unknown file reported found=true")
	}
}

// insertApprovedTagged inserts an approved, fingerprint-less file with the given
// tags (empty string = NULL column) and returns its hash. For the tag-fallback
// duplicate path.
func insertApprovedTagged(t *testing.T, db *DB, seed, title, artist, album string) string {
	t.Helper()
	hash := hash64(seed)
	meta := &MediaMetadata{Title: title, ExtractedAt: 1000}
	if artist != "" {
		meta.Artist = sql.NullString{String: artist, Valid: true}
	}
	if album != "" {
		meta.Album = sql.NullString{String: album, Valid: true}
	}
	if err := db.InsertFile(context.Background(), newFile(hash), newUpload(seed+".mp3"), meta); err != nil {
		t.Fatalf("InsertFile(%s): %v", seed, err)
	}
	return hash
}

func TestIsDuplicateSubmission_Fingerprint(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fp := repeated(0x2468ACE0, 120)

	a := insertFP(t, db, "s1", 200, fp) // approved by default
	db.ResolveRecording(ctx, a)
	b := insertFP(t, db, "s2", 200, fp) // same audio, grouped into a's recording
	db.ResolveRecording(ctx, b)

	if dup, err := db.IsDuplicateSubmission(ctx, hash64("s2")); err != nil || !dup {
		t.Fatalf("want duplicate, got %v err=%v", dup, err)
	}
	// Trashing the only approved sibling clears the flag.
	trashAppearancesByHash(t, db, hash64("s1"))
	if dup, _ := db.IsDuplicateSubmission(ctx, hash64("s2")); dup {
		t.Error("a trashed sibling must not flag a duplicate")
	}
}

func TestIsDuplicateSubmission_SiblingMustBeApproved(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fp := repeated(0x13579BDF, 120)

	a := insertFP(t, db, "s3", 200, fp)
	db.ResolveRecording(ctx, a)
	b := insertFP(t, db, "s4", 200, fp)
	db.ResolveRecording(ctx, b)
	// The sibling is itself still in review (not approved) → no duplicate yet.
	if _, err := db.ExecContext(ctx, `UPDATE tagsets SET review_state='submitted' WHERE origin_file_id IN (SELECT id FROM files WHERE hash=?)`, hash64("s3")); err != nil {
		t.Fatalf("set submitted: %v", err)
	}
	if dup, _ := db.IsDuplicateSubmission(ctx, hash64("s4")); dup {
		t.Error("a non-approved sibling must not flag a duplicate")
	}
}

func TestIsDuplicateSubmission_SingleRenditionNotFlagged(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	a := insertFP(t, db, "s5", 200, repeated(0xCAFEBABE, 120))
	db.ResolveRecording(ctx, a)
	if dup, _ := db.IsDuplicateSubmission(ctx, hash64("s5")); dup {
		t.Error("a lone recording is not a duplicate")
	}
}

func TestIsDuplicateSubmission_TagFallback(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	// No fingerprints (fpcalc absent): identical real tags collide.
	insertApprovedTagged(t, db, "t1", "Same Song", "The Artist", "The Album")
	insertApprovedTagged(t, db, "t2", "Same Song", "The Artist", "The Album")

	if dup, err := db.IsDuplicateSubmission(ctx, hash64("t2")); err != nil || !dup {
		t.Fatalf("matching tags should flag (fpcalc absent): %v err=%v", dup, err)
	}
}

func TestIsDuplicateSubmission_TagFallbackExcludesUntagged(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	// Untagged files (NULL artist/album) must never collide, even with equal title.
	insertApprovedTagged(t, db, "u1", "track", "", "")
	insertApprovedTagged(t, db, "u2", "track", "", "")
	if dup, _ := db.IsDuplicateSubmission(ctx, hash64("u2")); dup {
		t.Error("untagged files must never tag-collide")
	}
}

// tagsetOf returns the offered tagset's id for the file with the given hash.
func tagsetOf(t *testing.T, db *DB, hash string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(
		`SELECT t.id FROM tagsets t JOIN files f ON f.id = t.origin_file_id WHERE f.hash = ?`,
		hash).Scan(&id); err != nil {
		t.Fatalf("tagset of %s: %v", hash, err)
	}
	return id
}

func TestRecordingRenditionsByTagsetID(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fp := repeated(0x1A2B3C4D, 120)

	a := insertFP(t, db, "r1", 200, fp)
	db.ResolveRecording(ctx, a)
	b := insertFP(t, db, "r2", 200, fp) // grouped with a
	db.ResolveRecording(ctx, b)

	// Either appearance returns both renditions of the recording.
	rends, err := db.RecordingRenditionsByTagsetID(ctx, tagsetOf(t, db, hash64("r1")))
	if err != nil {
		t.Fatalf("renditions: %v", err)
	}
	if len(rends) != 2 {
		t.Errorf("recording renditions = %d, want 2", len(rends))
	}
	if rends2, _ := db.RecordingRenditionsByTagsetID(ctx, tagsetOf(t, db, hash64("r2"))); len(rends2) != 2 {
		t.Errorf("sibling appearance renditions = %d, want 2", len(rends2))
	}

	// A lone file (its own recording) returns just itself.
	c := insertFP(t, db, "r3", 90, repeated(0x99999999, 120))
	db.ResolveRecording(ctx, c)
	if rends, _ := db.RecordingRenditionsByTagsetID(ctx, tagsetOf(t, db, hash64("r3"))); len(rends) != 1 {
		t.Errorf("lone renditions = %d, want 1", len(rends))
	}

	// Unknown tagset → nil.
	if rends, _ := db.RecordingRenditionsByTagsetID(ctx, 99999); rends != nil {
		t.Errorf("unknown tagset returned %v, want nil", rends)
	}
}

func TestRecordingRenditionsByTagsetID_ExcludesNonApproved(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	// A staged (submitted) appearance is not yet listenable, so its renditions 404.
	h := insertApprovedTagged(t, db, "n1", "x", "A", "B")
	tsID := tagsetOf(t, db, h)
	if _, err := db.ExecContext(ctx, `UPDATE tagsets SET review_state='submitted' WHERE id=?`, tsID); err != nil {
		t.Fatalf("set submitted: %v", err)
	}
	if rends, _ := db.RecordingRenditionsByTagsetID(ctx, tsID); rends != nil {
		t.Errorf("non-approved appearance returned %v, want nil", rends)
	}
}

func TestRankRenditions_LosslessBeatsLossy(t *testing.T) {
	// A high-bitrate MP3 must still rank below any lossless file.
	ranked := RankRenditions([]Rendition{
		{FileID: 1, Codec: "mp3", Bitrate: 320000, ByteSize: 9_000_000},
		{FileID: 2, Codec: "flac", SampleRate: 44100, BitDepth: 16, ByteSize: 25_000_000},
	})
	if ranked[0].FileID != 2 {
		t.Errorf("best = file %d, want 2 (lossless before lossy)", ranked[0].FileID)
	}
}

func TestRankRenditions_LossyByBitrate(t *testing.T) {
	ranked := RankRenditions([]Rendition{
		{FileID: 1, Codec: "mp3", Bitrate: 128000},
		{FileID: 2, Codec: "mp3", Bitrate: 320000},
	})
	if ranked[0].FileID != 2 {
		t.Errorf("best = file %d, want 2 (higher bitrate)", ranked[0].FileID)
	}
}

func TestRankRenditions_LosslessBySampleRateThenDepth(t *testing.T) {
	bySR := RankRenditions([]Rendition{
		{FileID: 1, Codec: "flac", SampleRate: 44100, BitDepth: 24},
		{FileID: 2, Codec: "flac", SampleRate: 96000, BitDepth: 16},
	})
	if bySR[0].FileID != 2 {
		t.Errorf("best = file %d, want 2 (higher sample rate dominates depth)", bySR[0].FileID)
	}
	byDepth := RankRenditions([]Rendition{
		{FileID: 1, Codec: "flac", SampleRate: 44100, BitDepth: 16},
		{FileID: 2, Codec: "flac", SampleRate: 44100, BitDepth: 24},
	})
	if byDepth[0].FileID != 2 {
		t.Errorf("best = file %d, want 2 (higher bit depth at equal sample rate)", byDepth[0].FileID)
	}
}

func TestRankRenditions_DegradedSizeOnly(t *testing.T) {
	// ffprobe absent: no codec/bitrate, so the ladder falls back to size.
	ranked := RankRenditions([]Rendition{
		{FileID: 1, ByteSize: 3_000_000},
		{FileID: 2, ByteSize: 8_000_000},
	})
	if ranked[0].FileID != 2 {
		t.Errorf("best = file %d, want 2 (larger size in degraded mode)", ranked[0].FileID)
	}
}

// An appearance not read from the departing blob — hand-authored
// (CreateAppearance, origin NULL) or re-homed here by MoveTagset — describes
// this recording's audio. ResolveRecording takes all of that audio away, so the
// appearance goes with it. Before this, it was left on a recording with no file
// rows, where reaper P2 trashes it; restoring is futile because the next reap
// trashes it again, and this path is the unattended background worker.
func TestResolveRecording_StrandedAppearancesFollowTheAudio(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fp := repeated(0xABCD1234, 120)

	a := insertFP(t, db, "sa1", 200.0, fp)
	insertFP(t, db, "sa2", 200.0, fp)
	recA, _ := recordingOf(t, db, a)

	outc, err := db.CreateAppearance(ctx, recA, AppearanceInput{
		Title: "Hand Added", Artist: "The Band", AlbumArtist: "The Band", Album: "Rarities",
	}, sql.NullInt64{})
	if err != nil || outc.TagsetID == 0 {
		t.Fatalf("CreateAppearance: %v %+v", err, outc)
	}
	hand := outc.TagsetID

	merged, err := db.ResolveRecording(ctx, a)
	if err != nil {
		t.Fatalf("ResolveRecording: %v", err)
	}
	if merged == recA {
		t.Fatalf("the resolver did not regroup (still on recording %d)", recA)
	}

	var rec int64
	var deleted sql.NullInt64
	var primary int
	if err := db.QueryRow(
		`SELECT recording_id, deleted_at, is_primary FROM tagsets WHERE id=?`, hand).
		Scan(&rec, &deleted, &primary); err != nil {
		t.Fatalf("the hand-authored appearance was destroyed: %v", err)
	}
	if deleted.Valid {
		t.Error("the hand-authored appearance was trashed instead of following the audio")
	}
	if rec != merged {
		t.Errorf("appearance is on recording %d, want the merged one %d", rec, merged)
	}
	if primary != 0 {
		t.Error("a moved appearance must not arrive primary — the target keeps its own")
	}
	// And the husk is gone: with nothing left on it, the reap deletes it.
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, recA); n != 0 {
		t.Errorf("emptied recording %d survived as a husk", recA)
	}

	// The fix must survive the reaper, which is what made the old behaviour
	// unrecoverable rather than merely surprising.
	if _, err := db.Reap(ctx); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if n := countRow(t, db,
		`SELECT COUNT(*) FROM tagsets WHERE id=? AND deleted_at IS NULL`, hand); n != 1 {
		t.Error("the appearance was trashed by a later reap")
	}
}

// Splitting the LAST rendition of a recording that still holds appearances not
// read from it is refused: the split asserts a different composition, so
// carrying a curator's appearance across would file it under the very thing the
// moderator just declared separate — and leaving it behind is unrecoverable.
func TestSplitRendition_RefusesToStrandAppearances(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("sr1"), "studio.flac", "The Band", "Studio Album")
	rec := recordingOfFile(t, db, f1.ID)
	outc, err := db.CreateAppearance(ctx, rec, AppearanceInput{
		Title: "Hand Added", Artist: "The Band", AlbumArtist: "The Band", Album: "Rarities",
	}, sql.NullInt64{})
	if err != nil || outc.TagsetID == 0 {
		t.Fatalf("CreateAppearance: %v %+v", err, outc)
	}

	sp, err := db.SplitRendition(ctx, f1.ID)
	if err != nil {
		t.Fatalf("SplitRendition: %v", err)
	}
	if !sp.Found {
		t.Fatal("split reported not found")
	}
	if sp.StrandedAppearances != 1 {
		t.Errorf("StrandedAppearances = %d, want 1", sp.StrandedAppearances)
	}
	if sp.NewRecordingID != 0 {
		t.Error("a refused split must not create a recording")
	}
	// Nothing moved, nothing was trashed.
	if got := recordingOfFile(t, db, f1.ID); got != rec {
		t.Errorf("the rendition moved to %d despite the refusal", got)
	}
	if n := countRow(t, db,
		`SELECT COUNT(*) FROM tagsets WHERE recording_id=? AND deleted_at IS NULL`, rec); n != 2 {
		t.Errorf("live appearances = %d, want 2 untouched", n)
	}
}

// The refusal is narrow: with another rendition left behind, the recording keeps
// file rows, so it never becomes a husk and nothing is stranded.
func TestSplitRendition_AllowedWhenARenditionRemains(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("sr2"), "studio.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("sr3"), "live.mp3", "The Band", "Live")
	rec := groupIntoRecording(t, db, f1.ID, f2.ID)
	if _, err := db.CreateAppearance(ctx, rec, AppearanceInput{
		Title: "Hand Added", Artist: "The Band", AlbumArtist: "The Band", Album: "Rarities",
	}, sql.NullInt64{}); err != nil {
		t.Fatalf("CreateAppearance: %v", err)
	}

	sp, err := db.SplitRendition(ctx, f2.ID)
	if err != nil {
		t.Fatalf("SplitRendition: %v", err)
	}
	if !sp.Found || sp.StrandedAppearances != 0 || sp.NewRecordingID == 0 {
		t.Errorf("outcome = %+v, want a clean split", sp)
	}
}

// A soft-removed sibling is still a file ROW, so the recording stays dormant
// rather than becoming a husk — reaper P2 never fires and nothing is stranded.
// This is why the check counts rows, not live renditions.
func TestSplitRendition_SoftRemovedSiblingIsNotAHusk(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	f1 := insertTaggedFile(t, db, hash64("sr4"), "studio.flac", "The Band", "Studio Album")
	f2 := insertTaggedFile(t, db, hash64("sr5"), "live.mp3", "The Band", "Live")
	rec := groupIntoRecording(t, db, f1.ID, f2.ID)
	if _, err := db.CreateAppearance(ctx, rec, AppearanceInput{
		Title: "Hand Added", Artist: "The Band", AlbumArtist: "The Band", Album: "Rarities",
	}, sql.NullInt64{}); err != nil {
		t.Fatalf("CreateAppearance: %v", err)
	}
	if _, err := db.RemoveRendition(ctx, f1.ID); err != nil {
		t.Fatalf("RemoveRendition: %v", err)
	}

	sp, err := db.SplitRendition(ctx, f2.ID)
	if err != nil {
		t.Fatalf("SplitRendition: %v", err)
	}
	if sp.StrandedAppearances != 0 || sp.NewRecordingID == 0 {
		t.Errorf("outcome = %+v, want a clean split (the removed sibling keeps it dormant, not a husk)", sp)
	}
}
