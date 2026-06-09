package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/media"
	"github.com/go-chi/chi/v5"
)

// uploadFile handles POST /files/upload: a single multipart file upload. It
// hashes and dedupes the content, extracts tags on first insert, and — for a
// genuinely new file carrying embedded cover art — fills the album cover when
// the album has none yet (see maybeSaveEmbeddedCover).
func (h *handler) uploadFile(w http.ResponseWriter, r *http.Request) {
	// Concurrency gate (global + per-user). auth.FromContext returns nil when
	// anonymous — only reachable with auth unconfigured, since /files/upload is
	// otherwise gated by protect(auth.PermFileUpload); such requests collapse to
	// the "" key. Identity.UserID is an int64 field, formatted as the limiter's
	// string key. No admin bypass (see the plan's locked decisions).
	if h.limiter != nil {
		userID := ""
		if id := auth.FromContext(r.Context()); id != nil {
			userID = strconv.FormatInt(id.UserID, 10)
		}
		if err := h.limiter.Acquire(userID); err != nil {
			writeUploadLimitError(w, err)
			return
		}
		defer h.limiter.Release(userID)
	}

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
		restored := false
		if existing.DeletedAt.Valid {
			// File is in the trash. Whether re-uploading the bytes restores it is
			// governed by the admin trash-restore policy (default reupload_restores,
			// the historical behavior). Under inform/uploader_restore it stays
			// trashed. See docs/plans/upload-rework.md §3b.
			policy, err := h.repo.GetTrashRestorePolicy(ctx)
			if err != nil {
				http.Error(w, "storage error", http.StatusInternalServerError)
				return
			}
			if policy == database.TrashReuploadRestores {
				if _, err := h.repo.RestoreFileByHash(ctx, existing.Hash); err != nil {
					http.Error(w, "storage error", http.StatusInternalServerError)
					return
				}
				restored = true
				h.audit(ctx, "file.restore", hash, "restore-via-reupload: "+filename)
			} else {
				h.audit(ctx, "file.upload", hash, "dedup-trashed (policy="+policy+", not restored): "+filename)
			}
		} else {
			h.audit(ctx, "file.upload", hash, "dedup: "+filename)
		}
		// Record the (new) filename only for a live or just-restored file; a file
		// left trashed gets no new upload record.
		if (!existing.DeletedAt.Valid || restored) && filename != "" {
			if err := h.repo.RecordUpload(ctx, existing.ID, filename); err != nil {
				http.Error(w, "storage error", http.StatusInternalServerError)
				return
			}
		}
		// Embedded art is not re-processed for duplicate content — the cover (if
		// any) was already handled when the bytes were first ingested.
		// album/artist are left empty on dedup: tags aren't re-extracted for
		// already-stored bytes, and any cover was handled at first ingest.
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":               true,
			"existed":          true,
			"restored":         restored,
			"trashed":          existing.DeletedAt.Valid && !restored,
			"hash":             hash,
			"filename":         filename,
			"size":             size,
			"title":            "",
			"album":            "",
			"artist":           "",
			"cover_found":      false,
			"cover_processing": false,
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

	// Fill the album cover from embedded art when the album has none yet.
	// cover_found reports that usable embedded art (with album+artist context)
	// was present; cover_processing reports that this upload actually claimed
	// the cover and queued variant generation.
	coverArtist := effectiveAlbumArtist(tags)
	coverFound := tags.CoverImage != nil && tags.Album != "" && coverArtist != ""
	coverProcessing := false
	if coverFound {
		coverProcessing = h.maybeSaveEmbeddedCover(ctx, tags, coverArtist)
	}

	// album / artist (effective album artist) are echoed so the upload page can
	// group tracks and target the cover endpoints (POST/status) without a second
	// metadata round-trip. Empty when the file carried no such tags.
	writeJSON(w, http.StatusCreated, map[string]any{
		"ok":               true,
		"existed":          false,
		"hash":             hash,
		"filename":         filename,
		"size":             size,
		"title":            tags.Title,
		"album":            tags.Album,
		"artist":           coverArtist,
		"cover_found":      coverFound,
		"cover_processing": coverProcessing,
	})
}

// effectiveAlbumArtist returns the artist used to group an album cover: the
// album artist when set, otherwise the track artist. This mirrors the library's
// grouping rule (album_artist preferred over artist).
func effectiveAlbumArtist(t *media.Tags) string {
	if t.AlbumArtist != "" {
		return t.AlbumArtist
	}
	return t.Artist
}

