// Package app assembles and runs a madshare node.
//
// It owns everything the server's main() used to do between reading the config
// and waiting for a signal: the blob-layout migrations, the reconciliation and
// backfill passes, the first-admin bootstrap, the worker pools, the prune and
// data-source managers, the api.Deps wiring, the yggdrasil transport and the
// madnetwork node. madshare.go is now a shim over Start/Serve/Stop, so a program
// embedding madshare runs the SAME startup the server does rather than a second
// copy of it that drifts on every change.
//
// Start brings the node up; Serve makes it reachable over HTTP. Both are ordinary
// parts of the surface — an embedder that serves gets the whole middleware stack,
// identity resolution included. What the split buys is that reachability is an
// explicit choice rather than a side effect of embedding.
//
// Design, and the decisions behind the surface: docs/architecture/embedding.md.
package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"daemonlord.ygg/madshare/api"
	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/config"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
	"daemonlord.ygg/madshare/imageproc"
	"daemonlord.ygg/madshare/media"
	"daemonlord.ygg/madshare/mediaproc"
	"daemonlord.ygg/madshare/prune"
	"daemonlord.ygg/madshare/sources"
	"daemonlord.ygg/madshare/storages"
	"daemonlord.ygg/madshare/tagsource"
)

// shutdownGrace bounds the graceful HTTP shutdown when the context passed to
// Stop carries no deadline of its own.
const shutdownGrace = 10 * time.Second

// Instance is a running madshare node. Everything it owns is shut down by Stop,
// which is safe to call more than once and safe to call on the partially-built
// instance a failed Start tears down internally.
type Instance struct {
	cfg  config.Config
	log  *log.Logger
	db   *database.DB
	deps api.Deps

	// ctx is the long-lived worker context, derived from the one passed to Start
	// and cancelled by Stop so the pools' goroutines unblock and exit.
	ctx    context.Context
	cancel context.CancelFunc

	// tools performs the ingest analysis: ffprobe and fpcalc on a server, an
	// embedder's own implementation where those cannot run (WithMediaTools).
	tools media.Tools

	registry  *storages.Registry
	linker    *storages.Linker
	imagePool *imageproc.Pool
	mediaPool *mediaproc.Pool
	reaper    *database.Reaper
	prune     *prune.Manager
	sources   *sources.Manager
	mesh      *federation.Mesh
	node      *federation.Node
	traffic   *api.TrafficFlusher

	// token is the capability token this node presents on outbound mesh
	// requests, empty on everything except a listener node (Network.SetToken,
	// docs/architecture/federation-access.md §"The household").
	//
	// It lives here rather than being passed to federation.Start because a
	// device acquires one long after startup — it has to sign in first — and
	// renews it every half hour thereafter. An atomic pointer rather than a
	// mutex so the read on every outbound request costs nothing measurable on a
	// server, which will never store anything into it.
	token atomic.Pointer[string]

	servers  []*http.Server
	serveErr chan error
	stopOnce sync.Once
}

type options struct {
	logger        *log.Logger
	ui            *config.UIConfig
	license       []byte
	sourceArchive []byte
	sourceRoot    string
	tools         media.Tools
}

// Option configures Start. Each one carries something that belongs to the
// program rather than to the node — how it was built, how it was launched, where
// its own files are.
type Option func(*options)

// WithLogger routes startup and background logging somewhere other than the
// standard logger. A GUI embedder has no terminal, so this is how madshare's
// chatter reaches its own log view.
func WithLogger(lg *log.Logger) Option {
	return func(o *options) {
		if lg != nil {
			o.logger = lg
		}
	}
}

// WithUIConfig supplies the web UI's upload controls (webui.toml, served at
// GET /api/ui/config). Defaults to config.DefaultUIConfig().
func WithUIConfig(ui *config.UIConfig) Option {
	return func(o *options) {
		if ui != nil {
			o.ui = ui
		}
	}
}

// WithLicenseText supplies the AGPL licence served at GET /license. It is the
// server binary's own //go:embed, so it arrives from the program.
func WithLicenseText(b []byte) Option {
	return func(o *options) { o.license = b }
}

// WithSourceArchive supplies the pre-built AGPL Corresponding Source tarball
// served at GET /source (release builds, -tags embedsource). Without it, and
// without WithSourceRoot, /source is not registered.
func WithSourceArchive(b []byte) Option {
	return func(o *options) { o.sourceArchive = b }
}

// WithSourceRoot names the working tree GET /source builds its archive from when
// no pre-built one was supplied. Empty (the default) disables the endpoint.
func WithSourceRoot(dir string) Option {
	return func(o *options) { o.sourceRoot = dir }
}

