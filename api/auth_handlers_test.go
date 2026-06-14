package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
)

const testAdminPassword = "admin-password-123"

// newAuthTestServer wires a router that mirrors madshare.go's buildHandler:
// the Identify middleware on every route, the api group, and the admin group
// gated by file.delete. It bootstraps an admin user and returns the server.
// newAuthTestServer starts a server whose bootstrap admin has already completed
// the forced first-run password change, so the capability tests can act as
// admin directly. Server-side enforcement of the change-required flag is
// covered by auth/middleware_test.go and TestAuth_BootstrapRequiresPasswordChange.
func newAuthTestServer(t *testing.T) (*httptest.Server, *database.DB) {
	t.Helper()
	srv, db := newAuthTestServerRaw(t)
	if _, err := db.Exec(`UPDATE users SET password_change_required = 0 WHERE username = 'admin'`); err != nil {
		t.Fatalf("clear admin change-required: %v", err)
	}
	return srv, db
}

// newAuthTestServerRaw is newAuthTestServer without clearing the bootstrap
// admin's forced-change flag — i.e. a fresh, just-bootstrapped server. Used by
// the test that asserts the forced password change is actually enforced.
func newAuthTestServerRaw(t *testing.T) (*httptest.Server, *database.DB) {
	t.Helper()
	dir := t.TempDir()
	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	created, err := auth.Bootstrap(context.Background(), db, "admin", testAdminPassword)
	if err != nil || !created {
		t.Fatalf("bootstrap admin: created=%v err=%v", created, err)
	}

	// With Auth set, RegisterAPI/RegisterAdmin gate the protected routes
	// themselves (see Deps.protect); the test only needs Identify in the chain.
	deps := Deps{Store: storage.NewLocal(dir), Repo: db, CacheDir: t.TempDir(), FilesDir: dir, MaxUploadSize: testMaxUpload, Auth: db, Manage: db}
	r := chi.NewRouter()
	r.Use(auth.Identify(db))
	RegisterAPI(r, deps)
	RegisterAdmin(r, deps)

	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, db
}

// makeUser creates a user with the given password and role and returns its id.
func makeUser(t *testing.T, db *database.DB, username, password, role string) int64 {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	id, err := db.CreateUser(context.Background(), username, hash, false)
	if err != nil {
		t.Fatalf("create user %s: %v", username, err)
	}
	if err := db.AssignRoleByName(context.Background(), id, role); err != nil {
		t.Fatalf("assign role %s: %v", role, err)
	}
	return id
}

// makeRestrictedRole creates a non-built-in role that lacks content.access, and
// returns its name. The built-in listener/uploader roles hold content.access
// (full-library), so grant-based access tests use this role to exercise the
// Layer-B access-group machinery for users who are deliberately not full-library.
func makeRestrictedRole(t *testing.T, db *database.DB) string {
	t.Helper()
	const name = "restricted"
	if _, err := db.Exec(`INSERT INTO roles (name, built_in) VALUES (?, 0)`, name); err != nil {
		t.Fatalf("create restricted role: %v", err)
	}
	return name
}

func login(t *testing.T, client *http.Client, base, user, pass string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": user, "password": pass})
	resp, err := client.Post(base+"/api/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login request: %v", err)
	}
	return resp
}

func TestAuth_LoginMeLogout(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	// Wrong password -> 401.
	if resp := login(t, client, srv.URL, "admin", "wrong"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad login status = %d, want 401", resp.StatusCode)
	}

	// Correct login -> 200 and a session cookie.
	resp := login(t, client, srv.URL, "admin", testAdminPassword)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	if len(jar.Cookies(mustURL(t, srv.URL))) == 0 {
		t.Fatal("no session cookie set after login")
	}

	// /me with the cookie -> 200 and admin permissions.
	meResp, err := client.Get(srv.URL + "/api/auth/me")
	if err != nil {
		t.Fatalf("me: %v", err)
	}
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("me status = %d, want 200", meResp.StatusCode)
	}
	var me identityJSON
	json.NewDecoder(meResp.Body).Decode(&me)
	meResp.Body.Close()
	if me.Username != "admin" {
		t.Errorf("me.username = %q, want admin", me.Username)
	}
	if !contains(me.Permissions, auth.PermFileDelete) || !contains(me.Permissions, auth.PermContentAccess) {
		t.Errorf("admin permissions = %v, missing file.delete/content.access", me.Permissions)
	}

	// Logout, then /me must be anonymous again.
	logoutResp, _ := client.Post(srv.URL+"/api/auth/logout", "", nil)
	logoutResp.Body.Close()
	meResp2, _ := client.Get(srv.URL + "/api/auth/me")
	if meResp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("me after logout = %d, want 401", meResp2.StatusCode)
	}
	meResp2.Body.Close()
}

