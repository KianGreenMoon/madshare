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

// Deps bundles the dependencies the API route groups need. FilesDir is the base
// files directory: audio blobs are served from FilesDir/audio (so Store must be
// rooted there too — see storage.AudioSubdir) and cover images from
// FilesDir/images. MaxUploadSize caps the upload request body in bytes.
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
	// MediaPool, when set, is notified after an analysis job (ffprobe + fpcalc)
	// is enqueued so an idle worker wakes immediately. Optional; nil skips the
	// wake (tests / open embeddings).
	MediaPool interface{ Notify() }
	// UploadLimiter, when set, gates concurrent uploads (global + per-user caps
	// from [storage]). Optional; nil disables the gate.
	UploadLimiter *UploadLimiter
	// UIConfig is the parsed webui.toml served at GET /api/ui/config. When nil,
	// the handler falls back to config.DefaultUIConfig().
	UIConfig *config.UIConfig
	// SourceArchive, when non-nil, is the prebuilt AGPL source tar.gz embedded
	// into the binary at build time (make build, -tags embedsource). It is
	// served verbatim at GET /source, so the endpoint works with no working
	// tree. When nil, the archive is built from git ls-files in SourceRoot.
	SourceArchive []byte
	// LicenseText, when non-nil, is the embedded AGPL LICENSE served at
	// GET /license. When nil, the handler reads <SourceRoot>/LICENSE.md.
	LicenseText []byte
	// SourceRoot is the working directory used as the git ls-files / LICENSE.md
	// fallback for /source and /license when nothing is embedded (dev builds).
	// With no embedded data and an empty SourceRoot, both endpoints are disabled.
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
	// Layout: audio blobs live under <FilesDir>/audio (served at /files), cover
	// images under <FilesDir>/images (served at /images). The two are siblings,
	// so the /files server — rooted at the audio dir below — can never reach an
	// image; cover art is reachable only via /images. The store must be rooted
	// at <FilesDir>/audio to match (see madshare.go and NewRouter).
	h := &handler{
		storage:       d.Store,
		repo:          d.Repo,
		cacheDir:      d.CacheDir,
		imagesDir:     filepath.Join(d.FilesDir, "images"),
		maxUploadSize: d.MaxUploadSize,
		authzEnabled:  d.Auth != nil,
		imagePool:     d.ImagePool,
		mediaPool:     d.MediaPool,
		limiter:       d.UploadLimiter,
		uiConfig:      d.UIConfig,
	}
	if d.SourceArchive != nil || d.LicenseText != nil || d.SourceRoot != "" {
		h.source = &sourceArchiver{
			prebuilt:        d.SourceArchive,
			licensePrebuilt: d.LicenseText,
			root:            d.SourceRoot,
		}
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
	// Renditions of a track's recording for the player's quality control
	// (recordings P4). Read-only; playback is still gated by /files/*.
	r.Get("/api/tracks/{hash}/renditions", h.trackRenditions)
	r.Get("/api/search", h.search)
	r.Get("/api/ui/config", h.getUIConfig)
	// Cover reads are id-addressed (browse DTOs carry the entity id). chi routes
	// these by-id GETs alongside the by-name POSTs below on the same path node.
	r.Get("/api/artists/{artist_id}/image", h.getArtistImage)
	r.Get("/api/albums/{album_id}/image", h.getAlbumImage)
	r.Get("/api/albums/{album_id}/image/status", h.getAlbumImageStatus)
	// Editing cover images and base tags is a metadata.edit capability.
	// Cover uploads accept metadata.edit OR file.upload; the handlers add-only
	// for file.upload-only callers (an uploader fills a missing cover; replacing
	// stays a metadata.edit capability). The cover write path stays name-addressed
	// because it resolve-or-creates the entity at upload time, before it has a
	// browsable id (see docs/architecture/artist-album-model.md).
	r.With(d.protectAny(auth.PermMetadataEdit, auth.PermFileUpload)).Post("/api/artists/{artist}/image", h.uploadArtistImage)
	r.With(d.protectAny(auth.PermMetadataEdit, auth.PermFileUpload)).Post("/api/albums/{album}/image", h.uploadAlbumImage)
	r.With(d.protect(auth.PermMetadataEdit)).Get("/api/files/{hash}/metadata", h.getFileMetadata)
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
	// Read-only merge preview ("what would move / collapse / which cover wins"),
	// same {from_id, into_id} body. A distinct path (not a dry_run flag) so a
	// preview can never accidentally perform the merge.
	r.With(d.protect(auth.PermMetadataEdit)).Post("/api/artists/merge/preview", h.mergeArtistsPreview)
	r.With(d.protect(auth.PermMetadataEdit)).Post("/api/albums/merge/preview", h.mergeAlbumsPreview)

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
	// Rooted at the audio subtree, a sibling of <FilesDir>/images, so /files can
	// only ever serve audio blobs — never cover images.
	fileServer(r, "/files", noListFS{http.Dir(filepath.Join(d.FilesDir, storage.AudioSubdir))}, d.fileAccessGuard())

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
		r.With(fileUpload).Get("/api/my/uploads/{hash}/metadata", h.myUploadMetadataGet)
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
		r.With(fileDelete).Get("/storage", h.adminStorageStats)
		r.With(fileDelete).Get("/trash", h.adminTrashList)
		r.With(fileDelete).Delete("/trash/{hash}", h.adminTrashHardDelete)
		r.With(fileDelete).Post("/trash/{hash}/restore", h.adminTrashRestore)

		// Moderation queue (review bucket). Discard is not a distinct endpoint —
		// it is the soft delete above (moderators hold file.delete).
		moderate := d.protect(auth.PermContentModerate)
		r.With(moderate).Get("/moderation", h.moderationList)
		r.With(moderate).Post("/moderation/{hash}/approve", h.moderationApprove)
		r.With(moderate).Post("/moderation/{hash}/return", h.moderationReturn)

		// Duplicates / variants (recordings P2). List multi-rendition recordings
		// and split a rendition off; delete reuses the soft-delete above.
		r.With(moderate).Get("/duplicates", h.duplicatesList)
		r.With(moderate).Post("/duplicates/{file_id}/split", h.duplicatesSplit)

		// Content-access management (Phase 3c). Only registered when a store is
		// configured; its routes carry their own permission gates.
		if d.Manage != nil {
			registerManage(r, d)
		}
	})
}

