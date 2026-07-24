# Madnetwork mesh test suite — fault injection & multi-node lab

Correctness tests for federation under a **bad network**: latency, jitter, narrow
bandwidth, partitions, flapping links and seeders that vanish mid-transfer.
Everything here runs in userspace on one machine — no root, no `tc`/netem, no
containers.

> ⚠️ **These tools are hostile by nature.** `netfaultd` is an open relay whose
> control API can retarget it, and `meshlab` provisions madshare servers with
> known bootstrap admin credentials. Both bind **loopback only** and refuse
> anything else unless explicitly overridden. Run them on a disposable machine
> or your own workstation — never on a shared or reachable host.

**Build status:** `netfault` (the TCP fault relay), the `federation/` test seams
and the TCP chaos scenarios are built. `netfaultd`, `meshlab` and the QUIC/UDP
relay are not yet — see `docs/plans/mesh-testing.md` for the phase plan.

## Prerequisites

- Go (this project's toolchain; on the maintainer's machine that is
  `~/.guix-home/profile/bin/go`, which is **not** on the default `PATH`).
- Later phases only: `ffprobe` and `fpcalc` on `PATH`, so `meshlab`'s seeded
  libraries get a full quality ladder rather than the degraded fallback, and a
  `TEST_AUDIO_DIR` of audio fixtures (same convention as `tests/k6`).

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

The chaos suite builds its own topologies (`federation/chaoshelp_test.go`) rather
than wrapping those two, for one structural reason: `startNodeTrio` peers both
other nodes to a single hub, so its two seeders **share one underlay link** and
cannot be degraded independently. The faulted shape inverts it — **the fetcher
dials, every seeder listens**, each behind its own proxy — which both separates
the links and fixes the direction convention (`Down` is always blob bytes toward
the fetcher).

**Transport split.** TCP is a reliable byte stream, so this relay cannot emulate
packet loss: dropping bytes would corrupt the connection rather than model a
lossy path, and real loss surfaces as stalls and resets because the kernel
retransmits. So:

| Underlay | Use it for |
|---|---|
| `tcp://` | latency, jitter, bandwidth, slicing, partitions, mid-stream kills |
| `quic://` | genuine packet loss, reordering, duplication *(not yet built)* |

## Test seams in `federation/`

A hostile link is only half of it: production cadences are tuned for a quiet mesh
(a 15-minute catalog sync, a 2-minute per-chunk backstop), and "it eventually
finished" is not an assertion about *how* a transfer went. Three additive seams
close that — none changes default behavior, and nothing in the server sets them.

**`federation.WithIntervals`** shrinks the background cadences, so a scenario or
a lab converges in milliseconds rather than minutes:

