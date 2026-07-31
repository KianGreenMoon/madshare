package database

import (
	"context"
	"encoding/base64"
	"math/rand"
	"testing"

	"daemonlord.ygg/madshare/federation"
	"daemonlord.ygg/madshare/media"
)

// fpWords builds a deterministic pseudo-random fingerprint of n words from a
// seed. Two different seeds land near a bit-error rate of 0.5, which is what
// unrelated audio looks like.
func fpWords(seed int64, n int) []uint32 {
	rnd := rand.New(rand.NewSource(seed))
	out := make([]uint32, n)
	for i := range out {
		out[i] = rnd.Uint32()
	}
	return out
}

// fpWordsNear returns a copy of words with one bit flipped per flips words — a
// stand-in for the same audio fingerprinted by a slightly different build.
func fpWordsNear(words []uint32, flips int) []uint32 {
	out := append([]uint32(nil), words...)
	for i := 0; i < flips && i < len(out); i++ {
		out[i] ^= 1
	}
	return out
}

func claimOf(words []uint32) *federation.FingerprintClaim {
	fp := media.Fingerprint{Algo: "chromaprint", AlgoVersion: "1.5.1", Raw: words}
	return &federation.FingerprintClaim{
		Algo:    fp.Algo,
		Version: fp.AlgoVersion,
		Words:   len(words),
		Head:    base64.StdEncoding.EncodeToString(fp.Packed()),
	}
}

// seedFingerprintedFile inserts a local blob with a fingerprint and returns its
// content hash.
func seedFingerprintedFile(t *testing.T, db *DB, seed string, words []uint32) string {
	t.Helper()
	ctx := context.Background()
	fileID, hash := insertAnalysisFile(t, db, seed)
	fp := media.Fingerprint{Algo: "chromaprint", AlgoVersion: "1.5.1", Duration: 200, Raw: words}
	if err := db.InsertAudioFingerprint(ctx, fileID, fp, 1700000000); err != nil {
		t.Fatalf("InsertAudioFingerprint(%s): %v", seed, err)
	}
	return hash
}

func claimReportRows(t *testing.T, db *DB) []*federation.ClaimReport {
	t.Helper()
	reports, err := db.ListClaimReports(context.Background())
	if err != nil {
		t.Fatalf("ListClaimReports: %v", err)
	}
	return reports
}

