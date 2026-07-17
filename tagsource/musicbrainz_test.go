package tagsource

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"daemonlord.ygg/madshare/media"
)

func testMusicBrainz(srvURL string) *MusicBrainz {
	m := NewMusicBrainz()
	m.Endpoint = srvURL
	return m
}

const mbSearchBody = `{
	"count": 3, "offset": 0,
	"recordings": [
		{"score": 100, "title": "Come as You Are", "length": 218000,
		 "artist-credit": [{"name": "Nirvana", "joinphrase": ""}],
		 "releases": [
			{"title": "Bootleg Comp", "status": "Bootleg", "date": "1999",
			 "media": [{"position": 1, "track-count": 30, "track": [{"number": "12"}]}]},
			{"title": "Nevermind", "status": "Official", "date": "1991-09-24",
			 "media": [{"position": 1, "track-count": 12, "track": [{"number": "3"}]}]}
		 ]},
		{"score": 87, "title": "Come as You Are", "length": 219000,
		 "artist-credit": [{"name": "First", "joinphrase": " feat. "}, {"name": "Second", "joinphrase": ""}],
		 "releases": [
			{"title": "Covers Vol. 2", "status": "Official", "date": "2010-03",
			 "artist-credit": [{"name": "Various Artists", "joinphrase": ""}],
			 "media": [{"position": 2, "track-count": 18, "track": [{"number": "A4"}]}]}
		 ]},
		{"score": 55, "title": "Come as You Are (live)",
		 "artist-credit": [{"name": "Nirvana", "joinphrase": ""}],
		 "releases": []}
	]
}`

func TestMusicBrainzSearch_RequestAndMapping(t *testing.T) {
	var calls int
	var gotQuery url.Values
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotQuery = r.URL.Query()
		gotUA = r.Header.Get("User-Agent")
		w.Write([]byte(mbSearchBody))
	}))
	defer srv.Close()

	m := testMusicBrainz(srv.URL)
	got, err := m.Search(context.Background(), `recording:"Come as You Are" AND artist:"Nirvana"`)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if q := gotQuery.Get("query"); !strings.Contains(q, "Come as You Are") {
		t.Errorf("query = %q", q)
	}
	if gotQuery.Get("fmt") != "json" || gotQuery.Get("limit") == "" {
		t.Errorf("params = %v", gotQuery)
	}
	if !strings.Contains(gotUA, "Madshare") {
		t.Errorf("User-Agent = %q", gotUA)
	}

	if len(got) != 3 {
		t.Fatalf("got %d suggestions, want 3: %+v", len(got), got)
	}
	// Best match: Official release preferred over the bootleg listed first.
	best := got[0]
	if best.Source != SourceMusicBrainz || best.Confidence != 1 {
		t.Errorf("best = %+v", best)
	}
	if best.Label != "MusicBrainz — Nevermind (1991)" {
		t.Errorf("label = %q", best.Label)
	}
	for k, v := range map[string]any{
		"title": "Come as You Are", "artist": "Nirvana", "album": "Nevermind",
		"year": 1991, "track_number": 3, "track_total": 12,
	} {
		if best.Tags[k] != v {
			t.Errorf("best.Tags[%s] = %v, want %v", k, best.Tags[k], v)
		}
	}
	if _, ok := best.Tags["disc_number"]; ok {
		t.Errorf("medium 1 carries disc_number %v", best.Tags["disc_number"])
	}

	// Second match: join phrase, release artist-credit → album_artist, medium
	// position 2 → disc 2, vinyl track number "A4" → no opinion.
	second := got[1]
	if second.Confidence != 0.87 || second.Tags["artist"] != "First feat. Second" ||
		second.Tags["album_artist"] != "Various Artists" || second.Tags["disc_number"] != 2 {
		t.Errorf("second = %+v", second.Tags)
	}
	if _, ok := second.Tags["track_number"]; ok {
		t.Errorf("vinyl number mapped to track_number %v", second.Tags["track_number"])
	}

	// Third match: no releases at all — title/artist still useful.
	if got[2].Tags["title"] != "Come as You Are (live)" || got[2].Label != "MusicBrainz" {
		t.Errorf("third = %+v", got[2])
	}

	// Rapid repeat of the same query → cache, no second outbound request.
	if _, err := m.Search(context.Background(), `recording:"Come as You Are" AND artist:"Nirvana"`); err != nil {
		t.Fatalf("cached Search: %v", err)
	}
	if calls != 1 {
		t.Errorf("outbound calls = %d, want 1 (cache)", calls)
	}
}

func TestMusicBrainzSearch_BusyAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable) // MB rate-limit signal
	}))
	defer srv.Close()
	if _, err := testMusicBrainz(srv.URL).Search(context.Background(), "x"); err != ErrBusy {
		t.Errorf("503 err = %v, want ErrBusy", err)
	}

	m := testMusicBrainz("http://unreachable.invalid")
	m.lim.next = time.Now().Add(time.Minute)
	if _, err := m.Search(context.Background(), "x"); err != ErrBusy {
		t.Errorf("booked limiter err = %v, want ErrBusy", err)
	}

	if _, err := testMusicBrainz("http://unreachable.invalid").Search(context.Background(), "  "); err == nil {
		t.Error("want error for empty query")
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error": "malformed lucene"}`))
	}))
	defer bad.Close()
	if _, err := testMusicBrainz(bad.URL).Search(context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "malformed lucene") {
		t.Errorf("bad query err = %v, want the service message", err)
	}
}

func TestSeedQuery(t *testing.T) {
	sub := Subject{Current: media.Tags{Title: `Some "Song"`, Artist: "The Band"}, Duration: 237}
	got := SeedQuery(sub)
	want := `recording:"Some \"Song\"" AND artist:"The Band" AND dur:[227000 TO 247000]`
	if got != want {
		t.Errorf("SeedQuery = %q, want %q", got, want)
	}
	if got := SeedQuery(Subject{Current: media.Tags{Title: "Only Title"}}); got != `recording:"Only Title"` {
		t.Errorf("title-only = %q", got)
	}
	if got := SeedQuery(Subject{Duration: 100}); got != "" {
		t.Errorf("no terms = %q, want empty", got)
	}
}
