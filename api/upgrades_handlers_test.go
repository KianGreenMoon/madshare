package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
)

// TestUpgradesListAndDecide covers the page's two calls: what it reads, and the
// decision that has to survive the next scan.
func TestUpgradesListAndDecide(t *testing.T) {
	srv, db := newModerationServerWithNetwork(t, &fakeFederation{})
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")

	// Seed a finding directly: the scan itself is pinned against a real
	// database in database/upgrades_test.go, and what is under test here is the
	// endpoint pair.
	if _, err := db.Exec(`
		INSERT INTO recordings (id, created_at) VALUES (900, 1)`); err != nil {
		t.Fatalf("seed recording: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO library_upgrades
		  (id, recording_id, remote_hash, match, ber, codec, sample_rate, bit_depth,
		   byte_size, disposition, first_seen, last_seen)
		VALUES (1, 900, 'flachash', 'fingerprint', 0.02, 'flac', 44100, 16, 3000, 'new', 10, 20)`); err != nil {
		t.Fatalf("seed finding: %v", err)
	}

	var list struct {
		Total int `json:"total"`
		Items []struct {
			ID          int64  `json:"id"`
			RemoteHash  string `json:"remote_hash"`
			Match       string `json:"match"`
			Disposition string `json:"disposition"`
			Offered     struct {
				Codec string `json:"codec"`
			} `json:"offered"`
		} `json:"items"`
	}
	if code := doJSON(t, admin, http.MethodGet, srv.URL+"/api/admin/upgrades", nil, &list); code != http.StatusOK {
		t.Fatalf("list = %d, want 200", code)
	}
	if list.Total != 1 || len(list.Items) != 1 {
		t.Fatalf("listed %d/%d, want one open finding", list.Total, len(list.Items))
	}
	if list.Items[0].RemoteHash != "flachash" || list.Items[0].Offered.Codec != "flac" {
		t.Errorf("item = %+v, want the seeded flac finding", list.Items[0])
	}

	// Dismissing takes it out of the default (open) view but not out of "all".
	if code := doJSON(t, admin, http.MethodPatch, srv.URL+"/api/admin/upgrades/1",
		map[string]any{"disposition": "dismissed"}, nil); code != http.StatusOK {
		t.Fatalf("dismiss = %d, want 200", code)
	}
	if code := doJSON(t, admin, http.MethodGet, srv.URL+"/api/admin/upgrades", nil, &list); code != http.StatusOK {
		t.Fatalf("list after dismiss = %d, want 200", code)
	}
	if list.Total != 0 {
		t.Errorf("open findings after dismiss = %d, want 0", list.Total)
	}
	if code := doJSON(t, admin, http.MethodGet, srv.URL+"/api/admin/upgrades?disposition=all", nil, &list); code != http.StatusOK {
		t.Fatalf("list all = %d, want 200", code)
	}
	if list.Total != 1 || list.Items[0].Disposition != database.UpgradeDismissed {
		t.Errorf("all view = %d rows / %q, want the dismissal still readable", list.Total, list.Items[0].Disposition)
	}

	// Gating and input validation.
	if code := doJSON(t, up, http.MethodGet, srv.URL+"/api/admin/upgrades", nil, nil); code != http.StatusForbidden {
		t.Errorf("uploader list = %d, want 403", code)
	}
	if code := doJSON(t, admin, http.MethodGet, srv.URL+"/api/admin/upgrades?disposition=bogus", nil, nil); code != http.StatusBadRequest {
		t.Errorf("bogus disposition = %d, want 400", code)
	}
	if code := doJSON(t, admin, http.MethodPatch, srv.URL+"/api/admin/upgrades/1",
		map[string]any{"disposition": "bogus"}, nil); code != http.StatusBadRequest {
		t.Errorf("bogus decision = %d, want 400", code)
	}
	if code := doJSON(t, admin, http.MethodPatch, srv.URL+"/api/admin/upgrades/9999",
		map[string]any{"disposition": "dismissed"}, nil); code != http.StatusNotFound {
		t.Errorf("unknown finding = %d, want 404", code)
	}
}

// TestUpgradesAbsentWithoutAMadnetwork: with no catalogs there is nothing that
// could have been found, so the routes are not registered at all rather than
// answering an empty list — the page then reports nothing, which is honest.
func TestUpgradesAbsentWithoutAMadnetwork(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	if code := doJSON(t, admin, http.MethodGet, srv.URL+"/api/admin/upgrades", nil, nil); code != http.StatusNotFound {
		t.Errorf("list with federation off = %d, want 404", code)
	}
}

// TestUpgradesListShapesTheFakeStore keeps the handler's JSON shaping honest
// without a database: the fake returns one row and the handler must render the
// fields the page reads, including the freshness verdict it computes itself.
func TestUpgradesListShapesTheFakeStore(t *testing.T) {
	fake := &fakeMadnetwork{upgrades: []*database.UpgradeRow{{
		ID: 7, RecordingID: 900, Title: "Song", Artist: "Band",
		RemoteHash: "flachash", Match: database.MatchHash, Disposition: database.UpgradeNew,
		Ours:    database.Rendition{Hash: "ourhash", Codec: "mp3", Bitrate: 192000},
		Offered: database.Rendition{Hash: "flachash", Codec: "flac", SampleRate: 44100},
		Source:  "friendly", SourceKey: "aa11", SourceSeen: 0, SourcePinged: true,
	}}}
	r := chi.NewRouter()
	RegisterAdmin(r, Deps{Madnetwork: fake, Repo: nil})
	srv := httptest.NewServer(r)
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/admin/upgrades")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("list = %d, want 200", res.StatusCode)
	}
	var body struct {
		Items []struct {
			ID              int64  `json:"id"`
			Title           string `json:"title"`
			Source          string `json:"source"`
			SourceReachable bool   `json:"source_reachable"`
			Ours            struct {
				Codec string `json:"codec"`
			} `json:"ours"`
		} `json:"items"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(body.Items))
	}
	it := body.Items[0]
	if it.ID != 7 || it.Title != "Song" || it.Ours.Codec != "mp3" || it.Source != "friendly" {
		t.Errorf("item = %+v, want the seeded row's fields", it)
	}
	// last_seen 0 is "never heard from", which must not read as reachable.
	if it.SourceReachable {
		t.Error("a source never seen is reported reachable")
	}
}
