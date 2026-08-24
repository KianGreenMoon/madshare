# The swarm lab — the balance run and the siege run

Two test families over the mesh test suite (`tests/mesh/README.md`), asked for
on 2026-08-24, before the reader-rule work: first prove the swarm is a good
citizen of a *decentralized* network, then prove it survives people who are not.

## The claim under test

A well-behaved swarm depends on exactly one thing: **the quality of the
connections it is actually using**. Everything else on the list of plausible
influences must be measurably free:

| Influence | What a good swarm does with it |
|---|---|
| link capacity / latency (the "internet connection") | **the only thing that decides the byte split** — a holder's share follows its pipe |
| friendship depth / hops between nodes | free — a mesh address derives from the key, every holder is dialled directly (`meshlab reach` proved this for a single holder; the balance run proves it for a swarm) |
| underlay distance (routed hops) | free up to the physics of the path — two hops behind a relay is just a link whose capacity is the minimum of its segments |
| dead nodes (unresponsive, switched off, long gone) | a **bounded tax**, never a gate — each dead holder costs at most its dispatches' dial deadlines while the live ones carry the transfer |
| where the bytes came from originally | irrelevant once spread — holder information travels (catalog ∪ holdings), so content must remain fetchable when its origin is gone |

And the siege modification: a swarm that is only correct among friends is not
decentralized either. The same lab, plus **liars** — coordinated nodes serving
wrong bytes and lying manifests — concentrated behind **one branch** of the
friendship graph or spread across **several**. The design's promise is precise
and deliberately limited, and the tests assert exactly it:

- liars can cost **time and wasted bytes, never content**: every fetch is
  anchored on the whole-file sha256, so the worst attack ends in the right
  bytes, later;
- a liar farm behind one friendship is **one voice** (branch weighting) and
  one admin block removes the whole branch;
- infiltrating *k* friendships buys **exactly k voices, no more** — that is the
  honest limit of the defense, stated rather than hidden.

## Placement — why the assertions live in the chaos suite

The suite's standing split (README §What's here): **the chaos suite asserts
what the swarm does** — in Go, in milliseconds, with `TransferStats` to prove
*why* it passed; **meshlab shows what a person sees**. Both families follow it:

- The assertable claims (byte split, bounded death tax, byte-exact survival of
  a liar quorum) are chaos scenarios in `federation/`, where the shrunk clock
  makes them cheap and `TransferStats` makes the *reason* assertable.
- A **liar in meshlab would require a hostile madshare build**. The chaos
  suite's liar is cheaper and just as real: a genuine `federation.Node` whose
  blob resolver maps the true hash to a file with different bytes — same wire,
  same handlers, same manifest builder, so it produces a *self-consistent lie*
  (its manifest honestly describes its wrong file), which is the strongest lie
  the protocol admits. meshlab could add nothing but wall-clock to that.
- What meshlab *can* uniquely show is the person-visible balance run on real
  servers — real libraries, the real download path, the per-counterparty byte
  ledger (`/api/admin/swarm/peers`) as the instrument. That is `meshlab swarm`.

## Family 1 — the balance run

`federation/chaosbalance_test.go`. All gated by `requireChaos`; all on the
shrunk chaos clock; every fetch verified byte-exact via `assertCached`. New
topology helper `startFaultedSwarm` (N holders, each listening behind its own
`netfault.Proxy`; one fetcher dialling all of them) — `startNodeQuad`'s shape
with `startFaultedPair`'s faultable links.

### `TestChaosBalanceFollowsBandwidth`

**Question:** is the download balanced between holders — and balanced by the
right thing?

Three holders carrying the same ~3 MiB blob (12 × 256 KiB bulk chunks — enough
grain to read a split from), link-capped 64 / 128 / 256 KiB/s. Asserts:

- the transfer completes byte-exact, `corrupt=0`, **nobody retired** — a slow
  holder is *deprioritized, never punished* (retirement is relative and needs a
  failure streak, not a thin pipe);
