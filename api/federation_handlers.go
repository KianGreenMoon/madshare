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
// /admin/network page (federation F1, docs/architecture/federation.md). All
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

// federationImportCard handles POST /api/admin/federation/peers: import a
// friend's node card ({"card": {...}}) — the admin half of the pairing
// handshake. A new node becomes pending_outgoing (contacted immediately); a
// card for a node that already asked to pair completes the friendship.
func (h *handler) federationImportCard(w http.ResponseWriter, r *http.Request) {
	if h.federation == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "federation is not enabled"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var body struct {
		Card json.RawMessage `json:"card"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Card) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "invalid json (want {\"card\": {…}})"})
		return
	}
	card, err := federation.ParseCard(body.Card)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	peer, err := h.federation.ImportCard(r.Context(), card)
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

// federationGraph handles GET /api/admin/federation/graph: the gossiped network
// map — every node reachable through a chain of friendships, with branch
// attribution and the distrust marks against it (federation F6).
func (h *handler) federationGraph(w http.ResponseWriter, r *http.Request) {
	if h.federation == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "federation is not enabled"})
		return
	}
	m, err := h.federation.NetworkMap(r.Context())
	if err != nil {
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	if m.Nodes == nil {
		m.Nodes = []federation.MapNode{}
	}
	if m.Edges == nil {
		m.Edges = []federation.MapEdge{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "graph": m})
}