```go
node, err := federation.Start(fc, store, logger, federation.WithIntervals(federation.Intervals{
	Refresh:     50 * time.Millisecond, // refresh-loop sweep      (default 1 min)
	CatalogSync: 50 * time.Millisecond, // per-friend catalog pull (default 15 min)
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
cares about.

**`Transfer.Stats()`** returns a `TransferStats` snapshot — mode
(`local`/`swarm`/`whole`), time-to-first-byte, per-provider bytes and chunks,
retries, failovers, stalls, corrupt chunks, dropped holders. This is what lets a
scenario assert that failover *happened*, that the fast holder carried the
majority, or that the chunk-0 prefetch paid off — F4's load-bearing claims — and
it is the same data an admin transfer view would want.

## The scenarios

`federation/chaos_test.go` — real meshes under real faults. Each one asserts a
claim the code makes, not merely that a transfer eventually finished; the
`TransferStats` seam is what makes the difference assertable.

| Scenario | Fault | Claim under test |
|---|---|---|
| `SlowAndFastSeeder` | one holder at 4 KiB/s | a crawling source cannot gate the transfer; the fast holder carries the bulk |
| `SeederVanishesMidTransfer` | partition one link mid-fetch | the survivor finishes it, and `Failovers > 0` proves chunks were re-routed |
| `AllSeedersVanish` | partition the only link mid-fetch | fails cleanly and promptly — no hang, nothing promoted into the cache, no orphaned `.part` |
| `LatencyTimeToFirstByte` | 300 ms RTT + 512 KiB/s | the lead ramp + chunk-0 prefetch put the first byte early in the transfer, not near its end |
| `TailSeekBeatsPrefix` | 512 KiB/s | a seek to EOF fetches the tail chunk out of order, while the contiguous watermark is still far behind |
| `RateLimitedSeeder` | `seed_rate_kib` on one holder | the serving-side cap throttles that holder, not the swarm |
| `PartitionThenHeal` | cut, then heal | friendship survives; `last_seen` freezes and falls behind the window, then recovers on its own; the changed catalog syncs |
| `FlappingLinkStaysFresh` | 2 s down / 6 s up, ×3 | the anti-flap guarantee: repeated outages never push a friend past the freshness window |

Two scenarios from the plan resolve differently than written, both for the same
structural reason — **netfault faults the underlay, and some behavior lives
above it**:

- *"Friend down past the window → its tracks disappear from the browse."* The
  hiding is a SQL predicate (`last_seen >= now-window`,
  `database.MadnetworkView.Cutoff`), unit-tested in `database/madnetwork_test.go`.
  What only a real mesh can show is the *input*: `PartitionThenHeal` asserts an
  unreachable friend genuinely falls behind the cutoff.
- *"Local inbound path dead → fail open."* A cut peering **cannot** produce this,
  and that is the design working. `InboundReaderAlive` watches the goroutine
  reading from the yggdrasil core into the netstack — above the underlay — so a
  partition leaves it blocked but alive. `PartitionThenHeal` asserts the inverse
  instead, which is the property that actually matters: a *remote* outage must
  never be mistaken for a *local* fault, or every partition would stop hiding
  anything. The dead-reader path itself is unit-tested with an injected read
  error (`federation/availability_test.go`, and `runInboundReader` in the
  yggstack fork).

## Quick start

```bash
# Everything that runs by default — fast, deterministic, no timing assumptions.
go test ./tests/mesh/...

# Add the timing-sensitive cases (latency/bandwidth/timeline accuracy).
MADSHARE_CHAOS=1 go test -count=1 ./tests/mesh/...

# The chaos scenarios: real meshes under real faults (~2 min, serial).
MADSHARE_CHAOS=1 go test -p 1 -run Chaos -v ./federation/...

# One scenario, with its stats readout.
MADSHARE_CHAOS=1 go test -run TestChaosSeederVanishesMidTransfer -v ./federation/...

# Under the race detector: concurrency-heavy by design, and ~8× slower.
# (Not the usual 3300s — see Troubleshooting; this run has overshot 55 min.)
MADSHARE_CHAOS=1 go test -race -p 1 -timeout 7200s -count=1 ./federation/... ./tests/mesh/...

# The federation seams the scenarios build on.
go test -run 'Seams|Intervals|TransferStats' ./federation/...
```

Scenario runs are verbose on purpose: every one logs a `TransferStats` readout
(mode, TTFB, per-holder bytes/chunks/failures, retries, failovers, stalls), which
is usually enough to diagnose a failure without re-running it.

Using it in a test — peer a node at the proxy instead of the real underlay:

```go
p, err := netfault.New(realUnderlayAddr, netfault.Fault{
	Down: netfault.Dir{Latency: 200 * time.Millisecond, Bandwidth: 1 << 20},
})
if err != nil {
	t.Fatal(err)
}
defer p.Close()

peerURI := "tcp://" + p.Addr() // hand this to config.FederationConfig.Peers

