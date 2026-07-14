// Package prune runs the server's single, process-global Verify & Prune
// operation as a background job.
//
// Verify & Prune scans every files row for a missing or (deep) corrupted backing
// blob and can then delete the flagged rows. The scan — especially the deep,
// rehash-everything variant — is slow, so it runs detached from the HTTP request
// that starts it: the request returns immediately and the work outlives it. The
// Manager enforces that only one prune runs server-wide at a time (a second start
// is refused, not queued or run in parallel), exposes a shared status snapshot so
// any admin sees the in-flight run, supports cancellation, and persists a small
// summary of the last scan and last prune (counts + timestamp) so the outcome
// survives a restart. Design: docs/architecture/prune-job.md.
package prune

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"daemonlord.ygg/madshare/database"
)

// ErrBusy is returned by StartScan/StartPrune when a prune is already running.
var ErrBusy = errors.New("a prune is already running")

// ErrNoScan is returned by StartPrune when there is no in-memory scan result to
// prune — the operator must run a scan (preview) first so the delete acts only on
// a reviewed set. (After a restart the prior scan's detail is gone; the page
// re-scans before pruning.)
var ErrNoScan = errors.New("no scan to prune; run a scan first")

// Settings keys for the persisted last-run summaries (see database/settings.go —
// settings is a generic key/value table, so these need no migration).
const (
	settingLastScan  = "prune.last_scan"
	settingLastPrune = "prune.last_prune"
)

// State is the manager's top-level state.
type State string

const (
	StateIdle    State = "idle"
	StateRunning State = "running"
)

// Phase distinguishes the two kinds of in-flight run.
type Phase string

const (
	PhaseScanning Phase = "scanning" // a dry-run scan (preview)
	PhasePruning  Phase = "pruning"  // deleting the reviewed set
)

// Kind labels a finished run / held detail.
const (
	KindScan  = "scan"
	KindPrune = "prune"
)

// Outcome is how a finished run ended.
const (
	OutcomeCompleted = "completed"
	OutcomeCancelled = "cancelled"
	OutcomeFailed    = "failed"
)

// Probe is the local blob-store slice the prune needs (satisfied by
// api/storage.Local).
type Probe interface {
	BlobPresent(hash string) (bool, error)
	VerifyBlob(hash string) (bool, error)
	DeleteAll(hash string) (removed bool, err error)
}

// LinkProbe is the links-storage slice the prune needs to detect and reclaim
// broken symlink imports (data-sources P5; satisfied by *storages.Linker). It is
// never asked to touch an external target. May be nil when no links storage is
// wired (then links rows are skipped by the scan).
type LinkProbe interface {
	LinkInfo(hash string) (target string, exists, targetPresent bool, err error)
	VerifyLink(hash string) (bool, error)
	Remove(hash string) error
}

// SettingsStore persists the last-run summaries (satisfied by *database.DB).
type SettingsStore interface {
	GetSetting(ctx context.Context, key string) (value string, ok bool, err error)
	SetSetting(ctx context.Context, key, value string) error
}

// RunSummary is the at-a-glance record of a finished run, persisted per slot
// (last scan / last prune) so counts + date survive a restart.
type RunSummary struct {
	Kind       string    `json:"kind"` // KindScan | KindPrune
	Deep       bool      `json:"deep"`
	Scanned    int       `json:"scanned"`
	Dangling   int       `json:"dangling_count,omitempty"`     // scan
	Pruned     int       `json:"pruned_count,omitempty"`       // prune
	Failed     int       `json:"failed_count,omitempty"`       // prune
	Invalid    int       `json:"invalid_recordings,omitempty"` // prune (recordings GC'd)
	Outcome    string    `json:"outcome"`                      // OutcomeCompleted | ...
	Error      string    `json:"error,omitempty"`
	By         string    `json:"by,omitempty"`
	FinishedAt time.Time `json:"finished_at"`
}

// RunDetail is the full result of the most recent in-process run, kept in memory
// (not persisted) so the live page can render the dangling/pruned lists. It is
// absent after a restart — only the summaries above survive.
type RunDetail struct {
	Kind     string                  `json:"kind"`
	Deep     bool                    `json:"deep"`
	Scanned  int                     `json:"scanned"`
	Dangling []database.DanglingRef  `json:"dangling,omitempty"` // scan
	Pruned   []database.DanglingRef  `json:"pruned,omitempty"`   // prune
	Failed   []database.PruneFailure `json:"failed,omitempty"`   // prune
	// InvalidRecordings is how many rows the post-prune reap collected
	// (GC model, docs/architecture/gc-model.md).
	InvalidRecordings int    `json:"invalid_recordings,omitempty"` // prune
	Outcome           string `json:"outcome"`
}