- the byte split follows capacity: the fat holder carries the plurality
  (≥ 35 % against its 4∕7 ideal), the thin one a minority (≤ 25 % against 1∕7),
  fat strictly above thin — tolerance-shaped, because EWMA dispatch plus
  endgame hedging is stochastic by design;
- **the swarm beats the best single pipe**: elapsed under `size ∕ fastest-cap`
  (×`testTimeoutScale` slack) — the aggregate-the-pipes claim that is the whole
  point of a multi-source fetch.

### `TestChaosBalanceIgnoresUnderlayDistance`

**Question:** does graph position cost anything once capacity is equal — or
worse, does the swarm prefer *near* over *fast*?

Four nodes: fetcher, `near` (holder, one underlay hop, capped 64 KiB/s),
`mid` (an empty relay node — no library, no role but routing), `far` (holder
peered only to `mid`, so its traffic to the fetcher crosses two underlay hops,
plus 60 ms RTT on its link — and capped 128 KiB/s). The farther holder has the
fatter pipe. Asserts:

- byte-exact completion, nobody retired;
- **`far` carries more bytes than `near`** — capacity decides, distance and
  latency do not. This is `meshlab reach`'s flatness finding restated as a
  *scheduling* claim: not only can a distant holder be reached, the scheduler
  must not tax it for its distance.

(Friendship distance needs no chaos twin: `meshlab reach` measured it flat, and
the scheduler never sees it — a fetch plan is a flat list of keys.)

### `TestChaosDeadHoldersDoNotGateTheSwarm`

**Question:** what does a dead holder cost, for each way of being dead?

One live holder plus three dead ones, one per class — the classes fail
differently and a test of one is not a test of the others:

| Class | Made by | Real-world shape |
|---|---|---|
| **ghost** | a well-formed key nothing on the mesh answers to | a stale advertisement; a node long gone |
| **cut** | its link `Partition`ed after convergence | switched off / crashed; route lost |
| **mute** | its link at 10-min latency after convergence | a hung box: routable once, silent now |

A live-only baseline fetch first (a different blob — the cache is keyed by
hash), then the mixed plan via `EnsureBlobFrom`. Asserts:

- the mixed fetch **succeeds** while one live holder exists;
- it lands within `baseline + (2 dispatches × Connect per dead holder) + slack`
  — the F9 item 3 bound `TestStaleHoldersCostAFetch` pinned for ghosts,
  extended to all three classes. Losing the dial deadline or the load rule
  blows this budget by construction;
- the live holder is never blamed for the company it kept (`corrupt=0`, not
  dropped).

### `TestChaosHolderSpreadSurvivesTheOrigin`

**Question:** does *information about holders* spread far enough that content
outlives its origin?

Chain of standing: `A` publishes the blob (library), `B` is A's friend, `C` is
B's friend and **a stranger to A**. `B` materializes the blob (real
`EnsureBlob` → download cache). `C` learns of B's copy passively — its own
sweep pulls B's **holdings** (the cache tracker) on the catalog cadence; the
test waits for B to appear as a provider in C's store, never seeds it by hand.
Then **A is stopped** — the origin is off, not merely unreachable. Asserts:

