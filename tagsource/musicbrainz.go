package tagsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MusicBrainz text search — the fallback when there is no acoustic
// fingerprint (fpcalc absent, analysis pending) or the AcoustID lookup found
// nothing (tag-suggestions.md P2). One GET to the recording search endpoint
// (Lucene query syntax); needs no API key, only the identifying User-Agent
// and the same 1 req/s pacing as the fingerprint path.

const musicbrainzEndpoint = "https://musicbrainz.org/ws/2/recording"

// MusicBrainz is the recording text-search client. Like AcoustID, one
// instance is shared process-wide (madshare.go) so the limiter and cache are
// global; use NewMusicBrainz.
type MusicBrainz struct {
	Endpoint  string
	Client    *http.Client
	UserAgent string

	lim   svcLimiter
	cache svcCache
}

// NewMusicBrainz returns a client with the production endpoint and the
// standard limiter/cache plumbing.
func NewMusicBrainz() *MusicBrainz {
	return &MusicBrainz{
		Endpoint:  musicbrainzEndpoint,
		Client:    &http.Client{Timeout: 10 * time.Second},
		UserAgent: serviceUserAgent(),
		lim:       svcLimiter{interval: svcMinInterval, maxWait: svcMaxWait},
		cache:     svcCache{ttl: svcCacheTTL, cap: svcCacheCap},
	}
}

// SeedQuery builds the Lucene query for a subject whose user typed nothing:
// current title/artist as quoted phrase terms, duration-windowed (±10 s, MB
// measures in milliseconds) when a duration is known. Empty when the subject
// carries nothing to search for.
func SeedQuery(sub Subject) string {
	var terms []string
	if t := strings.TrimSpace(sub.Current.Title); t != "" {
		terms = append(terms, `recording:`+lucenePhrase(t))
	}
	if a := strings.TrimSpace(sub.Current.Artist); a != "" {
		terms = append(terms, `artist:`+lucenePhrase(a))
	}
	if len(terms) == 0 {
		return ""
	}
	if d := sub.Duration; d > 0 {
		lo := int((d - 10) * 1000)
		if lo < 0 {
			lo = 0
		}
		hi := int((d + 10) * 1000)
		terms = append(terms, fmt.Sprintf("dur:[%d TO %d]", lo, hi))
	}
	return strings.Join(terms, " AND ")
}

// lucenePhrase quotes s as a Lucene phrase term.
func lucenePhrase(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// Search runs the recording query (verbatim Lucene — a user-typed plain-words
// query works too, the default search field is the recording) and maps the
// top matches to candidate tagsets, one per recording, score order preserved.
func (m *MusicBrainz) Search(ctx context.Context, query string) ([]Suggestion, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("empty search query")
	}
	if s, ok := m.cache.get(query); ok {
		return s, nil
	}
	if err := m.lim.acquire(ctx); err != nil {
		return nil, err
	}

	q := url.Values{
		"query": {query},
		"fmt":   {"json"},
		"limit": {strconv.Itoa(svcMaxCandidates)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.Endpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", m.UserAgent)

	resp, err := m.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		return nil, ErrBusy // MB signals rate-limit violations with 503
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		// Search errors (e.g. malformed Lucene) come back as JSON {"error": …}.
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &e) == nil && e.Error != "" {
			return nil, fmt.Errorf("musicbrainz: %s", e.Error)
		}
		return nil, fmt.Errorf("musicbrainz: HTTP %d", resp.StatusCode)
	}
	var mr mbSearchResponse
	if err := json.Unmarshal(body, &mr); err != nil {
		return nil, fmt.Errorf("musicbrainz: bad response: %w", err)
	}

	out := mapMusicBrainz(mr.Recordings)
	m.cache.put(query, out)
	return out, nil
}

// MusicBrainz recording-search response (the fields we read).
type mbSearchResponse struct {
	Recordings []mbRecording `json:"recordings"`
}

type mbRecording struct {
	Score        int              `json:"score"` // 0–100, best-first
	Title        string           `json:"title"`
	ArtistCredit []mbArtistCredit `json:"artist-credit"`
	Releases     []mbRelease      `json:"releases"`
}

type mbArtistCredit struct {
	Name       string `json:"name"`
	JoinPhrase string `json:"joinphrase"`
}

type mbRelease struct {
	Title        string           `json:"title"`
	Status       string           `json:"status"`
	Date         string           `json:"date"` // "YYYY", "YYYY-MM" or "YYYY-MM-DD"
	ArtistCredit []mbArtistCredit `json:"artist-credit"`
	Media        []mbMedium       `json:"media"`
}

type mbMedium struct {
	Position   int       `json:"position"`
	TrackCount int       `json:"track-count"`
	Tracks     []mbTrack `json:"track"` // only the matched track
}

type mbTrack struct {
	Number string `json:"number"` // "5", but also "A4" on vinyl
}

// mapMusicBrainz emits one candidate per recording (its first Official
// release, else its first release), deduped, order preserved (the API sorts
// best-first). Confidence is the normalized search score.
func mapMusicBrainz(recs []mbRecording) []Suggestion {
	out := []Suggestion{}
	seen := map[string]bool{}
	for _, rec := range recs {
		if rec.Title == "" {
			continue
		}
		artist := joinCredit(rec.ArtistCredit)
		var rel *mbRelease
		for i := range rec.Releases {
			if rec.Releases[i].Status == "Official" {
				rel = &rec.Releases[i]
				break
			}
		}
		if rel == nil && len(rec.Releases) > 0 {
			rel = &rec.Releases[0]
		}
		var album, albumArtist string
		var year, trackNo, trackTotal, discNo int
		if rel != nil {
			album = rel.Title
			if aa := joinCredit(rel.ArtistCredit); aa != artist {
				albumArtist = aa
			}
			if len(rel.Date) >= 4 {
				year, _ = strconv.Atoi(rel.Date[:4])
			}
			if len(rel.Media) > 0 {
				md := rel.Media[0]
				trackTotal = md.TrackCount
				if md.Position > 1 {
					discNo = md.Position
				}
				if len(md.Tracks) > 0 {
					trackNo, _ = strconv.Atoi(md.Tracks[0].Number) // non-numeric ("A4") → 0 = no opinion
				}
			}
		}
		key := strings.ToLower(strings.Join([]string{
			rec.Title, artist, album, strconv.Itoa(year), strconv.Itoa(trackNo),
		}, "\x00"))
		if seen[key] {
			continue
		}
		seen[key] = true
		label := "MusicBrainz"
		if album != "" {
			label += " — " + album
			if year > 0 {
				label += " (" + strconv.Itoa(year) + ")"
			}
		}
		out = append(out, Suggestion{
			Source:     SourceMusicBrainz,
			Label:      label,
			Confidence: float64(rec.Score) / 100,
			Tags: tagsMap(map[string]any{
				"title":        rec.Title,
				"artist":       artist,
				"album":        album,
				"album_artist": albumArtist,
				"year":         year,
				"track_number": trackNo,
				"track_total":  trackTotal,
				"disc_number":  discNo,
			}),
		})
		if len(out) >= svcMaxCandidates {
			break
		}
	}
	return out
}

// joinCredit renders an artist credit with its join phrases
// ("First feat. Second").
func joinCredit(credit []mbArtistCredit) string {
	var b strings.Builder
	for _, c := range credit {
		b.WriteString(c.Name)
		b.WriteString(c.JoinPhrase)
	}
	return strings.TrimSpace(b.String())
}