// TestAuth_BootstrapRequiresPasswordChange asserts the forced first-run change
// is enforced, not merely advertised: a freshly bootstrapped admin sees the
// flag on /me, and any capability-gated action (or minting a token) is refused
// with 403 + the X-Password-Change-Required marker until the change is done.
func TestAuth_BootstrapRequiresPasswordChange(t *testing.T) {
	srv, _ := newAuthTestServerRaw(t)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	if resp := login(t, client, srv.URL, "admin", testAdminPassword); resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d, want 200", resp.StatusCode)
	}

	var me identityJSON
	meResp, _ := client.Get(srv.URL + "/api/auth/me")
	json.NewDecoder(meResp.Body).Decode(&me)
	meResp.Body.Close()
	if !me.PasswordChangeRequired {
		t.Error("bootstrap admin should require a password change")
	}

	// A capability-gated admin action is refused while the change is pending.
	if code := doJSON(t, client, http.MethodDelete, srv.URL+"/api/admin/users/999", nil, nil); code != http.StatusForbidden {
		t.Errorf("gated action while change-required = %d, want 403", code)
	}
	// Minting a token (a self-checking endpoint) is refused too.
	if code := doJSON(t, client, http.MethodPost, srv.URL+"/api/auth/tokens",
		map[string]any{"name": "x"}, nil); code != http.StatusForbidden {
		t.Errorf("create token while change-required = %d, want 403", code)
	}

	// Changing the password clears the flag; the gated action then passes the
	// password gate (404 = past it, the user 999 just doesn't exist).
	if code := doJSON(t, client, http.MethodPost, srv.URL+"/api/auth/password",
		map[string]any{"old_password": testAdminPassword, "new_password": "new-admin-pass-456"}, nil); code != http.StatusNoContent {
		t.Fatalf("change password = %d, want 204", code)
	}
	if code := doJSON(t, client, http.MethodDelete, srv.URL+"/api/admin/users/999", nil, nil); code == http.StatusForbidden {
		t.Errorf("gated action after change = 403, want past the gate")
	}
}

func TestAuth_MeAnonymous(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	resp, _ := http.Get(srv.URL + "/api/auth/me")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous /me = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAuth_AdminGate(t *testing.T) {
	srv, _ := newAuthTestServer(t)

	// Anonymous delete -> 401 (gated).
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/admin/files/deadbeef", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous admin delete = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Authenticated admin -> passes the gate (404 for the missing hash, not 401/403).
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	login(t, client, srv.URL, "admin", testAdminPassword).Body.Close()
	req2, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/admin/files/deadbeef", nil)
	resp2, _ := client.Do(req2)
	if resp2.StatusCode == http.StatusUnauthorized || resp2.StatusCode == http.StatusForbidden {
		t.Errorf("admin delete (authed) = %d, want past the gate (e.g. 404)", resp2.StatusCode)
	}
	resp2.Body.Close()
}

// TestUpdateFileMetadata_Gated verifies PATCH /api/files/{hash}/metadata requires
// metadata.edit: anonymous -> 401, an uploader (no metadata.edit) -> 403, and an
// admin -> 200 with the edited tags persisted.
func TestUpdateFileMetadata_Gated(t *testing.T) {
	srv, db := newAuthTestServer(t)
	ctx := context.Background()

	hash := "ed17000000000000000000000000000000000000000000000000000000000000"
	f := &database.File{
		Hash: hash, ByteSize: 1, MimeType: "audio/mpeg",
		StorageBackend: "local", ObjectKey: hash + "/s.mp3", CreatedAt: 1,
	}
	meta := &database.MediaMetadata{ExtractedAt: 1}
	meta.Album.String, meta.Album.Valid = "Old Album", true
	if err := db.InsertFile(ctx, f, &database.FileUpload{Filename: "s.mp3", UploadedAt: 1}, meta); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	makeUser(t, db, "up", testAdminPassword, "uploader") // lacks metadata.edit

	patch := func(client *http.Client) *http.Response {
		body, _ := json.Marshal(map[string]any{"album": "New Album", "album_artist": "New Artist"})
		req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/api/files/"+hash+"/metadata", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("patch request: %v", err)
		}
		return resp
	}

	// Anonymous -> 401.
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/api/files/"+hash+"/metadata", bytes.NewReader([]byte(`{"album":"x"}`)))
	req.Header.Set("Content-Type", "application/json")
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous patch = %d, want 401", resp.StatusCode)
	}

	// Uploader (no metadata.edit) -> 403.
	jarUp, _ := cookiejar.New(nil)
	cUp := &http.Client{Jar: jarUp}
	login(t, cUp, srv.URL, "up", testAdminPassword).Body.Close()
	if resp := patch(cUp); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("uploader patch = %d, want 403", resp.StatusCode)
	}

	// Admin (has metadata.edit) -> 200 and persisted.
	jarAd, _ := cookiejar.New(nil)
	cAd := &http.Client{Jar: jarAd}
	login(t, cAd, srv.URL, "admin", testAdminPassword).Body.Close()
	resp := patch(cAd)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin patch = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got["album"] != "New Album" || got["album_artist"] != "New Artist" {
		t.Errorf("response = %v, want album=New Album album_artist=New Artist", got)
	}
	back, err := db.UpdateFileMetadata(ctx, hash, database.MetadataPatch{})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if back.Album.String != "New Album" || back.AlbumArtist.String != "New Artist" {
		t.Errorf("persisted = (%q,%q), want (New Album,New Artist)", back.Album.String, back.AlbumArtist.String)
	}
}

