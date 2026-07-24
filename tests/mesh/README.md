# Madnetwork mesh test suite — fault injection for federation

Correctness tests for madshare's federation layer under a **bad network**:
latency, jitter, narrow bandwidth, partitions, flapping links, and seeders that
vanish mid-transfer. Everything runs in userspace on one machine — no root, no
`tc`/netem, no containers, no external services.

The question this suite answers is *"does the swarm still behave when the network
misbehaves"*, and — because a transfer that merely finishes proves very little —
*"did it behave for the reason we think it does"*.

> ⚠️ **`netfault` is a relay.** It listens on one socket and forwards to another.
> It binds loopback and refuses a non-loopback bind *or* target unless you pass
> `Options{AllowRemote: true}`, which turns it into an open relay reachable from
> the network and pointing at whatever the host can reach. Do not set that flag
> to work around a problem. The chaos tests never set it.

## What's here

```
tests/mesh/
  netfault/
    netfault.go        the fault model + TCP relay (a library; stdlib only)
    netfault_test.go   the injector's own tests — a fault proxy that lies is
                       worse than none
  README.md            this file
federation/
  chaos_test.go        the scenarios
  chaoshelp_test.go    faulted topologies, the shrunk clock, requireChaos
  seams_test.go        the injectable intervals/timeouts + TransferStats
```

The scenarios live in `federation/`, not here, because they are internal tests
(`package federation`) that reach into `chunkPlan`, `chunkLayout` and friends.
That is also a hard constraint, not just convenience: `database` imports
`federation`, so a test inside this package **cannot** import the browse layer
without an import cycle. See §The scenarios for what that costs.

Not built yet: `cmd/netfaultd` (standalone relay + control API), `cmd/meshlab`
(a lab of real madshare processes), and the UDP/QUIC relay that would allow
genuine packet loss. `docs/plans/mesh-testing.md` has the phase plan.

## Prerequisites

Go, and nothing else. On the maintainer's machine the toolchain is
`~/.guix-home/profile/bin/go`, which is **not** on the default `PATH`:

```bash
export PATH="$HOME/.guix-home/profile/bin:$PATH"
```

No audio fixtures, no database, no running server — the scenarios generate their
own blobs and run the mesh in-process.

## Quick start

```bash
# Nothing extra: the injector's deterministic tests. Instant.
go test ./tests/mesh/...

# Add netfault's timing-sensitive cases (measured latency, throughput, kills). ~5 s.
MADSHARE_CHAOS=1 go test -count=1 ./tests/mesh/...

# The scenarios: real meshes under real faults. ~2.5 min, serial, verbose.
MADSHARE_CHAOS=1 go test -p 1 -run Chaos -v ./federation/...

# One scenario, with its stats readout. ~20 s.
MADSHARE_CHAOS=1 go test -run TestChaosSeederVanishesMidTransfer -v ./federation/...

# The federation test seams the scenarios are built on. ~4 s.
go test -run 'Intervals|TransferStats' ./federation/...

# Everything, under the race detector. Slow — see Troubleshooting for the timeout.
MADSHARE_CHAOS=1 go test -race -p 1 -timeout 7200s -count=1 ./federation/... ./tests/mesh/...
```

**Without `MADSHARE_CHAOS` every scenario skips**, so a plain `go test ./...`
is unaffected (~60 s, same as before the suite existed) — while still *compiling*
every line of it. That split is deliberate; see §Gating.

## The model

Madshare's federation tests already build **real meshes**: `startNodePair` and
`startNodeTrio` (`federation/transfer_test.go`, `federation/swarm_test.go`) start
genuine yggdrasil cores with a gVisor netstack and peer them over a **loopback
TCP underlay** — a plain `127.0.0.1:0` socket handed over as a `tcp://` peering
URI.

That socket is the seam. `netfault.Proxy` listens on loopback, dials the real
endpoint, and pumps bytes through a per-direction fault pipeline; peering a node
at the proxy's address instead of the real one makes the link hostile without
touching yggdrasil, the OS, or the test's structure.

