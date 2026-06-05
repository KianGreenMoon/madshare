package api

import (
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/config"
	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Deps bundles the dependencies the API route groups need. filesDir is the
// directory uploaded blobs are served from (the same path the store writes to);
// MaxUploadSize caps the upload request body in bytes.
type Deps struct {
	Store         storage.Storage
	Repo          database.Repository
	CacheDir      string
	FilesDir      string
	MaxUploadSize int64
	// Auth backs the /api/auth/* endpoints. When nil (e.g. NewRouter in tests),
	// those endpoints are not registered.
	Auth AuthStore
	// Manage backs the content-access management endpoints (/api/admin/access/*,
	// per-file guest/license, auto-derive). When nil, they are not registered.
	Manage ManageStore
	// ImagePool, when set, is notified after a cover-variant job is enqueued so
	// an idle worker wakes immediately rather than waiting for its next poll.
	// Optional; nil (e.g. in tests) simply skips the wake.
	ImagePool interface{ Notify() }
	// UIConfig is the parsed webui.toml served at GET /api/ui/config. When nil,
	// the handler falls back to config.DefaultUIConfig().
	UIConfig *config.UIConfig
}

// protect returns middleware enforcing perm, but only when auth is configured
// (d.Auth != nil). With no auth backend — e.g. NewRouter in tests or a
// deliberately open embedding — it is a pass-through, so the gating is active
// exactly when the Identify middleware is also present (see madshare.go).
func (d Deps) protect(perm string) func(http.Handler) http.Handler {
	if d.Auth == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return auth.RequirePermission(perm)
}

func (d Deps) newHandler() *handler {
	// KNOWN ISSUE (TODO): imagesDir nests inside filesDir, and the /files/*
	// file server serves the whole filesDir tree. As a side effect cover images
	// are also reachable at /files/images/<key>, not just at /images/*. Harmless
	// today (no auth, images are public, nosniff is set, listing is 404'd), but
	// the URL surface is wider than intended and would bypass any future
	// access control applied only to /images/*. Revisit how this is laid out —
	// e.g. store images outside the served files tree, or 404 /files/images.
	return &handler{
		storage:       d.Store,
		repo:          d.Repo,
		cacheDir:      d.CacheDir,
		imagesDir:     filepath.Join(d.FilesDir, "images"),
		maxUploadSize: d.MaxUploadSize,
		authzEnabled:  d.Auth != nil,
		imagePool:     d.ImagePool,
		uiConfig:      d.UIConfig,
	}
}

// RegisterAPI mounts the core API route group on r: the health check, the
// non-admin /api/* endpoints, /files/*, and /images/*. It registers no
// middleware — the caller owns that (see NewRouter and madshare.go's
// buildHandler). The web UI owns "/", so the health check lives at /healthz to
// avoid colliding with it on a full-stack listener.
func RegisterAPI(r chi.Router, d Deps) {
	h := d.newHandler()
	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})

	r.Get("/api/files", h.listFiles)
	r.Get("/api/artists", h.listArtists)
	r.Get("/api/albums", h.listAlbums)
	r.Get("/api/tracks", h.listTracks)
	r.Get("/api/search", h.search)
	r.Get("/api/ui/config", h.getUIConfig)
	r.Get("/api/artists/{artist}/image", h.getArtistImage)
	r.Get("/api/albums/{album}/image", h.getAlbumImage)
	r.Get("/api/albums/{album}/image/status", h.getAlbumImageStatus)
	// Editing cover images is a metadata.edit capability.
	r.With(d.protect(auth.PermMetadataEdit)).Post("/api/artists/{artist}/image", h.uploadArtistImage)
	r.With(d.protect(auth.PermMetadataEdit)).Post("/api/albums/{album}/image", h.uploadAlbumImage)

	// Uploading new files requires file.upload. The route is registered here
	// (rather than inside fileServer) so the gate wraps only the write path; the
	// GET file server is guarded separately by the content-access check.
	r.With(d.protect(auth.PermFileUpload)).Post("/files/upload", h.uploadFile)
	fileServer(r, "/files", noListFS{http.Dir(d.FilesDir)}, d.fileAccessGuard())

	imagesFS := noListFS{http.Dir(h.imagesDir)}
	r.Get("/images/*", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		rctx := chi.RouteContext(r.Context())
		pathPrefix := strings.TrimSuffix(rctx.RoutePattern(), "/*")
		http.StripPrefix(pathPrefix, http.FileServer(imagesFS)).ServeHTTP(w, r)
	})

	// Authentication endpoints (login/logout/me/password/tokens) live in the
	// api group so they are reachable wherever the API is served.
	if d.Auth != nil {
		registerAuth(r, d.Auth)
	}
}

