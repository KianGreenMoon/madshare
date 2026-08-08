//go:build !nofederation

package federation

// Listener-node capability tokens (federation F7 item 9): one signed sentence,
// "bearer key K is my user until T", issued by a home server over its own node
// identity. Design: docs/architecture/federation.md §Principals & access, "The
// capability token".
//
// This is the only credential in madnetwork, and it exists for exactly one
// shape the rest of the access model cannot express. Every other requester is
// resolved from something we hold: a friend is a row in our peer table, a member
// is a key our own graph walk places in our component. A madplayer is neither —
// it publishes no friend list and appears in nobody else's, so there is nothing
// to walk to. The token is a node we *can* place saying "and this one is mine".
//
// What keeps it from being a general-purpose bearer credential is that it is
// bound to a key, not to a secret. A token names the bearer's node key, and the
// verifier checks that key against the mesh address the request actually arrived
// from — which is self-certifying. So a leaked token is worthless: using it
// requires the bearer's private key, and whoever has that did not need the token
// to impersonate anything. It is a vouch, not a password, and it is never
// re-presented onward by whoever receives it.
//
// Absolute timestamps are on the wire here, which the gossip freshness hints
// deliberately avoid (ages, "so clocks need not agree"). It is unavoidable —
// "until T" is the whole content of the claim and a bearer cannot be trusted to
// report its own token's age — so the skew is bounded instead: [TokenClockSkew]
// slack on the live checks, and a claimed lifetime that is validated with no
// clock at all (ExpiresAt - IssuedAt is a property of the token itself).

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

const (
	// TokenHeader carries the token on every mesh request a listener node makes.
	// A request without it is resolved exactly as before — the header is an
	// addition to the audience ladder, never a replacement for it.
	TokenHeader = "Madnetwork-Token"

	// TokenTTL is how long an issued token is valid: one hour (decided
	// 2026-08-01). The lifetime is *not* the main revocation mechanism, which is
	// why it can be this short without costing anything — blocking a home server
	// revokes every token it ever issued on the very next request, because the
	// verifier re-checks the issuer's community standing each time. The expiry
	// covers only the case that belongs to one node: a home server revoking one
	// of its own users (account disabled, permission withdrawn, phone lost).
	TokenTTL = time.Hour

	// TokenRenewAfter is when a listener node should ask for a fresh token —
	// half-life rather than expiry, so a transient outage of the home server
	// costs a renewal attempt instead of a service interruption. Advisory: it is
	// published to the client in the issuance response, and no verifier enforces
	// it.
	TokenRenewAfter = TokenTTL / 2

	// MaxTokenLifetime is the longest claimed lifetime a verifier will honour,
	// regardless of what the issuer stamped. Defense in depth against a home
	// server that is honest but misconfigured: a token grants no more than its
	// issuer already has, so a long-lived one is not an escalation, but it does
	// make that issuer's own revocation of its own user arbitrarily slow, and a
	// node is entitled to refuse to be the instrument of that.
	//
	// Checked as ExpiresAt-IssuedAt, which involves no clock at all — it is a
	// statement the token makes about itself.
	MaxTokenLifetime = 2 * TokenTTL

	// TokenClockSkew is the slack allowed on the two checks that do consult a
	// clock. Nodes have no synchronised time and none is worth requiring for a
	// credential this short-lived; without slack, an issuer a few minutes slow
	// would mint tokens that are dead on arrival everywhere.
	TokenClockSkew = 5 * time.Minute

	// maxTokenBytes bounds a decoded token document. net/http caps header size
	// long before this matters, but parsing untrusted bytes gets a bound of its
	// own on principle.
	maxTokenBytes = 4096
)

