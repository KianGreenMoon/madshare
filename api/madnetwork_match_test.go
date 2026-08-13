package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

func matchOf(key, name string, entry federation.CatalogEntry, kind string) database.NetworkMatch {
	return database.NetworkMatch{
		Source: federation.BlobProvider{SourceID: int64(len(key)), PublicKey: key, HeardName: name},
		Entry:  entry,
		Match:  kind,
	}
}

// TestFoldMatchTagsetsOrdersByVoices is the sybil rule at the review card: two
// independent nodes calling a recording one thing outrank a farm of nodes behind
// a single friendship calling it another, however many keys the farm runs. This
// is the same rule mergeVersions applies on /madnetwork, and it matters more
// here — this list is what a moderator reads as "what the network calls this".
func TestFoldMatchTagsetsOrdersByVoices(t *testing.T) {
	honest := func(key string) database.NetworkMatch {
		return matchOf(key, key, federation.CatalogEntry{Title: "Real Title", Artist: "Real Artist"}, database.MatchHash)
	}
	farm := func(key string) database.NetworkMatch {
		return matchOf(key, key, federation.CatalogEntry{Title: "Rickroll", Artist: "Nope"}, database.MatchHash)
	}
	opts := mergeOpts{branches: database.BranchMap{
		"h1": {"friend-a"}, "h2": {"friend-b"},
		// Three keys, all reachable only through one friendship: one voice.
		"f1": {"friend-c"}, "f2": {"friend-c"}, "f3": {"friend-c"},
	}}

	got := foldMatchTagsets([]database.NetworkMatch{
		farm("f1"), farm("f2"), farm("f3"), honest("h1"), honest("h2"),
	}, opts)

	if len(got) != 2 {
		t.Fatalf("folded to %d tagsets, want 2", len(got))
	}
	if got[0].Title != "Real Title" {
		t.Errorf("leading label = %q, want the two-voice one; the farm's three keys are one voice", got[0].Title)
	}
	if got[0].Voices != 2 || len(got[0].Holders) != 2 {
		t.Errorf("honest label = %d voices / %d holders, want 2/2", got[0].Voices, len(got[0].Holders))
	}
	if got[1].Voices != 1 || len(got[1].Holders) != 3 {
		t.Errorf("farm label = %d voices / %d holders, want 1/3 — the gap is the whole point",
			got[1].Voices, len(got[1].Holders))
	}
}

// TestFoldMatchRenditionsRanksAndFlags pins the three facts the card renders per
// remote encoding: the ladder order, which one would actually be an upgrade, and
// which we already hold. "Better" must be decided by RankRenditions itself, so
// the card can never disagree with the recording page about what better means.
func TestFoldMatchRenditionsRanksAndFlags(t *testing.T) {
	ours := database.Rendition{FileID: 1, Hash: "ourhash", Codec: "mp3", Bitrate: 192, ByteSize: 500}
	entry := federation.CatalogEntry{Title: "T", Renditions: []federation.CatalogRendition{
		{Hash: "ourhash", Codec: "mp3", Bitrate: 192, Size: 500},
		{Hash: "betterhash", Codec: "flac", SampleRate: 44100, BitDepth: 16, Size: 5000},
		{Hash: "worsehash", Codec: "mp3", Bitrate: 128, Size: 300},
	}}
	m := matchOf("k1", "peer", entry, database.MatchHash)
	m.SharedHash = "ourhash"

	got := foldMatchRenditions([]database.NetworkMatch{m}, mergeOpts{}, &ours)

	if len(got) != 3 {
		t.Fatalf("folded to %d renditions, want 3", len(got))
	}
	if got[0].Hash != "betterhash" {
		t.Errorf("ladder-best = %q, want betterhash (lossless outranks lossy)", got[0].Hash)
	}
	byHash := map[string]networkRendition{}
	for _, r := range got {
		byHash[r.Hash] = r
	}
	if !byHash["betterhash"].Better {
		t.Error("the lossless rendition is not flagged better than our 192k mp3")
	}
	if byHash["worsehash"].Better {
		t.Error("a 128k mp3 is flagged as an upgrade over our 192k one")
	}
	if !byHash["ourhash"].Held {
		t.Error("the shared blob is not flagged as held; a hash match is not news")
	}
	if byHash["ourhash"].Better {
		t.Error("a rendition we already hold is flagged as an upgrade over itself")
	}
}