// Progress is the live scan/prune counter.
type Progress struct {
	Scanned int `json:"scanned"`
	Total   int `json:"total"`
}

// Snapshot is the shared status any admin sees. When running it carries the live
// fields; when idle it carries the persisted summaries plus, if still held, the
// last in-process detail.
type Snapshot struct {
	State      State       `json:"state"`
	Phase      Phase       `json:"phase,omitempty"`
	Deep       bool        `json:"deep,omitempty"`
	StartedBy  string      `json:"started_by,omitempty"`
	StartedAt  *time.Time  `json:"started_at,omitempty"`
	Progress   *Progress   `json:"progress,omitempty"`
	LastScan   *RunSummary `json:"last_scan,omitempty"`
	LastPrune  *RunSummary `json:"last_prune,omitempty"`
	LastResult *RunDetail  `json:"last_result,omitempty"`
}

// Manager owns the single prune operation. The zero value is not usable; call New.
type Manager struct {
	repo      database.Repository
	probe     Probe
	linkProbe LinkProbe
	settings  SettingsStore

	mu        sync.Mutex
	running   bool
	phase     Phase
	deep      bool
	startedBy string
	startedAt time.Time
	cancel    context.CancelFunc

	// progress counters updated lock-free by the running job's onProgress.
	scanned atomic.Int64
	total   atomic.Int64

	// persisted summaries (loaded at New, refreshed on each finish).
	lastScan  *RunSummary
	lastPrune *RunSummary
	// held detail of the most recent in-process run (scan result awaiting prune,
	// or the last prune outcome). Cleared when a new run starts.
	lastResult *RunDetail

	wg sync.WaitGroup // tracks the in-flight run for Wait (shutdown).
}

// New builds the manager and loads any persisted last-run summaries so an admin
// (even right after a restart) sees the previous outcome and date. linkProbe may
// be nil when no links storage is configured (links rows are then skipped).
func New(repo database.Repository, probe Probe, linkProbe LinkProbe, settings SettingsStore) *Manager {
	m := &Manager{repo: repo, probe: probe, linkProbe: linkProbe, settings: settings}
	if settings != nil {
		m.lastScan = loadSummary(settings, settingLastScan)
		m.lastPrune = loadSummary(settings, settingLastPrune)
	}
	return m
}

func loadSummary(s SettingsStore, key string) *RunSummary {
	v, ok, err := s.GetSetting(context.Background(), key)
	if err != nil || !ok {
		return nil
	}
	var sum RunSummary
	if json.Unmarshal([]byte(v), &sum) != nil {
		return nil
	}
	return &sum
}

// Snapshot returns the current shared status.
func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

// StartScan begins a dry-run scan (the full damage sweep). It returns the
// just-started snapshot, or ErrBusy if a prune is already running.
func (m *Manager) StartScan(deep bool, by string) (Snapshot, error) {
	return m.start(PhaseScanning, deep, by, nil)
}

// StartPrune begins deleting the set the last scan found and the operator
// reviewed. It returns ErrNoScan when no in-memory scan result is held, or ErrBusy
// when a prune is already running. The deep flag and target set come from the held
// scan, so the delete acts on exactly what was reviewed.
func (m *Manager) StartPrune(by string) (Snapshot, error) {
	m.mu.Lock()
	if m.running {
		snap := m.snapshotLocked()
		m.mu.Unlock()
		return snap, ErrBusy
	}
	held := m.lastResult
	if held == nil || held.Kind != KindScan || len(held.Dangling) == 0 {
		snap := m.snapshotLocked()
		m.mu.Unlock()
		return snap, ErrNoScan
	}
	refs := held.Dangling
	deep := held.Deep
	m.mu.Unlock()
	return m.start(PhasePruning, deep, by, refs)
}

