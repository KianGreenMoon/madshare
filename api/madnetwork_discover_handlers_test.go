package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
)

// laneBody is the shape the landing view consumes.
type laneBody struct {
	Lanes []struct {
		Name   string `json:"name"`
		Title  string `json:"title"`
		More   bool   `json:"more"`
		Tracks []struct {
			Title       string `json:"title"`
			GroupArtist string `json:"group_artist"`
			Album       string `json:"album"`
			Holders     int    `json:"holders"`
			SelfHeld    bool   `json:"self_held"`
			SourceName  string `json:"source_name"`
			Versions    []struct {
				Renditions []struct {
					Hash string `json:"hash"`
				} `json:"renditions"`
			} `json:"versions"`
		} `json:"tracks"`
	} `json:"lanes"`
}

// laneRow builds a raw row with a full display identity, since a lane pairs a
// ranked candidate with a merged track by that identity.
func laneRow(sourceID int64, source, recording, artist, album, title string, track int64, hash string) *database.MadnetworkTrackRow {
	row := madRow(sourceID, source, recording, title, hash)
	row.GroupArtist, row.GroupAlbum = artist, album
	row.Entry.Album = album
	row.Entry.Artist = artist
	n := track
	row.Entry.TrackNumber = &n
	return row
}

func laneCand(artist, album, title string, track int64, holders ...string) *database.LaneCandidate {
	return &database.LaneCandidate{
		Ident: fmt.Sprintf("%s|%s|%s", artist, album, title),
		Title: title, Artist: artist, Album: album, Disc: -1, Track: track,
		Holders: len(holders), HolderKeys: holders, SourceName: "a-node",
	}
}

// TestMadnetworkDiscoverRendersLanes: a lane row is a full merged track — the
// same anatomy the drill-down renders, with the drill address and the fact that
// explains why it is in this lane.
func TestMadnetworkDiscoverRendersLanes(t *testing.T) {
	fake := &fakeMadnetwork{
		rows: []*database.MadnetworkTrackRow{
			laneRow(1, "alpha", "r1", "Artist", "Album", "Wanted", 1, "h-wanted"),
			laneRow(2, "beta", "r2", "Artist", "Album", "Wanted", 1, "h-wanted"),
			laneRow(1, "alpha", "r3", "Artist", "Album", "Rarity", 2, "h-rare"),
		},
		lanes: map[string][]*database.LaneCandidate{
			database.LaneMissing: {laneCand("Artist", "Album", "Wanted", 1, "k1", "k2")},
			database.LaneRare:    {laneCand("Artist", "Album", "Rarity", 2, "k1")},
		},
	}
	r := chi.NewRouter()
	RegisterAPI(r, Deps{Madnetwork: fake})
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/madnetwork/discover")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body laneBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}

	// Empty lanes are dropped: three of the five have no candidates.
	if len(body.Lanes) != 2 {
		t.Fatalf("lanes = %d, want the 2 with candidates: %+v", len(body.Lanes), body.Lanes)
	}
	missing := body.Lanes[0]
	if missing.Name != database.LaneMissing || missing.Title != "Not in your library" {
		t.Errorf("first lane = %s/%q", missing.Name, missing.Title)
	}
	if len(missing.Tracks) != 1 {
		t.Fatalf("missing lane tracks = %+v", missing.Tracks)
	}
	got := missing.Tracks[0]
	if got.Title != "Wanted" || got.GroupArtist != "Artist" || got.Album != "Album" {
		t.Errorf("lane row = %+v, want the merged track plus its drill address", got)
	}
	if got.Holders != 2 {
		t.Errorf("holders = %d, want 2 — the fact that explains the row", got.Holders)
	}
	if len(got.Versions) == 0 || len(got.Versions[0].Renditions) == 0 ||
		got.Versions[0].Renditions[0].Hash != "h-wanted" {
		t.Errorf("lane row carries no playable rendition: %+v", got.Versions)
	}
	// Two nodes offering the same hash is ONE version with two holders — the
	// lane must not have re-merged them differently from the drill-down.
	if len(got.Versions) != 1 {
		t.Errorf("versions = %d, want 1 (both nodes offer the same bytes)", len(got.Versions))
	}
	if body.Lanes[1].Tracks[0].Title != "Rarity" {
		t.Errorf("rare lane = %+v", body.Lanes[1].Tracks)
	}
}