// newModerationServerWithNetwork is newAuthTestServerRaw plus the madnetwork
// store and a stub federation node — the wiring a node with federation enabled
// actually has. Kept separate rather than folded into the shared helper because
// every other api test asserts the *absence* of that wiring.
func newModerationServerWithNetwork(t *testing.T, fed FederationNode) (*httptest.Server, *database.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if created, err := auth.Bootstrap(context.Background(), db, "admin", testAdminPassword); err != nil || !created {
		t.Fatalf("bootstrap admin: created=%v err=%v", created, err)
	}
	if _, err := db.Exec(`UPDATE users SET password_change_required = 0 WHERE username = 'admin'`); err != nil {
		t.Fatalf("clear admin change-required: %v", err)
	}
	deps := Deps{
		Store: storage.NewLocal(filepath.Join(dir, storage.AudioSubdir)), Repo: db,
		SpoolDir: t.TempDir(), FilesDir: dir, MaxUploadSize: testMaxUpload,
		Auth: db, Manage: db, Madnetwork: db, Federation: fed, MadnetworkName: "us",
	}
	r := chi.NewRouter()
	r.Use(auth.Identify(db))
	RegisterAPI(r, deps)
	RegisterAdmin(r, deps)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, db
}

// madnetworkArm is the wire shape the review card reads.
type madnetworkArm struct {
	Fingerprinted bool `json:"fingerprinted"`
	Tagsets       []struct {
		Title   string `json:"title"`
		Artist  string `json:"artist"`
		Voices  int    `json:"voices"`
		Match   string `json:"match"`
		Holders []struct {
			Name string `json:"name"`
			Key  string `json:"key"`
		} `json:"holders"`
	} `json:"tagsets"`
	Renditions []struct {
		Hash string `json:"hash"`
		Held bool   `json:"held"`
	} `json:"renditions"`
}

// TestClassifyCarriesTheMadnetworkArm is the end-to-end: a real upload, a real
// cached catalog advertising its bytes, and the real join — so the SQL, the fold
// and the handler are all exercised together rather than around a fake.
func TestClassifyCarriesTheMadnetworkArm(t *testing.T) {
	fed := &fakeFederation{branches: map[string][]string{"aa11": {"friend-a"}}}
	srv, db := newModerationServerWithNetwork(t, fed)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")

	hash, _ := uploadStaged(t, up, srv.URL, "fresh.mp3")
	tid := stagedTagsetID(t, up, srv.URL, hash)
	if code := doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/submit",
		map[string]any{"tagset_ids": []int64{tid}}, nil); code != http.StatusOK {
		t.Fatalf("submit = %d, want 200", code)
	}

	// A friend's catalog advertises the very bytes that were just uploaded,
	// under its own name for them.
	ctx := context.Background()
	if _, err := db.InsertFederationPeer(ctx, &federation.ExternalNode{
		PublicKey: "aa11", Label: "friendly", TrustState: federation.PeerFriend, TrustedAt: 1000,
	}); err != nil {
		t.Fatalf("insert peer: %v", err)
	}
	src, err := db.EnsureCatalogSource(ctx, "aa11", 1000)
	if err != nil {
		t.Fatalf("ensure source: %v", err)
	}
	if err := db.ReplaceSourceCatalog(ctx, src.ID, "s1", 100, []federation.CatalogEntry{{
		Key: "e1", RecordingKey: "r1", Title: "Their Title", Artist: "Their Artist",
		Renditions: []federation.CatalogRendition{{Hash: hash, Size: 1000, Codec: "flac"}},
	}}); err != nil {
		t.Fatalf("ReplaceSourceCatalog: %v", err)
	}

	var cl struct {
		Class      string         `json:"class"`
		Madnetwork *madnetworkArm `json:"madnetwork"`
	}
	if code := doJSON(t, admin, http.MethodGet, modAction(srv.URL, tid, "classify"), nil, &cl); code != http.StatusOK {
		t.Fatalf("classify = %d, want 200", code)
	}
	if cl.Madnetwork == nil {
		t.Fatal("classify carried no madnetwork arm despite a friend advertising these bytes")
	}
	if len(cl.Madnetwork.Tagsets) != 1 {
		t.Fatalf("arm carried %d tagsets, want 1", len(cl.Madnetwork.Tagsets))
	}
	ts := cl.Madnetwork.Tagsets[0]
	if ts.Title != "Their Title" || ts.Match != database.MatchHash {
		t.Errorf("tagset = %q via %q, want \"Their Title\" via %q", ts.Title, ts.Match, database.MatchHash)
	}
	if ts.Voices != 1 {
		t.Errorf("voices = %d, want 1", ts.Voices)
	}
	if len(ts.Holders) != 1 || ts.Holders[0].Name != "friendly" || ts.Holders[0].Key != "aa11" {
		t.Errorf("holders = %+v, want the friend labelled and keyed for the map link", ts.Holders)
	}
	if len(cl.Madnetwork.Renditions) != 1 || !cl.Madnetwork.Renditions[0].Held {
		t.Errorf("renditions = %+v, want the shared blob flagged held", cl.Madnetwork.Renditions)
	}
}

