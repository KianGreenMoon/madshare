# Madnetwork mesh test suite — fault injection for federation

Correctness tests for madshare's federation layer under a **bad network**:
latency, jitter, narrow bandwidth, partitions, flapping links, seeders that
vanish mid-transfer, and — on a QUIC underlay — genuine packet loss, reordering
and duplication. Everything runs in userspace on one machine — no root, no
`tc`/netem, no containers, no external services.

The question this suite answers is *"does the swarm still behave when the network
misbehaves"*, and — because a transfer that merely finishes proves very little —
*"did it behave for the reason we think it does"*.

> ⚠️ **Everything here is loopback-only, and two pieces of it are dangerous off
> loopback.**
>
> - **`netfault` is a relay**, in both its forms: it listens on one socket and
>   forwards to another. It refuses a non-loopback bind *or* target unless you
>   pass `Options{AllowRemote: true}`, which turns it into an open relay reachable
>   from the network and pointing at whatever the host can reach. Being
>   connectionless makes the datagram half no safer. The chaos tests never set it.
> - **`netfaultd` is that, plus a control API that can retarget it** — the shape
>   of program that must never face a network. Relays, targets and the control
>   listener are all loopback-only unless `-allow-remote` is given, which logs a
>   warning naming the risk. It has no config file, no daemonization and no init
>   script on purpose: nothing about it should look installable.
> - **`meshlab` provisions madshare servers with known, hardcoded admin
>   credentials** (`meshlab-admin-pw`, in `node.go`, deliberately public — a lab
>   node is disposable and a secret nobody can type only makes it harder to poke
>   at by hand). Every node and its control API bind loopback. Never run it on a
>   shared host, and never point `-root` at a directory you care about: the nodes
>   will migrate and write to whatever is there.
>
> Do not set any of those escape hatches to work around a problem.

## What's here

```
tests/mesh/
  netfault/
    netfault.go        the fault model + TCP relay (a library; stdlib only)
    udp.go             the datagram relay — loss, reorder, duplication
    faultjson.go       the wire format both control APIs speak
    netfault_test.go   the injector's own tests — a fault proxy that lies is
    udp_test.go        worse than none
  cmd/netfaultd/       standalone relays + a JSON control API   [tags tests]
  cmd/meshlab/         a lab of real madshare processes         [tags tests]
    scope.go           the F5 sharing-scope knobs
    probe.go           an outsider madnetwork node — nobody's friend
    check.go           the scope rules, asserted against the lab
  README.md            this file
federation/
  chaos_test.go        the scenarios, over a tcp:// underlay
  chaos_quic_test.go   the scenarios that need datagrams
  chaoshelp_test.go    faulted topologies, the shrunk clock, requireChaos
  seams_test.go        the injectable intervals/timeouts + TransferStats
```

Two arms, and they answer different questions. **The chaos suite** asserts what
the swarm does — in Go, in milliseconds, with `TransferStats` to prove *why* it
passed. **meshlab** shows what a person sees: real processes, real migrations,
the real analysis pipeline, the real browse endpoints, in a browser. Neither
replaces the other, and a claim about the UI can only be made by the second.

The scenarios live in `federation/`, not here, because they are internal tests
(`package federation`) that reach into `chunkPlan`, `chunkLayout` and friends.
That is also a hard constraint, not just convenience: `database` imports
`federation`, so a test inside this package **cannot** import the browse layer
without an import cycle. See §The scenarios for what that costs.

## Prerequisites

For the **chaos suite**: Go, and nothing else — no audio fixtures, no database,
no running server. The scenarios generate their own blobs and run the mesh
in-process. On the maintainer's machine the toolchain is
`~/.guix-home/profile/bin/go`, which is **not** on the default `PATH`:

```bash
export PATH="$HOME/.guix-home/profile/bin:$PATH"
```

For **meshlab**, additionally:

- **`ffprobe` and `fpcalc` on `PATH`.** Not required, but without them the
  analysis pipeline degrades: no duration/bitrate/codec, no acoustic
  fingerprint, so the quality ladder falls back to format-and-size and same-audio
  grouping does not happen. A lab meant to show rendition ranking needs both.
- **`TEST_AUDIO_DIR`** pointing at real audio. meshlab *discovers* files, it
  does not generate them — the same discover-don't-seed rule as `tests/k6`, and
  for the same reason: a synthesized blob produces a catalog that browses but
  ranks nothing. Each node gets a **distinct slice**, because the question a lab
  answers is "can this node see what that one has", and shared libraries look
  correct whether federation works or not.

```bash
export TEST_AUDIO_DIR=~/music
```

## Quick start

```bash
# Nothing extra: the injector's deterministic tests. ~2 s.
go test ./tests/mesh/...

# Add netfault's timing-sensitive cases (measured latency, throughput, kills). ~10 s.
MADSHARE_CHAOS=1 go test -count=1 ./tests/mesh/...

# The scenarios: real meshes under real faults. ~3.5 min, serial, verbose.
MADSHARE_CHAOS=1 go test -p 1 -run Chaos -v ./federation/...

# One scenario, with its stats readout. ~20 s.
MADSHARE_CHAOS=1 go test -run TestChaosSeederVanishesMidTransfer -v ./federation/...

# Just the datagram half — packet loss, reordering, duplication. ~40 s.
MADSHARE_CHAOS=1 go test -p 1 -run 'TestChaos(Lossy|Scrambled|SustainedLoss)' -v ./federation/...

# The federation test seams the scenarios are built on. ~4 s.
go test -run 'Intervals|TransferStats' ./federation/...

# Everything, under the race detector. Slow — see Troubleshooting for the timeout.
MADSHARE_CHAOS=1 go test -race -p 1 -timeout 7200s -count=1 ./federation/... ./tests/mesh/...
```

