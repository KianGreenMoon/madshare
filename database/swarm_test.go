package database

import (
	"context"
	"testing"
)

// The swarm traffic table's contract (docs/architecture/swarm-admin.md): writes
// are increments, totals are a SUM over the rows rather than counters kept
// beside them, and forgetting is the only thing that deletes.

func TestSwarmTraffic_WritesAreIncrements(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	const hash = "aa11"

	if err := db.AddSwarmTraffic(ctx, []SwarmTrafficDelta{{Hash: hash, Up: 100, Down: 40}}, 1000); err != nil {
		t.Fatalf("first flush: %v", err)
	}
	if err := db.AddSwarmTraffic(ctx, []SwarmTrafficDelta{{Hash: hash, Up: 5, Wasted: 7}}, 2000); err != nil {
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

	if err := db.AddSwarmTraffic(ctx, []SwarmTrafficDelta{{Hash: hash, Up: 10}}, 1000); err != nil {
		t.Fatal(err)
	}
	if err := db.AddSwarmTraffic(ctx, []SwarmTrafficDelta{{Hash: hash}}, 9999); err != nil {
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
	}, 1000); err != nil {
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
	if err := db.AddSwarmTraffic(ctx, nil, 1); err != nil {
		t.Fatalf("nil batch: %v", err)
	}
	if n, err := db.ForgetSwarmTraffic(ctx, nil); err != nil || n != 0 {
		t.Fatalf("empty forget = %d, %v", n, err)
	}
}
