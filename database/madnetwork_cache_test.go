package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// id3v1File builds the smallest thing dhowden/tag will read tags out of: some
// audio-ish bytes with a 128-byte ID3v1 trailer. Enough to prove that adoption
// reads the FILE'S OWN tags rather than anyone's claim about it.
func id3v1File(title, artist, album string) []byte {
	pad := func(s string, n int) []byte {
		b := make([]byte, n)
		copy(b, s)
		return b
	}
	data := make([]byte, 0, 512)
	data = append(data, make([]byte, 256)...) // stand-in payload
	data = append(data, []byte("TAG")...)
	data = append(data, pad(title, 30)...)
	data = append(data, pad(artist, 30)...)
	data = append(data, pad(album, 30)...)
	data = append(data, pad("1999", 4)...)
	data = append(data, pad("", 30)...) // comment
	data = append(data, 0)              // genre
	return data
}

// TestReconcileMadnetworkCache pins the rule the whole feature rests on: the
// files on disk are the truth and the index describes them. A blob the index has
// never seen is adopted (with whatever the file says about itself), a row whose
// file is gone is dropped, and nothing that is not a finished cache blob is
// touched either way.
func TestReconcileMadnetworkCache(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	cacheDir := t.TempDir()

	tagged := hexHash("tagged")
	untagged := hexHash("untagged")
	write := func(name string, body []byte) {
		if err := os.WriteFile(filepath.Join(cacheDir, name), body, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write(tagged, id3v1File("Cached Title", "Cached Artist", "Cached Album"))
	write(untagged, []byte("just bytes"))
	// Neither of these is a finished cache blob, and neither may be adopted: a
	// partial is not an entry, and a name we did not write is not ours at all.
	write(untagged+".part", []byte("in flight"))
	write("not-a-hash", []byte("stray"))

	// A row whose file is gone — the crash case reconciliation exists for.
	stale := hexHash("stale")
	if err := db.PutMadnetworkCacheEntry(ctx, &MadnetworkCacheEntry{
		Hash: stale, ByteSize: 99, FetchedAt: 1, LastUsedAt: 1,
	}); err != nil {
		t.Fatalf("seed stale row: %v", err)
	}

	added, dropped, err := ReconcileMadnetworkCache(ctx, db, cacheDir)
	if err != nil {
		t.Fatalf("ReconcileMadnetworkCache: %v", err)
	}
	if added != 2 || dropped != 1 {
		t.Errorf("added=%d dropped=%d, want 2/1 (two blobs adopted, the fileless row dropped)", added, dropped)
	}

	got, err := db.GetMadnetworkCacheEntry(ctx, tagged)
	if err != nil || got == nil {
		t.Fatalf("adopted entry = %v/%v, want a row", got, err)
	}
	if got.Title != "Cached Title" || got.Artist != "Cached Artist" || got.Album != "Cached Album" {
		t.Errorf("adopted tags = %q/%q/%q, want the file's own ID3v1 values",
			got.Title, got.Artist, got.Album)
	}
	if got.ByteSize == 0 || got.FetchedAt == 0 || got.LastUsedAt == 0 {
		t.Errorf("adopted row = %+v, want size and both timestamps from stat", got)
	}
	if got.Filename != "" {
		t.Errorf("adopted filename = %q, want empty — the origin name is not recoverable from disk", got.Filename)
	}

	// An untagged blob is a perfectly good cache entry with nothing to say.
	if e, err := db.GetMadnetworkCacheEntry(ctx, untagged); err != nil || e == nil {
		t.Fatalf("untagged entry = %v/%v, want a row (untagged is not an error)", e, err)
	}
	if e, err := db.GetMadnetworkCacheEntry(ctx, stale); err != nil || e != nil {
		t.Errorf("stale row = %v/%v, want gone", e, err)
	}

	// Idempotent: a second pass has nothing left to do.
	if a, d, err := ReconcileMadnetworkCache(ctx, db, cacheDir); err != nil || a != 0 || d != 0 {
		t.Errorf("second pass = %d/%d/%v, want 0/0/nil", a, d, err)
	}

	// A cache directory that never existed is not an error — federation may
	// never have run here — but the rows we hold are stale all the same.
	if _, d, err := ReconcileMadnetworkCache(ctx, db, filepath.Join(cacheDir, "nope")); err != nil {
		t.Errorf("missing cache dir: %v, want nil", err)
	} else if d != 2 {
		t.Errorf("missing cache dir dropped %d, want 2 — no directory means no cache entries", d)
	}
}

// TestReapAbandonedPartials closes the leak nothing else could: a `.part` left
// by a killed process was permanent dead disk, because both the eviction sweep
// and the holdings listing skip non-digest names on purpose.
func TestReapAbandonedPartials(t *testing.T) {
	dir := t.TempDir()
	dead, live := hexHash("dead"), hexHash("live")
	write := func(name string, n int) {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, n), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	write(dead+".part", 10)
	write(live+".part", 20)
	write(hexHash("whole"), 30) // a finished blob is not a partial
	write("stray.part", 5)      // not a hash: not ours to delete
	write("notes.txt", 5)

	// At startup `live` is correctly nil — a process that just started is writing
	// nothing — but at runtime a fetch in progress must keep its scratch file.
	count, bytes, err := CountAbandonedPartials(dir, map[string]bool{live: true})
	if err != nil {
		t.Fatalf("CountAbandonedPartials: %v", err)
	}
	if count != 1 || bytes != 10 {
		t.Errorf("count/bytes = %d/%d, want 1/10 (only the abandoned one)", count, bytes)
	}

	removed, freed, err := ReapAbandonedPartials(dir, map[string]bool{live: true})
	if err != nil {
		t.Fatalf("ReapAbandonedPartials: %v", err)
	}
	if removed != 1 || freed != 10 {
		t.Errorf("removed/freed = %d/%d, want 1/10", removed, freed)
	}
	for _, tc := range []struct {
		name string
		gone bool
		why  string
	}{
		{dead + ".part", true, "an abandoned fetch's scratch file is pure waste"},
		{live + ".part", false, "reaping a RUNNING transfer's partial is data loss mid-fetch"},
		{hexHash("whole"), false, "a finished blob is a cache entry, not a partial"},
		{"stray.part", false, "a name we did not write is not ours to delete"},
		{"notes.txt", false, "nor is anything else in the directory"},
	} {
		_, err := os.Stat(filepath.Join(dir, tc.name))
		if gone := os.IsNotExist(err); gone != tc.gone {
			t.Errorf("%s: gone=%v, want %v — %s", tc.name, gone, tc.gone, tc.why)
		}
	}

	// Idempotent, and silent about a directory that never existed.
	if n, _, err := ReapAbandonedPartials(dir, nil); err != nil || n != 1 {
		// The second pass now takes `live` too, since nothing is running.
		t.Logf("second pass removed %d (the formerly-live partial), err=%v", n, err)
	}
	if n, _, err := ReapAbandonedPartials(filepath.Join(dir, "nope"), nil); err != nil || n != 0 {
		t.Errorf("missing dir = %d/%v, want 0/nil", n, err)
	}
}

// TestMadnetworkCacheListing covers the page's three reads over one filter: the
// page itself, the count+bytes headline, and the "select all N matching" hash
// set. They must agree — a bulk removal that targeted a different set than the
// one on screen would delete things nobody chose.
func TestMadnetworkCacheListing(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	seed := func(name, title, artist string, size int64, fetched, used int64) string {
		h := hexHash(name)
		if err := db.PutMadnetworkCacheEntry(ctx, &MadnetworkCacheEntry{
			Hash: h, ByteSize: size, Filename: name + ".mp3", Title: title,
			Artist: artist, FetchedAt: fetched, LastUsedAt: used,
		}); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		return h
	}
	old := seed("old", "Old Song", "Alpha", 100, 10, 500)
	mid := seed("mid", "Middle", "Beta", 300, 20, 100)
	recent := seed("recent", "Recent", "Alpha", 200, 30, 300)

	count, bytes, err := db.CountMadnetworkCache(ctx, MadnetworkCacheFilter{})
	if err != nil {
		t.Fatalf("CountMadnetworkCache: %v", err)
	}
	if count != 3 || bytes != 600 {
		t.Errorf("count/bytes = %d/%d, want 3/600", count, bytes)
	}

	order := func(sort string) []string {
		rows, err := db.ListMadnetworkCachePage(ctx, MadnetworkCacheQuery{Sort: sort, Limit: 10})
		if err != nil {
			t.Fatalf("list %q: %v", sort, err)
		}
		out := make([]string, len(rows))
		for i, r := range rows {
			out[i] = r.Hash
		}
		return out
	}
	for _, tc := range []struct {
		sort string
		want []string
		why  string
	}{
		{"", []string{recent, mid, old}, "default is newest fetched first"},
		{"oldest", []string{old, mid, recent}, "oldest fetched first"},
		{"lru", []string{mid, recent, old}, "least recently USED first — what a retention sweep would take"},
		{"largest", []string{mid, recent, old}, "largest first"},
		{"smallest", []string{old, recent, mid}, "smallest first"},
	} {
		got := order(tc.sort)
		if len(got) != len(tc.want) {
			t.Fatalf("sort %q returned %d rows, want %d", tc.sort, len(got), len(tc.want))
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("sort %q position %d = %s, want %s — %s", tc.sort, i, got[i][:8], tc.want[i][:8], tc.why)
				break
			}
		}
	}

	// Search: tag text, and a hash by PREFIX (pasting the front of a digest is
	// how anyone actually looks one up; an infix match on hex is noise).
	for _, tc := range []struct {
		filter MadnetworkCacheFilter
		want   int
		why    string
	}{
		{MadnetworkCacheFilter{Q: "alpha"}, 2, "case-insensitive across every field by default"},
		{MadnetworkCacheFilter{Q: "Alpha", Field: "artist"}, 2, "scoped to the artist column"},
		{MadnetworkCacheFilter{Q: "Alpha", Field: "album"}, 0, "the album column holds none of it"},
		{MadnetworkCacheFilter{Q: "Middle", Field: "title"}, 1, "title scope also matches the filename"},
		{MadnetworkCacheFilter{Q: recent[:12], Field: "artist"}, 1, "a hash prefix matches whatever the field scope is"},
		{MadnetworkCacheFilter{Q: recent[20:32]}, 0, "an INFIX hash fragment is not a match"},
	} {
		n, _, err := db.CountMadnetworkCache(ctx, tc.filter)
		if err != nil {
			t.Fatalf("count %+v: %v", tc.filter, err)
		}
		if n != tc.want {
			t.Errorf("count for %+v = %d, want %d — %s", tc.filter, n, tc.want, tc.why)
		}
		hashes, err := db.MadnetworkCacheHashes(ctx, tc.filter)
		if err != nil {
			t.Fatalf("hashes %+v: %v", tc.filter, err)
		}
		if len(hashes) != tc.want {
			t.Errorf("select-all set for %+v = %d hashes, want %d (it must match the count)",
				tc.filter, len(hashes), tc.want)
		}
	}

	if total, err := db.MadnetworkCacheBytes(ctx); err != nil || total != 600 {
		t.Errorf("MadnetworkCacheBytes = %d/%v, want 600 (the dashboard's storage category)", total, err)
	}
}

// TestMadnetworkCacheUseClock pins what the retention clock does and does not
// move: it only ever goes forward, only for rows we actually hold, and a
// re-fetch is not a use.
func TestMadnetworkCacheUseClock(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	h := hexHash("clock")

	if err := db.PutMadnetworkCacheEntry(ctx, &MadnetworkCacheEntry{
		Hash: h, ByteSize: 10, Title: "First", FetchedAt: 100, LastUsedAt: 100,
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := db.TouchMadnetworkCache(ctx, h, 500); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if e, _ := db.GetMadnetworkCacheEntry(ctx, h); e.LastUsedAt != 500 {
		t.Errorf("last_used = %d, want 500", e.LastUsedAt)
	}
	// An out-of-order touch (a throttled writer racing, or a clock stepping back)
	// must not walk the row backwards toward eviction.
	if err := db.TouchMadnetworkCache(ctx, h, 200); err != nil {
		t.Fatalf("late touch: %v", err)
	}
	if e, _ := db.GetMadnetworkCacheEntry(ctx, h); e.LastUsedAt != 500 {
		t.Errorf("last_used after a backwards touch = %d, want 500 (monotonic)", e.LastUsedAt)
	}

	// Re-putting the same hash updates what the file IS without pretending
	// anyone asked for it.
	if err := db.PutMadnetworkCacheEntry(ctx, &MadnetworkCacheEntry{
		Hash: h, ByteSize: 20, Filename: "real.mp3", Title: "Second",
		FetchedAt: 900, LastUsedAt: 900,
	}); err != nil {
		t.Fatalf("re-put: %v", err)
	}
	e, err := db.GetMadnetworkCacheEntry(ctx, h)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if e.Title != "Second" || e.Filename != "real.mp3" || e.ByteSize != 20 || e.FetchedAt != 900 {
		t.Errorf("re-put row = %+v, want the new description", e)
	}
	if e.LastUsedAt != 500 {
		t.Errorf("last_used after re-put = %d, want 500 — landing in the cache is not a use", e.LastUsedAt)
	}

	// Touching a hash we do not index is a silent no-op: the index describes the
	// directory, so inventing a row here would invert that.
	if err := db.TouchMadnetworkCache(ctx, hexHash("absent"), 700); err != nil {
		t.Errorf("touch of an unindexed hash: %v, want nil", err)
	}
	if e, err := db.GetMadnetworkCacheEntry(ctx, hexHash("absent")); err != nil || e != nil {
		t.Errorf("unindexed hash = %v/%v, want no row conjured", e, err)
	}

	if err := db.DeleteMadnetworkCacheEntry(ctx, h); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := db.DeleteMadnetworkCacheEntry(ctx, h); err != nil {
		t.Errorf("second delete: %v, want nil — making the index agree twice is not a failure", err)
	}
}