And the lab, which needs its binaries built first (`make mesh-tools`):

```bash
make mesh-tools                                  # -> tests/mesh/bin/

# A 3-node chain, all friended. Foreground; Ctrl-C tears it down. ~10 s to up.
tests/mesh/bin/meshlab up -topology chain -nodes 3

# From another shell:
tests/mesh/bin/meshlab status
tests/mesh/bin/meshlab seed -audio ~/music       # distinct library per node
tests/mesh/bin/meshlab link b-a latency 200ms bandwidth 65536
tests/mesh/bin/meshlab link b-a clear
tests/mesh/bin/meshlab partition c               # cut every link touching c
tests/mesh/bin/meshlab heal c
tests/mesh/bin/meshlab kill c ; tests/mesh/bin/meshlab restart c
tests/mesh/bin/meshlab flap b -down 10s -up 20s  # `heal b` stops it

# Sharing scope (F5), and the assertion pass over it:
tests/mesh/bin/meshlab scope                     # every node's scope
tests/mesh/bin/meshlab scope a tracks private    # take a's tracks off the network
tests/mesh/bin/meshlab scope a tracks guest on   # …or open them to everyone
tests/mesh/bin/meshlab check                     # 20 cases, ~3 s, non-zero on failure
```

Or relays without a lab, for faulting something you started yourself:

```bash
tests/mesh/bin/netfaultd -link a-b=127.0.0.1:9001 -link b-c=quic://127.0.0.1:9002
curl -s localhost:7777/links
curl -s -X PUT localhost:7777/links/a-b -d '{"down":{"latency":"200ms"}}'
curl -s -X PUT localhost:7777/links/b-c -d '{"up":{"loss":0.05},"down":{"loss":0.05}}'
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

**Transport split.** TCP is a reliable byte stream, so the stream relay cannot
emulate packet loss: dropping bytes would corrupt the connection rather than
model a lossy path, and real loss surfaces as stalls and resets because the
kernel retransmits. Three faults therefore live one layer down, on the datagrams
a `quic://` peering rides — yggdrasil 0.5.14 speaks QUIC natively, so this costs
nothing but a different URI scheme:

| Underlay | Relay | Use it for |
|---|---|---|
| `tcp://` | `netfault.Proxy` | latency, jitter, bandwidth, slicing, partitions, mid-stream kills |
| `quic://` | `netfault.UDPProxy` | genuine packet **loss**, **reordering**, **duplication**, per-datagram jitter |

`UDPProxy` demultiplexes by source address into *flows*, each with its own socket
toward the target, and schedules delivery per datagram rather than queueing in
arrival order — which is what lets a reordered packet actually be overtaken
instead of holding up everything behind it.

Two knob sets that deliberately do **not** match, because each would be a lie on
the other transport:

- No `Loss`/`Reorder`/`Duplicate` on `Fault`. The kernel would repair them before
  yggdrasil saw anything.
- No `Slice`, `KillAfterBytes` or `KillAfterTime` on `DatagramFault`. Chopping a
  datagram corrupts it, and closing a flow removes nothing: the client's next
  packet opens a new one, which to QUIC is a NAT rebinding it migrates across.
  **`Partition` is the only way to remove a datagram source** — the same
  conclusion the stream side reached from the other direction, where a
  `KillAfterBytes` cut just makes yggdrasil redial.

`Partition` itself differs for the same reason: on TCP it also kills live
connections, because a peer must be *told* the path is gone; on UDP it only stops
carrying packets, so a heal needs no redial and QUIC picks up where it left off
(unless the outage outlasts its one-minute idle timeout).

**Degrade after convergence, always.** Every QUIC scenario friends the nodes on a
clean link and applies the fault afterwards. A handshake is the least interesting
thing a lossy path can break, and letting setup fail probabilistically would make
scenarios flaky for reasons unrelated to what they assert.

## The scenarios

`federation/chaos_test.go` (stream underlay) and `federation/chaos_quic_test.go`
(datagram underlay). Each asserts a claim the code makes, not merely that a
transfer eventually finished — `TransferStats` is what makes the difference
assertable.

Over a `tcp://` underlay:

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

Over a `quic://` underlay:

| Scenario | Fault | Claim under test |
|---|---|---|
| `LossyPathCompletes` | 15 % loss, both directions | loss stays a *transport* problem: the transfer completes byte-exact and never surfaces as a corrupt chunk — which would retire holders that did nothing wrong |
| `ScrambledPathKeepsChunksIntact` | 20 % reordered by 30 ms, 10 % duplicated, **no loss**, two holders | reassembly from two independently disordered sources still verifies per chunk *and* whole-file |
| `SustainedLossStaysReachable` | 5 % loss, sustained | a permanently lossy friend still counts as reachable — `last_seen` keeps up, friendship survives, and the local inbound signal is not flipped by a remote fault |

Every QUIC scenario also asserts the **injector's own counters** (`assertLossy`,
`assertScrambled`). A transfer that "survived 15 % loss" over a link that dropped
nothing has proved nothing, and that is a silent failure — so the fault is
verified to have happened before its result is believed.

### Two things this suite deliberately does not test

Both for the same structural reason — **netfault faults the underlay, and some
behavior lives above it.** Neither is a gap; knowing *why* saves the next person
from trying.

*(A third, for a different reason: **sharing scope** is an authorization rule, not
a timing one, so faulting the link would only make the answer arrive later.
`federation/scope_test.go` asserts it over an in-process mesh, and `meshlab check`
asserts it against real servers with a real outsider — see below.)*

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

## meshlab — a lab of real servers