// Token verification failures. They are deliberately distinct from each other
// but *not* distinct to the requester: every one of them resolves to an audience
// that serves nothing, and no endpoint reports which check failed. Telling a
// stranger "your signature was fine but you are expired" describes our trust
// graph to somebody outside it.
var (
	ErrTokenMalformed = errors.New("federation: capability token is malformed")
	ErrTokenExpired   = errors.New("federation: capability token has expired")
	// ErrTokenLifetime is a token whose issuer claimed more time than any
	// verifier honours (see MaxTokenLifetime).
	ErrTokenLifetime = errors.New("federation: capability token claims too long a lifetime")
	// ErrTokenBearer is the check that makes a leaked token useless: the token
	// names a bearer key that does not derive to the address the request came
	// from.
	ErrTokenBearer = errors.New("federation: capability token was presented by a node other than its bearer")
)

// CapabilityToken is a home server's signed statement that one node key belongs
// to one of its users.
//
// Four claims and no more. There is no scope, no reach, no hop count and no
// audience in here: what the token buys is fixed by the verifier
// (MemberAudience, §"The capability token"), not negotiated by the issuer. The
// one exception is GuestOnly, and it can only ever narrow.
type CapabilityToken struct {
	Protocol int `json:"protocol"`
	// Issuer is the home server's node key (lowercase hex ed25519) — the key the
	// signature is verified against, and the one the verifier must be able to
	// place in its own community.
	Issuer string `json:"issuer"`
	// Bearer is the listener node's own node key. The verifier checks the
	// request's source mesh address derives from it.
	Bearer string `json:"bearer"`
	// IssuedAt and ExpiresAt are unix seconds. Both are needed: ExpiresAt is the
	// claim, IssuedAt is what lets a verifier bound the claimed lifetime without
	// trusting its own clock against the issuer's.
	IssuedAt  int64 `json:"issued_at"`
	ExpiresAt int64 `json:"expires_at"`
	// GuestOnly carries the home server's own ACL outward: true when the account
	// this token was issued for lacks content.access, so the bearer sees only
	// guest-playable recordings — the same bit an unmapped friend gets from
	// PeerAudience. Absent means "a full member", which is the ceiling anyway, so
	// nothing is gained by stripping it.
	GuestOnly bool   `json:"guest_only,omitempty"`
	Signature string `json:"sig,omitempty"`
}

// SignCapabilityToken issues a token for bearer, valid for [TokenTTL] from now,
// and returns the header value to present. issuer must be the node's own private
// key — the same identity its mesh address and its gossip records derive from.
func SignCapabilityToken(priv ed25519.PrivateKey, issuerKey, bearerKey string, guestOnly bool, now time.Time) (string, time.Time, error) {
	issuer, err := NormalizeKey(issuerKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("federation: token issuer: %w", err)
	}
	bearer, err := NormalizeKey(bearerKey)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("federation: token bearer: %w", err)
	}
	if issuer == bearer {
		// A node vouching for itself is not a listener node, it is a peering —
		// and one that would sidestep the friendship handshake entirely.
		return "", time.Time{}, errors.New("federation: a node cannot issue a capability token to itself")
	}
	expires := now.Add(TokenTTL).Truncate(time.Second)
	raw, err := signDocument(priv, CapabilityToken{
		Protocol:  ProtocolVersion,
		Issuer:    issuer,
		Bearer:    bearer,
		IssuedAt:  now.Unix(),
		ExpiresAt: expires.Unix(),
		GuestOnly: guestOnly,
	})
	if err != nil {
		return "", time.Time{}, err
	}
	return base64.RawURLEncoding.EncodeToString(raw), expires, nil
}