// WithMediaTools supplies the ingest analysis — tech columns and acoustic
// fingerprint — in place of running ffprobe and fpcalc as child processes.
//
// It exists for an embedder that cannot run either: on a phone there is no PATH
// to install onto and the system refuses to execute anything the app wrote, so
// the default finds nothing and the node silently loses its tech columns, its
// fingerprints, and with them the mesh. Such an embedder implements media.Tools
// over decoders compiled into itself.
//
// A server never calls this. Nil is ignored, so a caller passing an unbuilt
// dependency gets the default rather than a node that analyses nothing.
func WithMediaTools(t media.Tools) Option {
	return func(o *options) {
		if t != nil {
			o.tools = t
		}
	}
}

// Start brings a madshare node up from cfg and returns it running. It performs
// every startup pass in the order the server has always performed them — the
// order is load-bearing, and each step says what it depends on.
//
// ctx bounds the startup passes and is the parent of the long-lived worker
// context, so cancelling it aborts a slow start and later stops the pools.
//
// On any error nothing is left running: the partially-built instance is torn
// down before the error is returned, so a caller never has to Stop an instance
// it did not receive.
func Start(ctx context.Context, cfg config.Config, opts ...Option) (*Instance, error) {
	o := options{logger: log.Default(), ui: config.DefaultUIConfig(), tools: media.ExecTools{}}
	for _, fn := range opts {
		fn(&o)
	}

	// Feature and environment gates first: a config the binary or the host
	// cannot honour must fail before anything is opened or created.
	if err := checkGates(cfg, o.logger, o.tools); err != nil {
		return nil, err
	}

	inst := &Instance{cfg: cfg, log: o.logger, tools: o.tools}
	inst.ctx, inst.cancel = context.WithCancel(ctx)

	if err := inst.start(o); err != nil {
		inst.Stop(context.Background())
		return nil, err
	}
	return inst, nil
}

// start is Start's body, split out so every failure path can unwind through Stop.
func (i *Instance) start(o options) error {
	cfg, lg := i.cfg, i.log
	lg.Println("Start the program")

	if err := os.MkdirAll(filepath.Dir(cfg.Database.Path), 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(cfg.Database.Path), err)
	}
	db, err := database.Open(cfg.Database.Path)
	if err != nil {
		return fmt.Errorf("open database %s: %w", cfg.Database.Path, err)
	}
	i.db = db

	filesDir := cfg.Storage.FilesDir
	variantsDir := cfg.Storage.VariantsDir
	audioDir := filepath.Join(filesDir, storage.AudioSubdir)
	filesImagesDir := filepath.Join(filesDir, storage.ImagesSubdir)
	imagesDir := filepath.Join(variantsDir, storage.ImagesSubdir)

	i.relocate(filesDir, variantsDir, audioDir)
	i.reconcile()
	i.backfill(filesImagesDir, imagesDir)

	// First-run admin bootstrap: create the admin only when no users exist.
	created, err := auth.Bootstrap(i.ctx, db, cfg.Auth.InitialAdminUser, cfg.Auth.InitialAdminPassword)
	if err != nil {
		return fmt.Errorf("bootstrap admin: %w", err)
	}
	if created {
		lg.Printf("created initial admin user %q (must change password on first login)", cfg.Auth.InitialAdminUser)
	} else if cfg.Auth.InitialAdminPassword != "" {
		lg.Printf("warning: users already exist; the initial admin password is unused — unset %s / [auth].initial_admin_password", config.InitialAdminPasswordEnv)
	}

	i.startImagePool(filesImagesDir, imagesDir)
	i.startMediaPool(filesDir)

	// The background reaper (GC model): write paths nudge it after committing a
	// delete/purge/move and it coalesces bursts into one collection pass. The
	// startup reap above and the prune backstop keep correctness independent of
	// nudges.
	i.reaper = database.NewReaper(i.ctx, db)

	// The Linker is the write side of the links storage (symlink create/remove)
	// and the prune's broken-link probe. Built before the prune manager so prune
	// can detect/reclaim broken symlink imports (data-sources P5).
	i.linker = storages.NewLinker(cfg.LinksDir())

	// The single, process-global Verify & Prune job. Detached from request and
	// shutdown contexts; Stop waits for an in-flight run. It shares the audio
	// blob store with the API and the links storage with the symlink sources; the
	// prune is storage-aware (missing local blob vs broken link), never touching
	// an external target.
	audioStore := storage.NewLocal(audioDir)
	i.prune = prune.New(db, audioStore, i.linker, db)

	if err := i.startSources(filesImagesDir); err != nil {
		return err
	}

	i.deps = api.Deps{
		Store:          audioStore,
		Repo:           db,
		SpoolDir:       os.TempDir(),
		FilesDir:       filesDir,
		VariantsDir:    variantsDir,
		DatabasePath:   cfg.Database.Path,
		Storages:       i.registry,
		MaxUploadSize:  cfg.Storage.MaxUploadBytes(),
		Auth:           db,
		Manage:         db,
		ImagePool:      i.imagePool,
		MediaPool:      i.mediaPool,
		PruneManager:   i.prune,
		SourcesManager: i.sources,
		Linker:         i.linker,
		UploadLimiter: api.NewUploadLimiter(
			cfg.Storage.ServerMaxParallelWorkers,
			cfg.Storage.UserMaxParallelWorkers,
		),
		UIConfig:      o.ui,
		AcoustID:      tagsource.NewAcoustID(),
		MusicBrainz:   tagsource.NewMusicBrainz(),
		SourceArchive: o.sourceArchive,
		LicenseText:   o.license,
		SourceRoot:    o.sourceRoot,
		Madnetwork:    db,
		// Unconditional, like Madnetwork above: the cache outlives federation
		// being switched off, and the index has to keep describing it either way
		// (docs/architecture/madnetwork-cache.md).
		MadnetworkCacheDir: cfg.MadnetworkCacheDir(),
		CacheSweep:         i.sweepCache,
		CacheDefaultBytes:  cfg.CacheDefaultBytes(),
		// How a signed-in device joins this node's mesh (§"The household").
		// Built from config rather than from the running node: it describes what
		// this node was told to dial, which is the useful answer whether or not
		// the transport came up.
		Peering: &api.Peering{
			Share:  cfg.SharesPeers(),
			Peers:  cfg.Yggdrasil.SharedPeers,
			Listen: cfg.Yggdrasil.Listen,
		},
	}

	i.startCacheRetention()
	return i.startMesh()
}