`tests/mesh/cmd/meshlab` starts N real madshare processes on one machine, each
with its own data dir, database, `federation.key` and ports, peered through
faulted links. `up` runs in the foreground and holds the lab; every other command
is an HTTP client to its control API.

### The two graphs

This is the load-bearing idea, and getting it wrong makes the lab lie.

> **`-topology` chooses the underlay peering graph. `-friends` chooses who is
> friends with whom. They are separate.**

Federation is **friends-only and direct** — nothing is relayed at the madshare
layer, and nothing is discovered transitively. A lab that friended everything it
started would test one point in the space and hide the two that matter:

| Shape | How | Must show |
|---|---|---|
| **friends across hops** | `-topology chain -friends all` — `a` and `c` are friends but two underlay hops apart | works exactly as if adjacent; yggdrasil routes it, and degrading `c-b` degrades a friendship neither end has a link to |
| **adjacency is not access** | `-topology chain -friends adjacent` — `a` and `c` can route to each other but are strangers *in the friend graph* | since F7 they are nonetheless **members of one community** (`a—b—c` is a mutual chain), so each reaches the other's Madnetwork-scoped library while `Direct friends` content stays private to `b`. A true outsider is a node in no community at all — that is what `probe.go` is for |

That prediction held: F5 landed as a different friendship graph plus new knobs
over the same topology, not a meshlab rewrite.

`-friends` takes `all` (default), `adjacent`, `none`, or an explicit list
(`a-c,b-c`).

### What each principal gets since F7

F5 made "not a friend" stop meaning one thing; F7
(`docs/architecture/federation-access.md` §Principals & access) split it again, into a
**member of our community** and an **outsider**. Three columns, and the middle
one is the phase:

| Route | Direct friend | Member of our community | Outsider |
|---|---|---|---|
| `GET /madnetwork/v0/ping` | 200 | 200 | **200** — `meshAuth` refuses only *blocked* peers |
| `GET /madnetwork/v0/catalog` | 200 | **200**, Madnetwork scope | **403** |
| `GET /madnetwork/v0/holdings` | 200 | **200** — the swarm's boundary is the community | **403** |
| `GET /madnetwork/v0/blob/{hash}` | 200 if in scope | 200 if Madnetwork-scoped, or a cache blob | **404**; 200 for guest-playable *only if* the node opened `serve_guests` |
| `GET /madnetwork/v0/manifest/{hash}` | follows the blob | follows the blob | follows the blob |

The blob answers are **404, not 403**: an outsider is never told which hashes
exist. `Direct friends` scope is the one thing membership does not buy — it is
the exception *inside* the perimeter, not the perimeter itself.

A **member** is a key reachable from us through **mutually declared**
friendships: both ends must have published the edge, so one friend relaying one
invented record mints nobody. Our own direct friends are members unconditionally
(that edge is a local fact, not hearsay), and further out a node that publishes
no friend list cannot be a member at all.

Guests are the deliberate exception and they are **off by default** since F7 —
`meshlab check` opens `serve_guests` for one case and closes it again. That
switch is the reason `check` needs a real outsider rather than a `-friends none`
lab node: with `-friends none` the nodes are still nobody's members only if the
gossip never links them, which is fragile to assert. `probe.go` starts a genuine
madnetwork node with its own key, in no community, which is unambiguous.

Scope beats the guest flag: a recording at `local` serves nobody, guest-playable
or not.

### Topologies

| Preset | Underlay shape | What it is for |
|---|---|---|
| `pair` | `b → a` | the smallest useful lab |
| `triangle` | `b → a`, `c → a`, `c → b` | the swarm shape: two holders reachable independently |
| `hub` (default 4) | every node → `a` | spokes are two hops apart; cutting `a`'s links isolates everything at once |
| `chain` (default 3) | `b → a`, `c → b`, … | the only shape that exercises yggdrasil's multi-hop routing rather than a single link |

`-transport tcp` (default) or `quic`. Use `quic` when you want packet loss —
same reason as the chaos suite, one layer down.

### Seed at `up`, not after

```bash
meshlab up -topology triangle -seed ~/music -per-node 1
```

A friend's catalog is pulled on the refresh sweep only when it is older than the
**15-minute** sync interval, and that timestamp lives in the database, so a
restart does not reset it. Friend an empty node and you sync an empty catalog,
then wait a quarter of an hour to see anything. `-seed` seeds *before* friending,
so the nudge that fires on a new friendship
(`federation/friendship.go:197`) pulls a library that is already there — catalogs
converge in seconds.

`meshlab seed` afterwards still works; its results just take until the next sync
to reach the friends, and the command says so.

Seeding is not instant: each file goes upload → submit → approve → analysis, and
meshlab waits for `ffprobe` to fill in durations before declaring a node ready.
Three FLACs totalling ~80 MB took about six minutes on the maintainer's machine.

### Commands

