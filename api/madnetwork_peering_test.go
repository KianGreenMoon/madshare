package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/auth"
)

// Peering info (docs/architecture/federation-access.md §"The household", "Getting onto
// the mesh at all"). What the handler owns is the rewrite — a bind address is
// not a dial address — and the two refusals that must stay distinguishable.

func peeringServer(t *testing.T, p *Peering) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(
				auth.WithIdentity(req.Context(), &auth.Identity{UserID: 1, Username: "kian"})))
		})
	})
	RegisterAPI(r, Deps{Madnetwork: &fakeMadnetwork{}, Federation: &fakeFederation{}, Peering: p})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func getPeering(t *testing.T, srv *httptest.Server) (*http.Response, map[string][]string) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/madnetwork/peering")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	body := map[string][]string{}
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
	}
	return resp, body
}

// TestPeeringRewritesAWildcardBind is the point of the endpoint doing anything
// at all rather than echoing config: listen = ["tls://0.0.0.0:12345"] is a
// correct bind and a useless address. The host the caller just reached us on is
// by construction one that reaches us.
func TestPeeringRewritesAWildcardBind(t *testing.T) {
	srv := peeringServer(t, &Peering{
		Share:  true,
		Peers:  []string{"tls://upstream.example:1"},
		Listen: []string{"tls://0.0.0.0:12345", "tls://backbone.example:99", "unix:///run/ygg.sock"},
	})
	resp, body := getPeering(t, srv)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("peering = %d, want 200", resp.StatusCode)
	}
	if len(body["peers"]) != 1 || body["peers"][0] != "tls://upstream.example:1" {
		t.Errorf("peers = %v, want the shared list verbatim", body["peers"])
	}
	// The httptest server is on 127.0.0.1:<port>; the wildcard bind should have
	// picked up that host and kept its OWN port, which is the listener's.
	listen := body["listen"]
	if len(listen) != 2 {
		t.Fatalf("listen = %v, want the two dialable entries (the unix socket is not one)", listen)
	}
	if listen[0] != "tls://127.0.0.1:12345" {
		t.Errorf("rewritten bind = %q, want tls://127.0.0.1:12345 — the caller's host, the listener's port", listen[0])
	}
	if listen[1] != "tls://backbone.example:99" {
		t.Errorf("explicit bind = %q, want it left alone", listen[1])
	}
}

// TestPeeringOffIsNotPeeringEmpty: two answers, two meanings. A 404 says this
// operator switched sharing off, so stop asking; an empty 200 says sharing is on
// and there is nothing to offer, which is true of a node that was itself only
// ever reached over the mesh.
func TestPeeringOffIsNotPeeringEmpty(t *testing.T) {
	if resp, _ := getPeering(t, peeringServer(t, &Peering{Share: false, Peers: []string{"tls://a:1"}})); resp.StatusCode != http.StatusNotFound {
		t.Errorf("sharing off = %d, want 404", resp.StatusCode)
	}
	if resp, _ := getPeering(t, peeringServer(t, nil)); resp.StatusCode != http.StatusNotFound {
		t.Errorf("no peering configured = %d, want 404", resp.StatusCode)
	}

	resp, body := getPeering(t, peeringServer(t, &Peering{Share: true}))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("sharing on with nothing to share = %d, want 200", resp.StatusCode)
	}
	if len(body["peers"]) != 0 || len(body["listen"]) != 0 {
		t.Errorf("body = %v, want both lists empty", body)
	}
}

// TestDialableListen covers the rewrite's edges directly, since a handler test
// can only exercise one request host at a time.
func TestDialableListen(t *testing.T) {
	for _, tc := range []struct {
		name, uri, host, want string
		ok                    bool
	}{
		{"wildcard v4", "tls://0.0.0.0:12345", "box.local:3000", "tls://box.local:12345", true},
		{"wildcard v6", "tls://[::]:12345", "box.local:3000", "tls://box.local:12345", true},
		{"empty host", "tls://:12345", "box.local:3000", "tls://box.local:12345", true},
		{"explicit host kept", "tls://a.example:1", "box.local:3000", "tls://a.example:1", true},
		// An IPv6 caller has to come back bracketed or the URI will not parse
		// again on the other side.
		{"v6 caller", "tls://0.0.0.0:12345", "[200:1::2]:3000", "tls://[200:1::2]:12345", true},
		{"host with no port", "tls://0.0.0.0:12345", "box.local", "tls://box.local:12345", true},
		{"unix dropped", "unix:///run/ygg.sock", "box.local:3000", "", false},
		{"garbage dropped", "::::", "box.local:3000", "", false},
		{"no caller host", "tls://0.0.0.0:12345", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := dialableListen(tc.uri, tc.host)
			if ok != tc.ok || got != tc.want {
				t.Errorf("dialableListen(%q, %q) = %q,%v; want %q,%v",
					tc.uri, tc.host, got, ok, tc.want, tc.ok)
			}
		})
	}
}
