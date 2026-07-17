package api

import (
	"errors"
	"io"
	"log"
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

	// The musicbrainz source exists only when the admin enabled it AND stored
	// an AcoustID key AND a lookup client was wired (settings + Deps.AcoustID).
	var mbKey string
	mbEnabled := false
	if h.acoustid != nil && h.manage != nil {
		p, err := h.manage.GetTagsourcePolicy(r.Context())
		if err != nil {
			http.Error(w, "storage error", http.StatusInternalServerError)
			return
		}
		if p.MusicBrainzEnabled && p.AcoustIDKey != "" {
			mbEnabled = true
			mbKey = p.AcoustIDKey
		}
	}

	q := r.URL.Query()
	charset := q.Get("charset")
	if charset != "" && !media.ValidCharset(charset) {
		http.Error(w, "unknown charset", http.StatusBadRequest)
		return
	}
	// Default = local sources only. The external source runs ONLY when the
	// request names it — user-triggered by design — and is refused outright
	// while disabled, so no fingerprint ever leaves the server unconfigured.
	localSources := tagsource.LocalSources()
	wantMusicBrainz := false
	if raw := strings.TrimSpace(q.Get("sources")); raw != "" {
		localSources = localSources[:0]
		for _, s := range strings.Split(raw, ",") {
			switch {
			case tagsource.ValidLocalSource(s):
				localSources = append(localSources, s)
			case s == tagsource.SourceMusicBrainz:
				if !mbEnabled {
					http.Error(w, "source not enabled", http.StatusBadRequest)
					return
				}
				wantMusicBrainz = true
			default:
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
	// read; respond with an empty list rather than an error. The external
	// lookup still works — it needs only the stored fingerprint.
	suggestions := []tagsource.Suggestion{}
	if path, _, ok := h.blobReg.Resolve(sub.Hash); ok && len(localSources) > 0 {
		suggestions = tagsource.Local(localSources, tagsource.Subject{
			OpenBlob: func() (io.ReadSeekCloser, error) { return os.Open(path) },
			Charset:  charset,
		})
	}
	if wantMusicBrainz {
		mb, err := h.acoustid.Suggest(r.Context(), mbKey, tagsource.Subject{
			RawFingerprint: media.DecodeFingerprint(sub.Fingerprint),
			Duration:       sub.Duration.Float64,
		})
		switch {
		case errors.Is(err, tagsource.ErrBusy):
			// Friendly panel message, not an error chip: the user should retry.
			http.Error(w, "lookup service busy — try again in a moment", http.StatusTooManyRequests)
			return
		case err != nil:
			// Degrade to an error chip; the endpoint itself never fails on a
			// provider error. Log the specifics server-side only.
			log.Printf("musicbrainz suggest (tagset %d): %v", tagsetID, err)
			msg := "lookup failed"
			if len(sub.Fingerprint) == 0 {
				msg = "no acoustic fingerprint (analysis pending or fpcalc unavailable)"
			}
			suggestions = append(suggestions, tagsource.Suggestion{
				Source: tagsource.SourceMusicBrainz, Label: "MusicBrainz", Err: msg,
			})
		default:
			suggestions = append(suggestions, mb...)
		}
	}

	// Enabled-but-not-queried external sources — the UI renders these as
	// on-demand chips that re-request with ?sources=<name>.
	external := []string{}
	if mbEnabled && !wantMusicBrainz {
		external = append(external, tagsource.SourceMusicBrainz)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"tagset_id":        tagsetID,
		"suggestions":      suggestions,
		"external_sources": external,
	})
}
