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
	// UploadLimiter, when set, gates concurrent uploads (global + per-user caps
	// from [storage]). Optional; nil disables the gate.
	UploadLimiter *UploadLimiter
	// UIConfig is the parsed webui.toml served at GET /api/ui/config. When nil,
	// the handler falls back to config.DefaultUIConfig().
	UIConfig *config.UIConfig
	// SourceRoot is the project root directory used to build the AGPL source
	// archive served at GET /source. Empty string disables the endpoint.
	SourceRoot string
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

// protectAny gates on holding ANY of perms (pass-through when auth is not
// configured). Used by the cover-upload routes, which the handler then narrows
// for file.upload-only callers (add-only, never replace).
func (d Deps) protectAny(perms ...string) func(http.Handler) http.Handler {
	if d.Auth == nil {
		return func(next http.Handler) http.Handler { return next }
	}
	return auth.RequireAnyPermission(perms...)
}

func (d Deps) newHandler() *handler {
	// KNOWN ISSUE (TODO): imagesDir nests inside filesDir, and the /files/*
	// file server serves the whole filesDir tree. As a side effect cover images
	// are also reachable at /files/images/<key>, not just at /images/*. Harmless
	// today (no auth, images are public, nosniff is set, listing is 404'd), but
	// the URL surface is wider than intended and would bypass any future
	// access control applied only to /images/*. Revisit how this is laid out —
	// e.g. store images outside the served files tree, or 404 /files/images.
	h := &handler{
		storage:       d.Store,
		repo:          d.Repo,
		cacheDir:      d.CacheDir,
		imagesDir:     filepath.Join(d.FilesDir, "images"),
		maxUploadSize: d.MaxUploadSize,
		authzEnabled:  d.Auth != nil,
		imagePool:     d.ImagePool,
		limiter:       d.UploadLimiter,
		uiConfig:      d.UIConfig,
	}
	if d.SourceRoot != "" {
		h.source = &sourceArchiver{root: d.SourceRoot}
	}
	return h
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
	r.Get("/source", h.sourceArchive)
	r.Get("/license", h.licenseDoc)

	r.Get("/api/files", h.listFiles)
	r.Get("/api/artists", h.listArtists)
	r.Get("/api/albums", h.listAlbums)
	r.Get("/api/tracks", h.listTracks)
	r.Get("/api/search", h.search)
	r.Get("/api/ui/config", h.getUIConfig)
	r.Get("/api/artists/{artist}/image", h.getArtistImage)
	r.Get("/api/albums/{album}/image", h.getAlbumImage)
	r.Get("/api/albums/{album}/image/status", h.getAlbumImageStatus)
	// Editing cover images and base tags is a metadata.edit capability.
	// Cover uploads accept metadata.edit OR file.upload; the handlers add-only
	// for file.upload-only callers (an uploader fills a missing cover; replacing
	// stays a metadata.edit capability).
	r.With(d.protectAny(auth.PermMetadataEdit, auth.PermFileUpload)).Post("/api/artists/{artist}/image", h.uploadArtistImage)
	r.With(d.protectAny(auth.PermMetadataEdit, auth.PermFileUpload)).Post("/api/albums/{album}/image", h.uploadAlbumImage)
	r.With(d.protect(auth.PermMetadataEdit)).Patch("/api/files/{hash}/metadata", h.updateFileMetadata)
	// Renaming an artist/album entity edits the entity in place; tracks and
	// covers follow via their FKs. Addressed by current name like the cover routes.
	r.With(d.protect(auth.PermMetadataEdit)).Post("/api/artists/{artist}/rename", h.renameArtist)
	r.With(d.protect(auth.PermMetadataEdit)).Post("/api/albums/{album}/rename", h.renameAlbum)
	// Merging "from" (path) into "into" (body) repoints tracks/albums/covers onto
	// the target and deletes the source entity.
	r.With(d.protect(auth.PermMetadataEdit)).Post("/api/artists/{artist}/merge", h.mergeArtists)
	r.With(d.protect(auth.PermMetadataEdit)).Post("/api/albums/{album}/merge", h.mergeAlbums)
	// Id-addressed merge: both source and target by stable entity id (robust
	// against name collisions and the empty-name bucket). Preferred by the admin
	// UI. Distinct path depth from the name-addressed routes above, so no clash.
	r.With(d.protect(auth.PermMetadataEdit)).Post("/api/artists/merge", h.mergeArtistsByID)
	r.With(d.protect(auth.PermMetadataEdit)).Post("/api/albums/merge", h.mergeAlbumsByID)

	// Uploading new files requires file.upload. The route is registered here
	// (rather than inside fileServer) so the gate wraps only the write path; the
	// GET file server is guarded separately by the content-access check.
	r.With(d.protect(auth.PermFileUpload)).Post("/files/upload", h.uploadFile)
	// Advisory pre-upload existence check (status: absent/present/trashed). Same
	// gate as upload — a by-hash existence oracle must not be anonymous.
	r.With(d.protect(auth.PermFileUpload)).Post("/api/files/check", h.checkFile)
	// Uploader-facing restore — only succeeds when the trash-restore policy is
	// "uploader_restore" (the handler enforces it); gated on file.upload.
	r.With(d.protect(auth.PermFileUpload)).Post("/api/files/{hash}/restore", h.restoreFileForUploader)
	fileServer(r, "/files", noListFS{http.Dir(d.FilesDir)}, d.fileAccessGuard())

	imagesFS := noListFS{http.Dir(h.imagesDir)}
	r.Get("/images/*", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		rctx := chi.RouteContext(r.Context())
		pathPrefix := strings.TrimSuffix(rctx.RoutePattern(), "/*")
		http.StripPrefix(pathPrefix, http.FileServer(imagesFS)).ServeHTTP(w, r)
	})

	// Authentication endpoints (login/logout/me/password/tokens) live in the
	// api group so they are reachable wherever the API is served. Playlists are
	// per-user, so they too exist only when auth is configured — as is the
	// uploader-facing review/staging flow (it is owner-scoped, meaningless
	// without identities; without auth, uploads insert approved directly).
	if d.Auth != nil {
		registerAuth(r, d.Auth)
		registerPlaylists(r, d, h)
		fileUpload := d.protect(auth.PermFileUpload)
		r.With(fileUpload).Get("/api/my/uploads", h.myUploads)
		r.With(fileUpload).Patch("/api/my/uploads/{hash}/metadata", h.myUploadMetadata)
		r.With(fileUpload).Post("/api/my/uploads/submit", h.submitMyUploads)
		r.With(fileUpload).Delete("/api/my/uploads/{hash}", h.myUploadDiscard)
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

		// Moderation queue (review bucket). Discard is not a distinct endpoint —
		// it is the soft delete above (moderators hold file.delete).
		moderate := d.protect(auth.PermContentModerate)
		r.With(moderate).Get("/moderation", h.moderationList)
		r.With(moderate).Post("/moderation/{hash}/approve", h.moderationApprove)
		r.With(moderate).Post("/moderation/{hash}/return", h.moderationReturn)

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
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, PATCH, DELETE, OPTIONS")
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
//
// Staged (pending-review) blobs serve only to identities holding file.upload or
// content.moderate — uploaders, moderators, admins — and 404 for everyone else.
// Deliberately not owner-scoped (owner decision, 2026-06-11): any uploader can
// fetch any pending blob by its unguessable hash. Documented as potentially
// dangerous, may be tightened — see docs/architecture/auth.md.
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
			id := auth.FromContext(r.Context())
			state, _, _, found, err := d.Repo.FileReviewInfo(r.Context(), seg)
			if err != nil {
				http.Error(w, "storage error", http.StatusInternalServerError)
				return
			}
			if found && state != database.ReviewApproved {
				if id.Has(auth.PermFileUpload) || id.Has(auth.PermContentModerate) {
					next.ServeHTTP(w, r)
					return
				}
				http.NotFound(w, r)
				return
			}
			if id.Has(auth.PermContentAccess) {
				next.ServeHTTP(w, r)
				return
			}
			ok, err := d.Repo.FileAccessibleByHash(r.Context(), seg)
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
