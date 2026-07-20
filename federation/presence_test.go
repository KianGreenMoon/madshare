//go:build !nofederation

package federation

import (
	"testing"
	"time"
)

// The presence state machine (docs/ui/madnetwork-page.md §Presence): offline
// after 10 s of silence, online only after 10 s of demonstrated reachability,
// and an outage restarts probation.
func TestPresenceTracker(t *testing.T) {
	tr := newPresenceTracker()
	t0 := time.Unix(1_000_000, 0)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

	// Unknown peer: offline.
	if tr.Online(1, t0) {
		t.Error("unknown peer reports online")
	}

	// Probation: first success does not flip online; 10 s of successes do.
	tr.ObserveSuccess(1, at(0))
	if tr.Online(1, at(0)) || tr.Online(1, at(5)) {
		t.Error("peer online during probation")
	}
	tr.ObserveSuccess(1, at(5))
	tr.ObserveSuccess(1, at(10))
	if !tr.Online(1, at(10)) {
		t.Error("peer not online after 10s of successes")
	}
	if !tr.Online(1, at(12)) {
		t.Error("peer flapped offline between probes")
	}

	// Silence: >10 s without a success → offline.
	if tr.Online(1, at(21)) {
		t.Error("peer still online after 11s of silence")
	}

	// Return after an outage restarts probation (hysteresis).
	tr.ObserveSuccess(1, at(30))
	if tr.Online(1, at(30)) || tr.Online(1, at(35)) {
		t.Error("peer online immediately after returning from an outage")
	}
	tr.ObserveSuccess(1, at(35))
	tr.ObserveSuccess(1, at(40))
	if !tr.Online(1, at(40)) {
		t.Error("peer not online after post-outage probation")
	}

	// OnlineIDs mirrors Online; Forget drops unfriended peers.
	tr.ObserveSuccess(2, at(40))
	ids := tr.OnlineIDs(at(40))
	if len(ids) != 1 || ids[0] != 1 {
		t.Errorf("OnlineIDs = %v, want [1] (peer 2 in probation)", ids)
	}
	tr.Forget(map[int64]bool{2: true})
	if tr.Online(1, at(40)) {
		t.Error("forgotten peer still online")
	}
}
