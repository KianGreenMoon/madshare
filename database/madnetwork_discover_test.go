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
// a snapshot is applied by deleting every row and re-inserting it, so without
// carrying the dates across, one sync would re-date a source's entire library
// and "New on the network" would list whoever synced most recently.
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

	// Not in your library: everything except the one we publish ourselves.
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

	// From your direct friends: excludes the member's exclusive entirely.
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
