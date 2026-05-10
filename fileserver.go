// This example demonstrates how to serve static files from your filesystem.
//
// Boot the server:
//
//	$ go run main.go
//
// Client requests:
//
//	$ curl http://localhost:3000/files/
//	<pre>
//	<a href="notes.txt">notes.txt</a>
//	</pre>
//
//	$ curl http://localhost:3000/files/notes.txt
//	Notessszzz
//
//	$ curl -X POST -F "file=@./01 - Murmaider.mp3" http://localhost:3000/files/upload
//  {"filename":"01 - Murmaider.mp3","ok":true,"path":"data/01 - Murmaider.mp3","size":8383732}
package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"encoding/json"
	"io"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

func mainfs() {
	r := chi.NewRouter()
	r.Use(middleware.Logger)

	// Create a route along /files that will serve contents from
	// the ./data/ folder.
	workDir, _ := os.Getwd()
	filesDir := http.Dir(filepath.Join(workDir, "data"))
	FileServer(r, "/files", filesDir)

	http.ListenAndServe(":3000", r)
}

// FileServer conveniently sets up a http.FileServer handler to serve
// static files from a http.FileSystem.
func FileServer(r chi.Router, path string, root http.FileSystem) {
	if strings.ContainsAny(path, "{}*") {
		panic("FileServer does not permit any URL parameters.")
	}

	r.Post(path + "/upload", UploadFile)

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

func UploadFile(w http.ResponseWriter, r *http.Request) {
	const maxUploadSize = 50 << 20 // 50 MB

	err := r.ParseMultipartForm(maxUploadSize)
	if err != nil {
		http.Error(w, "file too large or invalid multipart form", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	err = os.MkdirAll("./data", 0755)
	if err != nil {
		http.Error(w, "cannot create upload dir", http.StatusInternalServerError)
		return
	}

	dstPath := filepath.Join("./data", filepath.Base(header.Filename))
	dst, err := os.Create(dstPath)
	if err != nil {
		http.Error(w, "cannot create destination file", http.StatusInternalServerError)
		return
	}
	defer dst.Close()

	_, err = io.Copy(dst, file)
	if err != nil {
		http.Error(w, "cannot save file", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":       true,
		"filename": header.Filename,
		"size":     header.Size,
		"path":     dstPath,
	})
}
