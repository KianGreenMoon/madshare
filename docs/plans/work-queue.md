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

### 3. Member quotas: runtime + visibility — BLOCKED on an owner decision  ← NEXT (ask first)

- The question row lives in `.issues/open-issues.md` §"knob-resolution dig
  findings": the four F7 quotas are the only limiter family still requiring
  edit-and-restart, and nothing reports what a running node enforces.
- The mechanism is pre-paved (`WithRateResolver` shape + the
  `optionalIntSetting` spelling + the placement rule in
  `docs/architecture/swarm-admin.md` §"Which layers a knob gets") — what is
  missing is the decision that they should be live at all. **Ask, don't
  build.** If yes, the readout/editor belongs on `/admin/swarm` (the lens
  rule: the page that shows the traffic is the page that throttles it).

### 4. Cache retention, the age half  ✅ DONE 2026-08-13

`madnetwork.cache_max_age_days`, shipped off, swept before the ceiling on the
same hourly pass and in the same save request. Written up in
`docs/architecture/madnetwork-cache.md` §"The retention ceiling" + its decision
log. One thing the plan did not anticipate: the knob got **two** layers, not
the ceiling's three — the placement rule in `swarm-admin.md` names no config
consumer for an age, and that section now records the answer.

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
  content/hash verification catches it).
- **Fetch-path drift pair** (`.issues` §"fetch-path dig findings"): local
  `os.Rename` failure triggers the network fallback (Low); 429 in whole-file
  mode skips the patience rule (Info). Fix shapes are written in the rows.
- **Recording-tagsets review findings**, 5 still open, re-verified in code
  2026-08-07, two of which destroy curated data silently — repro recipes in
  the memory/issue rows; these want an owner session of their own.

## Verification ritual (applies to every slot above)

Go lives at `~/.guix-home/profile/bin/go` (not on PATH). Full tree build
under both tags (`-tags nofederation` too), `go vet ./...`, test the tree
with the federation suite run separately, `-race` needs `-timeout ≥ 3300s`
and no parallel suites. A new migration breaks `database_test.go`; a new
`Repository` method breaks the api `fakeRepo`; a `PeerStore` change wants
`go vet -tags tests ./tests/mesh/...`. When a phase ships, grep the
user-facing docs — the design doc is the one being edited, so it is the one
that stays right.
