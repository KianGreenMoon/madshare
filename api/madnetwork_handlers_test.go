package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

type fakeMadnetwork struct {
	rows       []*database.MadnetworkTrackRow
	ownRows    []*database.MadnetworkTrackRow
	artists    []*database.MadnetworkArtist
	lanes      map[string][]*database.LaneCandidate // per-lane ranked candidates
	lastView   database.MadnetworkView              // captured by MadnetworkSummary for assertions
	trackView  database.MadnetworkView              // captured by MadnetworkTracks for assertions
	artistView database.MadnetworkView              // captured by MadnetworkArtists (the ?source= resolution)
	hideOff    bool                                 // when true, GetMadnetworkPolicy reports hiding disabled
	matches    []database.NetworkMatch              // F8: what the join is to report
	matchErr   error                                // F8: a cache read that fails must cost only the arm
	upgrades   []*database.UpgradeRow               // F8 item 3: quality-upgrade findings
	// The node surfaces (docs/ui/madnetwork-nodes.md): the cached sources this
	// server holds a catalog from, the merged track count the summary reports,
	// and how many entries our own published set has.
	sources    []*database.MadnetworkNode
	trackCount int64
	ownEntries int64
}

// MadnetworkArtists honours limit the way the real store does — one page plus a
// cursor when there is more — because the search cap is now expressed as a limit
// passed down rather than a truncation in the handler.
func (f *fakeMadnetwork) MadnetworkArtists(_ context.Context, _ string, view database.MadnetworkView, limit int, _ string) ([]*database.MadnetworkArtist, string, error) {
	f.artistView = view
	out := f.artists
	if out == nil {
		out = []*database.MadnetworkArtist{{Name: "A", Albums: 1, Tracks: 2}}
	}
	if limit > 0 && len(out) > limit {
		return out[:limit], "more", nil
	}
	return out, "", nil
}
func (f *fakeMadnetwork) MadnetworkAlbums(context.Context, string, database.MadnetworkView) ([]*database.MadnetworkAlbum, error) {
	return nil, nil
}
func (f *fakeMadnetwork) MadnetworkTracks(_ context.Context, _, _ string, view database.MadnetworkView) ([]*database.MadnetworkTrackRow, error) {
	f.trackView = view
	return f.rows, nil
}
func (f *fakeMadnetwork) MadnetworkLaneCandidates(_ context.Context, lane string, _ database.MadnetworkView, limit int) ([]*database.LaneCandidate, error) {
	out := f.lanes[lane]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (f *fakeMadnetwork) MadnetworkRowsForIdents(context.Context, []string, database.MadnetworkView) ([]*database.MadnetworkTrackRow, error) {
	return append(append([]*database.MadnetworkTrackRow{}, f.rows...), f.ownRows...), nil
}
func (f *fakeMadnetwork) MadnetworkOwnTracks(context.Context, string, string, database.MadnetworkView) ([]*database.MadnetworkTrackRow, error) {
	return f.ownRows, nil
}
func (f *fakeMadnetwork) MadnetworkSummary(_ context.Context, view database.MadnetworkView) ([]*database.MadnetworkNode, int64, error) {
	f.lastView = view
	return f.sources, f.trackCount, nil
}
func (f *fakeMadnetwork) MadnetworkSourceByKey(_ context.Context, key string, _ database.MadnetworkView) (*database.MadnetworkNode, bool, error) {
	for _, s := range f.sources {
		if s.Key == key {
			return s, true, nil
		}
	}
	return nil, false, nil
}
func (f *fakeMadnetwork) MadnetworkOwnEntries(context.Context, database.MadnetworkView) (int64, error) {
	return f.ownEntries, nil
}
// MadnetworkSearchArtists answers from the same seeded list as MadnetworkArtists
// — the two differ in WHICH buckets qualify, which is SQL the database package
// pins; here only the cap is observable.
func (f *fakeMadnetwork) MadnetworkSearchArtists(_ context.Context, _ string, limit int, view database.MadnetworkView) ([]*database.MadnetworkArtist, error) {
	f.artistView = view
	out := f.artists
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (f *fakeMadnetwork) MadnetworkSearchAlbums(context.Context, string, int, database.MadnetworkView) ([]*database.MadnetworkSearchAlbum, error) {
	return []*database.MadnetworkSearchAlbum{{Artist: "A", Title: "B", Tracks: 2}}, nil
}
func (f *fakeMadnetwork) MadnetworkSearchTrackRows(context.Context, string, database.MadnetworkView) ([]*database.MadnetworkTrackRow, error) {
	return append(append([]*database.MadnetworkTrackRow{}, f.rows...), f.ownRows...), nil
}

// MatchRecording (F8) answers from whatever the test seeded in matches — the
// fake never re-derives the join, since the join itself is pinned in the
// database package where the SQL and the fingerprint arithmetic live.
func (f *fakeMadnetwork) MatchRecording(context.Context, int64, int64) ([]database.NetworkMatch, error) {
	return f.matches, f.matchErr
}

// The upgrade findings (F8 item 3) are written by a SQL scan and read back by
// one query; both are pinned in database/upgrades_test.go against a real
// database, so the fake only has to satisfy the interface.
func (f *fakeMadnetwork) ListUpgrades(context.Context, string, int64, int, int) ([]*database.UpgradeRow, int, error) {
	return f.upgrades, len(f.upgrades), nil
}
func (f *fakeMadnetwork) SetUpgradeDisposition(_ context.Context, id int64, d string) (bool, error) {
	for _, u := range f.upgrades {
		if u.ID == id {
			u.Disposition = d
			return true, nil
		}
	}
	return false, nil
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
	return database.MadnetworkPolicy{HideUnavailable: !f.hideOff}, nil
}

func madRow(peerID int64, peer, recording, title string, hashes ...string) *database.MadnetworkTrackRow {
	e := federation.CatalogEntry{Key: recording + "-key", RecordingKey: recording, Title: title}
	for _, h := range hashes {
		e.Renditions = append(e.Renditions, federation.CatalogRendition{Hash: h, Size: 1})
	}
	return &database.MadnetworkTrackRow{SourceID: peerID, SourceName: peer, Entry: e}
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

// TestMadnetworkVersions_BranchWeighted is the sybil rule where it bites
// hardest (F7 item 10): a crossing's LEADING version is what Play, Queue and
// Materialize act on, so ordering it by raw holder count would let a farm of
// keys behind one friendship make its claim everyone's default pick. Three
// forged holders behind friend-a must lose to two independent ones.
func TestMadnetworkVersions_BranchWeighted(t *testing.T) {
	keyed := func(id int64, name, key, recording string, hashes ...string) *database.MadnetworkTrackRow {
		row := madRow(id, name, recording, "Crossing", hashes...)
		row.SourceKey = key
		return row
	}
	fake := &fakeMadnetwork{rows: []*database.MadnetworkTrackRow{
		// The farm: three nodes, one claimed recording, all reached through
		// friend-a — and a bigger holder count than the honest version.
		keyed(1, "sybil-1", "s1", "r-fake", "h-fake"),
		keyed(2, "sybil-2", "s2", "r-fake", "h-fake"),
		keyed(3, "sybil-3", "s3", "r-fake", "h-fake"),
		// Two holders, two friendships, two voices.
		keyed(4, "alpha", "x1", "r-real", "h-real"),
		keyed(5, "beta", "y1", "r-real", "h-real"),
	}}
	fed := &fakeFederation{branches: map[string][]string{
		"s1": {"friend-a"}, "s2": {"friend-a"}, "s3": {"friend-a"},
		"x1": {"friend-b"}, "y1": {"friend-c"},
	}}
	r := chi.NewRouter()
	RegisterAPI(r, Deps{Madnetwork: fake, Federation: fed, MadnetworkName: "my node"})
	srv := httptest.NewServer(r)
	defer srv.Close()

	get := func() []struct {
		Voices     int `json:"voices"`
		Renditions []struct {
			Hash string `json:"hash"`
		} `json:"renditions"`
		Holders []struct {
			Name string `json:"name"`
		} `json:"holders"`
	} {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/madnetwork/tracks?artist=A&album=B")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct {
			Tracks []struct {
				Versions []struct {
					Voices     int `json:"voices"`
					Renditions []struct {
						Hash string `json:"hash"`
					} `json:"renditions"`
					Holders []struct {
						Name string `json:"name"`
					} `json:"holders"`
				} `json:"versions"`
			} `json:"tracks"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.Tracks) != 1 {
			t.Fatalf("tracks = %d, want 1", len(body.Tracks))
		}
		return body.Tracks[0].Versions
	}

	versions := get()
	if len(versions) != 2 {
		t.Fatalf("versions = %d, want 2 (disjoint hashes never merge)", len(versions))
	}
	if h := versions[0].Renditions[0].Hash; h != "h-real" {
		t.Errorf("default pick = %q, want h-real: three keys behind one friendship outranked two independent holders", h)
	}
	if versions[0].Voices != 0 {
		t.Errorf("honest version voices = %d, want it omitted — 2 holders, 2 voices is not news", versions[0].Voices)
	}
	if versions[1].Voices != 1 || len(versions[1].Holders) != 3 {
		t.Errorf("farmed version = %d voices / %d holders, want 1 voice reported beside 3 holders",
			versions[1].Voices, len(versions[1].Holders))
	}

	// Without the graph the same rows fall back to one source one voice, which
	// puts the farm back on top. That is the honest answer for a node that
	// cannot place anyone — and it is what makes the weighting above a fact
	// about the graph rather than about this test's row order.
	fed.branches = nil
	if h := get()[0].Renditions[0].Hash; h != "h-fake" {
		t.Errorf("ungraphed default pick = %q, want h-fake (3 unplaceable holders = 3 voices)", h)
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

// TestMadnetworkView_FailOpen verifies the availability policy the browse
// handlers build: a healthy node filters by a positive cutoff, while a suspect
// inbound path fails open (cutoff 0 = show the last-known catalog) and reports
// inbound_healthy=false (docs/architecture/federation.md §Availability).
func TestMadnetworkView_FailOpen(t *testing.T) {
	call := func(fake *fakeMadnetwork, fed FederationNode) (*fakeMadnetwork, bool) {
		r := chi.NewRouter()
		RegisterAPI(r, Deps{Madnetwork: fake, MadnetworkName: "n", Federation: fed})
		srv := httptest.NewServer(r)
		defer srv.Close()
		resp, err := http.Get(srv.URL + "/api/madnetwork/summary")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct {
			InboundHealthy bool `json:"inbound_healthy"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return fake, body.InboundHealthy
	}

	// Healthy + hiding on: filtering active (positive cutoff), inbound_healthy true.
	if fake, healthy := call(&fakeMadnetwork{}, &fakeFederation{}); fake.lastView.Cutoff <= 0 || !healthy {
		t.Errorf("healthy: cutoff = %d, inbound_healthy = %v; want cutoff>0, healthy true",
			fake.lastView.Cutoff, healthy)
	}
	// Admin turned hiding off: no filtering (cutoff 0) even though healthy.
	if fake, healthy := call(&fakeMadnetwork{hideOff: true}, &fakeFederation{}); fake.lastView.Cutoff != 0 || !healthy {
		t.Errorf("hiding off: cutoff = %d, inbound_healthy = %v; want cutoff 0, healthy true",
			fake.lastView.Cutoff, healthy)
	}
	// Inbound dead: fail open (cutoff 0), inbound_healthy false.
	if fake, healthy := call(&fakeMadnetwork{}, &fakeFederation{inboundDead: true}); fake.lastView.Cutoff != 0 || healthy {
		t.Errorf("inbound dead: cutoff = %d, inbound_healthy = %v; want cutoff 0, healthy false",
			fake.lastView.Cutoff, healthy)
	}
}

// TestMadnetworkHolders_Reachability: in a fail-open response (both fresh and
// stale holders present), each holder carries a display reachability flag —
// fresh within the window is reachable, long-ago is not (the ⓘ panel greys it).
func TestMadnetworkHolders_Reachability(t *testing.T) {
	fresh := madRow(1, "fresh-peer", "r1", "Song", "h1")
	fresh.SourceLastSeen = time.Now().Unix()
	stale := madRow(2, "stale-peer", "r2", "Song", "h1") // same hash → one version, two holders
	stale.SourceLastSeen = 1
	fake := &fakeMadnetwork{rows: []*database.MadnetworkTrackRow{fresh, stale}}
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
			Versions []struct {
				Holders []struct {
					Name      string `json:"name"`
					Reachable bool   `json:"reachable"`
				} `json:"holders"`
			} `json:"versions"`
		} `json:"tracks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tracks) != 1 || len(body.Tracks[0].Versions) != 1 {
		t.Fatalf("want 1 track / 1 version, got %+v", body.Tracks)
	}
	reach := map[string]bool{}
	for _, h := range body.Tracks[0].Versions[0].Holders {
		reach[h.Name] = h.Reachable
	}
	if !reach["fresh-peer"] || reach["stale-peer"] {
		t.Errorf("holder reachability = %+v, want fresh=true stale=false", reach)
	}
}
