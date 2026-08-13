package tagsource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"daemonlord.ygg/madshare/config"
	"daemonlord.ygg/madshare/internal/version"
	"daemonlord.ygg/madshare/media"
)

// SourceMusicBrainz is the external MusicBrainz-via-AcoustID source (P1). It
// is never part of LocalSources: the endpoint runs it only when the request
// names it explicitly — the enforcement point for "user-triggered only".
const SourceMusicBrainz = "musicbrainz"

// ErrBusy is returned when the outbound rate limiter cannot grant a slot
// within its wait budget. The API maps it to 429 so the panel can say
// "service busy — try again", instead of queueing user requests unboundedly
// against a shared per-IP limit.
var ErrBusy = errors.New("tagsource: lookup service busy")

const (
	acoustidEndpoint = "https://api.acoustid.org/v2/lookup"

	// svcMinInterval spaces outbound request starts. AcoustID allows 3 req/s
	// per application and MusicBrainz demands 1 req/s; both limits are per-IP
	// and may be shared with other tools on the host, so every service client
	// stays at the conservative 1 req/s.
	svcMinInterval = time.Second
	// svcMaxWait is how long a caller may be queued before ErrBusy —
	// a serializing limiter, not just a cap, but with a small waiting room.
	svcMaxWait = 3 * time.Second

	svcCacheTTL = 15 * time.Minute
	svcCacheCap = 128

	// acoustidMinScore drops low-confidence matches; svcMaxCandidates caps
	// each suggestion list (one recording can appear on dozens of releases).
	acoustidMinScore = 0.5
	svcMaxCandidates = 8
)

// AcoustID looks up the subject's Chromaprint fingerprint against the
// AcoustID web service and maps the matched MusicBrainz recordings/releases
// to candidate tagsets. One instance is shared process-wide (madshare.go) so
// the rate limiter and cache are global; the zero value is not usable — use
// NewAcoustID.
type AcoustID struct {
	Endpoint  string
	Client    *http.Client
	UserAgent string

	lim   svcLimiter
	cache svcCache
}

// serviceUserAgent is the identifying User-Agent MusicBrainz/AcoustID require.
func serviceUserAgent() string {
	v := version.Get()
	ver := v.Version
	if ver == "" {
		ver = "dev"
	}
	return fmt.Sprintf("%s/%s (%s)", v.Name, ver, config.DefaultGitRepoURL)
}

// NewAcoustID returns a client with the production endpoint, a sane timeout,
// and the standard limiter/cache plumbing.
func NewAcoustID() *AcoustID {
	return &AcoustID{
		Endpoint:  acoustidEndpoint,
		Client:    &http.Client{Timeout: 10 * time.Second},
		UserAgent: serviceUserAgent(),
		lim:       svcLimiter{interval: svcMinInterval, maxWait: svcMaxWait},
		cache:     svcCache{ttl: svcCacheTTL, cap: svcCacheCap},
	}
}