// NewRouter builds a full API handler (api + admin groups) with the standard
// middleware. It is a convenience for tests and pure-API embedding; the running
// server composes route groups per listener via the Register* functions. store
// must be rooted at filesDir/audio (storage.AudioSubdir), since the /files
// server reads from there.
func NewRouter(store storage.Storage, repo database.Repository, cacheDir, filesDir string, maxUploadSize int64) http.Handler {
	d := Deps{Store: store, Repo: repo, CacheDir: cacheDir, FilesDir: filesDir, MaxUploadSize: maxUploadSize}
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(CORS(nil)) // closed by default; configure [cors].allowed_origins for cross-origin
	RegisterAPI(r, d)
	RegisterAdmin(r, d)
	return r
}

// CORS-preflight advertisements, attached only when an origin is granted.
const (
	corsMethods = "POST, GET, HEAD, PATCH, DELETE, OPTIONS"
	corsHeaders = "Content-Type, Authorization"
	corsMaxAge  = "600"
)

// CORS returns middleware implementing the configurable cross-origin policy
// from [cors].allowed_origins. It also answers preflight OPTIONS directly so a
// granted origin can read error bodies and use non-simple methods.
//
//   - empty allow-list (the default) → no CORS headers are emitted. The bundled
//     web UI is same-origin and needs none; cross-origin browsers are blocked.
//   - "*" present                    → any origin is allowed, sent as a literal
//     "*" (and, per the CORS spec, without credentials).
//   - specific origins               → only those exact origins are allowed; a
//     matching request's Origin is echoed back with Vary: Origin and
//     Access-Control-Allow-Credentials, so a separately hosted UI may send the
//     session cookie or a bearer token.
//
// Non-browser clients ignore CORS entirely, so an empty allow-list does not
// affect API tokens / curl.
func CORS(allowedOrigins []string) func(http.Handler) http.Handler {
	wildcard := false
	allowed := make(map[string]bool, len(allowedOrigins))
	for _, o := range allowedOrigins {
		if o == "*" {
			wildcard = true
			continue
		}
		allowed[o] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if origin := r.Header.Get("Origin"); origin != "" {
				switch {
				case wildcard:
					w.Header().Set("Access-Control-Allow-Origin", "*")
				case allowed[origin]:
					w.Header().Add("Vary", "Origin")
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Access-Control-Allow-Credentials", "true")
				}
				// When an origin was granted above, advertise the methods/headers
				// a preflight needs (harmless on a non-preflight response).
				if w.Header().Get("Access-Control-Allow-Origin") != "" {
					w.Header().Set("Access-Control-Allow-Methods", corsMethods)
					w.Header().Set("Access-Control-Allow-Headers", corsHeaders)
					w.Header().Set("Access-Control-Max-Age", corsMaxAge)
				}
			}
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// SupportHEAD lets the GET-only routes answer HEAD requests. chi's r.Get
// registers the GET method alone, so a HEAD otherwise gets 405 — breaking
// uptime monitors that probe /healthz with HEAD, download managers sizing a
// file before fetching it, and link-preview crawlers.
//
// It rewrites HEAD to GET *before* the router matches the method, so the request
// runs the identical handler chain — auth.Identify, the /files access guard,
// http.FileServer (which natively sizes the file), everything — just with the
// response body discarded. Per RFC 7231 a HEAD response carries the headers a
// GET would, with no body. Because the rewrite happens ahead of routing, access
// control is never bypassed: a HEAD to a denied blob 404s exactly as the GET does.
//
// The rewrite is done on a clone so outer middleware (e.g. the request logger)
// still sees the original HEAD method.
func SupportHEAD(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		r2 := r.Clone(r.Context())
		r2.Method = http.MethodGet
		next.ServeHTTP(headResponseWriter{w}, r2)
	})
}

// headResponseWriter forwards status and headers but swallows the body, so a
// HEAD handler (which runs the GET code path) returns headers only. It reports a
// successful write count so handlers don't see short-write errors.
type headResponseWriter struct{ http.ResponseWriter }

func (h headResponseWriter) Write(b []byte) (int, error) { return len(b), nil }

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
// tests / open embedding). A denied request gets 404 — not 403 — so it does not
// reveal that a file exists. (Cover images live in a sibling tree served at
// /images and never reach this guard.)
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
