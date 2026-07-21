//go:build !nofederation

package federation

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestPresenceRecentlySeen: the prober-skip predicate — a peer heard from
// within the window is "recently seen" (so the prober skips its ping and lets
// an active transfer's byte flow stand in as liveness proof).
func TestPresenceRecentlySeen(t *testing.T) {
	tr := newPresenceTracker()
	now := time.Unix(2_000_000, 0)
	if tr.RecentlySeen(1, now, 5*time.Second) {
		t.Error("unknown peer reported recently seen")
	}
	tr.ObserveSuccess(1, now)
	if !tr.RecentlySeen(1, now.Add(4*time.Second), 5*time.Second) {
		t.Error("peer seen 4s ago not recently-seen within 5s")
	}
	if tr.RecentlySeen(1, now.Add(6*time.Second), 5*time.Second) {
		t.Error("peer seen 6s ago still recently-seen within 5s")
	}
}

// TestChunkPlanFeedsPresence: a delivered chunk calls onProviderAlive with the
// holder's peer id — the swarm's byte flow feeding the presence tracker.
func TestChunkPlanFeedsPresence(t *testing.T) {
	layout := &chunkLayout{offsets: []int64{0, 10, 20}}
	man := &blobManifest{Chunks: []string{"a", "b"}}
	holders := []*Peer{{ID: 42}}
	cp := newChunkPlan(man, layout, holders, false)
	var seen int64
	cp.onProviderAlive = func(peerID int64) { atomic.StoreInt64(&seen, peerID) }

	idx, ok := cp.next()
	if !ok {
		t.Fatal("next returned no chunk")
	}
	cp.succeed(idx, 0, &transfer{chunkOK: make([]bool, 2), changed: make(chan struct{})})
	if atomic.LoadInt64(&seen) != 42 {
		t.Errorf("onProviderAlive got peer %d, want 42", atomic.LoadInt64(&seen))
	}
}

// TestProbeKeepAlive: draining the /ping body lets net/http reuse the
// connection, so repeated probes ride ONE connection instead of churning a
// fresh one each time (the phase-4 regression: a 5 s cadence × undrained body =
// heavy netstack connection churn competing with transfers). This exercises the
// drain at the http.Client level against a local server counting connections.
func TestProbeKeepAlive(t *testing.T) {
	var conns int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A /ping-shaped JSON body — non-empty, so an undrained close would
		// prevent reuse.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"protocol":1,"software":"madshare","address":"200::1"}`+"\n")
	}))
	defer srv.Close()
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			atomic.AddInt64(&conns, 1)
		}
	}

	client := &http.Client{}
	probe := func() {
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		// The fix: drain before close.
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	for i := 0; i < 5; i++ {
		probe()
	}
	if got := atomic.LoadInt64(&conns); got != 1 {
		t.Errorf("opened %d connections across 5 drained probes, want 1 (keep-alive reuse)", got)
	}
}