**Orientation.** The chaos suite builds its own topologies rather than wrapping
those two, because `startNodeTrio` peers both other nodes to a single hub — so
its two seeders share one underlay link and cannot be degraded independently,
which is the entire point. The faulted shape inverts it:

> **The fetcher dials. Every seeder listens.** Each peering has its own proxy.

That fixes the direction convention for good, and every fault builder depends on
it:

| Proxy direction | Carries |
|---|---|
| `Up` | fetcher → seeder (requests, range headers) |
| `Down` | seeder → fetcher (**blob bytes**) |

So `slowDown()` degrades `Down`. Getting this backwards produces a scenario that
degrades nothing and passes anyway.

**Transport split.** TCP is a reliable byte stream, so this relay cannot emulate
packet loss: dropping bytes would corrupt the connection rather than model a
lossy path, and real loss surfaces as stalls and resets because the kernel
retransmits. So:

| Underlay | Use it for |
|---|---|
| `tcp://` | latency, jitter, bandwidth, slicing, partitions, mid-stream kills |
| `quic://` | genuine packet loss, reordering, duplication *(not built)* |

## The scenarios

`federation/chaos_test.go`. Each asserts a claim the code makes, not merely that
a transfer eventually finished — `TransferStats` is what makes the difference
assertable.

| Scenario | Fault | Claim under test |
|---|---|---|
| `SlowAndFastSeeder` | one holder at 4 KiB/s | a crawling source cannot gate the transfer; the fast holder carries the bulk |
| `SeederVanishesMidTransfer` | partition one link mid-fetch | the survivor finishes it, and `Failovers > 0` proves chunks were re-routed |
| `AllSeedersVanish` | partition the only link mid-fetch | fails cleanly and promptly — no hang, nothing promoted into the cache, no orphaned `.part` |
| `LatencyTimeToFirstByte` | 300 ms RTT + 512 KiB/s | the lead ramp + chunk-0 prefetch put the first byte early in the transfer, not near its end |
| `TailSeekBeatsPrefix` | 512 KiB/s | a seek to EOF fetches the tail chunk out of order, while the contiguous watermark is still far behind |
| `RateLimitedSeeder` | `seed_rate_kib` on one holder | the serving-side cap throttles that holder, not the swarm |
| `PartitionThenHeal` | cut, then heal | friendship survives; `last_seen` freezes and falls behind the cutoff, then recovers on its own; a catalog changed during the outage syncs |
| `FlappingLinkStaysFresh` | 2 s down / 6 s up, ×3 | the anti-flap guarantee: repeated outages never push a friend past the freshness window |

### Two things this suite deliberately does not test

Both for the same structural reason — **netfault faults the underlay, and some
behavior lives above it.** Neither is a gap; knowing *why* saves the next person
from trying.

- **"Friend down past the window → its tracks disappear from the browse."** The
  hiding is a SQL predicate (`last_seen >= now-window`,
  `database.MadnetworkView.Cutoff`), unit-tested in `database/madnetwork_test.go`
  — and unreachable from here anyway (import cycle). What only a real mesh can
  show is the *input*, so `PartitionThenHeal` asserts that an unreachable friend
  genuinely falls behind the cutoff.
- **"Local inbound path dead → fail open."** A cut peering **cannot** produce
  this, and that is the design working. `InboundReaderAlive` watches the
  goroutine reading from the yggdrasil core into the netstack — *above* the
  underlay — so a partition leaves it blocked but alive. `PartitionThenHeal`
  asserts the inverse instead, which is the property that actually matters: a
  *remote* outage must never be mistaken for a *local* fault, or every partition
  would stop hiding anything. The dead-reader path itself is unit-tested with an
  injected read error (`federation/availability_test.go`, and `runInboundReader`
  in the yggstack fork).

## Reading a failure

Scenario runs are verbose on purpose: each logs a `TransferStats` readout, which
is usually enough to diagnose a failure without re-running it.

```
seeder vanished: mode=swarm ttfb=506ms elapsed=9.19s chunks=9/9 retries=2 failovers=1 stalls=1 corrupt=0
  node-a   bytes=1835008   chunks=4   failures=2 dropped=false  Get "http://[…]:1314/…": timeout awaiting response headers
  node-c   bytes=2359296   chunks=5   failures=0 dropped=false
```

