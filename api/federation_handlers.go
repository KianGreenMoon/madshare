package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/federation"
)

// /api/admin/federation — the madnetwork friendship surface behind the
// /admin/network page (federation F1, docs/architecture/federation-trust.md). All
// routes are gated on federation.manage; the running node is h.federation
// (nil when federation is disabled or compiled out).

// federationStatus handles GET /api/admin/federation: whether a node is
// running and, if so, its identity (name, mesh address, public key) and the
// node card an admin hands to friends. enabled:false is a 200 — the page
// renders a disabled note, not an error.
func (h *handler) federationStatus(w http.ResponseWriter, r *http.Request) {
	if h.federation == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": false})
		return
	}
	info := h.federation.Info()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"enabled":         true,
		"node":            info,
		"mesh_port":       federation.MeshPort,
		"inbound_healthy": h.federation.InboundHealthy(),
	})
}

// federationPeers handles GET /api/admin/federation/peers: the trusted-peer
// table, friends first (see database.ListFederationPeers), with derived mesh
// addresses and mapped local usernames.
func (h *handler) federationPeers(w http.ResponseWriter, r *http.Request) {
	if h.federation == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "federation is not enabled"})
		return
	}
	peers, err := h.federation.Peers(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if peers == nil {
		peers = []*federation.Peer{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "peers": peers})
}

// federationImportCard handles POST /api/admin/federation/peers: the admin half
// of the pairing handshake, in either of the two forms an admin can have a node
// in.
//
//	{"card": {…}}                     a node card exchanged out-of-band
//	{"public_key": "<hex>", "name": …} a bare key — the form the network map has
//
// The two are the same act: identity is the key, and a card carries nothing else
// except a claimed name. A new node becomes pending_outgoing (contacted
// immediately); for a node that already asked to pair, importing completes the
// friendship.
func (h *handler) federationImportCard(w http.ResponseWriter, r *http.Request) {
	if h.federation == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "federation is not enabled"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var body struct {
		Card      json.RawMessage `json:"card"`
		PublicKey string          `json:"public_key"`
		Name      string          `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json (want {\"card\": {…}} or {\"public_key\": \"…\"})"})
		return
	}

	var (
		peer *federation.Peer
		err  error
	)
	switch {
	case len(body.Card) > 0:
		card, perr := federation.ParseCard(body.Card)
		if perr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": perr.Error()})
			return
		}
		peer, err = h.federation.ImportCard(r.Context(), card)
	case body.PublicKey != "":
		peer, err = h.federation.ImportKey(r.Context(), body.PublicKey, body.Name)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "nothing to import (want {\"card\": {…}} or {\"public_key\": \"…\"})"})
		return
	}
	if err != nil {
		writeFederationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "peer": peer})
}

// federationPeerPatch handles PATCH /api/admin/federation/peers/{peerID}:
// rename the local label and/or map the node to a local user account
// ({"name": "..."} / {"user_id": 3} / {"user_id": null} to clear — user_id is
// applied only when the key is present).
func (h *handler) federationPeerPatch(w http.ResponseWriter, r *http.Request) {
	node, id, ok := h.federationPeerID(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var body struct {
		Name   *string `json:"name"`
		UserID *int64  `json:"user_id"`
		raw    map[string]json.RawMessage
	}
	buf := json.NewDecoder(r.Body)
	if err := buf.Decode(&body.raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json"})
		return
	}
	if v, ok := body.raw["name"]; ok {
		if err := json.Unmarshal(v, &body.Name); err != nil || body.Name == nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "name must be a string"})
			return
		}
	}
	hasUser := false
	if v, ok := body.raw["user_id"]; ok {
		hasUser = true
		if err := json.Unmarshal(v, &body.UserID); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "user_id must be a number or null"})
			return
		}
	}
	if body.Name != nil {
		if err := node.RenamePeer(r.Context(), id, *body.Name); err != nil {
			writeFederationError(w, err)
			return
		}
	}
	if hasUser {
		if err := node.MapPeerUser(r.Context(), id, body.UserID); err != nil {
			// A dangling user id trips the foreign key — a client error, not ours.
			if errors.Is(err, federation.ErrPeerNotFound) {
				writeFederationError(w, err)
			} else {
				writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "no such user"})
			}
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// federationPeerRemove handles DELETE /api/admin/federation/peers/{peerID}.
func (h *handler) federationPeerRemove(w http.ResponseWriter, r *http.Request) {
	if node, id, ok := h.federationPeerID(w, r); ok {
		h.federationPeerOp(w, r, func() error { return node.RemovePeer(r.Context(), id) })
	}
}

// federationPeerAccept handles POST .../peers/{peerID}/accept: approve a
// pending_incoming pairing request (the admin verifies the shown key against
// the card received out-of-band).
func (h *handler) federationPeerAccept(w http.ResponseWriter, r *http.Request) {
	if node, id, ok := h.federationPeerID(w, r); ok {
		h.federationPeerOp(w, r, func() error { return node.AcceptPeer(r.Context(), id) })
	}
}

// federationPeerBlock handles POST .../peers/{peerID}/block: refuse the node all
// madnetwork service and publish the block as a distrust mark.
//
// The optional {"reason": "…"} body is what the rest of the network reads. An
// absent one is accepted — refusing to block without an explanation would be
// worse than an unexplained block — but it makes the mark an anonymous downvote.
func (h *handler) federationPeerBlock(w http.ResponseWriter, r *http.Request) {
	node, id, ok := h.federationPeerID(w, r)
	if !ok {
		return
	}
	reason := blockReasonFromBody(r)
	h.federationPeerOp(w, r, func() error { return node.BlockPeer(r.Context(), id, reason) })
}

// federationBlockKey handles POST /api/admin/federation/block with
// {"public_key", "name", "reason"}: block a node we have no relationship with,
// seen only on the gossiped graph.
func (h *handler) federationBlockKey(w http.ResponseWriter, r *http.Request) {
	if h.federation == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "federation is not enabled"})
		return
	}
	var req struct {
		PublicKey string `json:"public_key"`
		Name      string `json:"name"`
		Reason    string `json:"reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json (want {\"public_key\": \"…\"})"})
		return
	}
	if err := h.federation.BlockKey(r.Context(), req.PublicKey, req.Name, req.Reason); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// blockReasonFromBody reads the optional reason. A missing or malformed body is
