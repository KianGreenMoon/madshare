//go:build !nofederation

package federation

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The scheduler's own rules (F9 item 3). What a holder costs when it fails is
// pinned next door in streaming_test.go; these are about who gets asked for
// what, which is the half that changed.

// TestScheduleGoesToTheLeastLoadedHolder pins the dispatch rule that replaced
// round-robin. It is deliberately about BYTES OUTSTANDING and not about
// throughput: a holder that is not delivering keeps its dispatches, so the load
// rule notices before any timeout expires and without anyone having to decide
// the holder is slow.
func TestScheduleGoesToTheLeastLoadedHolder(t *testing.T) {
	holders := []*BlobProvider{{Name: "a"}, {Name: "b"}}
	cp := testPlan(wideLayout(8), holders, false)
	tr := newTransfer("h", "p", "p.part")

	// With everyone idle the two alternate: each dispatch loads one of them, so
	// the other becomes the least loaded.
	var got []string
	mine := map[int][]int{} // provider index → the chunks it was handed
	for i := 0; i < 4; i++ {
		idx, p, pidx, ok := cp.take()
		if !ok {
			t.Fatal("nothing to dispatch")
		}
		got = append(got, p.Name)
		mine[pidx] = append(mine[pidx], idx)
	}
	if got[0] == got[1] || got[1] == got[2] || got[2] == got[3] {
		t.Errorf("dispatch piled onto one holder while the other was idle: %v", got)
	}

	// Now let one of them deliver everything it was handed while the other keeps
	// its chunks in flight. The one that came back free takes all the new work.
	free := 0
	if got[0] == "b" {
		free = 1
	}
	for _, idx := range mine[free] {
		cp.succeed(idx, free, tr, time.Millisecond)
	}

	for i := 0; i < 2; i++ {
		_, p, _, ok := cp.take()
		if !ok {
			t.Fatal("nothing to dispatch after the free holder came back")
		}
		if p != holders[free] {
			t.Errorf("dispatch %d went to %q, want the holder with nothing outstanding (%q)",
				i, p.Name, holders[free].Name)
		}
	}
}

// TestScheduleAsksAPartialHolderOnlyForWhatItHas is the pair-selection rule F9
// item 1 forced: once a downloader seeds what it has so far, "pick a provider,
// then a chunk" is simply wrong, because not every holder can serve every chunk.
func TestScheduleAsksAPartialHolderOnlyForWhatItHas(t *testing.T) {
	holders := []*BlobProvider{{Name: "partial"}, {Name: "complete"}}
	cp := testPlan(buildLayout(50, 10, nil), holders, false) // 5 chunks of 10
	cp.setCoverage(0, &haveMessage{Ranges: []ByteRange{{Start: 0, End: 20}}})
	cp.setCoverage(1, &haveMessage{Complete: true, Ranges: []ByteRange{{Start: 0, End: 50}}})

	for i := 0; i < 5; i++ {
		idx, p, _, ok := cp.take()
		if !ok {
			t.Fatalf("dispatch %d: nothing handed out", i)
		}
		if p.Name == "partial" && idx > 1 {
			t.Errorf("chunk %d was asked of the holder that only has [0,20)", idx)
		}
	}
}

// ...but coverage is a snapshot of a growing thing, so it deprioritises rather
// than excludes. When nobody else will take a chunk, the partial holder is asked
// anyway: being wrong costs one fast 416, and never asking costs a source for
// the rest of the transfer.
func TestScheduleAsksAPartialHolderAnywayWhenNobodyElseWill(t *testing.T) {
	cp := testPlan(buildLayout(50, 10, nil), []*BlobProvider{{Name: "partial"}}, false)
	cp.setCoverage(0, &haveMessage{Ranges: []ByteRange{{Start: 0, End: 20}}})

	seen := map[int]bool{}
	for i := 0; i < 3; i++ {
		idx, _, _, ok := cp.take()
		if !ok {
			t.Fatalf("dispatch %d: the sole holder was written off over stale coverage", i)
		}
		seen[idx] = true
	}
	if !seen[2] && !seen[3] && !seen[4] {
		t.Errorf("only the covered chunks were ever dispatched: %v", seen)
	}
}

