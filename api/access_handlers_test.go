package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"daemonlord.ygg/madshare/database"
)

// doJSON performs a JSON request with the given client and returns the status
// and decoded body (into out, if non-nil).
func doJSON(t *testing.T, client *http.Client, method, url string, body any, out any) int {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	if out != nil && resp.Body != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// getFileItems GETs the paginated /api/files envelope as the given client and
// returns just the items slice, so callers keep their len()/index assertions
// against the listing (see docs/architecture/file-list-scaling.md).
func getFileItems(t *testing.T, client *http.Client, url string) []map[string]any {
	t.Helper()
	var env struct {
		Items []map[string]any `json:"items"`
	}
	if code := doJSON(t, client, http.MethodGet, url, nil, &env); code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, code)
	}
	return env.Items
}

// uploadAndHash uploads a file as the given client and returns its content hash
// and download path (/files/<hash>/<filename>). With moderation (migration 017)
// an authenticated upload stages as a draft, so the helper submits it right
// away — the callers here are admin clients, whose submit self-approves — to
// yield a published library file like before.
func uploadAndHash(t *testing.T, client *http.Client, base, name string) (hash, path string) {
	t.Helper()
	resp := uploadViaClient(t, client, base, name)
	defer resp.Body.Close()
	var body struct {
		Hash     string `json:"hash"`
		Filename string `json:"filename"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode upload response: %v", err)
	}
	if body.Hash == "" {
		t.Fatalf("upload returned no hash (status %d)", resp.StatusCode)
	}
	approveUpload(t, client, base, body.Hash)
	return body.Hash, "/files/" + body.Hash + "/" + body.Filename
}

// approveUpload publishes a staged upload via the submit endpoint. The client
// must hold content.moderate (e.g. the admin) so the submit self-approves.
func approveUpload(t *testing.T, client *http.Client, base, hash string) {
	t.Helper()
	var out struct {
		Approved  bool `json:"approved"`
		Submitted int  `json:"submitted"`
	}
	if code := doJSON(t, client, http.MethodPost, base+"/api/my/uploads/submit",
		map[string]any{"hashes": []string{hash}}, &out); code != http.StatusOK {
		t.Fatalf("submit upload = %d, want 200", code)
	}
	if !out.Approved || out.Submitted != 1 {
		t.Fatalf("submit upload: approved=%v submitted=%d, want self-approve of 1", out.Approved, out.Submitted)
	}
}

func TestManage_GuestPlayableEndpoint(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	hash, path := uploadAndHash(t, admin, srv.URL, "free.mp3")

	// Anonymous denied by default.
	if code := doJSON(t, http.DefaultClient, http.MethodGet, srv.URL+path, nil, nil); code != http.StatusNotFound {
		t.Fatalf("anon pre-guest GET = %d, want 404", code)
	}

	// Admin marks it guest-playable.
	if code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/files/"+hash+"/guest",
		map[string]any{"guest_playable": true}, nil); code != http.StatusOK {
		t.Fatalf("set guest = %d, want 200", code)
	}

	// Anonymous can now stream and browse it.
	if code := doJSON(t, http.DefaultClient, http.MethodGet, srv.URL+path, nil, nil); code != http.StatusOK {
		t.Errorf("anon post-guest GET = %d, want 200", code)
	}
	var artists []map[string]any
	doJSON(t, http.DefaultClient, http.MethodGet, srv.URL+"/api/artists", nil, &artists)
	if len(artists) != 1 {
		t.Errorf("anon /api/artists = %d, want 1", len(artists))
	}
}

// TestListings_AnonymousCannotBrowsePrivate is the regression test for the
// reported bug: a guest could not *play* a file but could still *see* it via the
// listing API. Every browse surface must be empty for an anonymous client when
// no file is guest-playable, and must reveal the file only once it is.
func TestListings_AnonymousCannotBrowsePrivate(t *testing.T) {
	srv, db := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	hash, path := uploadAndHash(t, admin, srv.URL, "private.mp3")

	// The fixture upload carries no tags, so it groups under the unknown
	// artist/album buckets. The id-addressed album listing is reached via that
	// bucket's artist id (resolved unfiltered through the DB).
	artists, err := db.ListArtists(context.Background())
	if err != nil || len(artists) != 1 {
		t.Fatalf("ListArtists: %d artists, err=%v", len(artists), err)
	}
	albumsURL := "/api/albums?artist_id=" + strconv.FormatInt(artists[0].ID, 10)
	// /api/files now returns a paginated envelope, so it is checked separately
	// via getFileItems; the others are bare arrays.
	listings := []string{
		"/api/artists",
		albumsURL,
	}

	// Anonymous: no file is guest-playable, so every listing is empty and the
	// blob itself is 404 (existence not revealed).
	if n := len(getFileItems(t, http.DefaultClient, srv.URL+"/api/files")); n != 0 {
		t.Errorf("anon /api/files = %d items, want 0 (private file leaked)", n)
	}
	for _, url := range listings {
		var items []map[string]any
		doJSON(t, http.DefaultClient, http.MethodGet, srv.URL+url, nil, &items)
		if len(items) != 0 {
			t.Errorf("anon GET %s = %d items, want 0 (private file leaked)", url, len(items))
		}
	}
	if code := doJSON(t, http.DefaultClient, http.MethodGet, srv.URL+path, nil, nil); code != http.StatusNotFound {
		t.Errorf("anon stream of private file = %d, want 404", code)
	}

	// Admin (content.access) always sees the full library.
	if n := len(getFileItems(t, admin, srv.URL+"/api/files")); n != 1 {
		t.Errorf("admin /api/files = %d, want 1", n)
	}

	// Once guest-playable, the same anonymous browse surfaces reveal it.
	if code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/files/"+hash+"/guest",
		map[string]any{"guest_playable": true}, nil); code != http.StatusOK {
		t.Fatalf("set guest = %d, want 200", code)
	}
	if n := len(getFileItems(t, http.DefaultClient, srv.URL+"/api/files")); n != 1 {
		t.Errorf("anon /api/files after guest flag = %d items, want 1", n)
	}
	for _, url := range listings {
		var items []map[string]any
		doJSON(t, http.DefaultClient, http.MethodGet, srv.URL+url, nil, &items)
		if len(items) != 1 {
			t.Errorf("anon GET %s after guest flag = %d items, want 1", url, len(items))
		}
	}
}

// insertTaggedFile inserts a file with real artist/album metadata directly
// through the store (the upload fixture carries no tags, so it can't populate
// the artist/album buckets /api/tracks requires).
func insertTaggedFile(t *testing.T, db *database.DB, hash, artist, album, title string) {
	t.Helper()
	f := &database.File{
		Hash: hash, ByteSize: 1, MimeType: "audio/mpeg", StorageBackend: "local",
		ObjectKey: hash + "/t.mp3", CreatedAt: 1700000000,
	}
	meta := &database.MediaMetadata{
		Title:       title,
		Artist:      sql.NullString{String: artist, Valid: true},
		Album:       sql.NullString{String: album, Valid: true},
		ExtractedAt: 1700000000,
	}
	if err := db.InsertFile(context.Background(), f,
		&database.FileUpload{Filename: "t.mp3", UploadedAt: 1700000000}, meta); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}
}

// insertGroupedFile seeds a file with the metadata the grouped sort orders on
// (album-artist / album / year / disc / track). A zero year/disc/track is stored
// as NULL (untagged), matching a real ingest with the tag absent.
func insertGroupedFile(t *testing.T, db *database.DB, hash, albumArtist, album, title string, year, disc, track int64) {
	t.Helper()
	nn := func(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: v != 0} }
	f := &database.File{
		Hash: hash, ByteSize: 1, MimeType: "audio/mpeg", StorageBackend: "local",
		ObjectKey: hash + "/t.mp3", CreatedAt: 1700000000,
	}
	meta := &database.MediaMetadata{
		Title:       title,
		AlbumArtist: sql.NullString{String: albumArtist, Valid: albumArtist != ""},
		Album:       sql.NullString{String: album, Valid: true},
		Year:        nn(year),
		TrackNumber: nn(track),
		DiscNumber:  nn(disc),
		ExtractedAt: 1700000000,
	}
	if err := db.InsertFile(context.Background(), f,
		&database.FileUpload{Filename: "t.mp3", UploadedAt: 1700000000}, meta); err != nil {
		t.Fatalf("InsertFile: %v", err)
	}
}

// TestListings_TrackFilteringOverHTTP exercises the /api/tracks access filter on
// the wire (handler guestListing + content.access bypass + the *Guest query),
// which the DB-only test cannot reach.
func TestListings_TrackFilteringOverHTTP(t *testing.T) {
	srv, db := newAuthTestServer(t)
	const hash = "aa11bb22cc33dd44ee55ff66aa77bb88cc99dd00ee11ff22aa33bb44cc55dd66"
	insertTaggedFile(t, db, hash, "An Artist", "An Album", "Track One")

	albumID, found, err := db.LookupAlbumID(context.Background(), "An Artist", "An Album")
	if err != nil || !found {
		t.Fatalf("LookupAlbumID: found=%v err=%v", found, err)
	}
	tracksURL := srv.URL + "/api/tracks?album_id=" + strconv.FormatInt(albumID, 10)

	count := func(client *http.Client) int {
		var tracks []map[string]any
		doJSON(t, client, http.MethodGet, tracksURL, nil, &tracks)
		return len(tracks)
	}

	// Anonymous: the private track is filtered out.
	if n := count(http.DefaultClient); n != 0 {
		t.Errorf("anon /api/tracks (private) = %d, want 0", n)
	}
	// Admin holds content.access and bypasses the filter entirely.
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	if n := count(admin); n != 1 {
		t.Errorf("admin /api/tracks (content.access) = %d, want 1", n)
	}
	// Once guest-playable, the anonymous track listing reveals it.
	if _, err := db.SetGuestPlayable(context.Background(), hash, true); err != nil {
		t.Fatalf("SetGuestPlayable: %v", err)
	}
	if n := count(http.DefaultClient); n != 1 {
		t.Errorf("anon /api/tracks after guest flag = %d, want 1", n)
	}
}

func TestManage_LicenseAutoDerive(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	hash, path := uploadAndHash(t, admin, srv.URL, "cc0.mp3")

	// Enable the auto-publish policy for CC0-1.0.
	if code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/settings/autoderive",
		map[string]any{"enabled": true, "licenses": []string{"CC0-1.0"}}, nil); code != http.StatusOK {
		t.Fatalf("set autoderive = %d, want 200", code)
	}

	// Setting a matching license grants guest access.
	if code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/files/"+hash+"/license",
		map[string]any{"license": "CC0-1.0"}, nil); code != http.StatusOK {
		t.Fatalf("set license = %d, want 200", code)
	}
	if code := doJSON(t, http.DefaultClient, http.MethodGet, srv.URL+path, nil, nil); code != http.StatusOK {
		t.Errorf("anon GET after auto-derive = %d, want 200", code)
	}

	// An unknown license is rejected.
	if code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/files/"+hash+"/license",
		map[string]any{"license": "bogus"}, nil); code != http.StatusBadRequest {
		t.Errorf("unknown license = %d, want 400", code)
	}
}
