package api

// Federation F8 item 2 — the fingerprint-vs-tagset mismatch warning
// (docs/architecture/federation.md §Quality upgrades, "The mismatch warning").
//
// A submission's tags are a claim about audio nobody has listened to yet. Two
// oracles can contradict that claim, and they fail in opposite directions, which
// is why both are here rather than either alone:
//
//   - the madnetwork's dominant label — free, needs no key, and blind to audio
//     the network has never seen;
//   - AcoustID → MusicBrainz — an oracle outside the social graph entirely, and
//     silent unless an admin configured a key.
//
// Neither acts. Nothing is auto-flagged into a stored state, no submission is
// blocked and nothing is scored: the no-automatic-reputation rule covers this
// surface too (§Trust graph). The endpoint hands a moderator two sentences and
// the preview player.

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strings"
	"unicode"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/media"
	"daemonlord.ygg/madshare/tagsource"
)

// identityVerdict is one oracle's answer. Available separates "this oracle has
// nothing configured / nothing to say" from "it answered and disagrees" — an
// unavailable oracle must never read as agreement.
type identityVerdict struct {
	Available bool   `json:"available"`
	Agrees    bool   `json:"agrees"`
	Title     string `json:"title,omitempty"`
	Artist    string `json:"artist,omitempty"`
	// Voices is the network oracle's independent-branch count behind the label
	// it reports; Confidence the external oracle's own score.
	Voices     int     `json:"voices,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`
	// Note explains a false Available in the reader's terms (no key configured,
	// no fingerprint yet, lookup busy). Never an excuse for a silent pass.
	Note string `json:"note,omitempty"`
}

// moderationIdentity handles GET /api/admin/moderation/{tagsetID}/identity —
// what the two oracles say this audio is, next to what the tags claim.
//
// Fired when a moderator expands one card: one row, one external lookup. That
// cadence is the whole reason the external oracle is affordable here — the
// AcoustID client serializes at one request a second process-wide, so a page of
// rows checked eagerly would queue behind itself, while a queue nobody opens
// costs nothing.
//
// Deliberately NOT folded into the classify response: classify is on the card's
// critical path and must not wait out a call to another continent.
func (h *handler) moderationIdentity(w http.ResponseWriter, r *http.Request) {
	tagsetID, ok := parseTagsetID(r)
	if !ok {
		http.Error(w, "invalid tagset id", http.StatusBadRequest)
		return
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
	claimed := media.Tags{Title: sub.Title, Artist: sub.Artist.String}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"tagset_id": tagsetID,
		"claimed":   map[string]any{"title": claimed.Title, "artist": claimed.Artist},
		"network":   h.networkIdentity(r.Context(), tagsetID, claimed),
		"external":  h.externalIdentity(r.Context(), sub, claimed),
	})
}

// networkIdentity asks the madnetwork what it calls this audio: the dominant
// label from the F8 join, which is already branch-weighted, so a farm of keys
// cannot manufacture the consensus a moderator is being shown.
func (h *handler) networkIdentity(ctx context.Context, tagsetID int64, claimed media.Tags) identityVerdict {
	if h.madnetwork == nil {
		return identityVerdict{Note: "federation is off on this node"}
	}
	sc, ok, err := h.repo.ClassifySubmission(ctx, tagsetID)
	if err != nil || !ok {
		return identityVerdict{Note: "could not resolve the recording"}
	}
	take := h.networkTakeOn(ctx, sc.RecordingID, sc.CurrentBest)
	if take == nil || len(take.Tagsets) == 0 {
		return identityVerdict{Note: "no node in your madnetwork holds this audio"}
	}
	dominant := take.Tagsets[0]
	return identityVerdict{
		Available: true,
		Agrees:    textAgrees(claimed.Title, dominant.Title),
		Title:     dominant.Title,
		Artist:    dominant.Artist,
		Voices:    dominant.Voices,
	}
}