// TestMadnetworkLaneSeeAll: the lane's own page is the same ranking with the
// digest's per-source cap lifted, and it reports whether there is more.
func TestMadnetworkLaneSeeAll(t *testing.T) {
	rows := []*database.MadnetworkTrackRow{}
	cands := []*database.LaneCandidate{}
	for i := 0; i < 3; i++ {
		title := fmt.Sprintf("Song %d", i)
		rows = append(rows, laneRow(1, "alpha", fmt.Sprintf("r%d", i), "Artist", "Album", title, int64(i), fmt.Sprintf("h%d", i)))
		cands = append(cands, laneCand("Artist", "Album", title, int64(i), "k1"))
	}
	fake := &fakeMadnetwork{rows: rows,
		lanes: map[string][]*database.LaneCandidate{database.LaneNew: cands}}
	r := chi.NewRouter()
	RegisterAPI(r, Deps{Madnetwork: fake})
	srv := httptest.NewServer(r)
	defer srv.Close()

	get := func(url string) struct {
		Lane struct {
			Name   string `json:"name"`
			More   bool   `json:"more"`
			Tracks []struct {
				Title string `json:"title"`
			} `json:"tracks"`
		} `json:"lane"`
	} {
		t.Helper()
		resp, err := http.Get(srv.URL + url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct {
			Lane struct {
				Name   string `json:"name"`
				More   bool   `json:"more"`
				Tracks []struct {
					Title string `json:"title"`
				} `json:"tracks"`
			} `json:"lane"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	page := get("/api/madnetwork/lane?name=new")
	if page.Lane.Name != database.LaneNew || len(page.Lane.Tracks) != 3 {
		t.Fatalf("see-all page = %+v, want all three rows", page.Lane)
	}
	if page.Lane.More {
		t.Error("see-all reported more rows than it has")
	}
	// Offset past the end is an empty page, not an error.
	if empty := get("/api/madnetwork/lane?name=new&offset=99"); len(empty.Lane.Tracks) != 0 {
		t.Errorf("offset past the end = %+v, want empty", empty.Lane.Tracks)
	}

	// An unknown lane is refused rather than silently answered with nothing.
	resp, err := http.Get(srv.URL + "/api/madnetwork/lane?name=whatever")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown lane status = %d, want 400", resp.StatusCode)
	}
}

// TestMadnetworkSourceFilterReachesTheStore: ?source= is parsed into the view
// the store is asked with, and a nonsense value falls back to the merged view
// rather than erroring — a stale link should land somewhere useful.
func TestMadnetworkSourceFilterReachesTheStore(t *testing.T) {
	fake := &fakeMadnetwork{}
	r := chi.NewRouter()
	RegisterAPI(r, Deps{Madnetwork: fake, MadnetworkName: "this-node"})
	srv := httptest.NewServer(r)
	defer srv.Close()

	// MadnetworkOwnTracks records the view it was called with.
	for _, tc := range []struct {
		query    string
		wantID   int64
		wantSelf bool
	}{
		{"", 0, false},
		{"&source=7", 7, false},
		{"&source=self", 0, true},
		{"&source=garbage", 0, false},
	} {
		resp, err := http.Get(srv.URL + "/api/madnetwork/tracks?artist=A&album=B" + tc.query)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if fake.trackView.SourceID != tc.wantID || fake.trackView.SelfOnly != tc.wantSelf {
			t.Errorf("source%q → view{SourceID:%d, SelfOnly:%v}, want {%d, %v}",
				tc.query, fake.trackView.SourceID, fake.trackView.SelfOnly, tc.wantID, tc.wantSelf)
		}
	}
}
