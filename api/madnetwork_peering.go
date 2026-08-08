package api

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Peering info (docs/architecture/federation.md §"The household", "Getting onto
// the mesh at all").
//
// Yggdrasil peers are not discovered: a node dials what it was configured to
// dial, so a fresh device with an empty peer list sits on an island and every
// other part of level 2b is unreachable. Of the three answers to that, this is
// the only one that asks nothing of a person who does not know what an underlay
// is — they signed in, and signing in is enough.
//
// What is handed out is not a secret. A peer URI is an address whose entire
// purpose is to be dialled, the account holder could read it off this node's
// config if they ran it, and yggdrasil authenticates every peering by key
// regardless of how it was found. Hence the default is on
// ([yggdrasil].share_peers) rather than an opt-in.

// Peering is what this node offers a signed-in device so it can reach the mesh.
//
// Deliberately not config.YggdrasilConfig: that struct also carries the identity
// key's path, and a handler that never needs to know where the private key lives
// should not be handed it.
type Peering struct {
	// Share is [yggdrasil].share_peers — false turns the endpoint off entirely.
	Share bool
	// Peers is [yggdrasil].shared_peers, already resolved from peers when unset.
	Peers []string
	// Listen is [yggdrasil].listen — this node's own underlay listeners, which a
	// device can dial directly. Wildcard binds are rewritten per request; see
	// dialableListen.
	Listen []string
}

// madnetworkPeering handles GET /api/madnetwork/peering.
//
// Two refusals that are not the same thing, and both are worth being able to
// tell apart from a client: a **404** means this operator switched sharing off,
// so stop asking; a **200 with empty lists** means sharing is on and this node
// has nothing to offer, which is the honest answer for a node that was itself
// only ever reached over the mesh.
func (h *handler) madnetworkPeering(w http.ResponseWriter, r *http.Request) {
	if h.peering == nil || !h.peering.Share {
		http.Error(w, "this node does not share its peering", http.StatusNotFound)
		return
	}
	listen := make([]string, 0, len(h.peering.Listen))
	for _, uri := range h.peering.Listen {
		if dialable, ok := dialableListen(uri, r.Host); ok {
			listen = append(listen, dialable)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"peers":  append([]string{}, h.peering.Peers...),
		"listen": listen,
	})
}

// dialableListen turns one configured listener URI into something the caller can
// actually dial, or reports that it cannot.
//
// The case that makes this necessary is the ordinary one: a backbone node writes
// listen = ["tls://0.0.0.0:12345"], which is a correct bind and a useless
// address. The host the client just used to reach us is by construction a host
// that reaches us, so it is substituted in — the same reasoning a reverse proxy
// applies to a redirect, and better than any answer this node could derive by
// enumerating its own interfaces, since only the caller knows which of them it
// can see.
//
// A unix socket is dropped: it is a real listener and not an address anybody
// else can dial.
func dialableListen(uri, requestHost string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme == "" || u.Scheme == "unix" {
		return "", false
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil {
		host, port = u.Host, ""
	}
	if !isWildcardHost(host) {
		return uri, true // an address the operator chose; leave it alone
	}
	via := requestHost
	if h, _, err := net.SplitHostPort(via); err == nil {
		via = h
	}
	via = strings.Trim(via, "[]")
	if via == "" {
		return "", false
	}
	if port == "" {
		u.Host = hostOnly(via)
	} else {
		u.Host = net.JoinHostPort(via, port)
	}
	return u.String(), true
}

// isWildcardHost reports the binds that mean "every interface" and therefore
// name none.
func isWildcardHost(host string) bool {
	switch strings.Trim(host, "[]") {
	case "", "0.0.0.0", "::", "[::]":
		return true
	}
	return false
}

// hostOnly brackets an IPv6 literal so the rebuilt URI stays parseable.
func hostOnly(host string) string {
	if ip := net.ParseIP(host); ip != nil && ip.To4() == nil {
		return "[" + host + "]"
	}
	return host
}
