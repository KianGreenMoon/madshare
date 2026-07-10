package api

// /api/admin/recordings — the recording-centric curation surface
// (recording-tagsets P5, docs/architecture/recording-tagsets.md "Admin
// surfaces"): the paged listing behind /admin/recordings, the both-arms
// detail, and the curation operations (merge, appearance move / set-primary,
// rendition remove/restore, whole-recording trash + hard delete, access edit).
// Listing/curation is content.moderate; deletes are file.delete; the access
// edit is metadata.edit (the same gates the equivalent file-addressed ops use).

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
)

type recordingRowDTO struct {
	ID                 int64  `json:"id"`
	Title              string `json:"title"`
	Artist             string `json:"artist"`
	LiveRenditions     int    `json:"live_renditions"`
	RemovedFiles       int    `json:"removed_files"`
	Appearances        int    `json:"appearances"`
	TrashedAppearances int    `json:"trashed_appearances"`
	BestFormat         string `json:"best_format"`
	Dormant            bool   `json:"dormant"`
	Pinned             bool   `json:"pinned"`
	License            string `json:"license"`
	GuestPlayable      bool   `json:"guest_playable"`
	CreatedAt          int64  `json:"created_at"`
}

type recordingRenditionDTO struct {
	FileID     int64   `json:"file_id"`
	Hash       string  `json:"hash"`
	URL        string  `json:"url"`
	Format     string  `json:"format"`
	Bitrate    int     `json:"bitrate"`
	SampleRate int     `json:"sample_rate"`
	BitDepth   int     `json:"bit_depth"`
	Size       int64   `json:"size"`
	Duration   float64 `json:"duration"`
	Rank       int     `json:"rank"` // 0 for removed blobs (not on the ladder)
	Best       bool    `json:"best"`
	Removed    bool    `json:"removed"`
	Pinned     bool    `json:"pinned"`
}

type recordingAppearanceDTO struct {
	TagsetID    int64  `json:"tagset_id"`
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	AlbumArtist string `json:"album_artist"`
	Album       string `json:"album"`
	Disc        *int64 `json:"disc,omitempty"`
	Track       *int64 `json:"track,omitempty"`
	Year        *int64 `json:"year,omitempty"`
	ReviewState string `json:"review_state"`
	Trashed     bool   `json:"trashed"`
	IsPrimary   bool   `json:"is_primary"`
	CreatedAt   int64  `json:"created_at"`
}

type recordingDetailDTO struct {
	ID              int64                    `json:"id"`
	CreatedAt       int64                    `json:"created_at"`
	License         string                   `json:"license"`
	GuestPlayable   bool                     `json:"guest_playable"`
	GuestManual     bool                     `json:"guest_manual"`
	Pinned          bool                     `json:"pinned"`
	PreferredFileID int64                    `json:"preferred_file_id,omitempty"`
	Renditions      []recordingRenditionDTO  `json:"renditions"`
	Appearances     []recordingAppearanceDTO `json:"appearances"`
}