| Field | Reads as |
|---|---|
| `mode` | `swarm` (F4 multi-source), `whole` (F3 single-source fallback), `local` (cache/library hit); `a→b` names a path that fell back |
| `ttfb` | time until the front of the file became readable — the streaming claim |
| `chunks=9/9` | verified / total in the manifest layout |
| `retries` | failed attempts that were re-queued |
| `failovers` | pieces finished by a holder *after a different one failed them* |
| `stalls` | idle-read watchdog firings — a holder that connected then went silent |
| `corrupt` | per-chunk hash mismatches (a holder serving wrong bytes) |
| `[abandoned …]` | an attempt the fetch gave up on, and how far it got before switching |
| per-holder | bytes and chunks carried, consecutive failures, whether it was dropped from rotation, and its last error |

A fetch that changed strategy mid-flight reports both halves — the top line is
the attempt that was live at the end, the indented ones are what came before:

```
all seeders vanished: mode=swarm→whole ttfb=0s elapsed=20.5s chunks=0/0 retries=6 failovers=0 stalls=0 corrupt=0
  [abandoned swarm] ttfb=520ms chunks=1/9
  node-a   bytes=262144   chunks=1   failures=6 dropped=true  Get "http://[…]:1314/…": context deadline exceeded
```

Three readings worth knowing:

- **A bare `mode=whole` in a swarm scenario** means the manifest probe failed and
  the fetch went straight to F3 — usually the link was already cut before the
  transfer got going. **`mode=swarm→whole`** is a different story: the swarm ran
  and gave up, and the `[abandoned swarm]` line says how far it got.
- **A holder with `dropped=true` and `bytes=0`** was never usable. That is the
  expected shape for the slow-seeder scenarios — see the note on
  drop-not-deprioritize in Troubleshooting.
- **Nobody `dropped=true` on a failed transfer** is normal now, not a puzzle: a
  chunk that exhausts its attempt budget aborts the fetch with every holder still
  live. Retiring holders and ending transfers are separate mechanisms
  (`docs/architecture/federation.md` §Distribution).

## Writing a new scenario

Copy the shape below — it compiles as written. The chaos-specific helpers live in
`federation/chaoshelp_test.go`; the library fixtures it leans on (`fillBytes`,
`publishBlob`, `newMemStore`, `makeFriends`, `seedBlobCatalog`, `waitFor`) are the
pre-existing ones in `swarm_test.go` and `friendship_test.go`, and a few
scenario-local ones (`assertCached`, `warmMesh`, `flapSteps`) sit in
`chaos_test.go` next to their users.

```go
func TestChaosMyScenario(t *testing.T) {
	requireChaos(t)                       // MUST be first — see Gating
	content := fillBytes(2 << 20)         // deterministic, chunk-distinct bytes
	storeA, storeB := newMemStore(), newMemStore()
	hash, resolveA := publishBlob(t, storeA, content) // A serves it from a temp library
	cacheB := t.TempDir()

	a, b, link := startFaultedPair(t, storeA, storeB,
		chaosOpts(resolveA),                    // seeder
		chaosOpts(WithCacheDir(cacheB)))        // fetcher
	friendsHolding(t, a, b, storeA, storeB, content) // friend them + seed B's catalog

	link.Set(slowDown(512 << 10))         // degrade AFTER the mesh converged

	tr, err := b.EnsureBlob(context.Background(), hash)
	if err != nil {
		t.Fatalf("EnsureBlob: %v", err)
	}
	if err := awaitTransfer(t, tr); err != nil {
		t.Fatalf("transfer failed: %v\n%s", err, describe(tr.Stats()))
	}
	st := tr.Stats()
	if st.Failovers != 0 {
		t.Errorf("unexpected failover on a healthy single holder\n%s", describe(st))
	}
	assertCached(t, cacheB, hash, content)
}
```

**Topologies**

