//go:build !nofederation

package federation

import (
	"bytes"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// F9 item 1, the serve side. Two rules are pinned here, and both are ways the
// feature can be quietly wrong rather than visibly broken: a node advertises
// only bytes it has PROVEN, and it advertises them as byte offsets rather than
// chunk indices (see [ByteRange] for why the second one matters).

func TestCompleteRangesCoalescesVerifiedChunks(t *testing.T) {
	tr := newTransfer("h", "final", "final.part")
	tr.beginChunks(buildLayout(50, 10, nil), nil) // 5 uniform 10-byte chunks
	tr.chunkDone(0, 10)
	tr.chunkDone(1, 20)
	tr.chunkDone(4, 20) // out of order: the tail arrived before the middle

	got := tr.CompleteRanges()
	want := []ByteRange{{Start: 0, End: 20}, {Start: 40, End: 50}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("CompleteRanges() = %v, want %v", got, want)
	}
}

// The load-bearing one. The F3 whole-file fallback streams straight into the
// part file and checks the hash only at the end, so its progress watermark
// counts bytes RECEIVED, not bytes proven. Advertising them would let this node
// re-seed whatever a bad holder sent it — the swarm's per-chunk verification
// exists precisely so that never happens.
func TestCompleteRangesRefusesUnverifiedSequentialBytes(t *testing.T) {
	tr := newTransfer("h", "final", "final.part")
	tr.addProgress(4096)

	if got := tr.CompleteRanges(); got != nil {
		t.Fatalf("a sequential transfer advertised %v; the whole-file path verifies "+
			"only at the end, so those bytes are unproven and must not be re-seeded", got)
	}
}

func TestCompleteRangesOfAFinishedTransferIsTheWholeBlob(t *testing.T) {
	tr := newTransfer("h", "final", "")
	tr.addProgress(1234)
	tr.finish(nil)

	want := []ByteRange{{Start: 0, End: 1234}}
	if got := tr.CompleteRanges(); !reflect.DeepEqual(got, want) {
		t.Fatalf("CompleteRanges() = %v, want %v", got, want)
	}
}

// A failed transfer advertises nothing even though some of its chunks did
// verify: the attempt is over, its part file is about to be discarded or
// truncated, and a holder that answers for bytes it is in the middle of throwing
// away is worse than one that answers for none.
func TestCompleteRangesOfAFailedTransferIsEmpty(t *testing.T) {
	tr := newTransfer("h", "final", "final.part")
	tr.beginChunks(buildLayout(30, 10, nil), nil)
	tr.chunkDone(0, 10)
	tr.finish(errors.New("every holder went away"))

	if got := tr.CompleteRanges(); got != nil {
		t.Fatalf("a failed transfer advertised %v, want nothing", got)
	}
}

func TestCapRangesKeepsTheLargestInOffsetOrder(t *testing.T) {
	var rs []ByteRange
	for i := 0; i < maxHaveRanges+2; i++ { // many one-byte crumbs
		rs = append(rs, ByteRange{Start: int64(i * 10), End: int64(i*10 + 1)})
	}
	big := ByteRange{Start: 100000, End: 200000}
	rs = append(rs, big)

	got := capRanges(rs)
	if len(got) != maxHaveRanges {
		t.Fatalf("capRanges returned %d ranges, want %d", len(got), maxHaveRanges)
	}
	if got[len(got)-1] != big {
		t.Fatalf("the largest range was dropped; a short answer must still be the useful one")
	}
	for i := 1; i < len(got); i++ {
		if got[i].Start < got[i-1].Start {
			t.Fatalf("capRanges returned ranges out of offset order: %v", got)
		}
	}
}

func TestCapRangesLeavesASmallListAlone(t *testing.T) {
	rs := []ByteRange{{Start: 0, End: 10}, {Start: 20, End: 30}}
	if got := capRanges(rs); !reflect.DeepEqual(got, rs) {
		t.Fatalf("capRanges() = %v, want the input unchanged %v", got, rs)
	}
}

// ── The serve path ───────────────────────────────────────────────────────────

func TestParseSingleRange(t *testing.T) {
	const size = 1000
	cases := []struct {
		hdr        string
		start, end int64
		ok         bool
	}{
		{"bytes=0-99", 0, 100, true},
		{"bytes=500-", 500, 1000, true},
		{"bytes=-200", 800, 1000, true},
		{"bytes=0-99999", 0, 1000, true}, // clamped at EOF
		{"bytes=0-99,200-299", 0, 0, false},
		{"bytes=1000-1099", 0, 0, false}, // starts past the end
		{"bytes=99-1", 0, 0, false},      // reversed
		{"bytes=", 0, 0, false},
		{"chunks=0-99", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, c := range cases {
		start, end, ok := parseSingleRange(c.hdr, size)
		if ok != c.ok || (ok && (start != c.start || end != c.end)) {
			t.Errorf("parseSingleRange(%q) = (%d, %d, %v), want (%d, %d, %v)",
				c.hdr, start, end, ok, c.start, c.end, c.ok)
		}
	}
}

// A request spanning two advertised extents spans a HOLE between them, because
// the extents are already coalesced into maximal runs. Treating the list as a
// union would serve the gap.
func TestRangeCoveredNeedsOneExtentNotAUnion(t *testing.T) {
	rs := []ByteRange{{Start: 0, End: 10}, {Start: 20, End: 30}}
	for _, c := range []struct {
		start, end int64
		want       bool
	}{
		{0, 10, true},
		{2, 8, true},
		{20, 30, true},
		{5, 25, false}, // spans the hole at [10,20)
		{8, 12, false},
		{25, 35, false},
	} {
		if got := rangeCovered(rs, c.start, c.end); got != c.want {
			t.Errorf("rangeCovered(%v, %d, %d) = %v, want %v", rs, c.start, c.end, got, c.want)
		}
	}
}

// partialFixture builds a node holding one in-flight download: a 40-byte part
// file whose first 10-byte chunk is verified and whose remaining 30 bytes are
// still the zero fill that fetchSwarm's Truncate laid down.
func partialFixture(t *testing.T) (*Node, string, []byte) {
	t.Helper()
	const hash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dir := t.TempDir()
	part := filepath.Join(dir, hash+".part")

	head := bytes.Repeat([]byte{0x01}, 10)
	full := make([]byte, 40) // the other 30 bytes stay zero, exactly as in flight
	copy(full, head)
	if err := os.WriteFile(part, full, 0o644); err != nil {
		t.Fatal(err)
	}

	n := &Node{
		store:     newMemStore(),
		transfers: map[string]*transfer{},
		upRate:    &adjustableRate{},
		downRate:  &adjustableRate{},
		traffic:   newTrafficTable(),
		logger:    log.New(io.Discard, "", 0),
	}
	tr := newTransfer(hash, filepath.Join(dir, hash), part)
	tr.setMeta(40, "song.mp3")
	tr.beginChunks(buildLayout(40, 10, nil), nil) // 4 chunks of 10
	tr.chunkDone(0, 10)                           // only chunk 0 is verified
	n.transfers[hash] = tr
	return n, hash, head
}

func servePartial(t *testing.T, n *Node, hash, rangeHdr string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/madnetwork/v0/blob/"+hash, nil)
	if rangeHdr != "" {
		req.Header.Set("Range", rangeHdr)
	}
	rec := httptest.NewRecorder()
	n.servePartialBlob(rec, req, hash, MemberAudience, nil, "")
	return rec
}

func TestPartialServesAVerifiedRange(t *testing.T) {
	n, hash, head := partialFixture(t)

	rec := servePartial(t, n, hash, "bytes=0-9")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, head) {
		t.Fatalf("body = %v, want the verified chunk %v", got, head)
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 0-9/40" {
		t.Fatalf("Content-Range = %q, want %q", cr, "bytes 0-9/40")
	}
}

// THE trap. The part file is full-length and mostly zeros from the moment
// fetchSwarm truncates it, so anything that answers off the file's extent —
// http.ServeContent above all — hands out zeros that verify at neither chunk nor
// whole-file level. Every byte must be gated on CompleteRanges instead.
func TestPartialNeverServesTheZeroFill(t *testing.T) {
	n, hash, _ := partialFixture(t)

	for _, hdr := range []string{
		"bytes=10-19", // wholly inside the un-fetched tail
		"bytes=5-14",  // starts verified, runs into the zero fill
		"bytes=30-39", // the last chunk, never fetched
	} {
		rec := servePartial(t, n, hash, hdr)
		if rec.Code != http.StatusRequestedRangeNotSatisfiable {
			t.Errorf("Range %q: status = %d, want 416 — a part file's length is a lie "+
				"and those bytes are zero fill, not content", hdr, rec.Code)
		}
		if bytes.Contains(rec.Body.Bytes(), []byte{0, 0, 0, 0}) {
			t.Errorf("Range %q: the reply carried zero fill", hdr)
		}
	}
}

// The F3 whole-file path sends no Range and would take a short 200 body for the
// whole blob, discovering otherwise only at the final hash. Answer as if we do
// not hold the hash: one wasted transfer avoided for free.
func TestPartialRefusesARequestWithNoRange(t *testing.T) {
	n, hash, _ := partialFixture(t)

	if rec := servePartial(t, n, hash, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for an unranged request against a partial", rec.Code)
	}
}

// Cache seeding off means partials are off too — they answer under the cache
// branch's rule exactly, which is the whole reason they are not a third branch.
func TestPartialFollowsTheCacheSeedingSwitch(t *testing.T) {
	n, hash, _ := partialFixture(t)
	n.store.(*memStore).setSeeding(true, false)

	if rec := servePartial(t, n, hash, "bytes=0-9"); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 with seed_cache off", rec.Code)
	}
}

// ── The advertisement path ───────────────────────────────────────────────────

func TestPartialHoldingsReadsLiveTransfersNotTheDirectory(t *testing.T) {
	const (
		fetching = "1111111111111111111111111111111111111111111111111111111111111111"
		empty    = "2222222222222222222222222222222222222222222222222222222222222222"
		complete = "3333333333333333333333333333333333333333333333333333333333333333"
		born     = "4444444444444444444444444444444444444444444444444444444444444444"
	)
	n := &Node{transfers: map[string]*transfer{}}

	// Verified bytes to offer — the one that should be advertised.
	withBytes := newTransfer(fetching, "final", "final.part")
	withBytes.setMeta(40, "")
	withBytes.beginChunks(buildLayout(40, 10, nil), nil)
	withBytes.chunkDone(0, 10)
	n.transfers[fetching] = withBytes

	// In flight but nothing proven yet: a holder with no ranges is not a holder.
	noBytes := newTransfer(empty, "final2", "final2.part")
	noBytes.setMeta(40, "")
	noBytes.beginChunks(buildLayout(40, 10, nil), nil)
	n.transfers[empty] = noBytes

	// Already in the cache list — must not be advertised twice, under two
	// different promises.
	alsoDone := newTransfer(complete, "final3", "final3.part")
	alsoDone.setMeta(40, "")
	alsoDone.beginChunks(buildLayout(40, 10, nil), nil)
	alsoDone.chunkDone(0, 10)
	n.transfers[complete] = alsoDone

	// A born-complete transfer (cache hit / local blob) has no part file.
	n.transfers[born] = completedTransfer(born, "somewhere", 40)

	got := n.partialHoldings([]string{complete})
	want := []string{fetching}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("partialHoldings() = %v, want %v", got, want)
	}
}