| Command | Effect |
|---|---|
| `meshlab status` | every node's library count, **madnetwork count**, inbound health, and each friend's `last_seen` age against the freshness window |
| `meshlab link NAME KNOB VALUE…` | e.g. `link b-a latency 200ms bandwidth 65536`; `-dir up\|down\|both` |
| `meshlab link NAME clear` | back to a perfect link |
| `meshlab partition NODE` | cut every link touching it — the closest thing to unplugging the machine. The process keeps running, so its own view of the outage is observable too |
| `meshlab heal NODE` | undo a partition (and stop a flap). Only the `Partition` bit is flipped, so a latency or bandwidth condition set earlier survives |
| `meshlab kill NODE` / `restart NODE` | stop / bring back. **Identity survives** — `federation.key` stays in the data dir, and a node that lost it would be a stranger to every friend it had |
| `meshlab flap NODE -down 10s -up 20s` | partition/heal on a period until `heal` |
| `meshlab seed -audio DIR` | see above |
| `meshlab friend A B` | friend two **running** nodes. `up -friends` fixes the graph at startup; this adds an edge to a live lab — the friend-of-a-friend case (`up -friends a-b,b-c` then `friend a c`) is the one an admin actually meets, and the trust graph has to be a graph to support it |
| `meshlab scope` | every node's sharing scope: its default depth, and how many recordings are private or guest-playable |
| `meshlab scope NODE default DEPTH` | the node-wide default. `DEPTH` is `private`, `friends`, `network`, or a hop count |
| `meshlab scope NODE tracks SCOPE [-limit N]` | pin the sharing scope of its recordings — `local`, `friends` or `network` (`inherit` clears the override). `-limit 1` touches only the oldest, which is the shape most assertions want |
| `meshlab scope NODE tracks guest on\|off [-limit N]` | flag recordings guest-playable |
| `meshlab check` | assert the scope rules; exits non-zero on a failure |
| `meshlab reach [-runs N] [-no-fetch]` | what friendship **distance** costs: mesh RTT per hop, then a real fetch |

**`madnetwork` is the number to watch.** It is what `/madnetwork` would show that
node — its own published set plus every friend's, after the availability filter,
computed at request time. So it falls when a friend goes stale and rises when one
returns, which is the feature working rather than a proxy for it.

### The availability walkthrough

This is the verification `docs/plans/availability.md` §Phase 4 left open —
*"reproduce on a real lossy/latent mesh, not loopback, that availability doesn't
flap"*. Measured on a 3-node triangle, one track each:

```bash
make mesh-tools
tests/mesh/bin/meshlab up -topology triangle -seed ~/music -per-node 1
```

```
$ meshlab status                    # each node: library 1, madnetwork 3
$ meshlab partition c               # c is unplugged; its process keeps running
   t+30s   a,b: madnetwork 3        # nothing hidden yet — the window has not passed
   t+90s   a,b: madnetwork 3
   t+120s  a,b: madnetwork 2        # c's exclusive track is gone from the browse
$ meshlab heal c
   t+90s   a,b: madnetwork 3        # back, with no restart and no admin action
```

