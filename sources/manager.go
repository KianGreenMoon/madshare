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
	UpdateDataSourceStatus(ctx context.Context, id, status string) error
	FinishDataSourceScan(ctx context.Context, id, status, summaryJSON string, scannedAt int64) error
	DeleteDataSource(ctx context.Context, id string) error
	ResetStaleScans(ctx context.Context) (int64, error)

	// Per-source attribution (migration 023) — drives Refresh/Remove.
	AttributeSourceFile(ctx context.Context, sourceID string, fileID int64) error
	SourceHasAttribution(ctx context.Context, sourceID string) (bool, error)
	CountSourceFiles(ctx context.Context, sourceID string) (int, error)
	SourceExclusiveFiles(ctx context.Context, sourceID string) ([]database.SourceFileRef, error)
	ListLinkFiles(ctx context.Context) ([]database.LinkFileRef, error)

	GetFileByHash(ctx context.Context, hash string) (*database.File, error)
	InsertFile(ctx context.Context, f *database.File, upload *database.FileUpload, meta *database.MediaMetadata) error
	RecordUpload(ctx context.Context, fileID int64, filename string) error
	HardDeleteFileByHash(ctx context.Context, hash string) ([]string, bool, error)
	EnqueueAnalysisJob(ctx context.Context, fileID, now int64) error

	// Cover attach (read-once-derive, P4). Mirrors the upload path's
	// maybeSaveEmbeddedCover: resolve the album, fill its cover if absent.
	ResolveAlbumID(ctx context.Context, artist, album string) (int64, error)
	HasAlbumCover(ctx context.Context, albumID int64) (bool, error)
	SetAlbumCoverIfAbsent(ctx context.Context, albumID int64, imageHash, sourceExt, objectKey, mimeType string, now int64) (bool, error)
	EnqueueImageJob(ctx context.Context, coverType, subjectKey, imageHash string, now int64) error
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
	anyRoot  bool              // [sources].allow_any: no allow-list at all
	accepted map[string]string // accepted audio extension -> canonical MIME
	clock    func() int64      // injectable for tests; defaults to time.Now().Unix

	// Cover read-once-derive (P4), set by WithCovers. When sourceImagesDir is
	// empty, cover extraction is skipped entirely (the P3 behaviour).
	sourceImagesDir string   // owned cover source-original tree (<files_dir>/images)
	imagePool       Notifier // wakes the cover-variant worker; nil skips the wake

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

// WithCovers enables read-once-derive cover extraction during scans: each new
// linked file's embedded or sidecar art is decoded once into an owned source
// original under sourceImagesDir (<files_dir>/images) and its album cover is
// filled if absent, with a variant job enqueued (woken via imagePool). Call once
// before serving. With an empty sourceImagesDir covers are skipped (P3 behaviour).
// imagePool may be nil (the job still queues; no immediate wake). See
// docs/architecture/data-sources.md (Cover images) and variants.md.
func (m *Manager) WithCovers(sourceImagesDir string, imagePool Notifier) *Manager {
	m.sourceImagesDir = sourceImagesDir
	m.imagePool = imagePool
	return m
}

// WithAnyRoot drops the allow-list when allow is true: any absolute directory may
// then be imported in place. It takes the flag rather than being a no-arg toggle
// so a call site reads as wiring [sources].allow_any through, and so forgetting to
// call it leaves the strict, allow-list-only behaviour. Only a deployment with no
// reachable admin surface should pass true — the reasoning, and the startup
// warning when a listener is configured too, are in
// docs/architecture/embedding.md.
func (m *Manager) WithAnyRoot(allow bool) *Manager {
	m.anyRoot = allow
	return m
}

// Enabled reports whether in-place imports are possible: an allow-list entry
// exists, or the allow-list has been dropped outright (WithAnyRoot).
func (m *Manager) Enabled() bool { return len(m.roots) > 0 || m.anyRoot }

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
	if !m.reserveScanSlot() {
		return nil, ErrBusy
	}

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
		m.releaseScanSlot()
		return nil, err
	}

	m.launchScan(ds.ID, realRoot, actor)

	src := toSource(ds)
	return &src, nil
}

