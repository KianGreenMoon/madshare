package database

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cached writes a blob into the cache directory and indexes it, with an explicit
// last-used clock — the one thing eviction order depends on.
func cached(t *testing.T, db *DB, dir, hash string, size int, lastUsed int64) {
	t.Helper()
	ctx := context.Background()
	body := make([]byte, size)
	if err := os.WriteFile(filepath.Join(dir, hash), body, 0o600); err != nil {
		t.Fatal(err)
	}
	err := db.PutMadnetworkCacheEntry(ctx, &MadnetworkCacheEntry{
		Hash: hash, ByteSize: int64(size), Filename: hash + ".mp3",
		FetchedAt: lastUsed, LastUsedAt: lastUsed,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func hashOf(n string) string { return strings.Repeat(n, 64)[:64] }

func TestSweepCacheCeilingEvictsColdestFirst(t *testing.T) {
	db := openMem(t)
	dir := t.TempDir()
	ctx := context.Background()

	cold, warm, hot := hashOf("a"), hashOf("b"), hashOf("c")
	cached(t, db, dir, cold, 100, 1000)
	cached(t, db, dir, warm, 100, 2000)
	cached(t, db, dir, hot, 100, 3000)

	// Room for two.
	removed, freed, err := SweepCacheCeiling(ctx, db, dir, 250, nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 1 || freed != 100 {
		t.Fatalf("removed=%d freed=%d, want 1 and 100", removed, freed)
	}
	if _, err := os.Stat(filepath.Join(dir, cold)); !os.IsNotExist(err) {
		t.Error("the least recently used blob survived")
	}
	for _, keep := range []string{warm, hot} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("a warmer blob was evicted: %v", err)
		}
	}

	// The index must agree with the directory, or the swarm would keep
	// advertising bytes that are gone.
	if e, err := db.GetMadnetworkCacheEntry(ctx, cold); err != nil || e != nil {
		t.Errorf("the evicted blob still has an index row: %+v (%v)", e, err)
	}
	total, err := db.MadnetworkCacheBytes(ctx)
	if err != nil || total != 200 {
		t.Errorf("indexed total = %d (%v), want 200", total, err)
	}
}

// 0 is OFF, and it is the shipped default. A ceiling nobody set must never
// delete anything.
func TestSweepCacheCeilingOffKeepsEverything(t *testing.T) {
	db := openMem(t)
	dir := t.TempDir()
	cached(t, db, dir, hashOf("a"), 100, 1000)

	for _, ceiling := range []int64{0, -1} {
		removed, _, err := SweepCacheCeiling(context.Background(), db, dir, ceiling, nil)
		if err != nil {
			t.Fatalf("sweep(%d): %v", ceiling, err)
		}
		if removed != 0 {
			t.Errorf("ceiling %d evicted %d blob(s); 0 means off", ceiling, removed)
		}
	}
}

func TestSweepCacheCeilingUnderTheLimitDoesNothing(t *testing.T) {
	db := openMem(t)
	dir := t.TempDir()
	cached(t, db, dir, hashOf("a"), 100, 1000)

	removed, freed, err := SweepCacheCeiling(context.Background(), db, dir, 10_000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 || freed != 0 {
		t.Errorf("removed=%d freed=%d under the ceiling, want nothing", removed, freed)
	}
}

// A blob being fetched right now must survive, however cold its predecessor's
// row looks: evicting what a transfer is about to publish makes the fetch
// pointless.
func TestSweepCacheCeilingSpareTheTransferInFlight(t *testing.T) {
	db := openMem(t)
	dir := t.TempDir()
	ctx := context.Background()

	live, other := hashOf("a"), hashOf("b")
	cached(t, db, dir, live, 100, 1000) // coldest
	cached(t, db, dir, other, 100, 2000)

	removed, _, err := SweepCacheCeiling(ctx, db, dir, 150, map[string]bool{live: true})
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed %d, want 1", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, live)); err != nil {
		t.Error("the blob being fetched right now was evicted")
	}
	if _, err := os.Stat(filepath.Join(dir, other)); !os.IsNotExist(err) {
		t.Error("the sweep should have taken the next-coldest instead")
	}
}

// The ceiling is three-valued: no override (inherit the config), a pinned
// number, or a pinned 0 meaning "no limit". The last two are different
// settings, and collapsing them is the bug this pins.
func TestCacheCeilingOverrideIsThreeValued(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	got, err := db.GetCacheCeiling(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("a fresh node has an override of %d, want none", *got)
	}

	pin := int64(2) << 30
	if err := db.SetCacheCeiling(ctx, &pin); err != nil {
		t.Fatal(err)
	}
	if got, err = db.GetCacheCeiling(ctx); err != nil || got == nil || *got != pin {
		t.Fatalf("override = %v (%v), want %d", got, err, pin)
	}

	// A pinned 0 is "no limit", chosen — NOT the absence of a choice.
	zero := int64(0)
	if err := db.SetCacheCeiling(ctx, &zero); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetCacheCeiling(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("a pinned 0 read back as no override; 'no limit' and 'use the default' are different")
	}
	if *got != 0 {
		t.Errorf("override = %d, want 0", *got)
	}

	// Clearing goes back to the config file.
	if err := db.SetCacheCeiling(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if got, err = db.GetCacheCeiling(ctx); err != nil || got != nil {
		t.Errorf("after clearing: %v (%v), want no override", got, err)
	}

	// A garbage stored value reads as no override rather than as some number:
	// the fallback chain always has a config value, and a node must not start
	// deleting on something nobody can read.
	if err := db.SetSetting(ctx, settingMadnetworkCacheMaxBytes, "two gigs"); err != nil {
		t.Fatal(err)
	}
	if got, err = db.GetCacheCeiling(ctx); err != nil || got != nil {
		t.Errorf("unparseable override read as %v, want none", got)
	}
}

// The resolution rule the settings card and the sweep must agree on.
func TestResolveCacheCeiling(t *testing.T) {
	pin := func(n int64) *int64 { return &n }
	cases := []struct {
		name     string
		override *int64
		def      int64
		want     int64
	}{
		{"no override inherits the config", nil, 512, 512},
		{"no override, no config = no limit", nil, 0, 0},
		{"an override wins over the config", pin(256), 512, 256},
		{"a pinned 0 beats a configured limit — that is what an override is for", pin(0), 512, 0},
		{"a negative override is treated as none", pin(-5), 512, 0},
	}
	for _, tc := range cases {
		if got := ResolveCacheCeiling(tc.override, tc.def); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Saving the rest of the madnetwork policy must not disturb the ceiling: they
// are separate keys precisely so the whole-object write cannot reach it.
func TestSavingThePolicyLeavesTheCeilingAlone(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	pin := int64(700)
	if err := db.SetCacheCeiling(ctx, &pin); err != nil {
		t.Fatal(err)
	}
	p, err := db.GetMadnetworkPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p.AutoapproveDownloads = true
	if err := db.SetMadnetworkPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetCacheCeiling(ctx)
	if err != nil || got == nil || *got != pin {
		t.Errorf("ceiling after a policy save = %v (%v), want %d", got, err, pin)
	}
}

// TestSweepCacheAgeEvictsOnlyWhatIsPastTheWindow: the age rule asks an absolute
// question, so it must not evict a blob merely for being the coldest — that is
// the ceiling's job, and it is the one thing the two rules could be confused
// for.
func TestSweepCacheAgeEvictsOnlyWhatIsPastTheWindow(t *testing.T) {
	db := openMem(t)
	dir := t.TempDir()
	ctx := context.Background()
	now := time.Now()

	ancient, stale, recent := hashOf("a"), hashOf("b"), hashOf("c")
	cached(t, db, dir, ancient, 100, now.Add(-40*24*time.Hour).Unix())
	cached(t, db, dir, stale, 100, now.Add(-31*24*time.Hour).Unix())
	cached(t, db, dir, recent, 100, now.Add(-2*24*time.Hour).Unix())

	removed, freed, err := SweepCacheAge(ctx, db, dir, 30, nil)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 2 || freed != 200 {
		t.Fatalf("removed=%d freed=%d, want 2 and 200", removed, freed)
	}
	for _, gone := range []string{ancient, stale} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("a blob unused for more than 30 days survived (%s)", gone[:8])
		}
	}
	if _, err := os.Stat(filepath.Join(dir, recent)); err != nil {
		t.Error("a blob used two days ago was evicted by a 30-day rule")
	}
	// File first, row second: the index must not keep describing a file that is
	// gone, or the swarm would go on advertising a blob it cannot serve.
	if n, _, _ := db.CountMadnetworkCache(ctx, MadnetworkCacheFilter{}); n != 1 {
		t.Errorf("index rows after the sweep = %d, want 1", n)
	}
}

func TestSweepCacheAgeOffKeepsEverything(t *testing.T) {
	db := openMem(t)
	dir := t.TempDir()
	cached(t, db, dir, hashOf("a"), 100, 1000) // 1970: as old as it gets

	for _, days := range []int64{0, -1} {
		removed, _, err := SweepCacheAge(context.Background(), db, dir, days, nil)
		if err != nil {
			t.Fatalf("sweep(%d): %v", days, err)
		}
		if removed != 0 {
			t.Errorf("age=%d evicted %d blob(s); 0 and below are OFF", days, removed)
		}
	}
}

// TestSweepCacheAgeSparesTheTransferInFlight: the same protection the ceiling
// has, and it must live in one place — a blob a fetch is about to publish is
// not a retention candidate whichever rule is asking.
func TestSweepCacheAgeSparesTheTransferInFlight(t *testing.T) {
	db := openMem(t)
	dir := t.TempDir()
	live := hashOf("a")
	cached(t, db, dir, live, 100, 1000)

	removed, _, err := SweepCacheAge(context.Background(), db, dir, 1, map[string]bool{live: true})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != 0 {
		t.Error("evicted a blob a transfer is about to publish, which makes that fetch pointless")
	}
	if _, err := os.Stat(filepath.Join(dir, live)); err != nil {
		t.Error("the in-flight blob's file was removed")
	}
}

// TestCacheMaxAgeDaysRoundTrip: the age knob is TWO-valued, which is the one
// way it differs from the ceiling beside it — there is no config layer beneath
// it, so unset and 0 mean the same thing and neither needs preserving.
func TestCacheMaxAgeDaysRoundTrip(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	if got, err := db.GetCacheMaxAgeDays(ctx); err != nil || got != 0 {
		t.Fatalf("unset age = %d (err %v), want 0 — the shipped default keeps everything", got, err)
	}
	if err := db.SetCacheMaxAgeDays(ctx, 30); err != nil {
		t.Fatal(err)
	}
	if got, err := db.GetCacheMaxAgeDays(ctx); err != nil || got != 30 {
		t.Fatalf("age = %d (err %v), want 30", got, err)
	}
	// Off is stored, not deleted: there is nothing above it to fall back to, so
	// the two states are the same answer and either spelling must read as 0.
	if err := db.SetCacheMaxAgeDays(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetCacheMaxAgeDays(ctx); got != 0 {
		t.Errorf("age after switching off = %d, want 0", got)
	}
	// A negative is an operator saying "off" in a way the number cannot hold.
	if err := db.SetCacheMaxAgeDays(ctx, -7); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetCacheMaxAgeDays(ctx); got != 0 {
		t.Errorf("age after a negative = %d, want it clamped to 0", got)
	}
}