The step at exactly 120 s is `reachable_window_sec`, and the lab uses the
smallest value madshare accepts (`config.MinReachableWindowSec`) — it cannot be
shrunk further, so this walkthrough costs two minutes of waiting by design. What
it demonstrates: hiding happens **at request time**, only for tracks held
*exclusively* by an unreachable friend (each node keeps its own and the reachable
friend's), and recovery is automatic.

To see the fail-open half, the local inbound path has to die rather than a
peering — a cut link deliberately cannot cause it (see §Two things this suite
deliberately does not test). `meshlab status` shows `INBOUND DEAD (browse fails
open)` if it ever does.

### The down-mark walkthrough (Phase 5, measured 2026-08-13)

The walkthrough above shows the *window*. This one shows the **down-mark**
(`federation.md` §Availability, "Reactive down-mark + the ping floor"), and it
needs a different shape: the mark is inert on the ping window by design, so the
subject has to be a node judged by the **pull** window — a member no friend of
ours vouches for, which means **three hops**.

```bash
meshlab up -nodes 4 -topology chain -friends adjacent -seed ~/music -per-node 1
```

From `a`: `b` is a friend, `c` is a member `b` hints about (tight window), and
`d` is unhinted at three hops — the 45-minute pull window. Check it before
trusting the run, straight out of `a`'s database:

```sql
SELECT substr(public_key,1,8), trust_state, last_seen, hinted_at, unreachable_at
  FROM federation_nodes;      -- d must show hinted_at = 0
```

Then, with all four nodes at `madnetwork 4`:

```
$ meshlab partition d
   t+5s    a: madnetwork 4     # nothing hidden — d is 4 min old on a 45-min window
$ curl … -X POST /api/madnetwork/download {"hash": <d's exclusive track>}
   → failed: mesh dial failed: context deadline exceeded
   t+20s   a: madnetwork 3     # d's track is gone, on ONE failed fetch
```

Measured: `unreachable_at` lands on `d`'s row above its `last_seen`, `b` and
`c` are **not** marked (they were answering — the guard recording evidence about
`d`, not about the network), the strip greys `d` alone, and `d`'s shelf goes
empty. Without the mark that state would have lasted another ~40 minutes.

Healing is the same fetch again: `last_seen` moves past the mark and everything
returns, with no clearing step and no admin action. Allow ~1 minute after `heal`
for the yggdrasil session to re-establish — the first retries fail on the dial
and simply move the mark forward, which is the forward-only rule working.

**The other half of the guard is worth running too:** `meshlab partition a`
— the observer itself. Every ping fails, and *nothing* is marked, because `a`
pings only its friend `b`, and `b` is also the node whose successful contact is
the most recent, so the "some **other** node answered" clause absorbs exactly
the node that fails most often. `a`'s browse still shrinks, but from the
ordinary 120 s friend window, not from a mark.

**The ping floor is not observable in a small lab, by construction.** It fires
for cached sources the pull rotation could not reach within a cycle, and with
three sources against a budget of four per minute there are none. It exists for
a frontier at `discovery_cap`; that half stays unit-tested.

> **Gotcha found while writing this.** `POST /api/admin/federation/discover`
> (pull-now) deliberately ignores membership — "an admin who pasted a key is
> asking us to try" — but **retention does not**, so a catalog pulled that way is
> dropped by the next sweep if the gossip graph has not yet made that node a
> member. The symptom is a node appearing in `madnetwork` and vanishing a minute
> later with *"dropped 1 cached catalog(s) from nodes outside our community"* in
> the log. Wait for `meshlab graph` to show every node **and** for the pull to
> happen on its own before starting a run. `graph` is the map (single-claim
> edges); membership needs **mutual** ones, so the map showing a node is not yet
> proof the frontier will keep it.

### `meshlab check` — the scope rules, asserted

Everything above is a knob for a person to turn. `check` is the one command that
*answers*, and it exists because F5's central claim is a **negative** one: what
an audience is not shown, it also cannot fetch. Negatives are what a browser
walkthrough is worst at — nothing appears, which looks exactly like a feature
that silently does nothing.

```bash
tests/mesh/bin/meshlab up -topology triangle -seed ~/music -per-node 1
tests/mesh/bin/meshlab check          # from another shell; non-zero on failure
```

**Two rules for anyone adding a case.**

*Assert on identities, never on labels or on counts of them.* A track's title,
an artist name and a node's name are all display text: two nodes may publish the
same one, and the merged browse then folds them into a single row by design
("N versions", §Catalog). A case that counts rows is therefore not asking about a
recording at all — it silently measures whatever else the lab happens to be
publishing. `private track leaves the node's own /madnetwork` was written that
way and passed only while no two nodes shared a title; it now walks the node's
own shelf and looks for the subject's **content hash**. Reach for the hash, the
tagset id or the recording id.

*`check` runs inside the `meshlab up` process*, not in the client that prints the
report — the client POSTs to the control API. Rebuilding after editing `check.go`
therefore changes nothing until the lab is restarted, and the symptom is a case
that keeps failing in exactly the way you just fixed.

```
PASS  outsider reaches the mesh                      ping = 200, want 200
PASS  outsider refused catalog                       /madnetwork/v0/catalog = 403, want 403 (friends only)
PASS  outsider refused holdings                      /madnetwork/v0/holdings = 403, want 403 (friends only)
PASS  normal blob is invisible to an outsider        blob = 404
PASS  normal blob is invisible to an outsider (manifest agrees) manifest = 404, want 404 (same as the blob)
PASS  guest-playable blob still refused while guests are closed blob = 404
PASS  guest-playable blob still refused while guests are closed (manifest agrees) manifest = 404, want 404 (same as the blob)
PASS  guests opened still cannot reach a Direct-friends track blob = 404
PASS  guests opened still cannot reach a Direct-friends track (manifest agrees) manifest = 404, want 404 (same as the blob)
PASS  guest-playable blob serves an outsider once opened blob = 200, 654618 bytes, sha256 47c9d7b1c13a… (want 47c9d7b1c13a…)
PASS  guest-playable blob serves an outsider once opened (manifest agrees) manifest = 200, want 200 (same as the blob)
PASS  private beats guest-playable                   blob = 404
PASS  private beats guest-playable (manifest agrees) manifest = 404, want 404 (same as the blob)
PASS  private track leaves the node's own /madnetwork madnetwork on a: 2 while private, 3 while shared (want +1)
PASS  friend is refused a now-private track          b streaming a's private track = 502, want any failure (its catalog is stale but the bytes are gated live)
PASS  friend can stream a shared track               b received 654618 bytes from a, sha256 47c9d7b1c13a… (want 47c9d7b1c13a…)
PASS  listener node without a token is refused       blob = 404, want 404
PASS  home server issues a capability token          b vouched for the probe
PASS  vouched listener node is served by a node that is not its home blob = 200, 654618 bytes, sha256 47c9d7b1c13a… (want 47c9d7b1c13a…)
PASS  a token buys membership, never friendship      Direct-friends blob = 404, want 404 even with a valid token

20 passed, 0 failed, 0 skipped in 3.4s
```

It picks the oldest published track on the first seeded node, walks it through
three scopes, and asks as an outsider each time. Three of the cases are ones an
in-process test cannot make:

- **the guest swarm, once opened, serves bytes**, verified against the content hash — not
  merely a 200;
- **the byte gate is live**: a friend whose cached catalog still advertises a
  now-private track is refused anyway. Catalog staleness is the *normal* state of
  a federated node (15-minute sync), which makes this the realistic case rather
  than an exotic one.
- **a listener node's token is honoured by a node that is not its issuer** (F7
  item 9). The token is minted by `b` through its ordinary authenticated API —
  no node card, no accept, no peer row — and presented to `a`, which serves it
  because it can place `b` in its own community. The same probe, same key and
  same connection is refused without it, which is what makes the pair an
  assertion rather than a demo. Only the probe can ask this: every lab node is
  already somebody's friend, so none of them is a stranger to test with.

The subject's original scope is restored afterwards, including on a failure — a
check that left a track private would quietly break whatever you did next.

The outsider is started on the first `check` and kept for the lab's life, so the
first run pays a few seconds of mesh convergence and later ones do not: measured
**3.1 s cold, then 24–68 ms**. Nothing here waits on a window or a sync, by design
— the byte endpoints re-read the scope predicate per request, and the one case
that depends on catalog staleness *wants* the stale copy rather than a fresh one.

It is **re-runnable**, which took one deliberate step: the success case leaves the
blob in the friend's download cache, and on the next run `EnsureBlob` would answer
from there without ever asking the holder — so the refusal case would read `200`,
a true answer to a question it had stopped asking. `check` therefore clears that
one hash from the friend's cache first. (The lab owns those directories; letting
an assertion quietly depend on a fresh lab does not.)

Verify it can go red before trusting it green — turning `seed_enabled` off on the
holder fails exactly the three cases that need bytes from it, and nothing else.

Watching it by hand tells the same story, and shows the asymmetry the check
asserts:

