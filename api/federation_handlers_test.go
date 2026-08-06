package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/federation"
)

// fakeFederation implements FederationNode with canned data; the real state
// machine is covered by the federation package's handshake test — here only the
// HTTP mapping is under test.
type fakeFederation struct {
	resyncs     int
	pulled      []string // node keys the discover endpoint asked to pull from (F7 item 5)
	peers       []*federation.Peer
	imported    *federation.Card
	patched     map[string]any
	opErr       error
	inboundDead bool   // when true, InboundHealthy() reports false (fail-open path)
	blockReason string // what the last block carried into the published mark (F6)
	graph       federation.NetworkMap
	active      []federation.TransferStats // fetches running right now (the cache page's in-flight line)
	branches    map[string][]string        // node key → the direct friends it reaches us through
	hops        map[string]int             // node key → friendship distance from us (absent = unplaceable)
	reports     []*federation.ClaimReport  // contradicted claims awaiting a decision (F6)
	disposed    []string                   // "<id>:<disposition>" per PATCH
	evicted     []string                   // hashes EvictCachedBlob was asked to drop
	// Swarm traffic (docs/architecture/swarm-admin.md): what this session has
	// moved, and the deltas the next drain hands the flusher. drained records how
	// often DrainTraffic was called, since "drains once, adds once" is the flush
	// contract worth pinning.
	traffic       federation.TrafficSnapshot
	pending       []federation.TrafficDelta
	drained       int
	upRate        int64 // the caps SwarmRates reports, in bytes/sec
	downRate      int64
	rateRefreshes int // how often a write asked the node to re-read them
	// What the last capability-token issuance was asked for (F7 item 9): the
	// bearer key, and the guest bit the caller's account earned.
	tokenBearer    string
	tokenGuestOnly bool
}

// federationTestTokenTTL is only how far ahead the fake stamps an expiry; the
// real lifetime rule is federation.TokenTTL and is tested there.
const federationTestTokenTTL = time.Hour

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
func (f *fakeFederation) ImportKey(ctx context.Context, publicKey, name string) (*federation.Peer, error) {
	return f.ImportCard(ctx, federation.Card{Version: federation.ProtocolVersion, Name: name, PublicKey: publicKey})
}

func (f *fakeFederation) EnsureBlob(context.Context, string) (federation.Transfer, error) {
	return nil, federation.ErrNoHolder
}

func (f *fakeFederation) ActiveTransfers() []federation.TransferStats { return f.active }

func (f *fakeFederation) Traffic() federation.TrafficSnapshot { return f.traffic }

func (f *fakeFederation) SwarmRates() (up, down int64) { return f.upRate, f.downRate }

func (f *fakeFederation) RefreshRates() { f.rateRefreshes++ }

// DrainTraffic hands over the pending deltas and clears them, exactly as the
// real one does — a second drain must come back empty, or a retrying flusher
// would double-count.
func (f *fakeFederation) DrainTraffic() []federation.TrafficDelta {
	f.drained++
	out := f.pending
	f.pending = nil
	return out
}

// IssueCapabilityToken records what the handler asked for (F7 item 9) so a test
// can assert the guest bit the caller's account earned, and hands back a grant
// that is well-formed without being signed — signing is federation's to test.
func (f *fakeFederation) IssueCapabilityToken(bearerKey string, guestOnly bool) (federation.CapabilityGrant, error) {
	f.tokenBearer, f.tokenGuestOnly = bearerKey, guestOnly
	if f.opErr != nil {
		return federation.CapabilityGrant{}, f.opErr
	}
	return federation.CapabilityGrant{
		Token:     "test-token",
		Issuer:    f.Info().PublicKey,
		Bearer:    bearerKey,
		ExpiresAt: time.Now().Add(federationTestTokenTTL),
	}, nil
}
func (f *fakeFederation) InboundHealthy() bool                    { return !f.inboundDead }
func (f *fakeFederation) AcceptPeer(context.Context, int64) error { return f.opErr }
func (f *fakeFederation) BlockPeer(_ context.Context, _ int64, reason string) error {
	f.blockReason = reason
	return f.opErr
}

// EvictCachedBlob records the hashes the handlers asked to drop, so a test can
// assert that landing a blob in the library retires its cache copy.
func (f *fakeFederation) EvictCachedBlob(hash string) error {
	f.evicted = append(f.evicted, hash)
	return nil
}

func (f *fakeFederation) BranchMap(context.Context) (map[string][]string, error) {
	return f.branches, nil
}

func (f *fakeFederation) HopMap(context.Context) (map[string]int, error) {
	return f.hops, nil
}

func (f *fakeFederation) NetworkMap(context.Context) (federation.NetworkMap, error) {
	return f.graph, nil
}

func (f *fakeFederation) ResyncGraph() { f.resyncs++ }

func (f *fakeFederation) PullFrom(publicKey string) error {
	if len(publicKey) != 64 {
		return errors.New("not a node key")
	}
	f.pulled = append(f.pulled, publicKey)
	return nil
}

func (f *fakeFederation) ClaimReports(context.Context) ([]*federation.ClaimReport, error) {
	return f.reports, nil
}

func (f *fakeFederation) SetClaimDisposition(_ context.Context, id int64, disposition string) error {
	f.disposed = append(f.disposed, fmt.Sprintf("%d:%s", id, disposition))
	return nil
}