// liveTransferHashes names the blobs a fetch is about to publish, which no
// retention rule may evict.
func (i *Instance) liveTransferHashes() map[string]bool {
	live := map[string]bool{}
	if i.node != nil {
		for _, t := range i.node.ActiveTransfers() {
			live[t.Hash] = true
		}
	}
	return live
}

// cacheRetentionInterval is how often the ceiling is re-applied. A sweep is two
// indexed queries and some unlinks, so the cadence is set by how long a node may
// sit over its ceiling rather than by cost.
const cacheRetentionInterval = time.Hour

// sweepCache enforces the download cache's retention ceiling. It is the api
// package's CacheSweeper: the handler that changes the number applies it in the
// same request, because somebody who has just lowered a ceiling is watching the
// disk.
func (i *Instance) sweepCache(ctx context.Context, maxBytes, maxAgeDays int64) (int, int64, error) {
	live := i.liveTransferHashes()
	dir := i.cfg.MadnetworkCacheDir()
	// Age first: it asks an absolute question, and the ceiling then measures what
	// survived. Running them the other way round would evict by coldness blobs
	// the age rule was about to remove anyway, and report the wrong reason for it.
	removed, freed, err := database.SweepCacheAge(ctx, i.db, dir, maxAgeDays, live)
	if err != nil {
		return removed, freed, err
	}
	byCeiling, freedByCeiling, err := database.SweepCacheCeiling(ctx, i.db, dir, maxBytes, live)
	return removed + byCeiling, freed + freedByCeiling, err
}

// effectiveCacheCeiling is the ceiling actually in force: the runtime override
// when there is one, and [federation].cache_max_mb otherwise. 0 means no limit.
func (i *Instance) effectiveCacheCeiling(ctx context.Context) (int64, error) {
	override, err := i.db.GetCacheCeiling(ctx)
	if err != nil {
		return 0, err
	}
	return database.ResolveCacheCeiling(override, i.cfg.CacheDefaultBytes()), nil
}

// cacheRetention reads both halves of the policy. They are read together
// because the sweep applies them together, and a partial read would silently
// disable whichever one failed.
func (i *Instance) cacheRetention(ctx context.Context) (ceiling, ageDays int64, err error) {
	if ceiling, err = i.effectiveCacheCeiling(ctx); err != nil {
		return 0, 0, err
	}
	ageDays, err = i.db.GetCacheMaxAgeDays(ctx)
	return ceiling, ageDays, err
}

