package api

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/federation"
)

// The household's tracker (docs/architecture/federation-access.md §"The household",
// "Being found") — the two calls a listener device makes to this server that are
// about bytes rather than about the library.
//
// A madplayer fetches into a cache and can seed it back, but nothing could ever
// ask it to: it publishes no catalog, it is in nobody's holdings sync, and no
// graph walk reaches it. So it tells its home server what it has, and asks its
// home server who else has a given hash. Both are ordinary authenticated calls
// on the same permission as browsing — a device participates in madnetwork, it
// just does its own fetching.
//
// What this server does NOT do with what it learns is the part worth
// remembering: a device's holdings never enter the mesh catalog and never appear
// in GET /madnetwork/v0/holdings. A device serves only what this server vouches
// for, so telling the wider community about it would be a promise we cannot
// keep, and the swarm reads a holder that refuses as a holder that is broken.

// maxListenerHoldings bounds one push. A device's cache is not unbounded — the
// shipped ceiling is 2 GiB, which is a few hundred files — so this is generous
// by two orders of magnitude and exists to stop one client turning a POST into a
// bulk insert nobody asked for. It sits comfortably inside decodeJSON's 1 MiB
// body cap, so the two limits cannot contradict each other.
const maxListenerHoldings = 10000

// madnetworkPutHoldings handles POST /api/madnetwork/holdings: a device saying
// what is in its cache right now.
//
// A complete statement, not a delta — the store replaces the whole set — because
// a delta needs both ends to agree about a history neither of them keeps. An
// empty list is therefore meaningful: it is a device whose cache has been swept,
// and it must stop being offered.
func (h *handler) madnetworkPutHoldings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		NodeKey string   `json:"node_key"`
		Name    string   `json:"name"`
		Hashes  []string `json:"hashes"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	id := auth.FromContext(r.Context())
	if id == nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	key, err := federation.NormalizeKey(body.NodeKey)
	if err != nil {
		http.Error(w, "node_key must be a 64-character hex ed25519 public key", http.StatusBadRequest)
		return
	}
	// This server is not one of its own listener devices. Refused with a reason
	// rather than stored, because the row would otherwise advertise us to
	// ourselves and outrank the catalog entry that describes the same bytes
	// properly.
	if key == h.federation.Info().PublicKey {
		http.Error(w, "a node cannot register itself as one of its own devices", http.StatusBadRequest)
		return
	}
	if len(body.Hashes) > maxListenerHoldings {
		http.Error(w, "too many hashes in one push", http.StatusRequestEntityTooLarge)
		return
	}
	if err := h.madnetwork.PutListenerHoldings(r.Context(), key, id.UserID, body.Name,
		body.Hashes, time.Now().Unix()); err != nil {
		log.Printf("madnetwork: record listener holdings for %s: %v", id.Username, err)
		http.Error(w, "could not record what this device holds", http.StatusInternalServerError)
		return
	}
	// refresh_after tells the client our cadence instead of making it guess one:
	// stop pushing for longer than this and the device stops being offered, which
	// is the whole retention mechanism (federation.ListenerHoldingsTTL). Half the
	// window, for the reason a token renews at half-life — one missed push should
	// cost a retry, not the advertisement.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"refresh_after": int64(federation.ListenerHoldingsTTL.Seconds()) / 2,
	})
}

// madnetworkHolders handles GET /api/madnetwork/holders/{hash}: who this server
// knows of that holds one blob, and how big it is — a device's fetch plan.
//
// The common case needs no call at all: a browse row already carries
// versions[].holders[].key beside the rendition's hash and size. This answers
// the case where the row is not to hand — a playlist item, a queue restored from
// disk — and the case the row cannot answer, which is this server's OTHER
// devices, since those are deliberately absent from everything the catalog
// publishes.
//
// An empty list is a normal answer and not a 404: the caller's fallback is
// GET /api/madnetwork/stream/{hash}, the relay this server has always run on its
// behalf, which works whether or not anybody else is reachable.
func (h *handler) madnetworkHolders(w http.ResponseWriter, r *http.Request) {
	hash := strings.ToLower(chi.URLParam(r, "hash"))
	if !isSHA256Hex(hash) {
		http.Error(w, "invalid hash", http.StatusBadRequest)
		return
	}
	size, providers, err := h.madnetwork.MadnetworkBlobProviders(r.Context(), hash)
	if err != nil {
		log.Printf("madnetwork: holders of %s: %v", hash, err)
		http.Error(w, "could not look up holders", http.StatusInternalServerError)
		return
	}
	holders := make([]madnetworkBlobHolder, 0, len(providers))
	for _, p := range providers {
		if p.PublicKey == "" {
			continue // unaddressable: a fetch plan cannot dial a node with no key
		}
		holders = append(holders, madnetworkBlobHolder{
			Key:      p.PublicKey,
			Name:     p.Display(),
			LastSeen: p.LastSeen,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"hash":    hash,
		"size":    size,
		"holders": holders,
	})
}

// madnetworkBlobHolder is one node a device may fetch chunks from. Deliberately
// not the browse page's madnetworkHolder: that one carries display state (self,
// reachable) for a UI, this one is an address to dial.
type madnetworkBlobHolder struct {
	Key      string `json:"key"`
	Name     string `json:"name,omitempty"`
	LastSeen int64  `json:"last_seen,omitempty"`
}
