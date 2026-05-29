package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"daemonlord.ygg/madshare/auth"
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

// uploadAndHash uploads a file as the given client and returns its content hash
// and download path (/files/<hash>/<filename>).
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
	return body.Hash, "/files/" + body.Hash + "/" + body.Filename
}

func TestManage_GatedByUserManage(t *testing.T) {
	srv, db := newAuthTestServer(t)
	makeUser(t, db, "lis", "listener-pass-1", auth.RoleListener)

	// Anonymous -> 401.
	if code := doJSON(t, http.DefaultClient, http.MethodPost, srv.URL+"/api/admin/access/groups",
		map[string]any{"name": "x"}, nil); code != http.StatusUnauthorized {
		t.Errorf("anon create group = %d, want 401", code)
	}

	// Listener lacks user.manage -> 403.
	lis := clientFor(t, srv.URL, "lis", "listener-pass-1")
	if code := doJSON(t, lis, http.MethodPost, srv.URL+"/api/admin/access/groups",
		map[string]any{"name": "x"}, nil); code != http.StatusForbidden {
		t.Errorf("listener create group = %d, want 403", code)
	}

	// Admin holds user.manage -> 201.
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	if code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/access/groups",
		map[string]any{"name": "friends"}, nil); code != http.StatusCreated {
		t.Errorf("admin create group = %d, want 201", code)
	}
}

func TestManage_GrantUnlocksPlayback(t *testing.T) {
	srv, db := newAuthTestServer(t)
	lisID := makeUser(t, db, "lis", "listener-pass-1", auth.RoleListener)

	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	hash, path := uploadAndHash(t, admin, srv.URL, "song.mp3")

	lis := clientFor(t, srv.URL, "lis", "listener-pass-1")

	// Before any grant: the listener cannot stream (404) nor see the file.
	if code := doJSON(t, lis, http.MethodGet, srv.URL+path, nil, nil); code != http.StatusNotFound {
		t.Fatalf("listener pre-grant GET = %d, want 404", code)
	}
	var files []map[string]any
	doJSON(t, lis, http.MethodGet, srv.URL+"/api/files", nil, &files)
	if len(files) != 0 {
		t.Fatalf("listener pre-grant /api/files = %d, want 0", len(files))
	}

	// Admin creates a group, adds the listener, grants the whole library.
	var grp struct {
		ID int64 `json:"id"`
	}
	if code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/access/groups",
		map[string]any{"name": "all"}, &grp); code != http.StatusCreated {
		t.Fatalf("create group = %d", code)
	}
	if code := doJSON(t, admin, http.MethodPost,
		srv.URL+"/api/admin/access/groups/1/members", map[string]any{"user_id": lisID}, nil); code != http.StatusNoContent {
		t.Fatalf("add member = %d, want 204", code)
	}
	if code := doJSON(t, admin, http.MethodPost,
		srv.URL+"/api/admin/access/groups/1/grants", map[string]any{"scope_type": "all"}, nil); code != http.StatusCreated {
		t.Fatalf("add grant = %d, want 201", code)
	}

	// After the grant: the listener can stream and see the file.
	if code := doJSON(t, lis, http.MethodGet, srv.URL+path, nil, nil); code != http.StatusOK {
		t.Errorf("listener post-grant GET = %d, want 200", code)
	}
	files = nil
	doJSON(t, lis, http.MethodGet, srv.URL+"/api/files", nil, &files)
	if len(files) != 1 || files[0]["hash"] != hash {
		t.Errorf("listener post-grant /api/files = %v, want the one file", files)
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

func TestManage_LicenseAutoDerive(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	hash, path := uploadAndHash(t, admin, srv.URL, "cc0.mp3")

	// Enable the auto-publish policy for CC0-1.0.
	if code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/settings/autoderive",
		map[string]any{"enabled": true, "licenses": []string{"CC0-1.0"}, "apply_now": true}, nil); code != http.StatusOK {
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