| Helper | Shape |
|---|---|
| `startFaultedPair(t, sA, sB, oA, oB)` | seeder `a` listens, fetcher `b` dials → returns `(a, b, link)` |
| `startFaultedTrio(t, sA, sB, sC, oA, oB, oC, seedRateA, seedRateC)` | seeders `a` and `c` listen, fetcher `b` dials both → returns `(a, b, c, linkA, linkC)`. The seed rates are `seed_rate_kib` (serving-side cap, 0 = unlimited), distinct from a link cap. |

**Faults** — `link.Set(...)` applies to live connections, not just new ones, so a
transfer already in flight feels the change:

| Builder | Effect |
|---|---|
| `slowDown(bytesPerSec)` | caps the seeder→fetcher direction |
| `rtt(d)` | splits a round-trip time across both directions |
| `partitioned` | refuse new dials **and** kill live ones; heal with `netfault.Fault{}` |
| `link.Script(flapSteps(cycles, down, up)...)` | a down/up timeline in the background |

**Waiting** — never `time.Sleep` for a mesh event:

| Helper | Use |
|---|---|
| `awaitTransfer(t, tr)` | block until the transfer ends (bounded by `chaosDeadline`); returns its error |
| `awaitProgress(t, tr, n)` | block until `n` bytes are readable — how you degrade a link *mid*-transfer |
| `waitFor(t, what, cond)` | poll any condition until `meshDeadline` (convergence, catalog sync, …) |
| `settleLastSeen(t, store, node, quiet)` | a post-partition liveness baseline; see Troubleshooting for why the naive read is racy |
| `warmMesh(t, a, b)` | pay yggdrasil session setup before measuring anything |

**Asserting**

| Helper | Use |
|---|---|
| `tr.Stats()` | the `TransferStats` snapshot — the point of the whole exercise |
| `describe(st)` | render it for a failure message; put it in *every* `t.Errorf` |
| `providerBytes(st, node)` | one holder's byte count |
| `assertCached(t, dir, hash, want)` | the blob landed byte-exact |
| `lastSeenOf(t, store, node)` | a peer's stored liveness timestamp |

**The shrunk clock.** `chaosOpts()` applies it to every node; the constants are in
`chaoshelp_test.go`. Values below are at 1×; all but the last are multiplied by
`testTimeoutScale`.

