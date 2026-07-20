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
	rows    []*database.MadnetworkTrackRow
	ownRows []*database.MadnetworkTrackRow
	artists []*database.MadnetworkArtist
}

func (f *fakeMadnetwork) MadnetworkArtists(context.Context, string, bool) ([]*database.MadnetworkArtist, error) {
	if f.artists != nil {
		return f.artists, nil
	}
	return []*database.MadnetworkArtist{{Name: "A", Albums: 1, Tracks: 2}}, nil
}
func (f *fakeMadnetwork) MadnetworkAlbums(context.Context, string, bool) ([]*database.MadnetworkAlbum, error) {
	return nil, nil
}
func (f *fakeMadnetwork) MadnetworkTracks(context.Context, string, string) ([]*database.MadnetworkTrackRow, error) {
	return f.rows, nil
}
func (f *fakeMadnetwork) MadnetworkOwnTracks(context.Context, string, string) ([]*database.MadnetworkTrackRow, error) {
	return f.ownRows, nil
}
func (f *fakeMadnetwork) MadnetworkSummary(context.Context, bool) ([]*database.MadnetworkFriend, int64, error) {
	return nil, 0, nil
}
func (f *fakeMadnetwork) MadnetworkSearchAlbums(context.Context, string, int, bool) ([]*database.MadnetworkSearchAlbum, error) {
	return []*database.MadnetworkSearchAlbum{{Artist: "A", Title: "B", Tracks: 2}}, nil
}
func (f *fakeMadnetwork) MadnetworkSearchTrackRows(context.Context, string, bool) ([]*database.MadnetworkTrackRow, error) {
	return append(append([]*database.MadnetworkTrackRow{}, f.rows...), f.ownRows...), nil
}
func (f *fakeMadnetwork) MadnetworkEntryForHash(_ context.Context, hash string) (*federation.CatalogEntry, error) {
	for _, r := range f.rows {
		for _, rd := range r.Entry.Renditions {
			if rd.Hash == hash {
				e := r.Entry
				return &e, nil
			}
		}
	}
	return nil, nil
}
func (f *fakeMadnetwork) GetMadnetworkPolicy(context.Context) (database.MadnetworkPolicy, error) {
	return database.MadnetworkPolicy{}, nil
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

// TestMadnetworkTracks_SelfMerge: with a running node (MadnetworkName set),
// own rows fold into the merged view — a shared hash makes one version whose
// holders include the self entry, the local play URL surfaces, and the track
// carries its local tagset id.
func TestMadnetworkTracks_SelfMerge(t *testing.T) {
	own := madRow(0, "", "r-local", "Crossing", "h-shared")
	own.Self = true
	own.Entry.Key = "42" // local tagset id
	own.GroupArtist, own.GroupAlbum = "A", "B"
	own.ObjectKeys = map[string]string{"h-shared": "h-shared/crossing.mp3"}
	fake := &fakeMadnetwork{
		rows:    []*database.MadnetworkTrackRow{madRow(1, "alpha", "r1", "Crossing", "h-shared")},
		ownRows: []*database.MadnetworkTrackRow{own},
	}
	r := chi.NewRouter()
	RegisterAPI(r, Deps{Madnetwork: fake, MadnetworkName: "my node"})
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
			TagsetID int64  `json:"tagset_id"`
			Versions []struct {
				URL     string `json:"url"`
				Holders []struct {
					Name string `json:"name"`
					Self bool   `json:"self"`
				} `json:"holders"`
			} `json:"versions"`
		} `json:"tracks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tracks) != 1 {
		t.Fatalf("tracks = %d, want 1 (self row folds into the peer's)", len(body.Tracks))
	}
	tr := body.Tracks[0]
	if tr.TagsetID != 42 {
		t.Errorf("tagset_id = %d, want the local appearance 42", tr.TagsetID)
	}
	if len(tr.Versions) != 1 {
		t.Fatalf("versions = %d, want 1 (shared hash)", len(tr.Versions))
	}
	v := tr.Versions[0]
	if v.URL != "/files/h-shared/crossing.mp3" {
		t.Errorf("version url = %q, want the direct local address", v.URL)
	}
	var selfHolder bool
	for _, h := range v.Holders {
		if h.Self && h.Name == "my node" {
			selfHolder = true
		}
	}
	if len(v.Holders) != 2 || !selfHolder {
		t.Errorf("holders = %+v, want peer + self(my node)", v.Holders)
	}
}

// TestMadnetworkSearch: the merged search mirrors /api/search's shape — artist
// hits capped, album hits carry the drill address, track hits merged per
// bucket with a playable hash (and the local url for self-held tracks).
func TestMadnetworkSearch(t *testing.T) {
	var artists []*database.MadnetworkArtist
	for _, n := range []string{"A1", "A2", "A3", "A4", "A5", "A6", "A7"} {
		artists = append(artists, &database.MadnetworkArtist{Name: n})
	}
	remote := madRow(1, "alpha", "r1", "Crossing", "h-shared")
	remote.GroupArtist, remote.GroupAlbum = "A", "B"
	own := madRow(0, "", "r-local", "Crossing", "h-shared")
	own.Self = true
	own.Entry.Key = "42"
	own.GroupArtist, own.GroupAlbum = "A", "B"
	own.ObjectKeys = map[string]string{"h-shared": "h-shared/crossing.mp3"}
	other := madRow(2, "beta", "r2", "Cross Words", "h-other")
	other.GroupArtist, other.GroupAlbum = "Z", "Y"
	fake := &fakeMadnetwork{
		artists: artists,
		rows:    []*database.MadnetworkTrackRow{remote, other},
		ownRows: []*database.MadnetworkTrackRow{own},
	}
	r := chi.NewRouter()
	RegisterAPI(r, Deps{Madnetwork: fake, MadnetworkName: "my node"})
	srv := httptest.NewServer(r)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/madnetwork/search?q=cross")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Artists []struct {
			Name string `json:"name"`
		} `json:"artists"`
		Albums []struct {
			Title      string `json:"title"`
			ArtistName string `json:"artist_name"`
		} `json:"albums"`
		Tracks []struct {
			Title      string `json:"title"`
			Artist     string `json:"artist"`
			AlbumTitle string `json:"album_title"`
			TagsetID   int64  `json:"tagset_id"`
			Hash       string `json:"hash"`
			URL        string `json:"url"`
		} `json:"tracks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Artists) != madnetworkSearchArtistCap {
		t.Errorf("artists = %d, want capped at %d", len(body.Artists), madnetworkSearchArtistCap)
	}
	if len(body.Albums) != 1 || body.Albums[0].ArtistName != "A" {
		t.Errorf("albums = %+v, want the fake hit with its drill artist", body.Albums)
	}
	if len(body.Tracks) != 2 {
		t.Fatalf("tracks = %+v, want 2 merged hits", body.Tracks)
	}
	crossing := body.Tracks[0]
	if crossing.Title != "Crossing" || crossing.Hash != "h-shared" ||
		crossing.URL != "/files/h-shared/crossing.mp3" || crossing.TagsetID != 42 {
		t.Errorf("crossing hit = %+v, want merged self+remote with local url", crossing)
	}
	if body.Tracks[1].AlbumTitle != "Y" || body.Tracks[1].URL != "" {
		t.Errorf("remote-only hit = %+v, want album Y and no local url", body.Tracks[1])
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
