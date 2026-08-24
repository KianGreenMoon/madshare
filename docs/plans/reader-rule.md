# The reader rule — request depth follows who is waiting

**Status: DESIGN, NOT BUILT.** Step 3 of `docs/plans/maybe-to-do.md` §5, designed
2026-08-15 against that file's §8 measurements. Four decisions are marked
**DECIDE** below (the fourth added 2026-08-24 from the swarm-lab results, §6);
nothing is scheduled until they are agreed. Companion reading:
`federation-swarm.md` §"Pipelining" (what ships today), maybe-to-do.md §8 (the
evidence this design stands on), and `docs/plans/swarm-lab.md` — the balance
and siege runs, whose 2026-08-24 full pass re-measured the shipped swarm and
is folded in as §6 below.

The goal, from the owner: *depth should be unimportant*. The measurements say
what that has to mean in practice: request depth has exactly one cost that
matters — **reader tail latency** — and exactly one benefit that survives
measurement — **~15 % of transfer time on a sole latent holder, where there is
no cross-holder parallelism to hide the RTT**. Every other claimed effect
(transfer failures, multi-holder pipelining) measured out as either a test-clock
artifact or not there at all. So the rule is one sentence:

> **A transfer somebody is reading runs per-holder depth 1. A transfer nobody
> is reading keeps `maxHolderRequests` (2).**

"Somebody is reading" is a fact the code already observes — `WaitFor` pokes
`plan.prioritize` on every blocked read (transfer.go) — it just never *kept*
it. This design keeps it, as one sticky bit.

---

## 1. Why not the rule as originally worded

maybe-to-do.md step 3 said: *"do not start a chunk nobody has asked for while a
reader is blocked on one already in flight."* The multi-holder sweep killed
that wording. A streaming reader is blocked on the watermark chunk almost
continuously — that is what "reading as fast as bytes arrive" means — so the
literal rule would forbid holder B from starting chunk W+1 while holder A
fetches the chunk W the reader waits on. That serializes the swarm to one
chunk at a time across ALL holders, and cross-holder parallelism is precisely
where depth 1's speed came from: the 16 MiB depth-1 cells reached ~85 % of the
two links' aggregate payload ceiling *while the reader was blocked the whole
time*. The harm the sweep measured was never "chunks in flight while a reader
waits" — it was **un-asked-for chunks sharing one holder's link with the
reader's chunk**, plus the hedge duplicates that deep dispatch feeds. Depth 1
per holder removes both; serializing the plan would remove the speed too.

## 2. The mechanism

### 2a. The watched bit

`transfer` gains one sticky flag, `watched`, set unconditionally at the top of
`WaitFor`'s wait loop — **on the transfer, not the plan**, because the common
case sets it before the plan exists: a stream's first `WaitFor(0)` parks during
the manifest round trip, and a bit that lived on the plan would miss it.
`fetchSwarm` hands it to `newChunkPlan`; the plan reads it through one
function so there is exactly one consumer:

```go
func (cp *chunkPlan) requestCapLocked() int {
	if cp.watched() {          // any reader has ever blocked on this transfer
		return 1
	}
	return maxHolderRequests   // 2 — the F9 dead-air finding, unchanged
}
```

The `liveProvidersLocked() <= 1` arm — slot 5's sole-holder narrowing
(`30f13cc`) — **deletes**. It is subsumed: a watched transfer gets depth 1 on
every holder (which is slot 5's behaviour extended to the multi-holder shape
it never covered), and an *unwatched* sole-holder transfer gets its second
slot back — recovering the ~15 % of materialize time slot 5 gave up because it
could not tell a stream from a background fetch. The watched bit can.

Everything else in the scheduler is untouched: `take()`'s priority order, the
blocked-reader hedge (priority 1 — a reader's chunk is still raced to a second
holder unconditionally), the endgame hedge, sequential/seek priority,
`worseThanPeers`, the backoffs.

### 2b. Sticky, and why

`watched` never clears. The alternatives are a TTL ("no poke for N seconds")
— a guessed timing constant of exactly the kind this codebase keeps refusing —
or reader refcounting, which `WaitFor`'s stateless contract does not support.
Stickiness costs precisely what the sweeps measured: a stream that is
abandoned mid-transfer leaves the rest of the fetch at depth 1, which is free
on multiple holders (depth 1 measured equal-or-faster) and ≤ 15 % on a sole
holder at 300 ms RTT. A bounded, known cost beats an unbounded knob.

