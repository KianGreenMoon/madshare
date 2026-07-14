package database

import (
	"context"
	"database/sql"
	"testing"
)

// fakeProbe is an in-test blobProbe backed by a set of "present" hashes and an
// "intact" set (for the deep scan). A hash absent from intact but present in
// present models a corrupted blob. deleted records DeleteAll sweeps.
type fakeProbe struct {
	present map[string]bool
	intact  map[string]bool
	deleted map[string]bool
	err     error
}

func (p *fakeProbe) BlobPresent(hash string) (bool, error) {
	if p.err != nil {
		return false, p.err
	}
	return p.present[hash], nil
}

func (p *fakeProbe) VerifyBlob(hash string) (bool, error) {
	if p.err != nil {
		return false, p.err
	}
	// A present blob with no explicit intact map defaults to intact.
	if p.intact == nil {
		return p.present[hash], nil
	}
	return p.intact[hash], nil
}

func (p *fakeProbe) DeleteAll(hash string) (bool, error) {
	if p.deleted == nil {
		p.deleted = map[string]bool{}
	}
	p.deleted[hash] = true
	return true, nil
}

// seedFile inserts a files row (plus one upload) and returns its hash.
func seedFile(t *testing.T, db *DB, hash, filename string) {
	t.Helper()
	f := newFile(hash)
	if err := db.InsertFile(context.Background(), f, newUpload(filename), newMeta()); err != nil {
		t.Fatalf("seed %s: %v", hash, err)
	}
}

const (
	hashHealthy  = "1111000000000000000000000000000000000000000000000000000000000000"
	hashDangling = "2222000000000000000000000000000000000000000000000000000000000000"
)

// TestPruneDangling_DryRunReportsButKeeps verifies a dry run reports the
// dangling row but deletes nothing.
func TestPruneDangling_DryRunReportsButKeeps(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedFile(t, db, hashHealthy, "healthy.mp3")
	seedFile(t, db, hashDangling, "gone.mp3")

	probe := &fakeProbe{present: map[string]bool{hashHealthy: true}} // dangling blob missing

	res, err := PruneDangling(ctx, db, probe, nil, false, false)
	if err != nil {
		t.Fatalf("PruneDangling: %v", err)
	}
	if res.Scanned != 2 {
		t.Errorf("Scanned = %d, want 2", res.Scanned)
	}
	if len(res.Dangling) != 1 || res.Dangling[0].Hash != hashDangling {
		t.Errorf("Dangling = %+v, want one entry for %s", res.Dangling, hashDangling)
	}
	if len(res.Pruned) != 0 {
		t.Errorf("Pruned = %+v, want empty on dry run", res.Pruned)
	}

	// Nothing deleted.
	var files int
	db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&files)
	if files != 2 {
		t.Errorf("files rows = %d, want 2 (dry run must not delete)", files)
	}
}

// TestPruneDangling_ConfirmDeletesDanglingOnly verifies confirm=true prunes the
// dangling row, leaves the healthy one, and is idempotent on a re-run.
func TestPruneDangling_ConfirmDeletesDanglingOnly(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedFile(t, db, hashHealthy, "healthy.mp3")
	seedFile(t, db, hashDangling, "gone.mp3")

	probe := &fakeProbe{present: map[string]bool{hashHealthy: true}}

	res, err := PruneDangling(ctx, db, probe, nil, true, false)
	if err != nil {
		t.Fatalf("PruneDangling: %v", err)
	}
	if len(res.Pruned) != 1 || res.Pruned[0].Hash != hashDangling {
		t.Errorf("Pruned = %+v, want one entry for %s", res.Pruned, hashDangling)
	}
	if len(res.Failed) != 0 {
		t.Errorf("Failed = %+v, want empty", res.Failed)
	}

	// The healthy file survives; the dangling one is gone.
	if got, _ := db.GetFileByHash(ctx, hashHealthy); got == nil {
		t.Error("healthy file was pruned; want it kept")
	}
	if got, _ := db.GetFileByHash(ctx, hashDangling); got != nil {
		t.Error("dangling file still present after confirm prune")
	}

	// Idempotent re-run: nothing left to prune, no error.
	res2, err := PruneDangling(ctx, db, probe, nil, true, false)
	if err != nil {
		t.Fatalf("PruneDangling re-run: %v", err)
	}
	if len(res2.Dangling) != 0 || len(res2.Pruned) != 0 {
		t.Errorf("re-run found work: dangling=%v pruned=%v", res2.Dangling, res2.Pruned)
	}
}