// RegisterAdmin mounts the admin route group (/api/admin/*) on r. Gating is
// per-route (not a blanket subrouter middleware) so each endpoint can require
// the capability it actually needs: destructive file ops are file.delete, while
// content-access management is user.manage / metadata.edit (registered by
// registerManage). protect is a pass-through when auth is not configured.
func RegisterAdmin(r chi.Router, d Deps) {
	h := d.newHandler()
	r.Route("/api/admin", func(r chi.Router) {
		fileDelete := d.protect(auth.PermFileDelete)
		r.With(fileDelete).Delete("/files/{hash}", h.adminDeleteFile)
		r.With(fileDelete).Post("/prune", h.adminPrune)
		r.With(fileDelete).Get("/trash", h.adminTrashList)
		r.With(fileDelete).Delete("/trash/{hash}", h.adminTrashHardDelete)
		r.With(fileDelete).Post("/trash/{hash}/restore", h.adminTrashRestore)

		// Content-access management (Phase 3c). Only registered when a store is
		// configured; its routes carry their own permission gates.
		if d.Manage != nil {
			registerManage(r, d)
		}
	})
}

// NewRouter builds a full API handler (api + admin groups) with the standard
// middleware. It is a convenience for tests and pure-API embedding; the running
// server composes route groups per listener via the Register* functions.
func NewRouter(store storage.Storage, repo database.Repository, cacheDir, filesDir string, maxUploadSize int64) http.Handler {
	d := Deps{Store: store, Repo: repo, CacheDir: cacheDir, FilesDir: filesDir, MaxUploadSize: maxUploadSize}
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(CORS)
	RegisterAPI(r, d)
	RegisterAdmin(r, d)
	return r
}

// CORS sets permissive CORS headers on every response, including error
// responses written via http.Error (which previously carried none, so
// cross-origin JS clients could not read the error body). It also answers
// preflight OPTIONS requests directly. With the bundled, same-origin web UI
// these headers are inert; they matter for separately hosted or non-browser
// clients. (Revisit making this opt-in alongside the auth layer.)
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// noListFS wraps an http.FileSystem to disable directory listings: opening a
// directory with no index.html returns fs.ErrNotExist, so http.FileServer
// responds 404 instead of rendering an index of hash dirs and filenames.
type noListFS struct{ fsys http.FileSystem }

func (n noListFS) Open(name string) (http.File, error) {
	f, err := n.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if info.IsDir() {
		index := strings.TrimSuffix(name, "/") + "/index.html"
		if idx, err := n.fsys.Open(index); err != nil {
			f.Close()
			return nil, fs.ErrNotExist
		} else {
			idx.Close()
		}
	}
	return f, nil
}

func fileServer(r chi.Router, path string, root http.FileSystem, guard func(http.Handler) http.Handler) {
	if strings.ContainsAny(path, "{}*") {
		panic("fileServer does not permit any URL parameters.")
	}

	// The POST <path>/upload route is registered (and gated) by the caller.

	if path != "/" && path[len(path)-1] != '/' {
		r.Get(path, http.RedirectHandler(path+"/", 301).ServeHTTP)
		path += "/"
	}
	path += "*"

	r.With(guard).Get(path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		rctx := chi.RouteContext(r.Context())
		pathPrefix := strings.TrimSuffix(rctx.RoutePattern(), "/*")
		fs := http.StripPrefix(pathPrefix, http.FileServer(root))
		fs.ServeHTTP(w, r)
	})
}

// fileAccessGuard returns middleware enforcing per-file play/download access on
// the blob server. It is a pass-through when auth is not configured (NewRouter /
// tests / open embedding). Cover images (served under <files>/images) are not
// gated. A denied request gets 404 — not 403 — so it does not reveal that a
// file exists.
func (d Deps) fileAccessGuard() func(http.Handler) http.Handler {
	if d.Auth == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rest := chi.URLParam(r, "*")
			seg, _, _ := strings.Cut(rest, "/")
			if seg == "images" { // cover art is not gated
				next.ServeHTTP(w, r)
				return
			}
			if id := auth.FromContext(r.Context()); id.Has(auth.PermContentAll) {
				next.ServeHTTP(w, r)
				return
			}
			ok, err := d.Repo.FileAccessibleByHash(r.Context(), seg, actorID(r.Context()))
			if err != nil {
				http.Error(w, "storage error", http.StatusInternalServerError)
				return
			}
			if !ok {
				http.NotFound(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
