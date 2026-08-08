package sources_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/sources"
	"daemonlord.ygg/madshare/storages"
)

// accepted is a minimal extension→MIME allow-list for tests.
var accepted = map[string]string{".mp3": "audio/mpeg", ".flac": "audio/flac"}

// fakeStore is an in-memory sources.Store. It is safe for concurrent use: the
// scan goroutine writes while the test reads after Wait().
type fakeStore struct {
	mu       sync.Mutex
	sources  map[string]*database.DataSource
	byHash   map[string]*database.File
	nextID   int64
	inserts  int
	uploads  int
	enqueued int
	insertFn func() error // optional hook to fail/block InsertFile

	finished []finishCall

	// per-source attribution (migration 023): sourceID -> set of file ids.
	attrib map[string]map[int64]bool

	// cover bookkeeping (P4)
	albumCovers map[int64]bool // albumID -> already has a cover
	coverClaims int            // SetAlbumCoverIfAbsent that inserted
	imageJobs   int            // EnqueueImageJob calls
}

type finishCall struct {
	id, status, summary string
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		sources:     map[string]*database.DataSource{},
		byHash:      map[string]*database.File{},
		attrib:      map[string]map[int64]bool{},
		albumCovers: map[int64]bool{},
	}
}

func (s *fakeStore) ResolveAlbumID(_ context.Context, artist, album string) (int64, error) {
	// A stable positive id per (artist, album) is enough for the cover path.
	return 1, nil
}

func (s *fakeStore) HasAlbumCover(_ context.Context, albumID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.albumCovers[albumID], nil
}

func (s *fakeStore) SetAlbumCoverIfAbsent(_ context.Context, albumID int64, _, _, _, _ string, _ int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.albumCovers[albumID] {
		return false, nil
	}
	s.albumCovers[albumID] = true
	s.coverClaims++
	return true, nil
}

func (s *fakeStore) EnqueueImageJob(_ context.Context, _, _, _ string, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.imageJobs++
	return nil
}

func (s *fakeStore) InsertDataSource(_ context.Context, ds *database.DataSource) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := *ds
	s.sources[ds.ID] = &cp
	return nil
}

func (s *fakeStore) ListDataSources(context.Context) ([]*database.DataSource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*database.DataSource, 0, len(s.sources))
	for _, v := range s.sources {
		cp := *v
		out = append(out, &cp)
	}
	return out, nil
}

func (s *fakeStore) GetDataSource(_ context.Context, id string) (*database.DataSource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ds, ok := s.sources[id]
	if !ok {
		return nil, database.ErrSourceNotFound
	}
	cp := *ds
	return &cp, nil
}

func (s *fakeStore) FinishDataSourceScan(_ context.Context, id, status, summaryJSON string, scannedAt int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ds := s.sources[id]; ds != nil {
		ds.Status = status
		ds.SummaryJSON = sql.NullString{String: summaryJSON, Valid: true}
		ds.ScannedAt = sql.NullInt64{Int64: scannedAt, Valid: true}
	}
	s.finished = append(s.finished, finishCall{id, status, summaryJSON})
	return nil
}

func (s *fakeStore) ResetStaleScans(context.Context) (int64, error) { return 0, nil }

func (s *fakeStore) GetFileByHash(_ context.Context, hash string) (*database.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byHash[hash], nil
}

func (s *fakeStore) InsertFile(_ context.Context, f *database.File, _ *database.FileUpload, _ *database.MediaMetadata) error {
	if s.insertFn != nil {
		if err := s.insertFn(); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	f.ID = s.nextID
	s.byHash[f.Hash] = f
	s.inserts++
	return nil
}

func (s *fakeStore) RecordUpload(_ context.Context, _ int64, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.uploads++
	return nil
}

func (s *fakeStore) EnqueueAnalysisJob(_ context.Context, _, _ int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enqueued++
	return nil
}

func (s *fakeStore) UpdateDataSourceStatus(_ context.Context, id, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ds := s.sources[id]; ds != nil {
		ds.Status = status
	}
	return nil
}

func (s *fakeStore) DeleteDataSource(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sources, id)
	delete(s.attrib, id) // ON DELETE CASCADE
	return nil
}

func (s *fakeStore) AttributeSourceFile(_ context.Context, sourceID string, fileID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attrib[sourceID] == nil {
		s.attrib[sourceID] = map[int64]bool{}
	}
	s.attrib[sourceID][fileID] = true
	return nil
}

func (s *fakeStore) SourceHasAttribution(_ context.Context, sourceID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.attrib[sourceID]) > 0, nil
}

func (s *fakeStore) CountSourceFiles(_ context.Context, sourceID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.attrib[sourceID]), nil
}

