package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"maps"
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
	"daemonlord.ygg/madshare/sources"
	"daemonlord.ygg/madshare/storages"
	"daemonlord.ygg/madshare/tagsource"
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
	".oga":  "audio/ogg",
	".flac": "audio/flac",
	".wav":  "audio/wav",
	".mp4":  "audio/mp4",
	".m4a":  "audio/mp4",
	".aac":  "audio/aac",
	".opus": "audio/opus",
}

// AcceptedAudioTypes returns a copy of the canonical extension→MIME audio
// allow-list. It is the single source of truth shared by the upload gate and the
// symlink-source scan engine (passed to sources.New to avoid an import cycle).
func AcceptedAudioTypes() map[string]string {
	out := make(map[string]string, len(acceptedAudioTypes))
	maps.Copy(out, acceptedAudioTypes)
	return out
}

// handler holds the dependencies for the API HTTP handlers.
type handler struct {
	storage storage.Storage
	repo    database.Repository
	// manage is the content-access store (per-file guest/license), used by the
	// bulk-edit action to write access alongside tags. Nil in tests / open
	// embeddings — the bulk handler then rejects access-bearing edits.
	manage ManageStore
	// spoolDir is the upload spool: large uploads are staged here as a temp file
	// while hashing (see storage.HashUpload). Not a cache.
	spoolDir string
	// imagesDir is where artist/album cover variants are stored and served — the
	// "images" subdirectory of the configured variants_dir (it falls back to
	// files_dir when variants_dir is unset; see Deps.newHandler). Served at /images.
	imagesDir string
	// sourceImagesDir is where cover source originals are stored — the "images"
	// subdirectory of files_dir (<files_dir>/images/<hash>/original<ext>). It is a
	// regenerate seed for the variant worker and is NEVER served; /images is rooted
	// at imagesDir (the variants tree), a different directory. See variants.md.
	sourceImagesDir string
	// filesDir is the configured source-blob root (parent of the audio/ subtree);
	// reported as the storage panel's "location".
	filesDir string
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
	// sourcesMgr owns symlink data sources (list/add/scan). Nil when no manager
	// was wired (the /api/admin/sources endpoints then respond 503).
	sourcesMgr *sources.Manager
	// linker is the write/probe side of the links storage. It makes hard delete
	// storage-aware (a links row → unlink the symlink, never the external target)
	// and backs external-bytes accounting + links health. Nil → no links storage
	// (deletes fall back to the local store; accounting omits external figures).
	linker *storages.Linker
	// limiter, when non-nil, gates concurrent uploads (global + per-user). Nil
	// disables the gate (tests / unlimited config).
	limiter *UploadLimiter
	// blobReg resolves a content hash to its on-disk path across storages
	// (local, then links) — the tag-suggestion sources re-read blobs through it,
	// same precedence as the /files server. Never nil (Deps.blobStorages falls
	// back to a local-only registry).
	blobReg *storages.Registry
	// uiConfig backs GET /api/ui/config (the upload page's worker controls).
	// Nil-safe: getUIConfig falls back to config.DefaultUIConfig() when unset.
	uiConfig *config.UIConfig
	// acoustid / musicbrainz are the shared external tag-suggestion clients:
	// AcoustID fingerprint lookup (P1) and MusicBrainz text search (P2). Rate
	// limiters + caches are process-global — madshare.go wires one instance of
	// each into every listener's Deps. Nil (tests / open embeddings) means the
	// musicbrainz suggestion source is unavailable regardless of settings.
	acoustid    *tagsource.AcoustID
	musicbrainz *tagsource.MusicBrainz
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

// canSeeRemoved reports whether this request may list soft-removed blobs (the
// All-files "Show removed" toggle): a moderation/curation capability, passed
// through when authz is off (open embedding).
func (h *handler) canSeeRemoved(ctx context.Context) bool {
	if !h.authzEnabled {
		return true
	}
	id := auth.FromContext(ctx)
	return id.Has(auth.PermContentModerate) || id.Has(auth.PermFileDelete)
}

// fileListDefaultLimit / fileListMaxLimit bound the page window. A missing
// limit defaults to fileListDefaultLimit; limit=0 is a count-only request (the
// dashboard reads just total); anything above the max is clamped.
const (
	fileListDefaultLimit = 100
	fileListMaxLimit     = 500
)

// normalizeQField allow-lists the search-field scope (the UI's filter-type
// dropdown). Anything outside the set falls back to "" — search every field —
// so a stray value can never widen or break the query.
func normalizeQField(s string) string {
	switch s {
	case "artist", "album", "title":
		return s
	default:
		return ""
	}
}

func (h *handler) listFiles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := q.Get("q")
	if len(query) > 200 {
		http.Error(w, "query too long", http.StatusBadRequest)
		return
	}
	filter := database.FileFilter{
		Guest:  h.guestListing(r.Context()),
		Q:      query,
		QField: normalizeQField(q.Get("field")),
	}
	// show_removed additionally lists soft-removed blobs (absorbed / removed
	// renditions) with their state — the All-files physical view
	// (recording-tagsets P5). Moderation-capability only: removal state is
	// curation detail, not library content.
	if q.Get("show_removed") == "1" && h.canSeeRemoved(r.Context()) {
		filter.ShowRemoved = true
	}
	limit := clampInt(q.Get("limit"), fileListDefaultLimit, 0, fileListMaxLimit)
	offset := clampInt(q.Get("offset"), 0, 0, 1<<30)

	total, err := h.repo.CountFiles(r.Context(), filter)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}

	// limit=0 → count-only: skip the page query entirely.
	var entries []*database.FileListEntry
	if limit > 0 {
		entries, err = h.repo.ListFilesPage(r.Context(), database.FileListQuery{
			FileFilter: filter, Sort: q.Get("sort"), Limit: limit, Offset: offset,
		})
		if err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
	}

	type fileItem struct {
		ID            int64    `json:"id"`
		Hash          string   `json:"hash"`
		Filename      string   `json:"filename"`
		MimeType      string   `json:"mime_type"`
		ByteSize      int64    `json:"byte_size"`
		CreatedAt     int64    `json:"created_at"`
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
		// Physical columns (recording-tagsets P5): storage backend, the
		// recording link, and whether the blob is soft-removed (only ever true
		// under show_removed).
		StorageBackend string `json:"storage_backend"`
		RecordingID    int64  `json:"recording_id"`
		Removed        bool   `json:"removed"`
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
			CreatedAt:      e.CreatedAt,
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
			StorageBackend: e.StorageBackend,
			RecordingID:    e.RecordingID,
			Removed:        e.DeletedAt.Valid,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":  total,
		"limit":  limit,
		"offset": offset,
		"items":  items,
	})
}

// clampInt parses a query-string integer, falling back to def when empty or
// malformed, then clamps the result to [lo, hi].
func clampInt(s string, def, lo, hi int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	if n < lo {
		return lo
	}
	if n > hi {
		return hi
	}
	return n
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