// startCacheRetention runs the ceiling sweep on a timer.
//
// It starts UNCONDITIONALLY and reads the ceiling every pass, which is a
// deliberate departure from the original design note ("started only when a knob
// is non-zero"): the ceiling is a runtime setting, and a daemon that has to be
// started at boot would make switching it on need a restart — which is the one
// property the setting was made runtime to avoid. An off ceiling costs one
// settings read an hour.
func (i *Instance) startCacheRetention() {
	if i.cfg.MadnetworkCacheDir() == "" {
		return
	}
	go func() {
		t := time.NewTicker(cacheRetentionInterval)
		defer t.Stop()
		for {
			select {
			case <-i.ctx.Done():
				return
			case <-t.C:
				ceiling, ageDays, err := i.cacheRetention(i.ctx)
				if err != nil {
					i.log.Printf("cache retention: read policy: %v", err)
					continue
				}
				if ceiling <= 0 && ageDays <= 0 {
					continue
				}
				// Reported per rule rather than as one number: an operator reading
				// this line wants to know WHY bytes went, and the two answers lead
				// to different knobs.
				byAge, freedByAge, err := database.SweepCacheAge(i.ctx, i.db, i.cfg.MadnetworkCacheDir(), ageDays, i.liveTransferHashes())
				if err != nil {
					i.log.Printf("cache retention: age: %v", err)
				} else if byAge > 0 {
					i.log.Printf("cache retention: evicted %d blob(s) unused for %d day(s), freed %d bytes",
						byAge, ageDays, freedByAge)
				}
				removed, freed, err := database.SweepCacheCeiling(i.ctx, i.db, i.cfg.MadnetworkCacheDir(), ceiling, i.liveTransferHashes())
				if err != nil {
					i.log.Printf("cache retention: ceiling: %v", err)
				} else if removed > 0 {
					i.log.Printf("cache retention: evicted %d blob(s) over the %d-byte ceiling, freed %d bytes",
						removed, ceiling, freed)
				}
			}
		}
	}()
}

// relocate runs the one-time, idempotent blob-layout migrations, then the orphan
// reconciliation that reads through the resulting tree.
func (i *Instance) relocate(filesDir, variantsDir, audioDir string) {
	// Audio blobs live under files_dir/audio; the /files server (rooted at the
	// audio dir) can never reach cover images, which live in the separate
	// variants tree. Relocate any pre-split blobs sitting directly under
	// files_dir first.
	if n, err := storage.RelocateLegacyBlobs(filesDir); err != nil {
		i.log.Printf("relocate legacy blobs: %v", err)
	} else if n > 0 {
		i.log.Printf("relocated %d audio blob(s) under %s/%s", n, filesDir, storage.AudioSubdir)
	}
	// One-time upgrade: move the owned cover-image tree out of files_dir into the
	// dedicated variants dir (files_dir/images -> variants_dir/images). Idempotent;
	// a no-op on fresh installs and once migrated. See docs/architecture/variants.md.
	if n, err := storage.RelocateImageVariants(filesDir, variantsDir); err != nil {
		i.log.Printf("relocate image variants: %v", err)
	} else if n > 0 {
		i.log.Printf("relocated %d image entries into %s/%s", n, variantsDir, storage.ImagesSubdir)
	}
	if err := database.ReconcileOrphans(i.ctx, i.db, audioDir); err != nil {
		i.log.Printf("reconcile orphans: %v", err)
	}
}