- `C.EnsureBlob` succeeds byte-exact with the origin down, served from B's
  cache (B's stats row carries the bytes);
- which is the decentralization headline in one sentence: **a blob that has
  been fetched once no longer depends on the node that introduced it.**

## Family 2 — the siege run

`federation/chaossiege_test.go`. Same fixtures plus one new one: `publishLie`
— a holder whose resolver maps the true hash to same-length wrong bytes, so it
serves a self-consistent lie (wrong chunks that verify against its own wrong
manifest). The scenarios differ only in how many liars and where they sit.

### `TestSiegeLiarMinorityIsRetired`

Two honest holders + one liar. The honest pair wins the manifest agreement
(two identical descriptions; the liar's differs), so the liar's chunks fail
per-chunk verify — wrong bytes are unambiguous evidence about their sender.
Asserts: byte-exact completion; the liar is retired (`dropped=true`,
`corrupt ≥ 1`); **no honest holder is blamed**; and the transfer never fell
off the swarm path — a minority liar is contained *in place*, at the cost of
its corrupt chunks and nothing else.

### `TestSiegeLiarQuorumCannotForgeTheBytes`

One honest holder + **two coordinated liars with identical wrong content** —
the farm shape, and the strongest position the protocol allows an attacker:
two identical votes capture the manifest agreement, the honest holder's true
chunks now look corrupt against the adopted lie, and the swarm can assemble a
complete, chunk-verified, **wrong** file. The anchor holds anyway: the
assembled-hash verify fails, the fetch falls back to the whole-file path,
liar attempts fail its sha256, the honest holder's attempt verifies. Asserts:

- the final bytes are the honest content, always — **a quorum of liars buys
  wasted transfer, never forged content**;
- the fetch *ends in success* (the fallback engages rather than the transfer
  dying);
- the price is logged (wire waste ≈ one liar assembly + failed whole-file
  attempts) — the doc's honest statement that this defense costs time.

The scenario deliberately tolerates either interior path (liar quorum adopted,
or honest manifest adopted and the two liars condemning it via
`errManifestSuspect`) — which path runs depends on probe timing, and **the
invariant is the same on both**: bytes exact, transfer completed.

### `TestSiegeOneBranchIsOneVoice`

The graph half, pure and ungated (`MemberKeys` / `branchesOf` — no mesh, no
clock): a 12-key farm, every edge mutually published, behind a single
friendship `F1`. Asserts, at farm scale, the three rules that make the sybil
answer structural rather than reactive:

- **admission**: the farm *is* admitted to membership (mutual edges are the
  rule and the farm followed it) — the defense is not exclusion;
- **attribution**: every farm key's branch set is exactly `{F1}` — one branch,
  one voice, whatever the farm's size, so no count the browse orders by can be
  farmed from one friendship;
- **eviction**: blocking `F1` removes the entire farm from membership in one
  action (branch snipping) — and the same farm spread across `F1..F3` yields
  exactly three branch roots: **k infiltrated friendships = k voices, no
  more** — the design's stated limit, pinned so nobody mistakes it for a hole.

(Small-graph versions of these rules are pinned in `branches_test.go` /
`gossip_test.go`; this test's subject is the *farm* — many keys, one root —
and the three rules asserted together, which is the attack's actual shape.)

### What the siege run deliberately does not test

- **Quota exhaustion by a farm** (N keys drawing the member budget) — the
  class-ceiling rule is unit-tested with `quota.go`; a chaos twin would assert
  arithmetic through a slower instrument.
- **Gossip forgery** (invented edges, tampered records) — signature and
  mutual-edge refusal are pinned in `gossip_test.go`; a farm that cannot forge
  edges is exactly why `TestSiegeOneBranchIsOneVoice` starts from edges the
  farm legitimately published.
- **Denial of service on the underlay** — not a swarm property; nothing above
  the transport can assert it honestly.

## The meshlab arm — `meshlab swarm`

The person-visible balance run, on real servers, shaped like `meshlab reach`:
one command that *answers*, over whatever link state the lab currently has —
the knobs (`link`, `partition`, `kill`) stay the person's.

```
meshlab up -topology triangle -seed ~/music -per-node 1
meshlab swarm                      # spread the subject, then a measured fetch
meshlab link c-b bandwidth 65536   # …now handicap one holder and run it again
meshlab swarm
```

What it does, server-side:

1. **Pick the subject**: the oldest published track of the first node (`a`)
   that the vantage (the *last* node) does not hold — vantage = the last node
   so the spread step has somewhere to go.
2. **Spread it**: every middle node materializes the blob
   (`POST /api/madnetwork/download`), so it lands in their caches; then the
   vantage's provider view is refreshed (pull-now against each holder — the
   production holdings cadence is 15 min and a lab should not wait it out).
   Reported as the **spread table**: who holds it, library or cache, and how
   long until the vantage *knew* — the holder-information-spread measurement.
3. **Fetch it measured**: clear the vantage's cache of the subject, snapshot
   `GET /api/admin/swarm/peers`, download, poll `/api/madnetwork/transfers`,
   snapshot again. The **balance table** is the per-counterparty delta: bytes
   per holder, share, elapsed, verified hash.

Printed verdict names the plurality carrier and the split, so a person can see
the balance move when they cap a link — the same fault grammar as everything
else in the lab. Re-runnable: the vantage's cache entry for the subject is
cleared at the start of every run (same rule as `check`).

Two instrument rules learned building it (both now in the suite README): the
balance must be read from the ledger's **session** counters — the row's
top-level bytes are the *stored* ledger on the flusher's 30-second cadence,
which cannot time a 200 ms fetch — and the after-snapshot must wait for the
transfer to **close** (`/api/admin/swarm/live`), because the reader can be done
while a chunk dispatched to a slow holder is still timing out (`fetchSwarm`
waits for every worker; the report's `settled` field is that straggler, shown
rather than hidden).

Measured on the triangle (one ~3 MiB track, loopback): flat links **53/47**;
one holder capped to 64 KiB/s → **100/0**; one holder partitioned → completes
and verifies from the survivor, with the first 64 KiB costing **5–20 s** — the
dead holder's manifest probe dying, a finding this run surfaced
(`.issues/open-issues.md` §"one dead holder in a two-holder plan", filed for
the owner beside the reader-rule decisions).

**What it does not do:** no assertions (those are the chaos family's job), no
link-shaping of its own (the lab's `link` command already owns that grammar),
no liars (see Placement).

## Status

- [x] this document
- [x] `startFaultedSwarm` + the balance run (4 scenarios) — built 2026-08-24, green under `MADSHARE_CHAOS=1`; the underlay-distance scenario's first cut capped the wrong proxy direction (far is the *dialer* on its own link — its blob bytes are that proxy's `Up`), caught because the measured rate contradicted the cap
- [x] `publishLie` + the siege run (3 scenarios) — built 2026-08-24, green; the minority scenario's first cut never dispatched to the liar (two fast honest holders absorbed a 4-chunk plan), fixed with 12 chunks + the liar leading the plan so the chunk-0 speculation meets it
- [x] `meshlab swarm` — built 2026-08-24, verified live on a triangle (see the measured numbers above)
- [x] a `-race` pass over the new scenarios (`MADSHARE_CHAOS=1 go test -race -p 1 -run 'TestChaosBalance|TestChaosDeadHolders|TestChaosHolderSpread|TestSiege' ./federation/`) — green 2026-08-24, with one honest concession it forced: under `-race` the netstack's CPU-bound loopback throughput (~35 KiB/s per holder, measured) sits **below every link cap** the balance scenarios set, so the caps stop binding and the byte split honestly collapses toward even — the first `-race` run failed on exactly that. The split-ratio assertions therefore run at `testTimeoutScale == 1` only (completion, integrity and no-retirement hold at every scale); caps low enough to bind under `-race` would starve chunks past `PerChunk` at scale 1 — the suite's cap/scale rule met from a third direction
- [x] the full pass (2026-08-24, logs under `results/`, gitignored; findings folded into `docs/plans/reader-rule.md` §6): whole federation package gated 248/248, gated `-race` 247/248 with 0 data races — the one failure a pre-existing pin (`TestChaosSeederVanishesMidTransfer` demanded a *failover* where a hedge win is now the designed rescue; widened to accept either, and its remaining `-race` capacity marginality recorded in `.issues/open-issues.md`) — and three live `meshlab swarm` runs: flat 51/49, one holder capped 100/0 **with the first 64 K at 1.46 s vs 59 ms flat** (a lead chunk on the capped holder — the reader pays where the transfer doesn't), one holder partitioned → completes verified from the survivor with the first byte at the predicted 20.0 s manifest-wave ceiling
