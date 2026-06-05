package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/config"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/media"
)

// actorID returns the acting user's id from the request context as a nullable
// column value (invalid when the request is anonymous / unauthenticated).
func actorID(ctx context.Context) sql.NullInt64 {
	if id := auth.FromContext(ctx); id != nil {
		return sql.NullInt64{Int64: id.UserID, Valid: true}
	}
	return sql.NullInt64{}
}

// audit records a privileged action, logging (but not failing the request) on
// error — the audit log must never block the operation it describes.
func (h *handler) audit(ctx context.Context, action, target, detail string) {
	if err := h.repo.RecordAudit(ctx, actorID(ctx), action, target, detail); err != nil {
		log.Printf("audit %s %s: %v", action, target, err)
	}
}

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
	// imagesDir is where artist/album cover images are stored and served. It is
	// the "images" subdirectory of the configured files_dir.
	imagesDir string
	// maxUploadSize caps the upload request body in bytes (from config).
	maxUploadSize int64
	// authzEnabled mirrors Deps.Auth != nil: when true, library listings are
	// access-filtered for the requesting identity (content.all and anonymous
	// handled in the listing handlers). When false (open embedding / tests),
	// listings are unfiltered, matching fileAccessGuard's pass-through.
	authzEnabled bool
	// imagePool, when non-nil, is notified after a cover-variant job is enqueued
	// so an idle worker wakes immediately. Nil in tests / open embeddings.
	imagePool interface{ Notify() }
	// uiConfig backs GET /api/ui/config (the upload page's worker controls).
	// Nil-safe: getUIConfig falls back to config.DefaultUIConfig() when unset.
	uiConfig *config.UIConfig
}

func (h *handler) uploadFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, h.maxUploadSize)
	if err := r.ParseMultipartForm(h.maxUploadSize); err != nil {
		http.Error(w, "file too large or invalid multipart form", http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "missing file field", http.StatusBadRequest)
		return
	}
	defer file.Close()

	// Parse off any parameters (e.g. "audio/mpeg; charset=utf-8") so the
	// allow-list check compares the bare media type.
	mimeType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil || !allowedMIMETypes[mimeType] {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}

	// Reduce the client-supplied name to a safe base name before it is used
	// for the extension check, on-disk path, and download URL.
	filename := sanitizeFilename(header.Filename)
	if filename == "" {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}
	ext := strings.ToLower(filepath.Ext(filename))
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
		if existing.DeletedAt.Valid {
			// File is in the trash. Re-uploading the same bytes intentionally
			// restores it — any uploader may do this by design (see open-issues.md).
			if _, err := h.repo.RestoreFileByHash(ctx, existing.Hash); err != nil {
				http.Error(w, "storage error", http.StatusInternalServerError)
				return
			}
			h.audit(ctx, "file.restore", hash, "restore-via-reupload: "+filename)
		} else {
			h.audit(ctx, "file.upload", hash, "dedup: "+filename)
		}
		if err := h.repo.RecordUpload(ctx, existing.ID, filename); err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":       true,
			"existed":  true,
			"hash":     hash,
			"filename": filename,
			"size":     size,
		})
		return
	}

	// New file: extract tags before writing so a parse failure doesn't
	// leave an orphan blob.
	tags := extractTagsOrEmpty(content, mimeType)

	if err := h.storage.Put(hash, filename, content); err != nil {
		http.Error(w, "cannot save file", http.StatusInternalServerError)
		return
	}

	now := time.Now().Unix()
	f := &database.File{
		Hash:           hash,
		ByteSize:       size,
		MimeType:       mimeType,
		StorageBackend: "local",
		ObjectKey:      hash + "/" + filename,
		CreatedAt:      now,
		UploadedBy:     actorID(ctx),
	}
	upload := &database.FileUpload{Filename: filename, UploadedAt: now}
	meta := tagsToMetadata(tags, now)

	if err := h.repo.InsertFile(ctx, f, upload, meta); err != nil {
		// The blob is on disk but the DB doesn't know about it. Log
		// loudly so the reconciler (or an operator) can clean it up.
		log.Printf("orphan blob: hash=%s err=%v", hash, err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	h.audit(ctx, "file.upload", hash, filename)

	writeJSON(w, http.StatusCreated, map[string]any{
		"ok":       true,
		"existed":  false,
		"hash":     hash,
		"filename": filename,
		"size":     size,
	})
}

// sanitizeFilename reduces a client-supplied filename to a safe base name.
// It strips both Unix and Windows path components: filepath.Base alone does
// not remove backslash-separated segments on non-Windows hosts, so a name
// like `C:\Users\evil.mp3` would otherwise be stored verbatim and produce a
// malformed ObjectKey and broken download URL. Returns "" when nothing usable
// remains.
func sanitizeFilename(name string) string {
	// Reject NUL and other control characters: a NUL would otherwise pass the
	// extension check and fail later at os.Create with a confusing 500.
	if strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return ""
	}
	// Normalise Windows separators so filepath.Base strips the directory part.
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	if name == "." || name == "/" || name == ".." {
		return ""
	}
	return name
}

// accessFilter reports whether library listings should be access-filtered for
// this request, and the actor id to filter by. Filtering applies only when
// authz is configured and the identity lacks content.all (admins/moderators see
// everything); when authz is off it mirrors fileAccessGuard's pass-through.
func (h *handler) accessFilter(ctx context.Context) (userID sql.NullInt64, filter bool) {
	if !h.authzEnabled {
		return sql.NullInt64{}, false
	}
	if auth.FromContext(ctx).Has(auth.PermContentAll) {
		return sql.NullInt64{}, false
	}
	return actorID(ctx), true
}

func (h *handler) listFiles(w http.ResponseWriter, r *http.Request) {
	var (
		entries []*database.FileListEntry
		err     error
	)
	if uid, filter := h.accessFilter(r.Context()); filter {
		entries, err = h.repo.ListFilesFiltered(r.Context(), uid)
	} else {
		entries, err = h.repo.ListFiles(r.Context())
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	type fileItem struct {
		ID            int64    `json:"id"`
		Hash          string   `json:"hash"`
		Filename      string   `json:"filename"`
		MimeType      string   `json:"mime_type"`
		ByteSize      int64    `json:"byte_size"`
		URL           string   `json:"url"`
		Title         string   `json:"title"`
		Artist        string   `json:"artist"`
		AlbumArtist   string   `json:"album_artist"`
		Album         string   `json:"album"`
		Year          int64    `json:"year"`
		Duration      *float64 `json:"duration"` // seconds; null when not yet extracted
		GuestPlayable bool     `json:"guest_playable"`
		License       string   `json:"license"`
	}

	items := make([]fileItem, 0, len(entries))
	for _, e := range entries {
		var dur *float64
		if e.DurationSeconds.Valid {
			dur = &e.DurationSeconds.Float64
		}
		items = append(items, fileItem{
			ID:            e.ID,
			Hash:          e.Hash,
			Filename:      e.Filename,
			MimeType:      e.MimeType,
			ByteSize:      e.ByteSize,
			URL:           "/files/" + e.ObjectKey,
			Title:         e.Title,
			Artist:        e.Artist,
			AlbumArtist:   e.AlbumArtist.String,
			Album:         e.Album,
			Year:          e.Year,
			Duration:      dur,
			GuestPlayable: e.GuestPlayable,
			License:       e.License.String,
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
	// CORS headers are set globally by corsMiddleware.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
