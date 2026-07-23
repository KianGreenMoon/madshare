# Implementation plan — Madnetwork test toolset (fault injection & multi-node lab)

Tooling to exercise federation under a **bad network** — latency, jitter, narrow
bandwidth, packet loss, partitions, flapping links, vanishing seeders — both from
`go test` and against real running servers in a browser. Working checklist;
delete once shipped and folded into `tests/mesh/README.md` (per
`docs/plans/roadmap.md` convention).

**Motivation.** `docs/plans/availability.md` §Phase 4 leaves one verification
open: *"reproduce/observe on a **real lossy/latent mesh**, not loopback, that (a)
transfers no longer stall and (b) availability doesn't flap."* Today that can only
be done by deploying to the real `.ygg` mesh and hoping the weather is bad. F5
(depth & tokens) adds strangers-inside-the-network to the seeder set, which makes
"what happens when a source is slow, lying, or gone" a first-class correctness
question rather than an operational one. We need the weather on tap.

**Core idea.** Every federation test already starts **real yggdrasil cores
in-process**, peered over a **loopback TCP underlay** (`startNodePair` in
`federation/transfer_test.go:24`, `startNodeTrio` in `federation/swarm_test.go:112`
— both reserve a `127.0.0.1:0` port and pass `tcp://…` as `Listen`/`Peers`). That
underlay socket is the injection seam: anything inserted between the two ends is a
fault injector, in userspace, rootless, in-process, and equally usable by real
server processes.

## What already exists (don't rebuild)

- **In-process multi-node meshes.** `startNodePair` / `startNodeTrio` start 2–3
  genuine `core.Core` + gVisor netstack nodes over loopback. Friending
  (`makeFriends`), catalog seeding (`seedBlobCatalog`), blob publishing
  (`publishBlob`) and fetch-and-verify (`fetchAndVerify`) helpers are all there.
  The lab does not need a new way to build a mesh — only a way to make it hostile.
- **Timeout scaling under `-race`.** `federation/meshtimeouts_test.go` +
  `racescale_{on,off}_test.go` define `testTimeoutScale` (8× under `-race`, the
  userspace netstack being several times slower). Every chaos budget must be
  expressed in these units, not in raw milliseconds.
- **A token-bucket rate limiter.** `federation/swarm.go` `rateLimiter`
  (`seed_rate_kib`) is the shape to copy for netfault's bandwidth knob — same
  math, different side of the wire.