```
$ meshlab scope a tracks private
a: 1 recording(s) -> depth private
  track-a
$ meshlab status
  a  … library 1   madnetwork 2      # a's own view drops immediately
  b  … library 1   madnetwork 3      # b's cached catalog still lists it …
```

`b` keeps showing the track until its next catalog sync — and cannot fetch a byte
of it in the meantime. Visibility is cached; authorization is not.

### `meshlab reach` — what does friendship distance cost?

Built 2026-07-31, for a design question that came up while planning F7: if the
track I want lives on a node six friendships away, is reaching it slow? The
intuition that it must be — *ask my friend to ask his friend to ask…* — is
reasonable and wrong, because **the friendship graph is not the transport**. A
yggdrasil address is derived from a node key, so knowing a key already means
being able to dial its owner: every fetch does `AddrForKeyHex(peer.PublicKey)`
and connects, at distance 1 or 20 alike.

Run it on a chain, the shape where friendship distance and underlay distance
coincide and a slope would therefore show at its worst:

```
$ meshlab up -nodes 5 -topology chain -friends adjacent -seed ./audio
$ meshlab reach
```

Two arms, separated because they fail for unrelated reasons. **Routing** pings
every node by friendship distance — `/madnetwork/v0/ping` is open to strangers
(`meshAuth` refuses only *blocked* peers), so it measures the network alone.
**Reach** then tries a real content fetch from the first node for a track only the
distant node publishes; before F7 item 5 that arm was a row of 404s, because a
non-friend was not a provider.

Measured on a cold 5-node chain **after F7 item 5** (loopback, so treat the
absolute values as a floor and the *shape* as the finding):

```
  node   dist  ping warm   ping cold   first 64K   whole file  note
  b      1     1.3ms       3.03s       21.9ms      62ms
  c      2     974µs       19.1ms      56.8ms      62.1ms
  d      3     1.5ms       24.3ms      56.5ms      48.1ms
  e      4     2.3ms       1m2.04s     68.7ms      61.1ms
```

Three things worth keeping:

- **Warm RTT is flat across distance** — 1.3 ms at one hop, 2.3 ms at four. There
  is no slope to find, which is the answer to the design question.
- **Cold contact is the cost that is actually there**, and it is paid per *peer*,
  not per hop: in this run `b` (3 s, contacted first, absorbing the mesh join) and
  `e` (62 s) are the two expensive ones, at opposite ends of the chain. It is
  yggdrasil session setup — the same cost the F4 streaming work hit — it is not
  distance, and it is the argument for preferring holders we already have a warm
  session with.
- **The fetch arm is green at every distance, and flat**: ~50–70 ms to first
  bytes and ~48–62 ms for the whole file, at one hop and at four alike. That was
  the hypothesis this command was written to check, and it held. Before item 5
  every row past `b` read `stream = 404 (not a provider: c is not a friend of a)`.

> **This run needs the gossip graph to have converged**, since a node is pulled
> from only once it is a *member* (mutually declared edges, §The membership rule).
> On a chain that is several relay rounds, and the catalog cadence is 15 minutes —
> so either wait, or press Rescan on each node
> (`POST /api/admin/federation/graph/resync`) and check `meshlab graph` shows all
> N nodes everywhere before running `reach`. The frontier then pulls
> `discovery_budget` (4) catalogs per sweep.

`-friends adjacent` is what makes this a chain of *friendships*; with the default
`-friends all` every node sits at distance 1 and there is nothing to measure.
`-no-fetch` runs the routing arm alone, which is fast enough to repeat.

> **The `ping cold` column is only cold once.** The outsider probe is started on
> first use and kept for the lab's life, so a second `reach` run against the same
> lab reports warm numbers there (sub-millisecond, and meaningless as a cold-start
> figure). Read that column on the first run after `up`, or restart the lab. The
> `ping warm` column is repeatable and is the one to compare across distances.

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
  (`docs/architecture/federation-swarm.md` §Distribution).

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

| Helper | Underlay | Shape |
|---|---|---|
| `startFaultedPair(t, sA, sB, oA, oB)` | `tcp://` | seeder `a` listens, fetcher `b` dials → returns `(a, b, link)` |
| `startFaultedTrio(t, sA, sB, sC, oA, oB, oC, seedRateA, seedRateC)` | `tcp://` | seeders `a` and `c` listen, fetcher `b` dials both → returns `(a, b, c, linkA, linkC)`. The seed rates are `seed_rate_kib` (serving-side cap, 0 = unlimited), distinct from a link cap. |
| `startQUICPair(t, sA, sB, oA, oB)` | `quic://` | the datagram counterpart → returns `(a, b, link)` with `link` a `*netfault.UDPProxy` |
| `startQUICTrio(t, sA, sB, sC, oA, oB, oC)` | `quic://` | two seeders, each behind its own datagram proxy → `(a, b, c, linkA, linkC)` |

**Faults** — `link.Set(...)` applies to live traffic, not just new connections, so
a transfer already in flight feels the change:

| Builder | Underlay | Effect |
|---|---|---|
| `slowDown(bytesPerSec)` | `tcp://` | caps the seeder→fetcher direction |
| `rtt(d)` | `tcp://` | splits a round-trip time across both directions |
| `partitioned` | `tcp://` | refuse new dials **and** kill live ones; heal with `netfault.Fault{}` |
| `link.Script(flapSteps(cycles, down, up)...)` | `tcp://` | a down/up timeline in the background |
| `lossy(rate)` | `quic://` | drops that share of datagrams in **both** directions — see below |
| `scrambled(reorder, duplicate, delay)` | `quic://` | reorders and duplicates without dropping anything |
| `netfault.DatagramFault{Partition: true}` | `quic://` | stops carrying packets; heal with `netfault.DatagramFault{}`. No builder — nothing needs one yet, and a cut path is already covered on the stream side, where it is the harder case. |

