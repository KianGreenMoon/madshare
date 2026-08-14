# Work queue — sequenced handoff (written 2026-08-13)

This is the **ordered** plan for the next sessions: what to build, in which
order, and what is known-stale. It holds sequence, status and pointers only —
every design lives in its named doc and is not repeated here. It is distinct
from `roadmap.md` (consciously deferred work, grouped by area, deliberately
unordered) and from `.issues/*.md` (defects + unanswered questions).

Context: on 2026-08-13 all six design problems in the owner's plain-words
explainer (`/home/kian/federation-explained.txt` §7) were dug and closed —
#1 node-store fold (mig 046), #2 audience ladder (mig 047), #3 catalog
diff-apply (`c884777`), #5 fetch paths (analysis), #6 knob resolution
(`4e79b7d`); #4 stays parked as F10. The availability round-2 design
(`aedc662`) came out of the follow-on consultation and **shipped 2026-08-13**
(migration 048). This queue is what remains.

## The sequence

### 1. Availability Phase 5 — reactive down-mark + ping floor  ✅ DONE 2026-08-13

Shipped as migration **048** (`federation_nodes.unreachable_at`) plus
`federation/reachability.go`. What was built and the two decisions the design
left to the build (the floor's per-round budget and its own in-memory clock)
are written up in `docs/plans/availability.md` §Phase 5 and
`docs/architecture/federation.md` §Availability. The 429-proof-of-life fix rode
along as designed.

**Confirmed live** on 2026-08-13, on a 4-node meshlab chain: a partitioned
3-hop member (the pull-window case the mark exists for) kept its tracks until
one failed fetch, then lost them within seconds; the nodes still answering were
not marked; the mark retired itself on the next successful fetch; and
partitioning the observer marked nobody. Written up as
`tests/mesh/README.md` §"The down-mark walkthrough". The ping floor cannot be
seen at that scale and stays unit-tested.

### 2. Node struct fold — the follow-up of migration 046  ✅ DONE 2026-08-13

`Peer` / `CatalogSource` / `HomeNode` are one `federation.ExternalNode`. The
name and the field-naming rule (columns win; JSON tags keep the pre-fold admin
wire) are written up in `docs/architecture/federation-nodes.md` §"The Go
surface — the fold". Pure refactor: no migration, no wire change, `PeerStore`
method names untouched.

Two things a future reader should not re-derive: the fold could **not** be
called `Node` (the running mesh node owns that name in this package), and
`BlobProvider` deliberately stayed its own flat type — a household device is a
holder with no row at all.

### 3. Member quotas: runtime + visibility  ✅ DONE 2026-08-14

Owner answered yes to both halves: config keeps the default, the override is
stored in the database, and an unset override falls back to the file. All four
quotas are three-layer knobs now, resolved on the rates' own memo through
`WithLimitResolver` (one struct, one query per refresh) and edited in the
`/admin/swarm` limits modal — the page that shows the traffic is the page that
throttles it, and it is also where the per-counterparty panel makes an operator
notice a member worth bounding. No migration: settings keys only.

Written up in `docs/architecture/swarm-admin.md` §"The member budget"; the
placement rule above it now records that it has been applied twice and answered
differently each time (this one three layers, the cache age two). Two things a
future reader should not re-derive: the buckets are **live** (`setLimits` reaches
the requesters already being served), and the refresh happens **at admission**,
because the concurrency halves of the budget are spent there rather than on the
write path.

### 4. Cache retention, the age half  ✅ DONE 2026-08-13

`madnetwork.cache_max_age_days`, shipped off, swept before the ceiling on the
same hourly pass and in the same save request. Written up in
`docs/architecture/madnetwork-cache.md` §"The retention ceiling" + its decision
log. One thing the plan did not anticipate: the knob got **two** layers, not
the ceiling's three — the placement rule in `swarm-admin.md` names no config
consumer for an age, and that section now records the answer.

### 5. Request depth on a sole capped holder  ✅ DONE 2026-08-14

**Owner picked shape 1 — depth 1 while a plan has exactly one live holder.**
Built as `chunkPlan.requestCapLocked` (`federation/scheduler.go`): the cap
`rankLocked` filters on is the plan's current state rather than the constant, so
`maxHolderRequests` still governs every plan with an alternative and the narrow
case gets its floor. No migration, no wire change, no new constant, and no
change to the multi-holder path.

Pinned by `TestASoleHolderIsAskedForOneChunkAtATime` and
`TestRetiringTheLastRivalNarrowsTheSurvivorToOneRequest` (the same rule arriving
the way it arrives in a real plan — retirement, not construction), both verified
to fail with the rule disabled. `TestOneHolderIsNotAskedForEverythingAtOnce` was
a SOLE-holder plan and now pins the general ceiling on two holders, which is the
shape that ceiling governs. `TestChaosReaderLatencyOnASoleCappedHolder` said in
its own comment that it was the test to tighten once this was decided, and it is:
**measurement → assertion at 2× the link's floor**, verified both ways — 8
uniform reads of ~595 ms (1.2× a 500 ms floor, identical over three runs) with
the rule, and 7 reads with a worst of 1.208 s at offset 0 (2.4×) without it.

One behaviour change fell out and is deliberate: on a sole holder the speculative
chunk-0 fetch now occupies the whole depth, so the plan waits for it rather than
starting chunk 1 beside it. `TestChunkPlanPrioritizeAndAdoptedFlight` hung on the
old assertion and now pins both halves — a second holder still starts at chunk 1,
a sole holder waits. It does not re-open the dribbling-first-holder finding
(`3ff5846`): that fix is a *second* holder taking the chunk over, and a plan with
a second holder is not at depth 1.

Verification: full `go test ./federation/` green in 172 s; the chaos suite
(`MADSHARE_CHAOS=1`, 380 s) green except the pre-existing
`TestStaleHoldersCostAFetch`, which fails identically at HEAD for the reason
already logged in `.issues`.

Written up in `docs/architecture/federation-swarm.md` §"…and the ceiling drops
to one when there is nobody else to ask", which carries both measurement tables:
the throughput one that shipped depth 2 and the reader one that narrowed it.

The record of what was measured and why, kept because it is the argument:

**The queue's first open slot, and the only one with a fresh measurement behind
it.** Reproduced 2026-08-14 (`.issues/open-issues.md` §"Madnetwork playback
stops mid-track", the in-flight-chunk row, which SPLIT on the measurement).

The fixed half needs no slot: `prioritize` marking an in-flight chunk and
`take()` hedging it ahead of the queue (F9 item 4) closes the multi-holder case
outright — worst reader wait 20/39/109 ms over three runs against a holder
throttled to 128 KiB/s, hedges won every time. Now pinned by
`TestBlockedReaderHedgesTheChunkItWaitsFor` (`federation/streaming_test.go`),
which is the first test of that rule; every other hedge test is the empty-queue
endgame.

**What is left is one question: how deep should a swarm ask a holder when it is
the ONLY holder and its link is capped?** That is the household shape by
construction — a madplayer's sole holder is its home server — and it is the
deployment the original mid-track symptom came from.

Measured, 2 MiB / 8 chunks over a 128 KiB/s link, three runs each. A 256 KiB
chunk at 128 KiB/s is ~2 s, so depth 1 is the floor:

| `maxHolderRequests` | worst reader wait | retries | rate | total |
|---|---|---|---|---|
| 2 (shipped) | 4.86 / 5.54 / 9.23 s | 0–4 | 76–108 KiB/s | 19.0–23.2 s |
| 1 (control) | 2.396 / 2.405 / 2.415 s | 0 | 108 KiB/s | 19.0–19.1 s |

Mechanism: the second slot puts a chunk **nobody has asked for** on the same
capped link as the chunk the reader is blocked on. Neither existing rule can
reclaim it — reordering cannot reach a dispatched chunk, and hedging needs a
second holder. Contention alone, not retries: the 5.54 s run reported
`retries=0`; chunks blowing `Timeouts.PerChunk` are a second-order consequence
that only appears once the link is oversubscribed.

**Why this was not caught by the F9 depth measurement** (1/2/4 deep =
12.36/12.30/12.80 s, which is why depth 2 shipped): that measured THROUGHPUT,
and throughput is genuinely unaffected here — 19.0 s either way. The entire
cost lands on the streaming reader's tail latency, which nothing measured until
now. Both numbers are right; they answer different questions.

**A second, independent scenario landed on the same cause** and is folded in
here rather than sequenced separately. The `.issues` row claiming a stall at the
lead-ramp → first-bulk-chunk transition (~768 KiB) was measured on 2026-08-14
and does not hold: over a 16 MiB blob the reader never blocks at 768 KiB on a
capped link (0 of 4 runs) because the parallel workers land the bulk chunks
while it is still stuck at the front. Its worst read is at **256 KiB** — the
chunk-0 → chunk-1 handover, chunk 0 being the speculative prefetch — and the
wait window fits chunks 1+2+3 sharing the link exactly. At depth 1 that run
becomes 18 uniform reads of 1.18 s, which is one 1 MiB chunk at 1 MiB/s: the
floor again, with 768 KiB indistinguishable from any later chunk. Worst read
2.48–3.25 s → 1.18 s, **total elapsed unchanged at 18.8 s**. So the ramp needs
no fix of its own, and the case for deciding the depth is now two measurements
from different scenarios rather than one.

**The decision — taken 2026-08-14, shape 1.** Depth is resource policy, so this
was the owner's, not the build's — and a blanket depth 1 is the wrong answer: it
would give up pipelining on healthy multi-holder swarms, where the second slot is
what keeps a fast holder busy. Two shapes that do not:

1. **Depth 1 while a plan has exactly one live holder.** ✅ **CHOSEN.**
   Narrowest possible; the condition is already computed
   (`liveProvidersLocked`). Costs nothing in any multi-holder plan, and the
   sole-holder case is precisely where the second slot can only take bandwidth
   from the first.
2. **Hold the second slot back while a reader is blocked.** More general — it
   also covers a two-holder plan where both are capped — and the signal already
   exists (`cp.wanted` is exactly "a reader is waiting on this"). Costs a
   little throughput on a plan whose reader is always blocked, i.e. any stream
   slower than its link. **Not built**, and it stays available: if a two-capped-
   holder plan is ever measured to starve a reader, this is the shape that
   answers it, and it composes with what shipped rather than replacing it.

Neither needs a migration, a wire change, or a new constant. Verification is
the repro pair itself, parked outside the repo (see the issue row) — the
scheduler-level one is deterministic; the chaos one wants `MADSHARE_CHAOS=1`
and reports the worst single `WaitFor`, which is the number that must move.
**Note for whoever measures a swarm change after this: time the reader, not the
transfer.** Total elapsed hid this for a month.

### Parked — named triggers, do NOT schedule

- **F10 merkle identity** — trigger: video support, or a *measured*
  all-partials reassembly failure (`federation-swarm.md`).
- **Fully-cached availability arm** (availability.md Phase 2b) — trigger:
  cache-only content proven to matter; needs a complete-cache-hash table +
  its own migration.
- **Catalog-delta announce + discovery-speed budget** — decided to be decided
  together (`federation.md` §"Later (decided-deferred)").
- **Since-serial catalog deltas (wire)** — trigger: measured wire pain; the
  apply-side churn is already fixed (`c884777`), and the escape hatch is
  recorded in `federation.md` §Catalog (no protocol break needed).
- **madplayer 2b remainder** (other repo): swarm fetch + materialize target —
  in madplayer's court; reaches this repo only as design questions.

## Known-stale — clean-up candidates, smallest first

1. ~~**gofmt drift**~~ — cleared 2026-08-13 (`e44d11c`); it was six files, not
   five (`tagsource/acoustid.go` had drifted since the note was written).
2. ~~**Migration-number claims**~~ — 048 is claimed and taken.
3. **The explainer's closing priority paragraph**
   (`/home/kian/federation-explained.txt`, "If you want my priority: …") is
   stale now that all six items carry verdicts — owner's file, outside the
   repo; fix on the next edit rather than for its own sake.

## Open defects — waiting on a repro or an owner call, NOT on a slot

Per the standing rule, none of these get a fix without a reproduction; they
are listed so the queue is honest about what it does *not* schedule.

- **Madnetwork playback stops mid-track** (High, unreproduced; all three
  candidate fixes merged in v0.8.5 and the symptom never seen since — row
  stays open; a length check would falsely pass the prime suspect, only
  content/hash verification catches it). Its in-flight-chunk row is no longer
  part of this bullet: it was reproduced 2026-08-14 and split — half fixed and
  now pinned, half sequenced as slot 5 above.
- **Fetch-path drift pair** (`.issues` §"fetch-path dig findings"): local
  `os.Rename` failure triggers the network fallback (Low); 429 in whole-file
  mode skips the patience rule (Info). Fix shapes are written in the rows.
- ~~**Recording-tagsets review findings**, 5 still open~~ — **stale, corrected
  2026-08-14**: every row in that section carries a fix (2026-07-14 through
  2026-08-08), each with named tests verified to fail beforehand, and the one
  remaining Info row is closed by design. Nothing there is waiting on a slot.

## Verification ritual (applies to every slot above)

Go lives at `~/.guix-home/profile/bin/go` (not on PATH). Full tree build
under both tags (`-tags nofederation` too), `go vet ./...`, test the tree
with the federation suite run separately, `-race` needs `-timeout ≥ 3300s`
and no parallel suites. A new migration breaks `database_test.go`; a new
`Repository` method breaks the api `fakeRepo`; a `PeerStore` change wants
`go vet -tags tests ./tests/mesh/...`. When a phase ships, grep the
user-facing docs — the design doc is the one being edited, so it is the one
that stays right.