// TestPruneDangling_RepairsRecordingAndReaps verifies the GC-model recording
// awareness: pruning a dangling last file leaves its recording file-less, so
// its appearance is TRASHED (the in-tx scoped reap — the catalog entry is
// preserved, only the lying row and its lost bytes go); and the standing
// post-prune reap likewise quarantines a separate crash-orphaned fileless
// recording's appearance, reported in InvalidRecordings.
func TestPruneDangling_RepairsRecordingAndReaps(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedFile(t, db, hashHealthy, "healthy.mp3")
	seedFile(t, db, hashDangling, "gone.mp3")

	var danglingRec int64
	if err := db.QueryRow(`SELECT recording_id FROM files WHERE hash=?`, hashDangling).Scan(&danglingRec); err != nil {
		t.Fatalf("read dangling recording: %v", err)
	}

	// A crash-orphaned fileless recording the standing reap must collect (the
	// per-row cascade never reaches it — it owns no file to prune).
	var orphanRec int64
	if err := db.QueryRow(`INSERT INTO recordings (created_at) VALUES (1700000000) RETURNING id`).Scan(&orphanRec); err != nil {
		t.Fatalf("insert orphan recording: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO tagsets (recording_id, title, review_state, is_primary, created_at) VALUES (?, 'orphan', 'approved', 1, 1700000000)`,
		orphanRec); err != nil {
		t.Fatalf("insert orphan tagset: %v", err)
	}

	probe := &fakeProbe{present: map[string]bool{hashHealthy: true}}
	res, err := PruneDangling(ctx, db, probe, nil, true, false)
	if err != nil {
		t.Fatalf("PruneDangling: %v", err)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM files WHERE recording_id=?`, danglingRec); n != 0 {
		t.Errorf("dangling file row survived prune: count=%d", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, danglingRec); n != 1 {
		t.Errorf("dangling file's recording destroyed, want kept (appearance in Trash): count=%d", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=? AND deleted_at IS NOT NULL`, danglingRec); n != 1 {
		t.Errorf("dangling file's appearance not trashed by the scoped reap: count=%d", n)
	}
	// Quarantined, not destroyed (GC model): the orphan's appearance moved to
	// Trash and the recording row remains until its last row is purged.
	if n := countRow(t, db, `SELECT COUNT(*) FROM tagsets WHERE recording_id=? AND deleted_at IS NOT NULL`, orphanRec); n != 1 {
		t.Errorf("orphan appearance not quarantined by the post-prune reap: trashed count=%d, want 1", n)
	}
	if n := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, orphanRec); n != 1 {
		t.Errorf("orphan recording destroyed instead of quarantined: count=%d, want 1", n)
	}
	if res.InvalidRecordings != 1 {
		t.Errorf("InvalidRecordings = %d, want 1 (the quarantined orphan appearance)", res.InvalidRecordings)
	}
	if got := countRow(t, db, `SELECT COUNT(*) FROM recordings WHERE id=?`, hashHealthyRec(t, db)); got != 1 {
		t.Errorf("healthy recording swept: count=%d", got)
	}
	assertInvariants(t, db)
}

