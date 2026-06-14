package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// run executes mw-wrapped handler against a request carrying id and reports
// whether next ran plus the recorded response.
func run(t *testing.T, mw func(http.Handler) http.Handler, id *Identity) (called bool, rec *httptest.ResponseRecorder) {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodPost, "/x", nil)
	req = req.WithContext(WithIdentity(req.Context(), id))
	rec = httptest.NewRecorder()
	mw(next).ServeHTTP(rec, req)
	return called, rec
}

func TestRequirePermission_PasswordChangeRequiredBlocks(t *testing.T) {
	// Holds the permission, but is flagged for a forced password change.
	id := &Identity{
		UserID:                 1,
		Username:               "alice",
		Permissions:            map[string]bool{PermFileUpload: true},
		PasswordChangeRequired: true,
	}
	called, rec := run(t, RequirePermission(PermFileUpload), id)

	if called {
		t.Fatal("next must not run while a password change is required")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if rec.Header().Get("X-Password-Change-Required") != "1" {
		t.Fatal("missing X-Password-Change-Required marker header")
	}
}

func TestRequireAnyPermission_PasswordChangeRequiredBlocks(t *testing.T) {
	id := &Identity{
		Permissions:            map[string]bool{PermFileUpload: true},
		PasswordChangeRequired: true,
	}
	called, rec := run(t, RequireAnyPermission(PermMetadataEdit, PermFileUpload), id)

	if called {
		t.Fatal("next must not run while a password change is required")
	}
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestRequirePermission_AllowsWhenNotFlagged(t *testing.T) {
	id := &Identity{Permissions: map[string]bool{PermFileUpload: true}}
	called, rec := run(t, RequirePermission(PermFileUpload), id)

	if !called {
		t.Fatal("next should run for a permitted, unflagged identity")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}
