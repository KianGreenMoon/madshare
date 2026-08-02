package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/database"
	"github.com/go-chi/chi/v5"
)

// nodeKey builds a well-formed 64-hex node key from a label, so a test can name
// its nodes readably and still pass the key validator every node address goes
// through.
func nodeKey(label string) string {
	return strings.Repeat(label, 64/len(label))
}

type nodesBody struct {
	Nodes []struct {
		Key    string `json:"key"`
		Name   string `json:"name"`
		Hops   *int   `json:"hops"`
		Self   bool   `json:"self"`
		Friend bool   `json:"friend"`
	} `json:"nodes"`
	Tracks int64 `json:"tracks"`
}

func nodesSummary(t *testing.T, srv *httptest.Server) nodesBody {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/madnetwork/summary")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body nodesBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

// TestMadnetworkNodesOrderedByHops pins the one ordering every node surface
// uses (docs/ui/madnetwork-nodes.md §Ordering): hops ascending, then the
// alphabet, with this server at 0 and a node the graph cannot place LAST.
//
// The last part is the one that could go wrong silently: an absent hop count
// read as zero would promote an unplaceable stranger to the top of the list,
// where this server belongs.
func TestMadnetworkNodesOrderedByHops(t *testing.T) {
	fake := &fakeMadnetwork{
		sources: []*database.MadnetworkNode{
			{ID: 1, Key: nodeKey("cc"), Name: "zeta", Friend: false},
			{ID: 2, Key: nodeKey("aa"), Name: "vinylcellar", Friend: true},
			{ID: 3, Key: nodeKey("bb"), Name: "attic", Friend: true},
			{ID: 4, Key: nodeKey("dd"), Name: "unplaceable"},
		},
		ownEntries: 412,
	}
	fed := &fakeFederation{hops: map[string]int{
		nodeKey("aa"): 1, nodeKey("bb"): 1, nodeKey("cc"): 2,
	}}
	r := chi.NewRouter()
	RegisterAPI(r, Deps{Madnetwork: fake, Federation: fed, MadnetworkName: "madshare@home"})
	srv := httptest.NewServer(r)
	defer srv.Close()

	body := nodesSummary(t, srv)
	var order []string
	for _, n := range body.Nodes {
		order = append(order, n.Name)
	}
	want := []string{"madshare@home", "attic", "vinylcellar", "zeta", "unplaceable"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v (hops, then alphabetical, unplaceable last)", order, want)
	}

	self := body.Nodes[0]
	if !self.Self || self.Hops == nil || *self.Hops != 0 {
		t.Errorf("self node = %+v, want self at 0 hops", self)
	}
	if last := body.Nodes[len(body.Nodes)-1]; last.Hops != nil {
		t.Errorf("unplaceable node reports hops %d, want none — absent, never 0", *last.Hops)
	}
}

// TestMadnetworkNodesWithoutAGraph: with nothing placeable the list is plain
// alphabetical — the same rule in a world where nobody can be placed — and it
// must not blow up or invent distances.
func TestMadnetworkNodesWithoutAGraph(t *testing.T) {
	fake := &fakeMadnetwork{sources: []*database.MadnetworkNode{
		{ID: 1, Key: nodeKey("cc"), Name: "zeta"},
		{ID: 2, Key: nodeKey("aa"), Name: "attic"},
	}}
	r := chi.NewRouter()
	RegisterAPI(r, Deps{Madnetwork: fake}) // no federation node at all
	srv := httptest.NewServer(r)
	defer srv.Close()

	body := nodesSummary(t, srv)
	if len(body.Nodes) != 2 || body.Nodes[0].Name != "attic" || body.Nodes[1].Name != "zeta" {
		t.Errorf("nodes = %+v, want the two sources alphabetically", body.Nodes)
	}
	// Federation off ⇒ no own published set ⇒ no self row promising a shelf that
	// cannot exist.
	for _, n := range body.Nodes {
		if n.Self {
			t.Errorf("self row on a node with federation off: %+v", n)
		}
	}
}

// TestMadnetworkNodeByKey covers the three different answers a node address can
// get (docs/ui/madnetwork-nodes.md §When there is nothing to show). They are
// different on purpose: "we hold no catalog from it" is an ordinary state of the
// discovery rotation and must not read as "no such node".
func TestMadnetworkNodeByKey(t *testing.T) {
	fake := &fakeMadnetwork{sources: []*database.MadnetworkNode{
		{ID: 7, Key: nodeKey("aa"), Name: "vinylcellar", Entries: 1204, Friend: true},
	}}
	fed := &fakeFederation{hops: map[string]int{
		nodeKey("aa"): 1,
		nodeKey("bb"): 3, // placeable, never pulled from
	}}
	r := chi.NewRouter()
	RegisterAPI(r, Deps{Madnetwork: fake, Federation: fed, MadnetworkName: "madshare@home"})
	srv := httptest.NewServer(r)
	defer srv.Close()

	get := func(key string) (int, map[string]any) {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/madnetwork/nodes/" + key)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body map[string]any
		json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}

	status, body := get(nodeKey("aa"))
	if status != http.StatusOK {
		t.Fatalf("known node = %d, want 200", status)
	}
	node, _ := body["node"].(map[string]any)
	if node["name"] != "vinylcellar" || node["hops"] != float64(1) {
		t.Errorf("node = %+v, want the cached source at 1 hop", node)
	}

	if status, body := get(nodeKey("bb")); status != http.StatusOK || body["no_catalog"] != true {
		t.Errorf("placeable-but-unpulled node = %d %+v, want 200 + no_catalog", status, body)
	}

	if status, _ := get(nodeKey("ff")); status != http.StatusNotFound {
		t.Errorf("unknown node = %d, want 404", status)
	}
	if status, _ := get("not-a-key"); status != http.StatusBadRequest {
		t.Errorf("malformed key = %d, want 400 (no lookup at all)", status)
	}
}

// TestMadnetworkSourceKeyNarrowsTheView: ?source= takes a node key, and a key we
// hold nothing from answers EMPTY rather than widening to the merged catalog.
// The asymmetry with a stale numeric id is the point — a row number is this
// server's bookkeeping, a key is an explicit request for one node, and serving
// the community's whole catalog under that node's name would be a lie about
// provenance.
func TestMadnetworkSourceKeyNarrowsTheView(t *testing.T) {
	fake := &fakeMadnetwork{sources: []*database.MadnetworkNode{
		{ID: 7, Key: nodeKey("aa"), Name: "vinylcellar"},
	}}
	r := chi.NewRouter()
	RegisterAPI(r, Deps{Madnetwork: fake, MadnetworkName: "madshare@home"})
	srv := httptest.NewServer(r)
	defer srv.Close()

	probe := func(source string) database.MadnetworkView {
		t.Helper()
		resp, err := http.Get(srv.URL + "/api/madnetwork/artists?source=" + source)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return fake.artistView
	}

	if v := probe(nodeKey("aa")); v.SourceID != 7 {
		t.Errorf("known key → view %+v, want the source it resolves to", v)
	}
	if v := probe(nodeKey("bb")); v.SourceID != database.NoSourceID {
		t.Errorf("unknown key → view %+v, want the empty shelf", v)
	}
	if v := probe("self"); !v.SelfOnly {
		t.Errorf("self → view %+v, want our own published set", v)
	}
	// A stale numeric id may still widen back to the merged view.
	if v := probe("nonsense"); v.SourceID != 0 || v.SelfOnly {
		t.Errorf("unparseable source → view %+v, want the merged catalog", v)
	}
}
