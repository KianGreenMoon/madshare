package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
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
	"daemonlord.ygg/madshare/webui"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// licenseText is the AGPL LICENSE, embedded unconditionally so GET /license
// works in every build with no working tree.
//
//go:embed LICENSE.md
var licenseText []byte

// embeddedSourceTGZ is the AGPL source archive served at GET /source. It is nil
// here and overridden only in release builds (-tags embedsource, see source.go),
// where it holds the embedded source.tar.gz; dev builds leave it nil and fall
// back to building the archive from git ls-files in the CWD.
var embeddedSourceTGZ []byte

func main() {
	configPath := flag.String("config", "madshare.toml", "path to config file")
	webuiConfigPath := flag.String("webui-config", "webui.toml", "path to web UI config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config %s: %v", *configPath, err)
	}
	uiCfg, err := config.LoadWebUI(*webuiConfigPath)
	if err != nil {
		log.Fatalf("load webui config %s: %v", *webuiConfigPath, err)
	}
	for _, w := range cfg.Warnings() {
		log.Printf("config warning: %s", w)
	}
	// Feature gate: a listener may only serve the web UI if it is compiled in.
	for i, l := range cfg.Listen {
		if l.Serves(config.GroupWebUI) && !webui.Available {
			log.Fatalf("listen[%d] serves %q but this binary was built with -tags nowebui; rebuild without that tag or drop %q", i, config.GroupWebUI, config.GroupWebUI)
		}
	}
	// Feature gate: federation may only be enabled if it is compiled in.
	if cfg.Federation.Enabled && !federation.Available {
		log.Fatalf("federation.enabled is set but this binary was built with -tags nofederation; rebuild without that tag or disable [federation]")
	}
	// Environment gate, same class as the one above: federation is enabled but
	// this host cannot do it correctly.
	if cfg.Federation.Enabled {
		requireFingerprinting(cfg)
	}

	log.Println("Start the program")

	if err := os.MkdirAll(filepath.Dir(cfg.Database.Path), 0755); err != nil {
		log.Fatalf("mkdir %s: %v", filepath.Dir(cfg.Database.Path), err)
	}

	db, err := database.Open(cfg.Database.Path)
	if err != nil {
		log.Fatalf("open database %s: %v", cfg.Database.Path, err)
	}
	defer db.Close()

	filesDir := cfg.Storage.FilesDir
	variantsDir := cfg.Storage.VariantsDir
	// Audio blobs live under files_dir/audio; the /files server (rooted at the
	// audio dir) can never reach cover images, which live in the separate variants
	// tree. Relocate any pre-split blobs sitting directly under files_dir first.
	if n, err := storage.RelocateLegacyBlobs(filesDir); err != nil {
		log.Printf("relocate legacy blobs: %v", err)
	} else if n > 0 {
		log.Printf("relocated %d audio blob(s) under %s/%s", n, filesDir, storage.AudioSubdir)
	}
	// One-time upgrade: move the owned cover-image tree out of files_dir into the
	// dedicated variants dir (files_dir/images -> variants_dir/images). Idempotent;
	// a no-op on fresh installs and once migrated. See docs/architecture/variants.md.
	if n, err := storage.RelocateImageVariants(filesDir, variantsDir); err != nil {
		log.Printf("relocate image variants: %v", err)
	} else if n > 0 {
		log.Printf("relocated %d image entries into %s/%s", n, variantsDir, storage.ImagesSubdir)
	}
	audioDir := filepath.Join(filesDir, storage.AudioSubdir)
	if err := database.ReconcileOrphans(context.Background(), db, audioDir); err != nil {
		log.Printf("reconcile orphans: %v", err)
	}

	// Catch-all sweep for remote playlist rows whose blob arrived by any path
	// the write-time hooks miss (docs/ui/madnetwork-page.md §Re-pointing).
	if n, err := db.RepointRemotePlaylistItems(context.Background()); err != nil {
		log.Printf("repoint remote playlist items: %v", err)
	} else if n > 0 {
		log.Printf("repointed %d remote playlist item(s) to local appearances", n)
	}

	// Collect whatever a crash or bug left unreferenced before anything reads
	// through it (GC model, docs/architecture/gc-model.md): quarantine the
	// blobs of appearance-less recordings, trash the appearances of file-less
	// ones, drop empty husks. Idempotent; logs when it finds anything (the
	// write paths reap inline, so a non-zero count here is a bug signal).
	if _, err := db.Reap(context.Background()); err != nil {
		log.Printf("reap: %v", err)
	}

	// Populate artist/album entity FKs for any tagsets that predate
	// the overlay (idempotent; new uploads resolve entities inline at import).
	if n, err := db.BackfillEntities(context.Background()); err != nil {
		log.Printf("backfill entities: %v", err)
	} else if n > 0 {
		log.Printf("backfilled artist/album entities for %d tracks", n)
	}

	// Migrate any pre-entity, string-keyed cover rows (set aside by migration
	// 014) onto the entity-id-keyed cover tables. Must run after the entity
	// backfill above so the entities it resolves against exist.
	if err := db.BackfillCoverEntities(context.Background()); err != nil {
		log.Printf("backfill cover entities: %v", err)
	}

	// Fold the unknown-artist/-album buckets (whose dedup keys migration 016 left
	// empty) onto their canonical keys, merging any pre-existing literal
	// "Unknown artist"/"Other" entities. Must run after the cover backfill above.
	if err := db.FoldUnknownBuckets(context.Background()); err != nil {
		log.Printf("fold unknown buckets: %v", err)
	}

	// Cover source/derivative split (data half of migration 022): re-key any legacy
	// 16-char-keyed album covers to the full image hash, moving the source original
	// out to <files_dir>/images (a regenerate seed, never served) and regenerating
	// its variants under <variants_dir>/images. Idempotent; a no-op once every cover
	// is full-hash-keyed. Must run after the cover backfills above (rows final) and
	// before the orphan sweep / worker pool below, which expect full-hash variant
	// directories. See docs/architecture/variants.md.
	filesImagesDir := filepath.Join(filesDir, storage.ImagesSubdir)
	imagesDir := filepath.Join(variantsDir, storage.ImagesSubdir)
	if n, err := db.SplitImageSources(context.Background(), filesImagesDir, imagesDir); err != nil {
		log.Printf("split image sources: %v", err)
	} else if n > 0 {
		log.Printf("split %d cover(s) into source seed + variants", n)
	}

	// First-run admin bootstrap: create the admin only when no users exist.
	created, err := auth.Bootstrap(context.Background(), db, cfg.Auth.InitialAdminUser, cfg.Auth.InitialAdminPassword)
	if err != nil {
		log.Fatalf("bootstrap admin: %v", err)
	}
	if created {
		log.Printf("created initial admin user %q (must change password on first login)", cfg.Auth.InitialAdminUser)
	} else if cfg.Auth.InitialAdminPassword != "" {
		log.Printf("warning: users already exist; the initial admin password is unused — unset %s / [auth].initial_admin_password", config.InitialAdminPasswordEnv)
	}

	// Long-lived context for background workers (the image-variant pool). It is
	// cancelled in the shutdown path so the pool's goroutines unblock and exit.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Recover any jobs left 'running' by a previous crash, then launch the
	// cover-variant worker pool. Done before listeners accept traffic.
	if err := db.ResetStaleJobs(ctx); err != nil {
		log.Printf("reset stale image jobs: %v", err)
	}
	// Sweep orphaned album-cover variant dirs (covers replaced with different
	// bytes, distinct-art race losers) with no referencing row and no active job.
	// Runs after the entity/cover backfills above (so album_images rows are final)
	// and before the pool starts writing variants — startup-only, no upload races.
	if n, err := db.ReconcileImageOrphans(ctx, imagesDir, filesImagesDir); err != nil {
		log.Printf("reconcile image orphans: %v", err)
	} else if n > 0 {
		log.Printf("reconciled %d orphan image dir(s)", n)
	}
	pool := imageproc.NewPool(db, filesImagesDir, imagesDir, cfg.Storage.ImageProcessingWorkers)
	go pool.Start(ctx)

	// Recover album covers stuck at variants_ready=0 with no job — the row whose
	// claim succeeded but whose enqueue then failed (a rare DB error). The
	// complement to ResetStaleJobs above: that recovers running jobs, this
	// recovers claimed-but-never-queued covers. Idempotent; skips terminal
	// 'failed' jobs. The original blob is already on disk, so a fresh job suffices.
	if n, err := db.RequeueStuckImageJobs(ctx); err != nil {
		log.Printf("requeue stuck image jobs: %v", err)
	} else if n > 0 {
		log.Printf("re-enqueued %d stuck cover job(s)", n)
		pool.Notify()
	}

	// Ingest media analysis (ffprobe tech columns + fpcalc fingerprint). Both
	// tools are optional: a missing binary warns (never fatal) and that tool is
	// skipped for every job. See docs/architecture/recordings.md.
	haveFFprobe, haveFpcalc := media.ToolStatus()
	if !haveFFprobe {
		log.Printf("warning: ffprobe not found on PATH; audio tech columns (bitrate/sample rate/codec) stay empty")
		if cfg.Federation.Enabled {
			// Not fatal (unlike fpcalc above): the output is poorer, the input
			// is still verified. Worth naming, because the cost lands on peers.
			log.Printf("         with federation enabled that also means the catalog this node " +
				"publishes carries no quality facts, so friends cannot rank its renditions")
		}
	}
	if !haveFpcalc {
		log.Printf("warning: fpcalc not found on PATH; acoustic fingerprinting disabled (duplicate detection degrades to tags)")
	}
	if err := db.ResetStaleAnalysisJobs(ctx); err != nil {
		log.Printf("reset stale analysis jobs: %v", err)
	}
	// The read-side resolver: serves a content hash by probing local (files_dir)
	// then the shared links storage (<data_dir>/links). Built here (ahead of its
	// other uses below) so the media-analysis pool resolves linked imports the
	// same way /files does — otherwise it only sees the local audio dir and logs
	// "no blob for hash" for every external file. The links dir need not exist
	// yet; it stays empty until a symlink source populates it (data-sources P3).
	storageRegistry := storages.New(filesDir, cfg.LinksDir())
	mediaPool := mediaproc.NewPool(db, storageRegistry, cfg.Storage.ImageProcessingWorkers, haveFFprobe, haveFpcalc)
	go mediaPool.Start(ctx)
	// Backfill analysis for blobs uploaded before this ran (idempotent; skips
	// files that already have a fingerprint and tech columns). Only worth doing
	// when at least one tool can produce something.
	if haveFFprobe || haveFpcalc {
		if ids, err := db.FilesNeedingAnalysis(ctx); err != nil {
			log.Printf("media analysis backfill: %v", err)
		} else if len(ids) > 0 {
			now := time.Now().Unix()
			for _, id := range ids {
				if err := db.EnqueueAnalysisJob(ctx, id, now); err != nil {
					log.Printf("media analysis backfill enqueue file=%d: %v", id, err)
				}
			}
			log.Printf("enqueued %d file(s) for media analysis backfill", len(ids))
			mediaPool.Notify()
		}
	}

	limiter := api.NewUploadLimiter(
		cfg.Storage.ServerMaxParallelWorkers,
		cfg.Storage.UserMaxParallelWorkers,
	)

	// The background reaper (GC model): write paths nudge it after committing a
	// delete/purge/move and it coalesces bursts into one collection pass. The
	// startup reap above and the prune backstop keep correctness independent of
	// nudges; call sites arrive with the unlink-only write paths (phase P2).
	reaper := database.NewReaper(ctx, db)

	// The Linker is the write side of the links storage (symlink create/remove)
	// and the prune's broken-link probe. Built before the prune manager so prune
	// can detect/reclaim broken symlink imports (data-sources P5).
	linker := storages.NewLinker(cfg.LinksDir())

	// The single, process-global Verify & Prune job. Detached from request and
	// shutdown contexts; graceful shutdown waits for an in-flight run below. It
	// shares the audio blob store with the API and the links storage with the
	// symlink sources; the prune is storage-aware (missing local blob vs broken
	// link), never touching an external target.
	audioStore := storage.NewLocal(audioDir)
	pruneMgr := prune.New(db, audioStore, linker, db)

	// Symlink data sources (import in place). The Manager runs one scan at a time
	// and is gated by the [sources].symlink_roots allow-list. Reset any source
	// left 'scanning' by a crash before serving. See docs/architecture/data-sources.md.
	sourcesMgr := sources.New(db, linker, mediaPool, cfg.Sources.SymlinkRoots, api.AcceptedAudioTypes()).
		// Read-once-derive covers (P4): decode each linked file's sidecar/embedded
		// art once into owned variants under <files_dir>/images, sharing the running
		// cover-variant pool (filesImagesDir + pool, same as uploads). See variants.md.
		WithCovers(filesImagesDir, pool)
	if err := sourcesMgr.ResetStaleScans(ctx); err != nil {
		log.Printf("reset stale source scans: %v", err)
	}
	// Heal pre-023 sources that have no per-file attribution yet, so Remove works
	// without a forced rescan first (idempotent — see data-sources.md, P7).
	if err := sourcesMgr.BackfillAttribution(ctx); err != nil {
		log.Printf("backfill source attribution: %v", err)
	}

	sourceRoot, err := os.Getwd()
	if err != nil {
		log.Printf("warning: cannot determine working directory for source archive: %v", err)
	}

	deps := api.Deps{
		Store:          audioStore,
		Repo:           db,
		SpoolDir:       os.TempDir(),
		FilesDir:       filesDir,
		VariantsDir:    variantsDir,
		Storages:       storageRegistry,
		MaxUploadSize:  cfg.Storage.MaxUploadBytes(),
		Auth:           db,
		Manage:         db,
		ImagePool:      pool,
		MediaPool:      mediaPool,
		PruneManager:   pruneMgr,
		SourcesManager: sourcesMgr,
		Linker:         linker,
		UploadLimiter:  limiter,
		UIConfig:       uiCfg,
		AcoustID:       tagsource.NewAcoustID(),
		MusicBrainz:    tagsource.NewMusicBrainz(),
		SourceArchive:  embeddedSourceTGZ,
		LicenseText:    licenseText,
		SourceRoot:     sourceRoot,
		Madnetwork:     db,
	}

	// The embedded madnetwork node (federation F0/F1): mesh identity, protocol
	// listener, and the friendship layer (trusted-peer table in the DB, pairing
	// handshake, refresh loop). Independent of the HTTP listeners below; started
	// before them so a broken key file aborts startup rather than surfacing
	// mid-flight. The node is also wired into Deps so /api/admin/federation and
	// the /admin/network page can manage it.
	var fedNode *federation.Node
	if cfg.Federation.Enabled {
		// F3 wiring: the blob resolver serves published hashes to friends (and
		// short-circuits fetches of hashes the library already holds); the
		// cache dir receives fetched blobs (cache-through streaming).
		resolve := func(hash string) (string, bool) {
			path, _, ok := storageRegistry.Resolve(hash)
			return path, ok
		}
		fedNode, err = federation.Start(cfg.Federation, db, log.Default(),
			federation.WithCacheDir(cfg.MadnetworkCacheDir()),
			federation.WithBlobResolver(resolve),
			federation.WithDiscovery(federation.Discovery{
				Budget: cfg.Federation.DiscoveryBudget,
				Cap:    cfg.Federation.DiscoveryCap,
			}))
		if err != nil {
			log.Fatalf("start federation node: %v", err)
		}
		deps.Federation = fedNode
		// A non-empty name switches the merged browse to include the own
		// published set (self holder label); guarantee one even for a
		// pathological hostname.
		deps.MadnetworkName = fedNode.Name()
		if deps.MadnetworkName == "" {
			deps.MadnetworkName = "this server"
		}
		deps.ReachableWindowSec = cfg.Federation.ReachableWindowSec
		log.Printf("federation: madnetwork node up — mesh address %s (key file %s)", fedNode.Address(), cfg.Federation.KeyFile)
	}

	servers, err := startListeners(cfg, deps)
	if err != nil {
		log.Fatalf("start listeners: %v", err)
	}

	// Block until a termination signal, then shut every server down gracefully.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("Shutting down...")
	cancel() // stop the background worker pools (image variants, media analysis, reaper)
	reaper.Stop()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	var wg sync.WaitGroup
	for _, srv := range servers {
		wg.Go(func() {
			if err := srv.Shutdown(shutdownCtx); err != nil {
				log.Printf("shutdown %s: %v", srv.Addr, err)
			}
		})
	}
	wg.Wait()
	if fedNode != nil {
		fedNode.Stop()
	}
	// Let a running Verify & Prune finish rather than killing it mid-pass (a hard
	// kill is still safe — prune is idempotent and re-runnable). A long deep prune
	// can hold shutdown here; an admin who needs an immediate exit can Cancel it.
	if pruneMgr.Running() {
		log.Println("Waiting for in-progress prune to finish...")
	}
	pruneMgr.Wait()
	log.Println("End!")
}

