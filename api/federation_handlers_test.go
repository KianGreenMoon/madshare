package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/federation"
)

// fakeFederation implements FederationNode with canned data; the real state
// machine is covered by the federation package's handshake test — here only the
// HTTP mapping is under test.
type fakeFederation struct {
	peers    []*federation.Peer
	imported *federation.Card
	patched  map[string]any
	opErr    error
}

func (f *fakeFederation) Info() federation.NodeInfo {
	return federation.NodeInfo{
		Name: "fake", Address: "200::1", PublicKey: "ab",
		Card: federation.Card{Version: federation.ProtocolVersion, Name: "fake", PublicKey: "ab"},
	}
}
func (f *fakeFederation) Peers(context.Context) ([]*federation.Peer, error) { return f.peers, nil }
func (f *fakeFederation) ImportCard(_ context.Context, c federation.Card) (*federation.Peer, error) {
	if f.opErr != nil {
		return nil, f.opErr
	}
	f.imported = &c
	return &federation.Peer{ID: 1, PublicKey: c.PublicKey, Name: c.Name, State: federation.PeerPendingOutgoing}, nil
}
func (f *fakeFederation) EnsureBlob(context.Context, string) (federation.Transfer, error) {
	return nil, federation.ErrNoHolder
}
func (f *fakeFederation) AcceptPeer(context.Context, int64) error  { return f.opErr }
func (f *fakeFederation) BlockPeer(context.Context, int64) error   { return f.opErr }
func (f *fakeFederation) UnblockPeer(context.Context, int64) error { return f.opErr }
func (f *fakeFederation) RemovePeer(context.Context, int64) error  { return f.opErr }
func (f *fakeFederation) RenamePeer(_ context.Context, _ int64, name string) error {
	f.patched["name"] = name
	return f.opErr
}
func (f *fakeFederation) MapPeerUser(_ context.Context, _ int64, userID *int64) error {
	f.patched["user_id"] = userID
	return f.opErr
}

func newFederationTestServer(t *testing.T, node FederationNode) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	RegisterAdmin(r, Deps{Federation: node}) // no auth wired — gates pass through
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func TestFederationEndpoints_Disabled(t *testing.T) {
	srv := newFederationTestServer(t, nil)

	resp, err := http.Get(srv.URL + "/api/admin/federation")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status struct {
		OK      bool `json:"ok"`
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !status.OK || status.Enabled {
		t.Errorf("status = %d %+v, want 200 enabled:false", resp.StatusCode, status)
	}

	for _, probe := range []struct{ method, path string }{
		{http.MethodGet, "/api/admin/federation/peers"},
		{http.MethodPost, "/api/admin/federation/peers/1/accept"},
	} {
		req, _ := http.NewRequest(probe.method, srv.URL+probe.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("%s %s = %d, want 503 when federation is off", probe.method, probe.path, resp.StatusCode)
		}
	}
}

func TestFederationEndpoints_ImportAndErrors(t *testing.T) {
	fake := &fakeFederation{patched: map[string]any{}}
	srv := newFederationTestServer(t, fake)
	key := strings.Repeat("ab", 32)

	// A well-formed card imports.
	body := `{"card": {"madshare_node_card": 0, "name": "pal", "public_key": "` + key + `"}}`
	resp, err := http.Post(srv.URL+"/api/admin/federation/peers", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import = %d, want 200", resp.StatusCode)
	}
	if fake.imported == nil || fake.imported.PublicKey != key {
		t.Fatalf("imported card = %+v, want key %s", fake.imported, key)
	}

	// A malformed card is a 400 with the parse message.
	resp, err = http.Post(srv.URL+"/api/admin/federation/peers", "application/json",
		strings.NewReader(`{"card": {"madshare_node_card": 0, "public_key": "nope"}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad card = %d, want 400", resp.StatusCode)
	}

	// PATCH applies name and user_id (null clears).
	req, _ := http.NewRequest(http.MethodPatch, srv.URL+"/api/admin/federation/peers/1",
		strings.NewReader(`{"name": "renamed", "user_id": null}`))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch = %d, want 200", resp.StatusCode)
	}
	if fake.patched["name"] != "renamed" {
		t.Errorf("patched name = %v, want renamed", fake.patched["name"])
	}
	if got, ok := fake.patched["user_id"].(*int64); !ok || got != nil {
		t.Errorf("patched user_id = %v, want explicit nil (clear)", fake.patched["user_id"])
	}

	// Error mapping: state conflict → 409, unknown peer → 404.
	fake.opErr = federation.ErrPeerState
	resp, _ = http.Post(srv.URL+"/api/admin/federation/peers/1/accept", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("state-conflict accept = %d, want 409", resp.StatusCode)
	}
	fake.opErr = federation.ErrPeerNotFound
	resp, _ = http.Post(srv.URL+"/api/admin/federation/peers/1/block", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("missing-peer block = %d, want 404", resp.StatusCode)
	}
}
