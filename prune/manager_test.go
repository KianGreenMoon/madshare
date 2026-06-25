package prune

import (
	"context"
	"sync"
	"testing"
	"time"

	"daemonlord.ygg/madshare/database"
)

// fakeProbe is a controllable blob store. present/intact drive the scan; a gate
// channel can block BlobPresent so a test can hold a run "running" deterministically.
type fakeProbe struct {
	mu      sync.Mutex
	present map[string]bool
	intact  map[string]bool
	deleted map[string]bool
	gate    chan struct{} // when non-nil, BlobPresent blocks on it (and on ctx via the caller)
}

func (p *fakeProbe) BlobPresent(hash string) (bool, error) {
	if p.gate != nil {
		<-p.gate
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.present[hash], nil
}

func (p *fakeProbe) VerifyBlob(hash string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.intact == nil {
		return p.present[hash], nil
	}
	return p.intact[hash], nil
}

func (p *fakeProbe) DeleteAll(hash string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.deleted == nil {
		p.deleted = map[string]bool{}
	}
	p.deleted[hash] = true
	return true, nil
}

// memSettings is an in-memory SettingsStore.
type memSettings struct {
	mu sync.Mutex
	kv map[string]string
}

func newMemSettings() *memSettings { return &memSettings{kv: map[string]string{}} }

func (s *memSettings) GetSetting(_ context.Context, key string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.kv[key]
	return v, ok, nil
}

func (s *memSettings) SetSetting(_ context.Context, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kv[key] = value
	return nil
}

const (
	hashHealthy  = "1111000000000000000000000000000000000000000000000000000000000000"
	hashDangling = "2222000000000000000000000000000000000000000000000000000000000000"
)

func seed(t *testing.T, db *database.DB, hash, name string) {
	t.Helper()
	f := &database.File{Hash: hash, ByteSize: 1, MimeType: "audio/mpeg", StorageBackend: "local", ObjectKey: hash + "/" + name}
	up := &database.FileUpload{Filename: name, UploadedAt: 1700000000}
	if err := db.InsertFile(context.Background(), f, up, &database.MediaMetadata{Title: name}); err != nil {
		t.Fatalf("seed %s: %v", hash, err)
	}
}

func openDB(t *testing.T) *database.DB {
	t.Helper()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestScanThenPrune covers the normal flow: a scan flags the dangling row, then a
// prune (acting on the held reviewed set) deletes exactly it and persists summaries.
func TestScanThenPrune(t *testing.T) {
	db := openDB(t)
	seed(t, db, hashHealthy, "healthy.mp3")
	seed(t, db, hashDangling, "gone.mp3")
	probe := &fakeProbe{present: map[string]bool{hashHealthy: true}} // dangling blob missing
	settings := newMemSettings()
	m := New(db, probe, nil, settings)

	if _, err := m.StartScan(false, "alice"); err != nil {
		t.Fatalf("StartScan: %v", err)
	}
	m.Wait()

	snap := m.Snapshot()
	if snap.State != StateIdle {
		t.Errorf("state = %s, want idle", snap.State)
	}
	if snap.LastResult == nil || snap.LastResult.Kind != KindScan || len(snap.LastResult.Dangling) != 1 {
		t.Fatalf("scan detail = %+v, want one dangling", snap.LastResult)
	}
	if snap.LastScan == nil || snap.LastScan.Dangling != 1 || snap.LastScan.By != "alice" {
		t.Fatalf("last_scan summary = %+v", snap.LastScan)
	}

	if _, err := m.StartPrune("bob"); err != nil {
		t.Fatalf("StartPrune: %v", err)
	}
	m.Wait()

	snap = m.Snapshot()
	if snap.LastResult == nil || snap.LastResult.Kind != KindPrune || len(snap.LastResult.Pruned) != 1 {
		t.Fatalf("prune detail = %+v, want one pruned", snap.LastResult)
	}
	if snap.LastPrune == nil || snap.LastPrune.Pruned != 1 {
		t.Fatalf("last_prune summary = %+v", snap.LastPrune)
	}
	if got, _ := db.GetFileByHash(context.Background(), hashDangling); got != nil {
		t.Error("dangling row still present after prune")
	}
	if got, _ := db.GetFileByHash(context.Background(), hashHealthy); got == nil {
		t.Error("healthy row removed by prune")
	}
	// Summaries persisted under both keys.
	if _, ok, _ := settings.GetSetting(context.Background(), settingLastScan); !ok {
		t.Error("last_scan not persisted")
	}
	if _, ok, _ := settings.GetSetting(context.Background(), settingLastPrune); !ok {
		t.Error("last_prune not persisted")
	}
}

func TestStartPrune_NoScan(t *testing.T) {
	db := openDB(t)
	m := New(db, &fakeProbe{present: map[string]bool{}}, nil, newMemSettings())
	if _, err := m.StartPrune("alice"); err != ErrNoScan {
		t.Fatalf("StartPrune without scan = %v, want ErrNoScan", err)
	}
}

// TestBusyGuard holds a scan mid-flight (via the probe gate) and asserts a second
// start is refused with ErrBusy — the singleton guard.
func TestBusyGuard(t *testing.T) {
	db := openDB(t)
	seed(t, db, hashHealthy, "a.mp3")
	gate := make(chan struct{})
	probe := &fakeProbe{present: map[string]bool{hashHealthy: true}, gate: gate}
	m := New(db, probe, nil, newMemSettings())

	if _, err := m.StartScan(false, "alice"); err != nil {
		t.Fatalf("first StartScan: %v", err)
	}
	// The first run is now blocked in BlobPresent on the gate.
	waitFor(t, m.Running, true)

	if _, err := m.StartScan(false, "bob"); err != ErrBusy {
		t.Fatalf("second StartScan = %v, want ErrBusy", err)
	}

	close(gate) // release the first run
	m.Wait()
	if m.Running() {
		t.Error("still running after Wait")
	}
}

// TestCancel cancels a held run and asserts it ends as cancelled.
func TestCancel(t *testing.T) {
	db := openDB(t)
	seed(t, db, hashHealthy, "a.mp3")
	seed(t, db, hashDangling, "b.mp3")
	gate := make(chan struct{})
	probe := &fakeProbe{present: map[string]bool{hashHealthy: true, hashDangling: true}, gate: gate}
	m := New(db, probe, nil, newMemSettings())

	if _, err := m.StartScan(false, "alice"); err != nil {
		t.Fatalf("StartScan: %v", err)
	}
	waitFor(t, m.Running, true)

	if !m.Cancel() {
		t.Fatal("Cancel reported no running job")
	}
	close(gate) // unblock the probe so the cancelled ctx is observed and the run ends
	m.Wait()

	snap := m.Snapshot()
	if snap.LastScan == nil || snap.LastScan.Outcome != OutcomeCancelled {
		t.Fatalf("last_scan outcome = %+v, want cancelled", snap.LastScan)
	}
	// A no-op Cancel on the now-idle manager reports false.
	if m.Cancel() {
		t.Error("Cancel on idle manager reported true")
	}
}

// TestLoadsPersistedSummaries seeds the settings store and asserts New surfaces
// the prior summaries (survives a restart).
func TestLoadsPersistedSummaries(t *testing.T) {
	db := openDB(t)
	settings := newMemSettings()
	settings.SetSetting(context.Background(), settingLastPrune,
		`{"kind":"prune","scanned":5,"pruned_count":2,"outcome":"completed","by":"alice","finished_at":"2026-06-18T14:00:00Z"}`)
	m := New(db, &fakeProbe{present: map[string]bool{}}, nil, settings)
	snap := m.Snapshot()
	if snap.LastPrune == nil || snap.LastPrune.Pruned != 2 || snap.LastPrune.By != "alice" {
		t.Fatalf("loaded last_prune = %+v", snap.LastPrune)
	}
}

// waitFor polls fn until it returns want, failing after a short deadline. Used to
// await the background goroutine reaching a state without sleeping a fixed time.
func waitFor(t *testing.T, fn func() bool, want bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fn() == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within deadline (want %v)", want)
}
