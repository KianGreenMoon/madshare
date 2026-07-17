package api

import (
	"io"
	"net/http"
	"os"
	"strings"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/media"
	"daemonlord.ygg/madshare/tagsource"
)

// GET /api/tagsets/{tagsetID}/suggestions — read-only candidate tagsets for
// the edit modal's "Suggest tags" panel (docs/architecture/tag-suggestions.md).
//
//	?sources=id3v2,id3v1  which sources to run; default = all local sources.
//	                      External sources (P1) run ONLY when explicitly named
//	                      here — the enforcement point for "user-triggered only".
//	?charset=windows-1251 charset override for the local sources' re-decode.
//
// Authorization mirrors "may edit this tagset": metadata.edit holders for any
// appearance, otherwise the owner of an editable (draft/returned) one — 404 on
// everything else, so other users' staged files are never revealed.
func (h *handler) tagsetSuggestions(w http.ResponseWriter, r *http.Request) {
	tagsetID, ok := parseTagsetID(r)
	if !ok {
		http.Error(w, "invalid tagset id", http.StatusBadRequest)
		return
	}
	if h.authzEnabled {
		id := auth.FromContext(r.Context())
		if id == nil {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		state, owner, deleted, found, err := h.repo.TagsetReviewInfo(r.Context(), tagsetID)
		if err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		if !found {
			http.NotFound(w, r)
			return
		}
		if !id.Has(auth.PermMetadataEdit) {
			editable := state == database.ReviewDraft || state == database.ReviewReturned
			owned := owner.Valid && owner.Int64 == id.UserID
			if deleted || !editable || !owned {
				http.NotFound(w, r)
				return
			}
		}
	}

	q := r.URL.Query()
	charset := q.Get("charset")
	if charset != "" && !media.ValidCharset(charset) {
		http.Error(w, "unknown charset", http.StatusBadRequest)
		return
	}
	sources := tagsource.LocalSources()
	if raw := strings.TrimSpace(q.Get("sources")); raw != "" {
		sources = strings.Split(raw, ",")
		for _, s := range sources {
			if !tagsource.ValidLocalSource(s) {
				http.Error(w, "unknown source", http.StatusBadRequest)
				return
			}
		}
	}

	sub, found, err := h.repo.TagsetSuggestSubject(r.Context(), tagsetID)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if !found {
		http.NotFound(w, r)
		return
	}

	// Blob gone (reaped / dangling link) → the local sources have nothing to
	// read; respond with an empty list rather than an error.
	suggestions := []tagsource.Suggestion{}
	if path, _, ok := h.blobReg.Resolve(sub.Hash); ok {
		suggestions = tagsource.Local(sources, tagsource.Subject{
			OpenBlob: func() (io.ReadSeekCloser, error) { return os.Open(path) },
			Charset:  charset,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"tagset_id":   tagsetID,
		"suggestions": suggestions,
		// Enabled-but-not-yet-queried external sources (P1: musicbrainz when the
		// admin has configured it). The UI renders these as on-demand chips.
		"external_sources": []string{},
	})
}
