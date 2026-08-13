//go:build !nofederation

package federation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The write half of the reactive down-mark (§Availability, "Reactive down-mark
// + the ping floor"): who may be marked, when, and by which contact.

func downMarkNode(store PeerStore, rt http.RoundTripper) *Node {
	return &Node{
		store:       store,
		logger:      log.New(io.Discard, "", 0),
		intervals:   defaultIntervals,
		timeouts:    defaultTimeouts,
		discovery:   Discovery{Budget: -1}.withDefaults(defaultDiscovery), // pulls off: the floor is what we are testing
		pullNow:     map[string]struct{}{},
		floorPinged: map[string]time.Time{},
		transferCtx: context.Background(),
		client:      &http.Client{Transport: rt},
	}
}

// meshDown fails every request the way an unreachable mesh node does — through
// the dialer, which is what makes it connect-class.
type meshDown struct{}

func (meshDown) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, fmt.Errorf("%w: no route to this node", errMeshDial)
}

// partialMesh answers for the hosts in live and fails to dial for everything
// else — one node up, one node gone, which is the only shape in which a failure
// is evidence about the node rather than about us.
type partialMesh struct {
	live map[string]bool
	reqs map[string]int
}

func (p *partialMesh) RoundTrip(r *http.Request) (*http.Response, error) {
	host := r.URL.Hostname()
	p.reqs[host]++
	if !p.live[host] {
		return nil, fmt.Errorf("%w: no route to this node", errMeshDial)
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(`{"name":"answering node"}`)),
		Header:     http.Header{},
		Request:    r,
	}, nil
}

// hexKey builds a 64-character key from a hex label. The floor ping derives a
// mesh address from a source's key, and k()'s readable labels are not hex.
func hexKey(label string) string {
	return label + strings.Repeat("0", 64-len(label))
}

func hostOf(t *testing.T, key string) string {
	t.Helper()
	addr, err := AddrForKeyHex(key)
	if err != nil {
		t.Fatalf("address for %s: %v", key, err)
	}
	return addr.String()
}

// TestDownMarkGuardIsRelative is the self-protection rule. One node silent while
// another answers is evidence about that node; everything silent is evidence
// about us and must mark nobody, or our own outage paints the whole community
// dead on the page.
func TestDownMarkGuardIsRelative(t *testing.T) {
	ms := newMemStore()
	n := downMarkNode(ms, meshDown{})

	n.observeUnreachable(hexKey("b"))
	if got := ms.markOf(hexKey("b")); got != 0 {
		t.Errorf("marked with nobody having answered us (mark = %d) — that is our own "+
			"outage, and it must accuse nobody", got)
	}

	// The only node that answered is the one that just failed: a flapping link
	// says nothing about anyone else, so it is not evidence either.
	n.noteContact(hexKey("b"))
	n.observeUnreachable(hexKey("b"))
	if got := ms.markOf(hexKey("b")); got != 0 {
		t.Errorf("marked on its own answer (mark = %d); the guard needs some OTHER node", got)
	}

	// Somebody else answered just now: the failure is finally about the node.
	n.noteContact(hexKey("a"))
	n.observeUnreachable(hexKey("b"))
	if ms.markOf(hexKey("b")) == 0 {
		t.Error("a failure went unrecorded while another node was answering us — this " +
			"is the case the down-mark exists for")
	}

	// The same answer, stale: it no longer says the mesh works for us NOW.
	n.contactMu.Lock()
	n.lastReplyAt = time.Now().Add(-10 * n.guardWindow())
	n.contactMu.Unlock()
	n.observeUnreachable(k("other"))
	if got := ms.markOf(k("other")); got != 0 {
		t.Errorf("marked on a contact from ten guard windows ago (mark = %d)", got)
	}
}

// TestConnectFailureIsOnlyTheDial: the mark is written for failures that mean
// "the node is not there", and for nothing else. A 429 never reaches this
// function — it is an answer, and answers are liveness — but a cancelled dial
// does, and blaming a holder for a connection we tore down ourselves is the
// hedging bug in another costume.
func TestConnectFailureIsOnlyTheDial(t *testing.T) {
	dial := fmt.Errorf("%w: connection refused", errMeshDial)
	if !connectFailure(dial) {
		t.Error("a failed dial was not read as connect-class")
	}
	if !connectFailure(fmt.Errorf("get %q: %w", "http://x/", dial)) {
		t.Error("a dial failure wrapped by the http client lost its class")
	}
	if connectFailure(errors.New("unexpected EOF")) {
		t.Error("a read error was read as connect-class; that is the scheduler's to judge")
	}
	if connectFailure(nil) {
		t.Error("nil was read as a failure")
	}
	if connectFailure(fmt.Errorf("%w: %w", errMeshDial, context.Canceled)) {
		t.Error("a dial WE cancelled was blamed on the holder")
	}
	if !connectFailure(fmt.Errorf("%w: %w", errMeshDial, context.DeadlineExceeded)) {
		t.Error("a dial timeout was not read as connect-class — it is the observation " +
			"this whole mechanism is built on")
	}
}

