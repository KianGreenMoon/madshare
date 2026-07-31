package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"daemonlord.ygg/madshare/federation"
	"github.com/go-chi/chi/v5"
)

// A small community for the map endpoints: two friends, a stranger both of them
// vouch for, and a chain out to four hops. `ab` is the fake node's own key.
//
//	ab ── f1 ── s1 ── s2
//	 └─── f2 ────┘
func mapFixture() federation.NetworkMap {
	return federation.NetworkMap{
		Radius: 4,
		Nodes: []federation.MapNode{
			{Key: "ab", Name: "us", State: federation.MapSelf, Distance: 0},
			{Key: "f1", Name: "Friend One", State: federation.MapFriend, Distance: 1, Via: []string{"f1"}},
			{Key: "f2", Name: "Friend Two", State: federation.MapFriend, Distance: 1, Via: []string{"f2"}},
			{Key: "s1", Name: "Stranger", Distance: 2, Via: []string{"f1", "f2"}, Address: "200:beef::1"},
			{Key: "s2", Name: "Far Away", Distance: 4, Via: []string{"f1"}},
		},
		Edges: []federation.MapEdge{
			{From: "ab", To: "f1", Mutual: true},
			{From: "ab", To: "f2", Mutual: true},
			{From: "f1", To: "s1", Mutual: true},
			{From: "f2", To: "s1", Mutual: true},
			{From: "s1", To: "s2", Mutual: true},
		},
	}
}

func mapServer(t *testing.T) (*httptest.Server, *fakeFederation) {
	t.Helper()
	fed := &fakeFederation{graph: mapFixture()}
	r := chi.NewRouter()
	RegisterAdmin(r, Deps{Federation: fed})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, fed
}