// parseRecordingID reads the {recordingID} route param; 0 means invalid (the
// caller responds 400).
func parseRecordingID(r *http.Request) int64 {
	id, err := strconv.ParseInt(chi.URLParam(r, "recordingID"), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// recordingsList handles GET /api/admin/recordings?q=&filter=&limit=&offset= —
// the paged listing, newest first, with the filter pills' semantics
// (multi_rendition / multi_appearance / dormant / pinned) and the #id /
// substring search. Gated content.moderate.
func (h *handler) recordingsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	countOnly := q.Get("limit") == "0" // limit=0 = count-only (the dashboard badge)
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	if offset < 0 {
		offset = 0
	}
	opts := database.RecordingListOptions{
		Search: q.Get("q"),
		Filter: q.Get("filter"),
		Limit:  limit,
		Offset: offset,
	}
	total, err := h.repo.CountRecordings(r.Context(), opts)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	var rows []database.RecordingRow
	if !countOnly {
		rows, err = h.repo.ListRecordings(r.Context(), opts)
		if err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
	}
	items := make([]recordingRowDTO, 0, len(rows))
	for _, rec := range rows {
		items = append(items, recordingRowDTO{
			ID: rec.ID, Title: rec.Title, Artist: rec.DisplayArtist,
			LiveRenditions: rec.LiveRenditions, RemovedFiles: rec.RemovedFiles,
			Appearances: rec.Appearances, TrashedAppearances: rec.TrashedTagsets,
			BestFormat: rec.BestFormat, Dormant: rec.Dormant, Pinned: rec.Pinned,
			License: rec.License, GuestPlayable: rec.GuestPlayable, CreatedAt: rec.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": total, "limit": limit, "offset": offset, "items": items,
	})
}

// recordingsDetail handles GET /api/admin/recordings/{recordingID} — both arms
// for the expanded card: renditions (live ladder-ranked best-first, then the
// soft-removed blobs) and appearances (primary first, incl. trashed ones).
// Gated content.moderate.
func (h *handler) recordingsDetail(w http.ResponseWriter, r *http.Request) {
	recID := parseRecordingID(r)
	if recID == 0 {
		http.Error(w, "recording id must be a positive integer", http.StatusBadRequest)
		return
	}
	d, err := h.repo.GetRecordingDetail(r.Context(), recID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if d == nil {
		http.NotFound(w, r)
		return
	}

	// Rank the live renditions on the shared ladder; removed blobs carry no rank.
	var ladder []database.Rendition
	for _, f := range d.Renditions {
		if !f.Removed {
			ladder = append(ladder, database.Rendition{
				FileID: f.FileID, Hash: f.Hash, Codec: f.Codec,
				Bitrate: f.Bitrate, SampleRate: f.SampleRate, BitDepth: f.BitDepth,
				ByteSize: f.ByteSize,
			})
		}
	}
	rankByFile := make(map[int64]int, len(ladder))
	for i, f := range database.RankRenditions(ladder) {
		rankByFile[f.FileID] = i + 1
	}

	dto := recordingDetailDTO{
		ID: d.ID, CreatedAt: d.CreatedAt, License: d.License,
		GuestPlayable: d.GuestPlayable, GuestManual: d.GuestManual,
		Pinned: d.Pinned, PreferredFileID: d.PreferredFileID,
		Renditions:  make([]recordingRenditionDTO, 0, len(d.Renditions)),
		Appearances: make([]recordingAppearanceDTO, 0, len(d.Appearances)),
	}
	for _, f := range d.Renditions {
		format := f.Codec
		if format == "" {
			format = f.MimeType
		}
		rank := rankByFile[f.FileID]
		dto.Renditions = append(dto.Renditions, recordingRenditionDTO{
			FileID: f.FileID, Hash: f.Hash, URL: "/files/" + f.ObjectKey,
			Format: format, Bitrate: f.Bitrate, SampleRate: f.SampleRate,
			BitDepth: f.BitDepth, Size: f.ByteSize, Duration: f.DurationSeconds,
			Rank: rank, Best: rank == 1, Removed: f.Removed, Pinned: f.Pinned,
		})
	}
	nullable := func(v int64, valid bool) *int64 {
		if !valid {
			return nil
		}
		return &v
	}
	for _, a := range d.Appearances {
		dto.Appearances = append(dto.Appearances, recordingAppearanceDTO{
			TagsetID: a.TagsetID, Title: a.Title, Artist: a.Artist,
			AlbumArtist: a.AlbumArtist, Album: a.Album,
			Disc:        nullable(a.DiscNumber.Int64, a.DiscNumber.Valid),
			Track:       nullable(a.TrackNumber.Int64, a.TrackNumber.Valid),
			Year:        nullable(a.Year.Int64, a.Year.Valid),
			ReviewState: a.ReviewState, Trashed: a.Trashed,
			IsPrimary: a.IsPrimary, CreatedAt: a.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, dto)
}

// recordingsMerge handles POST /api/admin/recordings/merge — fold the source
// recordings into the target (body {target_id, source_ids}). Renditions move
// pinned, appearances dedup (target wins), sources vanish. Gated
// content.moderate; a stale selection 404s so the page reloads.
func (h *handler) recordingsMerge(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		TargetID  int64   `json:"target_id"`
		SourceIDs []int64 `json:"source_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	if req.TargetID <= 0 || len(req.SourceIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "target_id and a non-empty source_ids are required"})
		return
	}
	out, err := h.repo.MergeRecordings(r.Context(), req.TargetID, req.SourceIDs)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !out.Found {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "a selected recording is no longer available; reload"})
		return
	}
	h.audit(r.Context(), "recording.merge", strconv.FormatInt(req.TargetID, 10),
		fmt.Sprintf("merged %d recording(s): %d rendition(s) moved, %d appearance(s) moved, %d dropped",
			out.SourcesMerged, out.RenditionsMoved, out.AppearancesMoved, out.AppearancesDropped))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "sources_merged": out.SourcesMerged,
		"renditions_moved":  out.RenditionsMoved,
		"appearances_moved": out.AppearancesMoved, "appearances_dropped": out.AppearancesDropped,
	})
}

// tagsetMove handles POST /api/admin/tagsets/{tagsetID}/move — re-home an
// appearance onto another existing recording (body {target_recording_id}).
// The refusals map to statuses: same recording → 400, last appearance /
// identity collision → 409 with a reason, unknown → 404. Gated
// content.moderate.
func (h *handler) tagsetMove(w http.ResponseWriter, r *http.Request) {
	tagsetID, err := strconv.ParseInt(chi.URLParam(r, "tagsetID"), 10, 64)
	if err != nil || tagsetID <= 0 {
		http.Error(w, "tagset id must be a positive integer", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		TargetRecordingID int64 `json:"target_recording_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TargetRecordingID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "target_recording_id is required"})
		return
	}
	out, err := h.repo.MoveTagset(r.Context(), tagsetID, req.TargetRecordingID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	switch {
	case !out.Found:
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "appearance or target recording not found"})
	case out.SameRecording:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "the appearance is already on that recording"})
	case out.LastAppearance:
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "reason": "last_appearance",
			"error": "this is the recording's only appearance — merge the recordings instead"})
	case out.Collides:
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "reason": "collides",
			"error": "an identical appearance already exists on the target recording"})
	default:
		h.audit(r.Context(), "tagset.move", strconv.FormatInt(tagsetID, 10),
			"to recording "+strconv.FormatInt(req.TargetRecordingID, 10))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

