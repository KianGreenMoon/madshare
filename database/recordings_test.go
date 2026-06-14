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
	if _, ok := recordingOf(t, db, id); ok {
		t.Error("file without a fingerprint should keep recording_id NULL")
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
	if got, ok := recordingOf(t, db, b); ok {
		t.Errorf("pinned file got recording_id %d (= A's %d?), want left untouched (NULL)", got, recA)
	}
}

func TestBackfillRecordings_GroupsExisting(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fp := repeated(0x55AA55AA, 120)

	// Two same-audio files with fingerprints but no recording_id yet (predate
	// the overlay): the backfill should resolve both into one recording.
	a := insertFP(t, db, "h1", 180, fp)
	b := insertFP(t, db, "h2", 180.3, fp)

	n, err := db.BackfillRecordings(ctx)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if n != 2 {
		t.Errorf("backfill processed %d, want 2", n)
	}
	recA, okA := recordingOf(t, db, a)
	recB, okB := recordingOf(t, db, b)
	if !okA || !okB || recA != recB {
		t.Errorf("backfill grouping: recA=%d(%v) recB=%d(%v), want equal & set", recA, okA, recB, okB)
	}

	// Idempotent: a second run finds nothing to do.
	if n2, _ := db.BackfillRecordings(ctx); n2 != 0 {
		t.Errorf("second backfill processed %d, want 0 (idempotent)", n2)
	}
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

	// Trashing one rendition drops the recording below the >1 threshold.
	if _, _, err := db.SoftDeleteFileByHash(ctx, hash64("p1")); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if dups, _ := db.ListDuplicateRecordings(ctx); len(dups) != 0 {
		t.Errorf("after trashing one rendition, duplicates = %d, want 0", len(dups))
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

	newRec, found, err := db.SplitRendition(ctx, a)
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
	if _, found, _ := db.SplitRendition(ctx, 999999); found {
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
	if _, _, err := db.SoftDeleteFileByHash(ctx, hash64("s1")); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
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
	if _, err := db.ExecContext(ctx, `UPDATE files SET review_state='submitted' WHERE hash=?`, hash64("s3")); err != nil {
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

func TestRecordingRenditionsByHash(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	fp := repeated(0x1A2B3C4D, 120)

	a := insertFP(t, db, "r1", 200, fp)
	db.ResolveRecording(ctx, a)
	b := insertFP(t, db, "r2", 200, fp) // grouped with a
	db.ResolveRecording(ctx, b)

	// Either member returns both renditions of the recording.
	rends, err := db.RecordingRenditionsByHash(ctx, hash64("r1"))
	if err != nil {
		t.Fatalf("renditions: %v", err)
	}
	if len(rends) != 2 {
		t.Errorf("recording renditions = %d, want 2", len(rends))
	}

	// A lone file (its own recording) returns just itself.
	c := insertFP(t, db, "r3", 90, repeated(0x99999999, 120))
	db.ResolveRecording(ctx, c)
	if rends, _ := db.RecordingRenditionsByHash(ctx, hash64("r3")); len(rends) != 1 {
		t.Errorf("lone renditions = %d, want 1", len(rends))
	}

	// Unknown hash → nil.
	if rends, _ := db.RecordingRenditionsByHash(ctx, hash64("zz")); rends != nil {
		t.Errorf("unknown hash returned %v, want nil", rends)
	}
}

func TestRecordingRenditionsByHash_ExcludesNonApproved(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	// A staged (submitted) file is not yet listenable, so its renditions 404.
	h := insertApprovedTagged(t, db, "n1", "x", "A", "B")
	if _, err := db.ExecContext(ctx, `UPDATE files SET review_state='submitted' WHERE hash=?`, h); err != nil {
		t.Fatalf("set submitted: %v", err)
	}
	if rends, _ := db.RecordingRenditionsByHash(ctx, h); rends != nil {
		t.Errorf("non-approved file returned %v, want nil", rends)
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