// Rescan re-walks an existing source's root and re-runs the scan, reusing the
// source's row (status → scanning, fresh summary). It is ADDITIVE: it links newly
// appeared files and re-affirms attribution, but never removes files that have
// disappeared from the root (a temporarily-unavailable drive must not trigger a
// mass delete; vanished originals surface on the Prune page and are removed via
// Remove). The root is re-validated against the allow-list, in case symlink_roots
// changed since the source was added. Errors: ErrDisabled, ErrSourceNotFound,
// ErrInvalidRoot, ErrRootNotAllowed, ErrBusy.
func (m *Manager) Rescan(ctx context.Context, id string, actor sql.NullInt64) (*Source, error) {
	if !m.Enabled() {
		return nil, ErrDisabled
	}
	ds, err := m.store.GetDataSource(ctx, id)
	if err != nil {
		return nil, err
	}
	realRoot, err := m.resolveAllowedRoot(ds.RootPath)
	if err != nil {
		return nil, err
	}
	if !m.reserveScanSlot() {
		return nil, ErrBusy
	}
	if err := m.store.UpdateDataSourceStatus(ctx, id, database.SourceStatusScanning); err != nil {
		m.releaseScanSlot()
		return nil, err
	}
	m.launchScan(id, realRoot, actor)
	ds.Status = database.SourceStatusScanning
	src := toSource(ds)
	return &src, nil
}

// Removal is the outcome (or, from RemovalPreview, the projection) of removing a
// source: how many records are deleted vs kept (shared with another source or
// owned by a local upload).
type Removal struct {
	WillRemove int `json:"will_remove"`
	WillKeep   int `json:"will_keep"`
}

// RemovalPreview reports, without changing anything, how many of a source's files
// would be deleted vs kept if it were removed — for a confirm dialog. Returns
// ErrSourceNotFound for an unknown id.
func (m *Manager) RemovalPreview(ctx context.Context, id string) (Removal, error) {
	if _, err := m.store.GetDataSource(ctx, id); err != nil {
		return Removal{}, err
	}
	excl, err := m.store.SourceExclusiveFiles(ctx, id)
	if err != nil {
		return Removal{}, err
	}
	total, err := m.store.CountSourceFiles(ctx, id)
	if err != nil {
		return Removal{}, err
	}
	return Removal{WillRemove: countLinks(excl), WillKeep: total - countLinks(excl)}, nil
}

// Remove deletes a source relation-awarely (see docs/architecture/data-sources.md,
// Removing a source). For each file the source references EXCLUSIVELY (no other
// source) it unlinks the hash's symlink — safe, only-this-source, never the
// external target — and, if the catalog row is a links import, hard-deletes it.
// A shared record (referenced by another source) or a local upload is kept. It
// reserves the single scan slot so attribution cannot shift underneath it.
// Errors: ErrSourceNotFound, ErrBusy.
func (m *Manager) Remove(ctx context.Context, id string) (Removal, error) {
	if _, err := m.store.GetDataSource(ctx, id); err != nil {
		return Removal{}, err
	}
	if !m.reserveScanSlot() {
		return Removal{}, ErrBusy
	}
	defer m.releaseScanSlot()

	excl, err := m.store.SourceExclusiveFiles(ctx, id)
	if err != nil {
		return Removal{}, err
	}
	total, err := m.store.CountSourceFiles(ctx, id)
	if err != nil {
		return Removal{}, err
	}

	removed := 0
	for _, f := range excl {
		// Only this source references the hash, so its single symlink is safe to
		// reclaim whether the row is a links import or a local upload's stray
		// re-link. os.Remove on the link never follows it to the external target.
		if err := m.linker.Remove(f.Hash); err != nil {
			logf("sources: remove %s: unlink %s: %v", id, f.Hash, err)
		}
		// A local blob is owned — keep the catalog row; only links rows are deleted.
		if f.StorageBackend != database.StorageBackendLinks {
			continue
		}
		if _, _, err := m.store.HardDeleteFileByHash(ctx, f.Hash); err != nil {
			logf("sources: remove %s: delete file %s: %v", id, f.Hash, err)
			continue
		}
		removed++
	}
	if err := m.store.DeleteDataSource(ctx, id); err != nil {
		return Removal{}, err
	}
	logf("sources: removed source %s: deleted=%d kept=%d", id, removed, total-removed)
	return Removal{WillRemove: removed, WillKeep: total - removed}, nil
}

