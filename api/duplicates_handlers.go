package api

import (
	"net/http"
	"strconv"
	"strings"

	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
)

// duplicateRenditionDTO is one rendition in the duplicates response: tech info,
// the ladder rank (1 = best), the best mark, and a play URL for the preview.
type duplicateRenditionDTO struct {
	FileID     int64   `json:"file_id"`
	Hash       string  `json:"hash"`
	URL        string  `json:"url"`
	Title      string  `json:"title"`
	Artist     string  `json:"artist"`
	Format     string  `json:"format"` // codec when probed, else MIME (degraded)
	Bitrate    int     `json:"bitrate"`
	SampleRate int     `json:"sample_rate"`
	BitDepth   int     `json:"bit_depth"`
	Size       int64   `json:"size"`
	Duration   float64 `json:"duration"`
	Rank       int     `json:"rank"`
	Best       bool    `json:"best"`
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
	ladder := make([]database.Rendition, len(g.Renditions))
	for i, r := range g.Renditions {
		ladder[i] = database.Rendition{
			FileID:     r.FileID,
			Hash:       r.Hash,
			Codec:      r.Codec,
			Bitrate:    r.Bitrate,
			SampleRate: r.SampleRate,
			BitDepth:   r.BitDepth,
			ByteSize:   r.ByteSize,
		}
	}
	ranked := database.RankRenditions(ladder)
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
			FileID:     r.FileID,
			Hash:       r.Hash,
			URL:        "/files/" + r.ObjectKey,
			Title:      r.Title,
			Artist:     r.Artist,
			Format:     format,
			Bitrate:    r.Bitrate,
			SampleRate: r.SampleRate,
			BitDepth:   r.BitDepth,
			Size:       r.ByteSize,
			Duration:   r.DurationSeconds,
			Rank:       rank,
			Best:       rank == 1,
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

// trackRenditions handles GET /api/tracks/{hash}/renditions — the renditions of
// the recording the given track belongs to, ranked by the quality ladder, for
// the player's Auto/High/Low control (recordings P4). A single-rendition track
// returns a one-element list; an unknown/non-approved hash 404s. Read-only;
// playback of any listed URL is still gated by /files/*.
func (h *handler) trackRenditions(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "hash")))
	if !isSHA256Hex(hash) {
		http.Error(w, "invalid hash", http.StatusBadRequest)
		return
	}
	rends, err := h.repo.RecordingRenditionsByHash(r.Context(), hash)
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

// duplicatesSplit handles POST /api/admin/duplicates/{file_id}/split — detach a
// rendition into its own pinned recording. Gated on content.moderate.
func (h *handler) duplicatesSplit(w http.ResponseWriter, r *http.Request) {
	fileID, err := strconv.ParseInt(chi.URLParam(r, "file_id"), 10, 64)
	if err != nil || fileID <= 0 {
		http.Error(w, "file_id must be a positive integer", http.StatusBadRequest)
		return
	}
	newRec, found, err := h.repo.SplitRendition(r.Context(), fileID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}
	h.audit(r.Context(), "recording.split", strconv.FormatInt(fileID, 10), "new recording "+strconv.FormatInt(newRec, 10))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "recording_id": newRec})
}