// TestClaimHeldBlobContradiction is the airtight case: a peer advertises a hash
// we hold with a fingerprint that cannot belong to those bytes. Identical bytes
// cannot fingerprint differently, so this is arithmetic rather than opinion.
func TestClaimHeldBlobContradiction(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	honest := fpWords(1, 400)
	other := fpWords(2, 400)

	agrees := seedFingerprintedFile(t, db, "ag", honest)
	lies := seedFingerprintedFile(t, db, "li", honest)
	unheld := "not-a-hash-we-hold"

	peer := insertPeer(t, db, "aa11", "claimer", federation.PeerFriend)
	src := insertSource(t, db, "aa11")
	entry := func(key, hash string, claim *federation.FingerprintClaim) federation.CatalogEntry {
		e := catEntry(key, "rec-"+key, "Artist", "Album", "Title "+key, hash)
		e.Renditions[0].Fingerprint = claim
		return e
	}
	if err := db.ReplaceSourceCatalog(ctx, src, "s1", 100, []federation.CatalogEntry{
		// Same bytes, same claim (a near-identical head, as a different
		// chromaprint build would produce) — not a contradiction.
		entry("1", agrees, claimOf(fpWordsNear(honest, 3))),
		// Same bytes, unrelated audio claimed.
		entry("2", lies, claimOf(other)),
		// A claim about bytes we do not hold is simply uncheckable.
		entry("3", unheld, claimOf(other)),
	}); err != nil {
		t.Fatalf("ReplaceSourceCatalog: %v", err)
	}

	open, err := db.CheckSourceClaims(ctx, src)
	if err != nil {
		t.Fatalf("CheckSourceClaims: %v", err)
	}
	if open != 1 {
		t.Fatalf("open reports = %d, want 1", open)
	}
	reports := claimReportRows(t, db)
	if len(reports) != 1 {
		t.Fatalf("len(reports) = %d, want 1", len(reports))
	}
	r := reports[0]
	if r.Kind != federation.ClaimHeldBlob || r.Hash != lies {
		t.Errorf("report = %s/%s, want held_blob on %s", r.Kind, r.Hash, lies)
	}
	if r.BER <= maxBitErrorRate {
		t.Errorf("BER = %f, want above the grouping threshold %f", r.BER, maxBitErrorRate)
	}
	if r.Words != federation.ClaimHeadWords {
		t.Errorf("compared words = %d, want the head length %d", r.Words, federation.ClaimHeadWords)
	}
	if r.OurHead == "" || r.TheirHead == "" || r.OurHead == r.TheirHead {
		t.Error("both fingerprint heads should be stored as evidence, and they should differ")
	}
	if r.TheirVersion != "1.5.1" || r.OurVersion != "1.5.1" {
		t.Errorf("versions = %q/%q, want both recorded (the innocent explanation)", r.OurVersion, r.TheirVersion)
	}
	if r.PeerName != "claimer" || r.PeerKey != "aa11" {
		t.Errorf("peer decoration = %q/%q, want the reporting peer's label and key", r.PeerName, r.PeerKey)
	}
	if r.Disposition != federation.ClaimNew {
		t.Errorf("disposition = %q, want new", r.Disposition)
	}

	// Re-checking must not re-alarm: same row, and an admin's decision stands.
	if _, err := db.CheckSourceClaims(ctx, src); err != nil {
		t.Fatalf("second CheckPeerClaims: %v", err)
	}
	if got := len(claimReportRows(t, db)); got != 1 {
		t.Fatalf("after re-check len(reports) = %d, want the same 1", got)
	}
	if err := db.SetClaimReportDisposition(ctx, r.ID, federation.ClaimDismissed); err != nil {
		t.Fatalf("SetClaimReportDisposition: %v", err)
	}
	if _, err := db.CheckSourceClaims(ctx, src); err != nil {
		t.Fatalf("third CheckPeerClaims: %v", err)
	}
	if got := len(claimReportRows(t, db)); got != 0 {
		t.Errorf("a dismissed finding came back (%d open); detection must not overwrite a decision", got)
	}
	if n, err := db.CountOpenClaimReports(ctx); err != nil || n != 0 {
		t.Errorf("CountOpenClaimReports = %d, %v; want 0", n, err)
	}

	// Forgetting the node forgets what we found about it (CASCADE). Since F7
	// item 5 the evidence hangs off the SOURCE — the cached catalog it was found
	// in — so removing the peer row alone leaves it standing, and the sweep's
	// retention walk is what collects it once the node stops being a member
	// (§Forgetting). Dropping the source is that act, seen from the store.
	if err := db.SetClaimReportDisposition(ctx, r.ID, federation.ClaimNew); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteFederationPeer(ctx, peer); err != nil {
		t.Fatalf("DeleteFederationPeer: %v", err)
	}
	if n, _ := db.CountOpenClaimReports(ctx); n != 1 {
		t.Errorf("report count after removing the peer row = %d, want 1 (the evidence is the cache's)", n)
	}
	if err := db.DropCatalogSources(ctx, []int64{src}); err != nil {
		t.Fatalf("DropCatalogSources: %v", err)
	}
	if n, _ := db.CountOpenClaimReports(ctx); n != 0 {
		t.Errorf("reports survived the cached catalog they were found in: %d", n)
	}
}

