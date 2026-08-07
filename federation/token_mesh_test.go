//go:build !nofederation

package federation

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Federation F7 item 9 — listener-node tokens over the real mesh
// (docs/architecture/federation.md §Principals & access, "The capability
// token"). The unit tests in token_test.go pin what a token says; these pin
// what a node *does* about one, which is a different thing and the one that
// broke every other time an access rule moved.
//
// The shape under test is the one the whole item exists for: node B is a
// stranger to node A — not its friend, and invisible to A's community walk, as a
// madplayer always is — and is served nothing at all, until it presents a vouch
// signed by a node A can place.

// meshGet fetches a mesh path from A as B, optionally presenting a capability
// token, and reports the status code with the body.
func meshGet(t *testing.T, a, b *Node, path, token string) (int, []byte) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DialContext: b.DialContext}, Timeout: meshClientTimeout}
	var (
		code int
		body []byte
	)
	waitFor(t, "request reaches node A", func() bool {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://[%s]:%d%s", a.Address(), MeshPort, path), nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if token != "" {
			req.Header.Set(TokenHeader, token)
		}
		resp, err := client.Do(req)
		if err != nil {
			return false // mesh converging
		}
		defer resp.Body.Close()
		code = resp.StatusCode
		body = readAllBody(t, resp)
		return true
	})
	return code, body
}

func readAllBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	return buf[:n]
}

// TestListenerNodeTokenBuysMembership is the item in one test: a stranger with a
// vouch from a node we can place is served the Madnetwork scope, and the same
// stranger without one is served nothing.
func TestListenerNodeTokenBuysMembership(t *testing.T) {
	storeA, storeB := newMemStore(), newMemStore()
	plain, _, a, b := scopePair(t, storeA, storeB)
	blobPath := "/madnetwork/v0/blob/" + plain

	// B is nobody: no peer row on A, and nothing in A's graph reaches it.
	if code, _ := meshGet(t, a, b, blobPath, ""); code != http.StatusNotFound {
		t.Fatalf("stranger without a token = %d, want 404", code)
	}

	// A home server that A's community DOES vouch for, mutually, behind one of
	// A's friends. It never talks to A here — only its signature does, which is
	// the point: the token travels with the bearer, not through the issuer.
	homePriv, homeKey := newSigner(t)
	vouchFor(t, storeA, k("voucher"), homeKey)
	token := mustSign(t, homePriv, homeKey, b.PublicKeyHex(), false, time.Now())

	if code, _ := meshGet(t, a, b, blobPath, token); code != http.StatusOK {
		t.Errorf("vouched listener node fetching a madnetwork blob = %d, want 200", code)
	}
	code, body := meshGet(t, a, b, "/madnetwork/v0/catalog", token)
	if code != http.StatusOK {
		t.Fatalf("vouched listener node fetching the catalog = %d, want 200", code)
	}
	var msg catalogMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(msg.Entries) != 2 {
		t.Errorf("vouched listener node's catalog = %d entries, want 2 — it may "+
			"discover exactly what it may fetch", len(msg.Entries))
	}

	// Membership, not friendship: the one thing a token must not buy. A
	// recording restricted to hand-picked nodes stays off a device this admin
	// never picked, however much the node vouching for it is trusted.
	storeA.mu.Lock()
	storeA.depths["1"] = DepthFriends
	storeA.mu.Unlock()
	if code, _ := meshGet(t, a, b, blobPath, token); code != http.StatusNotFound {
		t.Errorf("vouched listener node fetching a Direct-friends blob = %d, want 404", code)
	}
}