// recordingsSetPrimary handles POST /api/admin/recordings/{recordingID}/primary
// (body {tagset_id}) — the appearance that names the recording. Gated
// content.moderate.
func (h *handler) recordingsSetPrimary(w http.ResponseWriter, r *http.Request) {
	recID := parseRecordingID(r)
	if recID == 0 {
		http.Error(w, "recording id must be a positive integer", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		TagsetID int64 `json:"tagset_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.TagsetID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "tagset_id is required"})
		return
	}
	found, err := h.repo.SetPrimaryTagset(r.Context(), recID, req.TagsetID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "the appearance does not belong to this recording"})
		return
	}
	h.audit(r.Context(), "recording.set_primary", strconv.FormatInt(recID, 10),
		"tagset "+strconv.FormatInt(req.TagsetID, 10))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// recordingsAddAppearance handles POST /api/admin/recordings/{recordingID}/appearances
// — the "Add appearance" form: a hand-authored, blobless, approved appearance on
// an existing recording (recording-tagsets P7d). Gated content.moderate (the
// same gate as merge / move / set-primary — it publishes a library-visible row).
// The DB refusals map to specific statuses so the form can explain them; the new
// appearance is never primary (Set primary promotes it deliberately).
func (h *handler) recordingsAddAppearance(w http.ResponseWriter, r *http.Request) {
	recID := parseRecordingID(r)
	if recID == 0 {
		http.Error(w, "recording id must be a positive integer", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	// The numeric fields arrive as strings (the shared track-edit.js form sends
	// every input's value verbatim, the same convention MetadataPatch uses); an
	// empty or absent one is "unset", a non-numeric one is a 400.
	var req struct {
		Title       string `json:"title"`
		Artist      string `json:"artist"`
		AlbumArtist string `json:"album_artist"`
		Album       string `json:"album"`
		Genre       string `json:"genre"`
		Composer    string `json:"composer"`
		Comment     string `json:"comment"`
		Year        string `json:"year"`
		TrackNumber string `json:"track_number"`
		TrackTotal  string `json:"track_total"`
		DiscNumber  string `json:"disc_number"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	year, err1 := parseOptInt(req.Year)
	track, err2 := parseOptInt(req.TrackNumber)
	total, err3 := parseOptInt(req.TrackTotal)
	disc, err4 := parseOptInt(req.DiscNumber)
	if err := cmp.Or(err1, err2, err3, err4); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "year, track, and disc must be whole numbers"})
		return
	}
	out, err := h.repo.CreateAppearance(r.Context(), recID, database.AppearanceInput{
		Title: req.Title, Artist: req.Artist, AlbumArtist: req.AlbumArtist, Album: req.Album,
		Genre: req.Genre, Composer: req.Composer, Comment: req.Comment,
		Year: year, TrackNumber: track, TrackTotal: total, DiscNumber: disc,
	}, actorID(r.Context()))
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	switch {
	case out.NotFound:
		http.NotFound(w, r)
	case out.EmptyTitle:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "a title is required"})
	case out.Nameless:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"ok": false, "reason": "nameless",
			"error": "give the appearance an artist or an album — a nameless one adds nothing"})
	case out.Collides:
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "reason": "collides",
			"error": "an identical appearance already exists on this recording"})
	default:
		h.audit(r.Context(), "appearance.create", strconv.FormatInt(recID, 10),
			"tagset "+strconv.FormatInt(out.TagsetID, 10))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tagset_id": out.TagsetID})
	}
}