// ParseCapabilityToken decodes a presented header value and checks everything
// that can be checked without knowing who is asking or what we think of them:
// the signature against the issuer the token names, the claimed lifetime, and
// the expiry against now.
//
// It deliberately does *not* decide whether the token is worth anything. That
// takes two facts this function does not have — the address the request arrived
// from, and whether we can place the issuer in our community — and both belong
// to the caller (serveAudience).
func ParseCapabilityToken(header string, now time.Time) (CapabilityToken, error) {
	raw, err := base64.RawURLEncoding.DecodeString(header)
	if err != nil || len(raw) == 0 || len(raw) > maxTokenBytes {
		return CapabilityToken{}, ErrTokenMalformed
	}
	var tok CapabilityToken
	if err := json.Unmarshal(raw, &tok); err != nil {
		return CapabilityToken{}, ErrTokenMalformed
	}
	issuer, err := NormalizeKey(tok.Issuer)
	if err != nil {
		return CapabilityToken{}, ErrTokenMalformed
	}
	bearer, err := NormalizeKey(tok.Bearer)
	if err != nil {
		return CapabilityToken{}, ErrTokenMalformed
	}
	tok.Issuer, tok.Bearer = issuer, bearer
	if tok.Issuer == tok.Bearer {
		return CapabilityToken{}, ErrTokenMalformed
	}
	// Signature first: everything below is a claim by whoever signed this, and
	// checking claims before establishing who made them is how a forgery gets to
	// choose which error it provokes. Verified against the bytes as received, so
	// a field this build has never heard of still contributes to the signature
	// (see gossip.go's signingInput and the reason it works from raw JSON).
	if err := verifySignature(raw, tok.Issuer, tok.Signature); err != nil {
		return CapabilityToken{}, ErrTokenMalformed
	}
	if tok.IssuedAt <= 0 || tok.ExpiresAt <= tok.IssuedAt {
		return CapabilityToken{}, ErrTokenMalformed
	}
	// Clock-free: what the token says about itself.
	if time.Duration(tok.ExpiresAt-tok.IssuedAt)*time.Second > MaxTokenLifetime {
		return CapabilityToken{}, ErrTokenLifetime
	}
	// The two checks that do need a clock, both with skew slack. The
	// issued-in-the-future bound is what stops a skewed or lying issuer from
	// stretching a token past MaxTokenLifetime by simply post-dating it.
	if tok.IssuedAt > now.Add(TokenClockSkew).Unix() {
		return CapabilityToken{}, ErrTokenExpired
	}
	if now.Add(-TokenClockSkew).Unix() >= tok.ExpiresAt {
		return CapabilityToken{}, ErrTokenExpired
	}
	return tok, nil
}

// BoundTo checks the token against the address the request actually arrived
// from — the check that makes a stolen token worthless, since presenting one
// from anywhere but the bearer's own address fails, and presenting it from that
// address requires the bearer's private key.
func (t CapabilityToken) BoundTo(from net.IP) error {
	if from == nil {
		return ErrTokenBearer
	}
	addr, err := AddrForKeyHex(t.Bearer)
	if err != nil {
		return ErrTokenMalformed
	}
	if !addr.Equal(from) {
		return ErrTokenBearer
	}
	return nil
}

// ── The issuing side ─────────────────────────────────────────────────────────

// IssueCapabilityToken signs a vouch for one of this node's own users' devices.
// guestOnly comes from that user's account (no content.access ⇒ guest-playable
// content only), so the home server's ACL travels with the bearer instead of
// being re-litigated by every node it visits.
//
// There is nothing to store. A token is not a session: it is a statement that
// verifies from its own bytes, so issuing one creates no state to expire, sweep
// or replicate, and revoking is done by ceasing to issue (or, for the whole
// issuer at once, by whoever blocks it).
func (n *Node) IssueCapabilityToken(bearerKey string, guestOnly bool) (CapabilityGrant, error) {
	if n.signKey == nil {
		return CapabilityGrant{}, errors.New("federation: node has no identity key to sign with")
	}
	now := time.Now()
	issuer := n.PublicKeyHex()
	token, expires, err := SignCapabilityToken(n.signKey, issuer, bearerKey, guestOnly, now)
	if err != nil {
		return CapabilityGrant{}, err
	}
	bearer, _ := NormalizeKey(bearerKey)
	return CapabilityGrant{
		Token:      token,
		Issuer:     issuer,
		Bearer:     bearer,
		ExpiresAt:  expires,
		RenewAfter: now.Add(TokenRenewAfter).Truncate(time.Second),
	}, nil
}

// ListenerHoldingsTTL is derived from the renewal cadence above, and lives in
// the untagged file because the store enforces it. This ties the two together at
// compile time: shorten or lengthen TokenRenewAfter and the array lengths stop
// matching, rather than leaving a window silently behind the cadence it was
// sized from.
var _ [0]struct{} = [ListenerHoldingsTTL - 3*TokenRenewAfter]struct{}{}

