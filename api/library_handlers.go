package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/config"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/media"
	"github.com/go-chi/chi/v5"
)

const maxImageSize = 10 << 20 // 10 MB

// allowedImageMIMETypes / allowedImageExtensions gate cover uploads. WebP is
// intentionally excluded: the variant pipeline (media.ProcessImage) only decodes
// JPEG and PNG, so accepting WebP here would store an original the worker can
// never process. See docs/plans/roadmap.md (Covers) for the (non-breaking)
// path to add WebP later.
var allowedImageMIMETypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
}

var allowedImageExtensions = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
}

func (h *handler) listArtists(w http.ResponseWriter, r *http.Request) {
	var (
		artists []*database.ArtistEntry
		err     error
	)
	if h.guestListing(r.Context()) {
		artists, err = h.repo.ListArtistsGuest(r.Context())
	} else {
		artists, err = h.repo.ListArtists(r.Context())
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	type artistItem struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		TrackCount int    `json:"track_count"`
		HasImage   bool   `json:"has_image"`
	}

	items := make([]artistItem, 0, len(artists))
	for _, a := range artists {
		items = append(items, artistItem{
			ID:         a.ID,
			Name:       a.Name,
			TrackCount: a.TrackCount,
			HasImage:   a.HasImage,
		})
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *handler) listAlbums(w http.ResponseWriter, r *http.Request) {
	artistID, ok := parsePositiveID(w, r.URL.Query().Get("artist_id"), "artist_id")
	if !ok {
		return
	}
	var (
		albums []*database.AlbumEntry
		err    error
	)
	if h.guestListing(r.Context()) {
		albums, err = h.repo.ListAlbumsByArtistIDGuest(r.Context(), artistID)
	} else {
		albums, err = h.repo.ListAlbumsByArtistID(r.Context(), artistID)
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	type albumItem struct {
		ID         int64  `json:"id"`
		ArtistID   int64  `json:"artist_id"`
		Title      string `json:"title"`
		ArtistName string `json:"artist_name"`
		Year       *int64 `json:"year"`
		TrackCount int    `json:"track_count"`
		HasImage   bool   `json:"has_image"`
	}

	items := make([]albumItem, 0, len(albums))
	for _, a := range albums {
		var year *int64
		if a.Year.Valid {
			year = &a.Year.Int64
		}
		items = append(items, albumItem{
			ID:         a.ID,
			ArtistID:   a.ArtistID,
			Title:      a.Title,
			ArtistName: a.ArtistName,
			Year:       year,
			TrackCount: a.TrackCount,
			HasImage:   a.HasImage,
		})
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *handler) listTracks(w http.ResponseWriter, r *http.Request) {
	albumID, ok := parsePositiveID(w, r.URL.Query().Get("album_id"), "album_id")
	if !ok {
		return
	}

	var (
		tracks []*database.TrackEntry
		err    error
	)
	if h.guestListing(r.Context()) {
		tracks, err = h.repo.ListTracksByAlbumIDGuest(r.Context(), albumID)
	} else {
		tracks, err = h.repo.ListTracksByAlbumID(r.Context(), albumID)
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	type trackItem struct {
		ID          int64    `json:"id"`
		Title       string   `json:"title"`
		ArtistName  string   `json:"artist_name"`
		TrackNumber *int64   `json:"track_number"`
		DiscNumber  *int64   `json:"disc_number"`
		Duration    *float64 `json:"duration_seconds"`
		URL         string   `json:"url"`
		MimeType    string   `json:"mime_type"`
	}

	items := make([]trackItem, 0, len(tracks))
	for _, t := range tracks {
		var trackNum *int64
		if t.TrackNumber.Valid {
			trackNum = &t.TrackNumber.Int64
		}
		var discNum *int64
		if t.DiscNumber.Valid {
			discNum = &t.DiscNumber.Int64
		}
		var dur *float64
		if t.DurationSeconds.Valid {
			dur = &t.DurationSeconds.Float64
		}
		items = append(items, trackItem{
			ID:          t.ID,
			Title:       t.Title,
			ArtistName:  t.ArtistName,
			TrackNumber: trackNum,
			DiscNumber:  discNum,
			Duration:    dur,
			URL:         "/files/" + t.ObjectKey,
			MimeType:    t.MimeType,
		})
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *handler) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if len(q) > 200 {
		http.Error(w, "query too long", http.StatusBadRequest)
		return
	}

	var (
		results *database.SearchResults
		err     error
	)
	if h.guestListing(r.Context()) {
		results, err = h.repo.SearchGuest(r.Context(), q)
	} else {
		results, err = h.repo.Search(r.Context(), q)
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	type artistItem struct {
		ID         int64  `json:"id"`
		Name       string `json:"name"`
		TrackCount int    `json:"track_count"`
		HasImage   bool   `json:"has_image"`
	}
	type albumItem struct {
		ID         int64  `json:"id"`
		ArtistID   int64  `json:"artist_id"`
		Title      string `json:"title"`
		ArtistName string `json:"artist_name"`
		Year       *int64 `json:"year"`
		TrackCount int    `json:"track_count"`
		HasImage   bool   `json:"has_image"`
	}
	type trackItem struct {
		ID          int64    `json:"id"`
		Title       string   `json:"title"`
		TrackNumber *int64   `json:"track_number"`
		Duration    *float64 `json:"duration_seconds"`
		URL         string   `json:"url"`
		MimeType    string   `json:"mime_type"`
		ArtistName  string   `json:"artist_name"`
		AlbumTitle  string   `json:"album_title"`
	}
	type response struct {
		Artists []artistItem `json:"artists"`
		Albums  []albumItem  `json:"albums"`
		Tracks  []trackItem  `json:"tracks"`
	}

	resp := response{
		Artists: make([]artistItem, 0),
		Albums:  make([]albumItem, 0),
		Tracks:  make([]trackItem, 0),
	}
	for _, a := range results.Artists {
		resp.Artists = append(resp.Artists, artistItem{ID: a.ID, Name: a.Name, TrackCount: a.TrackCount, HasImage: a.HasImage})
	}
	for _, a := range results.Albums {
		var year *int64
		if a.Year.Valid {
			year = &a.Year.Int64
		}
		resp.Albums = append(resp.Albums, albumItem{ID: a.ID, ArtistID: a.ArtistID, Title: a.Title, ArtistName: a.ArtistName, Year: year, TrackCount: a.TrackCount, HasImage: a.HasImage})
	}
	for _, t := range results.Tracks {
		var trackNum *int64
		if t.TrackNumber.Valid {
			trackNum = &t.TrackNumber.Int64
		}
		var dur *float64
		if t.DurationSeconds.Valid {
			dur = &t.DurationSeconds.Float64
		}
		resp.Tracks = append(resp.Tracks, trackItem{
			ID: t.ID, Title: t.Title, TrackNumber: trackNum, Duration: dur,
			URL: "/files/" + t.ObjectKey, MimeType: t.MimeType,
			ArtistName: t.ArtistName, AlbumTitle: t.AlbumTitle,
		})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *handler) getArtistImage(w http.ResponseWriter, r *http.Request) {
	artistID, ok := parsePositiveID(w, chi.URLParam(r, "artist_id"), "artist_id")
	if !ok {
		return
	}
	objectKey, mimeType, found, err := h.repo.GetArtistImage(r.Context(), artistID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	h.serveImageFile(w, r, objectKey, mimeType)
}

func (h *handler) uploadArtistImage(w http.ResponseWriter, r *http.Request) {
	artist := chi.URLParam(r, "artist")
	ctx := r.Context()

	// Resolve + add-only guard before reading the body so an uploader replacing a
	// cover is rejected early (no wasted upload).
	artistID, err := h.repo.ResolveArtistID(ctx, artist)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	_, _, hasCover, err := h.repo.GetArtistImage(ctx, artistID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if h.coverReplaceBlocked(w, r, hasCover) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxImageSize)
	data, mimeType, ext, err := h.readImageUpload(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Artist covers have no variant pipeline yet (deferred — see the plan), so
	// they keep the flat <base_key><ext> object key rather than a <base_key>/
	// variant directory. The schema reserves variant columns for later.
	objectKey := media.BaseKey(data) + ext
	if err := os.MkdirAll(h.imagesDir, 0o755); err != nil {
		http.Error(w, "cannot create images dir", http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(filepath.Join(h.imagesDir, objectKey), data, 0o644); err != nil {
		http.Error(w, "cannot save image", http.StatusInternalServerError)
		return
	}
	if err := h.repo.UpsertArtistImage(ctx, artistID, objectKey, mimeType, time.Now().Unix()); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	h.audit(ctx, "metadata.image", "artist:"+artist, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// coverReplaceBlocked rejects (and returns true) when an authenticated caller
// without metadata.edit tries to overwrite an existing cover: a file.upload-only
// uploader may set a missing cover but never replace one. Anonymous callers were
// already stopped by the route gate; auth-unconfigured callers (id == nil) are
// unrestricted, as elsewhere.
func (h *handler) coverReplaceBlocked(w http.ResponseWriter, r *http.Request, hasCover bool) bool {
	id := auth.FromContext(r.Context())
	if id != nil && hasCover && !id.Has(auth.PermMetadataEdit) {
		http.Error(w, "a cover is already set — only a metadata editor can replace it", http.StatusForbidden)
		return true
	}
	return false
}

func (h *handler) getAlbumImage(w http.ResponseWriter, r *http.Request) {
	albumID, ok := parsePositiveID(w, chi.URLParam(r, "album_id"), "album_id")
	if !ok {
		return
	}
	objectKey, mimeType, found, err := h.repo.GetAlbumImage(r.Context(), albumID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	h.serveImageFile(w, r, objectKey, mimeType)
}

// albumImageStatusResponse is the JSON body of GET /api/albums/{album}/image/status.
// When has_cover is false: variants_ready is false, base_key is "", variants is {}.
// When variants_ready is false but a cover exists, the variant URLs are still
// included (they are deterministic and may already exist partially) — the UI is
// responsible for not displaying images until variants_ready is true.
type albumImageStatusResponse struct {
	HasCover      bool              `json:"has_cover"`
	VariantsReady bool              `json:"variants_ready"`
	BaseKey       string            `json:"base_key"`
	SourceExt     string            `json:"source_ext"`
	Variants      map[string]string `json:"variants"`
}

func (h *handler) getAlbumImageStatus(w http.ResponseWriter, r *http.Request) {
	albumID, ok := parsePositiveID(w, chi.URLParam(r, "album_id"), "album_id")
	if !ok {
		return
	}
	baseKey, sourceExt, ready, found, err := h.repo.GetAlbumCoverStatus(r.Context(), albumID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	resp := albumImageStatusResponse{
		HasCover:      found,
		VariantsReady: ready,
		BaseKey:       baseKey,
		SourceExt:     sourceExt,
		Variants:      map[string]string{},
	}
	// base_key is empty for legacy rows written before variants existed; those
	// have no deterministic variant paths, so report no variant URLs for them.
	if found && baseKey != "" {
		for _, name := range media.AllVariants {
			resp.Variants[name] = media.VariantURL(baseKey, name, sourceExt)
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// getUIConfig serves the parsed webui.toml (upload-page worker controls). It is
// public — the upload UI needs it before login. Falls back to built-in defaults
// when no UIConfig was wired (e.g. tests).
func (h *handler) getUIConfig(w http.ResponseWriter, r *http.Request) {
	cfg := h.uiConfig
	if cfg == nil {
		cfg = config.DefaultUIConfig()
	}
	// Surface the trash-restore policy so the upload UI knows how to handle a
	// "trashed" precheck result. Non-fatal: fall back to the default on error.
	policy, err := h.repo.GetTrashRestorePolicy(r.Context())
	if err != nil {
		policy = database.TrashReuploadRestores
	}
	// accepted_audio is the canonical extension→MIME allow-list (acceptedAudioTypes)
	// so the upload page can flag disallowed files and send the right Content-Type
	// without drifting from the server. See docs/api/upload.md (Accepted types).
	writeJSON(w, http.StatusOK, struct {
		*config.UIConfig
		TrashRestorePolicy string            `json:"trash_restore_policy"`
		AcceptedAudio      map[string]string `json:"accepted_audio"`
	}{cfg, policy, acceptedAudioTypes})
}

// uploadAlbumImage stores a manually uploaded album cover and triggers async
// variant generation. Unlike the embedded-cover path (which fills only when no
// cover exists), a manual upload always replaces the current cover —
// "explicit beats embedded" — via SetAlbumCover.
func (h *handler) uploadAlbumImage(w http.ResponseWriter, r *http.Request) {
	album := chi.URLParam(r, "album")
	artist := r.URL.Query().Get("artist")
	ctx := r.Context()

	albumID, err := h.repo.ResolveAlbumID(ctx, artist, album)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	_, _, hasCover, err := h.repo.GetAlbumImage(ctx, albumID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if h.coverReplaceBlocked(w, r, hasCover) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxImageSize)
	data, mimeType, ext, err := h.readImageUpload(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	baseKey := media.BaseKey(data)
	objectKey := media.VariantPath(baseKey, media.VariantOriginal, ext)
	destPath := filepath.Join(h.imagesDir, objectKey)
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		http.Error(w, "cannot create images dir", http.StatusInternalServerError)
		return
	}
	if err := os.WriteFile(destPath, data, 0o644); err != nil {
		http.Error(w, "cannot save image", http.StatusInternalServerError)
		return
	}

	now := time.Now().Unix()
	if err := h.repo.SetAlbumCover(ctx, albumID, baseKey, ext, objectKey, mimeType, now); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	subjectKey := artist + "\x1f" + album
	if err := h.repo.EnqueueImageJob(ctx, "album", subjectKey, baseKey, now); err != nil {
		// Non-fatal: the original is saved; variants stay missing until the cover
		// is re-uploaded (or a future reconciler re-enqueues the job).
		log.Printf("enqueue image job: %v", err)
	}
	if h.imagePool != nil {
		h.imagePool.Notify()
	}

	h.audit(ctx, "metadata.image", "album:"+artist+"/"+album, "manual upload")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "processing": true})
}

// readImageUpload parses and validates a multipart image upload (the "image"
// form field), returning the raw bytes, the canonical MIME type, and the
// canonical extension (".jpg"/".png" — never the raw uploaded ".jpeg"). Both the
// declared Content-Type and the filename extension must pass the allow-lists.
// It performs no disk writes; callers decide where the bytes land.
//
// The canonical extension matters: the status API, variant worker, and
// media.VariantPath all assume original.jpg / original.png, so a ".jpeg" upload
// must yield ext == ".jpg".
func (h *handler) readImageUpload(r *http.Request) (data []byte, mimeType, ext string, err error) {
	if err := r.ParseMultipartForm(maxImageSize); err != nil {
		return nil, "", "", fmt.Errorf("image too large or invalid form")
	}

	file, header, err := r.FormFile("image")
	if err != nil {
		return nil, "", "", fmt.Errorf("missing image field")
	}
	defer file.Close()

	// Parse off any parameters (e.g. "image/png; charset=binary") before the
	// allow-list check, mirroring the audio upload path.
	claimedMIME, _, parseErr := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if parseErr != nil || !allowedImageMIMETypes[claimedMIME] {
		return nil, "", "", fmt.Errorf("unsupported image type")
	}
	rawExt := strings.ToLower(filepath.Ext(sanitizeFilename(header.Filename)))
	canonicalMIME, ok := allowedImageExtensions[rawExt]
	if !ok {
		return nil, "", "", fmt.Errorf("unsupported image extension")
	}

	data, err = io.ReadAll(file)
	if err != nil {
		return nil, "", "", fmt.Errorf("read image: %w", err)
	}

	canonicalExt, ok := mimeToExt(canonicalMIME)
	if !ok {
		// Unreachable: allowedImageExtensions only maps to jpeg/png, both of
		// which mimeToExt knows. Guarded so a future allow-list edit can't
		// silently produce an empty extension.
		return nil, "", "", fmt.Errorf("unsupported image type")
	}
	return data, canonicalMIME, canonicalExt, nil
}

// metadataPatchRequest is the JSON body of PATCH /api/files/{hash}/metadata.
// Pointers distinguish an absent key (leave the field unchanged) from a present
// key (write it; "" clears the field). Only these base fields are accepted this
// round; any other key in the body is ignored. Richer tag editing is deferred —
// see .issues/open-issues.md.
type metadataPatchRequest struct {
	Title       *string `json:"title"`
	Album       *string `json:"album"`
	AlbumArtist *string `json:"album_artist"`
	Artist      *string `json:"artist"`
	// Extended tags (richer editing). Numeric fields are carried as strings so an
	// absent key (unchanged), "" (clear), and a value (set) stay distinct; the DB
	// layer parses them. See database.MetadataPatch.
	Genre       *string `json:"genre"`
	Composer    *string `json:"composer"`
	Comment     *string `json:"comment"`
	TrackNumber *string `json:"track_number"`
	TrackTotal  *string `json:"track_total"`
	DiscNumber  *string `json:"disc_number"`
	Year        *string `json:"year"`
}

// metadataJSON is the full editable-field echo shared by the GET endpoints and the
// PATCH response. Strings are "" when unset; numeric fields are null when unset.
func metadataJSON(hash string, m *database.MediaMetadata) map[string]any {
	nullInt := func(n sql.NullInt64) any {
		if !n.Valid {
			return nil
		}
		return n.Int64
	}
	return map[string]any{
		"ok":           true,
		"hash":         hash,
		"title":        m.Title,
		"album":        m.Album.String,
		"album_artist": m.AlbumArtist.String,
		"artist":       m.Artist.String,
		"genre":        m.Genre.String,
		"composer":     m.Composer.String,
		"comment":      m.Comment.String,
		"track_number": nullInt(m.TrackNumber),
		"track_total":  nullInt(m.TrackTotal),
		"disc_number":  nullInt(m.DiscNumber),
		"year":         nullInt(m.Year),
	}
}

// updateFileMetadata edits the base tags (title / album / album_artist / artist)
// of a single file, addressed by content hash. Gated on metadata.edit.
//
// It does not touch album_images: album covers are keyed by the album_artist +
// album strings, so when the album or artist changes the upload page re-POSTs the
// cover to the new identity (see §5d of the plan). The endpoint only rewrites the
// tags on the one file's media_metadata row.
func (h *handler) updateFileMetadata(w http.ResponseWriter, r *http.Request) {
	h.applyMetadataPatch(w, r, chi.URLParam(r, "hash"), "base tags")
}

// applyMetadataPatch is the shared core of the two tag-edit endpoints: the
// metadata.edit-gated PATCH /api/files/{hash}/metadata above and the
// owner-scoped PATCH /api/my/uploads/{hash}/metadata (review_handlers.go),
// whose authorization checks run before this is called.
func (h *handler) applyMetadataPatch(w http.ResponseWriter, r *http.Request, hash, auditDetail string) {
	// Tags are tiny; cap the body so a malformed request can't stream forever.
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var req metadataPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	patch := database.MetadataPatch{
		Title:       req.Title,
		Album:       req.Album,
		AlbumArtist: req.AlbumArtist,
		Artist:      req.Artist,
		Genre:       req.Genre,
		Composer:    req.Composer,
		Comment:     req.Comment,
		TrackNumber: req.TrackNumber,
		TrackTotal:  req.TrackTotal,
		DiscNumber:  req.DiscNumber,
		Year:        req.Year,
	}

	meta, err := h.repo.UpdateFileMetadata(r.Context(), hash, patch)
	if errors.Is(err, database.ErrFileNotFound) {
		http.NotFound(w, r)
		return
	}
	if errors.Is(err, database.ErrInvalidMetadata) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	h.audit(r.Context(), "metadata.edit", "file:"+hash, auditDetail)
	writeJSON(w, http.StatusOK, metadataJSON(hash, meta))
}

// getFileMetadata handles GET /api/files/{hash}/metadata — the full editable tag
// set for the edit modal to populate before editing. Gated on metadata.edit.
func (h *handler) getFileMetadata(w http.ResponseWriter, r *http.Request) {
	hash := chi.URLParam(r, "hash")
	meta, err := h.repo.FileMetadataByHash(r.Context(), hash)
	if errors.Is(err, database.ErrFileNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, metadataJSON(hash, meta))
}

// renameArtist handles POST /api/artists/{artist}/rename. The {artist} path
// segment is the current display name (resolved to its entity); the JSON body
// {"name": "..."} carries the new name. The artist entity is renamed in place,
// so all its tracks and its cover follow via their FKs — the proper way to
// rename an artist (vs. editing every track's tag). Gated on metadata.edit.
func (h *handler) renameArtist(w http.ResponseWriter, r *http.Request) {
	current := chi.URLParam(r, "artist")
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	newName := strings.TrimSpace(body.Name)
	if newName == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}

	id, found, err := h.repo.LookupArtistID(r.Context(), current)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}

	switch err := h.repo.RenameArtist(r.Context(), id, newName); {
	case errors.Is(err, database.ErrNameConflict):
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "an artist with that name already exists"})
		return
	case errors.Is(err, database.ErrEntityNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	h.audit(r.Context(), "metadata.rename", "artist:"+current, "→ "+newName)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "name": newName})
}

// renameAlbum handles POST /api/albums/{album}/rename?artist=<artist>. The
// album is addressed by its current title (path) plus its artist (query), same
// as the cover routes; the JSON body {"title": "..."} carries the new title.
// The album entity is renamed in place; tracks and cover follow via their FKs.
// Gated on metadata.edit.
func (h *handler) renameAlbum(w http.ResponseWriter, r *http.Request) {
	album := chi.URLParam(r, "album")
	artist := r.URL.Query().Get("artist")
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	newTitle := strings.TrimSpace(body.Title)
	if newTitle == "" {
		http.Error(w, "title is required", http.StatusBadRequest)
		return
	}

	id, found, err := h.repo.LookupAlbumID(r.Context(), artist, album)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}

	switch err := h.repo.RenameAlbum(r.Context(), id, newTitle); {
	case errors.Is(err, database.ErrNameConflict):
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": "an album with that title already exists for this artist"})
		return
	case errors.Is(err, database.ErrEntityNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	h.audit(r.Context(), "metadata.rename", "album:"+artist+"/"+album, "→ "+newTitle)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "title": newTitle})
}

// mergeArtists handles POST /api/artists/{artist}/merge. The {artist} path
// segment names the source ("from") artist to be merged away; the JSON body
// {"into": "..."} names the target ("into") artist that absorbs it. All of the
// source's tracks, albums, and covers move onto the target (colliding albums
// collapse), then the source entity is deleted. Gated on metadata.edit.
func (h *handler) mergeArtists(w http.ResponseWriter, r *http.Request) {
	from := chi.URLParam(r, "artist")
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body struct {
		Into string `json:"into"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	into := strings.TrimSpace(body.Into)
	if into == "" {
		http.Error(w, "into is required", http.StatusBadRequest)
		return
	}

	fromID, found, err := h.repo.LookupArtistID(r.Context(), from)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	intoID, found, err := h.repo.LookupArtistID(r.Context(), into)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "target artist not found"})
		return
	}

	switch err := h.repo.MergeArtists(r.Context(), fromID, intoID); {
	case errors.Is(err, database.ErrMergeSelf):
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "cannot merge an artist into itself"})
		return
	case errors.Is(err, database.ErrEntityNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	h.audit(r.Context(), "metadata.merge", "artist:"+from, "→ "+into)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "from_id": fromID, "into_id": intoID, "into": into})
}

// mergeAlbums handles POST /api/albums/{album}/merge?artist=<artist>. The path
// album + ?artist query name the source; the body {"into_artist","into_album"}
// names the target album. The source's tracks (and their artist) repoint onto
// the target, its cover moves if the target lacks one, then the source album is
// deleted. Gated on metadata.edit.
func (h *handler) mergeAlbums(w http.ResponseWriter, r *http.Request) {
	fromAlbum := chi.URLParam(r, "album")
	fromArtist := r.URL.Query().Get("artist")
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body struct {
		IntoArtist string `json:"into_artist"`
		IntoAlbum  string `json:"into_album"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	intoArtist := strings.TrimSpace(body.IntoArtist)
	intoAlbum := strings.TrimSpace(body.IntoAlbum)
	if intoAlbum == "" {
		http.Error(w, "into_album is required", http.StatusBadRequest)
		return
	}

	fromID, found, err := h.repo.LookupAlbumID(r.Context(), fromArtist, fromAlbum)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	intoID, found, err := h.repo.LookupAlbumID(r.Context(), intoArtist, intoAlbum)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "target album not found"})
		return
	}

	switch err := h.repo.MergeAlbums(r.Context(), fromID, intoID); {
	case errors.Is(err, database.ErrMergeSelf):
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "cannot merge an album into itself"})
		return
	case errors.Is(err, database.ErrEntityNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	h.audit(r.Context(), "metadata.merge", "album:"+fromArtist+"/"+fromAlbum, "→ "+intoArtist+"/"+intoAlbum)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "from_id": fromID, "into_id": intoID})
}

// mergeIDsRequest is the body of the id-addressed merge endpoints. Both entities
// are named by their stable surrogate id, so the merge is unambiguous even when
// names collide or one side is the empty-name bucket.
type mergeIDsRequest struct {
	FromID int64 `json:"from_id"`
	IntoID int64 `json:"into_id"`
}

func decodeMergeIDs(w http.ResponseWriter, r *http.Request) (mergeIDsRequest, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	var body mergeIDsRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return body, false
	}
	if body.FromID <= 0 || body.IntoID <= 0 {
		http.Error(w, "from_id and into_id are required", http.StatusBadRequest)
		return body, false
	}
	return body, true
}

// mergeArtistsByID handles POST /api/artists/merge with {from_id, into_id}. It
// folds the source artist into the target (moving tracks/albums/covers, collapsing
// colliding albums) and deletes the source. Gated on metadata.edit.
func (h *handler) mergeArtistsByID(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeMergeIDs(w, r)
	if !ok {
		return
	}
	switch err := h.repo.MergeArtists(r.Context(), body.FromID, body.IntoID); {
	case errors.Is(err, database.ErrMergeSelf):
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "cannot merge an artist into itself"})
		return
	case errors.Is(err, database.ErrEntityNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	h.audit(r.Context(), "metadata.merge", fmt.Sprintf("artist#%d", body.FromID), fmt.Sprintf("→ #%d", body.IntoID))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "from_id": body.FromID, "into_id": body.IntoID})
}

// mergeAlbumsByID handles POST /api/albums/merge with {from_id, into_id}. The
// source album's tracks repoint onto the target album (and its artist); the cover
// moves only if the target lacks one; the source album is deleted. Gated on
// metadata.edit.
func (h *handler) mergeAlbumsByID(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeMergeIDs(w, r)
	if !ok {
		return
	}
	switch err := h.repo.MergeAlbums(r.Context(), body.FromID, body.IntoID); {
	case errors.Is(err, database.ErrMergeSelf):
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "cannot merge an album into itself"})
		return
	case errors.Is(err, database.ErrEntityNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	h.audit(r.Context(), "metadata.merge", fmt.Sprintf("album#%d", body.FromID), fmt.Sprintf("→ #%d", body.IntoID))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "from_id": body.FromID, "into_id": body.IntoID})
}

// mergePreviewResponse is the JSON body of the two merge-preview endpoints. It
// mirrors database.MergePreview; unused fields for a given merge kind are zero.
type mergePreviewResponse struct {
	OK                   bool     `json:"ok"`
	FromID               int64    `json:"from_id"`
	IntoID               int64    `json:"into_id"`
	FromLabel            string   `json:"from_label"`
	IntoLabel            string   `json:"into_label"`
	TracksMoved          int      `json:"tracks_moved"`
	AlbumsMoved          int      `json:"albums_moved"`
	AlbumsCollapsed      int      `json:"albums_collapsed"`
	CollapsedTitles      []string `json:"collapsed_titles"`
	SourceHasCover       bool     `json:"source_has_cover"`
	TargetHasCover       bool     `json:"target_has_cover"`
	SourceArtistOrphaned bool     `json:"source_artist_orphaned"`
}

func previewResponse(p *database.MergePreview) mergePreviewResponse {
	titles := p.CollapsedTitles
	if titles == nil {
		titles = []string{}
	}
	return mergePreviewResponse{
		OK: true, FromID: p.FromID, IntoID: p.IntoID,
		FromLabel: p.FromLabel, IntoLabel: p.IntoLabel,
		TracksMoved: p.TracksMoved, AlbumsMoved: p.AlbumsMoved,
		AlbumsCollapsed: p.AlbumsCollapsed, CollapsedTitles: titles,
		SourceHasCover: p.SourceHasCover, TargetHasCover: p.TargetHasCover,
		SourceArtistOrphaned: p.SourceArtistOrphaned,
	}
}

// writeMergePreview runs a preview reader and writes its JSON, mapping the shared
// merge errors the same way the real merge endpoints do.
func (h *handler) writeMergePreview(w http.ResponseWriter, p *database.MergePreview, err error) {
	switch {
	case errors.Is(err, database.ErrMergeSelf):
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "cannot merge an entity into itself"})
	case errors.Is(err, database.ErrEntityNotFound):
		http.Error(w, "entity not found", http.StatusNotFound)
	case err != nil:
		http.Error(w, "storage error", http.StatusInternalServerError)
	default:
		writeJSON(w, http.StatusOK, previewResponse(p))
	}
}

// mergeArtistsPreview handles POST /api/artists/merge/preview with {from_id,
// into_id}. Read-only: it reports what mergeArtistsByID would do. Gated on
// metadata.edit (same as the real merge).
func (h *handler) mergeArtistsPreview(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeMergeIDs(w, r)
	if !ok {
		return
	}
	p, err := h.repo.MergeArtistsPreview(r.Context(), body.FromID, body.IntoID)
	h.writeMergePreview(w, p, err)
}

// mergeAlbumsPreview handles POST /api/albums/merge/preview with {from_id,
// into_id}. Read-only counterpart of mergeAlbumsByID. Gated on metadata.edit.
func (h *handler) mergeAlbumsPreview(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeMergeIDs(w, r)
	if !ok {
		return
	}
	p, err := h.repo.MergeAlbumsPreview(r.Context(), body.FromID, body.IntoID)
	h.writeMergePreview(w, p, err)
}

func (h *handler) serveImageFile(w http.ResponseWriter, r *http.Request, objectKey, mimeType string) {
	path := filepath.Join(h.imagesDir, objectKey)
	// Defensive check: a corrupted or crafted objectKey must not escape imagesDir.
	if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(h.imagesDir)+string(os.PathSeparator)) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeFile(w, r, path)
}