Shared transfers behave correctly by construction: `EnsureBlob` dedupes per
hash, so when a background materialize and a stream share one transfer, the
stream's first blocked read flips it — right, because now somebody *is*
waiting.

### 2c. Flip semantics: drain, never preempt

Narrowing mid-transfer reuses the mechanism slot 5 already documented: a
holder at 2 requests keeps both and is simply not handed a third until it is
back under the new cap. Costs at most one chunk-time once per holder, no
cancellations, no new code path.

### 2d. What deletes, what stays — the amended step 4

- **Deletes:** the sole-live narrowing in `requestCapLocked`; the parts of the
  `maxHolderRequests` comment that justify 2 by multi-holder pipelining
  (measured absent — the 2026-08-15 pointer already flags it).
- **Stays: `maxHolderRequests = 2` itself, as the unwatched ceiling.** This
  amends maybe-to-do step 4, which wanted the constant gone, and answers its
  §7 question 5 with **no** — on the evidence, not on caution: depth beyond 2
  measured zero benefit anywhere (F9: 1/2/4 ≈ equal on a sole latent holder;
  §8: nothing multi-holder), while uncapped depth costs more in-flight chunks
  at queue-empty (more endgame-hedge duplicates), more loss on a mid-transfer
  holder death, and — decisive for the suite — it re-creates the flooded-
  survivor shape `TestChaosOneLiveHolderIsNotFloodedWithWorkers` exists to
  forbid, which on the chaos clock's compressed ratios fails outright. A
  constant that a measurement now *supports* is not the constant the step-4
  objection was about.

### 2e. Hedge waste: mostly solved structurally, damper deferred