func getJSON(t *testing.T, url string, into any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if into != nil {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil && resp.StatusCode == http.StatusOK {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return resp.StatusCode
}

// TestFederationGraphRadius: the map opens on the neighbourhood, and the reply
// says how much is being held back — which is what makes "expand" offerable.
func TestFederationGraphRadius(t *testing.T) {
	srv, _ := mapServer(t)

	// A fresh target per request: `hidden` is omitempty, so a reused struct would
	// carry the previous reply's value into an assertion about this one.
	type graphBody struct {
		Graph struct {
			Nodes  []federation.MapNode `json:"nodes"`
			Edges  []federation.MapEdge `json:"edges"`
			Radius int                  `json:"radius"`
			Shown  int                  `json:"shown"`
			Hidden int                  `json:"hidden"`
		} `json:"graph"`
	}
	get := func(url string) graphBody {
		var b graphBody
		getJSON(t, url, &b)
		return b
	}

	// Default: three hops, so the node at four is not drawn.
	body := get(srv.URL + "/api/admin/federation/graph")
	if len(body.Graph.Nodes) != 4 || body.Graph.Hidden != 1 {
		t.Fatalf("default radius drew %d nodes, hid %d", len(body.Graph.Nodes), body.Graph.Hidden)
	}
	// Radius still describes the whole component, not the drawing.
	if body.Graph.Radius != 4 {
		t.Errorf("radius = %d, want the component's 4", body.Graph.Radius)
	}
	for _, e := range body.Graph.Edges {
		if e.From == "s2" || e.To == "s2" {
			t.Errorf("edge to an undrawn node: %+v", e)
		}
	}

	// radius=0 is the whole thing.
	whole := get(srv.URL + "/api/admin/federation/graph?radius=0")
	if len(whole.Graph.Nodes) != 5 || whole.Graph.Hidden != 0 {
		t.Errorf("radius=0 drew %d nodes, hid %d", len(whole.Graph.Nodes), whole.Graph.Hidden)
	}

	// radius=1 keeps only the friends.
	near := get(srv.URL + "/api/admin/federation/graph?radius=1")
	if len(near.Graph.Nodes) != 3 {
		t.Errorf("radius=1 drew %d nodes, want us and two friends", len(near.Graph.Nodes))
	}

	if code := getJSON(t, srv.URL+"/api/admin/federation/graph?radius=-2", nil); code != http.StatusBadRequest {
		t.Errorf("negative radius status = %d, want 400", code)
	}
}

// TestFederationGraphFind: search answers over the WHOLE component, which is the
// whole reason a view radius is affordable.
func TestFederationGraphFind(t *testing.T) {
	srv, _ := mapServer(t)

	var body struct {
		Hits []struct {
			Key      string `json:"key"`
			Matched  string `json:"matched"`
			Distance int    `json:"distance"`
		} `json:"hits"`
	}

	// s2 sits past the default radius and is still findable.
	getJSON(t, srv.URL+"/api/admin/federation/graph/find?q=far", &body)
	if len(body.Hits) != 1 || body.Hits[0].Key != "s2" || body.Hits[0].Matched != "name" {
		t.Fatalf("name search = %+v", body.Hits)
	}
	if body.Hits[0].Distance != 4 {
		t.Errorf("hit distance = %d, want 4 — the UI needs it to know how far to expand", body.Hits[0].Distance)
	}

	body.Hits = nil
	getJSON(t, srv.URL+"/api/admin/federation/graph/find?q=200:beef::1", &body)
	if len(body.Hits) != 1 || body.Hits[0].Matched != "address" {
		t.Errorf("address search = %+v", body.Hits)
	}

	// A branch: everything that reached us through one friend, which is what a
	// block on that friend would take with it.
	var branch struct {
		Nodes []federation.MapNode `json:"nodes"`
		Count int                  `json:"count"`
	}
	getJSON(t, srv.URL+"/api/admin/federation/graph/find?branch=f1", &branch)
	if branch.Count != 3 {
		t.Fatalf("f1's branch = %d nodes (%+v), want the friend plus the two behind it", branch.Count, branch.Nodes)
	}
}

// TestFederationGraphPaths: how a node is connected to us, and through whom —
// the question a block is the answer to.
func TestFederationGraphPaths(t *testing.T) {
	srv, _ := mapServer(t)

	var body struct {
		From      string     `json:"from"`
		Paths     [][]string `json:"paths"`
		Truncated bool       `json:"truncated"`
	}
	getJSON(t, srv.URL+"/api/admin/federation/graph/paths?to=s1", &body)

	// `from` defaults to this node, because that is the question an admin
	// actually arrives with.
	if body.From != "ab" {
		t.Errorf("from = %q, want this node's key by default", body.From)
	}
	if len(body.Paths) != 2 {
		t.Fatalf("paths to s1 = %v, want both friends' routes", body.Paths)
	}
	if body.Truncated {
		t.Error("a two-path answer reported itself truncated")
	}
	for _, p := range body.Paths {
		if p[0] != "ab" || p[len(p)-1] != "s1" {
			t.Errorf("path does not join the endpoints: %v", p)
		}
	}

	// An explicit pair is allowed too — the requirement is "given two keys".
	body.Paths = nil
	getJSON(t, srv.URL+"/api/admin/federation/graph/paths?from=f1&to=f2", &body)
	if body.From != "f1" || len(body.Paths) == 0 {
		t.Errorf("explicit endpoints: from=%q paths=%v", body.From, body.Paths)
	}

	if code := getJSON(t, srv.URL+"/api/admin/federation/graph/paths", nil); code != http.StatusBadRequest {
		t.Errorf("missing `to` status = %d, want 400", code)
	}
}

// TestFederationMapEndpointsWithoutFederation: all three refuse the same way
// when there is no node, rather than each inventing an answer.
func TestFederationMapEndpointsWithoutFederation(t *testing.T) {
	r := chi.NewRouter()
	RegisterAdmin(r, Deps{})
	srv := httptest.NewServer(r)
	defer srv.Close()

	for _, path := range []string{
		"/api/admin/federation/graph",
		"/api/admin/federation/graph/find?q=x",
		"/api/admin/federation/graph/paths?to=x",
	} {
		if code := getJSON(t, srv.URL+path, nil); code != http.StatusServiceUnavailable {
			t.Errorf("%s status = %d, want 503", path, code)
		}
	}
}