// startListeners binds and serves one http.Server per [[listen]] entry. Each
// server runs in its own goroutine; a bind failure aborts startup. The returned
// servers are live and must be shut down by the caller.
// requireFingerprinting refuses to bring up a federated node on a host without
// fpcalc, unless [federation].allow_missing_fingerprinting says otherwise. It is
// the one analysis tool federation cannot do without — the reasoning, including
// why ffprobe is deliberately not gated the same way, is on
// config.FederationConfig.AllowMissingFingerprinting.
//
// A PATH lookup is the whole check, on purpose: a binary that is present but
// broken shows up per analysis job, where the failure is recoverable and
// visible, and probing it here would only add a startup dependency on running
// somebody else's code.
func requireFingerprinting(cfg config.Config) {
	if _, haveFpcalc := media.ToolStatus(); haveFpcalc {
		return
	}
	if cfg.Federation.AllowMissingFingerprinting {
		log.Printf("warning: federation is enabled without fpcalc (allow_missing_fingerprinting is set) — " +
			"downloaded audio cannot be checked against what it claims to be, and never groups with " +
			"recordings this node already holds")
		return
	}
	log.Fatalf("federation.enabled is set but fpcalc (Chromaprint) is not on PATH.\n"+
		"  A federated node re-fingerprints downloaded audio locally before it joins a recording,\n"+
		"  because a peer's claims about it are hints and never facts. Without fpcalc that check\n"+
		"  cannot happen, so this node would import and re-publish content it is unable to verify.\n"+
		"  Install it:\n%s\n"+
		"  and restart — the startup backfill re-analyses every file that still lacks a\n"+
		"  fingerprint, so an existing library repairs itself.\n"+
		"  To federate without it anyway, set [federation] allow_missing_fingerprinting = true.",
		fpcalcInstallHint())
}