// reconcile makes the download cache, the playlists and the GC invariants agree
// with what is on disk, before anything reads through them.
func (i *Instance) reconcile() {
	cacheDir := i.cfg.MadnetworkCacheDir()

	// Drop download-cache copies of blobs the library already holds. The write
	// path evicts as each one lands, so this is the catch-all — and the only
	// thing that fixes a node which already materialized tracks before the
	// eviction existed. Duplicated copies are served under two different rules
	// and only the library's applies the recording's sharing scope
	// (.issues/open-issues.md, "Cache seeding overrides a recording's sharing
	// scope"). Runs regardless of whether federation is enabled now: a node that
	// has switched it off still has the cache it filled while it was on.
	if n, err := database.EvictCachedMadnetworkBlobs(i.ctx, i.db, cacheDir); err != nil {
		i.log.Printf("evict cached madnetwork blobs: %v", err)
	} else if n > 0 {
		i.log.Printf("evicted %d cached blob(s) the library already holds", n)
	}

	// Delete the scratch files of fetches that died. A failed transfer cleans up
	// after itself; a killed process cannot, and nothing swept them afterwards —
	// both the eviction sweep and the holdings listing skip non-digest names on
	// purpose, so an abandoned `.part` was permanent dead disk.
	//
	// Unconditional here, and safe for one reason: a process that has just
	// started is writing nothing, so every partial it finds is abandoned by
	// definition. No age heuristic, no policy, no knob.
	if n, freed, err := database.ReapAbandonedPartials(cacheDir, nil); err != nil {
		i.log.Printf("reap abandoned partials: %v", err)
	} else if n > 0 {
		i.log.Printf("reaped %d abandoned partial fetch(es), %d bytes", n, freed)
	}

	// Make the cache index agree with the cache directory
	// (docs/architecture/madnetwork-cache.md). After the eviction above, which
	// deletes files: reconciling first would only re-drop those rows a moment
	// later. The files are authoritative — this adopts blobs the index has never
	// seen (a cache older than the index, or a process killed between the rename
	// and the insert) and drops rows whose file is gone.
	if added, dropped, err := database.ReconcileMadnetworkCache(i.ctx, i.db, cacheDir); err != nil {
		i.log.Printf("reconcile madnetwork cache: %v", err)
	} else if added > 0 || dropped > 0 {
		i.log.Printf("madnetwork cache index: adopted %d blob(s), dropped %d stale row(s)", added, dropped)
	}

	// Catch-all sweep for remote playlist rows whose blob arrived by any path
	// the write-time hooks miss (docs/ui/madnetwork-page.md §Re-pointing).
	if n, err := i.db.RepointRemotePlaylistItems(i.ctx); err != nil {
		i.log.Printf("repoint remote playlist items: %v", err)
	} else if n > 0 {
		i.log.Printf("repointed %d remote playlist item(s) to local appearances", n)
	}

	// Collect whatever a crash or bug left unreferenced before anything reads
	// through it (GC model, docs/architecture/gc-model.md): quarantine the
	// blobs of appearance-less recordings, trash the appearances of file-less
	// ones, drop empty husks. Idempotent; logs when it finds anything (the
	// write paths reap inline, so a non-zero count here is a bug signal).
	if _, err := i.db.Reap(i.ctx); err != nil {
		i.log.Printf("reap: %v", err)
	}
}

// backfill brings pre-overlay rows up to the current model. The four passes are
// strictly ordered: entities exist before covers are re-keyed onto them, the
// unknown buckets fold after that, and the source/derivative split runs last so
// every cover row it rewrites is already final.
func (i *Instance) backfill(filesImagesDir, imagesDir string) {
	// Populate artist/album entity FKs for any tagsets that predate the overlay
	// (idempotent; new uploads resolve entities inline at import).
	if n, err := i.db.BackfillEntities(i.ctx); err != nil {
		i.log.Printf("backfill entities: %v", err)
	} else if n > 0 {
		i.log.Printf("backfilled artist/album entities for %d tracks", n)
	}
	// Migrate any pre-entity, string-keyed cover rows (set aside by migration
	// 014) onto the entity-id-keyed cover tables. Must run after the entity
	// backfill above so the entities it resolves against exist.
	if err := i.db.BackfillCoverEntities(i.ctx); err != nil {
		i.log.Printf("backfill cover entities: %v", err)
	}
	// Fold the unknown-artist/-album buckets (whose dedup keys migration 016 left
	// empty) onto their canonical keys, merging any pre-existing literal
	// "Unknown artist"/"Other" entities. Must run after the cover backfill above.
	if err := i.db.FoldUnknownBuckets(i.ctx); err != nil {
		i.log.Printf("fold unknown buckets: %v", err)
	}
	// Cover source/derivative split (data half of migration 022): re-key any legacy
	// 16-char-keyed album covers to the full image hash, moving the source original
	// out to <files_dir>/images (a regenerate seed, never served) and regenerating
	// its variants under <variants_dir>/images. Idempotent; a no-op once every cover
	// is full-hash-keyed. Must run after the cover backfills above (rows final) and
	// before the orphan sweep / worker pool below, which expect full-hash variant
	// directories. See docs/architecture/variants.md.
	if n, err := i.db.SplitImageSources(i.ctx, filesImagesDir, imagesDir); err != nil {
		i.log.Printf("split image sources: %v", err)
	} else if n > 0 {
		i.log.Printf("split %d cover(s) into source seed + variants", n)
	}
}