- **Seeding + auth recipes.** `tests/k6/lib/auth.js` (login → mint bearer token)
  and `tests/k6/prepare-data.sh` (`TEST_AUDIO_DIR`, discover-don't-seed) already
  encode how to get a library into a running server. meshlab ports the recipe to
  Go rather than inventing a second convention.
- **QUIC underlay support.** yggdrasil 0.5.14 ships `src/core/link_quic.go`, so
  `quic://` peerings are available — which is what makes genuine packet-loss
  emulation possible (see §Known gotchas).

## Layout

```
tests/mesh/
  netfault/           # library: fault model + TCP relay + UDP relay + control API
  cmd/netfaultd/      # standalone proxy binary (HTTP control plane)   [tags tests]
  cmd/meshlab/        # multi-node harness binary                      [tags tests]
  README.md           # how to run it — stub lands in T0, grows per phase
federation/
  chaos_test.go       # faulted scenarios                              [env-gated]
  chaoshelp_test.go   # faulted variants of startNodePair/startNodeTrio
```

All test tooling stays under `tests/` alongside `tests/{js,k6,playwright}`.

## Gating: what is excluded, and how

Two different things need excluding, and they want two different mechanisms.
Neither is about keeping the toolset out of the shipped server — Go already
guarantees that (see below).

**The two `cmd/` binaries carry `//go:build tests`.** They are the only artifacts
that could plausibly materialize somewhere unwanted, and one concrete hole
justifies the tag: `go install ./...` — unlike `go build ./...` — *does* write
every `main` package into `GOBIN`. A packager or CI recipe using it would drop
`netfaultd` next to `madshare` in `/usr/bin`. Verified against Go 1.26.1:

```
go install ./...   (untagged)  → GOBIN: madshare  meshlab  netfaultd
go install ./...   (tagged)    → GOBIN: madshare
```

`make install` never did this, but we don't control everyone's packaging, and
`netfaultd` is precisely the binary that should not appear by accident (§Security).

**The netfault library and the chaos suite stay untagged, gated at run time.**
Each chaos test skips unless an env var is set:

```go
if os.Getenv("MADSHARE_CHAOS") == "" {
	t.Skip("chaos scenario; set MADSHARE_CHAOS=1 to run")
}
```

They therefore **compile and vet on every default `go test ./...`** while never
running. That matters: this code leans on `federation/` internals (`Start`, the
`Option` set, `startNodePair`, plus T1's new `WithIntervals` and `TransferStats`)
that keep moving through F5–F7. Tag-excluded code is invisible to the compiler,
to `go vet`, and to gopls unless the editor is configured with
`buildFlags: ["-tags","tests"]` — so it rots silently, and the suite whose job is
catching regressions becomes the thing that regresses. Run-time gating buys the
same isolation with the compiler still standing guard.

```bash
go build -tags tests -o tests/mesh/bin/ ./tests/mesh/cmd/...   # the tools
MADSHARE_CHAOS=1 go test -p 1 ./federation/... ./tests/mesh/... # the scenarios
```

**What needs no mechanism at all.** Release builds already exclude every part of
this, unconditionally: `_test.go` files never enter a binary; `make build` builds
only `./`; `netfault` is imported solely by the chaos tests and the two `cmd/`
binaries, and a package nobody imports is never linked in. `go build ./...` also
writes no executables for a multi-package build. Weight is a non-issue too —
netfault is stdlib-only, so zero new `go.mod` entries and zero bytes added to the
`madshare` binary. The tag is a packaging safeguard, not a build-hygiene one; do
not extend it on the theory that it protects the release artifact.

`netfault` has no federation dependency, so it is orthogonal to `nofederation` /
`nowebui` and needs no interaction with them.

## Phase T0 — netfault core (TCP)

**Built (this commit)** — the library half. `tests/mesh/netfault` implements the
`Fault`/`Dir` model, the relay, `Set` (applying to live connections), `Script`,
`Stats`, and loopback-only defaults with an `Options.AllowRemote` escape hatch.
11 tests: 6 deterministic ones run by default in ~20 ms, 5 timing-sensitive ones
behind `MADSHARE_CHAOS`. Race-clean. `tests/mesh/README.md` stub written, its
Quick start commands and Go snippet both executed/compiled as written.

Two things worth recording:

- **Latency is a delay queue, not a sleep before each write.** Sleeping inline
  couples delay to throughput — a 200 ms link would deliver one 32 KiB buffer per
  200 ms (≈160 KiB/s) — which would have made every latency scenario measure a bug
  in the injector rather than anything federation did. `TestLatencyDoesNotThrottle`
  pins it: 4 MiB across a 200 ms link completes in 0.42 s, where the naive
  implementation needs ~25 s. Delivery times are forced monotonic, since jitter
  must vary delay without reordering a byte stream.
- **The token bucket's burst is 100 ms of traffic, not 1 s** as in `swarm.go`.
  That one is a fairness cap where bursting is harmless; this one emulates a
  *link*, which does not save up a second of unused capacity to spend at once —
  and a wide burst would make short transfers appear unthrottled, exactly the
  regime the swarm tests care about.

**Not yet built:** `cmd/netfaultd` and its HTTP control API. Nothing consumes it
until meshlab (T4) — in-process scenarios call the library directly — so it lands
with its only consumer, and the `-tags tests` gating arrives with it.

A dependency-free relay: listen on `127.0.0.1:0`, dial the real endpoint, pump
bytes both ways through a fault pipeline. Per-direction configuration so
asymmetry (fast down, crawling up) is expressible.

Knobs, v1:

| Knob | Semantics |
|---|---|
| `Latency`, `Jitter` | Per-direction delay queue; jitter drawn per write, clamped ≥ 0 |
| `Bandwidth` | Token bucket, bytes/s (mirror `swarm.go`'s `rateLimiter`) |
| `Slice` | Chop writes into ≤ N-byte pieces, delay between — models a jammed path |
| `Partition` | Refuse new dials **and** kill live conns; the heal is a knob flip |
| `KillAfter` | Drop a connection after N bytes or T elapsed — mid-transfer source loss |
| `Script` | Timeline: `{at: 10s, set: {Partition: true}}, {at: 30s, set: {Partition: false}}` |

Two faces:

- `netfault.New(target string, f Fault) (*Proxy, error)` — `p.Addr()` is what the
  node peers to; `p.Set(f)` mutates live; `p.Stats()` reports bytes/conns/kills.
- `cmd/netfaultd` — the same, with a small JSON control API
  (`GET/PUT /links/{name}`) so meshlab (and a human with `curl`) can change
  conditions while a browser session is open.

### Security posture (design, not gating)

`netfaultd` is, structurally, an open relay with a control API that can retarget
it — the shape of program that must never face a network. A build tag does
nothing about this: whoever builds with `-tags tests` and runs it on `0.0.0.0` is
exposed regardless. The control lives in the tool's own design, and is a v1
requirement, not a later hardening pass:

- **Relay and control API bind loopback-only by default.** A non-loopback bind
  requires an explicit flag that logs a warning naming the risk.
- **Targets must be loopback** unless the same explicit override is given —
  otherwise the relay is an SSRF pivot into whatever the host can reach.
- **No persistent config, no daemonization, no init script.** It is a foreground
  process you start and kill; nothing about it should look installable.

meshlab inherits the same rule and adds one: it provisions servers with **known
bootstrap admin credentials**, so its generated `madshare.toml` files and data
dirs live under a gitignored scratch dir and its nodes bind loopback only.

**The injector must itself be tested** — a fault proxy that lies is worse than
none. `netfault/netfault_test.go` (untagged, so it compiles with every default
`go test ./...`; the timing-sensitive cases carry the `MADSHARE_CHAOS` skip):
measured latency ≈ configured (within a tolerance band), throughput under a
bandwidth cap ≈ configured ±10 %, partition actually refuses, `KillAfter` cuts at
roughly the right offset, and a no-fault proxy is byte-transparent over a few MiB.

## Phase T1 — Test seams in `federation/`

Without these, most interesting behavior cannot be asserted in sane wall-clock.
All three are additive; none changes default behavior.

- **Injectable intervals.** `refreshLoop`'s ticker (`federation/friendship.go:285`,
  1 min), `catalogSyncInterval` (15 min) and `snapshotTTL` (1 min) in
  `federation/catalog.go` are hard consts. Add `WithIntervals(...)` (an `Option`,
  matching the existing `WithCacheDir`/`WithBlobResolver` style) so a chaos test
  runs the sweep at ~200 ms. Also needed by meshlab: a lab whose catalogs
  converge on a 15-minute cadence is unusable for demos.
- **Timeout scaling.** `chunkStallTimeout` (20 s), `manifestTimeout` (20 s),
  `perChunkTimeout` (2 min) and the 30-min transfer ceiling
  (`federation/transfer.go:436`) become fields with the current values as
  defaults, so a stall scenario finishes in seconds instead of minutes.
- **Per-transfer stats.** `type transfer` (`federation/transfer.go:81`) tracks
  progress but nothing diagnostic. Add a `TransferStats` snapshot: time-to-first-
  byte, bytes per provider, chunk retries, failover count, stall events, provider
  errors. Today a test can only assert *"it eventually finished"* — not *that
  failover happened*, *that seek-priority beat sequential*, or *that the
  `chunk0Prefetch` overlap paid off*, which are precisely F4's load-bearing
  claims. Dual-purpose: this is the data an admin transfer view wants anyway.

## Phase T2 — Scenario suite (TCP faults)

`federation/chaos_test.go` — compiled always, run only under `MADSHARE_CHAOS=1`
(§Gating), so `go test ./...` stays fast and `make test` is unaffected while the
compiler still checks every scenario against current `federation/` internals.
Helpers wrap the existing pair/trio builders, threading each peering through a
netfault proxy.

Scenarios, each an assertion about a claim the code already makes:

1. **Slow seeder + fast seeder** — no head-of-line blocking; the fast holder must
   carry the majority of chunks (assert via per-provider bytes).
2. **Seeder vanishes mid-transfer** (`KillAfter`) — failover completes the
   transfer; `TransferStats.Failovers > 0`.
3. **All seeders vanish** — clean error inside the (shrunk) budget, no hang, the
   `.part` file never promoted, no partial blob in the cache.
4. **300 ms RTT, 4 MiB blob** — streaming TTFB stays inside budget: the lead-ramp
   + `chunk0Prefetch` claim, which is exactly what latency was supposed to fix.
5. **Tail seek under latency** — seek-priority fetches the seeked chunk before the
   prefix (`transfer.Available` on the tail chunk goes true first).
6. **Partition then heal** — friendship survives, `last_seen` recovers, catalog
   resyncs after reconvergence.
7. **Friend down past the window** — its exclusively-held tracks disappear from
   the next browse; own and cached rows never do.
8. **Local inbound path dead** — fail-open: cutoff 0, `inbound_healthy:false`,
   full catalog retained. Currently only unit-tested with an injected read error
   (`availability_test.go`); this exercises it through a real dead link.
9. **Flapping link at the window boundary** — the anti-flap guarantee: a link
   cycling near `reachable_window_sec` must not flip availability per sweep.
10. **Rate-limited seeder** (`seed_rate_kib`) — a throttled holder doesn't starve
    the swarm; other holders take up the slack.

Budgets are `testTimeoutScale`-relative and tolerance-shaped ("completes within
N×", "no stall > X"), never exact timings — the mesh is stochastic and `-race`
runs it ~8× slower.

## Phase T3 — netfault UDP + QUIC underlay

A datagram relay for `quic://` peerings, adding what TCP structurally cannot
express: **loss**, **reorder**, **duplication**, per-datagram jitter. Same control
surface, same `Script` timeline. Extends the suite with: lossy path still
completes a transfer; reordered/duplicated datagrams don't corrupt a chunk (the
per-chunk sha256 is the anchor); sustained 5 % loss doesn't flap availability.

Sequenced after T0 so the TCP suite lands first, but independent of T1/T2.

## Phase T4 — meshlab (multi-node lab, real servers)

`tests/mesh/cmd/meshlab` — spins N real madshare processes on one machine:

- Per node: own `data_dir`, DB, `federation.key`, HTTP port, underlay port,
  generated `madshare.toml`.
- **Topology presets** — `pair`, `triangle`, `hub` (backbone + spokes), `chain`
  (multi-hop; the only shape that exercises yggdrasil routing rather than a single
  link). Every peering runs through a netfaultd link, addressable by name (`a-b`).
- **Bootstrap** — first-run admin, then the forced password change, then mint a
  bearer token (`POST /api/auth/tokens` is refused until the change is done,
  `api/auth_handlers.go:217`), then auto-friend via `POST /api/admin/federation/peers`
  + `/accept` (`api/api.go:450`).
- **Seeding** — distinct libraries per node from `TEST_AUDIO_DIR`, upload →
  submit → approve, waiting for the analysis pipeline before asserting catalogs.
- **Chaos commands** — `meshlab link a-b latency 200ms jitter 80ms`,
  `meshlab kill c`, `meshlab restart c`, `meshlab partition a`,
  `meshlab flap b --period 30s`, `meshlab status`.

This is the arm that makes availability, fail-open and Materialize observable in
the actual UI under an actual bad network — the open item from the availability
plan.

## Phase T5 — Docs, Makefile, closing the availability item

**Finalize** `tests/mesh/README.md` — the stub has been growing since T0
(§Deliverable), so T5 is a completeness pass over it, not a writing session: fill
the sections later phases added (QUIC transport, meshlab topologies), verify every
command in Quick start actually runs as written, and delete this plan doc.

Plus Makefile targets. Note the split: the binaries need `-tags tests` (an
explicit path without it is a hard error), the scenarios need the env var:

```make
test-mesh:                          # chaos suite + the injector's own tests
	MADSHARE_CHAOS=1 go test -p 1 ./federation/... ./tests/mesh/...

mesh-tools:                         # the two lab binaries
	go build -tags tests -o tests/mesh/bin/ ./tests/mesh/cmd/...
```

Then a walkthrough of the availability live-verification checklist run against
meshlab instead of the real mesh.

## Deliverable: `tests/mesh/README.md`

**Written incrementally, not at the end.** A stub lands with T0 and each phase
extends it; a tool nobody can run is not done, and this suite has more moving
parts than either existing one (two gating mechanisms, two underlay transports, a
process supervisor). It is also the doc this plan collapses into when shipped —
`docs/plans/mesh-testing.md` gets deleted, per the roadmap convention.

Follow the house pattern set by `tests/k6/README.md` and
`tests/playwright/README.md`, same section order:

- **Scope + safety callout.** What the suite tests, and the ⚠️ that `netfaultd` is
  an open relay and `meshlab` provisions servers with known admin credentials —
  loopback-only, disposable environments, never a shared host. (k6's README opens
  with exactly this shape of warning.)
- **Prerequisites.** Go on `PATH` (note: `~/.guix-home/profile/bin/go`),
  `ffprobe`/`fpcalc` for meshlab's seeding to produce a full quality ladder,
  `TEST_AUDIO_DIR` for library fixtures.
- **The model.** The one paragraph that makes the whole thing legible: federation
  tests already peer real yggdrasil cores over a loopback TCP underlay, and
  netfault sits in that seam. Plus the transport split — `tcp://` for
  latency/bandwidth/partition, `quic://` for genuine loss/reorder, and *why* TCP
  cannot express loss.
- **Quick start**, copy-pasteable, the k6 README's most useful section:
  run one scenario, run the whole suite, run under `-race`, bring up a 3-node lab,
  apply a fault to a live link, tear down.
- **Gating.** Both mechanisms and when each applies — `-tags tests` for the
  binaries, `MADSHARE_CHAOS=1` for the scenarios — with the one-line reason each
  exists, so nobody "simplifies" them back together.
- **Configuration.** netfault knob reference (the T0 table), meshlab topologies
  and env vars.
- **Layout**, **Troubleshooting**, **Separation from k6 / Playwright** — this
  suite is correctness-under-adverse-network, not load (k6) and not browser
  behavior (Playwright), though meshlab is a legitimate *target* for both.

Troubleshooting must carry the failures we already know are coming: mesh
convergence is not instant after a heal (poll, don't assert); `-race` needs
`-timeout ≥ 3300s` and `-p 1`; a restarted node that lost its `federation.key` is
a new identity; budgets are `testTimeoutScale`-relative; and a scenario that is
flaky under load is usually a budget expressed in wall-clock rather than scale
units.

## Dependencies & sequencing

```
T0 (netfault TCP) ─┬─> T1 (seams) ─> T2 (scenarios) ─> T5 (docs)
                   ├─> T3 (UDP/QUIC)          ─────────┘
                   └─> T4 (meshlab)  ─────────────────┘
```

T0 is the only hard prerequisite. T4 is the highest-value arm for the owner's own
eyes and depends on nothing but T0 (plus T1's shrunk intervals to be pleasant);
T2 is the highest-value arm for regressions.

## Known gotchas

- **Don't let the `tests` tag creep from the `cmd/` binaries onto the library or
  the suite.** If a package's sources are tag-excluded but a `_test.go` in it is
  not, Go still compiles the test package and every reference fails as
  *undefined* — a clean `go test ./...` turns into `FAIL … [build failed]`
  (verified). That trap is one symptom; the deeper reason for the split in
  §Gating is that tagged code stops being compiled, vetted and type-checked at
  all, and this suite tracks `federation/` internals too closely to survive that.
- **A new chaos test must carry the `MADSHARE_CHAOS` skip.** Forgetting it is the
  mirror-image failure: the scenario runs on every default `go test ./...` and
  makes it minutes-slow. Worth a one-line helper (`requireChaos(t)`) so the check
  is impossible to spell wrong.
- **TCP loss ≠ packet loss.** The kernel retransmits, so "loss" on a `tcp://`
  underlay degenerates into stalls and resets. Genuine loss/reorder needs the
  `quic://` underlay (T3). Don't ship a `Loss` knob on the TCP relay — it would
  be a lie; expose `Slice`/`KillAfter` instead.
- **netfault emulates a link, not the internet.** Multi-hop routing pathologies
  inside yggdrasil only appear with ≥ 3 nodes and a real topology — hence the
  `chain` preset. A faulted pair tests our timeouts; a faulted chain tests ygg's.
- **Restarting a "killed" node must reuse its key file.** Node identity derives
  from `federation.key`; a fresh key is a new identity and every friendship
  breaks. That is a *scenario* (identity loss) but a trap everywhere else.
- **`TouchFederationPeerSeen` is monotonic** (`database/federation.go`) — a peer
  cannot be made stale by rewinding `last_seen`. Staleness must be produced by
  actually stopping traffic and waiting out the (shrunk) window.
- **Convergence after heal is not instant.** yggdrasil applies its own link retry
  backoff; scenarios must wait for reconvergence (poll, like `waitFor`), never
  assert immediately after a heal.
- **Seeding a library under auth is a time sink.** Uploads land as drafts;
  catalogs only list renditions after submit/approve *and* after the analysis
  pipeline has run (`ffprobe`/`fpcalc` must be on PATH or the ladder degrades).
  Budget for this in T4 — it is more work than the process supervision.
- **`-race` needs `-timeout ≥ 3300s` and no parallel suites** (per the GC-model
  notes; SQLite `busy_timeout` flakes). Chaos tests run serial: `-p 1`.
- **Port allocation is racy.** The reserve-then-close `127.0.0.1:0` idiom the
  existing helpers use has an inherent window; serial chaos runs keep it narrow.
- **Go is at `~/.guix-home/profile/bin/go`**, not on the default PATH.

## Verification

- `netfault`'s own tests (T0) — the injector is honest.
- **Default runs are unchanged in behavior**: `go test ./...` still finishes in its
  usual time (every scenario skipped), while still *compiling* the suite — the
  point of the split. `go build ./...` (+ `-tags nofederation`, `-tags nowebui`)
  and `go vet ./...` behave as before; `make build` unaffected.
- **`go install ./...` yields only `madshare`** — the packaging hole the tag exists
  to close; assert it stays closed whenever a `cmd/` is added.
- `MADSHARE_CHAOS=1 go test -p 1 ./federation/... ./tests/mesh/...` green, and
  green under `-race` with the scaled budgets.
- `netfaultd` refuses a non-loopback bind or target without the explicit override,
  and says why.
- **Every command in the README's Quick start is executed as written** — the
  cheapest way this suite rots is a copy-pasteable block that no longer pastes.
- meshlab: a 3-node `hub` lab, browser-verified — kill a friend → its exclusive
  tracks gone on the next reload/search (not live); restart → back after a reload;
  own/cached always present; kill the local inbound path → banner + full catalog
  (fail open), not a blank page. This closes the open verification in
  `docs/plans/availability.md`.