`lossy` is symmetric on purpose. A one-sided drop rate is not something a path
does — loss is a property of the wire — and a `Down`-only rate would let every
acknowledgement and retransmission through unharmed, quietly making the scenario
easier than it claims to be.

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
| `describeLink(link)` | a datagram relay's counters — log it beside `describe(st)` |
| `assertLossy(t, link, want)` | the path really was that lossy, and the relay added no weather of its own |
| `assertScrambled(t, name, link)` | reordering and duplication really happened, and nothing was lost |

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

### Stream relay (`Proxy`, `tcp://`)

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

### Datagram relay (`UDPProxy`, `quic://`)

A `DatagramFault` carries two `DatagramDir`s plus `Partition`. Same `Options`,
same loopback-by-default rule, same `Script` shape (`DatagramStep`).

| Knob | Scope | Meaning |
|---|---|---|
| `Latency` | per direction | Added delivery delay, scheduled per datagram. Does **not** reduce throughput. |
| `Jitter` | per direction | Spreads each datagram's delay over ±Jitter, clamped at 0. Order is **not** preserved — a datagram path may reorder, and jitter alone will do it. |
| `Bandwidth` | per direction | Ceiling in bytes/s (token bucket, 100 ms burst). A datagram arriving with the transmit queue full is **dropped**, not buffered, and counted as `Overflowed`. |
| `Loss` | per direction | Probability in [0,1] of dropping a datagram outright. |
| `Duplicate` | per direction | Probability of delivering a second copy, immediately after the first. |
| `Reorder` / `ReorderDelay` | per direction | Probability of holding a datagram back by `ReorderDelay`, so its successors overtake it. Does nothing without a delay. |
| `Partition` | link | Stop carrying packets both ways. Flows survive; heal is the same knob back. |

```go
p, err := netfault.NewUDP(realUnderlayAddr, netfault.DatagramFault{})
if err != nil {
	t.Fatal(err)
}
defer p.Close()

peerURI := "quic://" + p.Addr() // hand this to config.FederationConfig.Peers

p.Set(netfault.DatagramFault{                    // 5 % loss both ways
	Up:   netfault.DatagramDir{Loss: 0.05},
	Down: netfault.DatagramDir{Loss: 0.05},
})
st := p.Stats() // flows / packets & bytes each way / lost / reordered /
                // duplicated / overflowed / refused
```

`Stats()` is not decoration here — it is how a scenario proves the fault it
configured actually happened. Always assert on it (§The scenarios).

### netfaultd — relays without a lab

For faulting a link between things meshlab did not start: a real deployment, a
hand-rolled pair, anything with a known address.

```bash
netfaultd -link a-b=127.0.0.1:9001 -link b-c=quic://127.0.0.1:9002
```

Each `-link` opens a relay and prints the address to point the near end at. The
control API is the same wire format meshlab's is (`netfault/faultjson.go` — one
codec, so a fault typed at a meshlab prompt and one curled here mean the same
thing):

| Route | |
|---|---|
| `GET /links` | every link's fault and counters |
| `GET /links/{name}` | one link |
| `PUT /links/{name}` | replace one link's fault |

A `PUT` is a **full replacement, not a merge** — the operation a session runs most
often is putting a link back to perfect, and a merge would make that depend on
what was set before. Unknown fields are rejected, which is how `loss` on a `tcp`
link becomes an error rather than a knob that silently does nothing.

## Gating

Two mechanisms, deliberately not unified:

- **`MADSHARE_CHAOS=1`** gates *execution* of the timing-sensitive and
  long-running tests. They still **compile** on every `go test ./...`, so a
  refactor in `federation/` breaks them loudly and immediately instead of rotting
  unnoticed. This suite tracks `federation/` internals far too closely to survive
  being invisible to the compiler.
- **`-tags tests`** gates the *tool binaries* (`cmd/netfaultd`, `cmd/meshlab`).
  Unlike `go build ./...`, `go install ./...` writes every `main` package into
  `GOBIN`, and neither an open relay nor a lab with hardcoded admin credentials
  may land next to `madshare` in someone's `/usr/bin`. Verified against Go 1.26:

  ```
  go install ./...   (untagged)  → GOBIN: madshare
  go install ./...   (tagged)    → GOBIN: madshare  meshlab  netfaultd
  ```

  **Do not extend that tag to the library or the scenarios.** If a package's
  sources are tag-excluded but a `_test.go` in it is not, Go still compiles the
  test package and every reference fails as *undefined* — a clean
  `go test ./...` turns into `FAIL … [build failed]`.

Nothing here needs gating to stay out of the shipped server: `_test.go` files
never enter a binary, `make build` builds only `./`, and a package nobody imports
is never linked in. `netfault` is stdlib-only, so it adds no `go.mod` entries and
no bytes to the `madshare` binary. The tag is a packaging safeguard, not a
build-hygiene one.

**Every new chaos test must call `requireChaos(t)` first.** Forgetting it is the
mirror-image failure: the scenario runs on every default `go test ./...` and makes
it minutes-slow.

## Troubleshooting