// TestQuotaRefusalCostsAWaitNotAMark: a 429 is a deliberate refusal under the
// member quota (F7 item 6), which the design says the swarm reads as "ask
// another holder". Counting it as a failure would retire a node for enforcing
// the limits we asked it to enforce; counting it as slowness would starve a
// busy-but-fast peer through the mechanism meant to find fast peers.
func TestQuotaRefusalCostsAWaitNotAMark(t *testing.T) {
	cp := testPlan(wideLayout(12), []*BlobProvider{{Name: "busy"}, {Name: "idle"}}, false)

	for i := 0; i < providerFailureLimit; i++ {
		cp.fail(dispatch(t, cp, 0), 0, errChunkBusy, false)
	}
	if cp.prov[0].dead {
		t.Error("a holder that answered 429 was retired for refusing under its own quota")
	}
	if cp.prov[0].fails != 0 {
		t.Errorf("fails = %d, want 0 — a quota refusal is not a failure", cp.prov[0].fails)
	}
	if !time.Now().Before(cp.prov[0].idleUntil) {
		t.Error("a 429 bought no backoff, so the swarm will ask straight back")
	}
	if cp.aborted {
		t.Error("the transfer gave up on a holder that is merely busy")
	}
}

// TestScheduleWaitsOutABackoffRatherThanSpinning: with a single holder resting
// after a failure there is nothing else to hand out, and take() must come back
// with the work once the rest is over rather than either spinning or blocking
// forever (the plan has no other broadcast to wake it).
func TestScheduleWaitsOutABackoffRatherThanSpinning(t *testing.T) {
	cp := testPlan(wideLayout(4), []*BlobProvider{{Name: "only"}}, false)
	cp.retry = 40 * time.Millisecond
	cp.fail(dispatch(t, cp, 0), 0, errors.New("mesh stalled"), false)

	start := time.Now()
	done := make(chan bool, 1)
	go func() {
		_, _, _, ok := cp.take()
		done <- ok
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("take gave up on a holder that was only resting")
		}
		if waited := time.Since(start); waited < cp.retry/2 {
			t.Errorf("take returned after %s, want it to wait out the %s backoff", waited, cp.retry)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("take never woke up after the backoff expired")
	}
}

// ── The manifest is a suspect too ────────────────────────────────────────────

// TestManifestAgreementIgnoresTheHoldersOwnName: two holders describing the same
// blob disagree about its filename as a matter of course — the library seeder
// knows it by its name, a node that fetched it has it under its hash — and
// reading that as a contradiction would make every mixed swarm look like a lie.
func TestManifestAgreementIgnoresTheHoldersOwnName(t *testing.T) {
	a := &blobManifest{Protocol: 1, Size: 100, ChunkSize: 10, Chunks: []string{"x", "y"}, Filename: "track.mp3"}
	b := *a
	b.Filename = "b1946ac92492d2347c6235b4d2611184"
	b.Protocol = 99
	if a.agreement() != b.agreement() {
		t.Error("two holders of the same bytes disagreed because they name the file differently")
	}
	c := *a
	c.Chunks = []string{"x", "z"}
	if a.agreement() == c.agreement() {
		t.Error("a different chunk hash agreed with the original")
	}
	d := *a
	d.ChunkSize = 20
	if a.agreement() == d.agreement() {
		t.Error("a different layout agreed with the original")
	}
}

