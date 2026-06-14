package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"daemonlord.ygg/madshare/api"
	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/config"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/imageproc"
	"daemonlord.ygg/madshare/webui"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

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
	if err := database.ReconcileOrphans(context.Background(), db, filesDir); err != nil {
		log.Printf("reconcile orphans: %v", err)
	}

	// Populate artist/album entity FKs for any media_metadata rows that predate
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
	imagesDir := filepath.Join(filesDir, "images")
	pool := imageproc.NewPool(db, imagesDir, cfg.Storage.ImageProcessingWorkers)
	go pool.Start(ctx)

	limiter := api.NewUploadLimiter(
		cfg.Storage.ServerMaxParallelWorkers,
		cfg.Storage.UserMaxParallelWorkers,
	)

	sourceRoot, err := os.Getwd()
	if err != nil {
		log.Printf("warning: cannot determine working directory for source archive: %v", err)
	}

	deps := api.Deps{
		Store:         storage.NewLocal(filesDir),
		Repo:          db,
		CacheDir:      os.TempDir(),
		FilesDir:      filesDir,
		MaxUploadSize: cfg.Storage.MaxUploadBytes(),
		Auth:          db,
		Manage:        db,
		ImagePool:     pool,
		UploadLimiter: limiter,
		UIConfig:      uiCfg,
		SourceRoot:    sourceRoot,
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
	cancel() // stop the image-variant worker pool
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
	log.Println("End!")
}

// startListeners binds and serves one http.Server per [[listen]] entry. Each
// server runs in its own goroutine; a bind failure aborts startup. The returned
// servers are live and must be shut down by the caller.
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