// recordingsAccess handles PATCH /api/admin/recordings/{recordingID}/access —
// the editable license/guest chip. Body {license?, guest_playable?}; absent
// fields stay unchanged; the license must be in the controlled vocabulary.
// Gated metadata.edit (the same gate as the file-addressed setters).
func (h *handler) recordingsAccess(w http.ResponseWriter, r *http.Request) {
	recID := parseRecordingID(r)
	if recID == 0 {
		http.Error(w, "recording id must be a positive integer", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		License       *string `json:"license"`
		GuestPlayable *bool   `json:"guest_playable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	if req.License == nil && req.GuestPlayable == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "provide license and/or guest_playable"})
		return
	}
	if req.License != nil && !knownLicenses[*req.License] {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "unknown license"})
		return
	}
	found, err := h.repo.SetRecordingAccess(r.Context(), recID, req.License, req.GuestPlayable)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	detail := ""
	if req.License != nil {
		detail = "license=" + *req.License
	}
	if req.GuestPlayable != nil {
		if detail != "" {
			detail += " "
		}
		detail += "guest=" + strconv.FormatBool(*req.GuestPlayable)
	}
	h.audit(r.Context(), "recording.access", strconv.FormatInt(recID, 10), detail)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// recordingsTrash handles POST /api/admin/recordings/{recordingID}/trash — the
// whole-recording soft delete: every appearance goes to Trash, the recording
// falls dormant, everything is restorable. Gated file.delete.
func (h *handler) recordingsTrash(w http.ResponseWriter, r *http.Request) {
	recID := parseRecordingID(r)
	if recID == 0 {
		http.Error(w, "recording id must be a positive integer", http.StatusBadRequest)
		return
	}
	n, found, err := h.repo.TrashRecording(r.Context(), recID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	h.audit(r.Context(), "recording.trash", strconv.FormatInt(recID, 10),
		fmt.Sprintf("%d appearance(s) trashed", n))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "appearances_trashed": n})
}

// recordingsTrashBulk handles POST /api/admin/recordings/trash — the bulk bar's
// "Trash selected" (body {recording_ids}). Gated file.delete.
func (h *handler) recordingsTrashBulk(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		RecordingIDs []int64 `json:"recording_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.RecordingIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "recording_ids is required"})
		return
	}
	recs, apps, err := h.repo.BulkTrashRecordings(r.Context(), req.RecordingIDs)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	h.audit(r.Context(), "recording.bulk_trash", "recordings",
		fmt.Sprintf("%d recording(s), %d appearance(s) trashed", recs, apps))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "recordings": recs, "appearances_trashed": apps})
}

// recordingsHardDelete handles DELETE /api/admin/recordings/{recordingID} — the
// count-aware permanent delete: recording + all appearances + all files in one
// transaction, blobs reclaimed after commit (storage-aware, like the Trash
// cascade). Gated file.delete.
func (h *handler) recordingsHardDelete(w http.ResponseWriter, r *http.Request) {
	recID := parseRecordingID(r)
	if recID == 0 {
		http.Error(w, "recording id must be a positive integer", http.StatusBadRequest)
		return
	}
	out, err := h.repo.HardDeleteRecording(r.Context(), recID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !out.Found {
		http.NotFound(w, r)
		return
	}
	blobsRemoved := 0
	for _, b := range out.Blobs {
		removed, rerr := h.reclaimStorage(&database.File{StorageBackend: b.StorageBackend}, b.Hash)
		if rerr != nil {
			log.Printf("orphan blob: hash=%s err=%v", b.Hash, rerr)
			continue
		}
		if removed {
			blobsRemoved++
		}
	}
	h.audit(r.Context(), "recording.delete", strconv.FormatInt(recID, 10),
		fmt.Sprintf("%d appearance(s), %d file(s) deleted", out.Appearances, out.Files))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "appearances": out.Appearances, "files": out.Files, "blobs_removed": blobsRemoved,
	})
}

