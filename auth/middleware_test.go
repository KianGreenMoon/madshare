package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeStore drives Identify: a configurable session/token lookup result.
type fakeStore struct {
	id  *Identity
	err error
}

func (s fakeStore) SessionIdentity(context.Context, string) (*Identity, error) {
	return s.id, s.err
}
func (s fakeStore) TokenIdentity(context.Context, string) (*Identity, error) {
	return s.id, s.err
}

// runIdentify wraps a next handler with Identify and a request carrying a session
// cookie, returning whether next ran (and with which identity) plus the response.
func runIdentify(t *testing.T, store Store) (ran bool, gotID *Identity, rec *httptest.ResponseRecorder) {
	t.Helper()
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		gotID = FromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "some-token"})
	rec = httptest.NewRecorder()
	Identify(store)(next).ServeHTTP(rec, req)
	return ran, gotID, rec
}

// A transient store error on a presented credential must fail closed (503), not
// silently downgrade the request to anonymous.
func TestIdentify_StoreErrorFailsClosed(t *testing.T) {
	ran, _, rec := runIdentify(t, fakeStore{err: errors.New("database is locked")})
	if ran {
		t.Fatal("next must not run when the session lookup errored")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// An unknown/expired credential (nil, nil) is anonymous, not an error.
func TestIdentify_UnknownCredentialIsAnonymous(t *testing.T) {
	ran, gotID, rec := runIdentify(t, fakeStore{})
	if !ran || rec.Code != http.StatusOK {
		t.Fatalf("next ran=%v status=%d, want true/200", ran, rec.Code)
	}
	if gotID != nil {
		t.Errorf("identity = %+v, want anonymous (nil)", gotID)
	}
}

// A resolving credential reaches next as that identity.
func TestIdentify_ResolvedIdentityPassesThrough(t *testing.T) {
	want := &Identity{UserID: 7, Username: "alice"}
	ran, gotID, rec := runIdentify(t, fakeStore{id: want})
	if !ran || rec.Code != http.StatusOK {
		t.Fatalf("next ran=%v status=%d, want true/200", ran, rec.Code)
	}
	if gotID == nil || gotID.UserID != 7 {
		t.Errorf("identity = %+v, want UserID 7", gotID)
	}
}

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
