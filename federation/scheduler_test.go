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
	cp := testPlan(wideLayout(8), holders)
	tr := newTransfer("h", "p", "p.part")

	// With everyone idle the two alternate: each dispatch loads one of them, so
	// the other becomes the least loaded.
	var got []string
	mine := map[int][]int{} // provider index → the chunks it was handed
	for i := 0; i < 4; i++ {
		d, ok := cp.take()
		if !ok {
			t.Fatal("nothing to dispatch")
		}
		got = append(got, d.p.Name)
		mine[d.pidx] = append(mine[d.pidx], d.idx)
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
		d, ok := cp.take()
		if !ok {
			t.Fatal("nothing to dispatch after the free holder came back")
		}
		if d.p != holders[free] {
			t.Errorf("dispatch %d went to %q, want the holder with nothing outstanding (%q)",
				i, d.p.Name, holders[free].Name)
		}
	}
}

// TestScheduleAsksAPartialHolderOnlyForWhatItHas is the pair-selection rule F9
// item 1 forced: once a downloader seeds what it has so far, "pick a provider,
// then a chunk" is simply wrong, because not every holder can serve every chunk.
func TestScheduleAsksAPartialHolderOnlyForWhatItHas(t *testing.T) {
	holders := []*BlobProvider{{Name: "partial"}, {Name: "complete"}}
	cp := testPlan(buildLayout(50, 10, nil), holders) // 5 chunks of 10
	cp.setCoverage(0, &haveMessage{Ranges: []ByteRange{{Start: 0, End: 20}}})
	cp.setCoverage(1, &haveMessage{Complete: true, Ranges: []ByteRange{{Start: 0, End: 50}}})

	// Each dispatch is resolved before the next is taken, as a worker does: one
	// holder may only have maxHolderRequests out at a time.
	tr := newTransfer("h", "p", "p.part")
	for i := 0; i < 5; i++ {
		d, ok := cp.take()
		if !ok {
			t.Fatalf("dispatch %d: nothing handed out", i)
		}
		if d.p.Name == "partial" && d.idx > 1 {
			t.Errorf("chunk %d was asked of the holder that only has [0,20)", d.idx)
		}
		cp.succeed(d.idx, d.pidx, tr, time.Millisecond)
	}
}