// startImagePool recovers interrupted cover work, sweeps orphaned variant dirs,
// re-indexes the variants tree and launches the resize pool. Done before
// listeners accept traffic.
func (i *Instance) startImagePool(filesImagesDir, imagesDir string) {
	// Recover any jobs left 'running' by a previous crash.
	if err := i.db.ResetStaleJobs(i.ctx); err != nil {
		i.log.Printf("reset stale image jobs: %v", err)
	}
	// Sweep orphaned album-cover variant dirs (covers replaced with different
	// bytes, distinct-art race losers) with no referencing row and no active job.
	// Runs after the entity/cover backfills (so album_images rows are final) and
	// before the pool starts writing variants — startup-only, no upload races.
	if n, err := i.db.ReconcileImageOrphans(i.ctx, imagesDir, filesImagesDir); err != nil {
		i.log.Printf("reconcile image orphans: %v", err)
	} else if n > 0 {
		i.log.Printf("reconciled %d orphan image dir(s)", n)
	}
	// Re-walk the variants tree into the cover-variant byte index (migration 043).
	// The directory is authoritative and the index only describes it, so this is
	// what keeps the storage panel's "images" figure honest after a crash
	// mid-write or an edit by hand. It runs AFTER the orphan sweep above (so
	// removed dirs are already gone) and is the one place the expensive walk is
	// still paid — once per process, instead of once per dashboard load.
	if n, err := i.db.ReconcileImageVariants(i.ctx, imagesDir); err != nil {
		i.log.Printf("reconcile image variants: %v", err)
	} else if n > 0 {
		i.log.Printf("indexed %d cover variant dir(s)", n)
	}
	i.imagePool = imageproc.NewPool(i.db, filesImagesDir, imagesDir, i.cfg.Storage.ImageProcessingWorkers)
	go i.imagePool.Start(i.ctx)

	// Recover album covers stuck at variants_ready=0 with no job — the row whose
	// claim succeeded but whose enqueue then failed (a rare DB error). The
	// complement to ResetStaleJobs above: that recovers running jobs, this
	// recovers claimed-but-never-queued covers. Idempotent; skips terminal
	// 'failed' jobs. The original blob is already on disk, so a fresh job suffices.
	if n, err := i.db.RequeueStuckImageJobs(i.ctx); err != nil {
		i.log.Printf("requeue stuck image jobs: %v", err)
	} else if n > 0 {
		i.log.Printf("re-enqueued %d stuck cover job(s)", n)
		i.imagePool.Notify()
	}
}

// startMediaPool launches ingest media analysis (ffprobe tech columns + fpcalc
// fingerprint) and backfills whatever predates it. Both tools are optional: a
// missing binary warns (never fatal) and that tool is skipped for every job. See
// docs/architecture/recordings.md.
func (i *Instance) startMediaPool(filesDir string) {
	haveFFprobe, haveFpcalc := i.tools.Available()
	if !haveFFprobe {
		i.log.Printf("warning: ffprobe not found on PATH; audio tech columns (bitrate/sample rate/codec) stay empty")
		if i.cfg.Federation.Enabled {
			// Not fatal (unlike fpcalc): the output is poorer, the input is still
			// verified. Worth naming, because the cost lands on peers.
			i.log.Printf("         with federation enabled that also means the catalog this node " +
				"publishes carries no quality facts, so friends cannot rank its renditions")
		}
	}
	if !haveFpcalc {
		i.log.Printf("warning: fpcalc not found on PATH; acoustic fingerprinting disabled (duplicate detection degrades to tags)")
	}
	if err := i.db.ResetStaleAnalysisJobs(i.ctx); err != nil {
		i.log.Printf("reset stale analysis jobs: %v", err)
	}
	// The read-side resolver: serves a content hash by probing local (files_dir)
	// then the shared links storage (<data_dir>/links). Built here (ahead of its
	// other uses) so the media-analysis pool resolves linked imports the same way
	// /files does — otherwise it only sees the local audio dir and logs "no blob
	// for hash" for every external file. The links dir need not exist yet; it
	// stays empty until a symlink source populates it (data-sources P3).
	i.registry = storages.New(filesDir, i.cfg.LinksDir())
	i.mediaPool = mediaproc.NewPool(i.db, i.registry, i.cfg.Storage.ImageProcessingWorkers, i.tools)
	go i.mediaPool.Start(i.ctx)
	// Backfill analysis for blobs uploaded before this ran (idempotent; skips
	// files that already have a fingerprint and tech columns). Only worth doing
	// when at least one tool can produce something.
	if !haveFFprobe && !haveFpcalc {
		return
	}
	ids, err := i.db.FilesNeedingAnalysis(i.ctx)
	if err != nil {
		i.log.Printf("media analysis backfill: %v", err)
		return
	}
	if len(ids) == 0 {
		return
	}
	now := time.Now().Unix()
	for _, id := range ids {
		if err := i.db.EnqueueAnalysisJob(i.ctx, id, now); err != nil {
			i.log.Printf("media analysis backfill enqueue file=%d: %v", id, err)
		}
	}
	i.log.Printf("enqueued %d file(s) for media analysis backfill", len(ids))
	i.mediaPool.Notify()
}