func (s *fakeStore) SourceExclusiveFiles(_ context.Context, sourceID string) ([]database.SourceFileRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	byID := map[int64]*database.File{}
	for _, f := range s.byHash {
		byID[f.ID] = f
	}
	var out []database.SourceFileRef
	for fid := range s.attrib[sourceID] {
		exclusive := true
		for sid, set := range s.attrib {
			if sid != sourceID && set[fid] {
				exclusive = false
				break
			}
		}
		f := byID[fid]
		if !exclusive || f == nil {
			continue
		}
		out = append(out, database.SourceFileRef{ID: fid, Hash: f.Hash, StorageBackend: f.StorageBackend})
	}
	return out, nil
}

func (s *fakeStore) ListLinkFiles(_ context.Context) ([]database.LinkFileRef, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []database.LinkFileRef
	for _, f := range s.byHash {
		if f.StorageBackend == database.StorageBackendLinks && f.LinkTarget.Valid && f.LinkTarget.String != "" {
			out = append(out, database.LinkFileRef{ID: f.ID, LinkTarget: f.LinkTarget.String})
		}
	}
	return out, nil
}

func (s *fakeStore) HardDeleteFileByHash(_ context.Context, hash string) ([]string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.byHash[hash]
	if !ok {
		return nil, false, nil
	}
	delete(s.byHash, hash)
	for _, set := range s.attrib { // ON DELETE CASCADE on file_id
		delete(set, f.ID)
	}
	return []string{}, true, nil
}

