//go:build !nofederation

package federation

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Listener-node capability tokens (F7 item 9, token.go). The tests are grouped
// by what they defend: the four verifier checks, then the two rules about what a
// token is allowed to say.

// TestCapabilityTokenRoundTrip: a freshly issued token parses back to the claims
// it was signed with, and buys the member audience — not the issuer's friendship.
func TestCapabilityTokenRoundTrip(t *testing.T) {
	priv, issuer := newSigner(t)
	_, bearer := newSigner(t)
	now := time.Unix(1754000000, 0)

	header, expires, err := SignCapabilityToken(priv, issuer, bearer, false, now)
	if err != nil {
		t.Fatalf("SignCapabilityToken: %v", err)
	}
	if got := expires.Sub(now); got != TokenTTL {
		t.Errorf("lifetime = %v, want %v", got, TokenTTL)
	}
	tok, err := ParseCapabilityToken(header, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ParseCapabilityToken: %v", err)
	}
	if tok.Issuer != issuer || tok.Bearer != bearer {
		t.Errorf("token = issuer %s bearer %s, want %s / %s", tok.Issuer, tok.Bearer, issuer, bearer)
	}
	aud := tok.Audience()
	if !aud.InCommunity() {
		t.Error("a verified token must reach the community")
	}
	if aud.IsFriend() {
		t.Error("a token buys membership, never friendship: content restricted to " +
			"hand-picked nodes must not follow a device its admin never picked")
	}
	if aud.Distance != DepthUnlimited {
		t.Errorf("Distance = %d, want DepthUnlimited", aud.Distance)
	}
	if aud.GuestOnly {
		t.Error("an unrestricted account's token must not be guest-limited")
	}

	// A key is accepted in any case and normalized on the way in, as everywhere
	// else keys are compared.
	if _, err := ParseCapabilityToken(mustSign(t, priv, issuer, strings.ToUpper(bearer), false, now), now); err != nil {
		t.Errorf("uppercase bearer key rejected: %v", err)
	}
}

// TestCapabilityTokenCarriesTheAccountACL: guest_only travels with the bearer,
// so a restricted account cannot widen its own reach by walking its library onto
// a device.
func TestCapabilityTokenCarriesTheAccountACL(t *testing.T) {
	priv, issuer := newSigner(t)
	_, bearer := newSigner(t)
	now := time.Unix(1754000000, 0)

	tok, err := ParseCapabilityToken(mustSign(t, priv, issuer, bearer, true, now), now)
	if err != nil {
		t.Fatalf("ParseCapabilityToken: %v", err)
	}
	if !tok.Audience().GuestOnly {
		t.Fatal("a guest-only token must yield a guest-only audience")
	}
}

// TestCapabilityTokenRefusesForgery: the signature covers every claim, so
// editing one after issuance invalidates the token rather than amending it.
// Checked field by field — a forgery that only had to avoid the fields somebody
// remembered to cover is not a forgery that fails.
func TestCapabilityTokenRefusesForgery(t *testing.T) {
	priv, issuer := newSigner(t)
	_, bearer := newSigner(t)
	_, other := newSigner(t)
	now := time.Unix(1754000000, 0)
	header := mustSign(t, priv, issuer, bearer, true, now)

	for _, tc := range []struct {
		name string
		edit func(*CapabilityToken)
	}{
		{"bearer", func(tk *CapabilityToken) { tk.Bearer = other }},
		{"issuer", func(tk *CapabilityToken) { tk.Issuer = other }},
		{"expiry", func(tk *CapabilityToken) { tk.ExpiresAt = now.Add(100 * TokenTTL).Unix() }},
		{"issued_at", func(tk *CapabilityToken) { tk.IssuedAt = now.Add(-100 * TokenTTL).Unix() }},
		{"guest bit", func(tk *CapabilityToken) { tk.GuestOnly = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok, err := ParseCapabilityToken(header, now)
			if err != nil {
				t.Fatalf("parse pristine token: %v", err)
			}
			tc.edit(&tok)
			if _, err := ParseCapabilityToken(reencode(t, tok), now); err == nil {
				t.Fatalf("a token with an edited %s verified", tc.name)
			}
		})
	}

	// Signed by the wrong key entirely: the issuer named is not the issuer who
	// signed. This is the forgery that matters, because the issuer is the only
	// field a verifier looks *up* rather than merely reads.
	otherPriv, _ := newSigner(t)
	if _, err := ParseCapabilityToken(mustSign(t, otherPriv, issuer, bearer, false, now), now); err == nil {
		t.Fatal("a token signed by somebody other than its named issuer verified")
	}
}