// renditionRemove / renditionRestore handle POST
// /api/admin/renditions/{fileID}/{remove,restore} — the renditions arm's
// per-row soft blob removal and its inverse (removing the last one leaves the
// recording dormant, reversible). Gated file.delete.
func (h *handler) renditionRemove(w http.ResponseWriter, r *http.Request) {
	h.renditionSetRemoved(w, r, true)
}

func (h *handler) renditionRestore(w http.ResponseWriter, r *http.Request) {
	h.renditionSetRemoved(w, r, false)
}

func (h *handler) renditionSetRemoved(w http.ResponseWriter, r *http.Request, remove bool) {
	fileID, err := strconv.ParseInt(chi.URLParam(r, "fileID"), 10, 64)
	if err != nil || fileID <= 0 {
		http.Error(w, "file id must be a positive integer", http.StatusBadRequest)
		return
	}
	var found bool
	action := "rendition.restore"
	if remove {
		action = "rendition.remove"
		found, err = h.repo.RemoveRendition(r.Context(), fileID)
	} else {
		found, err = h.repo.RestoreRendition(r.Context(), fileID)
	}
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	h.audit(r.Context(), action, strconv.FormatInt(fileID, 10), "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// tagsetRestore handles POST /api/admin/tagsets/{tagsetID}/restore — the
// appearances arm's inverse of Remove (discard): it un-trashes one appearance by
// id. The hash-addressed Trash page can't restore an appearance whose origin
// blob was absorbed/purged; this can. Gated file.delete.
func (h *handler) tagsetRestore(w http.ResponseWriter, r *http.Request) {
	tagsetID, ok := parseTagsetID(r)
	if !ok {
		http.Error(w, "invalid tagset id", http.StatusBadRequest)
		return
	}
	found, err := h.repo.RestoreTagset(r.Context(), tagsetID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	h.audit(r.Context(), "appearance.restore", strconv.FormatInt(tagsetID, 10), "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tagset_id": tagsetID})
}

// tagsetHardDelete handles DELETE /api/admin/tagsets/{tagsetID} — the
// appearances arm's permanent delete: it removes one trashed appearance through
// the shared cascade (last one → recording + files GC'd), reclaiming freed blobs
// after commit. Refuses a live appearance with 409 (trash it first). Gated
// file.delete.
func (h *handler) tagsetHardDelete(w http.ResponseWriter, r *http.Request) {
	tagsetID, ok := parseTagsetID(r)
	if !ok {
		http.Error(w, "invalid tagset id", http.StatusBadRequest)
		return
	}
	out, err := h.repo.HardDeleteTrashedTagset(r.Context(), tagsetID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !out.Found {
		http.NotFound(w, r)
		return
	}
	if !out.Trashed {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "error": "appearance is live — trash it before permanent delete"})
		return
	}
	blobsRemoved := 0
	for _, b := range out.Blobs {
		removed, rerr := h.reclaimStorage(&database.File{StorageBackend: b.StorageBackend}, b.Hash)
		if rerr != nil {
			log.Printf("orphan blob: hash=%s err=%v", b.Hash, rerr)
			continue
		}
		if removed {
			blobsRemoved++
		}
	}
	h.audit(r.Context(), "appearance.delete", strconv.FormatInt(tagsetID, 10),
		fmt.Sprintf("%d file(s) reclaimed", blobsRemoved))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tagset_id": tagsetID, "blobs_removed": blobsRemoved})
}

// parseOptInt parses an optional integer form field: "" (or whitespace) → nil,
// a valid integer → its pointer, anything else → error. The shared track-edit.js
// form sends numeric inputs as strings.
func parseOptInt(s string) (*int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil, err
	}
	return &n, nil
}
