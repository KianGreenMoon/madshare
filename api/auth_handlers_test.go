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

	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
)

const testAdminPassword = "admin-password-123"

// newAuthTestServer wires a router that mirrors madshare.go's buildHandler:
// the Identify middleware on every route, the api group, and the admin group
// gated by file.delete. It bootstraps an admin user and returns the server.
func newAuthTestServer(t *testing.T) (*httptest.Server, *database.DB) {
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
	if !contains(me.Permissions, auth.PermFileDelete) || !contains(me.Permissions, auth.PermContentAll) {
		t.Errorf("admin permissions = %v, missing file.delete/content.all", me.Permissions)
	}
	if !me.PasswordChangeRequired {
		t.Error("bootstrap admin should require a password change")
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