// BackfillAttribution heals sources created before migration 023 (no source_files
// rows): it attributes each such source's linked files by matching files.link_target
// under the source's symlink-resolved root_path, so Remove works without a forced
// rescan first. Idempotent — a source with any attribution is skipped, and the
// inserts are INSERT OR IGNORE. Call once at startup, after ResetStaleScans. A
// cross-root duplicate that earlier scans SKIPPED was never recorded; a Refresh of
// that source re-attributes it via the skip path.
func (m *Manager) BackfillAttribution(ctx context.Context) error {
	srcs, err := m.store.ListDataSources(ctx)
	if err != nil {
		return err
	}
	var need []*database.DataSource
	for _, s := range srcs {
		has, err := m.store.SourceHasAttribution(ctx, s.ID)
		if err != nil {
			return err
		}
		if !has {
			need = append(need, s)
		}
	}
	if len(need) == 0 {
		return nil
	}
	links, err := m.store.ListLinkFiles(ctx)
	if err != nil {
		return err
	}
	attributed := 0
	for _, s := range need {
		root := resolveRealPath(s.RootPath)
		for _, lf := range links {
			if !isUnder(lf.LinkTarget, root) {
				continue
			}
			if err := m.store.AttributeSourceFile(ctx, s.ID, lf.ID); err != nil {
				logf("sources: backfill attribute %s/%d: %v", s.ID, lf.ID, err)
				continue
			}
			attributed++
		}
	}
	if attributed > 0 {
		logf("sources: backfilled attribution for %d link reference(s) across %d source(s)", attributed, len(need))
	}
	return nil
}

// countLinks returns how many of the exclusive files are links rows (the set that
// Remove actually deletes; local rows are kept).
func countLinks(refs []database.SourceFileRef) int {
	n := 0
	for _, r := range refs {
		if r.StorageBackend == database.StorageBackendLinks {
			n++
		}
	}
	return n
}

// reserveScanSlot atomically claims the single scan/removal slot, returning false
// if one is already in flight. releaseScanSlot frees it.
func (m *Manager) reserveScanSlot() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		return false
	}
	m.running = true
	return true
}

func (m *Manager) releaseScanSlot() {
	m.mu.Lock()
	m.running = false
	m.mu.Unlock()
}

// launchScan starts the background scan goroutine, tracked by wg for Wait.
func (m *Manager) launchScan(id, realRoot string, actor sql.NullInt64) {
	m.wg.Add(1)
	go m.scan(id, realRoot, actor)
}

// resolveRealPath resolves symlinks in p (a scan walks the symlink-resolved root,
// so link_target values live under it), falling back to a plain Clean on error.
func resolveRealPath(p string) string {
	if r, err := filepath.EvalSymlinks(filepath.Clean(p)); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// isUnder reports whether target is root or nested under it, with a separator
// boundary so a sibling prefix (/srv/music vs /srv/music-evil) does not match.
func isUnder(target, root string) bool {
	target = filepath.Clean(target)
	return target == root || strings.HasPrefix(target, root+string(filepath.Separator))
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
	// The resolve-then-check order above is what stops a symlink inside the
	// requested path from escaping the allow-list; with no allow-list there is
	// nothing to escape, so the check is skipped rather than passed a wildcard
	// root (which would only work on a single-rooted filesystem anyway).
	if !m.anyRoot && !withinAllowlist(real, m.roots) {
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
