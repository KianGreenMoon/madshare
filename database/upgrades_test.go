package database

import (
	"context"
	"testing"

	"daemonlord.ygg/madshare/federation"
)

// seedUpgradeSource inserts a friend + its cache source and returns the source id.
func seedUpgradeSource(t *testing.T, db *DB, key string) int64 {
	t.Helper()
	insertPeer(t, db, key, "friendly", federation.PeerFriend)
	return insertSource(t, db, key)
}

func upgradeRows(t *testing.T, db *DB, disposition string) []*UpgradeRow {
	t.Helper()
	rows, _, err := db.ListUpgrades(context.Background(), disposition, 0, 100, 0)
	if err != nil {
		t.Fatalf("ListUpgrades: %v", err)
	}
	return rows
}

// TestScanFindsABetterRenditionOfWhatWeHold is stage 1 end to end: a friend's
// catalog entry advertises the very blob we hold *and* a lossless rendition of
// the same recording. The finding is the second one — and only the second one,
// because bytes we already have are not an upgrade to themselves.
func TestScanFindsABetterRenditionOfWhatWeHold(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	ours := seedFingerprintedFile(t, db, "ours", fpWords(1, 400))
	setRenditionTech(t, db, ours, "mp3", 192000, 44100, 0)
	src := seedUpgradeSource(t, db, "aa11")

	entry := catEntry("e1", "r1", "Artist", "Album", "Title", ours)
	entry.Renditions = append(entry.Renditions,
		federation.CatalogRendition{Hash: "flachash", Codec: "flac", SampleRate: 44100, BitDepth: 16, Size: 30000000},
		federation.CatalogRendition{Hash: "worsehash", Codec: "mp3", Bitrate: 128000, Size: 3000000},
	)
	if err := db.ReplaceSourceCatalog(ctx, src, "s1", 100, []federation.CatalogEntry{entry}); err != nil {
		t.Fatalf("ReplaceSourceCatalog: %v", err)
	}

	open, err := db.ScanSourceUpgrades(ctx, src, 1000)
	if err != nil {
		t.Fatalf("ScanSourceUpgrades: %v", err)
	}
	if open != 1 {
		t.Fatalf("open findings = %d, want 1 (the lossless one)", open)
	}
	rows := upgradeRows(t, db, "")
	if len(rows) != 1 {
		t.Fatalf("listed %d findings, want 1", len(rows))
	}
	u := rows[0]
	if u.RemoteHash != "flachash" {
		t.Errorf("finding is on %q, want flachash — the only rendition that beats ours", u.RemoteHash)
	}
	if u.Match != MatchHash {
		t.Errorf("match = %q, want %q (they hold our exact bytes)", u.Match, MatchHash)
	}
	if u.Ours.Hash != ours || u.Ours.Codec != "mp3" {
		t.Errorf("ours = %+v, want the mp3 it would upgrade", u.Ours)
	}
	if u.Offered.Codec != "flac" {
		t.Errorf("offered = %+v, want the claimed flac", u.Offered)
	}
	if u.Source != "friendly" || u.SourceKey != "aa11" {
		t.Errorf("source = %q/%q, want the advertising friend", u.Source, u.SourceKey)
	}
}

// TestScanFindsAReencodeByFingerprint is stage 2: nobody holds our bytes, but a
// friend advertises a lossless copy of the same audio. This is the case the
// whole feature exists for — a node holding our exact bytes by definition holds
// nothing better than them.
func TestScanFindsAReencodeByFingerprint(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	honest := fpWords(2, 400)
	ours := seedFingerprintedFile(t, db, "ours", honest)
	setRenditionTech(t, db, ours, "mp3", 192000, 44100, 0)
	src := seedUpgradeSource(t, db, "bb22")

	head := claimOf(fpWordsNear(honest, 3)[:federation.ClaimHeadWords])
	entry := catEntry("e1", "r1", "Artist", "Album", "Title", "flachash")
	entry.Duration = 200 // seedFingerprintedFile stores 200s
	entry.Renditions[0] = federation.CatalogRendition{
		Hash: "flachash", Codec: "flac", SampleRate: 44100, BitDepth: 16,
		Size: 30000000, Fingerprint: head,
	}
	// A different recording entirely, at a plausible duration: must not match.
	other := catEntry("e2", "r2", "Artist", "Album", "Other", "otherhash")
	other.Duration = 201
	other.Renditions[0].Fingerprint = claimOf(fpWords(9, 400)[:federation.ClaimHeadWords])
	other.Renditions[0].Codec, other.Renditions[0].Size = "flac", 30000000

	if err := db.ReplaceSourceCatalog(ctx, src, "s1", 100, []federation.CatalogEntry{entry, other}); err != nil {
		t.Fatalf("ReplaceSourceCatalog: %v", err)
	}

	if _, err := db.ScanSourceUpgrades(ctx, src, 1000); err != nil {
		t.Fatalf("ScanSourceUpgrades: %v", err)
	}
	rows := upgradeRows(t, db, "")
	if len(rows) != 1 {
		t.Fatalf("listed %d findings, want 1", len(rows))
	}
	if rows[0].RemoteHash != "flachash" || rows[0].Match != MatchFingerprint {
		t.Errorf("finding = %q via %q, want flachash via %q",
			rows[0].RemoteHash, rows[0].Match, MatchFingerprint)
	}
	if rows[0].BER > maxBitErrorRate {
		t.Errorf("BER = %f, want at or below the grouping threshold", rows[0].BER)
	}
}

