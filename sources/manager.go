// Package sources implements in-place "symlink" data sources: an admin points at
// an external directory of audio files, and the scan engine references each file
// by a symlink in the shared 'links' storage — never copying, never modifying the
// originals. It is the write/ingest counterpart to storages.Linker (the link
// helper) and the read-side storages.Registry (the resolver).
//
// The Manager runs one scan at a time, process-wide (a second Add returns
// ErrBusy), mirroring prune.Manager. Per-source state lives in the data_sources
// table (status active/scanning/error + the last scan summary); link health and
// per-storage accounting are reported elsewhere (data-sources P5). Design:
// docs/architecture/data-sources.md.
package sources

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"daemonlord.ygg/madshare/database"
)

// ErrBusy is returned by Add when a scan is already running (one at a time).
var ErrBusy = errors.New("a source scan is already running")

// ErrDisabled is returned by Add when no [sources].symlink_roots are configured,
// so the symlink source kind is turned off.
var ErrDisabled = errors.New("symlink sources are disabled (no [sources].symlink_roots configured)")

// ErrRootNotAllowed is returned by Add when the requested root resolves outside
// every configured symlink_roots entry (the allow-list trust boundary).
var ErrRootNotAllowed = errors.New("root is not under an allowed symlink_roots entry")

// ErrInvalidRoot is returned by Add when the root is empty, relative, or cannot
// be resolved on disk (so it cannot be scanned).
var ErrInvalidRoot = errors.New("root must be an absolute path to an existing directory")

// Store is the persistence slice the manager needs; *database.DB satisfies it.
// The data_sources methods live on *database.DB only (not the api Repository),
// so they do not widen that interface or its test fakes.
type Store interface {
	InsertDataSource(ctx context.Context, ds *database.DataSource) error
	ListDataSources(ctx context.Context) ([]*database.DataSource, error)
	GetDataSource(ctx context.Context, id string) (*database.DataSource, error)
	FinishDataSourceScan(ctx context.Context, id, status, summaryJSON string, scannedAt int64) error
	ResetStaleScans(ctx context.Context) (int64, error)

	GetFileByHash(ctx context.Context, hash string) (*database.File, error)
	InsertFile(ctx context.Context, f *database.File, upload *database.FileUpload, meta *database.MediaMetadata) error
	RecordUpload(ctx context.Context, fileID int64, filename string) error
	EnqueueAnalysisJob(ctx context.Context, fileID, now int64) error
}

// Linker is the write side of the 'links' storage; *storages.Linker satisfies it.
type Linker interface {
	Has(hash string) (bool, error)
	Link(hash, filename, target string) (created bool, err error)
	Remove(hash string) error
}

// Notifier wakes the media-analysis worker pool; *mediaproc.Pool satisfies it.
type Notifier interface{ Notify() }

// ScanSummary is the per-source last-scan outcome persisted as summary_json.
type ScanSummary struct {
	Scanned int `json:"scanned"` // audio files considered (= linked+skipped+failed)
	Linked  int `json:"linked"`  // new links created
	Skipped int `json:"skipped"` // hash already present in the links storage
	Failed  int `json:"failed"`  // hash/link/insert error
}

// Source is the API-facing view of a data_sources row (with summary_json parsed).
type Source struct {
	ID        string       `json:"id"`
	Kind      string       `json:"kind"`
	Name      string       `json:"name"`
	Root      string       `json:"root"`
	Status    string       `json:"status"`
	Summary   *ScanSummary `json:"summary,omitempty"`
	CreatedAt int64        `json:"created_at"`
	ScannedAt *int64       `json:"scanned_at,omitempty"`
}

// Manager owns the single source-scan operation and the symlink_roots allow-list.
type Manager struct {
	store    Store
	linker   Linker
	notify   Notifier          // optional (media pool); nil skips the wake
	roots    []string          // cleaned absolute allow-list entries
	accepted map[string]string // accepted audio extension -> canonical MIME
	clock    func() int64      // injectable for tests; defaults to time.Now().Unix

	mu      sync.Mutex
	running bool
	wg      sync.WaitGroup // tracks the in-flight scan for Wait (shutdown/tests)
}

// New builds the manager. roots is the [sources].symlink_roots allow-list
// (already validated absolute + Clean-stable by config); accepted is the
// extension→MIME audio allow-list (api.AcceptedAudioTypes), passed in to avoid an
// import cycle. notify may be nil.
func New(store Store, linker Linker, notify Notifier, roots []string, accepted map[string]string) *Manager {
	cleaned := make([]string, 0, len(roots))
	for _, r := range roots {
		cleaned = append(cleaned, filepath.Clean(r))
	}
	acc := make(map[string]string, len(accepted))
	maps.Copy(acc, accepted)
	return &Manager{
		store:    store,
		linker:   linker,
		notify:   notify,
		roots:    cleaned,
		accepted: acc,
		clock:    unixNow,
	}
}