- **`meshlab up -seed` sits for two minutes per node.** `waitAnalysis` waits for
  `ffprobe` to fill in track durations, and gives up quietly after two minutes
  because a missing `ffprobe` is not a seeding failure. Some files never satisfy
  it however long you wait: a headerless or streamed **FLAC reports
  `duration=N/A`**, so the column stays empty legitimately. Check before blaming
  the lab —

  ```bash
  ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 track.flac
  ```

  — and seed from files that report a duration if you want a fast `up`. (Until
  2026-07-25 this timeout fired on *every* run: the poll read a `duration` field
  where `/api/tracks` returns `duration_seconds`, so no duration ever arrived.)
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
  measures **1496 s** all-in under `-race` (the whole package), of which
  `-run TestChaos` is **1348 s** for all eleven scenarios; `./tests/mesh/...` adds
  12 s. `-timeout 7200s` leaves room; use `-run Chaos` to split it. Note the older
  "~15 min plus ~22 min" figure is stale — it predates the netstack-teardown fix
  in the yggstack fork.
- **`netfault`'s own `Close` hangs.** If you extend either relay, anything that
  registers a goroutine parked in a blocking socket read must be serialized
  against `Close` — the `closing` flag exists for exactly that. A goroutine in
  `conn.Read` has one way out, its socket closing; it cannot select on the
  `closed` channel, so a session or flow registered *after* `Close` took its list
  is never torn down and `done.Wait()` never returns. Guarded by
  `TestCloseRacesNewConnections` / `TestDatagramCloseRacesNewFlows`, which loop
  the race 100× and report the hang instead of taking the package timeout with
  them.
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
  exists to catch (and `TestDatagramLatencyDoesNotThrottle` one layer down): delay
  must be applied by the parcel queue or the per-datagram timer, never as a sleep
  before each write.
- **A "5 % loss" run measures some other rate.** The relay adds a hop, and a hop
  with a default-sized socket buffer starts dropping under a burst the two real
  endpoints would have absorbed — drops indistinguishable from injected loss in
  the counters. `UDPProxy` asks for 4 MiB buffers (best-effort; the kernel clamps
  to `net.core.rmem_max`) and counts its own transmit-queue drops separately as
  `Overflowed`, which `assertLossy` checks. If `Overflowed` is climbing, the
  measurement is the relay's, not the knob's. In the injector's *own* tests the
  same problem appears on the sending side, which is why every sender there paces
  itself — see `pace` in `udp_test.go`.
- **A datagram scenario passes but proved nothing.** Loss, reorder and duplicate
  are *draws*: a mis-wired knob produces a perfectly healthy link and a green
  test. Call `assertLossy`/`assertScrambled` — every QUIC scenario does — and log
  `describeLink(link)` next to the transfer stats.
- **`Partition` behaves differently on the two relays**, and it is not a bug.
  TCP kills live connections (a peer must be told); UDP just stops carrying
  packets, so a heal needs no redial and QUIC resumes — unless the outage outlasts
  its one-minute idle timeout. Also, `KillAfterBytes`/`KillAfterTime` do not exist
  on `DatagramFault` at all: closing a flow removes nothing, because the client's
  next packet opens a new one and QUIC migrates across it.
- **A QUIC scenario fails during setup, not during the fault.** Degrade *after*
  friending. `startQUICPair`/`startQUICTrio` hand back a transparent link for
  exactly this reason — a lossy handshake is a flaky test, not a finding.
- **Port allocation is racy.** The reserve-then-close `127.0.0.1:0` idiom has an
  inherent window; serial runs (`-p 1`) keep it narrow.

meshlab specifically:

- **A node's library looks empty and federation looks broken.** Check you are
  authenticated. Content access is **default-deny**, so an unauthenticated read
  of `/api/artists` on a node with a full library returns an empty list — a very
  convincing way to misdiagnose federation. meshlab's own client always carries
  the node's bearer token; a `curl` you type does not.
- **`madnetwork` stays at the node's own track count.** The friends' catalogs
  have not synced. Seed at `up`, not after — see §Seed at `up`, not after.
- **Nothing happens for 15 minutes.** Same cause. The sync interval is a
  production value and meshlab does not shrink it; `WithIntervals` is a *test*
  seam and the server never sets it.
- **A friend takes two minutes to go stale.** That is `reachable_window_sec`, and
  the lab already runs at the smallest value madshare accepts. Not a bug, and not
  something to work around — watch `meshlab status` rather than guessing.
- **A partitioned *member* takes 45 minutes to go stale, not two.** Also not a
  bug. Since F7 item 10 the freshness window follows the observer: two minutes for
  a node this one pings every minute, three catalog cycles for one it only ever
  pulls from (`federation.md` §Availability, "Two clocks, two windows"). The
  availability walkthrough uses the **triangle**, where every node is a direct
  friend, which is why it steps at exactly 120 s. On the chain topology the
  two-hop nodes are members — and a member a friend of ours still vouches for
  goes stale on the *two-minute* window instead, because a hint is minute-cadence
  evidence. `reachable_window_sec` cannot shrink either of the two.
- **The lab browse endpoints are drill-down, not flat.** `/api/albums` needs an
  `artist_id` and `/api/tracks` an `album_id`; there is no "every track"
  endpoint. Artists come back as a bare array normally and as `{"items": […]}` on
  the keyset-paginated branch, so a client has to accept both.
- **`meshlab up` exits immediately.** It runs in the foreground by design (so does
  `netfaultd`) — there is no daemon mode, no state file and no init script,
  because nothing about either tool should look installable. Keep the shell open
  and drive it from another one.
- **A restarted node lost its friendships** — see the general note above. meshlab
  keeps the data dir across `restart`, so identity survives; deleting the lab root
  between runs does not.

## Separation from the other suites

- **`tests/k6`** — load and performance against a running server. Throughput
  under concurrency, not correctness under adverse conditions.
- **`tests/playwright`** — browser end-to-end behavior.
- **This suite** — federation correctness when the network misbehaves.

They are complementary, and `meshlab` is a legitimate *target* for either of the
other two: point k6 at a lab node's URL to load it while a link is degraded, or
Playwright at one to drive the browse through a partition.
