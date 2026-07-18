package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

type fakeMadnetwork struct {
	rows []*database.MadnetworkTrackRow
}

func (f *fakeMadnetwork) MadnetworkArtists(context.Context, string) ([]*database.MadnetworkArtist, error) {
	return []*database.MadnetworkArtist{{Name: "A", Albums: 1, Tracks: 2}}, nil
}
func (f *fakeMadnetwork) MadnetworkAlbums(context.Context, string) ([]*database.MadnetworkAlbum, error) {
	return nil, nil
}
func (f *fakeMadnetwork) MadnetworkTracks(context.Context, string, string) ([]*database.MadnetworkTrackRow, error) {
	return f.rows, nil
}
func (f *fakeMadnetwork) MadnetworkSummary(context.Context) ([]*database.MadnetworkFriend, int64, error) {
	return nil, 0, nil
}

func madRow(peerID int64, peer, recording, title string, hashes ...string) *database.MadnetworkTrackRow {
	e := federation.CatalogEntry{Key: recording + "-key", RecordingKey: recording, Title: title}
	for _, h := range hashes {
		e.Renditions = append(e.Renditions, federation.CatalogRendition{Hash: h, Size: 1})
	}
	return &database.MadnetworkTrackRow{PeerID: peerID, PeerName: peer, Entry: e}
}

func TestMadnetworkTracks_VersionMerging(t *testing.T) {
	// Same title from three peers: peers 1+2 share a rendition hash (one
	// version, two holders); peer 3 claims a different recording with disjoint
	// hashes (a second version). Plus a second title to check row grouping.
	fake := &fakeMadnetwork{rows: []*database.MadnetworkTrackRow{
		madRow(1, "alpha", "r1", "Crossing", "h-shared", "h-extra"),
		madRow(2, "beta", "r2", "crossing", "h-shared"),
		madRow(3, "gamma", "r3", "Crossing", "h-other"),
		madRow(1, "alpha", "r4", "Solo", "h-solo"),
	}}
	r := chi.NewRouter()
	RegisterAPI(r, Deps{Madnetwork: fake})
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/madnetwork/tracks?artist=A&album=B")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Tracks []struct {
			Title    string `json:"title"`
			Versions []struct {
				Renditions []federation.CatalogRendition `json:"renditions"`
				Holders    []struct {
					Name string `json:"name"`
				} `json:"holders"`
			} `json:"versions"`
		} `json:"tracks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tracks) != 2 {
		t.Fatalf("tracks = %d, want 2 (Crossing + Solo)", len(body.Tracks))
	}
	crossing := body.Tracks[0]
	if crossing.Title != "Crossing" || len(crossing.Versions) != 2 {
		t.Fatalf("crossing = %+v, want 2 versions", crossing)
	}
	// Most-held version first: the shared-hash union of peers 1+2.
	v0 := crossing.Versions[0]
	if len(v0.Holders) != 2 || len(v0.Renditions) != 2 {
		t.Errorf("version 1 = %d holders / %d renditions, want 2/2 (hash union, deduped)", len(v0.Holders), len(v0.Renditions))
	}
	if len(crossing.Versions[1].Holders) != 1 {
		t.Errorf("version 2 holders = %d, want 1 (gamma alone)", len(crossing.Versions[1].Holders))
	}
	if len(body.Tracks[1].Versions) != 1 {
		t.Errorf("solo versions = %d, want 1", len(body.Tracks[1].Versions))
	}
}

func TestMadnetworkRoutes_NotRegisteredWithoutStore(t *testing.T) {
	r := chi.NewRouter()
	RegisterAPI(r, Deps{})
	srv := httptest.NewServer(r)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/madnetwork/artists")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("without a store = %d, want 404", resp.StatusCode)
	}
}