// ── 416 is a fact about the chunk, not about the holder ──────────────────────

// F9 item 1 recruits partial seeders, and the retirement rule would have thrown
// them straight back out: before this, a 416 was an ordinary transient failure,
// so a holder that simply had not reached chunk 7 accumulated a streak and was
// dropped like a broken one.
func TestChunkPlanDoesNotRetireAHolderForNotHavingAChunkYet(t *testing.T) {
	cp := testPlan(wideLayout(8), []*BlobProvider{{Name: "partial"}, {Name: "full"}}, false)
	var refused []int
	for i := 0; i < providerFailureLimit*2; i++ {
		if len(cp.pending) == 0 {
			break
		}
		idx := dispatch(t, cp, 0)
		refused = append(refused, idx)
		cp.fail(idx, 0, errChunkAbsent, false)
	}
	if cp.prov[0].dead {
		t.Error("a holder that answered 416 was retired; it told us about the chunk, not itself")
	}
	if cp.prov[0].fails != 0 {
		t.Errorf("fails = %d, want 0 — an absent chunk must not build a streak", cp.prov[0].fails)
	}
	// Since F9 item 3 the refusal is also REMEMBERED, so the scheduler stops
	// offering that holder the chunk it just said it does not have — the 416 is
	// coverage learned the expensive way.
	for _, idx := range refused {
		if cp.prov[0].canServe(cp.layout, idx) {
			t.Errorf("chunk %d is still considered fetchable from the holder that 416'd it", idx)
		}
		if !cp.prov[1].canServe(cp.layout, idx) {
			t.Errorf("chunk %d was written off for the OTHER holder too", idx)
		}
	}
}

// ...but it still costs the chunk an attempt, or a swarm of partials that
// between them lack a chunk would re-queue it forever.
func TestChunkPlanStillGivesUpWhenNobodyHasAChunk(t *testing.T) {
	cp := testPlan(wideLayout(8), []*BlobProvider{{Name: "a"}, {Name: "b"}}, false)

	// Chunks are re-queued behind the others, so one of them only reaches
	// attemptLimit after roughly attemptLimit × chunks dispatches. take() stops
	// handing work out once the plan aborts, so the loop ends on its own — and it
	// keeps handing chunks to holders known to lack them, which is matchLocked's
	// second pass: a partial holder is a growing thing, so "does not have it" is
	// only true until it isn't, and being wrong costs one fast 416.
	for i := 0; i < 1000; i++ {
		idx, _, pidx, ok := cp.take()
		if !ok {
			break
		}
		cp.fail(idx, pidx, errChunkAbsent, false)
	}
	if !cp.aborted {
		t.Fatal("a chunk no holder has must end the transfer, not loop")
	}
	if cp.prov[0].dead || cp.prov[1].dead {
		t.Error("the transfer ended by exhausting attempts, so nobody should have been condemned")
	}
}