// fpcalcInstallHint names the package carrying fpcalc on this platform, so the
// refusal above ends in a command rather than in a search. Indented to sit
// inside that message.
func fpcalcInstallHint() string {
	switch runtime.GOOS {
	case "linux":
		return "    Debian/Ubuntu: apt install libchromaprint-tools\n" +
			"    Fedora/RHEL:   dnf install chromaprint-tools\n" +
			"    Alpine:        apk add chromaprint"
	case "freebsd":
		return "    pkg install chromaprint"
	case "darwin":
		return "    brew install chromaprint"
	default:
		return "    it ships with Chromaprint, https://acoustid.org/chromaprint"
	}
}

func startListeners(cfg config.Config, deps api.Deps) ([]*http.Server, error) {
	servers := make([]*http.Server, 0, len(cfg.Listen))
	for _, lc := range cfg.Listen {
		handler, err := buildHandler(lc, deps, cfg.WebUI, cfg.CORS.AllowedOrigins)
		if err != nil {
			return nil, err
		}
		ln, err := net.Listen("tcp", lc.BindAddr())
		if err != nil {
			return nil, err
		}
		srv := &http.Server{Addr: lc.BindAddr(), Handler: handler}
		servers = append(servers, srv)
		log.Printf("listening on %s serving %v", lc.BindAddr(), lc.Serve)
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Fatalf("serve %s: %v", srv.Addr, err)
			}
		}()
	}
	return servers, nil
}