// ...but coverage is a snapshot of a growing thing, so it deprioritises rather
// than excludes. When nobody else will take a chunk, the partial holder is asked
// anyway: being wrong costs one fast 416, and never asking costs a source for
// the rest of the transfer.
func TestScheduleAsksAPartialHolderAnywayWhenNobodyElseWill(t *testing.T) {
	cp := testPlan(buildLayout(50, 10, nil), []*BlobProvider{{Name: "partial"}})
	cp.setCoverage(0, &haveMessage{Ranges: []ByteRange{{Start: 0, End: 20}}})

	seen := map[int]bool{}
	tr := newTransfer("h", "p", "p.part")
	for i := 0; i < 3; i++ {
		d, ok := cp.take()
		if !ok {
			t.Fatalf("dispatch %d: the sole holder was written off over stale coverage", i)
		}
		seen[d.idx] = true
		cp.succeed(d.idx, d.pidx, tr, time.Millisecond)
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
	cp := testPlan(wideLayout(12), []*BlobProvider{{Name: "busy"}, {Name: "idle"}})

	for i := 0; i < providerFailureLimit; i++ {
		cp.fail(askHolder(t, cp, 0), 0, errChunkBusy, false)
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

// TestQuotaRefusalSpendsNoAttemptBudget pins the 2026-08-13 decision: a 429
// never counts toward attemptLimit. Before this, four polite refusals from a
// sole holder aborted the transfer — and a sole holder is the household's
// NORMAL shape, since a listener node has exactly one holder (its home server)
// and draws on the member budget. The swarm waits a quota out; it does not
// fail over it.
func TestQuotaRefusalSpendsNoAttemptBudget(t *testing.T) {
	cp := testPlan(wideLayout(1), []*BlobProvider{{Name: "busy"}})
	tr := newTransfer("h", "p", "p.part")

	// Well past the attempt budget (attemptLimit is 4 for one holder).
	for i := 0; i < 2*cp.attemptLimit; i++ {
		cp.fail(askHolder(t, cp, 0), 0, errChunkBusy, false)
	}
	if cp.aborted {
		t.Fatalf("the transfer gave up on a busy sole holder: %v", cp.err)
	}
	if cp.attempts[0] != 0 {
		t.Errorf("attempts = %d, want 0 — a refusal spent the attempt budget", cp.attempts[0])
	}

	// The moment the quota clears, the transfer completes as if nothing happened.
	cp.succeed(askHolder(t, cp, 0), 0, tr, time.Millisecond)
	if cp.remaining != 0 {
		t.Errorf("remaining = %d after the quota cleared, want 0", cp.remaining)
	}
}

// TestConsecutiveRefusalsBackOffFurther: with patience the swarm may wait a
// quota out for minutes, and re-asking a node that keeps saying no at the base
// cadence would be a poll, not a retry — so consecutive 429s from one holder
// double the pause (through backoffFor's cap), and any success clears the run.
func TestConsecutiveRefusalsBackOffFurther(t *testing.T) {
	cp := testPlan(wideLayout(2), []*BlobProvider{{Name: "busy"}})
	tr := newTransfer("h", "p", "p.part")

	cp.fail(askHolder(t, cp, 0), 0, errChunkBusy, false)
	first := cp.prov[0].idleUntil
	cp.fail(askHolder(t, cp, 0), 0, errChunkBusy, false)
	second := cp.prov[0].idleUntil

	// The escalation step is deterministic even on a slow machine: the second
	// rest ends at least one whole extra backoff step after the first.
	step := backoffFor(cp.retry, busyBackoffSteps+1) - backoffFor(cp.retry, busyBackoffSteps)
	if second.Sub(first) < step {
		t.Errorf("second refusal rested until %v, only %v past the first — no escalation",
			second, second.Sub(first))
	}
	if cp.prov[0].busy != 2 {
		t.Errorf("busy streak = %d, want 2", cp.prov[0].busy)
	}
	cp.succeed(askHolder(t, cp, 0), 0, tr, time.Millisecond)
	if cp.prov[0].busy != 0 {
		t.Errorf("busy streak = %d after a success, want 0", cp.prov[0].busy)
	}
}

// TestBusyOnlyPlanAbortsAfterPatience is the bound that lets refusals stop
// counting: a plan that has delivered NOTHING for Timeouts.Transfer gives up,
// carrying the refusal as its reason. Without this, a holder over a quota that
// never clears would hold the transfer open forever.
func TestBusyOnlyPlanAbortsAfterPatience(t *testing.T) {
	cp := newChunkPlan(context.Background(), wideLayout(1), []*BlobProvider{{Name: "busy"}},
		newTransferStats(),
		Timeouts{Retry: time.Millisecond, PerChunk: time.Minute, Transfer: 30 * time.Millisecond})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			d, ok := cp.take()
			if !ok {
				return
			}
			cp.fail(d.idx, d.pidx, errChunkBusy, false)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the plan never gave up on a holder that refused forever")
	}
	if !errors.Is(cp.err, errChunkBusy) {
		t.Errorf("abort reason = %v, want it to carry the quota refusal", cp.err)
	}
}

// TestScheduleWaitsOutABackoffRatherThanSpinning: with a single holder resting
// after a failure there is nothing else to hand out, and take() must come back
// with the work once the rest is over rather than either spinning or blocking
// forever (the plan has no other broadcast to wake it).
func TestScheduleWaitsOutABackoffRatherThanSpinning(t *testing.T) {
	cp := testPlan(wideLayout(4), []*BlobProvider{{Name: "only"}})
	cp.retry = 40 * time.Millisecond
	cp.fail(askHolder(t, cp, 0), 0, errors.New("mesh stalled"), false)

	start := time.Now()
	done := make(chan bool, 1)
	go func() {
		_, ok := cp.take()
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
		got, voices := agreedManifest(ctx, p("liar", "one", "two"), answers(map[string]*blobManifest{
			"liar": liar, "one": honest, "two": honest,
		}))
		if got == nil || got.agreement() != honest.agreement() {
			t.Errorf("agreed manifest = %v, want the one two holders described", got)
		}
		if voices != 2 {
			t.Errorf("voices = %d, want 2 — a quorum outranks the advertised size, so the count must say quorum", voices)
		}
	})

	t.Run("one against one is undecidable", func(t *testing.T) {
		// Nothing here can say which of them is lying, so the swarm gives way to
		// the whole-file path — which carries its own reference, the content hash.
		if got, _ := agreedManifest(ctx, p("liar", "one"), answers(map[string]*blobManifest{
			"liar": liar, "one": honest,
		})); got != nil {
			t.Errorf("agreed manifest = %v, want nil: two holders contradicted each other", got)
		}
	})

	t.Run("a sole voice is believed", func(t *testing.T) {
		// The case F9 item 1 exists for: a partial seeder cannot BUILD a manifest,
		// so a swarm of one complete holder and several partials has exactly one
		// voice by construction. Refusing it would refuse the whole feature.
		got, voices := agreedManifest(ctx, p("complete", "partial", "partial2"), answers(
			map[string]*blobManifest{"complete": honest},
		))
		if got == nil || got.agreement() != honest.agreement() {
			t.Errorf("agreed manifest = %v, want the only holder that could build one", got)
		}
		if voices != 1 {
			t.Errorf("voices = %d, want 1 — a sole voice owes the size cross-check its honesty", voices)
		}
	})

	t.Run("nobody answers", func(t *testing.T) {
		if got, _ := agreedManifest(ctx, p("old", "older"), answers(nil)); got != nil {
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
	cp := testPlan(wideLayout(12), []*BlobProvider{{Name: "a"}, {Name: "b"}, {Name: "c"}})

	// The first corrupt chunk is unambiguous evidence about its sender: no amount
	// of environmental bad luck produces wrong bytes.
	cp.fail(askHolder(t, cp, 0), 0, errChunkCorrupt, true)
	if !cp.prov[0].dead {
		t.Fatal("the first holder to serve corrupt bytes was not retired")
	}
	if cp.aborted {
		t.Fatal("one corrupt chunk ended the transfer; two other holders were still live")
	}

	// The second, from a different holder, says something else entirely.
	cp.fail(askHolder(t, cp, 1), 1, errChunkCorrupt, true)
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

// ── Hedging (F9 item 4) ──────────────────────────────────────────────────────

// TestEndgameHedgesAChunkNobodyElseCanRescue is the gap item 3 left behind and
// named: dispatch stops HANDING work to a holder that is not delivering, but it
// cannot get back the chunk already in that holder's hands, so a transfer's tail
// was as slow as its slowest live holder however many idle holders were watching.
func TestEndgameHedgesAChunkNobodyElseCanRescue(t *testing.T) {
	cp := testPlan(wideLayout(2), []*BlobProvider{{Name: "slow"}, {Name: "fast"}})
	tr := newTransfer("h", "p", "p.part")

	slow, ok := cp.take()
	if !ok {
		t.Fatal("nothing to dispatch")
	}
	fast, ok := cp.take()
	if !ok {
		t.Fatal("nothing to dispatch to the second holder")
	}
	// The fast holder is done and the queue is empty, so there is nothing better
	// for the freed worker to do than help with the chunk still outstanding.
	cp.succeed(fast.idx, fast.pidx, tr, time.Millisecond)

	hedge, ok := cp.take()
	if !ok {
		t.Fatal("the freed worker was sent away while a chunk sat with a slow holder")
	}
	if !hedge.hedge || hedge.idx != slow.idx {
		t.Fatalf("take returned chunk %d (hedge=%v), want a hedge of chunk %d",
			hedge.idx, hedge.hedge, slow.idx)
	}
	if hedge.pidx == slow.pidx {
		t.Error("the hedge went back to the holder already sitting on the chunk, which is not a second chance")
	}

	// The hedge lands first. The copy that lost must be STOPPED, not merely
	// ignored: fetchSwarm waits for every worker, so an abandoned fetch would
	// hold the transfer open for exactly as long as hedging was meant to save.
	cp.succeed(hedge.idx, hedge.pidx, tr, time.Millisecond)
	select {
	case <-slow.ctx.Done():
	default:
		t.Error("the losing copy was left running after another holder delivered the chunk")
	}

	// And its holder is not blamed for losing a race we started.
	cp.fail(slow.idx, slow.pidx, context.Canceled, false)
	if cp.prov[slow.pidx].fails != 0 {
		t.Errorf("the losing holder collected %d failure(s) for being second", cp.prov[slow.pidx].fails)
	}
	if len(cp.pending) != 0 {
		t.Errorf("a completed chunk was re-queued by its cancelled copy: %v", cp.pending)
	}
	if cp.remaining != 0 {
		t.Errorf("remaining = %d, want 0", cp.remaining)
	}
	st := cp.stats.snapshot("h", 0, 0)
	if st.Hedges != 1 || st.HedgesWon != 1 {
		t.Errorf("stats say hedges=%d won=%d, want 1 and 1 — the trade has to be readable",
			st.Hedges, st.HedgesWon)
	}
}

// TestHedgeJumpsAheadForAReaderThatIsBlocked: prioritize can reorder the queue,
// but the chunk a stalled stream is waiting for has usually left it — it is in
// flight, on whichever holder happens to be slow. A second copy is the only
// thing that can make it arrive sooner, and it is worth more than starting a
// chunk nobody has asked for yet.
func TestHedgeJumpsAheadForAReaderThatIsBlocked(t *testing.T) {
	cp := testPlan(wideLayout(6), []*BlobProvider{{Name: "slow"}, {Name: "fast"}})

	stuck, ok := cp.take()
	if !ok {
		t.Fatal("nothing to dispatch")
	}
	cp.prioritize(stuck.idx) // a reader blocks on the offset it covers

	d, ok := cp.take()
	if !ok {
		t.Fatal("nothing to dispatch")
	}
	if !d.hedge || d.idx != stuck.idx {
		t.Errorf("take returned chunk %d (hedge=%v) with five chunks queued, want a hedge of the "+
			"chunk the reader is waiting for (%d)", d.idx, d.hedge, stuck.idx)
	}
}

// A chunk is never fetched more than maxChunkCopies times over. Duplication is
// bandwidth spent twice, and past the second copy it buys progressively less for
// the same price each time.
func TestHedgingStopsAtTwoCopies(t *testing.T) {
	cp := testPlan(wideLayout(1), []*BlobProvider{{Name: "a"}, {Name: "b"}, {Name: "c"}})

	first, _ := cp.take()
	cp.prioritize(first.idx)
	second, ok := cp.take()
	if !ok || !second.hedge {
		t.Fatal("the wanted chunk was not hedged at all")
	}
	// The third holder is free, eligible, and must still not be given a copy.
	done := make(chan bool, 1)
	go func() {
		d, ok := cp.take()
		done <- ok && d.hedge
	}()
	select {
	case hedged := <-done:
		if hedged {
			t.Error("a third copy of one chunk was dispatched")
		}
	case <-time.After(200 * time.Millisecond):
		// Blocking is the right answer: there is no work this worker may do.
	}
}

// A hedge that FAILS while the copy it was racing is still running must not
// re-queue the chunk. Two dispatches out of one failure is how a scheduler
// quietly doubles its own work.
func TestAFailedHedgeDoesNotRequeueAChunkStillInFlight(t *testing.T) {
	cp := testPlan(wideLayout(2), []*BlobProvider{{Name: "a"}, {Name: "b"}})
	tr := newTransfer("h", "p", "p.part")

	first, _ := cp.take()
	other, _ := cp.take()
	cp.succeed(other.idx, other.pidx, tr, time.Millisecond)
	hedge, ok := cp.take()
	if !ok || !hedge.hedge {
		t.Fatal("the outstanding chunk was not hedged")
	}

	cp.fail(hedge.idx, hedge.pidx, errors.New("mesh stalled"), false)
	if len(cp.pending) != 0 {
		t.Errorf("pending = %v, want empty — the original copy is still fetching that chunk", cp.pending)
	}
	if cp.prov[hedge.pidx].fails != 1 {
		t.Errorf("the hedge's holder recorded %d failures, want 1 — it did genuinely fail",
			cp.prov[hedge.pidx].fails)
	}
	// The original is still the chunk's owner and still finishes it.
	cp.succeed(first.idx, first.pidx, tr, time.Millisecond)
	if cp.remaining != 0 {
		t.Errorf("remaining = %d, want 0", cp.remaining)
	}
}

// TestOneHolderIsNotAskedForEverythingAtOnce pins the pipelining half of F9
// item 4, which the measurement turned around: the design wanted a request-depth
// FLOOR per holder ("keep the pipe full across the RTT"), and what the numbers
// asked for was a ceiling.
//
// Measured over a 300 ms-RTT link capped at 512 KiB/s: depth 1, 2 and 4 took
// 12.36 s, 12.30 s and 12.80 s — the dead air is real and a queueing link
// absorbs it — while depth 8 FAILED the transfer, because eight chunks sharing
// one capped link each take eight times as long and blow Timeouts.PerChunk
// together. Workers are capped in total, so a plan of four holders with one
// answering reaches exactly that.
//
// Two holders, because that is the shape the ceiling governs: a plan down to one
// live holder is asked for one chunk at a time instead, see the test below.
func TestOneHolderIsNotAskedForEverythingAtOnce(t *testing.T) {
	holders := []*BlobProvider{{Name: "a"}, {Name: "b"}}
	cp := testPlan(wideLayout(12), holders)
	tr := newTransfer("h", "p", "p.part")

	var out []dispatch
	reqs := map[int]int{}
	for i := 0; i < maxHolderRequests*len(holders); i++ {
		d, ok := cp.take()
		if !ok {
			t.Fatalf("dispatch %d: the plan handed out nothing", i)
		}
		out = append(out, d)
		if reqs[d.pidx]++; reqs[d.pidx] > maxHolderRequests {
			t.Fatalf("holder %d is fetching %d chunks at once, want at most %d",
				d.pidx, reqs[d.pidx], maxHolderRequests)
		}
	}

	// Every slot is taken, so the next worker must wait rather than pile another
	// request on somebody already at depth.
	done := make(chan bool, 1)
	go func() {
		_, ok := cp.take()
		done <- ok
	}()
	select {
	case <-done:
		t.Fatalf("a %dth request went to a holder already fetching %d chunks",
			maxHolderRequests+1, maxHolderRequests)
	case <-time.After(200 * time.Millisecond):
	}

	// ...and it is released as soon as a slot frees, so nothing is stuck.
	cp.succeed(out[0].idx, out[0].pidx, tr, time.Millisecond)
	select {
	case ok := <-done:
		if !ok {
			t.Error("the waiting worker was sent away instead of taking the freed slot")
		}
	case <-time.After(5 * time.Second):
		t.Error("a freed request slot never woke the waiting worker")
	}
}

// TestASoleHolderIsAskedForOneChunkAtATime pins the exception to the rule above
// (work-queue slot 5, decided 2026-08-14). With one live holder the second
// request slot cannot buy anything: both chunks share the one link, so a chunk
// nobody has asked for is taking bandwidth from the chunk a reader is blocked
// on, and neither rule that normally reclaims one applies — prioritize cannot
// reorder a dispatch that has left the queue, and a hedge needs a second holder.
//
// Measured over a 128 KiB/s link, 2 MiB, three runs each: the worst blocking
// read a streaming reader paid was 4.86 / 5.54 / 9.23 s at depth 2 against
// 2.396 / 2.405 / 2.415 s at depth 1, which is one 256 KiB chunk at the link
// rate — the floor. Total elapsed was 19.0 s either way, which is why the
// throughput measurement that shipped depth 2 scored it free.
func TestASoleHolderIsAskedForOneChunkAtATime(t *testing.T) {
	cp := testPlan(wideLayout(12), []*BlobProvider{{Name: "only"}})
	tr := newTransfer("h", "p", "p.part")

	first, ok := cp.take()
	if !ok {
		t.Fatal("the sole holder was not asked at all")
	}

	done := make(chan bool, 1)
	go func() {
		_, ok := cp.take()
		done <- ok
	}()
	select {
	case <-done:
		t.Fatal("a second chunk was dispatched to the only holder in the plan")
	case <-time.After(200 * time.Millisecond):
	}

	// The slot frees on the first chunk landing, so the transfer still proceeds
	// one chunk at a time rather than stalling.
	cp.succeed(first.idx, first.pidx, tr, time.Millisecond)
	select {
	case ok := <-done:
		if !ok {
			t.Error("the waiting worker was sent away instead of taking the freed slot")
		}
	case <-time.After(5 * time.Second):
		t.Error("a freed request slot never woke the waiting worker")
	}
}

// TestRetiringTheLastRivalNarrowsTheSurvivorToOneRequest is the same rule
// arriving the way it actually arrives in a real plan: the depth is a property
// of the plan RIGHT NOW, not of how it was built. A fetch that starts with four
// advertised holders and loses three of them to retirement is the sole-holder
// case by the time the survivor is carrying the transfer — which is precisely
// the plan shape the pipelining ceiling was written for.
func TestRetiringTheLastRivalNarrowsTheSurvivorToOneRequest(t *testing.T) {
	holders := []*BlobProvider{{Name: "live"}, {Name: "ghost"}}
	cp := testPlan(wideLayout(12), holders)

	cp.mu.Lock()
	if got := cp.requestCapLocked(); got != maxHolderRequests {
		cp.mu.Unlock()
		t.Fatalf("with two live holders the cap is %d, want %d", got, maxHolderRequests)
	}
	cp.prov[1].dead = true
	got := cp.requestCapLocked()
	cp.mu.Unlock()
	if got != 1 {
		t.Errorf("with the rival retired the cap is %d, want 1", got)
	}
}