The 40–80 % wire overhead in §8's depth-2 cells was fed by deep dispatch: more
chunks in flight when the queue empties means more endgame duplicates, each
costing full bytes on a capped link. Watched depth 1 bounds in-flight to
≤ #holders, so a streamed transfer's endgame waste is bounded at
(holders − 1) duplicates; unwatched depth 2 is today's shipped behaviour minus
the reader-hedges (an unwatched plan has no wanted marks, so priority-1
hedging never fires — only the endgame). A rate-gated endgame damper ("only
hedge to a measurably faster holder") was considered and **deferred**: with
EWMA noise, "measurably" needs a margin constant, and the structural bound may
already be enough. The measure harness gets a cell to find out (§4).

## 3. What this fixes that ships broken today

The multi-holder sweep was run to answer a design question and found a live
defect on the way: **shipped streaming over two holders blocks the reader
9–10 s at a stretch** (16 MiB, symmetric 512 KiB/s holders — §8's depth-2 rows
ARE the shipped `maxHolderRequests = 2` behaviour, measured with a reader
attached). Slot 5 never covered this shape; the watched bit does. That makes
step 3 a fix with a measured repro, not only step 4's enabler.

## 4. Tests — regressions pinned, corrections proven

The change is one bit and one branch; the test surface is where the work is.

**Existing pins that must stay green, unmodified** (they assert behaviour,
not mechanism — the point of keeping them verbatim):

| test | what it holds |
|---|---|
| `TestChaosReaderLatencyOnASoleCappedHolder` | streamed sole holder stays on the one-chunk floor (watched ⇒ 1 replaces sole-live ⇒ 1) |
| `TestChaosBlockedReaderIsRescuedByAHedge` | the priority-1 reader hedge survives the rule |
| `TestChaosOneLiveHolderIsNotFloodedWithWorkers` | an unwatched plan still caps the lone survivor at 2 — the test that forbids uncapped unwatched depth |
| `TestStaleHoldersCostAFetch` | ghost cost bound unchanged (unwatched = today's depth) |
| `TestChaosDribblingFirstHolderDoesNotGateFirstByte` | speculation adoption unaffected (chunk 0's flight charges `reqs` exactly as now) |
| `streaming_test.go` unit set | `take()`'s order, `prioritize`'s mark, hedge selection |

Plus the standing ritual: full `go test ./federation/`, and the chaos suite
**unfiltered** under `MADSHARE_CHAOS=1`.

**New tests that ship WITH the change** (each proves one clause of the rule;
unit tests drive `chunkPlan` directly, the chaos test drives a real mesh):

1. `TestWatchedPlanDispatchesOneChunkPerHolder` (unit) — before any poke, a
   two-holder plan hands each holder two chunks; after the watched bit is set,
   new dispatches cap at one per holder.
2. `TestWatchedFlipDrainsInsteadOfPreempting` (unit) — a holder at two
   requests when the bit flips keeps both, resolves them normally, and is not
   handed a third until it is under the new cap. Nothing is cancelled.
3. `TestWatchedIsSticky` (unit) — completing the chunk the reader waited on
   does not restore depth 2; neither does the queue emptying.
4. `TestUnwatchedSoleHolderKeepsItsSecondSlot` (unit) — the slot-5 narrowing
   is gone: one live holder, no reader, `rankLocked` admits a second request.
   This is the 15 %-recovery clause, pinned at the mechanism level because a
   timing assertion would be flaky; the timing claim lives in the measure
   harness (below).
5. `TestChaosReaderLatencyOnTwoCappedHolders` (chaos) — **the red-to-green
   test for §3's shipped defect**: two symmetric capped holders, a streaming
   reader, assert the worst blocking read ≤ ~2× the one-chunk floor. Today
   this measures 4–5× the floor (§8, depth-2 rows); under the rule it is the
   multi-holder twin of `TestChaosReaderLatencyOnASoleCappedHolder` and holds
   the same bound. It lands in the same commit as the fix — the suite is
   never red — and its "before" numbers are already on record in §8.
6. Measure-harness additions (`MADSHARE_MEASURE=1`, not assertions): a
   watched-vs-unwatched pair per topology (sole + two-holder) to put numbers
   on the 15 %-recovery and the unwatched endgame waste (§2e's deferred
   damper decision feeds on this); re-run `TestMeasureMultiHolderDepth` after
   the change — its depth-2 rows describe a configuration that no longer
   ships.

**Bound to re-derive, not assume:** `TestStaleHoldersCostAFetch`'s ghost
budget was written against dispatch behaviour that the watched bit changes for
streamed plans (a watched plan probes ghosts one slot at a time, which can
only lower the bound — but "can only lower" is the kind of sentence the
chaos suite exists to check).

## 5. Rollout & interactions

- **Independent of step 2** (the `PerChunk` question). Nothing here touches
  deadlines; the two land in either order.
- **Supersedes slot 5** (`30f13cc`): its test stays as the regression pin, its
  mechanism is replaced, its comment block moves to `requestCapLocked`'s new
  rule with the watched framing.
- **Facade/client:** madplayer's `Network().Fetch` never calls `WaitFor`, so a
  device materialize runs unwatched at depth 2 — the household bulk-sync gets
  the recovered dead-air slot. In-process playback after materialize reads
  via `BlobPath`, not the transfer, so it never flips anything.
- **Docs to touch when built:** `federation-swarm.md` §"Pipelining" (the rule
  replaces the sole-holder paragraph), maybe-to-do.md §5 (steps 3–4 verdicts),
  the `maxHolderRequests` comment (rewritten around the unwatched ceiling).

## 6. What the swarm lab measured (2026-08-24)

The full swarm-lab pass (`docs/plans/swarm-lab.md`: the gated chaos suite
unfiltered, the same under `-race`, and three live `meshlab swarm` runs on a
triangle; raw logs under `tests/results/`, gitignored) ran with this design still
unbuilt — so every number below describes the SHIPPED swarm, i.e. the
"before" state this design would change. Three facts bear on the decisions.

### 6a. The unwatched ceiling holds up from a third direction

`TestChaosBalanceFollowsBandwidth` measured the split at shipped depth 2:
holders capped 1:2:4 carried 8 %/33 %/58 % of the bytes (ideal 14/29/57),
nobody healthy retired, aggregate throughput above the best single pipe.
`TestChaosBalanceIgnoresUnderlayDistance`: the holder two routed hops and
60 ms away with the fatter pipe out-carried the adjacent thin one 9 chunks
to 3. On the live triangle, flat links split 51/49. **Depth 2 does not
distort the bandwidth-following balance** — so keeping `maxHolderRequests = 2`
as the unwatched ceiling (decision 1) costs the swarm's balance nothing
measurable, on top of §2d's existing grounds.

### 6b. The reader pays for a bad holder even when the transfer doesn't

Live triangle, one holder capped to 64 KiB/s: the scheduler routed the whole
33.5 MiB to the healthy holder (100 % / 62 KiB — the balance working), the
transfer was fine — but the **first 64 KiB took 1.46 s against 59 ms on flat
links (25×)**, because a lead chunk landed on the capped holder and the reader
waited out its rescue. This extends §3's shipped defect beyond the symmetric
two-holder shape: it is not depth that hurt here (the dispatch was the FIRST
to that holder), it is that the reader's chunk sat on the worst link until the
blocked-reader hedge recovered it. The watched bit does not prevent that first
dispatch; the hedge is the rescue and its cost is what decision 3's deferred
damper would be tuned against — this run puts the first live number on it.

### 6c. The first byte's biggest enemy is not depth — it is the manifest wave

The dead-holder run: one of two known holders partitioned, the fetch
completed and verified from the survivor — and the **first byte waited
20.0 s**, the warm-session `Timeouts.Manifest` ceiling (5.03 s in the
cold-dial variant; `TestChaosDeadHoldersDoNotGateTheSwarm` shows the same
shape on the chaos clock, ttfb 2.0 s of 2.5 s elapsed vs 27 ms healthy).
Mechanism and full analysis: `.issues/open-issues.md` §"one dead holder in a
two-holder plan" — `agreedManifest` returns early only on the second
*matching* vote, and the "sole answer is believed" arm runs only after the
whole wave resolves, so with holders [dead, live] the live vote waits out the
dead probe. **Nothing in this design reaches it**: the watched bit is set
while `WaitFor` parks during the manifest round trip, but the cap it changes
governs chunk dispatch, which has not begun. Since this document is the
reader-latency design, the question belongs beside its decisions — it is
DECIDE 4 below. In the household/listener shape (two known holders, one of
them the home server's sometimes-off peer) this 5–20 s dwarfs everything the
depth rule wins.

## 7. DECIDE — the four open decisions

1. **Unwatched ceiling: keep `maxHolderRequests = 2`** (recommended, §2d,
   now also §6a — the balance runs show depth 2 does not distort the
   bandwidth-following split) — or delete the constant and let
   `maxChunkWorkers` bound everything, which measured zero benefit, breaks
   the flood test on the chaos clock, and spends more on endgame duplicates.
   Keeping 2 amends step 4 from "the cap deletes" to "the cap's *wrong
   trigger* deletes".
2. **Sticky watched bit** (recommended, §2b) — or a decay rule, which needs a
   timing constant this design cannot justify. Cost of sticky is bounded and
   measured (≤ 15 %, sole-holder, abandoned-stream case only).
3. **Endgame-hedge damper: defer** (recommended, §2e) — measure unwatched
   endgame waste first via the §4 harness cell; build a damper only if the
   structural bound (depth 1 while watched) leaves waste worth chasing. §6b's
   capped-lead-chunk run is the first live number on what a hedge rescue
   costs the reader (1.46 s vs a 59 ms floor).
4. **The manifest wave on a half-dead plan** (added 2026-08-24, §6c; full
   entry in `.issues/open-issues.md`): should `agreedManifest` settle on a
   **sole surviving vote once every other probe has terminally FAILED**?
   Recommended: **yes** — it needs no new constant and weakens no trust: the
   same sole vote is believed today anyway once the wave resolves, so the
   5–20 s wait buys nothing after the other probes have already *failed*
   (a failure is not a pending contradiction; the wait's purpose — letting a
   disagreeing vote arrive — is spent). The wider alternative (adopt the
   FIRST vote provisionally, chunk-0-speculation-style, and let the wave
   finish in the background) buys the cold-dial 5 s too, but it does trust a
   single description while a contradicting one may still arrive, which is
   the line `fetchAgreedManifest` exists to hold — the siege quorum run is
   the reminder that a manifest's word is only ever as good as the whole-file
   anchor behind it. Decide the narrow version here; the wide one only if
   the household numbers still hurt afterwards. This is the one decision of
   the four that moves first-byte latency more than the depth rule itself.