func (f *fakeFederation) BlockKey(_ context.Context, _, _, reason string) error {
	f.blockReason = reason
	return f.opErr
}
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

	// A bare key is the same act — the form the network map has for a node whose
	// admin never exported a card. The name rides along as the peer's own claim.
	fake.imported = nil
	otherKey := strings.Repeat("cd", 32)
	resp, err = http.Post(srv.URL+"/api/admin/federation/peers", "application/json",
		strings.NewReader(`{"public_key": "`+otherKey+`", "name": "found on the map"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import by key = %d, want 200", resp.StatusCode)
	}
	if fake.imported == nil || fake.imported.PublicKey != otherKey || fake.imported.Name != "found on the map" {
		t.Fatalf("imported by key = %+v, want key %s named \"found on the map\"", fake.imported, otherKey)
	}

	// Neither form present is a 400 naming both.
	resp, err = http.Post(srv.URL+"/api/admin/federation/peers", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty import = %d, want 400", resp.StatusCode)
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

// Every block publishes a distrust mark, so the reason has to reach the node —
// a mark without one is an anonymous downvote nobody downstream can act on.
func TestFederationBlockCarriesReason(t *testing.T) {
	fake := &fakeFederation{patched: map[string]any{}}
	srv := newFederationTestServer(t, fake)

	post := func(path, body string) int {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}

	if code := post("/api/admin/federation/peers/7/block", `{"reason":"contradicted fingerprint"}`); code != http.StatusOK {
		t.Fatalf("block = %d, want 200", code)
	}
	if fake.blockReason != "contradicted fingerprint" {
		t.Errorf("reason reaching the node = %q, want the posted one", fake.blockReason)
	}

	// A block with no body still blocks: refusing to act without an explanation
	// would be worse than an unexplained block.
	fake.blockReason = "unset"
	resp, err := http.Post(srv.URL+"/api/admin/federation/peers/7/block", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("bodyless block = %d, want 200", resp.StatusCode)
	}
	if fake.blockReason != "" {
		t.Errorf("reason = %q, want empty for a bodyless block", fake.blockReason)
	}

	// Blocking a key seen only on the gossiped graph, with no peer row.
	if code := post("/api/admin/federation/block",
		`{"public_key":"`+strings.Repeat("ab", 32)+`","name":"stranger","reason":"sybil farm"}`); code != http.StatusOK {
		t.Fatalf("block by key = %d, want 200", code)
	}
	if fake.blockReason != "sybil farm" {
		t.Errorf("block-by-key reason = %q", fake.blockReason)
	}
}

// TestFederationReports covers the claim-report surface: the findings an admin
// sees, and recording a decision on one. The detection itself is SQL and is
// tested against a real database (database/madnetwork_claims_test.go).
func TestFederationReports(t *testing.T) {
	fake := &fakeFederation{
		patched: map[string]any{},
		reports: []*federation.ClaimReport{{
			ID: 7, SourceID: 3, Kind: federation.ClaimHeldBlob, Hash: "3a9f",
			BER: 0.47, Words: 64, OurVersion: "1.5.1", TheirVersion: "1.4.3",
			Disposition: federation.ClaimNew, PeerName: "studio", PeerKey: "ab12",
		}},
	}
	srv := newFederationTestServer(t, fake)

	resp, err := http.Get(srv.URL + "/api/admin/federation/reports")
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		OK      bool                      `json:"ok"`
		Reports []*federation.ClaimReport `json:"reports"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !listed.OK || len(listed.Reports) != 1 {
		t.Fatalf("reports = %+v, want the one finding", listed)
	}
	got := listed.Reports[0]
	if got.Hash != "3a9f" || got.PeerName != "studio" || got.PeerKey != "ab12" {
		t.Errorf("report = %+v, want the hash plus the peer's label AND key", got)
	}
	if got.BER == 0 || got.Words == 0 || got.TheirVersion == "" {
		t.Error("the evidence (measurement and both fingerprinter versions) must survive the wire")
	}

	patch := func(id, body string) int {
		req, _ := http.NewRequest(http.MethodPatch,
			srv.URL+"/api/admin/federation/reports/"+id, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := patch("7", `{"disposition":"dismissed"}`); code != http.StatusOK {
		t.Fatalf("dismiss = %d, want 200", code)
	}
	if len(fake.disposed) != 1 || fake.disposed[0] != "7:dismissed" {
		t.Errorf("recorded dispositions = %v, want [7:dismissed]", fake.disposed)
	}
	// An unknown disposition is a client error — the set is closed on purpose, so
	// nothing invents a state the schema's CHECK would reject.
	if code := patch("7", `{"disposition":"blocked"}`); code != http.StatusBadRequest {
		t.Errorf("unknown disposition = %d, want 400", code)
	}
	if code := patch("nope", `{"disposition":"acted"}`); code != http.StatusBadRequest {
		t.Errorf("bad id = %d, want 400", code)
	}
}

// The Rescan button: an accepted request that hands the work to the refresh
// loop, and a 503 when no node runs.
func TestFederationGraphResync(t *testing.T) {
	fake := &fakeFederation{patched: map[string]any{}}
	srv := newFederationTestServer(t, fake)

	resp, err := http.Post(srv.URL+"/api/admin/federation/graph/resync", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("resync = %d, want 202", resp.StatusCode)
	}
	if fake.resyncs != 1 {
		t.Errorf("ResyncGraph called %d times, want 1", fake.resyncs)
	}

	off := newFederationTestServer(t, nil)
	resp, err = http.Post(off.URL+"/api/admin/federation/graph/resync", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("resync with federation off = %d, want 503", resp.StatusCode)
	}
}