// start is the shared launch path. refs is nil for a scan, the reviewed set for a
// prune. It transitions idle→running atomically (ErrBusy if already running) and
// spawns the detached job goroutine.
func (m *Manager) start(phase Phase, deep bool, by string, refs []database.DanglingRef) (Snapshot, error) {
	m.mu.Lock()
	if m.running {
		snap := m.snapshotLocked()
		m.mu.Unlock()
		return snap, ErrBusy
	}
	// Detached from any request/shutdown context: the run outlives its initiator
	// and is stopped only via Cancel.
	ctx, cancel := context.WithCancel(context.Background())
	m.running = true
	m.phase = phase
	m.deep = deep
	m.startedBy = by
	m.startedAt = time.Now()
	m.cancel = cancel
	m.scanned.Store(0)
	m.total.Store(0)
	// A fresh run supersedes any held detail (a stale scan result, or the previous
	// prune outcome) so the page never shows the old detail under a new run.
	m.lastResult = nil
	snap := m.snapshotLocked()
	m.mu.Unlock()

	m.wg.Add(1)
	go m.run(ctx, phase, deep, by, refs)
	return snap, nil
}

func (m *Manager) run(ctx context.Context, phase Phase, deep bool, by string, refs []database.DanglingRef) {
	defer m.wg.Done()

	onProgress := func(done, total int) {
		m.scanned.Store(int64(done))
		m.total.Store(int64(total))
	}

	var (
		res     *database.PruneResult
		err     error
		kind    string
		setting string
	)
	if phase == PhaseScanning {
		kind, setting = KindScan, settingLastScan
		res, err = database.ScanDangling(ctx, m.repo, m.probe, m.linkProbe, deep, onProgress)
	} else {
		kind, setting = KindPrune, settingLastPrune
		res, err = database.PruneRefs(ctx, m.repo, m.probe, m.linkProbe, deep, refs, onProgress)
		if res != nil {
			res.Scanned = len(refs)
		}
	}

	outcome := OutcomeCompleted
	errText := ""
	switch {
	case errors.Is(err, context.Canceled):
		outcome = OutcomeCancelled
	case err != nil:
		outcome = OutcomeFailed
		errText = err.Error()
		log.Printf("prune %s: %v", kind, err)
	}
	if res == nil {
		res = &database.PruneResult{Deep: deep}
	}

	detail := &RunDetail{Kind: kind, Deep: deep, Scanned: res.Scanned, Outcome: outcome}
	summary := RunSummary{
		Kind: kind, Deep: deep, Scanned: res.Scanned,
		Outcome: outcome, Error: errText, By: by, FinishedAt: time.Now(),
	}
	if kind == KindScan {
		detail.Dangling = res.Dangling
		summary.Dangling = len(res.Dangling)
	} else {
		detail.Pruned = res.Pruned
		detail.Failed = res.Failed
		detail.InvalidRecordings = res.InvalidRecordings
		summary.Pruned = len(res.Pruned)
		summary.Failed = len(res.Failed)
		summary.Invalid = res.InvalidRecordings
	}

	m.mu.Lock()
	m.running = false
	m.phase = ""
	m.cancel = nil
	m.lastResult = detail
	if kind == KindScan {
		m.lastScan = &summary
	} else {
		m.lastPrune = &summary
	}
	m.mu.Unlock()

	m.persist(setting, summary)
}

// persist best-effort writes a summary slot. A storage error is logged, never
// fatal — losing a summary line must not break the prune itself.
func (m *Manager) persist(key string, summary RunSummary) {
	if m.settings == nil {
		return
	}
	data, err := json.Marshal(summary)
	if err != nil {
		log.Printf("prune persist %s: marshal: %v", key, err)
		return
	}
	if err := m.settings.SetSetting(context.Background(), key, string(data)); err != nil {
		log.Printf("prune persist %s: %v", key, err)
	}
}

// Cancel stops the running operation, if any. It reports whether a run was
// cancelled (false when already idle).
func (m *Manager) Cancel() bool {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// Wait blocks until the in-flight run (if any) finishes. Used by graceful
// shutdown to let a running prune complete rather than killing it mid-pass, and
// by tests to await a job synchronously.
func (m *Manager) Wait() { m.wg.Wait() }

// Running reports whether a prune is currently in flight.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// snapshotLocked builds a Snapshot; the caller must hold m.mu.
func (m *Manager) snapshotLocked() Snapshot {
	snap := Snapshot{State: StateIdle, LastScan: m.lastScan, LastPrune: m.lastPrune, LastResult: m.lastResult}
	if m.running {
		started := m.startedAt
		snap.State = StateRunning
		snap.Phase = m.phase
		snap.Deep = m.deep
		snap.StartedBy = m.startedBy
		snap.StartedAt = &started
		snap.Progress = &Progress{Scanned: int(m.scanned.Load()), Total: int(m.total.Load())}
	}
	return snap
}