// Enabled reports whether any symlink root is configured.
func (m *Manager) Enabled() bool { return len(m.roots) > 0 }

// Roots returns a copy of the configured allow-list (for the UI's Add form hint).
func (m *Manager) Roots() []string { return append([]string(nil), m.roots...) }

// ResetStaleScans flips any source left 'scanning' by a crash to 'error'. Call
// once at startup before serving.
func (m *Manager) ResetStaleScans(ctx context.Context) error {
	n, err := m.store.ResetStaleScans(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		logf("sources: reset %d stale scanning source(s) to error", n)
	}
	return nil
}

// List returns every data source as an API view.
func (m *Manager) List(ctx context.Context) ([]Source, error) {
	rows, err := m.store.ListDataSources(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Source, 0, len(rows))
	for _, r := range rows {
		out = append(out, toSource(r))
	}
	return out, nil
}

// Add validates a new symlink source against the allow-list, records it as
// 'scanning', and launches the scan in the background, returning the inserted
// source immediately. Errors: ErrDisabled, ErrInvalidRoot, ErrRootNotAllowed,
// ErrBusy. actor is the admin creating the source (recorded as uploaded_by on the
// files it links).
func (m *Manager) Add(ctx context.Context, name, root string, actor sql.NullInt64) (*Source, error) {
	if !m.Enabled() {
		return nil, ErrDisabled
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("name must not be empty")
	}
	realRoot, err := m.resolveAllowedRoot(root)
	if err != nil {
		return nil, err
	}

	// Reserve the single scan slot before touching the DB so a refused Add never
	// leaves a half-created 'scanning' row behind.
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return nil, ErrBusy
	}
	m.running = true
	m.mu.Unlock()

	now := m.clock()
	ds := &database.DataSource{
		ID:        newSourceID(),
		Kind:      database.SourceKindSymlink,
		Name:      name,
		RootPath:  filepath.Clean(root),
		Status:    database.SourceStatusScanning,
		CreatedAt: now,
	}
	if err := m.store.InsertDataSource(ctx, ds); err != nil {
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
		return nil, err
	}

	m.wg.Add(1)
	go m.scan(ds.ID, realRoot, actor)

	src := toSource(ds)
	return &src, nil
}

// Wait blocks until the in-flight scan (if any) finishes. Used by graceful
// shutdown and tests.
func (m *Manager) Wait() { m.wg.Wait() }

// Running reports whether a scan is currently in flight.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// resolveAllowedRoot validates root is absolute, resolves its symlinks, and
// checks the resolved path is within a configured allow-list entry. Resolving the
// INPUT first is what stops a symlink inside the requested path from escaping the
// allow-list. Returns the resolved root to walk, or an error.
func (m *Manager) resolveAllowedRoot(root string) (string, error) {
	if root == "" || !filepath.IsAbs(root) {
		return "", ErrInvalidRoot
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidRoot, err)
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", ErrInvalidRoot
	}
	if !withinAllowlist(real, m.roots) {
		return "", ErrRootNotAllowed
	}
	return real, nil
}

// withinAllowlist reports whether the (already resolved) path real is the same as
// or nested under one of the allow-list roots. Each allow entry is resolved too
// when possible, so a symlinked allow directory still matches. The path-separator
// boundary check prevents a sibling-prefix escape (/srv/music vs /srv/music-evil).
func withinAllowlist(real string, roots []string) bool {
	for _, allow := range roots {
		base := allow
		if r, err := filepath.EvalSymlinks(allow); err == nil {
			base = r
		}
		if real == base || strings.HasPrefix(real, base+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// toSource maps a data_sources row to its API view, parsing summary_json.
func toSource(r *database.DataSource) Source {
	s := Source{
		ID:        r.ID,
		Kind:      r.Kind,
		Name:      r.Name,
		Root:      r.RootPath,
		Status:    r.Status,
		CreatedAt: r.CreatedAt,
	}
	if r.SummaryJSON.Valid && r.SummaryJSON.String != "" {
		var sum ScanSummary
		if json.Unmarshal([]byte(r.SummaryJSON.String), &sum) == nil {
			s.Summary = &sum
		}
	}
	if r.ScannedAt.Valid {
		v := r.ScannedAt.Int64
		s.ScannedAt = &v
	}
	return s
}

// newSourceID returns a short random opaque id for a data_sources row.
func newSourceID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
