# Maybe to do — request depth, the per-chunk deadline, and what they cost

**Status: ANALYSIS, NOT SCHEDULED.** Nothing here is a decision and nothing here
is queued. It is the record of a conversation on 2026-08-14, immediately after
work-queue slot 5 shipped (`30f13cc`), kept so the reasoning does not have to be
rebuilt from scratch. Read `docs/architecture/federation-swarm.md` §"Pipelining"
for what is actually built.

The trigger was a question worth writing down as the goal:

> I wanted the depth to be unimportant. On 1 or 8 or 110 it should be the same
> speed.

That is the right target, and today it is not true. This file says how far it is
from true, what we measured, what we only *believe*, and what changing it would
cost.

---

## 1. Two settled points, so they are not re-asked

**Chunks do not travel through other nodes at our layer.** A fetch is one direct
connection from the fetching node to the holder's mesh address (`dialHolder` →
`DialContext` on the netstack). There is no application-level relaying, no
store-and-forward through friends. The one thing called a relay,
`GET /api/madnetwork/stream/{hash}`, is our own server streaming to our own
browser, not mesh-to-mesh.

What *is* true is that yggdrasil is a routed overlay: unless two nodes are
directly peered, the packets traverse intermediate ygg peers, end-to-end
encrypted, so those nodes carry bytes they cannot read. That transport path is
where every "capped link" in the measurements below actually lives.

**Membership is proven cryptographically, never by the path the bytes took.** A
requester's mesh address derives one-way from its public key, so it cannot be
presented by anyone without that key; `serveAudience` matches the address against
the member set built by walking the gossiped *mutual*-friendship graph
(`MemberKeys`). The friend chain and the yggdrasil route are different graphs —
a friend may be five hops away through strangers, and a stranger may be a direct
peer. Nothing about proving community membership requires the bytes to follow
friend edges, and they do not.

So the depth question is purely about scheduling and deadlines. It is not
entangled with the access model.

---

## 2. Why depth is not free today

Physically it should be. N chunks sharing one link finish at the same *total*
time whatever N is; what changes is that each individual chunk's wall-clock
duration grows by N. Two things turn that into a cost:

### 2a. Reader tail latency — physics, and it is real

A streaming reader is blocked on the lowest-index chunk. With N chunks in
flight, that chunk gets 1/N of the link, so its wait is N× the floor — and when
it lands, the other N−1 arrive almost together. Total time is unchanged, the
reader's *average* wait is unchanged, but the **worst** wait is N× the floor,
and worst-case wait is what empties a player's buffer.

This is what slot 5 fixed for the sole-holder case, and no deadline change
touches it.

### 2b. A wall-clock deadline per chunk — an artifact, and probably the wrong one

`Timeouts.PerChunk` (2 min) is an *overall* backstop on one chunk attempt. It
measures elapsed time, and elapsed time is exactly the quantity that scales with
depth: put 8 chunks on one link and each takes 8× as long in wall clock, so a
fixed per-chunk budget can fail them all together while bytes are flowing
perfectly well.

`Timeouts.ChunkStall` (20 s) is different in kind — an **idle-read** watchdog. It
measures *progress*, so it is depth-independent by construction and is the rule
we would want to keep.

**The recorded attribution for the depth-8 failure is `PerChunk`, and the
arithmetic does not support it.** See §4.

---

## 3. Measurements — what was measured, how, and by whom

Every number below came from `tests/mesh/netfault` links between real nodes.
Two different instruments, and the distinction is the whole reason slot 5 was
missed for a month:

- **Transfer-timed** — how long `EnsureBlob` took. Every swarm scenario except
  the three below.
- **Reader-timed** — `federation/readerlatency_test.go`, which drives the
  relay's own loop (`WaitFor` → read `Available` → advance) and times each
  blocking read. `MADSHARE_CHAOS=1` gated.

Chunk layouts, so the numbers can be read (verified against `chunkSizeFor` /
`leadSizes`, 2026-08-14):

| blob | bulk chunk | lead ramp | chunks |
|---|---|---|---|
| 2 MiB | 256 KiB | none | 8 |
| 4 MiB | 512 KiB | [256 KiB] | 9 |
| 16 MiB | 1 MiB | [256 KiB, 512 KiB] | 18 |

### 3a. The depth/throughput measurement — INHERITED, not re-run

F9 item 4, recorded in `federation/scheduler.go` and `federation-swarm.md`.
**One holder**, 300 ms RTT, link capped 512 KiB/s, 4 MiB blob (9 chunks).
Instrument: transfer-timed.

| per-holder depth | elapsed |
|---|---:|
| 1 | 12.36 s |
| 2 | 12.30 s |
| 4 | 12.80 s (retries appear) |
| 8 | **transfer failed** |

