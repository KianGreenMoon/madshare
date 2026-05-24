package api

import (
	"encoding/json"
	"net/http"
	"path/filepath"

	"daemonlord.ygg/madshare/api/storage"
)

const maxUploadSize = 500 << 20 // 500 MB

// handler holds the dependencies for the API HTTP handlers.
type handler struct {
	storage storage.Storage
}

func (h *handler) uploadFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
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

	hash, content, size, cleanup, err := storage.HashUpload(file, header.Size, h.storage.CacheDir())
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		http.Error(w, "failed to process upload", http.StatusInternalServerError)
		return
	}
	if hash == "" {
		http.Error(w, "failed to hash upload", http.StatusInternalServerError)
		return
	}

	stored, err := h.storage.Stat(hash)
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

	if err := h.storage.Put(hash, header.Filename, content, size); err != nil {
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
