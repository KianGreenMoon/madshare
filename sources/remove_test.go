package sources_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/sources"
	"daemonlord.ygg/madshare/storages"
)

// A scan attributes every file it links to its source (migration 023).
func TestScan_Attributes(t *testing.T) {
	root := t.TempDir()
	writeAudio(t, root, "a.mp3", "alpha")
	writeAudio(t, root, "b.mp3", "bravo")

	store := newFakeStore()
	m := newManager(t, store, root)
	src, err := m.Add(context.Background(), "x", root, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	m.Wait()

	store.mu.Lock()
	n := len(store.attrib[src.ID])
	store.mu.Unlock()
	if n != 2 {
		t.Errorf("attributed files = %d, want 2", n)
	}
}

// Refresh re-walks the existing source row additively: it links a newly appeared
// file, re-affirms attribution for the already-linked one, and never duplicates
// the data_sources row.
func TestRescan_AdditiveReusesRow(t *testing.T) {
	root := t.TempDir()
	writeAudio(t, root, "a.mp3", "alpha")

	store := newFakeStore()
	m := newManager(t, store, root)
	src, err := m.Add(context.Background(), "x", root, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	m.Wait()
	if store.inserts != 1 {
		t.Fatalf("first scan inserts = %d, want 1", store.inserts)
	}

	writeAudio(t, root, "b.mp3", "bravo") // appears between scans
	rs, err := m.Rescan(context.Background(), src.ID, sql.NullInt64{})
	if err != nil {
		t.Fatalf("Rescan: %v", err)
	}
	if rs.Status != database.SourceStatusScanning {
		t.Errorf("rescan status = %q, want scanning", rs.Status)
	}
	m.Wait()

	if store.inserts != 2 {
		t.Errorf("inserts after rescan = %d, want 2 (b linked)", store.inserts)
	}
	store.mu.Lock()
	nAttr, nSrc := len(store.attrib[src.ID]), len(store.sources)
	store.mu.Unlock()
	if nAttr != 2 {
		t.Errorf("attributed = %d, want 2", nAttr)
	}
	if nSrc != 1 {
		t.Errorf("data_sources rows = %d, want 1 (row reused)", nSrc)
	}
	got, _ := store.GetDataSource(context.Background(), src.ID)
	if got.Status != database.SourceStatusActive {
		t.Errorf("final status = %q, want active", got.Status)
	}
}

func TestRescan_NotFound(t *testing.T) {
	store := newFakeStore()
	m := newManager(t, store, t.TempDir())
	if _, err := m.Rescan(context.Background(), "nope", sql.NullInt64{}); !errors.Is(err, database.ErrSourceNotFound) {
		t.Errorf("Rescan(unknown) = %v, want ErrSourceNotFound", err)
	}
}

// Remove is relation-aware: a record shared with another source is kept; one this
// source references exclusively is unlinked + deleted. The external original is
// never our concern here (the links storage only holds symlinks).
func TestRemove_KeepsShared_DeletesExclusive(t *testing.T) {
	ctx := context.Background()
	rootA := t.TempDir()
	rootB := t.TempDir()
	writeAudio(t, rootA, "shared.mp3", "shared-bytes")
	writeAudio(t, rootA, "onlyA.mp3", "onlyA-bytes")
	writeAudio(t, rootB, "shared.mp3", "shared-bytes") // same content → same hash

	store := newFakeStore()
	linker := storages.NewLinker(t.TempDir())
	m := sources.New(store, linker, nil, []string{rootA, rootB}, accepted)

	srcA, err := m.Add(ctx, "A", rootA, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	m.Wait()
	srcB, err := m.Add(ctx, "B", rootB, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	m.Wait()

	sharedHash := sha256Hex("shared-bytes")
	onlyAHash := sha256Hex("onlyA-bytes")

	res, err := m.Remove(ctx, srcA.ID)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if res.WillRemove != 1 || res.WillKeep != 1 {
		t.Errorf("removal = %+v, want {remove:1 keep:1}", res)
	}

	// onlyA is gone (catalog + link); shared survives, now owned by B alone.
	store.mu.Lock()
	_, onlyAExists := store.byHash[onlyAHash]
	sharedFile, sharedExists := store.byHash[sharedHash]
	_, srcAExists := store.sources[srcA.ID]
	sharedAttrB := sharedFile != nil && store.attrib[srcB.ID][sharedFile.ID]
	store.mu.Unlock()
	if onlyAExists {
		t.Error("exclusive file onlyA should be deleted")
	}
	if !sharedExists {
		t.Error("shared file should be kept")
	}
	if srcAExists {
		t.Error("source A row should be deleted")
	}
	if !sharedAttrB {
		t.Error("shared file should remain attributed to source B")
	}
	if has, _ := linker.Has(onlyAHash); has {
		t.Error("onlyA link should be reclaimed")
	}
	if has, _ := linker.Has(sharedHash); !has {
		t.Error("shared link should be kept")
	}
}

// A local upload the scan merely re-linked is kept on Remove (owned bytes); only
// the stray symlink is reclaimed.
func TestRemove_KeepsLocalUpload(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeAudio(t, root, "dup.mp3", "same-bytes")

	store := newFakeStore()
	h := sha256Hex("same-bytes")
	store.byHash[h] = &database.File{ID: 99, Hash: h, StorageBackend: database.StorageBackendLocal}

	linker := storages.NewLinker(t.TempDir())
	m := sources.New(store, linker, nil, []string{root}, accepted)
	src, err := m.Add(ctx, "x", root, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	m.Wait()
	if has, _ := linker.Has(h); !has {
		t.Fatal("scan should have created a resilience link for the local hash")
	}

	res, err := m.Remove(ctx, src.ID)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if res.WillRemove != 0 || res.WillKeep != 1 {
		t.Errorf("removal = %+v, want {remove:0 keep:1} (local kept)", res)
	}
	if _, ok := store.byHash[h]; !ok {
		t.Error("local upload row must be kept")
	}
	if has, _ := linker.Has(h); has {
		t.Error("stray resilience link should be reclaimed")
	}
}

// RemovalPreview reports the same counts as Remove without mutating anything.
func TestRemovalPreview_ReadOnly(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeAudio(t, root, "a.mp3", "alpha")
	writeAudio(t, root, "b.mp3", "bravo")

	store := newFakeStore()
	m := newManager(t, store, root)
	src, err := m.Add(ctx, "x", root, sql.NullInt64{})
	if err != nil {
		t.Fatal(err)
	}
	m.Wait()

	prev, err := m.RemovalPreview(ctx, src.ID)
	if err != nil {
		t.Fatalf("RemovalPreview: %v", err)
	}
	if prev.WillRemove != 2 || prev.WillKeep != 0 {
		t.Errorf("preview = %+v, want {remove:2 keep:0}", prev)
	}
	store.mu.Lock()
	stillThere := len(store.byHash)
	srcThere := len(store.sources)
	store.mu.Unlock()
	if stillThere != 2 || srcThere != 1 {
		t.Errorf("preview mutated state: files=%d sources=%d", stillThere, srcThere)
	}
}

// BackfillAttribution attributes a pre-023 source's linked files by matching
// link_target under its (resolved) root, and is idempotent.
func TestBackfillAttribution(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}

	store := newFakeStore()
	store.sources["s1"] = &database.DataSource{ID: "s1", Kind: database.SourceKindSymlink, Name: "legacy", RootPath: root}
	store.byHash["h1"] = &database.File{
		ID: 1, Hash: "h1", StorageBackend: database.StorageBackendLinks,
		LinkTarget: sql.NullString{String: filepath.Join(realRoot, "album/x.flac"), Valid: true},
	}
	store.byHash["h2"] = &database.File{ // outside the root → not attributed
		ID: 2, Hash: "h2", StorageBackend: database.StorageBackendLinks,
		LinkTarget: sql.NullString{String: "/elsewhere/y.flac", Valid: true},
	}

	m := newManager(t, store, root)
	if err := m.BackfillAttribution(ctx); err != nil {
		t.Fatalf("BackfillAttribution: %v", err)
	}

	store.mu.Lock()
	got1, got2 := store.attrib["s1"][1], store.attrib["s1"][2]
	store.mu.Unlock()
	if !got1 {
		t.Error("file under the root should be attributed")
	}
	if got2 {
		t.Error("file outside the root must not be attributed")
	}

	// Idempotent: a source that now has attribution is skipped.
	if err := m.BackfillAttribution(ctx); err != nil {
		t.Fatalf("second BackfillAttribution: %v", err)
	}
	store.mu.Lock()
	n := len(store.attrib["s1"])
	store.mu.Unlock()
	if n != 1 {
		t.Errorf("attributed after re-run = %d, want 1", n)
	}
}
