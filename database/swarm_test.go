package database

import (
	"context"
	"testing"

	"daemonlord.ygg/madshare/federation"
)

// The swarm traffic table's contract (docs/architecture/swarm-admin.md): writes
// are increments, totals are a SUM over the rows rather than counters kept
// beside them, and forgetting is the only thing that deletes.

func TestSwarmTraffic_WritesAreIncrements(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	const hash = "aa11"

	if err := db.AddSwarmTraffic(ctx, []SwarmTrafficDelta{{Hash: hash, Up: 100, Down: 40}}, nil, 1000); err != nil {
		t.Fatalf("first flush: %v", err)
	}
	if err := db.AddSwarmTraffic(ctx, []SwarmTrafficDelta{{Hash: hash, Up: 5, Wasted: 7}}, nil, 2000); err != nil {
		t.Fatalf("second flush: %v", err)
	}

	row, err := db.GetSwarmTraffic(ctx, hash)
	if err != nil || row == nil {
		t.Fatalf("GetSwarmTraffic = %+v, %v", row, err)
	}
	if row.Up != 105 || row.Down != 40 || row.Wasted != 7 {
		t.Errorf("counters = up %d down %d wasted %d, want 105/40/7 (the second flush must ADD)",
			row.Up, row.Down, row.Wasted)
	}
	// first_at is when the hash first moved; last_at follows every flush.
	if row.FirstAt != 1000 || row.LastAt != 2000 {
		t.Errorf("clocks = first %d last %d, want 1000/2000", row.FirstAt, row.LastAt)
	}
}

// An empty delta must not touch the row. A drain that found nothing for a hash
// is not evidence of activity, and moving last_at would make a blob nobody has
// touched for a month look freshly active on the page's "last active" sort.
func TestSwarmTraffic_EmptyDeltaLeavesTheClockAlone(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	const hash = "bb22"

	if err := db.AddSwarmTraffic(ctx, []SwarmTrafficDelta{{Hash: hash, Up: 10}}, nil, 1000); err != nil {
		t.Fatal(err)
	}
	if err := db.AddSwarmTraffic(ctx, []SwarmTrafficDelta{{Hash: hash}}, nil, 9999); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetSwarmTraffic(ctx, hash)
	if err != nil || row == nil {
		t.Fatalf("GetSwarmTraffic = %+v, %v", row, err)
	}
	if row.LastAt != 1000 {
		t.Errorf("last_at = %d, want 1000 — an all-zero delta is not activity", row.LastAt)
	}
}

func TestSwarmTraffic_TotalsSumTheRows(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	if err := db.AddSwarmTraffic(ctx, []SwarmTrafficDelta{
		{Hash: "aa", Up: 10, Down: 1},
		{Hash: "bb", Up: 20, Down: 2, Wasted: 3},
	}, nil, 1000); err != nil {
		t.Fatal(err)
	}

	total, err := db.SwarmTrafficTotals(ctx)
	if err != nil {
		t.Fatalf("SwarmTrafficTotals: %v", err)
	}
	if total.Up != 30 || total.Down != 3 || total.Wasted != 3 {
		t.Errorf("totals = %+v, want up 30 down 3 wasted 3", total)
	}

	// The node's all-time figure IS this sum, so forgetting a row lowers it —
	// which is what the UI has to say out loud before it forgets anything.
	if n, err := db.ForgetSwarmTraffic(ctx, []string{"bb"}); err != nil || n != 1 {
		t.Fatalf("ForgetSwarmTraffic = %d, %v, want 1, nil", n, err)
	}
	total, err = db.SwarmTrafficTotals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if total.Up != 10 || total.Wasted != 0 {
		t.Errorf("totals after forget = %+v, want up 10 wasted 0", total)
	}
}

// A hash that never moved has no row at all — the listing LEFT JOINs and reads
// absence as zeros, so nothing pre-creates rows for a whole library.
func TestSwarmTraffic_UnknownHashHasNoRow(t *testing.T) {
	db := openMem(t)
	row, err := db.GetSwarmTraffic(context.Background(), "never")
	if err != nil {
		t.Fatalf("GetSwarmTraffic: %v", err)
	}
	if row != nil {
		t.Errorf("row = %+v, want nil", row)
	}
}