// TestClaimGroupingContradiction: the peer asserts two renditions are the same
// recording, we hold both, and our own fingerprints say otherwise. No wire claim
// is involved — the assertion is testable without the peer's cooperation.
func TestClaimGroupingContradiction(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	a := fpWords(11, 400)
	b := fpWords(12, 400)
	sameA := seedFingerprintedFile(t, db, "ga", a)
	sameB := seedFingerprintedFile(t, db, "gb", fpWordsNear(a, 2))
	differs := seedFingerprintedFile(t, db, "gc", b)

	insertPeer(t, db, "bb22", "grouper", federation.PeerFriend)
	src := insertSource(t, db, "bb22")
	// One claimed recording per entry, each carrying two renditions we hold.
	honest := catEntry("1", "rec-honest", "Artist", "Album", "Honest", sameA)
	honest.Renditions = append(honest.Renditions, federation.CatalogRendition{Hash: sameB, Size: 2000, Codec: "mp3"})
	wrong := catEntry("2", "rec-wrong", "Artist", "Album", "Wrong", sameA)
	wrong.Renditions = append(wrong.Renditions, federation.CatalogRendition{Hash: differs, Size: 2000, Codec: "mp3"})
	if err := db.ReplaceSourceCatalog(ctx, src, "s1", 100, []federation.CatalogEntry{honest, wrong}); err != nil {
		t.Fatalf("ReplaceSourceCatalog: %v", err)
	}

	if _, err := db.CheckSourceClaims(ctx, src); err != nil {
		t.Fatalf("CheckSourceClaims: %v", err)
	}
	reports := claimReportRows(t, db)
	if len(reports) != 1 {
		t.Fatalf("len(reports) = %d, want 1 (only the wrong grouping)", len(reports))
	}
	r := reports[0]
	if r.Kind != federation.ClaimGrouping {
		t.Errorf("kind = %s, want grouping", r.Kind)
	}
	if r.Hash == "" || r.OtherHash == "" || r.Hash == r.OtherHash {
		t.Errorf("grouping report should name both blobs, got %q and %q", r.Hash, r.OtherHash)
	}
	if r.Hash > r.OtherHash {
		t.Errorf("hashes should be stored in a stable order (%q > %q), or the upsert key is not idempotent", r.Hash, r.OtherHash)
	}
}

// TestClaimTooShortToCheck: a head with almost nothing in it must produce no
// finding. A claim we cannot check is not a claim we distrust.
func TestClaimTooShortToCheck(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	held := seedFingerprintedFile(t, db, "sh", fpWords(21, 400))
	insertPeer(t, db, "cc33", "terse", federation.PeerFriend)
	src := insertSource(t, db, "cc33")
	e := catEntry("1", "rec-1", "Artist", "Album", "Terse", held)
	e.Renditions[0].Fingerprint = claimOf(fpWords(22, 4)) // 4 words of unrelated audio
	if err := db.ReplaceSourceCatalog(ctx, src, "s1", 100, []federation.CatalogEntry{e}); err != nil {
		t.Fatalf("ReplaceSourceCatalog: %v", err)
	}
	if open, err := db.CheckSourceClaims(ctx, src); err != nil || open != 0 {
		t.Errorf("CheckPeerClaims = %d, %v; want no finding from a head too short to compare", open, err)
	}
}

// TestPublishedCatalogCarriesFingerprintHead pins the wire half: the claim is
// published as a bounded head, never the whole fingerprint, and a blob without a
// fingerprint publishes no claim at all (uncheckable, not suspicious).
func TestPublishedCatalogCarriesFingerprintHead(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	full := fpWords(31, 900)
	seedFingerprintedFile(t, db, "pf", full)
	insertAnalysisFile(t, db, "np") // no fingerprint row

	entries, err := db.PublishedCatalog(ctx, federation.FriendAudience)
	if err != nil {
		t.Fatalf("PublishedCatalog: %v", err)
	}
	var withClaim, without int
	for _, e := range entries {
		for _, rd := range e.Renditions {
			if rd.Fingerprint == nil {
				without++
				continue
			}
			withClaim++
			if rd.Fingerprint.Words != len(full) {
				t.Errorf("claim words = %d, want the whole fingerprint's length %d", rd.Fingerprint.Words, len(full))
			}
			head, err := base64.StdEncoding.DecodeString(rd.Fingerprint.Head)
			if err != nil {
				t.Fatalf("head is not base64: %v", err)
			}
			if got := len(head) / 4; got != federation.ClaimHeadWords {
				t.Errorf("published head = %d words, want the %d-word bound", got, federation.ClaimHeadWords)
			}
			if rd.Fingerprint.Algo != "chromaprint" || rd.Fingerprint.Version != "1.5.1" {
				t.Errorf("claim provenance = %q/%q, want the fingerprinter recorded",
					rd.Fingerprint.Algo, rd.Fingerprint.Version)
			}
		}
	}
	if withClaim != 1 || without != 1 {
		t.Errorf("claims published = %d with, %d without; want exactly one of each", withClaim, without)
	}
}
