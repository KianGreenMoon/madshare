package api

import (
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/media"
)

const maxUploadSize = 500 << 20 // 500 MB

// allowedMIMETypes is the set of media types accepted for upload.
// v0 is audio only; video support is deferred.
var allowedMIMETypes = map[string]bool{
	"audio/mpeg":  true,
	"audio/ogg":   true,
	"audio/flac":  true,
	"audio/wav":   true,
	"audio/x-wav": true,
	"audio/mp4":   true,
}

// allowedExtensions guards against MIME bypass: an attacker can declare any
// Content-Type, but the stored filename's extension determines what the file
// server advertises to browsers. Both checks must pass.
var allowedExtensions = map[string]bool{
	".mp3":  true,
	".ogg":  true,
	".flac": true,
	".wav":  true,
	".mp4":  true,
	".m4a":  true,
	".aac":  true,
	".opus": true,
}

// handler holds the dependencies for the API HTTP handlers.
type handler struct {
	storage  storage.Storage
	repo     database.Repository
	cacheDir string
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

	mimeType := header.Header.Get("Content-Type")
	if !allowedMIMETypes[mimeType] {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if !allowedExtensions[ext] {
		http.Error(w, "unsupported file extension", http.StatusUnsupportedMediaType)
		return
	}

	hash, content, size, cleanup, err := storage.HashUpload(file, header.Size, h.cacheDir)
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

	ctx := r.Context()

	// Dedupe via DB: same content hash means we already have the bytes
	// on disk. Record the (possibly new) filename and short-circuit.
	existing, err := h.repo.GetFileByHash(ctx, hash)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if existing != nil {
		if err := h.repo.RecordUpload(ctx, existing.ID, header.Filename); err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"existed":  true,
			"hash":     hash,
			"filename": header.Filename,
			"size":     size,
		})
		return
	}

	// New file: extract tags before writing so a parse failure doesn't
	// leave an orphan blob.
	tags := extractTagsOrEmpty(content, mimeType)

	if err := h.storage.Put(hash, header.Filename, content); err != nil {
		http.Error(w, "cannot save file", http.StatusInternalServerError)
		return
	}

	now := time.Now().Unix()
	f := &database.File{
		Hash:           hash,
		ByteSize:       size,
		MimeType:       mimeType,
		StorageBackend: "local",
		ObjectKey:      hash + "/" + header.Filename,
		CreatedAt:      now,
	}
	upload := &database.FileUpload{Filename: header.Filename, UploadedAt: now}
	meta := tagsToMetadata(tags, now)

	if err := h.repo.InsertFile(ctx, f, upload, meta); err != nil {
		// The blob is on disk but the DB doesn't know about it. Log
		// loudly so the reconciler (or an operator) can clean it up.
		log.Printf("orphan blob: hash=%s err=%v", hash, err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"ok":       true,
		"existed":  false,
		"hash":     hash,
		"filename": header.Filename,
		"size":     size,
	})
}

func (h *handler) listFiles(w http.ResponseWriter, r *http.Request) {
	entries, err := h.repo.ListFiles(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	type fileItem struct {
		ID          int64    `json:"id"`
		Hash        string   `json:"hash"`
		Filename    string   `json:"filename"`
		MimeType    string   `json:"mime_type"`
		ByteSize    int64    `json:"byte_size"`
		URL         string   `json:"url"`
		Title       string   `json:"title"`
		Artist      string   `json:"artist"`
		AlbumArtist string   `json:"album_artist"`
		Album       string   `json:"album"`
		Year        int64    `json:"year"`
		Duration    *float64 `json:"duration"` // seconds; null when not yet extracted
	}

	items := make([]fileItem, 0, len(entries))
	for _, e := range entries {
		var dur *float64
		if e.DurationSeconds.Valid {
			dur = &e.DurationSeconds.Float64
		}
		items = append(items, fileItem{
			ID:          e.ID,
			Hash:        e.Hash,
			Filename:    e.Filename,
			MimeType:    e.MimeType,
			ByteSize:    e.ByteSize,
			URL:         "/files/" + e.ObjectKey,
			Title:       e.Title,
			Artist:      e.Artist,
			AlbumArtist: e.AlbumArtist.String,
			Album:       e.Album,
			Year:        e.Year,
			Duration:    dur,
		})
	}
	writeJSON(w, http.StatusOK, items)
}

// extractTagsOrEmpty runs media.ExtractTags on content if it is seekable.
// A failure or non-seekable reader returns empty Tags — metadata is
// nice-to-have, not load-bearing for the upload flow.
//
// content is left positioned at offset 0 so the subsequent storage.Put
// reads the full body.
func extractTagsOrEmpty(content io.Reader, mimeType string) *media.Tags {
	seeker, ok := content.(io.ReadSeeker)
	if !ok {
		return &media.Tags{}
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		return &media.Tags{}
	}
	tags, err := media.ExtractTags(seeker, mimeType)
	if err != nil {
		log.Printf("tag extraction failed: %v", err)
		tags = &media.Tags{}
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		log.Printf("rewind after tag extraction: %v", err)
	}
	return tags
}

// tagsToMetadata maps the in-process Tags struct onto the nullable
// database.MediaMetadata struct. Empty strings and zero ints become NULL.
func tagsToMetadata(t *media.Tags, extractedAt int64) *database.MediaMetadata {
	return &database.MediaMetadata{
		Title:       nullString(t.Title),
		Artist:      nullString(t.Artist),
		Album:       nullString(t.Album),
		AlbumArtist: nullString(t.AlbumArtist),
		Genre:       nullString(t.Genre),
		Composer:    nullString(t.Composer),
		Comment:     nullString(t.Comment),
		TagFormat:   nullString(t.TagFormat),
		Year:        nullInt(t.Year),
		TrackNumber: nullInt(t.TrackNumber),
		TrackTotal:  nullInt(t.TrackTotal),
		DiscNumber:  nullInt(t.DiscNumber),
		ExtractedAt: extractedAt,
	}
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func nullInt(i int) sql.NullInt64 {
	return sql.NullInt64{Int64: int64(i), Valid: i != 0}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