// externalIdentity runs the AcoustID lookup — the oracle outside the social
// graph. Every unavailable path says why: a warning that silently fails to
// appear is worse than one that says it could not be asked.
func (h *handler) externalIdentity(ctx context.Context, sub *database.SuggestSubject, claimed media.Tags) identityVerdict {
	if h.acoustid == nil || h.manage == nil {
		return identityVerdict{Note: "no lookup service configured"}
	}
	p, err := h.manage.GetTagsourcePolicy(ctx)
	if err != nil {
		return identityVerdict{Note: "could not read the tag-service settings"}
	}
	if !p.MusicBrainzEnabled || p.AcoustIDKey == "" {
		return identityVerdict{Note: "MusicBrainz lookup is not enabled (needs an AcoustID key)"}
	}
	if len(sub.Fingerprint) == 0 {
		return identityVerdict{Note: "no acoustic fingerprint yet (analysis pending, or fpcalc unavailable)"}
	}
	found, err := h.acoustid.Suggest(ctx, p.AcoustIDKey, tagsource.Subject{
		RawFingerprint: media.DecodeFingerprint(sub.Fingerprint),
		Duration:       sub.Duration.Float64,
	})
	switch {
	case errors.Is(err, tagsource.ErrBusy):
		return identityVerdict{Note: "lookup service busy — expand this card again in a moment"}
	case err != nil:
		// Specifics are for the log; the moderator gets a sentence. An oracle
		// that failed is an oracle that did not agree with anything.
		log.Printf("acoustid identity check (blob %s): %v", sub.Hash, err)
		return identityVerdict{Note: "lookup failed"}
	}
	best, ok := bestSuggestion(found)
	if !ok {
		return identityVerdict{Note: "AcoustID recognises no recording for this audio"}
	}
	title, _ := best.Tags["title"].(string)
	artist, _ := best.Tags["artist"].(string)
	return identityVerdict{
		Available:  true,
		Agrees:     textAgrees(claimed.Title, title),
		Title:      title,
		Artist:     artist,
		Confidence: best.Confidence,
	}
}

// bestSuggestion picks the highest-confidence candidate that actually names a
// title. An error entry (the panel's disabled-chip shape) is not a candidate.
func bestSuggestion(found []tagsource.Suggestion) (tagsource.Suggestion, bool) {
	var best tagsource.Suggestion
	ok := false
	for _, s := range found {
		if s.Err != "" {
			continue
		}
		if title, _ := s.Tags["title"].(string); strings.TrimSpace(title) == "" {
			continue
		}
		if !ok || s.Confidence > best.Confidence {
			best, ok = s, true
		}
	}
	return best, ok
}

// textAgrees is the one comparison rule both oracles are judged by, and it is
// deliberately loose in exactly one direction.
//
// It compares TITLES, because that is the axis a mislabel actually turns on —
// the classic attack tags one song with another's name, and artist strings
// disagree far too readily on their own ("feat.", "The", credited vs. album
// artist) to carry a warning by themselves.
//
// Agreement is one title's words being a PREFIX of the other's. Suffixes are
// where catalogue noise lives — "(Remastered 2011)", "- Live at Wembley",
// "[Radio Edit]" — so a difference there must not cry wolf, or the warning gets
// trained away long before a real mislabel arrives. A difference at the FRONT is
// a different title. Prefix rather than substring on purpose: containment would
// make "Time" agree with "Timeless".
//
// An empty claim agrees with nothing: there is no claim to contradict.
func textAgrees(claimed, other string) bool {
	a, b := titleWords(claimed), titleWords(other)
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	for i, w := range a {
		if b[i] != w {
			return false
		}
	}
	return true
}

// titleWords folds a title to lowercase words. Apostrophes are DELETED rather
// than split on, because they sit inside words — "Don't" and "Dont" are the same
// title typed twice, and splitting would make them disagree. Every other
// non-alphanumeric run is a word separator, so punctuation and bracket styles
// cannot make two identical titles look different.
func titleWords(s string) []string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '\'' || r == '’': // straight and typographic apostrophes
		default:
			b.WriteByte(' ')
		}
	}
	return strings.Fields(b.String())
}
