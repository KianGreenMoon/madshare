package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/auth"
)

// Listener-node token issuance (federation F7 item 9). The signing itself is the
// federation package's to test; what is under test here is the mapping the API
// owns — who may ask, what the request must look like, and the one claim the
// handler decides rather than passes through: the guest bit.

// tokenServer mounts the API with an injected identity, since the whole point of
// the endpoint is that the caller is an authenticated *account* rather than a
// friended node.
func tokenServer(t *testing.T, fake *fakeFederation, id *auth.Identity) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithIdentity(req.Context(), id)))
		})
	})
	RegisterAPI(r, Deps{Madnetwork: &fakeMadnetwork{}, Federation: fake})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func postToken(t *testing.T, srv *httptest.Server, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+"/api/madnetwork/token", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestIssueCapabilityTokenCarriesTheAccountACL is the rule the handler owns: the
// caller's own rights travel with the device, so a user who may not reach the
// whole library here cannot reach it by asking from a phone instead.
func TestIssueCapabilityTokenCarriesTheAccountACL(t *testing.T) {
	bearer := strings.Repeat("ab", 32)

	for _, tc := range []struct {
		name          string
		perms         map[string]bool
		wantGuestOnly bool
	}{
		{"full account", map[string]bool{auth.PermContentAccess: true}, false},
		{"restricted account", map[string]bool{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeFederation{patched: map[string]any{}}
			srv := tokenServer(t, fake, &auth.Identity{UserID: 1, Username: "kian", Permissions: tc.perms})

			resp := postToken(t, srv, `{"node_key":"`+bearer+`"}`)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("issue = %d, want 200", resp.StatusCode)
			}
			var grant struct {
				Token  string `json:"token"`
				Bearer string `json:"bearer"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&grant); err != nil {
				t.Fatal(err)
			}
			if grant.Token == "" || grant.Bearer != bearer {
				t.Errorf("grant = %+v, want a token for bearer %s", grant, bearer)
			}
			if fake.tokenBearer != bearer {
				t.Errorf("signed for bearer %q, want %q", fake.tokenBearer, bearer)
			}
			if fake.tokenGuestOnly != tc.wantGuestOnly {
				t.Errorf("guest_only = %v, want %v", fake.tokenGuestOnly, tc.wantGuestOnly)
			}
		})
	}
}

// TestIssueCapabilityTokenRejectsBadRequests: a node key is the only input, and
// it is checked before anything is signed.
func TestIssueCapabilityTokenRejectsBadRequests(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"not hex", `{"node_key":"nonsense"}`, http.StatusBadRequest},
		{"too short", `{"node_key":"abcd"}`, http.StatusBadRequest},
		{"missing", `{}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeFederation{patched: map[string]any{}}
			srv := tokenServer(t, fake, &auth.Identity{UserID: 1, Username: "kian"})
			if got := postToken(t, srv, tc.body).StatusCode; got != tc.want {
				t.Errorf("issue = %d, want %d", got, tc.want)
			}
			if fake.tokenBearer != "" {
				t.Errorf("signed a token for %q despite a bad request", fake.tokenBearer)
			}
		})
	}
}

// TestIssueCapabilityTokenNeedsAnAccount: there is no anonymous vouch. The
// endpoint's entire premise is that a home server authenticated this person
// itself.
func TestIssueCapabilityTokenNeedsAnAccount(t *testing.T) {
	fake := &fakeFederation{patched: map[string]any{}}
	srv := tokenServer(t, fake, nil)
	if got := postToken(t, srv, `{"node_key":"`+strings.Repeat("ab", 32)+`"}`).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("issue without an identity = %d, want 401", got)
	}
	if fake.tokenBearer != "" {
		t.Error("signed a token for an anonymous caller")
	}
}
