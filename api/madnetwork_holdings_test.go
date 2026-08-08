package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/federation"
)

// The household's tracker (docs/architecture/federation.md §"The household",
// "Being found"). The store's job is covered in the database package; what is
// under test here is what the API owns — who may push, what a push may contain,
// and the shape a device's fetch plan comes back in.

// holdingsServer mounts the API with an injected identity, since both endpoints
// exist for an authenticated person's device rather than for a friended node.
func holdingsServer(t *testing.T, mad *fakeMadnetwork, fed *fakeFederation, id *auth.Identity) *httptest.Server {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(auth.WithIdentity(req.Context(), id)))
		})
	})
	RegisterAPI(r, Deps{Madnetwork: mad, Federation: fed})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func postHoldings(t *testing.T, srv *httptest.Server, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+"/api/madnetwork/holdings", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// TestPutListenerHoldingsRecordsWhatTheDeviceSaid: the push arrives whole — the
// device's key, its name, its hashes — attributed to the account that made the
// call rather than to anything the body claims. A device speaks for its cache;
// only the session speaks for whose device it is.
func TestPutListenerHoldingsRecordsWhatTheDeviceSaid(t *testing.T) {
	device := strings.Repeat("ab", 32)
	one, two := strings.Repeat("11", 32), strings.Repeat("22", 32)
	mad := &fakeMadnetwork{}
	srv := holdingsServer(t, mad, &fakeFederation{}, &auth.Identity{UserID: 7, Username: "kian"})

	resp := postHoldings(t, srv, `{"node_key":"`+device+`","name":"kian's phone","hashes":["`+one+`","`+two+`"]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push = %d, want 200", resp.StatusCode)
	}
	var got struct {
		OK           bool  `json:"ok"`
		RefreshAfter int64 `json:"refresh_after"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	// The client is told our cadence rather than left to guess one: stop pushing
	// for longer than the window and the device stops being offered.
	if !got.OK || got.RefreshAfter <= 0 ||
		got.RefreshAfter >= int64(federation.ListenerHoldingsTTL.Seconds()) {
		t.Errorf("reply = %+v, want ok with a refresh_after inside the TTL", got)
	}
	if mad.putDeviceKey != device || mad.putDeviceName != "kian's phone" {
		t.Errorf("recorded device = %q/%q, want %q/%q",
			mad.putDeviceKey, mad.putDeviceName, device, "kian's phone")
	}
	if mad.putUserID != 7 {
		t.Errorf("attributed to user %d, want 7 — the session decides whose device this is", mad.putUserID)
	}
	if len(mad.putHoldings) != 2 || mad.putHoldings[0] != one || mad.putHoldings[1] != two {
		t.Errorf("recorded hashes = %v, want both as sent", mad.putHoldings)
	}
}

// TestPutListenerHoldingsAcceptsAnEmptyCache: a device whose cache was swept is
// making a statement, not a mistake. Treating it as a no-op would leave it
// advertised as holding what it deleted.
func TestPutListenerHoldingsAcceptsAnEmptyCache(t *testing.T) {
	mad := &fakeMadnetwork{putHoldings: []string{"stale"}}
	srv := holdingsServer(t, mad, &fakeFederation{}, &auth.Identity{UserID: 1})

	resp := postHoldings(t, srv, `{"node_key":"`+strings.Repeat("cd", 32)+`","hashes":[]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("empty push = %d, want 200", resp.StatusCode)
	}
	if len(mad.putHoldings) != 0 {
		t.Errorf("recorded %v, want the advertisement cleared", mad.putHoldings)
	}
}

// TestPutListenerHoldingsRejectsBadRequests covers what the handler refuses
// before the store ever sees it, including the one refusal that is about
// identity rather than syntax: this server is not one of its own devices.
func TestPutListenerHoldingsRejectsBadRequests(t *testing.T) {
	self := strings.Repeat("ef", 32)
	many := make([]string, maxListenerHoldings+1)
	for i := range many {
		many[i] = `"` + strings.Repeat("11", 32) + `"`
	}

	for _, tc := range []struct {
		name, body string
		want       int
	}{
		{"not hex", `{"node_key":"nonsense"}`, http.StatusBadRequest},
		{"missing key", `{"hashes":[]}`, http.StatusBadRequest},
		{"our own key", `{"node_key":"` + self + `"}`, http.StatusBadRequest},
		{"too many hashes", `{"node_key":"` + strings.Repeat("ab", 32) + `","hashes":[` +
			strings.Join(many, ",") + `]}`, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mad := &fakeMadnetwork{}
			srv := holdingsServer(t, mad, &fakeFederation{selfKey: self}, &auth.Identity{UserID: 1})
			if got := postHoldings(t, srv, tc.body).StatusCode; got != tc.want {
				t.Errorf("push = %d, want %d", got, tc.want)
			}
			if mad.putDeviceKey != "" {
				t.Errorf("stored %q despite a bad request", mad.putDeviceKey)
			}
		})
	}
}

// TestPutListenerHoldingsNeedsAnAccount: there is no anonymous advertisement.
// The rows exist because this server authenticated somebody.
func TestPutListenerHoldingsNeedsAnAccount(t *testing.T) {
	mad := &fakeMadnetwork{}
	srv := holdingsServer(t, mad, &fakeFederation{}, nil)
	if got := postHoldings(t, srv, `{"node_key":"`+strings.Repeat("ab", 32)+`"}`).StatusCode; got != http.StatusUnauthorized {
		t.Errorf("anonymous push = %d, want 401", got)
	}
	if mad.putDeviceKey != "" {
		t.Error("stored an advertisement for an anonymous caller")
	}
}

func getHolders(t *testing.T, srv *httptest.Server, hash string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/api/madnetwork/holders/" + hash)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	var body map[string]any
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
	}
	return resp, body
}

// TestHoldersIsAFetchPlan: keys and a size, which is exactly what EnsureBlobFrom
// needs and no more. The display name follows BlobProvider.Display, so a device
// that only ever claimed its own name still reads as something.
func TestHoldersIsAFetchPlan(t *testing.T) {
	hash := strings.Repeat("aa", 32)
	mad := &fakeMadnetwork{
		providerSize: 4242,
		providers: map[string][]*federation.BlobProvider{hash: {
			{PublicKey: strings.Repeat("11", 32), Name: "big server", LastSeen: 100},
			{PublicKey: strings.Repeat("22", 32), HeardName: "kian's phone", LastSeen: 90},
			// No key: in the tracker, but not something a fetch can dial.
			{HeardName: "nameless", LastSeen: 80},
		}},
	}
	srv := holdingsServer(t, mad, &fakeFederation{}, &auth.Identity{UserID: 1})

	resp, body := getHolders(t, srv, hash)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("holders = %d, want 200", resp.StatusCode)
	}
	if size, _ := body["size"].(float64); int64(size) != 4242 {
		t.Errorf("size = %v, want 4242", body["size"])
	}
	holders, _ := body["holders"].([]any)
	if len(holders) != 2 {
		t.Fatalf("holders = %d, want 2 — a provider with no key cannot be dialled", len(holders))
	}
	first, _ := holders[0].(map[string]any)
	if first["key"] != strings.Repeat("11", 32) || first["name"] != "big server" {
		t.Errorf("first holder = %+v, want the keyed server", first)
	}
	second, _ := holders[1].(map[string]any)
	if second["name"] != "kian's phone" {
		t.Errorf("second holder name = %v, want the device's own claim", second["name"])
	}
}

// TestHoldersOnAnUnknownHashIsEmptyNotMissing pins the decision behind the
// endpoint: nobody holding a blob is a normal answer, because the caller's
// fallback is the relay this server has always run on its behalf. A 404 would
// read as "no such content" and send a client looking for a bug.
func TestHoldersOnAnUnknownHashIsEmptyNotMissing(t *testing.T) {
	srv := holdingsServer(t, &fakeMadnetwork{}, &fakeFederation{}, &auth.Identity{UserID: 1})
	resp, body := getHolders(t, srv, strings.Repeat("bb", 32))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("holders of an unheld hash = %d, want 200", resp.StatusCode)
	}
	if holders, _ := body["holders"].([]any); len(holders) != 0 {
		t.Errorf("holders = %v, want an empty list", holders)
	}
}

// TestHoldersRejectsABadHash: the path parameter is checked before any lookup.
func TestHoldersRejectsABadHash(t *testing.T) {
	srv := holdingsServer(t, &fakeMadnetwork{}, &fakeFederation{}, &auth.Identity{UserID: 1})
	if resp, _ := getHolders(t, srv, "nonsense"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("holders of a bad hash = %d, want 400", resp.StatusCode)
	}
}
