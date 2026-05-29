package api

import (
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// NewRouter builds and returns the API HTTP handler.
// The caller is responsible for calling http.ListenAndServe.
func NewRouter(store storage.Storage, repo database.Repository, cacheDir string) http.Handler {
	h := &handler{storage: store, repo: repo, cacheDir: cacheDir}

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
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

	workDir, err := os.Getwd()
	if err != nil {
		log.Printf("getwd: %v; using relative path", err)
		workDir = "."
	}
	filesDir := http.Dir(filepath.Join(workDir, "data", "files"))
	fileServer(r, "/files", filesDir, h)

	imagesDir := http.Dir(filepath.Join(workDir, "data", "images"))
	r.Get("/images/*", func(w http.ResponseWriter, r *http.Request) {
		rctx := chi.RouteContext(r.Context())
		pathPrefix := strings.TrimSuffix(rctx.RoutePattern(), "/*")
		http.StripPrefix(pathPrefix, http.FileServer(imagesDir)).ServeHTTP(w, r)
	})

	return r
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
		rctx := chi.RouteContext(r.Context())
		pathPrefix := strings.TrimSuffix(rctx.RoutePattern(), "/*")
		fs := http.StripPrefix(pathPrefix, http.FileServer(root))
		fs.ServeHTTP(w, r)
	})
}
