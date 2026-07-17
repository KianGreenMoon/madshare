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

func testAcoustID(srvURL string) *AcoustID {
	a := NewAcoustID()
	a.Endpoint = srvURL
	return a
}

var acoustidSubject = Subject{
	RawFingerprint: []uint32{2515916061, 2516440381, 2516442428},
	Duration:       237.4,
}

const acoustidBody = `{
	"status": "ok",
	"results": [
		{"score": 0.31, "recordings": [{"title": "Too Weak", "artists": [{"name": "Nobody"}]}]},
		{"score": 0.97, "recordings": [{
			"title": "Some Song",
			"artists": [{"name": "First"}, {"name": "Second"}],
			"releasegroups": [{
				"title": "Great Album", "type": "Album",
				"releases": [
					{"date": {"year": 2004}, "medium_count": 2,
					 "mediums": [{"position": 2, "track_count": 12, "tracks": [{"position": 5, "title": "Some Song"}]}]},
					{"date": {"year": 2004}, "medium_count": 2,
					 "mediums": [{"position": 2, "track_count": 12, "tracks": [{"position": 5, "title": "Some Song"}]}]}
				]
			}]
		}]},
		{"score": 0.81, "recordings": [{
			"title": "Some Song",
			"artists": [{"name": "First"}, {"name": "Second"}],
			"releasegroups": [{
				"title": "Hits Comp", "artists": [{"name": "Various Artists"}],
				"releases": [{"title": "Hits Comp Vol. 1", "date": {"year": 2010}, "medium_count": 1,
					"mediums": [{"position": 1, "track_count": 20, "tracks": [{"position": 3, "title": "Some Song"}]}]}]
			}]
		}]}
	]
}`

func TestAcoustIDSuggest_RequestAndMapping(t *testing.T) {
	var calls int
	var gotForm url.Values
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		r.ParseForm()
		gotForm = r.PostForm
		gotUA = r.Header.Get("User-Agent")
		w.Write([]byte(acoustidBody))
	}))
	defer srv.Close()

	a := testAcoustID(srv.URL)
	got, err := a.Suggest(context.Background(), "test-key", acoustidSubject)
	if err != nil {
		t.Fatalf("Suggest: %v", err)
	}

	if gotForm.Get("client") != "test-key" {
		t.Errorf("client = %q", gotForm.Get("client"))
	}
	if want := media.CompressFingerprint(acoustidSubject.RawFingerprint); gotForm.Get("fingerprint") != want {
		t.Errorf("fingerprint = %q, want %q", gotForm.Get("fingerprint"), want)
	}
	if gotForm.Get("duration") != "237" {
		t.Errorf("duration = %q, want 237", gotForm.Get("duration"))
	}
	if meta := gotForm.Get("meta"); !strings.Contains(meta, "recordings") || !strings.Contains(meta, "releasegroups") {
		t.Errorf("meta = %q", meta)
	}
	if !strings.Contains(gotUA, "Madshare") {
		t.Errorf("User-Agent = %q, want it to identify the app", gotUA)
	}

	// Mapping: below-threshold result dropped, best score first, identical
	// releases deduped → exactly two candidates.
	if len(got) != 2 {
		t.Fatalf("got %d suggestions, want 2: %+v", len(got), got)
	}
	best := got[0]
	if best.Source != SourceMusicBrainz || best.Confidence != 0.97 {
		t.Errorf("best = %+v", best)
	}
	if best.Label != "MusicBrainz — Great Album (2004)" {
		t.Errorf("label = %q", best.Label)
	}
	wantTags := map[string]any{
		"title": "Some Song", "artist": "First, Second", "album": "Great Album",
		"year": 2004, "track_number": 5, "track_total": 12, "disc_number": 2,
	}
	for k, v := range wantTags {
		if best.Tags[k] != v {
			t.Errorf("best.Tags[%s] = %v, want %v", k, best.Tags[k], v)
		}
	}
	if _, ok := best.Tags["album_artist"]; ok {
		t.Errorf("best carries album_artist %v, want none", best.Tags["album_artist"])
	}
	second := got[1]
	if second.Tags["album"] != "Hits Comp Vol. 1" || second.Tags["album_artist"] != "Various Artists" {
		t.Errorf("second = %+v", second.Tags)
	}
	if _, ok := second.Tags["disc_number"]; ok {
		t.Errorf("single-medium release carries disc_number %v", second.Tags["disc_number"])
	}

	// Rapid repeat → served from the cache, no second outbound request.
	if _, err := a.Suggest(context.Background(), "test-key", acoustidSubject); err != nil {
		t.Fatalf("cached Suggest: %v", err)
	}
	if calls != 1 {
		t.Errorf("outbound calls = %d, want 1 (cache)", calls)
	}
}

func TestAcoustIDSuggest_Busy(t *testing.T) {
	a := testAcoustID("http://unreachable.invalid")
	a.lim.next = time.Now().Add(time.Minute) // limiter fully booked
	if _, err := a.Suggest(context.Background(), "k", acoustidSubject); err != ErrBusy {
		t.Errorf("err = %v, want ErrBusy", err)
	}
}

func TestAcoustIDSuggest_ServiceThrottle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	a := testAcoustID(srv.URL)
	if _, err := a.Suggest(context.Background(), "k", acoustidSubject); err != ErrBusy {
		t.Errorf("err = %v, want ErrBusy", err)
	}
}

func TestAcoustIDSuggest_ServiceError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status": "error", "error": {"message": "invalid API key"}}`))
	}))
	defer srv.Close()
	a := testAcoustID(srv.URL)
	_, err := a.Suggest(context.Background(), "bad", acoustidSubject)
	if err == nil || !strings.Contains(err.Error(), "invalid API key") {
		t.Errorf("err = %v, want the service message", err)
	}
}

func TestAcoustIDSuggest_NoFingerprint(t *testing.T) {
	a := testAcoustID("http://unreachable.invalid")
	if _, err := a.Suggest(context.Background(), "k", Subject{}); err == nil {
		t.Error("want error for a subject without fingerprint")
	}
}
