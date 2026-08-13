package database

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/federation"
)

// laneNames renders a lane's ranking as titles, so an assertion reads as the
// order a person would see rather than a slice of structs.
func laneNames(t *testing.T, db *DB, lane string, view MadnetworkView, limit int) []string {
	t.Helper()
	got, err := db.MadnetworkLaneCandidates(context.Background(), lane, view, limit)
	if err != nil {
		t.Fatalf("lane %s: %v", lane, err)
	}
	out := make([]string, 0, len(got))
	for _, c := range got {
		out = append(out, c.Title)
	}
	return out
}

// TestCatalogFirstSeenSurvivesReplace is the whole reason migration 037 exists:
// without preserving the dates across a changed sync, one sync would re-date a
// source's entire library and "New on the network" would list whoever synced
// most recently. Since the 2026-08-13 diff-apply the preservation is
// structural — surviving rows are not rewritten — but the observable promise
// is the same and this pins it.
func TestCatalogFirstSeenSurvivesReplace(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	insertPeer(t, db, "a1", "friend-a", federation.PeerFriend)
	src := insertSource(t, db, "a1")

	if err := db.ReplaceSourceCatalog(ctx, src, "s1", 100, []federation.CatalogEntry{
		catEntry("1", "r1", "Artist", "Album", "Old Song", "hash-old"),
	}); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// A later sync re-advertises the old entry and adds a new one.
	if err := db.ReplaceSourceCatalog(ctx, src, "s2", 500, []federation.CatalogEntry{
		catEntry("1", "r1", "Artist", "Album", "Old Song", "hash-old"),
		catEntry("2", "r2", "Artist", "Album", "New Song", "hash-new"),
	}); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	dates := map[string]int64{}
	rows, err := db.Query(`SELECT title, first_seen FROM federation_catalog`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var title string
		var at int64
		if err := rows.Scan(&title, &at); err != nil {
			t.Fatal(err)
		}
		dates[title] = at
	}
	if dates["Old Song"] != 100 {
		t.Errorf("surviving entry first_seen = %d, want 100 (its original date)", dates["Old Song"])
	}
	if dates["New Song"] != 500 {
		t.Errorf("added entry first_seen = %d, want 500 (this sync)", dates["New Song"])
	}

	// And the lane reads it: only the genuinely new entry is new.
	if got := laneNames(t, db, LaneNew, MadnetworkView{}, 10); len(got) != 2 || got[0] != "New Song" {
		t.Errorf("new lane = %v, want New Song first", got)
	}
}