// TestClassifySendsAnEmptyArmWhenTheNetworkKnowsNothing pins the distinction the
// card's wording depends on: with a madnetwork wired but no match, the arm is
// present and empty ("we asked, nobody knows it"), which is a different sentence
// from the absent arm of the test below ("there was nothing to ask").
func TestClassifySendsAnEmptyArmWhenTheNetworkKnowsNothing(t *testing.T) {
	srv, db := newModerationServerWithNetwork(t, &fakeFederation{})
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")

	hash, _ := uploadStaged(t, up, srv.URL, "fresh.mp3")
	tid := stagedTagsetID(t, up, srv.URL, hash)
	if code := doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/submit",
		map[string]any{"tagset_ids": []int64{tid}}, nil); code != http.StatusOK {
		t.Fatalf("submit = %d, want 200", code)
	}

	var cl struct {
		Madnetwork *madnetworkArm `json:"madnetwork"`
	}
	if code := doJSON(t, admin, http.MethodGet, modAction(srv.URL, tid, "classify"), nil, &cl); code != http.StatusOK {
		t.Fatalf("classify = %d, want 200", code)
	}
	if cl.Madnetwork == nil {
		t.Fatal("arm absent on a node that has a madnetwork; the card cannot tell the two cases apart")
	}
	if len(cl.Madnetwork.Tagsets) != 0 {
		t.Errorf("arm carried %d tagsets, want none", len(cl.Madnetwork.Tagsets))
	}
}

// TestClassifyOmitsTheArmWithoutFederation: a node with federation off must get
// the classification unchanged and no empty arm to render.
func TestClassifyOmitsTheArmWithoutFederation(t *testing.T) {
	srv, db := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")

	hash, _ := uploadStaged(t, up, srv.URL, "fresh.mp3")
	tid := stagedTagsetID(t, up, srv.URL, hash)
	if code := doJSON(t, up, http.MethodPost, srv.URL+"/api/my/uploads/submit",
		map[string]any{"tagset_ids": []int64{tid}}, nil); code != http.StatusOK {
		t.Fatalf("submit = %d, want 200", code)
	}

	var cl struct {
		Class      string         `json:"class"`
		Madnetwork *madnetworkArm `json:"madnetwork"`
	}
	if code := doJSON(t, admin, http.MethodGet, modAction(srv.URL, tid, "classify"), nil, &cl); code != http.StatusOK {
		t.Fatalf("classify = %d, want 200", code)
	}
	if cl.Class != database.SubmissionNewRecording {
		t.Errorf("class = %q, want %q", cl.Class, database.SubmissionNewRecording)
	}
	if cl.Madnetwork != nil {
		t.Errorf("arm present with federation off: %+v", cl.Madnetwork)
	}
}