p.Set(netfault.Fault{Partition: true})           // cut the link mid-scenario
p.Script(                                        // …or drive a timeline
	netfault.Step{At: 10 * time.Second, Fault: netfault.Fault{Partition: true}},
	netfault.Step{At: 30 * time.Second, Fault: netfault.Fault{}},
)
```

`Set` applies to **live** connections, not just new ones — a transfer already in
flight feels the change, which is the point.

## Gating

Two mechanisms, deliberately not unified — see
`docs/plans/mesh-testing.md` §Gating for the full reasoning:

- **`MADSHARE_CHAOS=1`** gates *execution* of timing-sensitive and long-running
  scenarios. They still **compile** on every `go test ./...`, so a refactor in
  `federation/` breaks them loudly and immediately instead of rotting unnoticed.
- **`-tags tests`** will gate the *tool binaries* (`cmd/netfaultd`, `cmd/meshlab`)
  once they exist. Unlike `go build ./...`, `go install ./...` writes every `main`
  package into `GOBIN`, and `netfaultd` must not land next to `madshare` in
  someone's `/usr/bin`.

Nothing here needs gating to stay out of the shipped server: `_test.go` files
never enter a binary, `make build` builds only `./`, and a package nobody imports
is never linked in.

## Configuration — netfault knobs

A `Fault` carries two `Dir`s (`Up` = client→target, `Down` = target→client) plus
link-level triggers. The zero value is a transparent proxy.

| Knob | Scope | Meaning |
|---|---|---|
| `Latency` | per direction | Added delivery delay. Does **not** reduce throughput. |
| `Jitter` | per direction | Spreads each parcel's delay over ±Jitter, clamped at 0. Order is always preserved. |
| `Bandwidth` | per direction | Ceiling in bytes/s (token bucket, 100 ms burst). 0 = unlimited. |
| `Slice` / `SliceDelay` | per direction | Chop writes into ≤N bytes with a pause between — a dribbling path. |
| `Partition` | link | Refuse new connections and kill live ones. Healing is the same knob back. |
| `KillAfterBytes` | link | Cut a connection after N delivered bytes — a source dying mid-transfer. |
| `KillAfterTime` | link | Cut a connection N after it was accepted. |

`Options{AllowRemote: true}` lifts the loopback restriction on both the bind
address and the target. It exists for deliberate multi-host use; it turns the
process into an open relay, so it is never a default.

## Layout

```
netfault/       fault model + TCP relay (built)
cmd/netfaultd/  standalone relay + HTTP control API   [not yet built]
cmd/meshlab/    multi-node lab of real servers        [not yet built]
```

Federation's chaos scenarios live in `federation/chaos_test.go`, with their
faulted topologies in `federation/chaoshelp_test.go` — next to the internals and
helpers they reuse, not here. They are internal (`package federation`) by
necessity as well as convenience: `database` imports `federation`, so a test in
this package cannot import the browse layer without a cycle.

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
- **`-race` on the database or federation packages** needs `-p 1`; parallel
  suites make SQLite's `busy_timeout` flake. The usual `-timeout 3300s` is **not
  enough** once the chaos scenarios join the same binary: `./federation/...` on
  its own runs ~15 min under `-race` and the scenarios add ~22 min, and the
  combined run has overshot 55 min. Use `-timeout 7200s`, or run the scenarios
  separately with `-run Chaos`.
- **A healed partition doesn't reconnect immediately.** yggdrasil applies its own
  link retry backoff — measured at **2–4 s** on a loopback underlay. Poll for
  reconvergence (as `waitFor` does); never assert right after a heal. That delay
  is *wall-clock*, not part of the shrunk test clock, so a budget that has to
  absorb it must add it rather than scale it.
- **Which direction is which.** In the chaos helpers the fetcher always dials and
  the seeder always listens, so blob bytes are the proxy's `Down` direction and
  requests are `Up`. `slowDown()` throttles the seeder for that reason. Getting it
  backwards produces a scenario that degrades nothing and passes anyway.
- **A "slow holder" is dropped, not deprioritized.** `chunkPlan` picks providers
  round-robin with no speed awareness; what keeps a crawling holder from
  dominating is the per-chunk timeout plus `providerFailureLimit`. Scenarios
  therefore need a per-chunk budget short enough that a slow holder actually
  fails — `chaosPerChunk`, not the 2-minute production default.
- **A restarted node lost its friendships.** Node identity derives from
  `federation.key` — restarting without the same key file is a *new node*, not
  the same one returning.
- **Latency seems to cap throughput.** That is the bug `TestLatencyDoesNotThrottle`
  exists to catch: delay must be applied by the parcel queue, never as a sleep
  before each write.

## Separation from the other suites

- **`tests/k6`** — load and performance against a running server. Throughput
  under concurrency, not correctness under adverse conditions.
- **`tests/playwright`** — browser end-to-end behavior.
- **This suite** — federation correctness when the network misbehaves.

They are complementary, and a `meshlab` deployment is a legitimate *target* for
either of the other two once it exists.