// simply no reason: the block itself must not depend on parsing succeeding.
func blockReasonFromBody(r *http.Request) string {
	var req struct {
		Reason string `json:"reason"`
	}
	if r.Body == nil {
		return ""
	}
	_ = json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<10)).Decode(&req)
	return req.Reason
}

// federationPeerUnblock handles POST .../peers/{peerID}/unblock.
func (h *handler) federationPeerUnblock(w http.ResponseWriter, r *http.Request) {
	if node, id, ok := h.federationPeerID(w, r); ok {
		h.federationPeerOp(w, r, func() error { return node.UnblockPeer(r.Context(), id) })
	}
}

// federationPeerID parses {peerID} and checks a node is running; on failure it
// has already written the response.
func (h *handler) federationPeerID(w http.ResponseWriter, r *http.Request) (FederationNode, int64, bool) {
	if h.federation == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "federation is not enabled"})
		return nil, 0, false
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "peerID"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid peer id"})
		return nil, 0, false
	}
	return h.federation, id, true
}

// federationPeerOp runs one peer state operation and maps its errors.
func (h *handler) federationPeerOp(w http.ResponseWriter, r *http.Request, op func() error) {
	if err := op(); err != nil {
		writeFederationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// writeFederationError maps friendship errors: unknown peer → 404, a state
// conflict (accept a friend, import a blocked node's card, …) → 409 with the
// explanatory message, anything else → 500.
func writeFederationError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, federation.ErrPeerNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "peer not found"})
	case errors.Is(err, federation.ErrPeerState):
		writeJSON(w, http.StatusConflict, map[string]any{"ok": false, "error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "storage error"})
	}
}

// federationGraph handles GET /api/admin/federation/graph[?radius=N]: the
// gossiped network map — every node reachable through a chain of friendships,
// with branch attribution and the distrust marks against it (federation F6),
// trimmed to a view radius (F7 item 7).
//
// `radius` is a RENDERING parameter and nothing else: it decides how much of the
// map is drawn, never who is served. Absent it defaults to
// federation.DefaultMapRadius; `radius=0` asks for the whole component, which
// stays available because search and paths are answered over everything.
func (h *handler) federationGraph(w http.ResponseWriter, r *http.Request) {
	m, ok := h.networkMap(w, r)
	if !ok {
		return
	}
	radius := federation.DefaultMapRadius
	if v := r.URL.Query().Get("radius"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "radius must be a non-negative integer"})
			return
		}
		radius = n
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "graph": federation.TrimMap(m, radius)})
}

// networkMap fetches the map and writes the shared refusals, so the three
// endpoints reading it do not each re-state what "federation is off" means.
func (h *handler) networkMap(w http.ResponseWriter, r *http.Request) (federation.NetworkMap, bool) {
	if h.federation == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "federation is not enabled"})
		return federation.NetworkMap{}, false
	}
	m, err := h.federation.NetworkMap(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return federation.NetworkMap{}, false
	}
	if m.Nodes == nil {
		m.Nodes = []federation.MapNode{}
	}
	if m.Edges == nil {
		m.Edges = []federation.MapEdge{}
	}
	return m, true
}