// TestAgreedManifestNeedsASecondOpinion covers all three outcomes of the
// cross-check (.issues/open-issues.md, "a lying manifest retires every honest
// holder"). The mesh is stubbed out: what is under test is the rule, and three
// real nodes would only slow it down.
func TestAgreedManifestNeedsASecondOpinion(t *testing.T) {
	honest := &blobManifest{Size: 100, ChunkSize: 10, Chunks: []string{"x", "y"}, Filename: "a"}
	liar := &blobManifest{Size: 100, ChunkSize: 10, Chunks: []string{"x", "NOPE"}, Filename: "b"}
	answers := func(m map[string]*blobManifest) func(*BlobProvider) *blobManifest {
		return func(p *BlobProvider) *blobManifest { return m[p.Name] }
	}
	p := func(names ...string) []*BlobProvider {
		var out []*BlobProvider
		for _, n := range names {
			out = append(out, &BlobProvider{Name: n})
		}
		return out
	}
	ctx := context.Background()

	t.Run("two agree, the liar loses", func(t *testing.T) {
		got := agreedManifest(ctx, p("liar", "one", "two"), answers(map[string]*blobManifest{
			"liar": liar, "one": honest, "two": honest,
		}))
		if got == nil || got.agreement() != honest.agreement() {
			t.Errorf("agreed manifest = %v, want the one two holders described", got)
		}
	})

	t.Run("one against one is undecidable", func(t *testing.T) {
		// Nothing here can say which of them is lying, so the swarm gives way to
		// the whole-file path — which carries its own reference, the content hash.
		if got := agreedManifest(ctx, p("liar", "one"), answers(map[string]*blobManifest{
			"liar": liar, "one": honest,
		})); got != nil {
			t.Errorf("agreed manifest = %v, want nil: two holders contradicted each other", got)
		}
	})

	t.Run("a sole voice is believed", func(t *testing.T) {
		// The case F9 item 1 exists for: a partial seeder cannot BUILD a manifest,
		// so a swarm of one complete holder and several partials has exactly one
		// voice by construction. Refusing it would refuse the whole feature.
		got := agreedManifest(ctx, p("complete", "partial", "partial2"), answers(
			map[string]*blobManifest{"complete": honest},
		))
		if got == nil || got.agreement() != honest.agreement() {
			t.Errorf("agreed manifest = %v, want the only holder that could build one", got)
		}
	})

	t.Run("nobody answers", func(t *testing.T) {
		if got := agreedManifest(ctx, p("old", "older"), answers(nil)); got != nil {
			t.Errorf("agreed manifest = %v, want nil (F3 fallback)", got)
		}
	})
}

// TestBlameFallsOnTheReferenceWhenHoldersDisagreeWithIt is the other half of the
// same defect. errChunkCorrupt blames the chunk's SENDER, but the accusation
// comes from the MANIFEST's sender, and those are different nodes — so one lying
// manifest would retire every honest holder in the swarm, one confident
// judgement at a time.
func TestBlameFallsOnTheReferenceWhenHoldersDisagreeWithIt(t *testing.T) {
	cp := testPlan(wideLayout(12), []*BlobProvider{{Name: "a"}, {Name: "b"}, {Name: "c"}}, false)

	// The first corrupt chunk is unambiguous evidence about its sender: no amount
	// of environmental bad luck produces wrong bytes.
	cp.fail(dispatch(t, cp, 0), 0, errChunkCorrupt, true)
	if !cp.prov[0].dead {
		t.Fatal("the first holder to serve corrupt bytes was not retired")
	}
	if cp.aborted {
		t.Fatal("one corrupt chunk ended the transfer; two other holders were still live")
	}

	// The second, from a different holder, says something else entirely.
	cp.fail(dispatch(t, cp, 1), 1, errChunkCorrupt, true)
	if cp.prov[1].dead {
		t.Error("the second holder was condemned by the same reference the first was")
	}
	if cp.prov[0].dead {
		t.Error("the first holder was not reinstated once its accuser became the suspect")
	}
	if !errors.Is(cp.err, errManifestSuspect) {
		t.Errorf("abort error = %v, want it to name the manifest", cp.err)
	}
	if !cp.aborted {
		t.Error("a suspect manifest must end the swarm attempt so the whole-file path can take over")
	}
}
