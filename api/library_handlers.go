package api

import (
	"crypto/sha256"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
)

const maxImageSize = 10 << 20 // 10 MB

var allowedImageMIMETypes = map[string]bool{
	"image/jpeg": true,
	"image/png":  true,
	"image/webp": true,
}

var allowedImageExtensions = map[string]string{
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".webp": "image/webp",
}

func (h *handler) listArtists(w http.ResponseWriter, r *http.Request) {
	var (
		artists []*database.ArtistEntry
		err     error
	)
	if uid, filter := h.accessFilter(r.Context()); filter {
		artists, err = h.repo.ListArtistsFiltered(r.Context(), uid)
	} else {
		artists, err = h.repo.ListArtists(r.Context())
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	type artistItem struct {
		Name       string `json:"name"`
		TrackCount int    `json:"track_count"`
		HasImage   bool   `json:"has_image"`
	}

	items := make([]artistItem, 0, len(artists))
	for _, a := range artists {
		items = append(items, artistItem{
			Name:       a.Name,
			TrackCount: a.TrackCount,
			HasImage:   a.HasImage,
		})
	}
	writeJSON(w, http.StatusOK, items)
}

func (h *handler) listAlbums(w http.ResponseWriter, r *http.Request) {
	artist := r.URL.Query().Get("artist")
	var (
		albums []*database.AlbumEntry
		err    error
	)
	if uid, filter := h.accessFilter(r.Context()); filter {
		albums, err = h.repo.ListAlbumsByArtistFiltered(r.Context(), artist, uid)
	} else {
		albums, err = h.repo.ListAlbumsByArtist(r.Context(), artist)
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	type albumItem struct {
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
	artist := r.URL.Query().Get("artist")
	album := r.URL.Query().Get("album")

	var (
		tracks []*database.TrackEntry
		err    error
	)
	if uid, filter := h.accessFilter(r.Context()); filter {
		tracks, err = h.repo.ListTracksByAlbumArtistFiltered(r.Context(), artist, album, uid)
	} else {
		tracks, err = h.repo.ListTracksByAlbumArtist(r.Context(), artist, album)
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	type trackItem struct {
		ID          int64    `json:"id"`
		Title       string   `json:"title"`
		TrackNumber *int64   `json:"track_number"`
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
		var dur *float64
		if t.DurationSeconds.Valid {
			dur = &t.DurationSeconds.Float64
		}
		items = append(items, trackItem{
			ID:          t.ID,
			Title:       t.Title,
			TrackNumber: trackNum,
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
	if uid, filter := h.accessFilter(r.Context()); filter {
		results, err = h.repo.SearchFiltered(r.Context(), q, uid)
	} else {
		results, err = h.repo.Search(r.Context(), q)
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	type artistItem struct {
		Name       string `json:"name"`
		TrackCount int    `json:"track_count"`
		HasImage   bool   `json:"has_image"`
	}
	type albumItem struct {
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
		resp.Artists = append(resp.Artists, artistItem{Name: a.Name, TrackCount: a.TrackCount, HasImage: a.HasImage})
	}
	for _, a := range results.Albums {
		var year *int64
		if a.Year.Valid {
			year = &a.Year.Int64
		}
		resp.Albums = append(resp.Albums, albumItem{Title: a.Title, ArtistName: a.ArtistName, Year: year, TrackCount: a.TrackCount, HasImage: a.HasImage})
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
	artist := chi.URLParam(r, "artist")
	objectKey, mimeType, found, err := h.repo.GetArtistImage(r.Context(), artist)
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
	r.Body = http.MaxBytesReader(w, r.Body, maxImageSize)
	objectKey, mimeType, err := h.saveImageUpload(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.repo.UpsertArtistImage(r.Context(), artist, objectKey, mimeType, time.Now().Unix()); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	h.audit(r.Context(), "metadata.image", "artist:"+artist, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *handler) getAlbumImage(w http.ResponseWriter, r *http.Request) {
	album := chi.URLParam(r, "album")
	artist := r.URL.Query().Get("artist")
	objectKey, mimeType, found, err := h.repo.GetAlbumImage(r.Context(), artist, album)
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

func (h *handler) uploadAlbumImage(w http.ResponseWriter, r *http.Request) {
	album := chi.URLParam(r, "album")
	artist := r.URL.Query().Get("artist")
	r.Body = http.MaxBytesReader(w, r.Body, maxImageSize)
	objectKey, mimeType, err := h.saveImageUpload(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := h.repo.UpsertAlbumImage(r.Context(), artist, album, objectKey, mimeType, time.Now().Unix()); err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	h.audit(r.Context(), "metadata.image", "album:"+artist+"/"+album, "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (h *handler) saveImageUpload(r *http.Request) (objectKey, mimeType string, err error) {
	if err := r.ParseMultipartForm(maxImageSize); err != nil {
		return "", "", fmt.Errorf("image too large or invalid form")
	}

	file, header, err := r.FormFile("image")
	if err != nil {
		return "", "", fmt.Errorf("missing image field")
	}
	defer file.Close()

	// Parse off any parameters (e.g. "image/png; charset=binary") before the
	// allow-list check, mirroring the audio upload path.
	claimedMIME, _, parseErr := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if parseErr != nil || !allowedImageMIMETypes[claimedMIME] {
		return "", "", fmt.Errorf("unsupported image type")
	}
	ext := strings.ToLower(filepath.Ext(sanitizeFilename(header.Filename)))
	canonicalMIME, ok := allowedImageExtensions[ext]
	if !ok {
		return "", "", fmt.Errorf("unsupported image extension")
	}

	data, err := io.ReadAll(file)
	if err != nil {
		return "", "", fmt.Errorf("read image: %w", err)
	}

	sum := sha256.Sum256(data)
	hashHex := fmt.Sprintf("%x", sum)
	key := hashHex[:16] + ext

	if err := os.MkdirAll(h.imagesDir, 0o755); err != nil {
		return "", "", fmt.Errorf("create images dir: %w", err)
	}
	dest := filepath.Join(h.imagesDir, key)
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return "", "", fmt.Errorf("write image: %w", err)
	}

	return key, canonicalMIME, nil
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