| Constant | Replaces |
|---|---|
| `chaosRefresh` 200 ms | refresh sweep 1 min, catalog sync 15 min |
| `chaosSnapshot` 50 ms | own-snapshot TTL 1 min |
| `chaosControl` 2 s | control-call timeout 15 s |
| `chaosChunkStall` 2 s | idle-read watchdog 20 s |
| `chaosPerChunk` 6 s | per-chunk backstop 2 min |
| `chaosTransfer` 6 s | whole-file backstop 30 min |
| `chaosDeadline` 90 s | *(not a production value — the test's own "slow vs. hung" bound on a transfer, deliberately much larger than `meshDeadline` because a chaos transfer is supposed to hit timeouts)* |
| `drainQuiet` 4 s | *(not scaled — wall-clock, and `last_seen` has one-second granularity either way)* |

Budgets you assert must be `testTimeoutScale`-relative and tolerance-shaped
("completes within N×"), never exact timings. **Faults must not be scaled that
way** — see the rule in Troubleshooting.

## Test seams in `federation/`

A hostile link is only half of it: production cadences are tuned for a quiet mesh
(a 15-minute catalog sync, a 2-minute per-chunk backstop), and "it eventually
finished" is not an assertion about *how* a transfer went. Three additive seams
close that — none changes default behavior, and nothing in the server sets them.

**`federation.WithIntervals`** shrinks the background cadences, so a scenario
converges in milliseconds rather than minutes:

```go
node, err := federation.Start(fc, store, logger, federation.WithIntervals(federation.Intervals{
	Refresh:     50 * time.Millisecond, // refresh-loop sweep       (default 1 min)
	CatalogSync: 50 * time.Millisecond, // per-friend catalog pull  (default 15 min)
	SnapshotTTL: 20 * time.Millisecond, // own-snapshot memoization (default 1 min)
}))
```

All three matter together: without `SnapshotTTL` the *serving* node keeps handing
out its memoized old catalog, so a fast puller learns nothing new.

**`federation.WithTimeouts`** shrinks the deadlines, so a stall scenario asserts
in seconds what would otherwise take minutes to fail:

| Field | Bounds | Default |
|---|---|---|
| `Control` | one ping / catalog / holdings call | 15 s |
| `Manifest` | one manifest probe against a holder | 20 s |
| `ChunkStall` | idle read: no bytes for this long ⇒ hung connection (also the blob client's response-header timeout) | 20 s |
| `PerChunk` | overall backstop for one chunk fetch | 2 min |
| `Transfer` | overall backstop for one whole-file fetch | 30 min |

A zero field keeps the default in both structs, so an override names only what it
cares about. The production values live in one place, `defaultIntervals` /
`defaultTimeouts` in `federation/node.go`.

**`Transfer.Stats()`** returns the `TransferStats` snapshot decoded in §Reading a
failure. It is on the `Transfer` interface, not test-only — the same data an
admin transfer view would want.

## netfault reference

A `Fault` carries two `Dir`s plus link-level triggers. The zero `Fault` is a
transparent proxy, which is what makes it a usable baseline.

| Knob | Scope | Meaning |
|---|---|---|
| `Latency` | per direction | Added delivery delay. Does **not** reduce throughput. |
| `Jitter` | per direction | Spreads each parcel's delay over ±Jitter, clamped at 0. Order is always preserved. |
| `Bandwidth` | per direction | Ceiling in bytes/s (token bucket, 100 ms burst). 0 = unlimited. |
| `Slice` / `SliceDelay` | per direction | Chop writes into ≤N bytes with a pause between — a dribbling path. |
| `Partition` | link | Refuse new connections and kill live ones. Healing is the same knob back. |
| `KillAfterBytes` | link | Cut a connection after N delivered bytes. |
| `KillAfterTime` | link | Cut a connection N after it was accepted. |

Standalone use, outside the chaos helpers:

```go
p, err := netfault.New(realUnderlayAddr, netfault.Fault{
	Down: netfault.Dir{Latency: 200 * time.Millisecond, Bandwidth: 1 << 20},
})
if err != nil {
	t.Fatal(err)
}
defer p.Close()

peerURI := "tcp://" + p.Addr() // hand this to config.FederationConfig.Peers

p.Set(netfault.Fault{Partition: true})   // cut the link mid-scenario
p.Script(                                // …or drive a timeline
	netfault.Step{At: 10 * time.Second, Fault: netfault.Fault{Partition: true}},
	netfault.Step{At: 30 * time.Second, Fault: netfault.Fault{}},
)
st := p.Stats() // accepted / refused / killed / active / bytes each way
```

Note `Up` = client→target and `Down` = target→client at this level. The chaos
helpers pin which end is which (§The model); a standalone proxy does not.

`Options{AllowRemote: true}` lifts the loopback restriction on both the bind
address and the target — see the warning at the top.

## Gating

Two mechanisms, deliberately not unified —
`docs/plans/mesh-testing.md` §Gating has the full reasoning:

- **`MADSHARE_CHAOS=1`** gates *execution* of the timing-sensitive and
  long-running tests. They still **compile** on every `go test ./...`, so a
  refactor in `federation/` breaks them loudly and immediately instead of rotting
  unnoticed. This suite tracks `federation/` internals far too closely to survive
  being invisible to the compiler.
- **`-tags tests`** will gate the *tool binaries* (`cmd/netfaultd`, `cmd/meshlab`)
  once they exist. Unlike `go build ./...`, `go install ./...` writes every `main`
  package into `GOBIN`, and `netfaultd` must not land next to `madshare` in
  someone's `/usr/bin`. Do not extend that tag to the library or the scenarios.

**Every new chaos test must call `requireChaos(t)` first.** Forgetting it is the
mirror-image failure: the scenario runs on every default `go test ./...` and makes
it minutes-slow.

Nothing here needs gating to stay out of the shipped server: `_test.go` files
never enter a binary, `make build` builds only `./`, and a package nobody imports
is never linked in. `netfault` is stdlib-only, so it adds no `go.mod` entries and
no bytes to the `madshare` binary.

## Troubleshooting

- **A scenario is flaky under load.** Almost always a budget written in
  wall-clock instead of `testTimeoutScale` units — the mesh is stochastic, and
  `-race` runs the gVisor netstack several times slower (`racescale_on_test.go`
  scales by 8). Assert "completes within N×", never an exact duration.
- **…but do not scale a *fault*.** The rule is **scale what costs *us* time, not
  what the *link* does**. A deadline you wait on scales; a cut cable does not.
  Three ways this bit while writing the suite, all under `-race`:
  - Scaling an *outage* 8× makes recovery *far* more than 8× worse, because
    yggdrasil's redial backoff grows with how long the peer was unreachable and
    runs on its own wall clock. The flap scenario went 61 s stale against its own
    54 s window purely from that.
  - But the *recovery window* after an outage **must** scale — reconnect plus a
    sweep landing takes ~4 s normally and ~23 s under `-race`. Leave it unscaled
    and the friend is never refreshed at all, which fails the same test from the
    opposite direction.
  - A bandwidth cap that stays fixed while the per-chunk timeout scales hands a
    "too slow to be usable" holder 8× more budget, so it stops being dropped and
    the scenario changes character. Pick a rate that misses the budget at *both*
    scales (the suite uses 4 KiB/s).
- **`last_seen` keeps moving for a moment after a partition.** `pingPeer`
  timestamps the store write, not the reply, so an exchange that succeeded just
  before the cut can land seconds later — reliably under `-race`. Take the
  baseline with `settleLastSeen`, never straight after `Set(partitioned)`.
- **`-race` needs `-p 1`** (parallel suites make SQLite's `busy_timeout` flake)
  **and a bigger `-timeout` than the repo's usual 3300 s**: `./federation/...`
  alone runs ~15 min under `-race` and the scenarios add ~22 min, and the combined
  run has overshot 55 min. Use `-timeout 7200s`, or split it with `-run Chaos`.
- **A healed partition doesn't reconnect immediately.** yggdrasil applies its own
  link retry backoff — measured at **2–8 s** on a loopback underlay, the longer
  figure after a longer outage. Poll for reconvergence (`waitFor`); never assert
  right after a heal. That delay is *wall-clock*, not part of the shrunk test
  clock, so a budget absorbing it must add it rather than scale it.
- **A scenario "degrades" a link and nothing changes.** Check the direction:
  blob bytes are `Down`, requests are `Up` (§The model). Also check you called
  `Set` *after* the friendship handshake — degrading before convergence usually
  just makes the handshake slow instead of testing the transfer.
- **A "slow holder" is dropped, not deprioritized.** `chunkPlan` picks providers
  round-robin with no speed awareness; what keeps a crawling holder from
  dominating is the per-chunk timeout plus the retirement rule. Scenarios
  therefore need a per-chunk budget short enough that a slow holder actually
  fails — `chaosPerChunk`, not the 2-minute production default. (This is a known
  efficiency gap, logged in `.issues/open-issues.md`.)
- **Retirement is relative, so a slow holder needs a *faster peer* to be
  retired.** A holder is dropped once it is `providerFailureLimit` failures worse
  than the best live holder — so a scenario where *every* holder is degraded will
  see nobody dropped, by design. If you want a holder retired, leave another one
  healthy. A fetch with no healthy holder left ends on the per-chunk attempt
  budget instead, with everyone still live.
- **A restarted node lost its friendships.** Node identity derives from
  `federation.key` — restarting without the same key file is a *new node*, not
  the same one returning.
- **Latency seems to cap throughput.** That is the bug `TestLatencyDoesNotThrottle`
  exists to catch: delay must be applied by the parcel queue, never as a sleep
  before each write.
- **Port allocation is racy.** The reserve-then-close `127.0.0.1:0` idiom has an
  inherent window; serial runs (`-p 1`) keep it narrow.

## Separation from the other suites

- **`tests/k6`** — load and performance against a running server. Throughput
  under concurrency, not correctness under adverse conditions.
- **`tests/playwright`** — browser end-to-end behavior.
- **This suite** — federation correctness when the network misbehaves.

They are complementary. Once `meshlab` exists it will be a legitimate *target*
for either of the other two.