func hashHealthyRec(t *testing.T, db *DB) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT recording_id FROM files WHERE hash=?`, hashHealthy).Scan(&id); err != nil {
		t.Fatalf("read healthy recording: %v", err)
	}
	return id
}

// TestPruneDangling_DeepDetectsCorruption verifies the opt-in deep scan flags a
// present-but-corrupted blob (Issue 1) while the cheap scan leaves it alone, and
// that the reason is reported and the blob swept on confirm.
func TestPruneDangling_DeepDetectsCorruption(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedFile(t, db, hashHealthy, "good.mp3")
	seedFile(t, db, hashDangling, "rotted.mp3")

	// Both blobs are present, but hashDangling's content no longer matches.
	probe := &fakeProbe{
		present: map[string]bool{hashHealthy: true, hashDangling: true},
		intact:  map[string]bool{hashHealthy: true}, // hashDangling corrupted
	}

	// Cheap scan: both present, nothing flagged.
	shallow, err := PruneDangling(ctx, db, probe, nil, false, false)
	if err != nil {
		t.Fatalf("shallow PruneDangling: %v", err)
	}
	if len(shallow.Dangling) != 0 {
		t.Errorf("shallow Dangling = %+v, want none (corruption invisible to cheap scan)", shallow.Dangling)
	}

	// Deep scan (dry run): the corrupted blob is flagged with reason "corrupt".
	deep, err := PruneDangling(ctx, db, probe, nil, false, true)
	if err != nil {
		t.Fatalf("deep PruneDangling: %v", err)
	}
	if !deep.Deep {
		t.Error("result.Deep = false, want true")
	}
	if len(deep.Dangling) != 1 || deep.Dangling[0].Hash != hashDangling || deep.Dangling[0].Reason != ReasonCorrupt {
		t.Fatalf("deep Dangling = %+v, want one corrupt entry for %s", deep.Dangling, hashDangling)
	}

	// Deep scan (confirm): prunes the row and sweeps the bad blob.
	committed, err := PruneDangling(ctx, db, probe, nil, true, true)
	if err != nil {
		t.Fatalf("deep confirm PruneDangling: %v", err)
	}
	if len(committed.Pruned) != 1 || committed.Pruned[0].Reason != ReasonCorrupt {
		t.Fatalf("deep Pruned = %+v, want one corrupt entry", committed.Pruned)
	}
	if !probe.deleted[hashDangling] {
		t.Error("corrupt blob was not swept via DeleteAll on confirm")
	}
	if got, _ := db.GetFileByHash(ctx, hashHealthy); got == nil {
		t.Error("healthy file was pruned; want it kept")
	}
}

// TestPruneDangling_AllHealthy verifies that when every blob exists nothing is
// reported or deleted.
func TestPruneDangling_AllHealthy(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedFile(t, db, hashHealthy, "a.mp3")
	seedFile(t, db, hashDangling, "b.mp3")

	probe := &fakeProbe{present: map[string]bool{hashHealthy: true, hashDangling: true}}

	res, err := PruneDangling(ctx, db, probe, nil, true, false)
	if err != nil {
		t.Fatalf("PruneDangling: %v", err)
	}
	if len(res.Dangling) != 0 {
		t.Errorf("Dangling = %+v, want none", res.Dangling)
	}
	if len(res.Pruned) != 0 {
		t.Errorf("Pruned = %+v, want none", res.Pruned)
	}
}

// fakeLinkProbe is an in-test linkProbe. Each hash maps to its on-disk link state.
type linkState struct {
	target  string // recorded readlink target ("" + exists models a non-symlink entry)
	exists  bool   // a link entry is present at all
	present bool   // the target stats as a regular file
	intact  bool   // (deep) the target still hashes to the digest
}

type fakeLinkProbe struct {
	links   map[string]linkState
	removed map[string]bool
}

func (p *fakeLinkProbe) LinkInfo(hash string) (string, bool, bool, error) {
	s := p.links[hash]
	return s.target, s.exists, s.present, nil
}

func (p *fakeLinkProbe) VerifyLink(hash string) (bool, error) {
	return p.links[hash].intact, nil
}

func (p *fakeLinkProbe) Remove(hash string) error {
	if p.removed == nil {
		p.removed = map[string]bool{}
	}
	p.removed[hash] = true
	return nil
}

// seedLinkFile inserts a links-backed files row with the given recorded target.
func seedLinkFile(t *testing.T, db *DB, hash, filename, target string) {
	t.Helper()
	f := newFile(hash)
	f.StorageBackend = StorageBackendLinks
	f.LinkTarget = sql.NullString{String: target, Valid: true}
	if err := db.InsertFile(context.Background(), f, newUpload(filename), newMeta()); err != nil {
		t.Fatalf("seed link %s: %v", hash, err)
	}
}

const (
	hashLinkOK     = "3333000000000000000000000000000000000000000000000000000000000000"
	hashLinkGone   = "4444000000000000000000000000000000000000000000000000000000000000"
	hashRetargeted = "5555000000000000000000000000000000000000000000000000000000000000"
)

// TestPrune_LinksBrokenDetection verifies the prune classifies links rows by
// their symlink health (healthy / dangling / retargeted), separately from local
// blobs, and that a confirmed prune unlinks only the symlink (Remove), never the
// blob store's DeleteAll.
func TestPrune_LinksBrokenDetection(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	// A healthy local blob, plus three links rows in different states.
	seedFile(t, db, hashHealthy, "local.mp3")
	seedLinkFile(t, db, hashLinkOK, "ok.flac", "/srv/music/ok.flac")
	seedLinkFile(t, db, hashLinkGone, "gone.flac", "/srv/music/gone.flac")
	seedLinkFile(t, db, hashRetargeted, "moved.flac", "/srv/music/moved.flac")

	blob := &fakeProbe{present: map[string]bool{hashHealthy: true}}
	link := &fakeLinkProbe{links: map[string]linkState{
		hashLinkOK:     {target: "/srv/music/ok.flac", exists: true, present: true},
		hashLinkGone:   {target: "/srv/music/gone.flac", exists: true, present: false}, // dangling
		hashRetargeted: {target: "/srv/music/SOMEWHERE-ELSE.flac", exists: true, present: true},
	}}

	scan, err := ScanDangling(ctx, db, blob, link, false, nil)
	if err != nil {
		t.Fatalf("ScanDangling: %v", err)
	}
	got := map[string]string{}
	for _, d := range scan.Dangling {
		got[d.Hash] = d.Reason
	}
	if got[hashLinkGone] != ReasonDangling {
		t.Errorf("gone link reason = %q, want %q", got[hashLinkGone], ReasonDangling)
	}
	if got[hashRetargeted] != ReasonRetargeted {
		t.Errorf("retargeted link reason = %q, want %q", got[hashRetargeted], ReasonRetargeted)
	}
	if _, flagged := got[hashLinkOK]; flagged {
		t.Errorf("healthy link should not be flagged")
	}
	if _, flagged := got[hashHealthy]; flagged {
		t.Errorf("healthy local blob should not be flagged")
	}
	if len(scan.Dangling) != 2 {
		t.Fatalf("Dangling = %d, want 2 (dangling + retargeted)", len(scan.Dangling))
	}

	// Confirm: prune the two broken links. Each must be unlinked via Remove, and
	// the local blob store's DeleteAll must NOT be called for a links hash.
	pruned, err := PruneRefs(ctx, db, blob, link, false, scan.Dangling, nil)
	if err != nil {
		t.Fatalf("PruneRefs: %v", err)
	}
	if len(pruned.Pruned) != 2 {
		t.Errorf("Pruned = %d, want 2", len(pruned.Pruned))
	}
	if !link.removed[hashLinkGone] || !link.removed[hashRetargeted] {
		t.Errorf("broken links not unlinked: removed=%v", link.removed)
	}
	if blob.deleted[hashLinkGone] || blob.deleted[hashRetargeted] {
		t.Errorf("local DeleteAll must never run on a links hash: %v", blob.deleted)
	}
	// The rows are gone; the healthy ones remain.
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&n)
	if n != 2 {
		t.Errorf("files rows = %d, want 2 (healthy local + healthy link survive)", n)
	}
}

// TestPrune_LinksDeepCorruption verifies the deep scan flags a links target whose
// bytes no longer hash to the digest (ReasonCorrupt).
func TestPrune_LinksDeepCorruption(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedLinkFile(t, db, hashLinkOK, "ok.flac", "/srv/music/ok.flac")

	blob := &fakeProbe{}
	link := &fakeLinkProbe{links: map[string]linkState{
		hashLinkOK: {target: "/srv/music/ok.flac", exists: true, present: true, intact: false},
	}}

	shallow, err := ScanDangling(ctx, db, blob, link, false, nil)
	if err != nil {
		t.Fatalf("shallow: %v", err)
	}
	if len(shallow.Dangling) != 0 {
		t.Errorf("shallow scan flagged a present link: %+v", shallow.Dangling)
	}

	deep, err := ScanDangling(ctx, db, blob, link, true, nil)
	if err != nil {
		t.Fatalf("deep: %v", err)
	}
	if len(deep.Dangling) != 1 || deep.Dangling[0].Reason != ReasonCorrupt {
		t.Errorf("deep scan = %+v, want one ReasonCorrupt", deep.Dangling)
	}
}
