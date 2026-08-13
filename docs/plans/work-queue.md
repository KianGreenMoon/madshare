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
(`aedc662`) came out of the follow-on consultation. This queue is what
remains.

## The sequence

### 1. Availability Phase 5 — reactive down-mark + ping floor  ← START HERE

The freshest design, fully decided, nothing blocking. Do it first while the
decisions are hot.

- **Design:** `docs/architecture/federation.md` §Availability, "Reactive
  down-mark + the ping floor". **Build order:** `docs/plans/availability.md`
  Phase 5 (six numbered steps, starting with migration **048** —
  `federation_nodes.unreachable_at`).
- Shape: one migration, one predicate change (SQL + its Go twin **together**),
  `Node.observeUnreachable` with the relative guard, three hook sites, the
  floor ping in the sweep, and the 429-proof-of-life verification riding
  along.
- Verification: migration 048 breaks the `database_test.go` version/table
  assertions (expected, fix the assertions); PeerStore grows → `go vet -tags
  tests ./tests/mesh/...`; federation suite run **alone** (flakes when
  parallel to other suites); optional live two-node check that a killed
  member's tracks vanish on the next browse after a failed fetch.

### 2. Node struct fold — the recorded follow-up of migration 046

- **Design:** `docs/architecture/federation-nodes.md` §"The Go surface":
  fold the `Peer` / `CatalogSource` (/ `HomeNode`) view-structs into one
  `Node` struct. Pure refactor, no migration, no behavior change.
- Sequenced **after** Phase 5 on purpose: Phase 5 is small and lands on
  today's shapes; the fold rewrites `PeerStore` signatures and breaks the api
  `fakeRepo` + the mesh-lab stores (`emptyStore`/`memStore`), so it wants a
  session of its own.

### 3. Member quotas: runtime + visibility — BLOCKED on an owner decision

- The question row lives in `.issues/open-issues.md` §"knob-resolution dig
  findings": the four F7 quotas are the only limiter family still requiring
  edit-and-restart, and nothing reports what a running node enforces.
- The mechanism is pre-paved (`WithRateResolver` shape + the
  `optionalIntSetting` spelling + the placement rule in
  `docs/architecture/swarm-admin.md` §"Which layers a knob gets") — what is
  missing is the decision that they should be live at all. **Ask, don't
  build.** If yes, the readout/editor belongs on `/admin/swarm` (the lens
  rule: the page that shows the traffic is the page that throttles it).

### 4. Cache retention, the age half

- **Design:** `docs/architecture/madnetwork-cache.md` §"The retention
  ceiling": `madnetwork.cache_max_age_days` — evict entries whose
  `last_used_at` is older. The size half shipped 2026-08-08 with the
  three-layer arrangement; the age half reuses the same sweep and the same
  settings-card shape. Small, self-contained, no open decisions.

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

1. **gofmt drift, 5 files** (pre-existing since ~`ee1578e`, flagged
   2026-08-13, still present): `api/handlers_test.go`,
   `api/login_throttle.go`, `api/madnetwork_handlers_test.go`,
   `api/playlists.go`, `api/storage/diskusage_unix.go`. One trivial
   standalone `gofmt -w` commit — do it at the start of the next session so
   later diffs stay clean.
2. **Migration-number claims:** the availability design claims **048**
   (federation.md + availability.md Phase 5 + the memory note). If anything
   else lands a migration first, renumber the claims in the same commit.
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
