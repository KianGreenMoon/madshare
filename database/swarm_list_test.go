package database

import (
	"context"
	"database/sql"
	"testing"

	"daemonlord.ygg/madshare/federation"
)

// The swarm listing (docs/architecture/swarm-admin.md §The model): every blob
// with bytes, both halves of the disk, one row per hash.

// seedSwarmLibraryFile inserts an approved library file with tags.
func seedSwarmLibraryFile(t *testing.T, db *DB, hash, title, artist string, size int64) *File {
	t.Helper()
	f := &File{Hash: hash, ByteSize: size, MimeType: "audio/mpeg", ObjectKey: hash + "/x.mp3",
		ReviewState: ReviewApproved}
	up := &FileUpload{Filename: title + ".mp3"}
	meta := &MediaMetadata{Title: title,
		Artist: sql.NullString{String: artist, Valid: artist != ""},
		Album:  sql.NullString{String: "An Album", Valid: true}}
	if err := db.InsertFile(context.Background(), f, up, meta); err != nil {
		t.Fatalf("InsertFile(%s): %v", title, err)
	}
	return f
}

func seedSwarmCacheEntry(t *testing.T, db *DB, hash, title string, size int64) {
	t.Helper()
	err := db.PutMadnetworkCacheEntry(context.Background(), &MadnetworkCacheEntry{
		Hash: hash, ByteSize: size, Filename: title + ".flac", Title: title,
		Artist: "Remote Artist", FetchedAt: 1000, LastUsedAt: 1000,
	})
	if err != nil {
		t.Fatalf("PutMadnetworkCacheEntry(%s): %v", title, err)
	}
}

func TestSwarmListing_CoversBothHalvesAndScopes(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedSwarmLibraryFile(t, db, "aa11", "Local Song", "Local Artist", 100)
	seedSwarmCacheEntry(t, db, "bb22", "Fetched Song", 200)

	all, err := db.ListSwarmFiles(ctx, SwarmQuery{SwarmFilter: SwarmFilter{Scope: SwarmScopeAll}})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("all scope = %d rows, want 2 (one per half)", len(all))
	}

	for _, tc := range []struct {
		scope    SwarmScope
		wantHash string
	}{
		{SwarmScopeLibrary, "aa11"},
		{SwarmScopeCache, "bb22"},
	} {
		rows, err := db.ListSwarmFiles(ctx, SwarmQuery{SwarmFilter: SwarmFilter{Scope: tc.scope}})
		if err != nil {
			t.Fatalf("list %s: %v", tc.scope, err)
		}
		if len(rows) != 1 || rows[0].Hash != tc.wantHash {
			t.Errorf("%s scope = %+v, want only %s", tc.scope, rows, tc.wantHash)
		}
	}

	// The count and the page must agree — they share one predicate so that a
	// select-all can never act on a different set than the one on screen.
	n, bytes, err := db.CountSwarmFiles(ctx, SwarmFilter{Scope: SwarmScopeAll})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 || bytes != 300 {
		t.Errorf("count = %d rows / %d bytes, want 2 / 300", n, bytes)
	}
}