// TestPingFloorReachesEveryCachedSource covers the floor: a member the pull
// rotation could not reach still gets one cheap ping per cycle, its answer is an
// ordinary observation, and its failure is a write site of the mark.
//
// Discovery budget is off here, so nothing but the floor makes contact.
func TestPingFloorReachesEveryCachedSource(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	rt := &partialMesh{live: map[string]bool{}, reqs: map[string]int{}}
	n := downMarkNode(ms, rt)

	// Stale enough to be due: older than one catalog cycle.
	stale := time.Now().Add(-2 * defaultIntervals.CatalogSync).Unix()
	live := seedSource(t, ms, hexKey("a"), stale)
	seedSource(t, ms, hexKey("b"), stale)
	fresh := seedSource(t, ms, hexKey("c"), time.Now().Unix()) // seen a moment ago: not due
	rt.live[hostOf(t, hexKey("a"))] = true

	n.members = &memberSet{keys: map[string]struct{}{
		hexKey("a"): {}, hexKey("b"): {}, hexKey("c"): {},
	}, built: time.Now()}

	// One cycle's worth of rounds: the floor spends a fraction of the source list
	// per round, so "every cached source once per cycle" is a claim about the
	// cycle and has to be asserted over one.
	rounds := int(n.intervals.CatalogSync / n.intervals.Refresh)
	for i := 0; i < rounds; i++ {
		n.syncSources(ctx, nil)
	}

	after := sourcesByID(t, ms)
	if after[live.ID].LastSeen <= stale {
		t.Error("the floor ping did not advance last_seen for a node that answered — " +
			"success is an ordinary observation")
	}
	if got := ms.markOf(hexKey("a")); got != 0 {
		t.Errorf("a node that answered was marked unreachable (mark = %d)", got)
	}
	if ms.markOf(hexKey("b")) == 0 {
		t.Error("a member that could not be dialled was not marked; without this the " +
			"floor only ever delivers good news")
	}
	if rt.reqs[hostOf(t, hexKey("c"))] != 0 {
		t.Error("a source seen inside the cycle was pinged — the floor is a floor, " +
			"not a poll")
	}
	if after[fresh.ID].LastSeen < time.Now().Unix()-5 {
		t.Error("the fresh source's clock moved, which nothing in this round should touch")
	}

	// Once each across the whole cycle, never once per round: the floor's own
	// clock is what stops a node that neither answers nor earns a mark from being
	// retried every minute while the due ones starve behind it.
	for _, label := range []string{"a", "b"} {
		if got := rt.reqs[hostOf(t, hexKey(label))]; got != 1 {
			t.Errorf("node %q was pinged %d times in one cycle of %d rounds, want exactly 1",
				label, got, rounds)
		}
	}
}

// TestFloorBudgetSpreadsOverTheCycle: the cost is a handful of tiny requests per
// round rather than a burst per cycle, and every source is still reached once a
// cycle by construction.
func TestFloorBudgetSpreadsOverTheCycle(t *testing.T) {
	n := downMarkNode(newMemStore(), meshDown{})
	rounds := int(n.intervals.CatalogSync / n.intervals.Refresh) // 15 at the defaults
	for _, tc := range []struct{ sources, want int }{
		{0, 0},
		{1, 1},
		{rounds, 1},
		{rounds * 3, 3},
		{200, (200 + rounds - 1) / rounds},
	} {
		if got := n.floorBudget(tc.sources); got != tc.want {
			t.Errorf("floorBudget(%d) = %d, want %d", tc.sources, got, tc.want)
		}
	}
	if got := n.floorBudget(defaultDiscovery.Cap) * rounds; got < defaultDiscovery.Cap {
		t.Errorf("a cycle's worth of budget covers %d of %d cached sources — the floor "+
			"must reach every one of them", got, defaultDiscovery.Cap)
	}
}

func sourcesByID(t *testing.T, ms *memStore) map[int64]*ExternalNode {
	t.Helper()
	list, err := ms.ListCatalogSources(context.Background())
	if err != nil {
		t.Fatalf("list sources: %v", err)
	}
	out := map[int64]*ExternalNode{}
	for _, s := range list {
		out[s.ID] = s
	}
	return out
}