// startSources builds the symlink data-source manager (import in place) and
// repairs whatever a crash or an older schema left behind. The Manager runs one
// scan at a time and is gated by the [sources] allow-list. See
// docs/architecture/data-sources.md.
func (i *Instance) startSources(filesImagesDir string) error {
	i.sources = sources.New(i.db, i.linker, i.mediaPool, i.cfg.Sources.SymlinkRoots, api.AcceptedAudioTypes()).
		// allow_any drops the allow-list for a deployment with no reachable
		// admin surface (docs/architecture/embedding.md); config warns when it
		// is combined with a listener.
		WithAnyRoot(i.cfg.Sources.AllowAny).
		// Read-once-derive covers (P4): decode each linked file's sidecar/embedded
		// art once into owned variants under <files_dir>/images, sharing the running
		// cover-variant pool (filesImagesDir + pool, same as uploads). See variants.md.
		WithCovers(filesImagesDir, i.imagePool)
	// Reset any source left 'scanning' by a crash before serving.
	if err := i.sources.ResetStaleScans(i.ctx); err != nil {
		i.log.Printf("reset stale source scans: %v", err)
	}
	// Heal pre-023 sources that have no per-file attribution yet, so Remove works
	// without a forced rescan first (idempotent — see data-sources.md, P7).
	if err := i.sources.BackfillAttribution(i.ctx); err != nil {
		i.log.Printf("backfill source attribution: %v", err)
	}
	return nil
}

// startMesh brings up the yggdrasil transport and, when madnetwork is enabled,
// the embedded federation node on top of it.
//
// Independent of the HTTP listeners, and started before them so a broken key file
// aborts startup rather than surfacing mid-flight. The transport comes up first
// and independently of madnetwork: [[listen_mesh]] serves the ordinary web UI and
// API on this node's own yggdrasil address whether or not federation is enabled
// (docs/plans/mesh-listener.md §4).
func (i *Instance) startMesh() error {
	cfg := i.cfg
	if cfg.MeshEnabled() {
		mesh, err := federation.StartTransport(cfg.Yggdrasil, i.log)
		if err != nil {
			return fmt.Errorf("start yggdrasil mesh: %w", err)
		}
		i.mesh = mesh
		i.log.Printf("yggdrasil: mesh up — address %s (key file %s)", mesh.Address(), cfg.Yggdrasil.KeyFile)
	}
	if !cfg.Federation.Enabled {
		return nil
	}

	// F3 wiring: the blob resolver serves published hashes to friends (and
	// short-circuits fetches of hashes the library already holds); the cache dir
	// receives fetched blobs (cache-through streaming).
	resolve := func(hash string) (string, bool) {
		path, _, ok := i.registry.Resolve(hash)
		return path, ok
	}
	// WithMesh hands the running transport over: from here Node.Stop owns it, so
	// Stop stops one or the other, never both.
	node, err := federation.Start(cfg.Federation, i.db, i.log,
		federation.WithMesh(i.mesh),
		federation.WithCacheDir(cfg.MadnetworkCacheDir()),
		// Empty until an embedder calls Network.SetToken, which a server never
		// does. Installed unconditionally all the same: the source is read at
		// request time, so there is nothing to decide here that the value does
		// not already answer.
		federation.WithCapabilityToken(func() string {
			if tok := i.token.Load(); tok != nil {
				return *tok
			}
			return ""
		}),
		federation.WithBlobResolver(resolve),
		// The node's rate caps and member budget are editable at runtime on
		// /admin/swarm; this is how it learns about a change without a restart.
		// Memoized inside the node, so this runs at most once every few seconds.
		federation.WithLimitResolver(func(ctx context.Context) (federation.LimitOverrides, error) {
			up, down, err := i.db.GetSwarmRates(ctx)
			if err != nil {
				return federation.LimitOverrides{}, err
			}
			member, err := i.db.GetMemberQuotas(ctx)
			return federation.LimitOverrides{Up: up, Down: down, Member: member}, err
		}),
		federation.WithDiscovery(federation.Discovery{
			Budget: cfg.Federation.DiscoveryBudget,
			Cap:    cfg.Federation.DiscoveryCap,
		}))
	if err != nil {
		return fmt.Errorf("start federation node: %w", err)
	}
	i.node = node
	i.deps.Federation = node
	// A non-empty name switches the merged browse to include the own published
	// set (self holder label); guarantee one even for a pathological hostname.
	i.deps.MadnetworkName = node.Name()
	if i.deps.MadnetworkName == "" {
		i.deps.MadnetworkName = "this server"
	}
	i.deps.ReachableWindowSec = cfg.Federation.ReachableWindowSec
	i.log.Printf("federation: madnetwork node up — mesh address %s (key file %s)", node.Address(), cfg.Yggdrasil.KeyFile)

	// Persist what the swarm moves (docs/architecture/swarm-admin.md). The node
	// counts in memory; this drains it into swarm_traffic on a timer, so no
	// database write ever lands on a chunk-fetch path.
	//
	// Guarded on the concrete pointer, not inside the constructor: a nil
	// *federation.Node handed to an interface parameter is a NON-nil interface,
	// so `node == nil` there would be false and the first drain would panic.
	i.traffic = api.NewTrafficFlusher(node, i.db)
	go i.traffic.Run(i.ctx)
	return nil
}

