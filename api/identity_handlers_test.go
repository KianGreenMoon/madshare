package api

import (
	"context"
	"net/http"
	"testing"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/federation"
)

// TestTextAgrees is the whole mismatch warning in one table. The rule has to be
// loose enough that a remaster suffix never cries wolf — a warning that fires on
// ordinary catalogue noise is a warning nobody reads by the time a real mislabel
// arrives — and tight enough that a different song is a different song.
func TestTextAgrees(t *testing.T) {
	cases := []struct {
		name          string
		claimed, othr string
		want          bool
	}{
		{"identical", "Never Gonna Give You Up", "Never Gonna Give You Up", true},
		{"case and spacing", "  never   gonna GIVE you up ", "Never Gonna Give You Up", true},
		{"punctuation styles", "Don't Stop Me Now", "Dont Stop Me Now", true},
		{"remaster suffix", "Song (Remastered 2011)", "Song", true},
		{"live suffix the other way", "Song", "Song - Live at Wembley", true},
		{"typographic apostrophe", "Don’t Stop Me Now", "Don't Stop Me Now", true},
		{"the rickroll", "Smells Like Teen Spirit", "Never Gonna Give You Up", false},
		{"a different track off one album", "Breathe", "Time", false},
		// Prefix, not containment: a longer word starting with a shorter title is
		// a different title, and substring matching used to call it agreement.
		{"a longer word is not a suffix", "Time", "Timeless", false},
		{"a difference at the front", "Song", "My Song", false},
		{"empty claim contradicts nothing", "", "Never Gonna Give You Up", false},
		{"empty oracle answer", "Song", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := textAgrees(tc.claimed, tc.othr); got != tc.want {
				t.Errorf("textAgrees(%q, %q) = %v, want %v", tc.claimed, tc.othr, got, tc.want)
			}
		})
	}
}

// identityReply is the wire shape the review card reads.
type identityReply struct {
	Claimed struct {
		Title  string `json:"title"`
		Artist string `json:"artist"`
	} `json:"claimed"`
	Network struct {
		Available bool   `json:"available"`
		Agrees    bool   `json:"agrees"`
		Title     string `json:"title"`
		Artist    string `json:"artist"`
		Voices    int    `json:"voices"`
		Note      string `json:"note"`
	} `json:"network"`
	External struct {
		Available bool   `json:"available"`
		Agrees    bool   `json:"agrees"`
		Note      string `json:"note"`
	} `json:"external"`
}

// TestIdentityWarnsWhenTheNetworkCallsItSomethingElse is the mislabel case end to
// end: the tags claim one song, every node holding the same bytes calls it
// another, and the endpoint says so without acting on it.
func TestIdentityWarnsWhenTheNetworkCallsItSomethingElse(t *testing.T) {
	srv, db := newModerationServerWithNetwork(t, &fakeFederation{})
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")

	hash, _ := uploadStaged(t, up, srv.URL, "fresh.mp3")
	tid := stagedTagsetID(t, up, srv.URL, hash)

	// Rename the staged appearance to a claim the network will contradict.
	if code := doJSON(t, up, http.MethodPatch, srv.URL+"/api/my/uploads/"+itoa(tid)+"/metadata",
		map[string]any{"title": "Smells Like Teen Spirit", "artist": "Nirvana"}, nil); code != http.StatusOK {
		t.Fatalf("retag = %d, want 200", code)
	}
	if code := doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/submit",
		map[string]any{"tagset_ids": []int64{tid}}, nil); code != http.StatusOK {
		t.Fatalf("submit = %d, want 200", code)
	}

	ctx := context.Background()
	if _, err := db.InsertFederationPeer(ctx, &federation.Peer{
		PublicKey: "aa11", Name: "friendly", State: federation.PeerFriend, CreatedAt: 1000,
	}); err != nil {
		t.Fatalf("insert peer: %v", err)
	}
	src, err := db.EnsureCatalogSource(ctx, "aa11", 1000)
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}
	if err := db.ReplaceSourceCatalog(ctx, src.ID, "s1", 100, []federation.CatalogEntry{{
		Key: "e1", RecordingKey: "r1", Title: "Never Gonna Give You Up", Artist: "Rick Astley",
		Renditions: []federation.CatalogRendition{{Hash: hash, Size: 1000, Codec: "mp3"}},
	}}); err != nil {
		t.Fatalf("ReplaceSourceCatalog: %v", err)
	}

	var rep identityReply
	if code := doJSON(t, admin, http.MethodGet, modAction(srv.URL, tid, "identity"), nil, &rep); code != http.StatusOK {
		t.Fatalf("identity = %d, want 200", code)
	}
	if rep.Claimed.Title != "Smells Like Teen Spirit" {
		t.Errorf("claimed title = %q, want the submission's own tag", rep.Claimed.Title)
	}
	if !rep.Network.Available {
		t.Fatalf("network oracle unavailable: %q", rep.Network.Note)
	}
	if rep.Network.Agrees {
		t.Error("network oracle agrees; it is holding the same bytes under a different name")
	}
	if rep.Network.Title != "Never Gonna Give You Up" || rep.Network.Artist != "Rick Astley" {
		t.Errorf("network says %q by %q, want the label its holders published",
			rep.Network.Title, rep.Network.Artist)
	}
	// With no AcoustID key configured the external oracle must say so rather
	// than read as quiet agreement.
	if rep.External.Available || rep.External.Agrees {
		t.Errorf("external oracle available/agreeing with no key configured: %+v", rep.External)
	}
	if rep.External.Note == "" {
		t.Error("external oracle unavailable with no explanation; silence reads as a pass")
	}
}

// TestIdentityIsGatedAndScoped: an uploader may not run the moderator's oracles,
// and an unknown appearance is a 404 like every other tagset-addressed route.
func TestIdentityIsGatedAndScoped(t *testing.T) {
	srv, db := newModerationServerWithNetwork(t, &fakeFederation{})
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")

	hash, _ := uploadStaged(t, up, srv.URL, "fresh.mp3")
	tid := stagedTagsetID(t, up, srv.URL, hash)

	if code := doJSON(t, up, http.MethodGet, modAction(srv.URL, tid, "identity"), nil, nil); code != http.StatusForbidden {
		t.Errorf("uploader identity = %d, want 403", code)
	}
	if code := doJSON(t, admin, http.MethodGet, modAction(srv.URL, 999999, "identity"), nil, nil); code != http.StatusNotFound {
		t.Errorf("unknown identity = %d, want 404", code)
	}
}