This is the measurement that shipped `maxHolderRequests = 2`. Note what it
actually establishes: a **ceiling** (4 hurts, 8 is fatal). The gap between 1 and
2 is 0.5%, i.e. nothing. **The `2` was never earned by a measurement** — it was
the design's ask, and the experiment was run to find out whether *more* helped.

Also worth noting: 4 MiB in 12.3 s over a 512 KiB/s link is ~340 KiB/s, about
two-thirds of the cap. A third of the link went somewhere unexplained, and the
second slot did not recover it.

### 3b. The slot-5 reader measurement — 2026-08-14, earlier session

**One holder**, link capped 128 KiB/s, 2 MiB blob (8 chunks of 256 KiB → floor =
one chunk ≈ 2 s). Three runs each. Instrument: reader-timed.

| depth | worst reader wait | retries | rate | total elapsed |
|---|---|---|---|---:|
| 2 | 4.86 / 5.54 / 9.23 s | 0–4 | 76–108 KiB/s | 19.0–23.2 s |
| 1 | 2.396 / 2.405 / 2.415 s | 0 | 108 KiB/s | 19.0–19.1 s |

**Total elapsed is identical.** The entire cost is reader tail latency. The
5.54 s run reported `retries=0`, so it is contention alone — chunks blowing
`PerChunk` are a second-order consequence that appears only once the link is
oversubscribed.

A second, independent scenario (16 MiB at 1 MiB/s, 4 runs, the reported
"768 KiB stall") landed on the same cause: the reader never blocked at the
lead→bulk seam at all, its worst read was at 256 KiB (2.48–3.25 s), and at
depth 1 the run became 18 uniform 1.18 s reads — one 1 MiB chunk at 1 MiB/s,
the floor — with total elapsed unchanged at 18.8 s.

### 3c. Verification of the shipped fix — 2026-08-14, this session

`TestChaosReaderLatencyOnASoleCappedHolder`: one holder, 512 KiB/s, 2 MiB
(8 chunks of 256 KiB → floor 500 ms). Reader-timed, three runs.

| | reads | per-read waits | worst | ratio to floor |
|---|---|---|---|---|
| depth 1 (shipped) | 8 | 503, 598, 594, 596, 596, 598, 593, 593 ms | 598 ms | **1.2×**, identical 3/3 |
| depth 2 (rule disabled) | 7 | — | 1.208 s at offset 0 | **2.4×** |

The read-count difference is the mechanism in one line: at depth 2 the reader
pays two chunk-times for its first read and then gets a chunk for free, because
the second slot fetched a chunk it could not use yet at the price of the one it
was waiting for.

Same run, other reader-timed scenarios: the ramp (16 MiB) gave 18 reads, worst
298 ms against a 250 ms floor (1.2×); the hedge guard gave 4 reads, worst 57 ms.

`TestStaleHoldersCostAFetch`'s table, from the same chaos run — useful because
its "2 stale" row **is** a sole live holder at depth 1:

| scenario | measured | outcome |
|---|---|---|
| 0 stale (3 live) | 95 ms | swarm 8/8, retries 0 |
| 1 stale (2 live) | 45 ms | swarm 8/8, retries 0 |
| 2 stale (1 live) | 566 ms | swarm 8/8, retries 0 |
| 3 stale (0 live) | 2.004 s | correctly fails |

Suite state at the time of writing: `go test ./federation/` green in 172 s;
`MADSHARE_CHAOS=1` green in 380 s **except** `TestStaleHoldersCostAFetch`'s
final assertion — since **repaired (2026-08-14, `da0841a`)**: it now asserts
the F9 bound (each ghost ≤ its two dispatches × `Connect`) instead of a tax
that no longer exists, and the full chaos-enabled suite is green.

---

## 4. The hole in the story — read this before building anything

The doc says depth 8 fails because it "spends `Timeouts.PerChunk` rather than the
link". **The arithmetic does not close.**

At the measured settings — 4 MiB blob, bulk chunk 512 KiB, 8 workers, link
512 KiB/s — the eight in-flight chunks total 3.75 MiB, which the link delivers in
**~7.5 s** even with perfectly fair sharing. `PerChunk` is **120 s**: a 16×
headroom. It cannot plausibly be what fired.

Candidates that fit better, none of them verified:

1. **`ChunkStall` (20 s idle), not `PerChunk`.** Eight streams sharing one
   300 ms-RTT link do not share it evenly; one stream starved for 20 s while the
   others run is far more reachable than 120 s. **If this is the cause, removing
   `PerChunk` does not help at all** and the question becomes what an idle
   watchdog should do under deliberate concurrency.