// ── The presenting side ──────────────────────────────────────────────────────

// present wraps a transport so every outbound mesh request carries this node's
// capability token. It is applied to both of the node's clients in Start, which
// is the point: a per-call-site header is a rule eleven request builders have to
// remember and the twelfth will not, and the one it forgets is a fetch that
// mysteriously 404s.
//
// Attached to every request rather than to the ones judged to need it, because
// judging is both unnecessary and unreliable: serveAudience resolves a friend
// from the peer table and a member from the community walk *before* it reads the
// header, so a token presented to a node that already knows us is never
// consulted — while the node we most need to reach is by definition one that
// knows nothing about us.
//
// A nil source skips the wrapper entirely, which is what a caller with no token
// to offer should pass. The app facade installs one unconditionally instead,
// because a device acquires its token long after startup — so on a server the
// cheap path is the EMPTY token rather than the absent source, and that path is
// an atomic load and a passthrough.
func (n *Node) present(rt http.RoundTripper) http.RoundTripper {
	if n == nil || n.token == nil {
		return rt
	}
	return &tokenTransport{rt: rt, token: n.token}
}

// tokenTransport adds the token header without mutating the caller's request,
// which RoundTripper implementations are required not to do.
type tokenTransport struct {
	rt    http.RoundTripper
	token TokenSource
}

func (t *tokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tok := t.token()
	if tok == "" {
		return t.rt.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	clone.Header.Set(TokenHeader, tok)
	return t.rt.RoundTrip(clone)
}

// ── The verifying side ───────────────────────────────────────────────────────

// tokenAudience resolves a presented capability token to the audience it earns,
// completing the two checks ParseCapabilityToken could not: that the connection
// really is the bearer, and that we can place the issuer in our own community.
//
// ok false means "no token worth anything here" and is the overwhelmingly common
// answer — almost no mesh request carries one. It is never an error the caller
// should surface: a request without a token is not a failed request, it is a
// request resolved by the ordinary friend/member/outsider ladder. A non-nil err
// is a storage failure, which must not be quietly read as "no token".
//
// Refusals are logged but never distinguished on the wire (see the error
// declarations): the log is for whoever is debugging their own madplayer, and
// the response says only that this requester is served nothing.
func (n *Node) tokenAudience(r *http.Request) (Audience, bool, error) {
	header := r.Header.Get(TokenHeader)
	if header == "" {
		return Audience{}, false, nil
	}
	tok, err := ParseCapabilityToken(header, time.Now())
	if err != nil {
		n.logger.Printf("federation: capability token from %s refused: %v", remoteIP(r), err)
		return Audience{}, false, nil
	}
	if err := tok.BoundTo(remoteIP(r)); err != nil {
		n.logger.Printf("federation: capability token for bearer %s refused: %v", shortKey(tok.Bearer), err)
		return Audience{}, false, nil
	}
	set, err := n.community(r.Context())
	if err != nil {
		return Audience{}, false, err
	}
	if !set.vouches(tok.Issuer) {
		// The issuer is a node we cannot place — so this is a stranger vouching
		// for a stranger, which is worth exactly as much as the bearer arriving
		// with nothing. One issuer, one hop, no chain.
		n.logger.Printf("federation: capability token for bearer %s refused: issuer %s is not in our community",
			shortKey(tok.Bearer), shortKey(tok.Issuer))
		return Audience{}, false, nil
	}
	return tok.Audience(), true, nil
}

// Audience is what a verified token buys: membership, never friendship
// (§"The capability token"). A recording marked Direct friends was restricted to
// nodes this admin picked by hand, and a device enrolled by somebody else is not
// one of them however much its home server is trusted — so the token grants
// precisely what the community walk could not work out on its own, and not a
// step more.
func (t CapabilityToken) Audience() Audience {
	aud := MemberAudience
	if t.GuestOnly {
		aud.GuestOnly = true
	}
	return aud
}
