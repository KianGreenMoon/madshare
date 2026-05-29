package api

import (
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter builds and returns the API HTTP handler.
// The caller is responsible for calling http.ListenAndServe.
//
// filesDir is the directory uploaded blobs are served from (the same path the
// store writes to); maxUploadSize caps the upload request body in bytes.
func NewRouter(store storage.Storage, repo database.Repository, cacheDir, filesDir string, maxUploadSize int64) http.Handler {
	// KNOWN ISSUE (TODO): imagesDir nests inside filesDir, and the /files/*
	// file server serves the whole filesDir tree. As a side effect cover images
	// are also reachable at /files/images/<key>, not just at /images/*. Harmless
	// today (no auth, images are public, nosniff is set, listing is 404'd), but
	// the URL surface is wider than intended and would bypass any future
	// access control applied only to /images/*. Revisit how this is laid out —
	// e.g. store images outside the served files tree, or 404 /files/images.
	imagesDir := filepath.Join(filesDir, "images")
	h := &handler{
		storage:       store,
		repo:          repo,
		cacheDir:      cacheDir,
		imagesDir:     imagesDir,
		maxUploadSize: maxUploadSize,
	}

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(corsMiddleware)
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Hello World!"))
	})

	r.Get("/api/files", h.listFiles)
	r.Get("/api/artists", h.listArtists)
	r.Get("/api/albums", h.listAlbums)
	r.Get("/api/tracks", h.listTracks)
	r.Get("/api/artists/{artist}/image", h.getArtistImage)
	r.Post("/api/artists/{artist}/image", h.uploadArtistImage)
	r.Get("/api/albums/{album}/image", h.getAlbumImage)
	r.Post("/api/albums/{album}/image", h.uploadAlbumImage)

	// Admin endpoints are grouped so an auth gate can slot in ahead of them.
	r.Route("/api/admin", func(r chi.Router) {
		// SECURITY TODO: these endpoints delete files and prune DB records with
		// no authentication. Acceptable for v0 loopback-only use; gate this
		// group (e.g. r.Use(adminGate)) before any non-loopback deployment.
		// r.Use(adminGate)
		r.Delete("/files/{hash}", h.adminDeleteFile)
		r.Post("/prune", h.adminPrune)
	})

	fileServer(r, "/files", noListFS{http.Dir(filesDir)}, h)

	imagesFS := noListFS{http.Dir(imagesDir)}
	r.Get("/images/*", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		rctx := chi.RouteContext(r.Context())
		pathPrefix := strings.TrimSuffix(rctx.RoutePattern(), "/*")
		http.StripPrefix(pathPrefix, http.FileServer(imagesFS)).ServeHTTP(w, r)
	})

	return r
}

// corsMiddleware sets permissive CORS headers on every response, including
// error responses written via http.Error (which previously carried none, so
// cross-origin JS clients could not read the error body). It also answers
// preflight OPTIONS requests directly.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
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

func fileServer(r chi.Router, path string, root http.FileSystem, h *handler) {
	if strings.ContainsAny(path, "{}*") {
		panic("fileServer does not permit any URL parameters.")
	}

	r.Post(path+"/upload", h.uploadFile)

	if path != "/" && path[len(path)-1] != '/' {
		r.Get(path, http.RedirectHandler(path+"/", 301).ServeHTTP)
		path += "/"
	}
	path += "*"

	r.Get(path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		rctx := chi.RouteContext(r.Context())
		pathPrefix := strings.TrimSuffix(rctx.RoutePattern(), "/*")
		fs := http.StripPrefix(pathPrefix, http.FileServer(root))
		fs.ServeHTTP(w, r)
	})
}