// TestCapabilityTokenExpires covers the clock checks and their skew slack. The
// slack exists because nodes have no synchronised time and a credential that
// dies on arrival everywhere is worse than one that lives five minutes longer.
func TestCapabilityTokenExpires(t *testing.T) {
	priv, issuer := newSigner(t)
	_, bearer := newSigner(t)
	now := time.Unix(1754000000, 0)
	header := mustSign(t, priv, issuer, bearer, false, now)

	for _, tc := range []struct {
		name    string
		at      time.Time
		wantErr error
	}{
		{"at issuance", now, nil},
		{"just inside the lifetime", now.Add(TokenTTL - time.Second), nil},
		{"just past it, inside the skew slack", now.Add(TokenTTL + time.Minute), nil},
		{"well past the slack", now.Add(TokenTTL + 2*TokenClockSkew), ErrTokenExpired},
		{"before it was issued, inside the slack", now.Add(-time.Minute), nil},
		{"long before it was issued", now.Add(-2 * TokenClockSkew), ErrTokenExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCapabilityToken(header, tc.at)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("parse = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestCapabilityTokenRefusesOverlongLifetime: a verifier bounds the claimed
// lifetime regardless of what the issuer stamped. A long-lived token is not an
// escalation — it grants no more than its issuer already has — but it does make
// that issuer's revocation of its own user arbitrarily slow, and a node may
// refuse to be the instrument of that.
//
// The check reads ExpiresAt-IssuedAt, so it holds with no clock involved: a
// post-dated token is refused at any "now", which is what stops the bound from
// being sidestepped by an issuer whose clock disagrees with ours.
func TestCapabilityTokenRefusesOverlongLifetime(t *testing.T) {
	priv, issuer := newSigner(t)
	_, bearer := newSigner(t)
	now := time.Unix(1754000000, 0)

	greedy := CapabilityToken{
		Protocol:  ProtocolVersion,
		Issuer:    issuer,
		Bearer:    bearer,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(MaxTokenLifetime + time.Second).Unix(),
	}
	raw, err := signDocument(priv, greedy)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	header := encodeToken(raw)
	for _, at := range []time.Time{now, now.Add(MaxTokenLifetime), now.Add(-time.Hour)} {
		if _, err := ParseCapabilityToken(header, at); !errors.Is(err, ErrTokenLifetime) {
			t.Fatalf("parse at %v = %v, want ErrTokenLifetime", at, err)
		}
	}

	// Exactly at the bound is fine — the refusal is for claims beyond it.
	greedy.ExpiresAt = now.Add(MaxTokenLifetime).Unix()
	raw, err = signDocument(priv, greedy)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := ParseCapabilityToken(encodeToken(raw), now); err != nil {
		t.Fatalf("a token at exactly MaxTokenLifetime was refused: %v", err)
	}
}

// TestCapabilityTokenIsBoundToItsBearer is the check that makes a stolen token
// worthless: it names a key, and the verifier tests that key against the mesh
// address the request actually arrived from. Using a leaked token needs the
// bearer's private key, and whoever holds that never needed the token.
func TestCapabilityTokenIsBoundToItsBearer(t *testing.T) {
	priv, issuer := newSigner(t)
	_, bearer := newSigner(t)
	_, thief := newSigner(t)
	now := time.Unix(1754000000, 0)

	tok, err := ParseCapabilityToken(mustSign(t, priv, issuer, bearer, false, now), now)
	if err != nil {
		t.Fatalf("ParseCapabilityToken: %v", err)
	}
	bearerAddr, err := AddrForKeyHex(bearer)
	if err != nil {
		t.Fatalf("derive bearer address: %v", err)
	}
	if err := tok.BoundTo(bearerAddr); err != nil {
		t.Fatalf("the bearer's own address was refused: %v", err)
	}
	thiefAddr, err := AddrForKeyHex(thief)
	if err != nil {
		t.Fatalf("derive thief address: %v", err)
	}
	if err := tok.BoundTo(thiefAddr); !errors.Is(err, ErrTokenBearer) {
		t.Errorf("presenting a stolen token from another address = %v, want ErrTokenBearer", err)
	}
	if err := tok.BoundTo(nil); !errors.Is(err, ErrTokenBearer) {
		t.Errorf("presenting a token from no address at all = %v, want ErrTokenBearer", err)
	}
}

// TestCapabilityTokenRefusesSelfIssue: a node vouching for itself is not a
// listener node, it is a peering — and one that would sidestep the friendship
// handshake, since a token is accepted from any issuer the verifier can place.
func TestCapabilityTokenRefusesSelfIssue(t *testing.T) {
	priv, issuer := newSigner(t)
	now := time.Unix(1754000000, 0)

	if _, _, err := SignCapabilityToken(priv, issuer, issuer, false, now); err == nil {
		t.Fatal("a node issued itself a capability token")
	}
	// And the verifier does not rely on the issuer having refused: a hand-rolled
	// self-issued token is refused on the way in too.
	raw, err := signDocument(priv, CapabilityToken{
		Protocol: ProtocolVersion, Issuer: issuer, Bearer: issuer,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(TokenTTL).Unix(),
	})
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := ParseCapabilityToken(encodeToken(raw), now); err == nil {
		t.Fatal("a hand-rolled self-issued token verified")
	}
}

// TestCapabilityTokenRefusesGarbage: everything that is not a token, refused
// without panicking. These arrive from unauthenticated strangers.
func TestCapabilityTokenRefusesGarbage(t *testing.T) {
	now := time.Unix(1754000000, 0)
	for _, header := range []string{
		"", "!!!not base64!!!", encodeToken([]byte("{")), encodeToken([]byte("{}")),
		encodeToken([]byte(`{"issuer":"zz","bearer":"zz"}`)),
		encodeToken(make([]byte, maxTokenBytes+1)),
	} {
		if _, err := ParseCapabilityToken(header, now); err == nil {
			t.Errorf("garbage token %q verified", header)
		}
	}
}

// TestMemberSetVouches: which issuers a node honours. The community, plus
// ourselves — our own users' devices present tokens we signed, and a madplayer
// fetching chunks from its own home server is the ordinary case.
func TestMemberSetVouches(t *testing.T) {
	_, self := newSigner(t)
	_, member := newSigner(t)
	_, stranger := newSigner(t)

	set := &memberSet{keys: map[string]struct{}{member: {}}, self: self}
	if !set.vouches(self) {
		t.Error("a node must honour the tokens it issued itself")
	}
	if !set.vouches(member) {
		t.Error("a community member must be able to vouch for its own users")
	}
	if set.vouches(stranger) {
		t.Error("a stranger vouching for a stranger is worth nothing: one issuer, " +
			"one hop, no chain")
	}
	if set.vouches("") {
		t.Error("the empty key vouched for something")
	}
	var nilSet *memberSet
	if nilSet.vouches(self) {
		t.Error("a node with no community memo must vouch for nothing")
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

func mustSign(t *testing.T, priv ed25519.PrivateKey, issuer, bearer string, guestOnly bool, now time.Time) string {
	t.Helper()
	header, _, err := SignCapabilityToken(priv, issuer, bearer, guestOnly, now)
	if err != nil {
		t.Fatalf("SignCapabilityToken: %v", err)
	}
	return header
}

// reencode re-signs nothing: it serializes an edited token as-is, which is
// exactly what a forger can do — they can rewrite the claims, they just cannot
// produce a signature over them.
func reencode(t *testing.T, tok CapabilityToken) string {
	t.Helper()
	raw, err := json.Marshal(tok)
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}
	return encodeToken(raw)
}

// encodeToken is the wire form: base64url of the signed document, which is what
// travels in the TokenHeader.
func encodeToken(raw []byte) string { return base64.RawURLEncoding.EncodeToString(raw) }
