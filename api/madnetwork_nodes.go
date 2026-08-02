package api

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
	"github.com/go-chi/chi/v5"
)

// The node surfaces of /madnetwork (docs/ui/madnetwork-nodes.md): the list every
// node view is built from, and one node addressed by its public key.
//
// A node is addressed by its KEY and never by the catalog-source row id. The id
// is a local row number — the frontier rotation evicts the coldest sources past
// discovery_cap, so a node that goes quiet and comes back is a different id on
// this same server, never mind another one. The key is the node's identity
// everywhere (federation.md §Goal & vocabulary), which is what makes a node page
// a link somebody can send.

// madnetworkNode is one node as every node surface renders it: what the store
// knows, plus the two facts only this layer can supply — how many friendship
// hops away it is, and whether it is us.
type madnetworkNode struct {
	*database.MadnetworkNode
	// Hops is friendship distance: 0 this node, 1 a direct friend. Omitted when
	// the graph cannot place the node, which the UI reads as "distance unknown"
	// and the ordering sorts last. Never 0 for a stranger — 0 is us.
	Hops *int `json:"hops,omitempty"`
	Self bool `json:"self,omitempty"`
}

// nodeList is every node the merged view draws on, our own included, in the one
// order all node surfaces use: hops ascending, then name, then key.
//
// The order is a rendering decision and nothing else — the same warning the map
// radius carries (federation/mapview.go), and it matters more here because this
// list is on a page ordinary users see. Hops never limits who is served and
// never appears in a scope.
func (h *handler) nodeList(ctx context.Context, view database.MadnetworkView) ([]madnetworkNode, int64, error) {
	sources, tracks, err := h.madnetwork.MadnetworkSummary(ctx, view)
	if err != nil {
		return nil, 0, err
	}
	hops := h.hopsByKey(ctx)
	nodes := make([]madnetworkNode, 0, len(sources)+1)
	if self := h.selfNode(ctx, view); self != nil {
		nodes = append(nodes, *self)
	}
	for _, s := range sources {
		n := madnetworkNode{MadnetworkNode: s}
		if d, ok := hops[s.Key]; ok {
			n.Hops = &d
		}
		nodes = append(nodes, n)
	}
	sortNodes(nodes)
	return nodes, tracks, nil
}

// selfNode is this server as an entry in that list: an ordinary node at 0 hops
// rather than a field beside it. Nil when the federation node is not running —
// then there is no published set and no key, and a "this server" row promising a
// shelf that cannot exist would be the only dishonest row on the page.
func (h *handler) selfNode(ctx context.Context, view database.MadnetworkView) *madnetworkNode {
	if !h.includeSelf() {
		return nil
	}
	zero := 0
	self := &madnetworkNode{
		MadnetworkNode: &database.MadnetworkNode{
			Name: h.madnetworkName, Reachable: true, Friend: true,
		},
		Hops: &zero, Self: true,
	}
	if h.federation != nil {
		self.Key = h.federation.Info().PublicKey
	}
	if n, err := h.madnetwork.MadnetworkOwnEntries(ctx, view); err == nil {
		self.Entries = n
	}
	return self
}

// sortNodes applies the one ordering (docs/ui/madnetwork-nodes.md §Ordering):
// the nodes this admin chose personally first, then the ones they chose, and so
// on outward; alphabetically within a ring; key as the last tiebreak so two
// unnamed nodes still have a stable order.
//
// A node the graph cannot place sorts after every placeable one rather than at
// distance 0 — we know less about it, not more.
func sortNodes(nodes []madnetworkNode) {
	sort.SliceStable(nodes, func(a, b int) bool {
		x, y := nodes[a], nodes[b]
		if (x.Hops == nil) != (y.Hops == nil) {
			return y.Hops == nil // placeable first
		}
		if x.Hops != nil && *x.Hops != *y.Hops {
			return *x.Hops < *y.Hops
		}
		if nx, ny := strings.ToLower(x.Name), strings.ToLower(y.Name); nx != ny {
			return nx < ny
		}
		return x.Key < y.Key
	})
}

// hopsByKey is the friendship distance of every node we can place. Empty when
// there is no federation node, no graph yet, or the read fails — the same
// swallow-and-degrade as branchesByKey, and it degrades to a plain alphabetical
// list, which is the same rule in a world where nobody can be placed.
func (h *handler) hopsByKey(ctx context.Context) map[string]int {
	if h.federation == nil {
		return nil
	}
	hops, err := h.federation.HopMap(ctx)
	if err != nil {
		return nil
	}
	return hops
}

// madnetworkNode handles GET /api/madnetwork/nodes/{key}: one node's card.
//
// The list itself is /api/madnetwork/summary, which already answers with every
// node plus the two view-wide facts (merged track count, inbound health) the
// directory's header needs. A second endpoint returning the same rows would be
// one implementation with two names.
//
// The three answers are deliberately different (docs/ui/madnetwork-nodes.md
// §When there is nothing to show). A malformed key is a 400 and no lookup. A
// key we hold no catalog from is a 200 with the node still described as far as
// the graph can describe it — an ordinary state of the frontier rotation, since
// we can place a node long before its turn to be pulled from comes up. Only a
// key nothing in our view knows at all is a 404.
func (h *handler) madnetworkNode(w http.ResponseWriter, r *http.Request) {
	key, err := federation.NormalizeKey(chi.URLParam(r, "key"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "not a node key"})
		return
	}
	view := h.madnetworkView(r.Context())

	if self := h.selfNode(r.Context(), view); self != nil && self.Key == key {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "node": self})
		return
	}

	src, found, err := h.madnetwork.MadnetworkSourceByKey(r.Context(), key, view)
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	hops := h.hopsByKey(r.Context())
	d, placed := hops[key]
	if !found {
		if !placed {
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "not in view", "key": key})
			return
		}
		// Placeable but not pulled from: describe what we can and let the page
		// say the shelf is empty because nothing has been fetched, not because
		// the node is offering nothing.
		node := madnetworkNode{MadnetworkNode: &database.MadnetworkNode{Key: key}, Hops: &d}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "node": node, "no_catalog": true})
		return
	}
	node := madnetworkNode{MadnetworkNode: src}
	if placed {
		node.Hops = &d
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "node": node})
}