// federationGraphFind handles GET /api/admin/federation/graph/find?q= or
// ?branch=<key>: search over the WHOLE component, which is what makes a view
// radius affordable — the map may be showing three hops, but nothing beyond it
// has become unfindable (F7 item 7, §The network map).
//
// `q` matches a key, a mesh address or a name; `branch` lists everything that
// reached us through one direct friend, which is the unit blocking operates on.
func (h *handler) federationGraphFind(w http.ResponseWriter, r *http.Request) {
	m, ok := h.networkMap(w, r)
	if !ok {
		return
	}
	if branch := r.URL.Query().Get("branch"); branch != "" {
		nodes := federation.BranchNodes(m, branch)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "branch": branch, "nodes": nodes, "count": len(nodes),
		})
		return
	}
	q := r.URL.Query().Get("q")
	if len(q) > 200 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "query too long"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "hits": federation.FindNodes(m, q, mapFindLimit)})
}

// mapFindLimit is how many search results are worth showing at once. A search
// that returns two hundred nodes has not answered anything.
const mapFindLimit = 25

// federationGraphPaths handles GET /api/admin/federation/graph/paths?from=&to=:
// every (bounded) way two nodes are connected. `from` defaults to this node,
// because "how is this connected to ME, and through whom" is the question an
// admin actually arrives with — and the question a block is the answer to.
func (h *handler) federationGraphPaths(w http.ResponseWriter, r *http.Request) {
	m, ok := h.networkMap(w, r)
	if !ok {
		return
	}
	from, to := r.URL.Query().Get("from"), r.URL.Query().Get("to")
	if from == "" {
		from = h.federation.Info().PublicKey
	}
	if to == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "to is required"})
		return
	}
	paths := federation.Paths(m, from, to)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "from": from, "to": to, "paths": paths,
		// Truncation is reported rather than hidden: a list that silently
		// dropped the connection an admin was looking for is worse than none.
		"truncated": len(paths) >= federation.MaxPathResults,
	})
}

// federationGraphResync handles POST /api/admin/federation/graph/resync: pull
// the gossiped graph from every friend now, rather than when the 15-minute
// catalog cadence next comes round (the Rescan button on /admin/network).
//
// 202 rather than 200, and no result: the round runs on the refresh loop, so
// there is nothing to report back except that it was asked for. Repeated presses
// coalesce into the round already running, which is why this needs no throttle
// of its own — see docs/architecture/federation-trust.md §Refreshing the graph on
// demand for why the *serving* side answers with a memo instead of a refusal.
func (h *handler) federationGraphResync(w http.ResponseWriter, r *http.Request) {
	if h.federation == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "federation is not enabled"})
		return
	}
	h.federation.ResyncGraph()
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// federationDiscover handles POST /api/admin/federation/discover: pull one
// node's catalog on the next refresh round, ahead of the frontier rotation
// (F7 item 5, §Discovery beyond the friend ring).
//
// The rotation exists so that seeing the community costs a bounded amount per
// cycle, but fairness is the wrong answer when an admin is looking at a
// particular node on the map — interest should beat rotation. 202 and no result,
// for the same reason as the Rescan button: the round runs on the refresh loop,
// and whether that node answers is between it and the mesh.
func (h *handler) federationDiscover(w http.ResponseWriter, r *http.Request) {
	if h.federation == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "federation is not enabled"})
		return
	}
	var req struct {
		PublicKey string `json:"public_key"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json (want {\"public_key\": \"…\"})"})
		return
	}
	if err := h.federation.PullFrom(req.PublicKey); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true})
}

// federationReports handles GET /api/admin/federation/reports: contradicted
// identity claims awaiting a decision (federation F6). A peer's catalog makes
// claims this node can check — it advertises a content hash together with the
// head of its own fingerprint — and when we hold those exact bytes, a materially
// different claim contradicts something we can hash ourselves.
//
// The response is evidence, never a verdict: the peer card renders what was
// compared and how each side was obtained, next to the Block action that was
// already there. Blocking stays manual, because an automatic reputation score is
// a weapon in intra-network wars.
func (h *handler) federationReports(w http.ResponseWriter, r *http.Request) {
	if h.federation == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "federation is not enabled"})
		return
	}
	reports, err := h.federation.ClaimReports(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reports": reports})
}

// federationReportPatch handles PATCH /api/admin/federation/reports/{reportID}:
// record the admin's decision on one finding ({"disposition": "dismissed"} or
// "acted"). Detection never overwrites it, so a dismissed finding does not come
// back every fifteen minutes.
func (h *handler) federationReportPatch(w http.ResponseWriter, r *http.Request) {
	if h.federation == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "federation is not enabled"})
		return
	}
	id, err := strconv.ParseInt(chi.URLParam(r, "reportID"), 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad report id"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	var body struct {
		Disposition string `json:"disposition"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json"})
		return
	}
	switch body.Disposition {
	case federation.ClaimDismissed, federation.ClaimActed, federation.ClaimNew:
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": `disposition must be "dismissed", "acted" or "new"`})
		return
	}
	if err := h.federation.SetClaimDisposition(r.Context(), id, body.Disposition); err != nil {
		writeFederationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