func TestAuth_TokenFlow(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	login(t, client, srv.URL, "admin", testAdminPassword).Body.Close()

	// Create a token.
	body, _ := json.Marshal(map[string]any{"name": "cli"})
	resp, _ := client.Post(srv.URL+"/api/auth/tokens", "application/json", bytes.NewReader(body))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create token = %d, want 201", resp.StatusCode)
	}
	var created struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.Token == "" {
		t.Fatal("created token is empty")
	}

	// Use the token as a Bearer credential (no cookie) to reach /me.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+created.Token)
	meResp, _ := http.DefaultClient.Do(req)
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("bearer /me = %d, want 200", meResp.StatusCode)
	}
	meResp.Body.Close()

	// Revoke it; the Bearer credential must stop working.
	delReq, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/auth/tokens/"+itoa(created.ID), nil)
	delResp, _ := client.Do(delReq)
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke = %d, want 204", delResp.StatusCode)
	}
	delResp.Body.Close()
	req2, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/auth/me", nil)
	req2.Header.Set("Authorization", "Bearer "+created.Token)
	meResp2, _ := http.DefaultClient.Do(req2)
	if meResp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("revoked bearer /me = %d, want 401", meResp2.StatusCode)
	}
	meResp2.Body.Close()
}

// TestAuth_TokenExpiresAt covers the absolute-expiry branch the web UI's date
// picker uses: a past timestamp is rejected, a future one is accepted and
// surfaced by the list endpoint.
func TestAuth_TokenExpiresAt(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	login(t, client, srv.URL, "admin", testAdminPassword).Body.Close()

	// A past expiry is rejected.
	past, _ := json.Marshal(map[string]any{"name": "stale", "expires_at": time.Now().Add(-time.Hour).Unix()})
	resp, _ := client.Post(srv.URL+"/api/auth/tokens", "application/json", bytes.NewReader(past))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("past expires_at = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	// A future expiry is accepted and echoed back by the list endpoint.
	future := time.Now().Add(48 * time.Hour).Unix()
	body, _ := json.Marshal(map[string]any{"name": "dated", "expires_at": future})
	resp2, _ := client.Post(srv.URL+"/api/auth/tokens", "application/json", bytes.NewReader(body))
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("future expires_at = %d, want 201", resp2.StatusCode)
	}
	resp2.Body.Close()

	listResp, _ := client.Get(srv.URL + "/api/auth/tokens")
	var tokens []struct {
		Name      string `json:"name"`
		ExpiresAt int64  `json:"expires_at"`
	}
	json.NewDecoder(listResp.Body).Decode(&tokens)
	listResp.Body.Close()
	var found bool
	for _, tk := range tokens {
		if tk.Name == "dated" {
			found = true
			if tk.ExpiresAt != future {
				t.Errorf("listed expires_at = %d, want %d", tk.ExpiresAt, future)
			}
		}
	}
	if !found {
		t.Error("created token not present in list")
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
