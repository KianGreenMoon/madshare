package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
)

// duplicateRenditionDTO is one rendition in the duplicates response: tech info,
// the ladder rank (1 = best), the best mark, and a play URL for the preview.
type duplicateRenditionDTO struct {
	FileID      int64   `json:"file_id"`
	Hash        string  `json:"hash"`
	URL         string  `json:"url"`
	Title       string  `json:"title"`
	Artist      string  `json:"artist"`       // raw artist tag (edit prefill)
	AlbumArtist string  `json:"album_artist"` // raw album_artist tag (edit prefill)
	Album       string  `json:"album"`        // raw album tag (edit prefill)
	Format      string  `json:"format"`       // codec when probed, else MIME (degraded)
	Bitrate     int     `json:"bitrate"`
	SampleRate  int     `json:"sample_rate"`
	BitDepth    int     `json:"bit_depth"`
	Size        int64   `json:"size"`
	Duration    float64 `json:"duration"`
	Rank        int     `json:"rank"`
	Best        bool    `json:"best"`
}

type duplicateRecordingDTO struct {
	RecordingID int64                   `json:"recording_id"`
	Suggestion  string                  `json:"suggestion"`
	Renditions  []duplicateRenditionDTO `json:"renditions"`
}

// duplicatesList handles GET /api/admin/duplicates — recordings with more than
// one live rendition, each ranked by the quality ladder with a plain-language
// keep/variant suggestion. Gated on content.moderate. Read-only.
func (h *handler) duplicatesList(w http.ResponseWriter, r *http.Request) {
	groups, err := h.repo.ListDuplicateRecordings(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	out := make([]duplicateRecordingDTO, 0, len(groups))
	for _, g := range groups {
		out = append(out, buildDuplicateDTO(g))
	}
	writeJSON(w, http.StatusOK, out)
}

// buildDuplicateDTO ranks a recording's renditions via the shared quality ladder
// and derives the keep/variant suggestion. The ladder runs on the codec when
// ffprobe probed it; otherwise it falls back to size (the degraded path), and
// the displayed format becomes the MIME type.
func buildDuplicateDTO(g database.DuplicateRecording) duplicateRecordingDTO {
	ranked := database.RankDuplicateRenditions(g.Renditions)
	rankByFile := make(map[int64]int, len(ranked))
	for i, r := range ranked {
		rankByFile[r.FileID] = i + 1 // 1-based, best first
	}

	dto := duplicateRecordingDTO{
		RecordingID: g.RecordingID,
		Renditions:  make([]duplicateRenditionDTO, 0, len(g.Renditions)),
	}
	for _, r := range g.Renditions {
		format := r.Codec
		if format == "" {
			format = r.MimeType
		}
		rank := rankByFile[r.FileID]
		dto.Renditions = append(dto.Renditions, duplicateRenditionDTO{
			FileID:      r.FileID,
			Hash:        r.Hash,
			URL:         "/files/" + r.ObjectKey,
			Title:       r.Title,
			Artist:      r.Artist,
			AlbumArtist: r.AlbumArtist,
			Album:       r.Album,
			Format:      format,
			Bitrate:     r.Bitrate,
			SampleRate:  r.SampleRate,
			BitDepth:    r.BitDepth,
			Size:        r.ByteSize,
			Duration:    r.DurationSeconds,
			Rank:        rank,
			Best:        rank == 1,
		})
	}
	dto.Suggestion = duplicateSuggestion(g.Renditions)
	return dto
}

// duplicateSuggestion returns a plain-language hint for the moderator. It never
// recommends a destructive action automatically — it only describes what the
// tech info implies (and admits when there is no tech info to go on).
func duplicateSuggestion(rs []database.DuplicateRendition) string {
	anyTech := false
	classes := map[int]int{}
	for _, r := range rs {
		if r.Codec != "" || r.Bitrate != 0 || r.SampleRate != 0 {
			anyTech = true
		}
		classes[codecClassOf(r.Codec)]++
	}
	if !anyTech {
		return "No tech details (ffprobe not run) — compare by size and decide manually."
	}
	lossless, lossy := classes[0], classes[1]
	if lossless > 0 && lossy > 0 {
		return "Keep the lossless copy; the others are lower-quality re-encodes."
	}
	return "Keep the top-ranked rendition; review the rest — they may be intentional variants."
}

// codecClassOf mirrors database.codecClass for the suggestion heuristic (0
// lossless, 1 lossy, 2 unknown) without exporting the internal classifier.
func codecClassOf(codec string) int {
	switch codec {
	case "flac", "alac", "FLAC", "ALAC":
		return 0
	case "mp3", "aac", "vorbis", "opus", "wmav2", "ac3", "mp2":
		return 1
	default:
		return 2
	}
}

// trackRenditions handles GET /api/tagsets/{tagsetID}/renditions — the
// surviving renditions of the appearance's recording, ranked by the quality
// ladder, for the player's Auto/High/Low control (recordings P4,
// tagset-addressed since recording-tagsets P1). A single-rendition track
// returns a one-element list; an unknown/unavailable tagset 404s. Read-only;
// playback of any listed URL is still gated by /files/*.
func (h *handler) trackRenditions(w http.ResponseWriter, r *http.Request) {
	tagsetID, perr := strconv.ParseInt(chi.URLParam(r, "tagsetID"), 10, 64)
	if perr != nil || tagsetID <= 0 {
		http.Error(w, "tagset id must be a positive integer", http.StatusBadRequest)
		return
	}
	rends, err := h.repo.RecordingRenditionsByTagsetID(r.Context(), tagsetID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if len(rends) == 0 {
		http.NotFound(w, r)
		return
	}
	dto := buildDuplicateDTO(database.DuplicateRecording{Renditions: rends})
	writeJSON(w, http.StatusOK, dto.Renditions)
}

// duplicatesAbsorb handles POST /api/admin/duplicates/absorb/{recording_id} —
// keep one rendition's blob and absorb the others (recording-tagsets P3): their
// bytes are soft-removed but their distinct appearances are preserved (redundant
// / nameless ones dropped). Body: {keep_file_id, absorb_file_ids}. Gated on
// content.moderate. A stale selection (a non-live rendition of this recording)
// returns 404 so the page reloads.
func (h *handler) duplicatesAbsorb(w http.ResponseWriter, r *http.Request) {
	recordingID, err := strconv.ParseInt(chi.URLParam(r, "recording_id"), 10, 64)
	if err != nil || recordingID <= 0 {
		http.Error(w, "recording_id must be a positive integer", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		KeepFileID    int64   `json:"keep_file_id"`
		AbsorbFileIDs []int64 `json:"absorb_file_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}
	if req.KeepFileID <= 0 || len(req.AbsorbFileIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "keep_file_id and a non-empty absorb_file_ids are required"})
		return
	}

	out, err := h.repo.AbsorbRenditions(r.Context(), recordingID, req.KeepFileID, req.AbsorbFileIDs)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !out.Found {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "recording or a selected rendition is no longer available; reload"})
		return
	}
	h.audit(r.Context(), "recording.absorb", strconv.FormatInt(recordingID, 10),
		fmt.Sprintf("kept %d, absorbed %d rendition(s), dropped %d appearance(s)",
			req.KeepFileID, out.RenditionsRemoved, out.AppearancesDropped))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "renditions_removed": out.RenditionsRemoved, "appearances_dropped": out.AppearancesDropped,
	})
}

// duplicatesAbsorbBulk handles POST /api/admin/duplicates/absorb — absorb each
// listed recording's non-best renditions into its ladder-best ("keep best" over
// a set). Body: {recording_ids:[…]} or {all:true} (every duplicate recording).
// Gated on content.moderate.
func (h *handler) duplicatesAbsorbBulk(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		RecordingIDs []int64 `json:"recording_ids"`
		All          bool    `json:"all"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid JSON body"})
		return
	}

	recIDs := req.RecordingIDs
	if req.All {
		groups, err := h.repo.ListDuplicateRecordings(r.Context())
		if err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		recIDs = make([]int64, 0, len(groups))
		for _, g := range groups {
			recIDs = append(recIDs, g.RecordingID)
		}
	} else if len(recIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": `provide recording_ids or "all": true`})
		return
	}

	recs, rends, err := h.repo.BulkAbsorbKeepBest(r.Context(), recIDs)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	h.audit(r.Context(), "recording.bulk_absorb", "duplicates",
		fmt.Sprintf("%d recording(s), %d rendition(s) absorbed", recs, rends))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "recordings_absorbed": recs, "renditions_removed": rends,
	})
}

// duplicatesSplit handles POST /api/admin/duplicates/{file_id}/split — detach a
// rendition into its own pinned recording. Gated on content.moderate.
func (h *handler) duplicatesSplit(w http.ResponseWriter, r *http.Request) {
	fileID, err := strconv.ParseInt(chi.URLParam(r, "file_id"), 10, 64)
	if err != nil || fileID <= 0 {
		http.Error(w, "file_id must be a positive integer", http.StatusBadRequest)
		return
	}
	out, err := h.repo.SplitRendition(r.Context(), fileID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !out.Found {
		http.NotFound(w, r)
		return
	}
	// Refusal, not a failure: the split would take this recording's last
	// rendition and leave appearances behind that are not read from that blob.
	// They would be trashed by the reaper and could not be restored (the next
	// reap trashes them again), so the moderator is asked to re-home them first.
	if out.StrandedAppearances > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "error": "stranded_appearances",
			"stranded_appearances": out.StrandedAppearances,
			"message": fmt.Sprintf(
				"This is the recording's last rendition, and %d appearance(s) here are not read from it. "+
					"Move them onto another recording (or remove them) before splitting.",
				out.StrandedAppearances),
		})
		return
	}
	h.audit(r.Context(), "recording.split", strconv.FormatInt(fileID, 10), "new recording "+strconv.FormatInt(out.NewRecordingID, 10))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "recording_id": out.NewRecordingID})
}
