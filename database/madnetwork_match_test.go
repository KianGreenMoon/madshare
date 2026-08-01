package database

import (
	"context"
	"testing"

	"daemonlord.ygg/madshare/federation"
)

// recordingOf reads the recording a seeded file landed on. Every file owns one
// from insert time, so this is just the id the join is asked about.
func recordingForHash(t *testing.T, db *DB, hash string) int64 {
	t.Helper()
	var rec int64
	if err := db.QueryRow(`SELECT recording_id FROM files WHERE hash = ?`, hash).Scan(&rec); err != nil {
		t.Fatalf("recording of %s: %v", hash, err)
	}
	return rec
}

// TestMatchRecordingJoinsByHashThenFingerprint is the F8 join in one picture: the
// two stages find different things, the second never repeats the first, and text
// is not a join at all — the entry that shares our title and nothing else is
// absent from the answer, which is the property the whole design rests on.
func TestMatchRecordingJoinsByHashThenFingerprint(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	honest := fpWords(1, 400)
	unrelated := fpWords(2, 400)

	ours := seedFingerprintedFile(t, db, "ours", honest)
	rec := recordingForHash(t, db, ours)

	insertPeer(t, db, "aa11", "friendly", federation.PeerFriend)
	src := insertSource(t, db, "aa11")

	// head is what a real catalog advertises: PublishedCatalog truncates the
	// fingerprint to ClaimHeadWords in SQL, so a fixture carrying whole
	// fingerprints would compare over far more words than the wire ever offers.
	head := func(words []uint32) *federation.FingerprintClaim {
		return claimOf(words[:federation.ClaimHeadWords])
	}
	entry := func(key, hash string, dur float64, claim *federation.FingerprintClaim) federation.CatalogEntry {
		e := catEntry(key, "rec-"+key, "Artist", "Album", "Title", hash)
		e.Duration = dur
		e.Renditions[0].Fingerprint = claim
		return e
	}
	if err := db.ReplaceSourceCatalog(ctx, src, "s1", 100, []federation.CatalogEntry{
		// Stage 1: they hold our exact bytes.
		entry("same-bytes", ours, 200, head(honest)),
		// Stage 2: a re-encode — different bytes, same audio, plausible duration.
		entry("reencode", "reencoded-hash", 201, head(fpWordsNear(honest, 3))),
		// Same audio claimed, but a duration no shortlist would admit.
		entry("far-duration", "far-hash", 900, head(fpWordsNear(honest, 3))),
		// Different audio entirely, sharing only the tag text.
		entry("namesake", "other-hash", 200, head(unrelated)),
	}); err != nil {
		t.Fatalf("ReplaceSourceCatalog: %v", err)
	}

	matches, err := db.MatchRecording(ctx, rec, 0)
	if err != nil {
		t.Fatalf("MatchRecording: %v", err)
	}
	got := map[string]NetworkMatch{}
	for _, m := range matches {
		if _, dup := got[m.Entry.Key]; dup {
			t.Errorf("entry %q reported twice; the stages must not overlap", m.Entry.Key)
		}
		got[m.Entry.Key] = m
	}
	if len(got) != 2 {
		t.Fatalf("matched %d entries (%v), want same-bytes and reencode", len(got), keysOf(got))
	}

	hashMatch, ok := got["same-bytes"]
	if !ok {
		t.Fatal("the entry holding our own bytes was not matched")
	}
	if hashMatch.Match != MatchHash || hashMatch.SharedHash != ours {
		t.Errorf("same-bytes = %s/%s, want %s on %s", hashMatch.Match, hashMatch.SharedHash, MatchHash, ours)
	}

	fpMatch, ok := got["reencode"]
	if !ok {
		t.Fatal("the re-encode was not matched; stage 2 is what finds a better rendition")
	}
	if fpMatch.Match != MatchFingerprint {
		t.Errorf("reencode matched by %s, want %s", fpMatch.Match, MatchFingerprint)
	}
	if fpMatch.BER > maxBitErrorRate {
		t.Errorf("reencode BER = %f, want at or below the grouping threshold %f", fpMatch.BER, maxBitErrorRate)
	}
	if fpMatch.Words != federation.ClaimHeadWords {
		t.Errorf("compared words = %d, want the head length %d", fpMatch.Words, federation.ClaimHeadWords)
	}
	if _, bad := got["namesake"]; bad {
		t.Error("matched an entry that shares only its tag text — text must never be a join")
	}
}

// TestMatchRecordingHidesBlockedSources pins that the join obeys the same trust
// condition as browsing: a blocked node's cached rows stay stored (so an unblock
// restores them) and stay invisible.
func TestMatchRecordingHidesBlockedSources(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	honest := fpWords(3, 400)
	ours := seedFingerprintedFile(t, db, "ours", honest)
	rec := recordingForHash(t, db, ours)

	insertPeer(t, db, "bb22", "blocked-one", federation.PeerBlocked)
	src := insertSource(t, db, "bb22")
	e := catEntry("held", "rec-held", "Artist", "Album", "Title", ours)
	e.Renditions[0].Fingerprint = claimOf(honest)
	if err := db.ReplaceSourceCatalog(ctx, src, "s1", 100, []federation.CatalogEntry{e}); err != nil {
		t.Fatalf("ReplaceSourceCatalog: %v", err)
	}

	matches, err := db.MatchRecording(ctx, rec, 0)
	if err != nil {
		t.Fatalf("MatchRecording: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("matched %d entries from a blocked source, want none", len(matches))
	}
}

// TestMatchRecordingWithoutFingerprintStillJoinsOnHash: an unfingerprinted
// recording is uncheckable beyond its bytes, which is a smaller answer, not an
// error. Nothing about F8 may require fpcalc to have run.
func TestMatchRecordingWithoutFingerprintStillJoinsOnHash(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	_, hash := insertAnalysisFile(t, db, "bare")
	rec := recordingForHash(t, db, hash)

	insertPeer(t, db, "cc33", "friendly", federation.PeerFriend)
	src := insertSource(t, db, "cc33")
	if err := db.ReplaceSourceCatalog(ctx, src, "s1", 100, []federation.CatalogEntry{
		catEntry("held", "rec-held", "Artist", "Album", "Title", hash),
	}); err != nil {
		t.Fatalf("ReplaceSourceCatalog: %v", err)
	}

	matches, err := db.MatchRecording(ctx, rec, 0)
	if err != nil {
		t.Fatalf("MatchRecording: %v", err)
	}
	if len(matches) != 1 || matches[0].Match != MatchHash {
		t.Fatalf("matches = %+v, want one hash match", matches)
	}
}

func keysOf(m map[string]NetworkMatch) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