// TestListenerNodeTokenNeedsAnIssuerWeCanPlace: one issuer, one hop, no chain. A
// stranger vouching for a stranger is worth exactly as much as arriving with
// nothing — otherwise anyone could mint a key and self-authorize.
func TestListenerNodeTokenNeedsAnIssuerWeCanPlace(t *testing.T) {
	storeA, storeB := newMemStore(), newMemStore()
	plain, _, a, b := scopePair(t, storeA, storeB)
	blobPath := "/madnetwork/v0/blob/" + plain

	strangerPriv, strangerKey := newSigner(t)
	token := mustSign(t, strangerPriv, strangerKey, b.PublicKeyHex(), false, time.Now())
	if code, _ := meshGet(t, a, b, blobPath, token); code != http.StatusNotFound {
		t.Errorf("token from an issuer we cannot place = %d, want 404", code)
	}

	// And the branch snip carries the bearers with it: a token whose issuer we
	// *could* place stops working the moment we cannot. This is what makes the
	// one-hour lifetime enough — revoking a whole issuer never waits for it.
	homePriv, homeKey := newSigner(t)
	vouchFor(t, storeA, k("voucher"), homeKey)
	good := mustSign(t, homePriv, homeKey, b.PublicKeyHex(), false, time.Now())
	// Wait, don't assert: vouchFor writes the record, but A *accepting* it is a
	// separate step (GraphAccept, 1 minute by default) that MembershipTTL: noMemo
	// does not remove. meshGet retries only until the mesh answers at all and
	// returns the first status it gets, so a bare 200 here reads "issuer not
	// placed yet" as the verdict.
	waitFor(t, "A to place the issuer", func() bool {
		code, _ := meshGet(t, a, b, blobPath, good)
		return code == http.StatusOK
	})
	unvouch(storeA, homeKey)
	a.Nudge()
	waitFor(t, "A to stop placing the issuer", func() bool {
		code, _ := meshGet(t, a, b, blobPath, good)
		return code == http.StatusNotFound
	})
}

// TestListenerNodeTokenIsBoundToTheConnection: the check that makes a leaked
// token worthless. B presents a perfectly valid token — signed by an issuer A
// places, unexpired, unedited — that names somebody else as its bearer.
func TestListenerNodeTokenIsBoundToTheConnection(t *testing.T) {
	storeA, storeB := newMemStore(), newMemStore()
	plain, _, a, b := scopePair(t, storeA, storeB)

	homePriv, homeKey := newSigner(t)
	vouchFor(t, storeA, k("voucher"), homeKey)
	_, someoneElse := newSigner(t)
	stolen := mustSign(t, homePriv, homeKey, someoneElse, false, time.Now())

	if code, _ := meshGet(t, a, b, "/madnetwork/v0/blob/"+plain, stolen); code != http.StatusNotFound {
		t.Errorf("a token presented by a node other than its bearer = %d, want 404", code)
	}
}

// TestListenerNodeTokenCarriesTheAccountACL: the home server's own ACL travels
// with the bearer, so a restricted account cannot widen its reach by moving to a
// device. Guest-only in, guest-playable content only out.
func TestListenerNodeTokenCarriesTheAccountACL(t *testing.T) {
	storeA, storeB := newMemStore(), newMemStore()
	plain, guest, a, b := scopePair(t, storeA, storeB)

	homePriv, homeKey := newSigner(t)
	vouchFor(t, storeA, k("voucher"), homeKey)
	token := mustSign(t, homePriv, homeKey, b.PublicKeyHex(), true, time.Now())

	// Same convergence race as the sibling above: wait for A to place the issuer
	// rather than reading the pre-acceptance 404 as the answer.
	waitFor(t, "A to place the issuer", func() bool {
		code, _ := meshGet(t, a, b, "/madnetwork/v0/blob/"+guest, token)
		return code == http.StatusOK
	})
	if code, _ := meshGet(t, a, b, "/madnetwork/v0/blob/"+plain, token); code != http.StatusNotFound {
		t.Errorf("guest-only bearer fetching ordinary content = %d, want 404", code)
	}
}

// TestListenerNodeTokenExpiresOnTheWire: an expired token is not a weaker
// credential, it is no credential — the bearer falls back to whatever it is
// without one, which here is nothing.
func TestListenerNodeTokenExpiresOnTheWire(t *testing.T) {
	storeA, storeB := newMemStore(), newMemStore()
	plain, _, a, b := scopePair(t, storeA, storeB)

	homePriv, homeKey := newSigner(t)
	vouchFor(t, storeA, k("voucher"), homeKey)
	stale := mustSign(t, homePriv, homeKey, b.PublicKeyHex(), false,
		time.Now().Add(-TokenTTL-2*TokenClockSkew))

	if code, _ := meshGet(t, a, b, "/madnetwork/v0/blob/"+plain, stale); code != http.StatusNotFound {
		t.Errorf("expired token = %d, want 404", code)
	}
}