// buildHandler composes the chi router for one listener: shared middleware plus
// only the route groups named in the listener's serve list. web carries the
// page-render options ([webui]: api_base + the resolved GitRepo button URL);
// corsOrigins is [cors].allowed_origins (empty → no cross-origin headers).
func buildHandler(lc config.ListenConfig, deps api.Deps, web config.WebUIConfig, corsOrigins []string) (http.Handler, error) {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(api.CORS(corsOrigins))
	// Resolve the request's identity (session cookie / bearer token) for every
	// route; authorization is enforced per group below.
	if deps.Auth != nil {
		r.Use(auth.Identify(deps.Auth))
	}
	if len(lc.AllowFrom) > 0 {
		mw, err := allowFrom(lc.AllowFrom)
		if err != nil {
			return nil, err
		}
		r.Use(mw)
	}
	// Let GET routes answer HEAD. Registered last (innermost) so the rewrite to
	// GET happens just before routing: the logger above still records HEAD, and
	// Identify / allow_from have already run, so the HEAD takes the same auth and
	// access path as a GET. See api.SupportHEAD.
	r.Use(api.SupportHEAD)

	if lc.Serves(config.GroupAPI) {
		api.RegisterAPI(r, deps)
	}
	if lc.Serves(config.GroupAdmin) {
		// RegisterAdmin gates the destructive API by file.delete when auth is
		// configured (deps.Auth set). The admin page itself is left ungated so
		// it can render its login prompt.
		api.RegisterAdmin(r, deps)
		webui.RegisterAdminPage(r, web.APIBase, web.GitRepoURL()) // no-op in -tags nowebui builds
	}
	if lc.Serves(config.GroupWebUI) {
		webui.Register(r, web.APIBase, web.GitRepoURL())
	}
	return r, nil
}

// allowFrom returns middleware that rejects (403) any request whose source IP
// is not within one of the given CIDRs. The CIDRs are already validated by
// config.Load; they are re-parsed here so the middleware owns its own state.
func allowFrom(cidrs []string) (func(http.Handler) http.Handler, error) {
	nets := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, err
		}
		nets = append(nets, n)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			ip := net.ParseIP(host)
			for _, n := range nets {
				if ip != nil && n.Contains(ip) {
					next.ServeHTTP(w, r)
					return
				}
			}
			http.Error(w, "forbidden", http.StatusForbidden)
		})
	}, nil
}