// writeAudio creates root/name with content and returns the dir's path.
func writeAudio(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newManager(t *testing.T, store sources.Store, root string) *sources.Manager {
	t.Helper()
	linker := storages.NewLinker(t.TempDir())
	return sources.New(store, linker, nil, []string{root}, accepted)
}

func TestAdd_Disabled(t *testing.T) {
	store := newFakeStore()
	m := sources.New(store, storages.NewLinker(t.TempDir()), nil, nil, accepted)
	if m.Enabled() {
		t.Fatal("manager with no roots should be disabled")
	}
	if _, err := m.Add(context.Background(), "x", "/srv/music", sql.NullInt64{}); err != sources.ErrDisabled {
		t.Errorf("Add = %v, want ErrDisabled", err)
	}
}

func TestAdd_RootNotAllowed(t *testing.T) {
	allowed := t.TempDir()
	outside := t.TempDir() // a sibling temp dir, not under the allow-list
	store := newFakeStore()
	m := newManager(t, store, allowed)

	if _, err := m.Add(context.Background(), "x", outside, sql.NullInt64{}); err != sources.ErrRootNotAllowed {
		t.Errorf("Add(outside) = %v, want ErrRootNotAllowed", err)
	}
	if _, err := m.Add(context.Background(), "x", "relative/path", sql.NullInt64{}); err != sources.ErrInvalidRoot {
		t.Errorf("Add(relative) = %v, want ErrInvalidRoot", err)
	}
}

// WithAnyRoot is what an embedded, listener-less madshare uses instead of an
// allow-list (docs/architecture/embedding.md): any absolute directory is
// importable, but a relative path is still refused — that check is about a path
// being unambiguous, not about trust.
func TestAdd_AnyRoot(t *testing.T) {
	outside := t.TempDir()
	store := newFakeStore()
	m := sources.New(store, storages.NewLinker(t.TempDir()), nil, nil, accepted).WithAnyRoot(true)

	if !m.Enabled() {
		t.Fatal("allow_any should enable the manager with no roots")
	}
	if _, err := m.Add(context.Background(), "x", outside, sql.NullInt64{}); err != nil {
		t.Errorf("Add(outside) with allow_any = %v, want success", err)
	}
	m.Wait()
	if _, err := m.Add(context.Background(), "y", "relative/path", sql.NullInt64{}); err != sources.ErrInvalidRoot {
		t.Errorf("Add(relative) with allow_any = %v, want ErrInvalidRoot", err)
	}
	// A missing directory wraps the sentinel with the lstat detail, so match on
	// the chain rather than on identity.
	if _, err := m.Add(context.Background(), "z", filepath.Join(outside, "not-there"), sql.NullInt64{}); !errors.Is(err, sources.ErrInvalidRoot) {
		t.Errorf("Add(missing dir) with allow_any = %v, want ErrInvalidRoot", err)
	}
}

func TestScan_LinksAndIngests(t *testing.T) {
	root := t.TempDir()
	writeAudio(t, root, "a.mp3", "alpha")
	writeAudio(t, root, "b.flac", "bravo")
	writeAudio(t, root, "notes.txt", "ignore me") // non-audio: not counted
	sub := filepath.Join(root, "disc2")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeAudio(t, sub, "c.mp3", "charlie") // nested file is found too

	store := newFakeStore()
	m := newManager(t, store, root)

	src, err := m.Add(context.Background(), "My music", root, sql.NullInt64{Int64: 7, Valid: true})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if src.Status != database.SourceStatusScanning {
		t.Errorf("freshly added source status = %q, want scanning", src.Status)
	}
	m.Wait()

	if store.inserts != 3 {
		t.Errorf("inserts = %d, want 3 (a.mp3, b.flac, c.mp3)", store.inserts)
	}
	if store.enqueued != 3 {
		t.Errorf("analysis enqueued = %d, want 3", store.enqueued)
	}
	// Every inserted file is a links row with a link_target and approved.
	store.mu.Lock()
	for h, f := range store.byHash {
		if f.StorageBackend != database.StorageBackendLinks {
			t.Errorf("file %s storage_backend = %q, want links", h, f.StorageBackend)
		}
		if !f.LinkTarget.Valid || f.LinkTarget.String == "" {
			t.Errorf("file %s missing link_target", h)
		}
		if f.ReviewState != database.ReviewApproved {
			t.Errorf("file %s review = %q, want approved", h, f.ReviewState)
		}
		if f.UploadedBy.Int64 != 7 {
			t.Errorf("file %s uploaded_by = %v, want 7", h, f.UploadedBy)
		}
	}
	store.mu.Unlock()

	got, _ := store.GetDataSource(context.Background(), src.ID)
	if got.Status != database.SourceStatusActive {
		t.Errorf("final status = %q, want active", got.Status)
	}
	final := toSummary(t, got.SummaryJSON.String)
	if final.Scanned != 3 || final.Linked != 3 || final.Skipped != 0 || final.Failed != 0 {
		t.Errorf("summary = %+v, want scanned=3 linked=3", final)
	}
}

// A second scan of the same source finds every hash already in the links storage
// and skips it (idempotent, one link per hash).
func TestScan_RescanSkips(t *testing.T) {
	root := t.TempDir()
	writeAudio(t, root, "a.mp3", "alpha")
	writeAudio(t, root, "b.mp3", "bravo")

	store := newFakeStore()
	// Reuse one linker across both scans so the second sees the first's links.
	linker := storages.NewLinker(t.TempDir())
	m := sources.New(store, linker, nil, []string{root}, accepted)

	if _, err := m.Add(context.Background(), "x", root, sql.NullInt64{}); err != nil {
		t.Fatal(err)
	}
	m.Wait()
	if store.inserts != 2 {
		t.Fatalf("first scan inserts = %d, want 2", store.inserts)
	}

	src, err := m.Add(context.Background(), "x again", root, sql.NullInt64{})
	if err != nil {
		t.Fatalf("second Add: %v", err)
	}
	m.Wait()
	if store.inserts != 2 {
		t.Errorf("second scan inserts = %d, want still 2 (all skipped)", store.inserts)
	}
	got, _ := store.GetDataSource(context.Background(), src.ID)
	final := toSummary(t, got.SummaryJSON.String)
	if final.Skipped != 2 || final.Linked != 0 {
		t.Errorf("rescan summary = %+v, want skipped=2 linked=0", final)
	}
}

// When a hash already has a catalog row (e.g. a prior local upload), the scan
// reuses it: it links for resilience and records the filename but does NOT insert
// a second files row.
func TestScan_ReusesExistingRow(t *testing.T) {
	root := t.TempDir()
	writeAudio(t, root, "dup.mp3", "same-bytes")

	store := newFakeStore()
	// Pre-seed the catalog with the file's hash as a local upload.
	h := sha256Hex("same-bytes")
	store.byHash[h] = &database.File{ID: 99, Hash: h, StorageBackend: database.StorageBackendLocal}

	m := newManager(t, store, root)
	if _, err := m.Add(context.Background(), "x", root, sql.NullInt64{}); err != nil {
		t.Fatal(err)
	}
	m.Wait()

	if store.inserts != 0 {
		t.Errorf("inserts = %d, want 0 (existing row reused)", store.inserts)
	}
	if store.uploads != 1 {
		t.Errorf("RecordUpload calls = %d, want 1", store.uploads)
	}
}

func TestAdd_Busy(t *testing.T) {
	root := t.TempDir()
	writeAudio(t, root, "a.mp3", "alpha")

	store := newFakeStore()
	// Hold the first scan inside InsertFile until released, so it stays running.
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	store.insertFn = func() error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil
	}
	m := newManager(t, store, root)

	if _, err := m.Add(context.Background(), "first", root, sql.NullInt64{}); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	<-entered // the scan is now blocked inside InsertFile

	if _, err := m.Add(context.Background(), "second", root, sql.NullInt64{}); err != sources.ErrBusy {
		t.Errorf("concurrent Add = %v, want ErrBusy", err)
	}
	close(release)
	m.Wait()
	if m.Running() {
		t.Error("manager still running after Wait")
	}
}
