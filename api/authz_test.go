package api

import (
	"net/http"
	"net/http/cookiejar"
	"testing"

	"daemonlord.ygg/madshare/auth"
)

// uploadViaClient posts a small audio upload through the test server using the
// given client (carrying any session cookie).
func uploadViaClient(t *testing.T, client *http.Client, base, name string) *http.Response {
	t.Helper()
	req := buildUploadRequest(t, "file", name, "audio/mpeg", []byte("ID3-ish bytes that need no real tags"))
	resp, err := client.Post(base+"/files/upload", req.Header.Get("Content-Type"), req.Body)
	if err != nil {
		t.Fatalf("upload request: %v", err)
	}
	return resp
}

func clientFor(t *testing.T, base, user, pass string) *http.Client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}
	login(t, c, base, user, pass).Body.Close()
	return c
}

func TestAuthz_UploadRequiresPermission(t *testing.T) {
	srv, db := newAuthTestServer(t)
	uploaderID := makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)
	makeUser(t, db, "lis", "listener-pass-1", auth.RoleListener)

	// Anonymous -> 401.
	if resp := uploadViaClient(t, http.DefaultClient, srv.URL, "anon.mp3"); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous upload = %d, want 401", resp.StatusCode)
		resp.Body.Close()
	}

	// Listener lacks file.upload -> 403.
	lis := clientFor(t, srv.URL, "lis", "listener-pass-1")
	if resp := uploadViaClient(t, lis, srv.URL, "listener.mp3"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("listener upload = %d, want 403", resp.StatusCode)
		resp.Body.Close()
	}

	// Uploader can upload -> 201, and uploaded_by is recorded.
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")
	resp := uploadViaClient(t, up, srv.URL, "ok.mp3")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("uploader upload = %d, want 201", resp.StatusCode)
	}
	resp.Body.Close()

	var got int64
	if err := db.QueryRow(`SELECT uploaded_by FROM files LIMIT 1`).Scan(&got); err != nil {
		t.Fatalf("read uploaded_by: %v", err)
	}
	if got != uploaderID {
		t.Errorf("uploaded_by = %d, want %d (the uploader)", got, uploaderID)
	}
}

func TestAuthz_AdminDeleteRequiresFileDelete(t *testing.T) {
	srv, db := newAuthTestServer(t)
	makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)

	// Uploader has no file.delete -> 403.
	up := clientFor(t, srv.URL, "up", "uploader-pass-1")
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/admin/files/deadbeef", nil)
	resp, _ := up.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("uploader delete = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestAudit_UploadRecorded(t *testing.T) {
	srv, db := newAuthTestServer(t)
	uploaderID := makeUser(t, db, "up", "uploader-pass-1", auth.RoleUploader)

	up := clientFor(t, srv.URL, "up", "uploader-pass-1")
	uploadViaClient(t, up, srv.URL, "audited.mp3").Body.Close()

	var actor int64
	var action string
	err := db.QueryRow(
		`SELECT actor_user_id, action FROM audit_log WHERE action = 'file.upload' ORDER BY id DESC LIMIT 1`).
		Scan(&actor, &action)
	if err != nil {
		t.Fatalf("read audit_log: %v", err)
	}
	if actor != uploaderID {
		t.Errorf("audit actor = %d, want %d", actor, uploaderID)
	}
}