// TestCatalogDiffApplyEdges pins the three decisions the diff-apply carries
// (owner calls, 2026-08-13; federation.md §Catalog): a CHANGED row keeps its
// first_seen, a first_seen of 0 stays 0 (unknown is not re-dated into the
// `new` lane), and a duplicate entry key from the remote resolves last-wins
// instead of failing the whole sync. Plus the diff's baseline: a vanished
// entry is deleted, so the result is exactly the snapshot.
func TestCatalogDiffApplyEdges(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	insertPeer(t, db, "a1", "friend-a", federation.PeerFriend)
	src := insertSource(t, db, "a1")

	if err := db.ReplaceSourceCatalog(ctx, src, "s1", 100, []federation.CatalogEntry{
		catEntry("1", "r1", "Artist", "Album", "Renamed Later", "hash-1"),
		catEntry("2", "r2", "Artist", "Album", "Vanishes", "hash-2"),
		catEntry("3", "r3", "Artist", "Album", "Unknown Date", "hash-3"),
	}); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// Simulate the migration-037 backfill: entry 3's date is unknown.
	if _, err := db.Exec(`UPDATE federation_catalog SET first_seen = 0 WHERE entry_key = '3'`); err != nil {
		t.Fatal(err)
	}

	// Second sync: entry 1 renamed (changed row), entry 2 gone, entry 3
	// unchanged, entry 4 sent twice with different titles (remote bug).
	if err := db.ReplaceSourceCatalog(ctx, src, "s2", 500, []federation.CatalogEntry{
		catEntry("1", "r1", "Artist", "Album", "New Name", "hash-1"),
		catEntry("3", "r3", "Artist", "Album", "Unknown Date", "hash-3"),
		catEntry("4", "r4", "Artist", "Album", "First Copy", "hash-4"),
		catEntry("4", "r4", "Artist", "Album", "Second Copy", "hash-4"),
	}); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	rows, err := db.Query(`SELECT entry_key, title, first_seen FROM federation_catalog ORDER BY entry_key`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		title string
		seen  int64
	}
	got := map[string]row{}
	for rows.Next() {
		var key string
		var r row
		if err := rows.Scan(&key, &r.title, &r.seen); err != nil {
			t.Fatal(err)
		}
		got[key] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(got) != 3 {
		t.Errorf("rows after sync = %d (%v), want 3 — the vanished entry must be deleted", len(got), got)
	}
	if r := got["1"]; r.title != "New Name" || r.seen != 100 {
		t.Errorf("changed row = %q/first_seen %d, want New Name/100 (rename applied, date kept)", r.title, r.seen)
	}
	if r := got["3"]; r.seen != 0 {
		t.Errorf("unknown-date row first_seen = %d, want 0 (unknown stays unknown)", r.seen)
	}
	if r := got["4"]; r.title != "Second Copy" {
		t.Errorf("duplicate key resolved to %q, want Second Copy (last wins)", r.title)
	}
}

// TestLaneRankings drives all five lanes over one seeded community, because
// each lane is only meaningful relative to the others: the same catalog has to
// answer "what don't I have", "what is everywhere" and "what is nearly gone"
// differently.
func TestLaneRankings(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// Three nodes: two direct friends, one member the frontier reached (a
	// source with no peer row — the F7 item 5 shape).
	insertPeer(t, db, "a1", "friend-a", federation.PeerFriend)
	insertPeer(t, db, "b2", "friend-b", federation.PeerFriend)
	a := insertSource(t, db, "a1")
	b := insertSource(t, db, "b2")
	stranger := insertSource(t, db, "c3")

	// "Everywhere" is held by all three; "Both Friends" by the two friends;
	// "Stranger Only" by the member alone; "We Have It" is also in our library.
	if err := db.ReplaceSourceCatalog(ctx, a, "sa", 100, []federation.CatalogEntry{
		catEntry("1", "r1", "Artist", "Album", "Everywhere", "h-every"),
		catEntry("2", "r2", "Artist", "Album", "Both Friends", "h-both"),
		catEntry("3", "r3", "Artist", "Album", "We Have It", "shared01"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSourceCatalog(ctx, b, "sb", 100, []federation.CatalogEntry{
		catEntry("1", "r1", "Artist", "Album", "Everywhere", "h-every"),
		catEntry("2", "r2", "Artist", "Album", "Both Friends", "h-both"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSourceCatalog(ctx, stranger, "sc", 900, []federation.CatalogEntry{
		catEntry("1", "r1", "Artist", "Album", "Everywhere", "h-every"),
		catEntry("7", "r7", "Artist", "Album", "Stranger Only", "h-rare"),
	}); err != nil {
		t.Fatal(err)
	}

	// Our own published library holds "We Have It" — same display identity as
	// the cached row, track number included (the identity the merge folds on).
	meta := &MediaMetadata{Title: "We Have It", ExtractedAt: 1700000000,
		Artist:      sql.NullString{String: "Artist", Valid: true},
		AlbumArtist: sql.NullString{String: "Artist", Valid: true},
		Album:       sql.NullString{String: "Album", Valid: true},
		TrackNumber: sql.NullInt64{Int64: 1, Valid: true}}
	if err := db.InsertFile(ctx, newFile("shared01"), newUpload("shared01.mp3"), meta); err != nil {
		t.Fatalf("seed own library: %v", err)
	}
	view := MadnetworkView{IncludeSelf: true}

	// Missing here: everything except the one we publish ourselves.
	missing := laneNames(t, db, LaneMissing, view, 10)
	if len(missing) != 3 {
		t.Fatalf("missing lane = %v, want the three we do not hold", missing)
	}
	for _, name := range missing {
		if name == "We Have It" {
			t.Error("missing lane offered a track this node already publishes")
		}
	}
	if missing[0] != "Everywhere" {
		t.Errorf("missing lane leads with %q, want the most-held one", missing[0])
	}

	// Local library: only what this node publishes. It is the one lane whose
	// membership is decided by our own library rather than by the network, so
	// the assertion is exactly the inverse of the missing lane's.
	local := laneNames(t, db, LaneLocal, view, 10)
	if len(local) != 1 || local[0] != "We Have It" {
		t.Errorf("local lane = %v, want only the track we publish", local)
	}
	// With federation off there is no published set and the lane is empty —
	// never the merged catalog, which is what a missing self-side filter would
	// silently produce.
	if got := laneNames(t, db, LaneLocal, MadnetworkView{}, 10); len(got) != 0 {
		t.Errorf("local lane without the own set = %v, want nothing", got)
	}

	// Most held: three holders, then two, then one.
	if got := laneNames(t, db, LaneHeld, view, 10); got[0] != "Everywhere" || got[1] != "Both Friends" {
		t.Errorf("held lane = %v, want Everywhere then Both Friends", got)
	}

	// Only one node has it: the member's exclusive, and NOT the one only we
	// hold (a self row is not a holder to fetch from).
	rare := laneNames(t, db, LaneRare, view, 10)
	if len(rare) != 1 || rare[0] != "Stranger Only" {
		t.Errorf("rare lane = %v, want only Stranger Only", rare)
	}

	// From direct friends: excludes the member's exclusive entirely.
	friends := laneNames(t, db, LaneFriends, view, 10)
	for _, name := range friends {
		if name == "Stranger Only" {
			t.Error("direct-friends lane included a track no direct friend offers")
		}
	}
	if len(friends) != 3 || friends[0] != "Everywhere" {
		t.Errorf("friends lane = %v, want the three friend-held tracks, most held first", friends)
	}

	// New on the network: the member synced last, so its rows are the new ones.
	if got := laneNames(t, db, LaneNew, view, 10); got[0] != "Stranger Only" {
		t.Errorf("new lane leads with %q, want the most recently learned entry", got[0])
	}

	// The candidate carries the holder keys the branch weighting needs, and the
	// identity pieces the handler pairs rows by.
	cands, err := db.MadnetworkLaneCandidates(ctx, LaneHeld, view, 1)
	if err != nil {
		t.Fatal(err)
	}
	c := cands[0]
	if len(c.HolderKeys) != 3 {
		t.Errorf("holder keys = %v, want all three nodes", c.HolderKeys)
	}
	if c.Title != "Everywhere" || c.Artist != "Artist" || c.Album != "Album" || c.Track != 1 {
		t.Errorf("candidate identity = %+v, want the full display identity", c)
	}

	// And the rows behind an identity come back for both halves of the union.
	own, err := db.MadnetworkLaneCandidates(ctx, LaneHeld, view, 10)
	if err != nil {
		t.Fatal(err)
	}
	var weHave *LaneCandidate
	for _, cand := range own {
		if cand.Title == "We Have It" {
			weHave = cand
		}
	}
	if weHave == nil || !weHave.Self {
		t.Fatalf("own-held candidate = %+v, want Self set", weHave)
	}
	rows, err := db.MadnetworkRowsForIdents(ctx, []string{weHave.Ident}, view)
	if err != nil {
		t.Fatalf("rows for idents: %v", err)
	}
	var sawSelf, sawRemote bool
	for _, r := range rows {
		if r.Self {
			sawSelf = true
		} else {
			sawRemote = true
		}
	}
	if !sawSelf || !sawRemote {
		t.Errorf("rows for ident: self=%v remote=%v, want both halves", sawSelf, sawRemote)
	}
}

// TestLaneBlockedAndUnreachableExcluded pins that the lanes are computed over
// the SAME visible set as the drill-down. A lane that leaked a blocked node's
// rows would be a hole in the block, not a ranking bug.
func TestLaneBlockedAndUnreachableExcluded(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	blockedPeer := insertPeer(t, db, "bad1", "blocked", federation.PeerFriend)
	insertPeer(t, db, "old1", "stale", federation.PeerFriend)
	blocked := insertSource(t, db, "bad1")
	stale := insertSource(t, db, "old1")

	if err := db.ReplaceSourceCatalog(ctx, blocked, "s", 5000, []federation.CatalogEntry{
		catEntry("1", "r1", "Artist", "Album", "Blocked Song", "h-blocked"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSourceCatalog(ctx, stale, "s", 100, []federation.CatalogEntry{
		catEntry("2", "r2", "Artist", "Album", "Stale Song", "h-stale"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.BlockFederationPeer(ctx, blockedPeer, federation.PeerFriend, "", 6000); err != nil {
		t.Fatal(err)
	}

	// Unfiltered: only the blocked node is gone.
	if got := laneNames(t, db, LaneNew, MadnetworkView{}, 10); len(got) != 1 || got[0] != "Stale Song" {
		t.Errorf("lane with no cutoff = %v, want just the stale node's row", got)
	}
	// With a freshness cutoff past the stale node's last contact, nothing shows.
	if got := laneNames(t, db, LaneNew, MadnetworkView{Cutoff: 1000}, 10); len(got) != 0 {
		t.Errorf("lane past the freshness window = %v, want empty", got)
	}
}

// TestSourceFilteredBrowse is the "By node" shelf: one node's offering on its
// own, and our own library as one shelf among them.
func TestSourceFilteredBrowse(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	insertPeer(t, db, "a1", "friend-a", federation.PeerFriend)
	insertPeer(t, db, "b2", "friend-b", federation.PeerFriend)
	a := insertSource(t, db, "a1")
	b := insertSource(t, db, "b2")
	if err := db.ReplaceSourceCatalog(ctx, a, "sa", 100, []federation.CatalogEntry{
		catEntry("1", "r1", "Artist A", "Album A", "Song A", "h-a"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.ReplaceSourceCatalog(ctx, b, "sb", 100, []federation.CatalogEntry{
		catEntry("2", "r2", "Artist B", "Album B", "Song B", "h-b"),
	}); err != nil {
		t.Fatal(err)
	}
	meta := &MediaMetadata{Title: "Song Mine", ExtractedAt: 1700000000,
		Artist:      sql.NullString{String: "Artist Mine", Valid: true},
		AlbumArtist: sql.NullString{String: "Artist Mine", Valid: true},
		Album:       sql.NullString{String: "Album Mine", Valid: true}}
	if err := db.InsertFile(ctx, newFile("mine0001"), newUpload("mine.mp3"), meta); err != nil {
		t.Fatal(err)
	}

	// Merged: all three artists.
	all, _, err := db.MadnetworkArtists(ctx, "", MadnetworkView{IncludeSelf: true}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("merged artists = %d, want 3", len(all))
	}

	// One node's shelf: only that node, and NOT our own set folded in — we are
	// a different node.
	only, _, err := db.MadnetworkArtists(ctx, "", MadnetworkView{IncludeSelf: true, SourceID: a}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[0].Name != "Artist A" {
		t.Fatalf("source-filtered artists = %+v, want only Artist A", only)
	}
	if rows, _ := db.MadnetworkTracks(ctx, "Artist B", "Album B", MadnetworkView{SourceID: a}); len(rows) != 0 {
		t.Errorf("another node's tracks leaked into a source-filtered browse: %+v", rows)
	}

	// Our own shelf: our library, nobody else's.
	mine, _, err := db.MadnetworkArtists(ctx, "", MadnetworkView{IncludeSelf: true, SelfOnly: true}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].Name != "Artist Mine" {
		t.Fatalf("self shelf = %+v, want only our own artist", mine)
	}

	// Our own shelf on a node that publishes nothing to the network is EMPTY —
	// not the merged catalog, which is the one answer that would be certainly
	// wrong for "show me only my shelf".
	none, _, err := db.MadnetworkArtists(ctx, "", MadnetworkView{SelfOnly: true}, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("self shelf with federation off = %+v, want nothing", none)
	}
	if got := laneNames(t, db, LaneHeld, MadnetworkView{SelfOnly: true}, 10); len(got) != 0 {
		t.Errorf("lane over an empty view = %v, want nothing", got)
	}
}

// TestMadnetworkArtistPaging walks the keyset cursor to the end, including the
// unknown-artist bucket that has to stay last across a page boundary.
func TestMadnetworkArtistPaging(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	insertPeer(t, db, "a1", "friend-a", federation.PeerFriend)
	src := insertSource(t, db, "a1")

	entries := []federation.CatalogEntry{}
	for _, name := range []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo"} {
		entries = append(entries, catEntry(name, "r-"+name, name, "Album", "Song "+name, "h-"+name))
	}
	// One entry with no artist at all — the Unknown bucket, which sorts last.
	nameless := catEntry("none", "r-none", "", "Album", "Song None", "h-none")
	entries = append(entries, nameless)
	if err := db.ReplaceSourceCatalog(ctx, src, "s", 100, entries); err != nil {
		t.Fatal(err)
	}

	var seen []string
	cursor := ""
	for page := 0; page < 10; page++ {
		got, next, err := db.MadnetworkArtists(ctx, "", MadnetworkView{}, 2, cursor)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		for _, a := range got {
			seen = append(seen, a.Name)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	want := []string{"Alpha", "Bravo", "Charlie", "Delta", "Echo", DefaultArtistName}
	if strings.Join(seen, ",") != strings.Join(want, ",") {
		t.Errorf("paged artists = %v, want %v", seen, want)
	}

	// A page is a page: asking for two returns two, with a cursor for the rest.
	first, next, err := db.MadnetworkArtists(ctx, "", MadnetworkView{}, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || next == "" {
		t.Errorf("first page = %d rows, cursor %q; want 2 and a cursor", len(first), next)
	}
	// A malformed cursor is ignored rather than fatal — a stale link should land
	// on the first page, not on an error.
	if got, _, err := db.MadnetworkArtists(ctx, "", MadnetworkView{}, 2, "!!!not-base64!!!"); err != nil || len(got) != 2 {
		t.Errorf("malformed cursor: got %d rows, err %v; want the first page", len(got), err)
	}
}

// TestCapPerSourceSpreads is the owner's decision made testable: one node's bulk
// arrival must not own a lane, and the spreading must never cost the lane rows.
func TestCapPerSourceSpreads(t *testing.T) {
	cand := func(title, key string) *LaneCandidate {
		return &LaneCandidate{Ident: title, Title: title, HolderKeys: []string{key}}
	}
	// A node that just arrived with six entries, against two others with one each.
	candidates := []*LaneCandidate{
		cand("big1", "big"), cand("big2", "big"), cand("big3", "big"),
		cand("big4", "big"), cand("big5", "big"), cand("big6", "big"),
		cand("small1", "small"), cand("other1", "other"),
	}
	got := CapPerSource(candidates, 4)
	if len(got) != 4 {
		t.Fatalf("capped lane = %d rows, want 4 (the cap must not shrink the lane)", len(got))
	}
	sources := map[string]int{}
	for _, c := range got {
		sources[c.HolderKeys[0]]++
	}
	if sources["big"] > 2 {
		t.Errorf("one node contributed %d of 4 rows: %v", sources["big"], sources)
	}
	if sources["small"] != 1 || sources["other"] != 1 {
		t.Errorf("the other nodes were not reached: %v", sources)
	}

	// A one-node network is untouched: there is nothing to spread across, and a
	// cap that emptied the lane would be worse than no lane.
	solo := []*LaneCandidate{cand("a", "one"), cand("b", "one"), cand("c", "one")}
	if got := CapPerSource(solo, 2); len(got) != 2 {
		t.Errorf("single-source lane = %d rows, want 2", len(got))
	}
}

// TestWeightByBranch is the sybil answer: a farm of keys behind one friendship
// is one voice, however many keys it mints.
func TestWeightByBranch(t *testing.T) {
	farm := &LaneCandidate{Title: "farmed", Holders: 4,
		HolderKeys: []string{"s1", "s2", "s3", "s4"}}
	real := &LaneCandidate{Title: "corroborated", Holders: 2,
		HolderKeys: []string{"x1", "y1"}}
	branches := map[string][]string{
		"s1": {"friend-a"}, "s2": {"friend-a"}, "s3": {"friend-a"}, "s4": {"friend-a"},
		"x1": {"friend-b"}, "y1": {"friend-c"},
	}
	got := []*LaneCandidate{farm, real}
	WeightByBranch(got, branches)
	if farm.Branches != 1 {
		t.Errorf("farm branches = %d, want 1 (one friendship is one voice)", farm.Branches)
	}
	if real.Branches != 2 {
		t.Errorf("corroborated branches = %d, want 2", real.Branches)
	}
	if got[0] != real {
		t.Error("branch weighting left the sybil farm ranked above two independent holders")
	}

	// With no graph at all the rule degrades to one source one voice, which is
	// the same rule in a smaller world — never a wrong answer.
	WeightByBranch([]*LaneCandidate{farm}, nil)
	if farm.Branches != 4 {
		t.Errorf("ungraphed branches = %d, want one voice per source", farm.Branches)
	}
}

// TestBranchMapVoices pins the shared primitive's edge cases directly, because
// every weighted surface on the page now reads it and they cannot each restate
// them: an unplaceable key is its own voice, a node reachable through two
// friends is still one voice per friend it corroborates, an EMPTY key is never a
// voice (its caller has to supply a distinguishing token), and self counts once.
func TestBranchMapVoices(t *testing.T) {
	bm := BranchMap{
		"s1": {"friend-a"}, "s2": {"friend-a"},
		"m1": {"friend-a", "friend-b"}, // seen down two branches
	}
	cases := []struct {
		name string
		keys []string
		self bool
		want int
	}{
		{"farm behind one friendship", []string{"s1", "s2"}, false, 1},
		{"farm plus our own copy", []string{"s1", "s2"}, true, 2},
		{"multi-branch node", []string{"m1"}, false, 2},
		{"multi-branch node overlapping the farm", []string{"s1", "m1"}, false, 2},
		{"unplaceable keys speak for themselves", []string{"u1", "u2"}, false, 2},
		{"empty keys are not voices", []string{"", ""}, false, 0},
		{"self alone", nil, true, 1},
		{"nothing at all", nil, false, 0},
	}
	for _, tc := range cases {
		if got := bm.Voices(tc.keys, tc.self); got != tc.want {
			t.Errorf("%s: voices = %d, want %d", tc.name, got, tc.want)
		}
	}
	// A nil map is the no-federation build and the no-graph-yet case: one source,
	// one voice, and never a collapse to zero.
	if got := BranchMap(nil).Voices([]string{"a", "b"}, true); got != 3 {
		t.Errorf("nil branch map: voices = %d, want 3 (two sources + self)", got)
	}
}

// TestLocalLaneCarriesTheWholeLibrary: the Local library lane is a doorway to
// `/`, not a view of the network, so a recording scoped Local is IN it — while
// the merged browse, which is what the network sees of us, still leaves it out.
//
// The second half is the one that must not regress: the lane may show more of
// our own shelf than we publish, but publishing is a different question and a
// different query.
func TestLocalLaneCarriesTheWholeLibrary(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	public := seedScopeFile(t, db, "lane0001", "Shared Song")
	private := seedScopeFile(t, db, "lane0002", "Private Song")
	_ = public
	depth := federation.DepthPrivate
	if ok, err := db.SetRecordingAccess(ctx, private, nil, nil,
		ShareDepthUpdate{Set: true, Depth: depth}); err != nil || !ok {
		t.Fatalf("set private: ok=%v err=%v", ok, err)
	}

	view := MadnetworkView{IncludeSelf: true}
	local := laneNames(t, db, LaneLocal, view, 10)
	if len(local) != 2 {
		t.Fatalf("local lane = %v, want both — the whole library, scope included", local)
	}

	// The merged catalog is still only what we publish.
	albums, err := db.MadnetworkAlbums(ctx, "Scope Artist", view)
	if err != nil {
		t.Fatal(err)
	}
	if len(albums) != 1 || albums[0].Tracks != 1 {
		t.Errorf("merged album = %+v, want the one published track", albums)
	}
	rows, err := db.MadnetworkOwnTracks(ctx, "Scope Artist", "Scope Album", view)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Entry.Title != "Shared Song" {
		t.Errorf("own rows = %d, want only the published one", len(rows))
	}

	// And the lane's ROWS come back for the private track too — a candidate that
	// ranks but renders nothing would be a lane with holes in it.
	cands, err := db.MadnetworkLaneCandidates(ctx, LaneLocal, view, 10)
	if err != nil {
		t.Fatal(err)
	}
	idents := make([]string, 0, len(cands))
	for _, c := range cands {
		idents = append(idents, c.Ident)
	}
	laneView := view
	laneView.AllOwn = true
	got, err := db.MadnetworkRowsForIdents(ctx, idents, laneView)
	if err != nil {
		t.Fatal(err)
	}
	titles := map[string]bool{}
	for _, r := range got {
		titles[r.Entry.Title] = true
	}
	if !titles["Private Song"] || !titles["Shared Song"] {
		t.Errorf("lane rows = %v, want both titles", titles)
	}
}
