//go:build !nofederation

package federation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

// F9 item 2. The announce is a PUSH, not gossip, and it is first-hand — the
// sender speaks about itself, which is what lets it mint a holdings row where a
// freshness hint may not, and equally why it is never relayed.

func TestAnnounceDrainsCompletionsOnceAndReadsPartialsLive(t *testing.T) {
	const (
		landed   = "1111111111111111111111111111111111111111111111111111111111111111"
		inFlight = "2222222222222222222222222222222222222222222222222222222222222222"
	)
	n := &Node{transfers: map[string]*transfer{}}

	tr := newTransfer(inFlight, "final", "final.part")
	tr.setMeta(40, "")
	tr.beginChunks(buildLayout(40, 10, nil), nil)
	tr.chunkDone(0, 10)
	n.transfers[inFlight] = tr

	n.noteAcquired(landed)
	n.noteAcquired(landed) // idempotent — a set, not a log
	n.noteAcquired("not a hash")

	msg := n.drainAnnounce()
	if want := []string{landed}; !reflect.DeepEqual(msg.Hashes, want) {
		t.Fatalf("Hashes = %v, want %v", msg.Hashes, want)
	}
	if want := []string{inFlight}; !reflect.DeepEqual(msg.Partial, want) {
		t.Fatalf("Partial = %v, want %v", msg.Partial, want)
	}

	// Completions are a delta and drain; partials are a live reading and do not.
	again := n.drainAnnounce()
	if len(again.Hashes) != 0 {
		t.Errorf("a completion announced twice: %v", again.Hashes)
	}
	if want := []string{inFlight}; !reflect.DeepEqual(again.Partial, want) {
		t.Errorf("Partial = %v, want it still read live %v", again.Partial, want)
	}
}

// announceFixture builds a node whose store knows one friend, and a helper that
// posts an announce as that friend (identity comes from the connection).
func announceFixture(t *testing.T) (*Node, *memStore, string, func(announceMessage) *httptest.ResponseRecorder) {
	t.Helper()
	ms := newMemStore()
	friend := nodeKeyN(0x11)
	if _, err := ms.InsertFederationPeer(context.Background(),
		&Peer{PublicKey: friend, State: PeerFriend, LastSeen: time.Now().Unix()}); err != nil {
		t.Fatalf("insert friend: %v", err)
	}
	n := &Node{
		store:     ms,
		transfers: map[string]*transfer{},
		logger:    log.New(io.Discard, "", 0),
		intervals: Intervals{MembershipTTL: time.Hour},
	}
	// Pre-install the perimeter so community() answers from the memo. Rebuilding
	// it would read the node's own key off the mesh, which a unit-test node does
	// not have; the friend arm never gets that far, and this keeps the
	// not-a-member arm reachable too.
	n.installMembers(newMemberSet(nodeKeyN(0x01), nil, nil, nil, time.Now()))

	post := func(key string) func(announceMessage) *httptest.ResponseRecorder {
		return func(msg announceMessage) *httptest.ResponseRecorder {
			t.Helper()
			addr, err := AddrForKeyHex(key)
			if err != nil {
				t.Fatalf("derive address: %v", err)
			}
			body, _ := json.Marshal(msg)
			r := httptest.NewRequest(http.MethodPost, "/madnetwork/v0/announce", bytes.NewReader(body))
			r.RemoteAddr = fmt.Sprintf("[%s]:40000", addr)
			rec := httptest.NewRecorder()
			n.handleAnnounce(rec, r)
			return rec
		}
	}
	return n, ms, friend, post(friend)
}

// The trap named in the design doc: EnsureCatalogSource sets first_seen and NOT
// last_seen, so a source minted by an announce and not touched would be stale on
// arrival and filtered straight back out by StaleHolderWindow — recorded as a
// holder and never used.
func TestAnnounceMintsAHolderAndMarksItSeen(t *testing.T) {
	const hash = "3333333333333333333333333333333333333333333333333333333333333333"
	_, ms, friend, announce := announceFixture(t)

	if rec := announce(announceMessage{Protocol: ProtocolVersion, Hashes: []string{hash}}); rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}

	srcs, err := ms.ListCatalogSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var src *CatalogSource
	for _, s := range srcs {
		if s.PublicKey == friend {
			src = s
		}
	}
	if src == nil {
		t.Fatal("the announcing node was not minted as a catalog source")
	}
	if src.LastSeen <= 0 {
		t.Error("last_seen was left at zero, so StaleHolderWindow would drop the holder " +
			"we just recorded — EnsureCatalogSource sets first_seen only")
	}
	if got := ms.holdings[src.ID]; !reflect.DeepEqual(got, []string{hash}) {
		t.Errorf("holdings = %v, want %v", got, []string{hash})
	}
}

func TestAnnounceUnionsCompleteAndPartialAndDedupes(t *testing.T) {
	const (
		a = "4444444444444444444444444444444444444444444444444444444444444444"
		b = "5555555555555555555555555555555555555555555555555555555555555555"
	)
	_, ms, _, announce := announceFixture(t)

	announce(announceMessage{
		Protocol: ProtocolVersion,
		Hashes:   []string{a, a, "junk"},
		Partial:  []string{b, a},
	})

	srcs, _ := ms.ListCatalogSources(context.Background())
	got := ms.holdings[srcs[0].ID]
	if want := []string{a, b}; !reflect.DeepEqual(got, want) {
		t.Fatalf("holdings = %v, want %v", got, want)
	}
}

// An announce we cannot attribute is a claim about nobody. A guest or a
// capability-token bearer arrives with no key, and a listener node's holdings
// have their own path (the household tracker), so this endpoint refuses them.
func TestAnnounceRefusesAnUnattributableRequester(t *testing.T) {
	const hash = "6666666666666666666666666666666666666666666666666666666666666666"
	n, _, _, _ := announceFixture(t)

	addr, err := AddrForKeyHex(nodeKeyN(0x99)) // not a friend, not in the community
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(announceMessage{Protocol: ProtocolVersion, Hashes: []string{hash}})
	r := httptest.NewRequest(http.MethodPost, "/madnetwork/v0/announce", bytes.NewReader(body))
	r.RemoteAddr = fmt.Sprintf("[%s]:40000", addr)
	rec := httptest.NewRecorder()
	n.handleAnnounce(rec, r)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a node we cannot place", rec.Code)
	}
}

// Never advertise what we would refuse to serve: with cache seeding off, the
// blob endpoint answers 404, so the announce must not go out — and the pending
// set must survive, since nothing was said.
func TestAnnounceStaysSilentWhenSeedingIsOff(t *testing.T) {
	const hash = "7777777777777777777777777777777777777777777777777777777777777777"
	n, ms, _, _ := announceFixture(t)
	ms.setSeeding(true, false)
	n.noteAcquired(hash)

	peers, _ := ms.ListFederationPeers(context.Background())
	n.announceHoldings(context.Background(), peers)

	n.announceMu.Lock()
	pending := len(n.announceNew)
	n.announceMu.Unlock()
	if pending != 1 {
		t.Fatalf("pending announcements = %d, want the completion kept while seeding is off", pending)
	}
}