// Stop shuts the node down gracefully, in the order that loses least: the worker
// pools first, then the listeners, then the last interval of byte accounting
// while the node is still up, then the mesh, then any in-flight prune.
//
// If ctx carries no deadline, shutdownGrace bounds the HTTP shutdown. Safe to
// call repeatedly; later calls do nothing.
func (i *Instance) Stop(ctx context.Context) {
	i.stopOnce.Do(func() { i.stop(ctx) })
}

func (i *Instance) stop(ctx context.Context) {
	// Stop the background worker pools (image variants, media analysis, reaper).
	if i.cancel != nil {
		i.cancel()
	}
	if i.reaper != nil {
		i.reaper.Stop()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, shutdownGrace)
		defer cancel()
	}
	var wg sync.WaitGroup
	for _, srv := range i.servers {
		wg.Go(func() {
			if err := srv.Shutdown(ctx); err != nil {
				i.log.Printf("shutdown %s: %v", srv.Addr, err)
			}
		})
	}
	wg.Wait()
	i.servers = nil
	// Persist the last interval's byte accounting while the node is still up.
	// Done here rather than left to the flusher's own ctx.Done branch so it is
	// ordered rather than racing process exit — a graceful shutdown must not be a
	// small data loss.
	if i.traffic != nil {
		i.traffic.Flush(ctx)
	}
	// Exactly one of these owns the transport: federation.Start adopted the mesh
	// when madnetwork is on, so stopping the node stops it too (federation/mesh.go,
	// "Ownership"). A transport-only deployment stops the mesh itself.
	switch {
	case i.node != nil:
		i.node.Stop()
	case i.mesh != nil:
		i.mesh.Stop()
	}
	// Let a running Verify & Prune finish rather than killing it mid-pass (a hard
	// kill is still safe — prune is idempotent and re-runnable). A long deep prune
	// can hold shutdown here; an admin who needs an immediate exit can Cancel it.
	if i.prune != nil {
		if i.prune.Running() {
			i.log.Println("Waiting for in-progress prune to finish...")
		}
		i.prune.Wait()
	}
	if i.db != nil {
		if err := i.db.Close(); err != nil {
			i.log.Printf("close database: %v", err)
		}
	}
}

// Config returns the config the node was started from, resolved.
func (i *Instance) Config() config.Config { return i.cfg }

// Sources exposes the in-place import manager, so an embedder can offer "add a
// music folder" without going through the admin API.
func (i *Instance) Sources() *sources.Manager { return i.sources }

// UserID resolves a username to the row id an embedder has to act AS. Data
// sources, uploads and playlists are all attributed to a user, so provisioning an
// identity is only half of what an embedder needs — this is the other half.
// Reports false when no such user exists, which is not an error.
func (i *Instance) UserID(ctx context.Context, username string) (int64, bool, error) {
	u, err := i.db.GetUserByUsername(ctx, username)
	if err != nil {
		return 0, false, err
	}
	if u == nil {
		return 0, false, nil
	}
	return u.ID, true, nil
}

// GenerateSecret returns a random, URL-safe credential — what an embedder with no
// operator to prompt hands to [auth].initial_admin_password so madshare's
// empty-users refusal is satisfied by something unguessable.
//
// Whether to keep it is the embedder's decision. A deployment that never serves
// should discard it: nothing can authenticate with a secret that exists nowhere,
// and there is nothing to authenticate to. One that does serve wants a credential
// somebody chose instead — see docs/architecture/embedding.md §"Silent
// provisioning".
func GenerateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// ErrNoListeners is returned by Serve when the config binds nothing. It is a
// wiring mistake rather than a config-file one: a file-shaped config cannot get
// here (config.Load requires a listener), so reaching it means a program built a
// listener-less config and then asked to serve anyway.
var ErrNoListeners = errors.New("app: no listeners configured")