func TestSwarmTraffic_EmptyBatchIsANoop(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	if err := db.AddSwarmTraffic(ctx, nil, nil, 1); err != nil {
		t.Fatalf("nil batch: %v", err)
	}
	if n, err := db.ForgetSwarmTraffic(ctx, nil); err != nil || n != 0 {
		t.Fatalf("empty forget = %d, %v", n, err)
	}
}

// The node's two rate overrides are three-valued the way the API is: absent
// means "inherit the config file", and an explicit 0 means unlimited — a real
// override, and how one node escapes a cap its config ships with.
func TestSwarmRates_ThreeValued(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	up, down, err := db.GetSwarmRates(ctx)
	if err != nil {
		t.Fatalf("GetSwarmRates: %v", err)
	}
	if up != nil || down != nil {
		t.Errorf("fresh node = %v/%v, want no overrides", up, down)
	}

	zero, cap := 0, 1900
	if err := db.SetSwarmRates(ctx, &zero, &cap); err != nil {
		t.Fatalf("SetSwarmRates: %v", err)
	}
	up, down, err = db.GetSwarmRates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if up == nil || *up != 0 {
		t.Errorf("up = %v, want an explicit 0 (unlimited), not absence", up)
	}
	if down == nil || *down != 1900 {
		t.Errorf("down = %v, want 1900", down)
	}

	// Clearing puts the node back on its config file.
	if err := db.SetSwarmRates(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	up, down, err = db.GetSwarmRates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if up != nil || down != nil {
		t.Errorf("after clearing = %v/%v, want no overrides", up, down)
	}
}

// The member budget stores the same way, and under key names that mirror the
// [federation] ones it overrides — an operator reading the settings table and an
// operator reading the TOML must be looking at the same four words.
func TestMemberQuotas_ThreeValued(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	q, err := db.GetMemberQuotas(ctx)
	if err != nil {
		t.Fatalf("GetMemberQuotas: %v", err)
	}
	if q != (federation.QuotaOverrides{}) {
		t.Errorf("fresh node = %+v, want no overrides", q)
	}

	zero, cap := 0, 4
	if err := db.SetMemberQuotas(ctx, federation.QuotaOverrides{
		MemberRateKiB: &zero, PerMemberMaxTransfers: &cap}); err != nil {
		t.Fatalf("SetMemberQuotas: %v", err)
	}
	q, err = db.GetMemberQuotas(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if q.MemberRateKiB == nil || *q.MemberRateKiB != 0 {
		t.Errorf("member_rate_kib = %v, want an explicit 0 (unlimited), not absence", q.MemberRateKiB)
	}
	if q.PerMemberMaxTransfers == nil || *q.PerMemberMaxTransfers != 4 {
		t.Errorf("per_member_max_transfers = %v, want 4", q.PerMemberMaxTransfers)
	}
	if q.PerMemberRateKiB != nil || q.MemberMaxTransfers != nil {
		t.Errorf("untouched bounds gained overrides: %+v", q)
	}
	if v, ok, _ := db.GetSetting(ctx, "swarm.per_member_max_transfers"); !ok || v != "4" {
		t.Errorf("stored under %q = %q/%v, want the [federation] key name", "swarm.per_member_max_transfers", v, ok)
	}

	if err := db.SetMemberQuotas(ctx, federation.QuotaOverrides{}); err != nil {
		t.Fatal(err)
	}
	if q, _ = db.GetMemberQuotas(ctx); q != (federation.QuotaOverrides{}) {
		t.Errorf("after clearing = %+v, want no overrides", q)
	}
}

// A node must not stop serving because somebody typed into the settings table.
func TestSwarmRates_GarbageReadsAsNoOverride(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	if err := db.SetSetting(ctx, "swarm.up_rate_kib", "fast please"); err != nil {
		t.Fatal(err)
	}
	up, _, err := db.GetSwarmRates(ctx)
	if err != nil {
		t.Fatalf("GetSwarmRates: %v", err)
	}
	if up != nil {
		t.Errorf("up = %v, want no override for an unparseable value", up)
	}
}