// maybeSaveEmbeddedCover stores the audio file's embedded cover art as the album
// cover when the album has none yet, returning true only when this call claimed
// the cover and enqueued a variant job. artist is the already-resolved effective
// album artist (see effectiveAlbumArtist).
//
// Concurrency: two tracks of the same album uploaded at once both pass the cheap
// HasAlbumCover pre-check, so correctness rests on SetAlbumCoverIfAbsent, an
// atomic INSERT ... ON CONFLICT DO NOTHING. Exactly one caller wins the row; the
// losers return false without enqueuing. The original is written before the
// claim so the winner's file is always present. Tracks of one album normally
// embed identical art (same base_key → same path → the "loser" wrote the very
// same bytes, no orphan); only the rare distinct-art loser leaves a harmless
// orphan image file — never a row pointing at a missing file.
func (h *handler) maybeSaveEmbeddedCover(ctx context.Context, tags *media.Tags, artist string) bool {
	if tags.CoverImage == nil || tags.Album == "" || artist == "" {
		return false
	}
	// Resolve the album entity (InsertFile already created it for this track, so
	// this is effectively a lookup). The cover attaches to the stable album id.
	albumID, err := h.repo.ResolveAlbumID(ctx, artist, tags.Album)
	if err != nil {
		log.Printf("embedded cover: resolve album: %v", err)
		return false
	}
	// Cheap pre-check: skip the decode/write entirely once a cover exists. The
	// atomic claim below is what actually guarantees fill-if-missing under races.
	if has, err := h.repo.HasAlbumCover(ctx, albumID); err != nil || has {
		return false
	}
	ext, ok := mimeToExt(tags.CoverImage.MIMEType)
	if !ok {
		return false // unsupported embedded format (e.g. webp/gif) — skip, do not enqueue
	}
	// Cap the embedded cover at the same ceiling the manual-upload path enforces
	// (maxImageSize). Without this, a crafted audio file could carry a huge APIC
	// frame and write it verbatim to disk (bounded only by max_upload_mb) —
	// disk-write amplification, and orphan bloat under the distinct-art race.
	if len(tags.CoverImage.Data) > maxImageSize {
		log.Printf("embedded cover: %d bytes exceeds %d cap; skipping", len(tags.CoverImage.Data), maxImageSize)
		return false
	}
	baseKey := media.BaseKey(tags.CoverImage.Data)
	objectKey := media.VariantPath(baseKey, media.VariantOriginal, ext)
	destPath := filepath.Join(h.imagesDir, objectKey)
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		log.Printf("embedded cover: mkdir %s: %v", filepath.Dir(destPath), err)
		return false
	}
	if err := os.WriteFile(destPath, tags.CoverImage.Data, 0o644); err != nil {
		log.Printf("embedded cover: write %s: %v", destPath, err)
		return false
	}
	now := time.Now().Unix()
	inserted, err := h.repo.SetAlbumCoverIfAbsent(ctx, albumID, baseKey, ext, objectKey, tags.CoverImage.MIMEType, now)
	if err != nil {
		log.Printf("embedded cover: claim album cover: %v", err)
		return false
	}
	if !inserted {
		// Another upload claimed this album's cover first. Our write (only when
		// the art differs) is a harmless orphan; nothing else to do.
		return false
	}
	subjectKey := artist + "\x1f" + tags.Album
	if err := h.repo.EnqueueImageJob(ctx, "album", subjectKey, baseKey, now); err != nil {
		log.Printf("embedded cover: enqueue job: %v", err)
		return false
	}
	if h.imagePool != nil {
		h.imagePool.Notify()
	}
	return true
}

// writeUploadLimitError responds 429 with the JSON contract the upload client
// expects ({"error":…,"code":"upload_limit"}) and a Retry-After hint. The client
// reduces its worker count and re-queues the file on this code.
func writeUploadLimitError(w http.ResponseWriter, err error) {
	msg := "server upload limit reached"
	if errors.Is(err, ErrUserLimit) {
		msg = "user upload limit reached"
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": msg,
		"code":  "upload_limit",
	})
}

// mimeToExt maps a canonical image MIME type to its file extension. Only the
// two formats the variant pipeline can process (media.ProcessImage) are
// accepted; everything else returns ok=false so callers skip it rather than
// queue a job the worker would only fail to process.
func mimeToExt(mimeType string) (string, bool) {
	switch mimeType {
	case "image/jpeg":
		return ".jpg", true
	case "image/png":
		return ".png", true
	}
	return "", false
}

// checkFile reports whether content with the given SHA-256 hash is already on the
// server: "absent" (no such content), "present" (live), or "trashed" (soft-
// deleted). Advisory only — the client uses it to skip duplicate uploads; the
// upload path re-hashes and dedupes on receipt regardless. Gated on file.upload
// (a by-hash existence oracle must not be anonymous). See docs/plans/upload-rework.md §3b.
func (h *handler) checkFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var body struct {
		Hash string `json:"hash"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	hash := strings.ToLower(strings.TrimSpace(body.Hash))
	if !isSHA256Hex(hash) {
		http.Error(w, "invalid hash (want 64 hex chars)", http.StatusBadRequest)
		return
	}

	f, err := h.repo.GetFileByHash(r.Context(), hash)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	status := "absent"
	if f != nil {
		if f.DeletedAt.Valid {
			status = "trashed"
		} else {
			status = "present"
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

// restoreFileForUploader lets a non-admin uploader restore a trashed file —
// but only when the admin trash-restore policy is "uploader_restore" (else 403).
// Gated on file.upload (restoring content you may upload is equivalent). See
// docs/plans/upload-rework.md §3b.
func (h *handler) restoreFileForUploader(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "hash")))
	if !isSHA256Hex(hash) {
		http.Error(w, "invalid hash", http.StatusBadRequest)
		return
	}
	policy, err := h.repo.GetTrashRestorePolicy(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if policy != database.TrashUploaderRestore {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "uploader restore is not enabled"})
		return
	}
	found, err := h.repo.RestoreFileByHash(r.Context(), hash)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	h.audit(r.Context(), "file.restore", hash, "uploader-restore")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// isSHA256Hex reports whether s is exactly 64 lowercase hex characters.
func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
