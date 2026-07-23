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

**Build status:** `netfault` (the TCP fault relay) is built. `netfaultd`,
`meshlab`, the QUIC/UDP relay and the federation chaos scenarios are not yet —
see `docs/plans/mesh-testing.md` for the phase plan.

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

**Transport split.** TCP is a reliable byte stream, so this relay cannot emulate
packet loss: dropping bytes would corrupt the connection rather than model a
lossy path, and real loss surfaces as stalls and resets because the kernel
retransmits. So:

| Underlay | Use it for |
|---|---|
| `tcp://` | latency, jitter, bandwidth, slicing, partitions, mid-stream kills |
| `quic://` | genuine packet loss, reordering, duplication *(not yet built)* |

## Quick start

```bash
# Everything that runs by default — fast, deterministic, no timing assumptions.
go test ./tests/mesh/...

# Add the timing-sensitive cases (latency/bandwidth/timeline accuracy).
MADSHARE_CHAOS=1 go test -count=1 ./tests/mesh/...

# Under the race detector: this package is concurrency-heavy by design.
MADSHARE_CHAOS=1 go test -race -count=1 ./tests/mesh/...
```

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

Federation's chaos scenarios live in `federation/chaos_test.go` (not yet built) —
next to the helpers they reuse, not here.

## Troubleshooting

- **A scenario is flaky under load.** Almost always a budget written in
  wall-clock instead of `testTimeoutScale` units — the mesh is stochastic, and
  `-race` runs the gVisor netstack several times slower (`racescale_on_test.go`
  scales by 8). Assert "completes within N×", never an exact duration.
- **`-race` on the database or federation packages** needs `-timeout ≥ 3300s` and
  `-p 1`; parallel suites make SQLite's `busy_timeout` flake.
- **A healed partition doesn't reconnect immediately.** yggdrasil applies its own
  link retry backoff. Poll for reconvergence (as `waitFor` does); never assert
  right after a heal.
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
