//go:build !nofederation

package federation

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"
)

// Member quotas (F7 item 6, docs/architecture/federation-swarm.md §Distribution, "What
// a member may cost us"). What these pin is that both halves of the bound exist
// and that friends are outside them — the two properties the design turns on.

// TestQuotasBothCapsBind: the per-requester cap is fairness inside the class, and
// the class ceiling is the bound that a sybil farm cannot multiply its way past.
// A per-identity limit alone would let N keys buy N quotas, which is the whole
// reason the second cap is here.
func TestQuotasBothCapsBind(t *testing.T) {
	// Two serves each, four in total: one node can saturate itself without
	// touching the ceiling, and three nodes cannot exceed it together.
	q := newQuotas(0, 0, 4, 2)

	var releases []func()
	admit := func(key string) bool {
		_, release, ok := q.admit(key)
		if ok {
			releases = append(releases, release)
		}
		return ok
	}

	if !admit("a") || !admit("a") {
		t.Fatal("a requester must get its own per-node allowance")
	}
	if admit("a") {
		t.Error("a third serve for one node exceeded per_member_max_transfers")
	}
	// A second node is unaffected by the first one's exhaustion — that is what
	// makes the per-node cap fairness rather than a queue.
	if !admit("b") || !admit("b") {
		t.Fatal("one node's exhausted allowance must not block another's")
	}
	// The class is now full at 4. A third key buys nothing, which is the point.
	if admit("c") {
		t.Error("a fresh key was admitted past member_max_transfers — a per-identity " +
			"limit alone is exactly what a sybil farm defeats")
	}

	releases[0]() // 4 in flight -> 3
	if !admit("c") {
		t.Error("a released slot should be reusable")
	} // back to 4, the ceiling

	// Releasing the same serve twice must credit nothing the second time, or a
	// requester could mint class capacity by finishing requests.
	releases[0]()
	q.mu.Lock()
	active := q.active
	q.mu.Unlock()
	if active != 4 {
		t.Errorf("active = %d, want 4 — a repeated release credited the class again", active)
	}
	if admit("d") {
		t.Error("the ceiling was passable after a repeated release")
	}
}

// TestQuotasUnlimitedByDefault: the shipped configuration is all zeroes, and it
// must admit everything (owner decision 2026-08-01 — the caps are opt-in).
func TestQuotasUnlimitedByDefault(t *testing.T) {
	q := newQuotas(0, 0, 0, 0)
	for i := 0; i < 64; i++ {
		rls, _, ok := q.admit(fmt.Sprintf("node-%d", i%3))
		if !ok {
			t.Fatalf("serve %d refused under an all-zero (unlimited) config", i)
		}
		if len(rls) != 0 {
			t.Fatalf("serve %d got %d limiters; unlimited must add no throttle", i, len(rls))
		}
	}
	// A nil budget is the same answer, so callers need no special case.
	if _, _, ok := (*quotas)(nil).admit("anyone"); !ok {
		t.Error("a nil *quotas must admit")
	}
}

// TestQuotasRateLimitersAreStable: a requester's own bucket has to survive its
// fetches, or the rate limit is one a caller can reset by reconnecting.
func TestQuotasRateLimitersAreStable(t *testing.T) {
	q := newQuotas(512, 128, 0, 0)

	rls, release, ok := q.admit("a")
	if !ok {
		t.Fatal("admit")
	}
	if len(rls) != 2 {
		t.Fatalf("limiters = %d, want the class bucket and the node's own", len(rls))
	}
	q.mu.Lock()
	first := q.nodes["a"].rate
	q.mu.Unlock()
	release()

	if _, release2, ok := q.admit("a"); ok {
		defer release2()
	}
	q.mu.Lock()
	second := q.nodes["a"].rate
	q.mu.Unlock()
	if first != second {
		t.Error("the requester's bucket was rebuilt between fetches — reconnecting " +
			"would reset the rate limit it is supposed to be under")
	}
}

// TestQuotasPruneKeepsBusyAndRecentNodes: the table is keyed by mesh address, so
// it needs an expiry — but one that cannot drop a node mid-fetch, and does not
// hand back a fresh bucket the moment a fetch ends.
func TestQuotasPruneKeepsBusyAndRecentNodes(t *testing.T) {
	q := newQuotas(0, 0, 0, 0)
	_, _, _ = q.admit("busy") // never released: in flight

	q.mu.Lock()
	q.nodes["recent"] = &quotaNode{seen: time.Now()}
	q.nodes["idle"] = &quotaNode{seen: time.Now().Add(-2 * quotaIdleTTL)}
	q.nodes["busy"].seen = time.Now().Add(-2 * quotaIdleTTL) // stale clock, still active
	q.pruneLocked()
	_, keptBusy := q.nodes["busy"]
	_, keptRecent := q.nodes["recent"]
	_, keptIdle := q.nodes["idle"]
	q.mu.Unlock()

	if !keptBusy {
		t.Error("pruned a requester with a serve in flight")
	}
	if !keptRecent {
		t.Error("pruned a requester that finished moments ago — its bucket must outlive " +
			"the fetch, or the limit resets on reconnect")
	}
	if keptIdle {
		t.Error("kept a long-idle requester; the table would grow with every member " +
			"that ever fetched")
	}
}

// TestAdmitServeExemptsFriends: a friend is an admin's decision and is served
// under the global cap alone. This is the anti-starvation half of the design —
// without it the nodes an admin chose queue behind the ones the graph let in.
func TestAdmitServeExemptsFriends(t *testing.T) {
	n := &Node{quotas: newQuotas(0, 0, 1, 1)}
	addr, err := AddrForKeyHex(nodeKeyN(0x51))
	if err != nil {
		t.Fatalf("derive address: %v", err)
	}
	r := httptest.NewRequest("GET", "/madnetwork/v0/blob/"+nodeKeyN(0x52), nil)
	r.RemoteAddr = fmt.Sprintf("[%s]:40000", addr)

	// The class cap is 1. A member takes it; a second member is refused.
	if _, _, ok := n.admitServe(r, MemberAudience); !ok {
		t.Fatal("the first member serve should be admitted")
	}
	if _, _, ok := n.admitServe(r, MemberAudience); ok {
		t.Error("a second member serve was admitted past member_max_transfers")
	}
	// A friend is served regardless, and takes nothing from the member budget.
	for i := 0; i < 3; i++ {
		rls, _, ok := n.admitServe(r, FriendAudience)
		if !ok {
			t.Fatalf("friend serve %d was refused by the member budget", i)
		}
		if len(rls) != 0 {
			t.Errorf("friend serve %d picked up %d member limiters", i, len(rls))
		}
	}
	n.quotas.mu.Lock()
	active := n.quotas.active
	n.quotas.mu.Unlock()
	if active != 1 {
		t.Errorf("class active = %d, want 1 — friends must not consume member slots", active)
	}
}