// TestRescanKeepsADismissal is the reason findings are stored at all: an admin
// who said no must not be asked again on the next sync, fifteen minutes later,
// forever. Detection writes measurements; it never writes a disposition.
func TestRescanKeepsADismissal(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	ours := seedFingerprintedFile(t, db, "ours", fpWords(3, 400))
	setRenditionTech(t, db, ours, "mp3", 192000, 44100, 0)
	src := seedUpgradeSource(t, db, "cc33")

	entry := catEntry("e1", "r1", "Artist", "Album", "Title", ours)
	entry.Renditions = append(entry.Renditions,
		federation.CatalogRendition{Hash: "flachash", Codec: "flac", SampleRate: 44100, BitDepth: 16, Size: 30000000})
	if err := db.ReplaceSourceCatalog(ctx, src, "s1", 100, []federation.CatalogEntry{entry}); err != nil {
		t.Fatalf("ReplaceSourceCatalog: %v", err)
	}
	if _, err := db.ScanSourceUpgrades(ctx, src, 1000); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	rows := upgradeRows(t, db, "")
	if len(rows) != 1 {
		t.Fatalf("listed %d findings after the first scan, want 1", len(rows))
	}
	found, err := db.SetUpgradeDisposition(ctx, rows[0].ID, UpgradeDismissed)
	if err != nil || !found {
		t.Fatalf("dismiss: found=%v err=%v", found, err)
	}

	open, err := db.ScanSourceUpgrades(ctx, src, 2000)
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	if open != 0 {
		t.Errorf("rescan reopened %d finding(s); a dismissal must survive it", open)
	}
	all := upgradeRows(t, db, "all")
	if len(all) != 1 || all[0].Disposition != UpgradeDismissed {
		t.Fatalf("after rescan: %d row(s), disposition %q — want the dismissal intact",
			len(all), all[0].Disposition)
	}
	if all[0].LastSeen != 2000 {
		t.Errorf("last_seen = %d, want the rescan's clock: the measurement moves, the decision does not", all[0].LastSeen)
	}
}

// TestSweepDropsResolvedUpgrades: once we hold the bytes, the finding is not
// news any more, and a finding for a blob that left every catalog is a dead link.
func TestSweepDropsResolvedUpgrades(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	ours := seedFingerprintedFile(t, db, "ours", fpWords(4, 400))
	setRenditionTech(t, db, ours, "mp3", 192000, 44100, 0)
	src := seedUpgradeSource(t, db, "dd44")

	better := seedFingerprintedFile(t, db, "better", fpWords(5, 400))
	entry := catEntry("e1", "r1", "Artist", "Album", "Title", ours)
	entry.Renditions = append(entry.Renditions,
		federation.CatalogRendition{Hash: better, Codec: "flac", SampleRate: 44100, BitDepth: 16, Size: 30000000},
		federation.CatalogRendition{Hash: "goneha", Codec: "flac", SampleRate: 96000, BitDepth: 24, Size: 90000000})
	if err := db.ReplaceSourceCatalog(ctx, src, "s1", 100, []federation.CatalogEntry{entry}); err != nil {
		t.Fatalf("ReplaceSourceCatalog: %v", err)
	}
	// Record both findings against a recording that does not hold either blob.
	if _, err := db.Exec(`UPDATE files SET recording_id = (SELECT recording_id FROM files WHERE hash = ?)
	                       WHERE hash = ?`, ours, ours); err != nil {
		t.Fatalf("normalise recording: %v", err)
	}
	if _, err := db.ScanSourceUpgrades(ctx, src, 1000); err != nil {
		t.Fatalf("scan: %v", err)
	}

	// Now drop the second blob out of every cached catalog.
	entry.Renditions = entry.Renditions[:2]
	if err := db.ReplaceSourceCatalog(ctx, src, "s2", 200, []federation.CatalogEntry{entry}); err != nil {
		t.Fatalf("ReplaceSourceCatalog (shrunk): %v", err)
	}
	if err := db.SweepUpgrades(ctx); err != nil {
		t.Fatalf("SweepUpgrades: %v", err)
	}
	for _, u := range upgradeRows(t, db, "all") {
		if u.RemoteHash == "goneha" {
			t.Error("kept a finding for a blob no cached catalog advertises any more")
		}
		if u.RemoteHash == better {
			t.Error("kept a finding for bytes this node now holds; the upgrade already happened")
		}
	}
}

// setRenditionTech fills the ffprobe columns the quality ladder ranks on. Seeded
// files carry none, and the ladder degrades to size without them — which would
// make every test here a size comparison rather than a codec one.
func setRenditionTech(t *testing.T, db *DB, hash, codec string, bitrate, sampleRate, bitDepth int) {
	t.Helper()
	var fileID int64
	if err := db.QueryRow(`SELECT id FROM files WHERE hash = ?`, hash).Scan(&fileID); err != nil {
		t.Fatalf("file for %s: %v", hash, err)
	}
	// InsertFile already wrote the row (newMeta); this is the ffprobe pass
	// filling in what it learned.
	res, err := db.Exec(`
		UPDATE media_metadata SET codec = ?, bitrate = ?, sample_rate = ?, bit_depth = ?
		WHERE file_id = ?`, codec, bitrate, sampleRate, bitDepth, fileID)
	if err != nil {
		t.Fatalf("set tech for %s: %v", hash, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("set tech for %s: updated %d rows, want 1", hash, n)
	}
}
