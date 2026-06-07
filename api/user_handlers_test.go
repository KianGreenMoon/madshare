package api

import (
	"fmt"
	"net/http"
	"testing"

	"daemonlord.ygg/madshare/auth"
)

// TestUsers_GatedByUserManage verifies the user-admin endpoints require
// user.manage: anonymous → 401, a listener → 403, an admin → success.
func TestUsers_GatedByUserManage(t *testing.T) {
	srv, db := newAuthTestServer(t)
	makeUser(t, db, "lis", "listener-pass-1", auth.RoleListener)

	body := map[string]any{"username": "newbie", "password": "newbie-pass-1"}

	if code := doJSON(t, http.DefaultClient, http.MethodPost, srv.URL+"/api/admin/users", body, nil); code != http.StatusUnauthorized {
		t.Errorf("anon create user = %d, want 401", code)
	}

	lis := clientFor(t, srv.URL, "lis", "listener-pass-1")
	if code := doJSON(t, lis, http.MethodPost, srv.URL+"/api/admin/users", body, nil); code != http.StatusForbidden {
		t.Errorf("listener create user = %d, want 403", code)
	}

	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	if code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/users", body, nil); code != http.StatusCreated {
		t.Errorf("admin create user = %d, want 201", code)
	}
}

// TestUsers_CreateListenerCanPlay covers the headline use case: an admin creates
// a regular listener account, and that user can authenticate. The default role
// (no roles given) is "listener" = content.access.
func TestUsers_CreateListenerCanPlay(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)

	var created struct {
		ID    int64    `json:"id"`
		Roles []string `json:"roles"`
	}
	code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/users",
		map[string]any{"username": "music_fan", "password": "play-and-download"}, &created)
	if code != http.StatusCreated {
		t.Fatalf("create listener = %d, want 201", code)
	}
	if len(created.Roles) != 1 || created.Roles[0] != auth.RoleListener {
		t.Fatalf("default roles = %v, want [listener]", created.Roles)
	}

	// The new user can log in, and /me reports the play/download permissions.
	fan := clientFor(t, srv.URL, "music_fan", "play-and-download")
	var me struct {
		Username    string   `json:"username"`
		Permissions []string `json:"permissions"`
	}
	if code := doJSON(t, fan, http.MethodGet, srv.URL+"/api/auth/me", nil, &me); code != http.StatusOK {
		t.Fatalf("new user /me = %d, want 200", code)
	}
	if me.Username != "music_fan" {
		t.Errorf("me.username = %q, want music_fan", me.Username)
	}
	// listener holds content.access: a logged-in listener sees and can play the
	// whole library (full-library access under the roles-only model).
	if !hasRole(me.Permissions, auth.PermContentAccess) {
		t.Errorf("listener perms = %v, want content.access (full library)", me.Permissions)
	}
}

// TestUsers_Validation rejects short passwords, bad usernames, duplicates, and
// unknown roles.
func TestUsers_Validation(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)

	cases := []struct {
		name string
		body map[string]any
		want int
	}{
		{"short password", map[string]any{"username": "ok_name", "password": "short"}, http.StatusBadRequest},
		{"bad username", map[string]any{"username": "no spaces!", "password": "long-enough-1"}, http.StatusBadRequest},
		{"unknown role", map[string]any{"username": "ok_name", "password": "long-enough-1", "roles": []string{"wizard"}}, http.StatusBadRequest},
		{"duplicate admin", map[string]any{"username": "admin", "password": "long-enough-1"}, http.StatusConflict},
	}
	for _, tc := range cases {
		if code := doJSON(t, admin, http.MethodPost, srv.URL+"/api/admin/users", tc.body, nil); code != tc.want {
			t.Errorf("%s: create = %d, want %d", tc.name, code, tc.want)
		}
	}
}

// TestUsers_EditDisableAndRoles changes a user's roles and disabled state.
func TestUsers_EditDisableAndRoles(t *testing.T) {
	srv, db := newAuthTestServer(t)
	id := makeUser(t, db, "lis", "listener-pass-1", auth.RoleListener)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)
	userURL := fmt.Sprintf("%s/api/admin/users/%d", srv.URL, id)

	// Promote to uploader.
	if code := doJSON(t, admin, http.MethodPatch, userURL,
		map[string]any{"roles": []string{auth.RoleUploader}}, nil); code != http.StatusOK {
		t.Fatalf("set roles = %d, want 200", code)
	}
	roles, _ := db.AllUserRoles(t.Context())
	if !hasRole(roles[id], auth.RoleUploader) || hasRole(roles[id], auth.RoleListener) {
		t.Errorf("roles after promote = %v, want [uploader]", roles[id])
	}

	// Disable: the user can no longer log in.
	if code := doJSON(t, admin, http.MethodPatch, userURL,
		map[string]any{"disabled": true}, nil); code != http.StatusOK {
		t.Fatalf("disable = %d, want 200", code)
	}
	if resp := login(t, &http.Client{}, srv.URL, "lis", "listener-pass-1"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("disabled user login = %d, want 401", resp.StatusCode)
	}
}

// TestUsers_LastAdminProtected ensures the last admin cannot be deleted,
// disabled, or demoted, and that an admin cannot delete their own account.
func TestUsers_LastAdminProtected(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)

	// The bootstrap admin is user id 1 and the only admin.
	if code := doJSON(t, admin, http.MethodDelete, srv.URL+"/api/admin/users/1", nil, nil); code != http.StatusBadRequest {
		t.Errorf("delete last admin = %d, want 400", code)
	}
	if code := doJSON(t, admin, http.MethodPatch, srv.URL+"/api/admin/users/1",
		map[string]any{"disabled": true}, nil); code != http.StatusBadRequest {
		t.Errorf("disable last admin = %d, want 400", code)
	}
	if code := doJSON(t, admin, http.MethodPatch, srv.URL+"/api/admin/users/1",
		map[string]any{"roles": []string{auth.RoleListener}}, nil); code != http.StatusBadRequest {
		t.Errorf("demote last admin = %d, want 400", code)
	}
}

// TestUsers_DeleteRemovesAccount deletes a user and confirms they can no longer
// authenticate.
func TestUsers_DeleteRemovesAccount(t *testing.T) {
	srv, db := newAuthTestServer(t)
	id := makeUser(t, db, "temp", "temp-pass-123", auth.RoleListener)
	admin := clientFor(t, srv.URL, "admin", testAdminPassword)

	userURL := fmt.Sprintf("%s/api/admin/users/%d", srv.URL, id)
	if code := doJSON(t, admin, http.MethodDelete, userURL, nil, nil); code != http.StatusNoContent {
		t.Fatalf("delete user = %d, want 204", code)
	}
	if resp := login(t, &http.Client{}, srv.URL, "temp", "temp-pass-123"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("deleted user login = %d, want 401", resp.StatusCode)
	}
}
