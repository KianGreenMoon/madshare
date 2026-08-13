//go:build !nofederation

package federation

import (
	"fmt"
	"io"
	"log"
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
	q := newQuotas(QuotaLimits{MemberMaxTransfers: 4, PerMemberMaxTransfers: 2})

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
	q := newQuotas(QuotaLimits{})
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
	q := newQuotas(QuotaLimits{MemberRateKiB: 512, PerMemberRateKiB: 128})

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
	q := newQuotas(QuotaLimits{})
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
	n := quotaTestNode(QuotaLimits{MemberMaxTransfers: 1, PerMemberMaxTransfers: 1})
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

// quotaTestNode builds the minimum Node the member budget needs, in the shape
// Start builds it: the config layer and the live budget agree, because a serve
// resolves the limits before admitting and would otherwise reset the budget the
// test just set to a cfgQuota nobody filled in.
func quotaTestNode(l QuotaLimits) *Node {
	return &Node{
		cfgQuota: l,
		quotas:   newQuotas(l),
		upRate:   &adjustableRate{},
		downRate: &adjustableRate{},
		logger:   log.New(io.Discard, "", 0),
	}
}

// TestQuotasAreLive: the four bounds are runtime knobs (docs/architecture/
// swarm-admin.md §"The member budget"), so a budget written while the node runs
// has to bind the requesters already being served — the operator tightening it
// is watching the transfer that made them tighten it.
func TestQuotasAreLive(t *testing.T) {
	q := newQuotas(QuotaLimits{PerMemberRateKiB: 128})
	rls, release, ok := q.admit("a")
	if !ok || len(rls) != 1 {
		t.Fatalf("admit = %v with %d limiters, want one per-member bucket", ok, len(rls))
	}
	defer release()
	bucket := rls[0]

	// Raising the per-member rate must reach the bucket the requester is already
	// writing through, not just the next one handed out.
	q.setLimits(QuotaLimits{PerMemberRateKiB: 256, MemberMaxTransfers: 1})
	bucket.mu.Lock()
	rate := bucket.rate
	bucket.mu.Unlock()
	if rate != 256*1024 {
		t.Errorf("live bucket rate = %.0f, want the new 256 KiB/s", rate)
	}

	// And the new ceiling refuses the next admission: one serve is in flight.
	if _, _, ok := q.admit("b"); ok {
		t.Error("a second serve was admitted past a ceiling set moments ago")
	}

	// Clearing the budget back to unlimited swaps the bucket out entirely rather
	// than leaving a zero-rate limiter in the path.
	q.setLimits(QuotaLimits{})
	if rl := q.classRate.limiter(); rl != nil {
		t.Error("the class bucket survived a return to unlimited")
	}
	if got := q.current(); got != (QuotaLimits{}) {
		t.Errorf("current() = %+v, want the cleared budget", got)
	}
}

// TestQuotaOverridesResolve: the three-valued encoding, which is the whole
// reason these are pointers — nil inherits the config, and a stored 0 is a real
// override meaning unlimited (docs/architecture/swarm-admin.md §"Which layers a
// knob gets").
func TestQuotaOverridesResolve(t *testing.T) {
	cfg := QuotaLimits{MemberRateKiB: 2048, PerMemberRateKiB: 512,
		MemberMaxTransfers: 16, PerMemberMaxTransfers: 4}

	if got := (QuotaOverrides{}).Resolve(cfg); got != cfg {
		t.Errorf("no overrides = %+v, want the config file", got)
	}

	zero, pinned := 0, 8
	got := QuotaOverrides{MemberRateKiB: &zero, MemberMaxTransfers: &pinned}.Resolve(cfg)
	if got.MemberRateKiB != 0 {
		t.Error("an explicit 0 was read as unset — it is how a node escapes a cap " +
			"its config file ships with")
	}
	if got.MemberMaxTransfers != 8 {
		t.Errorf("member_max_transfers = %d, want the override", got.MemberMaxTransfers)
	}
	if got.PerMemberRateKiB != 512 || got.PerMemberMaxTransfers != 4 {
		t.Error("an untouched field lost its config value")
	}
}