// A hash in both halves is ONE row carrying both flags. Two rows would
// double-count the bytes in every total the page shows.
func TestSwarmListing_OneRowWhenABlobIsInBothHalves(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedSwarmLibraryFile(t, db, "cc33", "Both Places", "Someone", 500)
	seedSwarmCacheEntry(t, db, "cc33", "Cached Name", 500)

	rows, err := db.ListSwarmFiles(ctx, SwarmQuery{SwarmFilter: SwarmFilter{Scope: SwarmScopeAll}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	r := rows[0]
	if !r.InLibrary || !r.InCache {
		t.Errorf("flags = library %v cache %v, want both", r.InLibrary, r.InCache)
	}
	// Our own curated appearance beats what the cached copy's tags happen to say.
	if r.Title != "Both Places" {
		t.Errorf("title = %q, want the library's", r.Title)
	}
	if n, bytes, _ := db.CountSwarmFiles(ctx, SwarmFilter{Scope: SwarmScopeAll}); n != 1 || bytes != 500 {
		t.Errorf("count = %d / %d bytes, want 1 / 500 — the halves must not double-count", n, bytes)
	}
}

// Drafts and trashed blobs are listed: they occupy the disk, and a page that
// says what this node holds must not omit them. Each carries the state that
// explains why it is not moving.
func TestSwarmListing_ListsEveryStateWithItsExplanation(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	live := seedSwarmLibraryFile(t, db, "aa11", "Approved", "A", 10)
	_ = live
	draft := &File{Hash: "bb22", ByteSize: 20, MimeType: "audio/mpeg", ObjectKey: "bb22/x.mp3",
		ReviewState: ReviewDraft}
	if err := db.InsertFile(ctx, draft, &FileUpload{Filename: "draft.mp3"},
		&MediaMetadata{Title: "Draft"}); err != nil {
		t.Fatal(err)
	}

	rows, err := db.ListSwarmFiles(ctx, SwarmQuery{SwarmFilter: SwarmFilter{Scope: SwarmScopeAll}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want both the approved and the draft blob", len(rows))
	}
	byHash := map[string]*SwarmFileRow{}
	for _, r := range rows {
		byHash[r.Hash] = r
	}
	if got := byHash["bb22"]; got == nil || got.ReviewState == "approved" {
		t.Errorf("draft row = %+v, want a non-approved review state", got)
	}
	if byHash["bb22"].Seedable() {
		t.Error("a draft is not seedable — the row must be able to say so")
	}
	if !byHash["aa11"].Seedable() {
		t.Error("an approved, unrestricted library blob should read as seedable")
	}

	// The review pill narrows to exactly that half.
	staged, err := db.ListSwarmFiles(ctx, SwarmQuery{
		SwarmFilter: SwarmFilter{Scope: SwarmScopeAll, State: SwarmStateReview}})
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != 1 || staged[0].Hash != "bb22" {
		t.Errorf("review pill = %+v, want only the draft", staged)
	}
}

// A library-state pill excludes cache rows: "in review" is not a state a cached
// blob can be in, and answering such a filter with cache rows would widen it.
func TestSwarmListing_StatePillExcludesTheCacheHalf(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedSwarmLibraryFile(t, db, "aa11", "Approved", "A", 10)
	seedSwarmCacheEntry(t, db, "bb22", "Fetched", 20)

	rows, err := db.ListSwarmFiles(ctx, SwarmQuery{
		SwarmFilter: SwarmFilter{Scope: SwarmScopeAll, State: SwarmStateLive}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Hash != "aa11" {
		t.Errorf("live pill = %+v, want only the library row", rows)
	}
	// And a cache scope with a library pill selects nothing rather than
	// everything.
	none, err := db.ListSwarmFiles(ctx, SwarmQuery{
		SwarmFilter: SwarmFilter{Scope: SwarmScopeCache, State: SwarmStateLive}})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("cache scope + library pill = %+v, want nothing", none)
	}
}

// A Local-scoped recording is listed and marked, not hidden: the page has to be
// able to answer "why isn't this seeding?".
func TestSwarmListing_PrivateRowsAreListedAndMarked(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	f := seedSwarmLibraryFile(t, db, "aa11", "Private Song", "A", 10)
	if _, err := db.SetRecordingAccess(ctx, f.RecordingID, nil, nil,
		ShareDepthUpdate{Set: true, Depth: federation.DepthPrivate}); err != nil {
		t.Fatalf("SetRecordingAccess: %v", err)
	}

	rows, err := db.ListSwarmFiles(ctx, SwarmQuery{
		SwarmFilter: SwarmFilter{Scope: SwarmScopeAll, State: SwarmStatePrivate}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Hash != "aa11" {
		t.Fatalf("private pill = %+v, want the Local-scoped row", rows)
	}
	if rows[0].Seedable() {
		t.Error("a Local-scoped recording must not read as seedable")
	}
}

// Traffic is a LEFT JOIN: most of a library has never moved, and absence has to
// read as zeros rather than as a missing row.
func TestSwarmListing_TrafficIsOptional(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedSwarmLibraryFile(t, db, "aa11", "Moved", "A", 10)
	seedSwarmLibraryFile(t, db, "bb22", "Never Moved", "A", 10)
	if err := db.AddSwarmTraffic(ctx, []SwarmTrafficDelta{{Hash: "aa11", Up: 999}}, 5000); err != nil {
		t.Fatal(err)
	}

	rows, err := db.ListSwarmFiles(ctx, SwarmQuery{
		SwarmFilter: SwarmFilter{Scope: SwarmScopeAll}, Sort: "up"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].Hash != "aa11" || rows[0].Up != 999 {
		t.Errorf("first row by upload = %+v, want aa11 with 999", rows[0])
	}
	if rows[1].Up != 0 || rows[1].LastAt != 0 {
		t.Errorf("untouched row = %+v, want zeros", rows[1])
	}
}

func TestSwarmListing_SearchMatchesTextAndHashPrefix(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedSwarmLibraryFile(t, db, "abcdef01", "Findable", "Someone", 10)
	seedSwarmCacheEntry(t, db, "99887766", "Other", 20)

	for _, tc := range []struct{ name, q, want string }{
		{"by title", "findab", "abcdef01"},
		{"by artist", "someone", "abcdef01"},
		{"by hash prefix", "abcd", "abcdef01"},
		{"cache row by title", "other", "99887766"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.ListSwarmFiles(ctx, SwarmQuery{
				SwarmFilter: SwarmFilter{Scope: SwarmScopeAll, Q: tc.q}})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Hash != tc.want {
				t.Errorf("q=%q matched %+v, want %s", tc.q, rows, tc.want)
			}
		})
	}
}

func TestSwarmListing_GetOneAndHashResolution(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	seedSwarmLibraryFile(t, db, "aa11", "One", "A", 10)

	row, err := db.GetSwarmFile(ctx, "aa11")
	if err != nil || row == nil {
		t.Fatalf("GetSwarmFile = %+v, %v", row, err)
	}
	if row.Title != "One" {
		t.Errorf("title = %q", row.Title)
	}
	if missing, err := db.GetSwarmFile(ctx, "nothere"); err != nil || missing != nil {
		t.Errorf("unknown hash = %+v, %v, want nil, nil", missing, err)
	}

	hashes, err := db.SwarmFileHashes(ctx, SwarmFilter{Scope: SwarmScopeAll})
	if err != nil || len(hashes) != 1 || hashes[0] != "aa11" {
		t.Errorf("SwarmFileHashes = %v, %v", hashes, err)
	}
}

// The sort dropdown. Every order is a whitelist token — an unrecognised one has
// to fall back to the default rather than reach the ORDER BY — and every order
// ends in hash, so a page boundary landing inside a tie cannot list a blob twice
// or skip it while the operator is paging.
func TestSwarmListing_SortOrders(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	// Cache rows, because fetched_at is the one added_at this test can set: the
	// library half's created_at is whatever the clock said during the insert, and
	// three rows written in the same second cannot be ordered by date at all.
	seedSwarmCacheAt(t, db, "aa11", "Cee", 300, 1000)
	seedSwarmCacheAt(t, db, "bb22", "Aay", 100, 3000)
	seedSwarmCacheAt(t, db, "cc33", "Bee", 200, 2000)
	if err := db.AddSwarmTraffic(ctx, []SwarmTrafficDelta{
		{Hash: "aa11", Up: 5, Down: 90},
		{Hash: "cc33", Up: 50, Down: 7},
	}, 4000); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		sort string
		want []string
	}{
		{"", []string{"bb22", "cc33", "aa11"}}, // default: newest first
		{"newest", []string{"bb22", "cc33", "aa11"}},
		{"oldest", []string{"aa11", "cc33", "bb22"}},
		{"name", []string{"bb22", "cc33", "aa11"}}, // Aay · Bee · Cee
		{"largest", []string{"aa11", "cc33", "bb22"}},
		{"smallest", []string{"bb22", "cc33", "aa11"}},
		{"up", []string{"cc33", "aa11", "bb22"}},   // 50 · 5 · none
		{"down", []string{"aa11", "cc33", "bb22"}}, // 90 · 7 · none
		// A stale link, a typo, a client that invents a token: none of them may
		// change the order under the operator. Unknown means the default.
		{"sideways", []string{"bb22", "cc33", "aa11"}},
	} {
		t.Run("sort="+tc.sort, func(t *testing.T) {
			rows, err := db.ListSwarmFiles(ctx, SwarmQuery{
				SwarmFilter: SwarmFilter{Scope: SwarmScopeAll}, Sort: tc.sort})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, r := range rows {
				got = append(got, r.Hash)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("sort %q = %v, want %v", tc.sort, got, tc.want)
				}
			}
		})
	}
}

// Ties break on hash, ascending, in every order — which is what makes paging
// stable when a whole page shares one size or one second.
func TestSwarmListing_TiesBreakOnHashSoPagingIsStable(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	for _, h := range []string{"cc33", "aa11", "bb22"} {
		seedSwarmCacheAt(t, db, h, "Same", 100, 1000)
	}

	for _, sort := range []string{"newest", "oldest", "largest", "smallest", "name", "up", "down", "active"} {
		var seen []string
		for offset := 0; offset < 3; offset++ {
			rows, err := db.ListSwarmFiles(ctx, SwarmQuery{
				SwarmFilter: SwarmFilter{Scope: SwarmScopeAll}, Sort: sort, Limit: 1, Offset: offset})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 {
				t.Fatalf("sort %q offset %d = %d rows, want 1", sort, offset, len(rows))
			}
			seen = append(seen, rows[0].Hash)
		}
		want := []string{"aa11", "bb22", "cc33"}
		for i := range want {
			if seen[i] != want[i] {
				t.Errorf("sort %q paged one at a time = %v, want %v — a page boundary inside a tie repeats or drops rows", sort, seen, want)
				break
			}
		}
	}
}

// seedSwarmCacheAt is seedSwarmCacheEntry with the fetch time (the row's
// added_at) and size under the test's control.
func seedSwarmCacheAt(t *testing.T, db *DB, hash, title string, size, fetchedAt int64) {
	t.Helper()
	err := db.PutMadnetworkCacheEntry(context.Background(), &MadnetworkCacheEntry{
		Hash: hash, ByteSize: size, Filename: title + ".flac", Title: title,
		FetchedAt: fetchedAt, LastUsedAt: fetchedAt,
	})
	if err != nil {
		t.Fatalf("PutMadnetworkCacheEntry(%s): %v", title, err)
	}
}