// Suggest looks up the subject's fingerprint. Rapid repeats served from the
// TTL cache never leave the process; a cache miss goes through the serializing
// limiter and may return ErrBusy. Other errors mean "this lookup failed" —
// the caller degrades them to an error chip, never a failed endpoint.
func (a *AcoustID) Suggest(ctx context.Context, apiKey string, sub Subject) ([]Suggestion, error) {
	if len(sub.RawFingerprint) == 0 {
		return nil, errors.New("no acoustic fingerprint")
	}
	fp := media.CompressFingerprint(sub.RawFingerprint)
	dur := int(sub.Duration + 0.5)
	key := fp + ":" + strconv.Itoa(dur)

	if s, ok := a.cache.get(key); ok {
		return s, nil
	}
	if err := a.lim.acquire(ctx); err != nil {
		return nil, err
	}

	form := url.Values{
		"client":      {apiKey},
		"duration":    {strconv.Itoa(dur)},
		"fingerprint": {fp},
		// Space-separated meta groups; releases nest under releasegroups and
		// "compress" dedupes them. Everything a candidate tagset needs comes
		// back in this one call — no second MusicBrainz round-trip.
		"meta": {"recordings releasegroups releases tracks compress"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Endpoint,
		strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", a.UserAgent)

	resp, err := a.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
		return nil, ErrBusy
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var ar acoustidResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("acoustid: bad response: %w", err)
	}
	if ar.Status != "ok" {
		msg := ar.Error.Message
		if msg == "" {
			msg = "HTTP " + strconv.Itoa(resp.StatusCode)
		}
		return nil, fmt.Errorf("acoustid: %s", msg)
	}

	out := mapAcoustID(ar.Results)
	a.cache.put(key, out)
	return out, nil
}

// AcoustID lookup response (the fields we read). Releases nest under
// releasegroups because the request asks for both.
type acoustidResponse struct {
	Status string `json:"status"`
	Error  struct {
		Message string `json:"message"`
	} `json:"error"`
	Results []acoustidResult `json:"results"`
}

type acoustidResult struct {
	Score      float64             `json:"score"`
	Recordings []acoustidRecording `json:"recordings"`
}

type acoustidRecording struct {
	Title         string                 `json:"title"`
	Artists       []acoustidArtist       `json:"artists"`
	ReleaseGroups []acoustidReleaseGroup `json:"releasegroups"`
}

type acoustidArtist struct {
	Name string `json:"name"`
}

type acoustidReleaseGroup struct {
	Title    string            `json:"title"`
	Type     string            `json:"type"`
	Artists  []acoustidArtist  `json:"artists"`
	Releases []acoustidRelease `json:"releases"`
}

type acoustidRelease struct {
	Title string `json:"title"` // omitted by "compress" when equal to the group title
	Date  struct {
		Year int `json:"year"`
	} `json:"date"`
	MediumCount int              `json:"medium_count"`
	Mediums     []acoustidMedium `json:"mediums"`
}

type acoustidMedium struct {
	Position   int             `json:"position"`
	TrackCount int             `json:"track_count"`
	Tracks     []acoustidTrack `json:"tracks"`
}

type acoustidTrack struct {
	Position int    `json:"position"`
	Title    string `json:"title"`
}

// mapAcoustID flattens results into deduped candidate tagsets, best score
// first, capped at acoustidMaxCandidates.
func mapAcoustID(results []acoustidResult) []Suggestion {
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	out := []Suggestion{}
	seen := map[string]bool{}
	for _, res := range results {
		if res.Score < acoustidMinScore {
			continue
		}
		for _, rec := range res.Recordings {
			artist := joinArtists(rec.Artists)
			groups := rec.ReleaseGroups
			if len(groups) == 0 {
				groups = []acoustidReleaseGroup{{}} // recording-only match: title/artist still useful
			}
			for _, rg := range groups {
				albumArtist := joinArtists(rg.Artists)
				releases := rg.Releases
				if len(releases) == 0 {
					releases = []acoustidRelease{{}}
				}
				for _, rel := range releases {
					album := rel.Title
					if album == "" {
						album = rg.Title
					}
					title := rec.Title
					var trackNo, trackTotal, discNo int
					if len(rel.Mediums) > 0 {
						m := rel.Mediums[0]
						trackTotal = m.TrackCount
						if rel.MediumCount > 1 {
							discNo = m.Position
						}
						if len(m.Tracks) > 0 {
							trackNo = m.Tracks[0].Position
							if title == "" {
								title = m.Tracks[0].Title
							}
						}
					}
					key := strings.ToLower(strings.Join([]string{
						title, artist, album, strconv.Itoa(rel.Date.Year), strconv.Itoa(trackNo),
					}, "\x00"))
					if title == "" || seen[key] {
						continue
					}
					seen[key] = true
					label := "MusicBrainz"
					if album != "" {
						label += " — " + album
						if rel.Date.Year > 0 {
							label += " (" + strconv.Itoa(rel.Date.Year) + ")"
						}
					}
					out = append(out, Suggestion{
						Source:     SourceMusicBrainz,
						Label:      label,
						Confidence: res.Score,
						Tags: tagsMap(map[string]any{
							"title":        title,
							"artist":       artist,
							"album":        album,
							"album_artist": albumArtist,
							"year":         rel.Date.Year,
							"track_number": trackNo,
							"track_total":  trackTotal,
							"disc_number":  discNo,
						}),
					})
					if len(out) >= svcMaxCandidates {
						return out
					}
				}
			}
		}
	}
	return out
}

func joinArtists(as []acoustidArtist) string {
	names := make([]string, 0, len(as))
	for _, a := range as {
		if a.Name != "" {
			names = append(names, a.Name)
		}
	}
	return strings.Join(names, ", ")
}