2. **The unexplained third of the link** (§3a) — whatever makes a 512 KiB/s link
   deliver 340 KiB/s may also be what makes eight streams collapse.
3. **Something at the holder**: eight concurrent serves against one node's
   `throttledResponseWriter` / quota admission.

**So step 1 is a measurement, not a change.** Reproduce the depth-8 failure and
find out which deadline actually fires. Doing it in the other order risks
deleting a deadline that was not the problem, and shipping a regression against
a mechanism nobody has looked at.

---

## 5. Suggestions, in the order they would have to happen

Each step is gated on the one before it. Nothing is committed to.

### Step 1 — measure (no decision needed; it is the standing gate anyway)

Reproduce depth 1/2/4/8 on a capped link, **timing both the transfer and the
reader**, and instrument which deadline fires on the failure. Cheap, and it
either confirms or kills §4's candidates.

### Step 2 — if it is `PerChunk`: make it about progress, not elapsed time

The invariant: *a chunk fetch is killed for lack of progress, never for taking
long.*

- `dispatchLocked` — `context.WithTimeout(cp.parent, cp.perChunk)` becomes
  `context.WithCancel`. The context is still needed (it is how a hedge cancels
  the loser), just not deadlined.
- `fetchRange` — drops its own nested `PerChunk` (the speculation path).
- **Keep** `Timeouts.Connect` (5 s on the dial): a dial that never connects is
  making no progress, and it is what made stale holders cheap.
- **Keep** `ChunkStall` as the only per-chunk rule, and `Timeouts.Transfer` as
  the plan-level backstop.
- **Re-home `tryWindow`.** `worseThanPeers` currently uses `PerChunk` as "how
  recently a holder must have been asked to count as a benchmark". `ChunkStall`
  is the better source anyway: the window in which a healthy holder must have
  shown *something*.

*Is anything left unprotected?* The case `PerChunk` catches that `ChunkStall`
does not is a holder dribbling just above the stall threshold — progressing,
hopeless. Two existing mechanisms already cover it, which is the argument that
the deadline is redundant rather than load-bearing:

- **Multi-holder:** hedging reclaims it — a blocked reader hedges the chunk, the
  endgame hedges it when the queue empties, and the winner *cancels* the
  dribbler. Exactly what `3ff5846` was built for.
- **Sole holder:** cancelling gains nothing; you re-request the same chunk from
  the same holder over the same link.

With `Timeouts.Transfer` as the floor under both.

### Step 3 — replace the per-holder cap with the rule it approximates

The cap is a poor proxy for what we want. The real rule is:

> Do not start a chunk **nobody has asked for** while a reader is blocked on one
> already in flight.

That is work-queue shape 2, off the existing `cp.wanted` mark, and it makes the
distinction the cap cannot: a **background** materialize runs full width (nobody
is waiting, depth is genuinely free) while a **stream** stays uniform.

### Step 4 — then the cap deletes

`maxHolderRequests` and `requestCapLocked` both go, and `30f13cc` is superseded
by something more general. **Blast radius of removing it is bounded at 8, not
110**: `maxChunkWorkers = 8` caps concurrent chunk fetches per transfer, so "no
cap" means at most 8 in flight — which is exactly the depth-8 case, which is
exactly why step 1 has to come first.

Net effect if all four land: **less code than we have now**, and depth genuinely
irrelevant — free where nobody is waiting, invisible where somebody is.

---

## 6. What the cap is doing today, honestly

Three jobs, and only one still needs it:

| job | still needed? |
|---|---|
| **Reader tail latency** | Real, but better served by step 3's reader rule, which the cap only approximates. |
| **The depth-8 failure** | Unproven — see §4. If it is a deadline, step 2 owns it. |
| **Politeness to the holder** | Weak. Identical total bytes either way, and concurrent serves are the holder's own business via the F7 member quotas. Honest caveat: those default to `0` = unlimited and **direct friends bypass them entirely**, so today nothing on either end limits it — but 8 range requests is ordinary swarm behaviour, not abuse. |

---

## 7. Open questions

1. Which deadline actually fires at depth 8? (§4 — blocks everything else.)
2. Why does a 512 KiB/s link deliver ~340 KiB/s in the F9 measurement? Unexplained,
   and it may be the same phenomenon.
3. Does depth 2 buy anything at all in a **multi-holder** plan? Never measured,
   in either direction. If it does not, the whole knob is decoration.
4. If step 2 lands, does `ChunkStall` need to become concurrency-aware — and if
   it does, is that not the same wall-clock mistake in a smaller hat?
5. Should `maxChunkWorkers = 8` itself be the only concurrency bound? It is
   already the real ceiling; the per-holder cap sits underneath it.
