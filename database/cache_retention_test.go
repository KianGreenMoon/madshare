package database

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// The ceiling is a runtime setting, and 0 = off is what an untouched node reads.
func TestMadnetworkPolicyCarriesTheCacheCeiling(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	p, err := db.GetMadnetworkPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if p.CacheMaxBytes != 0 {
		t.Errorf("a fresh node reads a ceiling of %d, want 0 = off", p.CacheMaxBytes)
	}

	p.CacheMaxBytes = 2 << 30
	if err := db.SetMadnetworkPolicy(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetMadnetworkPolicy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.CacheMaxBytes != 2<<30 {
		t.Errorf("ceiling = %d, want %d", got.CacheMaxBytes, int64(2<<30))
	}
	// Writing the policy must not disturb its neighbours — this row is written
	// whole, and the seed switches default ON.
	if !got.SeedEnabled || !got.SeedCache {
		t.Error("saving a ceiling switched seeding off")
	}

	// A garbage stored value reads as off rather than as some number: the safe
	// direction is keeping too much, never deleting on a typo.
	if err := db.SetSetting(ctx, settingMadnetworkCacheMaxBytes, "two gigs"); err != nil {
		t.Fatal(err)
	}
	if got, err = db.GetMadnetworkPolicy(ctx); err != nil {
		t.Fatal(err)
	}
	if got.CacheMaxBytes != 0 {
		t.Errorf("unparseable ceiling read as %d, want 0", got.CacheMaxBytes)
	}
}
