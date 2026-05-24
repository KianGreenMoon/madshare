// This file handles file uploads with SHA-256 content-addressed storage and
// deduplication. Small files are hashed in memory; large files are spooled to
// a cache directory first so the hash can be verified before committing.
//
// Boot the server:
//
//	$ go run madshare.go
//
// Upload a file:
//
//	$ curl -X POST -F "file=@./song.mp3" http://localhost:3000/files/upload
//	{"existed":false,"filename":"song.mp3","hash":"a3f...","ok":true,"path":"a3f.../song.mp3","size":8383732}
//
// Uploading the same file again:
//
//	{"existed":true,"filename":"song.mp3","hash":"a3f...","ok":true,"size":8383732}
package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/go-chi/chi/v5"
)

const maxUploadSize = 500 << 20 // 500 MB

// memBufferLimit is the file size below which uploads are hashed entirely in
// memory. Above this threshold the upload is spooled to fileStorage.cacheDirF()
// so heap pressure stays bounded. Computed once at startup from heap headroom,
// capped at 50 MB.
var memBufferLimit = func() int64 {
	const hardCap = 50 << 20
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	// Use at most a quarter of idle heap, capped at hardCap.
	avail := int64(ms.HeapIdle) / 4
	if avail <= 0 || avail > hardCap {
		return hardCap
	}
	return avail
}()

// fileStorage abstracts where files are persisted.
// Implement this interface for local disk (localStorage) or S3.
type fileStorage interface {
	// Stat returns the stored byte count for hash, or -1 if the hash is unknown.
	Stat(hash string) (int64, error)
	// Put stores the content of r under <hash>/<filename>.
	Put(hash, filename string, r io.Reader, size int64) error
	// cacheDirF returns a local directory suitable for spooling large uploads
	// before their hash is confirmed. Remote backends (S3) return os.TempDir().
	cacheDirF() string
}

// defaultStorage is used by UploadFile. Swap it out (e.g. in main or tests)
// to point at a different backend.
var defaultStorage fileStorage = newLocalStorage("./data/files", "./data/files/.cache")

// FileServer registers static file serving under path and the upload endpoint.
func FileServer(r chi.Router, path string, root http.FileSystem) {
	if strings.ContainsAny(path, "{}*") {
		panic("FileServer does not permit any URL parameters.")
	}

	r.Post(path+"/upload", UploadFile)

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
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		http.Error(w, "file too large or invalid multipart form", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	hash, content, size, cleanup, err := hashUpload(file, header.Size, defaultStorage.cacheDirF())
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		http.Error(w, "failed to process upload", http.StatusInternalServerError)
		return
	}

	stored, err := defaultStorage.Stat(hash)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if stored == size {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"existed":  true,
			"hash":     hash,
			"filename": header.Filename,
			"size":     size,
		})
		return
	}

	if err := defaultStorage.Put(hash, header.Filename, content, size); err != nil {
		http.Error(w, "cannot save file", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"ok":       true,
		"existed":  false,
		"hash":     hash,
		"filename": header.Filename,
		"size":     size,
		"path":     hash + "/" + filepath.Base(header.Filename),
	})
}


// localStorage stores files at BaseDir/<sha256>/<filename>.
type localStorage struct {
	baseDir  string
	cacheDir string
}

func newLocalStorage(baseDir, cacheDir string) *localStorage {
	return &localStorage{baseDir: baseDir, cacheDir: cacheDir}
}

func (s *localStorage) Stat(hash string) (int64, error) {
	entries, err := os.ReadDir(filepath.Join(s.baseDir, hash))
	if os.IsNotExist(err) {
		return -1, nil
	}
	if err != nil {
		return -1, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			info, err := e.Info()
			if err != nil {
				return -1, err
			}
			return info.Size(), nil
		}
	}
	return -1, nil
}

func (s *localStorage) Put(hash, filename string, r io.Reader, size int64) error {
	dir := filepath.Join(s.baseDir, hash)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	dst := filepath.Join(dir, filepath.Base(filename))
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

func (s *localStorage) cacheDirF() string { return s.cacheDir }

// hashUpload computes the SHA-256 of r. Files with declaredSize <= memBufferLimit
// are read entirely into memory. Larger files are spooled to a temp file in
// cacheDir so heap usage stays bounded.
//
// The returned cleanup func (if non-nil) must be deferred by the caller to
// remove the temp file once it is no longer needed.
func hashUpload(r io.Reader, declaredSize int64, cacheDir string) (
	hash string, content io.Reader, size int64, cleanup func(), err error,
) {
	h := sha256.New()

	if declaredSize <= memBufferLimit {
		buf, readErr := io.ReadAll(io.TeeReader(r, h))
		if readErr != nil {
			err = readErr
			return
		}
		hash = hex.EncodeToString(h.Sum(nil))
		content = bytes.NewReader(buf)
		size = int64(len(buf))
		return
	}

	// Large file: spool to a temp file while computing the hash.
	if err = os.MkdirAll(cacheDir, 0755); err != nil {
		return
	}
	tmp, tmpErr := os.CreateTemp(cacheDir, "upload-*")
	if tmpErr != nil {
		err = tmpErr
		return
	}
	cleanup = func() {
		tmp.Close()
		os.Remove(tmp.Name())
	}

	n, copyErr := io.Copy(io.MultiWriter(tmp, h), r)
	if copyErr != nil {
		err = copyErr
		return
	}
	if _, seekErr := tmp.Seek(0, io.SeekStart); seekErr != nil {
		err = seekErr
		return
	}

	hash = hex.EncodeToString(h.Sum(nil))
	content = tmp
	size = n
	return
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
