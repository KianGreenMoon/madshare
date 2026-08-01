package api

import (
	"log"
	"net/http"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/federation"
)

// Listener-node capability tokens (federation F7 item 9,
// docs/architecture/federation.md §Principals & access, "The capability token").
//
// This is the issuing end, and it is deliberately an ordinary authenticated API
// call rather than anything federation-shaped. A madplayer is a person's device:
// it signs in to its home server with the same session or bearer token a browser
// uses, and asks for a vouch it can present on the mesh. There is no node card,
// no admin accept and no federation_peers row anywhere in this flow — that is
// the whole point of the listener node being a third kind of participant.
//
// Nothing is stored. The grant verifies from its own bytes, so there is no
// session table to sweep and no revocation list to distribute: a token stops
// working when it expires (one hour, renewed at half-life), or the moment its
// issuer stops being placeable in the verifier's community.

// madnetworkIssueToken handles POST /api/madnetwork/token {node_key}: sign
// "this bearer is my user until T" for the calling account's own device.
//
// The account's rights travel with it. A user without content.access is vouched
// for as guest-only, exactly as a friend node mapped to such an account would
// be — so a restricted account cannot widen its own reach by walking its library
// onto a phone.
func (h *handler) madnetworkIssueToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		NodeKey string `json:"node_key"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	id := auth.FromContext(r.Context())
	if id == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	bearer, err := federation.NormalizeKey(body.NodeKey)
	if err != nil {
		http.Error(w, "node_key must be a 64-character hex ed25519 public key", http.StatusBadRequest)
		return
	}
	// A node vouching for itself is not a listener node, it is a peering — and
	// one that would sidestep the friendship handshake entirely. Refused here so
	// the caller gets a real explanation rather than a 500.
	if bearer == h.federation.Info().PublicKey {
		http.Error(w, "a node cannot issue a capability token to itself", http.StatusBadRequest)
		return
	}
	grant, err := h.federation.IssueCapabilityToken(bearer, !id.Has(auth.PermContentAccess))
	if err != nil {
		log.Printf("madnetwork: issue capability token for %s: %v", id.Username, err)
		http.Error(w, "could not issue a token", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, grant)
}
