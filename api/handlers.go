package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/config"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/media"
	"daemonlord.ygg/madshare/prune"
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

// acceptedAudioTypes maps an accepted upload extension to its canonical audio
// MIME type. v0 is audio only; video support is deferred.
//
// The extension is the security-relevant guard: it determines what the file
// server later advertises to browsers (defended further by X-Content-Type-
// Options: nosniff on the file routes). The browser-declared part Content-Type
// is unreliable — empty for FLAC/M4A/OPUS, application/octet-stream from
// curl -F — so it is not used to gate; the canonical MIME here is persisted and
// served instead. This single map is also surfaced to the upload page at
// GET /api/ui/config (accepted_audio) so client and server share one source of
// truth. See docs/api/upload.md (Accepted types).
var acceptedAudioTypes = map[string]string{
	".mp3":  "audio/mpeg",
	".ogg":  "audio/ogg",
	".flac": "audio/flac",
	".wav":  "audio/wav",
	".mp4":  "audio/mp4",
	".m4a":  "audio/mp4",
	".aac":  "audio/aac",
	".opus": "audio/opus",
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
	// access-filtered for the requesting identity (content.access and anonymous
	// handled in the listing handlers). When false (open embedding / tests),
	// listings are unfiltered, matching fileAccessGuard's pass-through.
	authzEnabled bool
	// imagePool, when non-nil, is notified after a cover-variant job is enqueued
	// so an idle worker wakes immediately. Nil in tests / open embeddings.
	imagePool interface{ Notify() }
	// mediaPool, when non-nil, is notified after an analysis job (ffprobe +
	// fpcalc) is enqueued so an idle worker wakes immediately. Nil in tests.
	mediaPool interface{ Notify() }
	// pruneMgr owns the single Verify & Prune background job. Nil when no manager
	// was wired (the prune endpoints then respond 503).
	pruneMgr *prune.Manager
	// limiter, when non-nil, gates concurrent uploads (global + per-user). Nil
	// disables the gate (tests / unlimited config).
	limiter *UploadLimiter
	// uiConfig backs GET /api/ui/config (the upload page's worker controls).
	// Nil-safe: getUIConfig falls back to config.DefaultUIConfig() when unset.
	uiConfig *config.UIConfig
	// source, when non-nil, serves the AGPL-required source archive at GET /source.
	// Nil when no SourceRoot was configured (e.g. in tests via NewRouter).
	source *sourceArchiver
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

// guestListing reports whether library listings should be restricted to the
// guest-visible subset for this request. That is the case only when authz is
// configured and the identity lacks content.access (full-library users see
// everything); when authz is off it mirrors fileAccessGuard's pass-through.
func (h *handler) guestListing(ctx context.Context) bool {
	if !h.authzEnabled {
		return false
	}
	return !auth.FromContext(ctx).Has(auth.PermContentAccess)
}

func (h *handler) listFiles(w http.ResponseWriter, r *http.Request) {
	var (
		entries []*database.FileListEntry
		err     error
	)
	if h.guestListing(r.Context()) {
		entries, err = h.repo.ListFilesGuest(r.Context())
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
		TrackNumber   *int64   `json:"track_number"` // null when untagged (used to sort the grouped view)
		DiscNumber    *int64   `json:"disc_number"`  // null when untagged (groups the grouped view)
		Year          int64    `json:"year"`
		Duration      *float64 `json:"duration"` // seconds; null when not yet extracted
		GuestPlayable bool     `json:"guest_playable"`
		License       string   `json:"license"`
		// ArtistHasImage / AlbumHasImage drive the grouped view's "Add cover"
		// affordance (offered only when the entity has no cover yet).
		ArtistHasImage bool `json:"artist_has_image"`
		AlbumHasImage  bool `json:"album_has_image"`
	}

	items := make([]fileItem, 0, len(entries))
	for _, e := range entries {
		var dur *float64
		if e.DurationSeconds.Valid {
			dur = &e.DurationSeconds.Float64
		}
		var trackNum *int64
		if e.TrackNumber.Valid {
			trackNum = &e.TrackNumber.Int64
		}
		var discNum *int64
		if e.DiscNumber.Valid {
			discNum = &e.DiscNumber.Int64
		}
		items = append(items, fileItem{
			ID:             e.ID,
			Hash:           e.Hash,
			Filename:       e.Filename,
			MimeType:       e.MimeType,
			ByteSize:       e.ByteSize,
			URL:            "/files/" + e.ObjectKey,
			Title:          e.Title,
			Artist:         e.Artist,
			AlbumArtist:    e.AlbumArtist.String,
			Album:          e.Album,
			TrackNumber:    trackNum,
			DiscNumber:     discNum,
			Year:           e.Year,
			Duration:       dur,
			GuestPlayable:  e.GuestPlayable,
			License:        e.License.String,
			ArtistHasImage: e.ArtistHasImage,
			AlbumHasImage:  e.AlbumHasImage,
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

// tagsToMetadata maps the in-process Tags struct onto the database.MediaMetadata
// struct. Empty strings and zero ints become NULL. Title may be empty here; the
// required non-empty value (filename with extension stripped) is filled by
// InsertFile, which has the upload filename (migration 016).
func tagsToMetadata(t *media.Tags, extractedAt int64) *database.MediaMetadata {
	return &database.MediaMetadata{
		Title:       t.Title,
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

// parsePositiveID parses a required positive int64 entity id from a query/path
// value, writing a 400 and returning ok=false on a missing, malformed, or
// non-positive value. Used by the id-addressed browse and cover endpoints.
func parsePositiveID(w http.ResponseWriter, raw, name string) (int64, bool) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, name+" must be a positive integer", http.StatusBadRequest)
		return 0, false
	}
	return id, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	// CORS headers are set globally by corsMiddleware.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
