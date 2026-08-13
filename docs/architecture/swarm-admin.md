# The swarm admin page — what this node moves, and how fast

The swarm (federation F3/F4) fetches and serves bytes continuously and reports
almost none of it. `TransferStats` describes a fetch **while it runs** and is
discarded when it ends; the seeding side counts nothing at all, ever. So the two
questions an operator of a peer-to-peer node actually asks — *what is my node
moving?* and *what may it cost?* — have no surface.

This doc designs `/admin/swarm`: one list of every blob this node has bytes for,
with per-file transfer accounting, live progress, every transfer limit this node
has (its two rate caps and the four member quotas), and the totals underneath
them.

Reference for the transfer machinery it reports on:
[`federation-swarm.md`](federation-swarm.md) §Distribution. Reference for the cache half of
the row set: [`madnetwork-cache.md`](madnetwork-cache.md).

---

## Two lenses on one node, not two lists

Both pages stay (owner, 2026-08-06), and the reason is the one that should be
read first, because everything below follows from it:

> **The page follows the admin's job, not the row set.** On one page you
> configure the cache; on the other you configure how this node works in the
> swarm. Those are two tasks, so they are two lenses — the same blobs seen
> through a different question.

That is not a new idea here: `/admin/library` already carries three lenses (By
entity / All Appearances / Recordings) over one body of content, and they earn
their keep the same way — each is *complete* for one job, and none has to carry
another's vocabulary. Cache work never has to explain rates; swarm work never
has to explain retention.

| | `/admin/cache` | `/admin/swarm` |
|---|---|---|
| The job | keeping the disk in order | running this node in the swarm |
| Asks | *What is on my disk, and should it stay?* | *What is my node moving, and how fast?* |
| Rows | the download cache | **every blob with bytes** — library, cache, partials |
| Columns | what it is · when fetched · when last used | up · down · rate · progress |
| Owns | the cache index, reconciliation, Rescan, the retention daemon | the traffic counters, and every transfer limit |
| Deletes | cache blobs | nothing of its own — it *calls* the cache's remover |

What separates a lens from a copy is one rule, and it is a requirement on the
build:

> **A lens may duplicate a view. It must never duplicate an editor.**

Both pages *show* cached blobs; exactly one page *changes* any given thing. That
single rule decides every case in this design — including, further down, why the
rate knobs live here and not also on `/admin/settings`. Concretely:

1. **No parallel endpoints.** Where the two pages do the same thing — remove a
   cached blob, reap partials, read claims, play cache bytes — the swarm page
   calls the cache page's existing endpoint. `POST /api/admin/cache/bulk` stays
   the one remover, exactly as the library lenses share `database/curate.go`
   rather than each growing their own delete.
2. **No second index of the cache.** `madnetwork_cache` remains the only
   description of cache contents; the swarm list *joins* it. The swarm page
   never walks the cache directory for content (only for partials, which are not
   in any table by design).
3. **No retention UI here.** Rescan, last-used and the coming daemon belong to
   the cache page. The swarm page shows `last_used_at` nowhere — not because it
   could not, but because that is the other lens's question.

What moves *to* here is the thing the cache doc explicitly parked: **holders and
who-has-what**, in both directions (`madnetwork-cache.md` §Decisions, "No holder
count, no 'who has it'"). It lives in this page's per-row info panel, computed
live from the cached catalogs — a fact about now, not a recorded one.

---

## What exists today

Worth being precise, because most of this design is filling in absences.

**Nothing counts outbound bytes.** `handleBlob` resolves a path and hands the
file to `http.ServeContent` (`federation/transfer.go:90`). When a rate cap is
configured the writer is wrapped by `throttledResponseWriter`; when it is not —
the shipped default — the response writer is unwrapped and no byte passes
through anything of ours. There is no per-hash, per-peer or node total.

**Inbound bytes are counted only as diagnostics.** `transferStats.noteBytes` /
`noteSucceed` credit a *provider* during a fetch, and the whole `transferStats`
object dies with the transfer (`n.transfers` is deleted in `runTransfer`'s
defer). Nothing is written down, and a failed chunk's bytes are credited
nowhere, so even the live figure is "useful bytes", not "wire bytes".

**There is no download-side limiter at all.** `fetchRange` and `fetchFrom` read
`resp.Body` as fast as the mesh delivers. Every rate-limiting mechanism in the
codebase (`seedLimiter`, `quotas`) is on the serving side.

**Rate limits are fixed at construction.** `newRateLimiter(fc.SeedRateKiB*1024)`
runs once in `Start` (`federation/node.go:247`). Changing a cap means editing
`madshare.toml` and restarting.

**What does exist and is reusable:** `Node.ActiveTransfers()` (live fetches, with
`Size`/`Progress`/`Mode`/`Chunks`/`Providers`), the per-chunk readiness bitmap
inside a running transfer (`t.chunkOK` + `t.layout`), the `rateLimiter` token
bucket and its `throttledResponseWriter`, the cache index, and
`MadnetworkBlobProviders` (who advertises a hash).

---

## The model

### A row is a blob this node has bytes for

Three provenances, one list, and a blob may have more than one:

| Provenance | Where the bytes are | Seedable? |
|---|---|---|
| **library** | `files` row → storages registry | only when approved, live and in the requester's scope (`BlobVisibleTo`) |
| **cache** | `<data_dir>/cache/madnetwork/<hash>` | only when `seed_enabled && seed_cache` and the requester is in our community |
| **partial** | `<hash>.part` | never — unverified bytes are not seeded and not advertised |

The library arm lists **everything with bytes** (owner, 2026-08-06): drafts,
returned uploads and trashed blobs included. They occupy the disk, and a page
that answers "what is my node holding and moving" must not quietly omit the
900 MB of un-reviewed FLAC that is not moving at all. Each row carries the state
that explains itself — a **state chip** (`live` / `review` / `trashed`) and a
**scope chip** (the `share_depth` vocabulary already in
`webui/static/js/share-depth.js`) — and a row that cannot seed says so instead
of being absent. This is display only: nothing here changes review or trash
state, and the row links to the page that does.

Library and cache copies of one hash are transient (the startup sweep evicts the
cache duplicate) but must never double-count, so the union is grouped by hash
into one row carrying **both** provenance flags. Text is preferred from the
library's representative appearance (`reprTagset` — our curated truth) and falls
back to the cache index's own-tag text.

### Migration 041 — `swarm_traffic`

One row per content hash this node has ever moved. Counters only — there are no
per-file settings of any kind (see §Rate limits):

```sql
CREATE TABLE swarm_traffic (
    hash         TEXT PRIMARY KEY,           -- lowercase sha256 hex
    up_bytes     INTEGER NOT NULL DEFAULT 0, -- served to the mesh, all time
    down_bytes   INTEGER NOT NULL DEFAULT 0, -- pulled off the mesh, all time
    wasted_bytes INTEGER NOT NULL DEFAULT 0, -- received and discarded
    first_at     INTEGER NOT NULL,
    last_at      INTEGER NOT NULL            -- last byte moved in either direction
);
CREATE INDEX idx_swarm_up   ON swarm_traffic(up_bytes);
CREATE INDEX idx_swarm_down ON swarm_traffic(down_bytes);
CREATE INDEX idx_swarm_last ON swarm_traffic(last_at);
```

Notes on the shape:

- **A hash with no row is the normal case**: never transferred, zero traffic.
  The list LEFT JOINs, so absence reads as zeros. Nothing has to pre-create rows
  for a library of 40 000 files.
- **The table is append-and-increment only.** Every write is an UPSERT adding a
  delta, from one place (the flusher). Nothing else in the codebase writes here,
  which is what lets it be flushed in batches without a lock story.
- **Rows are never deleted by any other subsystem.** Removing a cached blob,
  trashing a recording or hard-deleting a file leaves the row: the bytes really
  did move, and a node's contribution history should not be erasable as a side
  effect of housekeeping. Only the explicit *Forget stats* action deletes one,
  and it says plainly that it lowers the node's all-time totals.
- **`wasted_bytes`** is what a torrent client calls wasted: bytes that arrived
  and were thrown away (a chunk that failed its sha256, an abandoned swarm
  attempt). Counted because it is the only visible symptom of a holder serving
  corrupt bytes, and because down-bytes that exclude it would understate what
  the mesh cost us.

**No provenance columns, no per-source rows here.** Which node served which
bytes is answered live (below), not accumulated per hash — that table's size is
hashes × peers and it would grow forever to answer a question that is almost
always about *now*. Who we traded with **in total** is a different table, one
row per counterparty rather than per pair:

### Migration 042 — `swarm_peer_traffic`

The companion to the F7 member quotas: those bound what a member *may* cost us,
this says what one *has*. Bounded by the size of the community, never by the
size of the library — which is exactly why it can exist while a per-pair table
cannot.

```sql
CREATE TABLE swarm_peer_traffic (
    public_key TEXT PRIMARY KEY,           -- ed25519 hex; '' = the unplaced bucket
    up_bytes   INTEGER NOT NULL DEFAULT 0, -- served to that node, all time
    down_bytes INTEGER NOT NULL DEFAULT 0, -- pulled from it, all time
    first_at   INTEGER NOT NULL,
    last_at    INTEGER NOT NULL
);
CREATE INDEX idx_swarm_peer_last ON swarm_peer_traffic(last_at);
```

- **Keyed by the public key, and it stores no name.** A node is addressed by its
  key everywhere in this codebase (the frontier rotation recycles source ids),
  and a heard name is a claim that changes; the page joins
  `federation_peers`/`federation_catalog_sources` at display time to get the
  current one. A node we have since forgotten shows as its key, which is the
  honest answer.
- **One `''` row is the unplaced bucket.** Friends and members arrive with a
  key (`serveAudienceKey` establishes it while resolving the audience); a guest
  or a token bearer arrives with only a mesh address, and every one of those
  gets folded into a single row. Persisting them per address was rejected: a
  keyed row set is bounded by the community, while an address-only one is sized
  by whoever chooses to talk to us — N forged keys, N rows — and it answers a
  question this table is not for. Dropping them entirely was rejected too: then
  the panel's figures silently fail to add up and nobody can see why.
- **No `wasted_bytes` column.** Waste is computed once per transfer, as
  received minus kept, and a transfer spans several holders — attributing it to
  one of them would be a guess.
- **This table and `swarm_traffic` are two independent ledgers of the same
  bytes**, written from the same drain in the same transaction, so their totals
  agree — until someone forgets on one side. *Forget stats* on a blob does not
  touch what a peer cost us, and *Forget* on a peer does not rewrite any blob's
  history. Neither figure is ever computed from the other, and the page never
  sums across them.

### Session and all-time, from two sources

Both were asked for (owner, 2026-08-06), and they come from different places
because they are different facts:

- **All time** = `SUM(up_bytes)`, `SUM(down_bytes)` over `swarm_traffic`. One
  indexed aggregate, survives restarts, and it is the *same* number the per-row
  column shows — so the strip and the list can never disagree.
- **This session** = the in-memory traffic table on the running node, since
  process start. Lost on restart, which is the point.

No separate node-total counters in `settings`. Two stores of one number is the
drift this doc spends its first section preventing; the SUM is authoritative and
cheap. The only artefact is that all-time lags by up to one flush interval,
which the strip closes by adding the un-flushed session delta it already has.

### Where the counting happens

Bytes are counted **in `federation`, in memory**; they are persisted **by
`api`**. That split is the same one the cache index made, for the same reason:
fetching and seeding must not require a database, and `federation` must not grow
an edge to `database`.

```
federation.Node.traffic          (in memory: per hash, per peer, per direction)
        │  DrainTraffic() → deltas, zeroing the pending half
        ▼
api swarm flusher (30 s ticker + shutdown)  →  swarm_traffic  (UPSERT … += )
```

- `Traffic()` snapshots the **session** counters (monotonic since start) — what
  the live view reads.
- `DrainTraffic()` returns the **un-flushed deltas** and zeroes only those. A
  crash loses at most one interval of accounting, which is the correct trade for
  never putting a DB write in a chunk-fetch loop.

**Outbound** is counted by a metered writer in `handleBlob`, wrapping the
response *unconditionally* (today's wrapper only exists when a cap is set). It
attributes bytes to `(hash, requester mesh address)`. Note what this makes
possible for free: "who is pulling from us right now", which is the seeding half
of the who-has-what question the cache doc parked here.

**Inbound** is counted by a metered reader around `resp.Body` in `fetchRange`
and `fetchFrom` — *wire* bytes, so a chunk that fails verification is still
counted (and additionally recorded as wasted). This is deliberately **separate
from `transferStats`**: that structure measures the swarm's *behaviour*
(failovers, stalls, per-provider chunks) and mesh tests assert on its exact
semantics. Traffic measures the wire. Keeping them apart means nothing in
`tests/mesh/` has to be re-read.

No `PeerStore` change anywhere in this design — so the standing "re-vet
`tests/mesh/` when `PeerStore` changes" gotcha does not fire.

---

## Rate limits

### Two knobs, node-wide, and no per-file ones

The whole limit surface is two numbers: **outbound (seeding) KiB/s** and
**inbound (fetching) KiB/s**, each `0` = unlimited.

**Per-file limits are deliberately absent** (owner, 2026-08-06). What a rate cap
protects is *the line*, and the line is shared by every transfer at once — so
the only cap that means anything is the sum. A per-file limit can merely
redistribute a budget the node-wide cap already fixes, and to do that it needs a
per-hash limiter table, a resolver reaching from the fetch path into the
database, and a three-valued editor on every row: a lot of machinery for a
question an operator does not actually have ("which of my files should be
slower?"). Fairness *between peers* is the real version of that question, and it
already has an answer — the F7 member quotas (`per_member_rate_kib`,
`member_rate_kib`), which bound what any one member and all non-friends together
may cost.

So `swarm_traffic` carries counters only, the row menu has no limits item, and
the modal is reached once, from the strip.

### Where they live, and why not on the settings page

Two layers, resolved **runtime override → config → unlimited**:

- **config** — `[federation] seed_rate_kib` (exists) and `fetch_rate_kib` (new,
  the inbound counterpart with the same `0`-means-unlimited default and the same
  negative-clamps-with-a-warning handling). This is the value a deployment ships
  with.
- **runtime override** — two `settings` keys, `swarm.up_rate_kib` /
  `swarm.down_rate_kib`, where an unset key means "inherit the config file". A
  rate cap is exactly the knob someone needs to change *while* something is
  saturating the link, so it must not require an edit-and-restart.

They are edited **on the swarm page and nowhere else** (owner, 2026-08-06). The
`/admin/settings` madnetwork card does not gain them, and this is the lens rule
doing its work: two pages may *show* the same rate, but only one may *set* it.
The page that shows you the traffic is the page where you throttle it — you are
already looking at the number the knob is about.

They also get their **own endpoint** — `GET/POST /api/admin/swarm/limits` — and
that is not tidiness. `setMadnetworkSettings` (`api/access_handlers.go:362`)
decodes `seed_enabled`, `seed_cache`, `hide_unavailable` and
`autoapprove_downloads` as **plain bools with hard-coded defaults**, so a client
posting only the rate fields would switch seeding *on* and autoapprove *off* as a
side effect. That handler's own comment records the last time this exact bug
shipped (`publish_friend_list`, silently disabled by every save of the seed
checkboxes). A second client is precisely the shape that trips it, so the rate
knobs get a write path that cannot reach the seeding policy at all.

### Which layers a knob gets (the rule, written down)

The `override → config → unlimited` arrangement is not the shape of every knob —
it exists exactly where BOTH of its layers earn their keep, and each layer has
one test:

- A knob gets a **config layer** when a deployment without this UI must still be
  able to ship the value: an embedder sets `cache_max_mb` in code (madplayer,
  2 GiB) and a headless node writes its caps into the TOML it deploys with.
- A knob gets a **runtime override** when an operator plausibly changes it while
  *watching* something — a saturating link (the rate caps), a filling disk (the
  ceiling). Anything with that property must not require an edit-and-restart.
  The override is three-valued because "inherit the config" and "0 = unlimited"
  are different states; that encoding (`unset ≠ 0`, key deleted to clear) has
  one spelling at the settings table, `database.optionalIntSetting` /
  `setOptionalIntSetting`, and every consumer resolves it the same way
  (`refreshLimits`, `ResolveCacheCeiling`, `QuotaOverrides.Resolve`).
- The `MadnetworkPolicy` switches are **runtime-only** (defaults hard-coded in
  `GetMadnetworkPolicy`): pure policy with a universal default and no embedder
  story — an embedder's sharing posture is `default_share_depth`, not a config
  twin of every checkbox.

Full three-layer knobs today: **seven** — `swarm.up_rate_kib`,
`swarm.down_rate_kib`, `madnetwork.cache_max_bytes`, and the four member quotas
below. An eighth should copy this section, not invent another spelling.

The rule has since been applied twice, and answered differently each time —
which is what a rule is for.

`madnetwork.cache_max_age_days` (built 2026-08-13) is **runtime-only**, because
the config test names no consumer — an embedder's bound on this cache is a size,
not an age. With no config layer there is nothing to inherit, so unset and 0 are
the same answer and the setting is a plain number rather than the three-valued
encoding above. Answering "two" is the rule working, not an exception to it.

The **four member quotas** (built 2026-08-14, owner's call) were the family this
section recorded as a disagreement between its own tests and the implementation:
config-only, fixed at Node construction, invisible at runtime. Both tests said
three layers. The config layer is real — a headless node ships its posture in the
TOML it deploys with — and so is the runtime one, because *a node being drained
by members is exactly an operator watching something*, and the four bounds are
what they would reach for. They now resolve `override → config → unlimited` like
the rates, through the same memo and the same resolver (below), and are edited in
the same modal. Nothing about the *policy* changed: all four still ship 0 =
unlimited, and friends still bypass them.

### The member budget

The four quotas (`federation-swarm.md` §"What a member may cost us") are on
`/admin/swarm` for the reason the rate caps are: the page that shows you the
traffic is the page where you throttle it, and here the page also names *who* is
spending it — the per-counterparty panel directly below the limits line is how an
operator notices a member worth bounding in the first place.

Three things about the implementation are worth not re-deriving:

- **They ride the rates' resolver and memo**, not their own
  (`WithLimitResolver`, one struct, one query per refresh). A second resolver
  would double the per-request cost of a knob family nobody sets, to keep two
  things apart that the same operator changes on the same page in the same modal.
- **The buckets are live.** `quotas.setLimits` reaches the `adjustableRate` of
  every requester currently being served, so a tightened cap binds the transfer
  that made the operator tighten it. Concurrency changes are not retroactive in
  the other direction: a lowered ceiling refuses the *next* admission rather than
  cutting a response already streaming, which is the same rule the rate caps
  follow.
- **The refresh happens at admission**, not only on the write path, because the
  concurrency halves of the budget are spent there. Without it a new ceiling
  would first bind at the first byte of the *next* response.

The readout (`GET /api/admin/swarm/limits`) reports all six knobs as
`{source, override, effective}` — the layer that answered, as well as the answer.
Before this existed, nothing could be asked at all: the quotas were built once in
`Start`, so an operator could not confirm what a running node enforced without
reading the TOML it had started with. Rates carry their unit in the field name
(`*_kib`); counts do not, and the one decoder reads the unit off the name so the
six share a write path without any of them telling a client the wrong thing about
itself.

### Making the limiters adjustable

`rateLimiter` gains `setRate(bytesPerSec)`, adjusting rate and burst under its
own mutex while **keeping accumulated tokens**. Rebuilding the bucket instead
would hand a full burst to anyone who nudged the slider, which is a rate limit a
requester could reset by making the admin fidget.

The Node keeps exactly two of them — one up, one down — whose rates are
re-resolved on a short memo (5 s) rather than per request. `seedableBlob`
already does a per-request policy read and this must not add a second. The
override arrives through **one injected resolver** (`WithLimitResolver`, wired
in `app.startMesh` to `db.GetSwarmRates` + `db.GetMemberQuotas`) because
`federation` stays DB-free; what the per-file design would have needed and this
does not is a per-hash limiter table and a `PeerStore` change — with per-file
limits gone, the whole knob surface is six numbers behind one option.

### The chains

Outbound, in `handleBlob` (order is irrelevant to the result — the effective
rate is the minimum — but fixed for readability):

```
node up  →  member class  →  per-member
```

The quota limiters (F7 item 6) are unchanged and still bypassed by direct
friends. **The node limit is bypassed by nobody, friends included**: it is a
statement about this node's own pipe, not about who deserves what.

Inbound, in `fetchRange` / `fetchFrom`:

```
node down
```

One bucket shared across the parallel chunk workers, which is exactly right: a
swarm fetch opens several Range requests at once and the cap is on their sum.

### Choosing a rate — and the floor below which the node stops being useful

This belongs in the doc *and* in the editor: the modal carries the short version
(the arithmetic, the suggestion, the floor warning), because a knob whose sensible
range lives only in a design document will be set wrong.

**What the outbound cap is for.** Not defense — bounding what a stranger may
cost this node is the member quotas' job (F7 item 6), and they do it per
requester and per class, which a single global number cannot. This knob exists
for the *household*: so seeding does not saturate an uplink that video calls,
other people, and your own listening share. A madplayer streaming from this node
reads through the same pipe as everything else.

**Size it against your uplink, not your download speed.** ISPs quote the
downstream figure; the one that binds here is upstream, typically 5–20× smaller
on cable or DSL. Convert once:

```
KiB/s  ≈  Mbit/s × 128          (so 20 Mbit up ≈ 2560 KiB/s)
```

Leave roughly a quarter of the uplink free for everything else — i.e.
`seed_rate_kib ≈ uplink Mbit/s × 96`:

| Uplink | ≈ KiB/s | Suggested cap |
|---|---|---|
| 5 Mbit (DSL) | 640 | 480 |
| 10 Mbit (cable) | 1 280 | 960 |
| 20 Mbit | 2 560 | 1 900 |
| 40 Mbit (fibre) | 5 120 | 3 800 |
| 100 Mbit+ symmetric | 12 800+ | `0` — the line is not the bottleneck |

**Do not set it small.** This is the warning the UI must carry, because the
second consequence is the one nobody expects:

- A stream needs the track's real bitrate, *continuously*: MP3 320 ≈ 40 KiB/s,
  CD-rate FLAC ≈ 110–125 KiB/s, hi-res FLAC ≈ 300 KiB/s and up.
- The cap is shared by **every concurrent serve**. Two listeners want twice
  that, and one *fetching* node is several parallel Range requests drawing on
  the same bucket by design.
- Under that floor, first playback stutters for anyone streaming from you. Then
  — the part that surprises people — **your node starts looking broken**: a
  fetching peer's stall watchdog fires on your slow responses, `worseThanPeers`
  de-ranks you, and the swarm fails over to another holder. You have paid for
  every byte you sent and the transfer finishes without you. A cap set too low
  does not make this node a small contributor; it makes it a rejected one.

So: **at least 4× the bitrate of what you mostly share, and not below ~256 KiB/s**
if more than one person might listen at once. If the uplink genuinely cannot
spare that, the honest setting is **seeding off**, not a starvation rate —
switching seeding off is a clear statement to the network, while 20 KiB/s is a
promise this node cannot keep.

**The inbound cap** answers a different problem: a bulk Materialize eating the
line while someone is on a call. It is the safer of the two to set, with one
caveat carried below — and if what you actually want is "fetch this later rather
than slower", that knob does not exist yet.

### Two hazards, named

**A throttled read looks like a stalled holder.** The chunk readers wrap their
body in `readStall`, whose watchdog fires after `Timeouts.ChunkStall` without
progress and counts a stall against the holder. Put a 50 KiB/s bucket inside that
and a 1 MiB chunk spends 20 s not progressing, and the swarm retires a perfectly
healthy peer. The answer: the throttle sits **inside** the watchdog (so
backpressure reaches TCP, which is the whole point of a rate limit), and the
reader **suspends the watchdog for the length of its own pause** —
`readStall` hands the reader a hook that stops the timer and returns the func
re-arming it (`pausingReader`). A limit we imposed is never evidence against a
peer.

It has to *bracket* the wait, not follow it. Resetting the timer after the sleep
was the first implementation and it failed the test immediately: one `Read` can
return a whole chunk buffer, whose tokens take seconds to earn, so the watchdog
fires in the middle of that single call and there is no "after" to reset from.

**Below ~9 KiB/s inbound, fetching stops working entirely.** `Timeouts.PerChunk`
(2 min) bounds one chunk fetch *including* time spent waiting on our own tokens,
and the bulk chunk is 1 MiB — so a cap under roughly 8.7 KiB/s cannot complete a
bulk chunk at all, and every chunk fails, retries and fails. The whole-file
fallback has the same shape against `Timeouts.Transfer` (30 min). This is far
below the ~256 KiB/s floor the guidance already gives, so an operator following
it never approaches the cliff — but the cliff is real and the number is worth
having written down.

**The inbound cap throttles listening to the mesh.** The streaming relay serves
from the same transfer the limiter is slowing, so a cap below the bitrate of what
someone is playing from another node makes it stutter — and unlike seeding, this
one is felt by the person sitting in front of the node. It is inherent (torrent
clients behave the same way) and it is **not** silently worked around: no "ignore
the limit while someone is listening" cleverness, which would make the limit mean
nothing at the one moment it binds. The modal says so, and the floor is the same
arithmetic as above — a fetch below the track's bitrate cannot be played as it
arrives.

Limits apply to **mesh traffic only**. Playing a cached file locally, the
`/files/*` server and the admin audio endpoint are never throttled: they are not
the swarm.

---

## The page — `/admin/swarm`

A routed admin sub-page (`adminSubPages["swarm"]`, `webui/html/admin/swarm.html`,
`webui/static/js/admin/swarm.js`), one nav link, one dashboard card.

### Summary strip

```
▲ 41.2 GB up · 12.8 GB down          all time
▲ 118 MB   · ▼ 2.4 GB                this session          [Reset…]
3 downloading · 22.4/58.1 MB · ▼ 1.2 MB/s      2 peers pulling · ▲ 340 KB/s
limits   up: 1 900 KiB/s (set here)   down: unlimited (from config)   [Change…]
         non-friends — 2048 KiB/s together · 4 transfers each (friends are exempt)
seeding: on · cache seeding: on      1 284 cached · 41.2 GB · 2 partials [Reclaim]
```

Every figure is already available or defined above. The limits line is the
**only** editor for any transfer limit anywhere in the UI, and it names where
each value came from (set here / from config / unlimited) so an operator can see
whether an override is in force without opening the modal. `[Change…]` posts to
`/api/admin/swarm/limits`; `[Reclaim]` calls the cache page's existing partial
reaper.

Its second line is the member budget, and it names only the bounds that are
actually set — four zeroes is the shipped default, so printing all four would be
wallpaper, and printing them as `0 KiB/s` would read as a node throttled to a
standstill. When none are set it says so, because "no budget" is a real answer an
operator may be checking for. Friends are named on the line every time: a reader
seeing "non-friends" throttled should not have to remember who that excludes.

The modal behind `[Change…]` is titled **Transfer limits** and holds both
families under their own headings — this node's line, then the member budget,
each with its own prose. Only an *override* prefills a field; a config value
shown in the box would be saved back as an override on the next Save, quietly
pinning a number the operator only ever read. It carries the sizing guidance from
§Choosing a rate in short form: the `Mbit/s × 128` conversion, the ¾-of-uplink suggestion, and the
floor warning — with a **live check** that flags a value below ~256 KiB/s (or
below 4× the bitrate of a typical file in this library) as likely to make the
node unusable rather than merely slow. It warns; it does not refuse. An operator
on a genuinely tiny uplink is allowed to make that choice knowingly, and the
modal points at the honest alternative — turning seeding off.

### Who we trade with

A disclosure under the strip, collapsed by default — the numbers matter when you
go looking for them, and a list of node names above the file list would be a
second page:

```
▸ Who we trade with — 7 nodes · ▲ 38.1 GB · ▼ 9.4 GB all time

  fiona's box        ▲ 21.4 GB  ▼ 2.1 GB   friend   active 3 min ago   [Forget]
  a-node-we-pull-from ▲ 6.2 GB  ▼ 5.9 GB   member   active 2 days ago  [Forget]
  9f3c1e…            ▲ 410 MB   ▼ 0 B      gone     last seen 12/07
  unnamed requesters ▲ 88 MB    ▼ 0 B      guests and listener devices
```

Rows are all-time from `swarm_peer_traffic`, plus the running node's session
delta so a peer pulling right now moves without waiting for a flush — the same
add the file rows already make. **Name and class are resolved at read time**
from the peer and source tables: `friend` (a `federation_peers` row in that
state), `member` (a `federation_catalog_sources` row — a node the sweep reached
through the community), or `gone` (neither: a former friend, or a node the
frontier rotation has since evicted). A `gone` row keeps its bytes: what a node
cost us does not stop being true when we forget who it was.

The bucket row is always last, and its `[Forget]` drops the whole aggregate:
there is nothing selective to do inside it, because the requesters it counts were
never told apart in the first place.

This is the **only** home for these figures. `/admin/network` shows the same
nodes as trust decisions (accept, rename, block, the map); adding a byte column
there would put one number under two owners, and the F7 quotas it pairs with are
`madshare.toml` config with no UI at all today. Traffic belongs on the traffic
page.

### The switcher

Three values, as asked: **All · In library · Cached**. It is a scope on the
query (`scope=all|library|cache`), not a client filter, so counts and select-all
mean what they say. Secondary filter pills refine within it — *sharing* (seeding
now / not shared / review / trashed) and *active* (transferring now) — following
the recordings lens's pill idiom.

### A row

```
[▸] Some Track — Some Artist               ████████████░░░░  74%
    cached · Madnetwork · 4.2 MB      ▲ 118 MB  ▼ 4.2 MB   ▼ 890 KB/s   [⋯]
```

- **Progress bar** — the graphical fill asked for. 100 % for any complete blob
  (a `<hash>` file is verified before the rename, so completeness is binary);
  live `progress/size` for a running fetch; for an **abandoned** partial, bytes
  on disk over the size the catalog claims for that hash, or an indeterminate
  bar when nothing claims it.
- **Chips** — provenance (library/cached), sharing scope, state.
- **Traffic** — up / down, all time, plus a live rate while moving.
- **`[⋯]`** — the row menu, keeping the row visually quiet as asked.

Row menu (`row-menu.js`), gated as noted:

| Item | Rows | Gate | Notes |
|---|---|---|---|
| Play | all | `file.delete` | library → the ladder-best rendition; cache → `/api/admin/cache/{hash}/audio` |
| Info | all | — | expands the panel below |
| Materialize | cache | `file.upload` + a running node | existing `POST /api/madnetwork/download` |
| Download | all | `file.delete` | to the device; existing endpoints |
| Open in library | library | — | `/admin/library#recordings-<id>` |
| Holders… | all | `file.delete` | reuses the cache page's claims endpoint, widened |
| Remove from cache | cache | `file.delete` | `POST /api/admin/cache/bulk` — the one remover |
| Forget stats | all | `user.manage` | modal; says it lowers the node totals |

**Library files are never deletable here** (owner). "Open in library" is the
answer, and it is a better one than a hidden button: deletion of our own content
has a curation surface with the recording context that makes it a safe decision.

### The info panel

The "see info about the file" half. Static facts (hash, byte size, filename,
tags, mime, provenance, scope, added/fetched), traffic (up/down/wasted, all time
and session, ratio when down > 0), and — while a transfer runs — the live detail that is free because the swarm
already keeps it: mode (`swarm`/`whole`), chunks done/total, TTFB, retries,
failovers, stalls, corrupt, **per-provider bytes**, and a **piece map** rendered
from the transfer's `chunkOK` bitmap. Plus **holders**: who advertises this hash
now (`MadnetworkBlobProviders` + the cached catalogs), and who is pulling it
from us right now (the outbound traffic table).

### Partials

`.part` files are in no table, and they must not enter one: the cache doc's rule
is that unverified bytes are never described, advertised or retained. So they
are not part of the paged SQL query. They render as a **pinned block above the
list** — live transfers first (the thing an operator is watching), then
abandoned ones, capped at 200 with a "…and N more" line pointing at Reclaim.
Sources: `ActiveTransfers()` and one directory walk, the same pair the cache
summary already does.

### Live updating

One small poll, `GET /api/admin/swarm/live`: node counters, active transfers,
current outbound peers, and the traffic deltas of the hashes that are moving.
Every **2 s** while anything is active, **10 s** when idle, **paused when the
tab is hidden**. It patches the strip, the pinned block and the traffic cells of
*visible* rows by hash — it never re-renders the list, which would fight the
virtual window and lose the selection.

### Why a bespoke module rather than a `file-list.js` scope

The cache page is a `file-list.js` scope and that was right. This one is not,
for three reasons the component cannot absorb without becoming everyone's
problem: rows carry a **progress bar and a live-updating rate cell** (the scope
contract renders static columns from one page fetch); the list is a **union of
two origins** with per-origin actions in one selection; and a **2-second patch
loop** into windowed rows is a different rendering model from "fetch a page,
render it". Bloating a component five surfaces share, to serve one, is the
trade this avoids.

It is bespoke in the same sense `admin/recordings.js`, `admin/moderation.js` and
`admin/duplicates.js` are — a page module, not a new framework — and it reuses
everything that already exists: `virtual-list.js` (windowing), `row-menu.js`
(the ⋯ menu), `toast.js`, the shared player core + player bar, the admin bulk-bar
and table CSS, `share-depth.js` (scope vocabulary and chips), the modal-confirm
convention, and the cache page's endpoints for every action they share. New CSS
is the progress bar and the piece map, and nothing else.

---

## API

Under `/api/admin/swarm` (admin group, already behind `file.delete`):

```
GET  /api/admin/swarm?scope=&filter=&q=&field=&sort=&limit=&offset=
     → {ok, total, selectable_total, bytes, items:[…]}

GET  /api/admin/swarm/summary
     → {ok, all_time:{up,down,wasted}, session:{up,down,wasted},
        limits:{up:{value,source}, down:{value,source}},
        active:[…], peers_out:[{key,name,rate}],
        seeding:{enabled,cache}, cache:{entries,bytes,partials:{count,bytes}},
        federation: true|false}

GET  /api/admin/swarm/live
     → {ok, session:{…}, active:[{hash,size,progress,rate,mode,chunks,
        chunks_done,chunk_map}], peers_out:[…], rows:{<hash>:{up,down,rate}}}

GET  /api/admin/swarm/{hash}
     → the info panel: facts, traffic, holders, live transfer detail

GET  /api/admin/swarm/limits
     → {ok, up:{value,source}, down:{value,source}, config:{up,down}}
POST /api/admin/swarm/limits            {up_kib?: null|0|N, down_kib?: …}
     → {ok, up, down}       null = inherit config, 0 = unlimited,
                            absent = unchanged (gate: user.manage)

POST /api/admin/swarm/stats/forget      {hashes|filter+all}

GET  /api/admin/swarm/peers
     → {ok, peers:[{key, name, kind:"friend"|"member"|"unknown", up_bytes,
        down_bytes, first_at, last_at, session:{up_bytes,down_bytes}}],
        unplaced:{up_bytes,down_bytes,first_at,last_at,session:{…}},
        totals:{up_bytes,down_bytes}, federation: true|false}
POST /api/admin/swarm/peers/forget      {keys:[…]|all:true}
     → {ok, forgotten:N}    (gate: user.manage; "" forgets the bucket)
```

The two rate fields are three-valued for the same reason `share_depth` is —
absent ≠ `null` ≠ a number — and use the same technique: `json.RawMessage` per
field, decoded in a second pass (`api/share_depth.go`). This is the *only*
place that shape survives, now that per-file limits are gone.

Sorts: name · size · **uploaded** · **downloaded** · ratio · last active ·
newest. Every order ends in `hash`, so paging is stable when the leading key
ties — the same rule the cache listing already follows.

`stats/forget` — the one endpoint here that takes a set — carries the standing
guardrail: an empty filter is refused without `"all": true`.

**Materialize, Download, Remove, Holders and partial reaping mint no new
endpoints** — they are the cache page's, per the boundary rules.

### With federation off

The page works: rows list, all-time traffic reads from the table, limits can be
set (they apply when a node next runs), the cache is playable and removable
through its own endpoints. What disappears is what genuinely needs a node —
`active`, `peers_out`, live rates, holders, Materialize — reported by the
summary's `federation: false` so the page omits those controls rather than
offering ones that 404. Same discipline as `/admin/cache`, and it was learned
the same way there.

---

## Permissions

No new permission.

- Viewing, playing, downloading, removing cache blobs — `file.delete`, which the
  admin route group already requires.
- Setting the node's rate limits and forgetting stats — `user.manage`, matching
  every other runtime madnetwork policy knob.
- Materialize keeps its own existing gates (`file.upload` + `madnetwork.access`).

---

## What this touches

| Existing thing | Change |
|---|---|
| `handleBlob` | metered writer (always on); the node up limiter becomes adjustable |
| `fetchRange`, `fetchFrom` | metered reader + the node down limiter (new — nothing throttled inbound before) |
| `readStall` | suspends its watchdog around a wait on our own tokens (`pausingReader`) |
| `rateLimiter` | gains `setRate` (token-preserving); `adjustableRate` wraps one live cap |
| `Node` | traffic table, `Traffic()`, `DrainTraffic()`, `SwarmRates()`, `MemberQuotas()`, `WithLimitResolver` |
| `api.FederationNode` + `federation/node_stub.go` | the four new methods (and the `nofederation` stub must satisfy them) |
| `MadnetworkPolicy`, `/api/admin/settings/madnetwork`, the `/admin/settings` card | **unchanged** — the rate knobs deliberately do not go there |
| `config.FederationConfig` | `fetch_rate_kib`, plus the config example and `configuration.md` (with the sizing guidance in the comment) |
| `PeerStore`, the catalog, gossip, quotas | **unchanged** — nothing to re-vet in `tests/mesh/`. Mig 042 reads `federation_peers`/`federation_catalog_sources`, but only in a LEFT JOIN on the way out: it stores no foreign key and neither table learns of it |
| `/admin/cache` and its endpoints | **unchanged**; the swarm page calls them |
| `database_test.go` | migration count/table assertions for 041 and 042 (standing gotcha) |
| `api` `fakeRepo` / federation fake | new `Repository` and node methods (standing gotcha) |
| `madnetwork-cache.md` | its "belongs to a future swarm page" decision row points here |
| `CLAUDE.md`, `README`, `docs/ui/shells.md` | the new admin page in the route-group lists |

---

## Build plan

1. **Accounting** — ✅ built. `swarm_traffic` (mig 041) + the store methods; the Node's
   traffic table with `Traffic`/`DrainTraffic`; metered writer in `handleBlob`
   and metered reader in both fetch paths; the api flusher (ticker + shutdown).
   Tests: served bytes land against the right hash and peer; wire bytes count a
   chunk that later fails verification, and it also lands in `wasted`; a drain
   returns each byte exactly once and never zeroes the session view; a crash
   between drain and commit loses at most the drained window. Also verified end
   to end over a real two-node mesh rather than through the wrappers alone: both
   nodes account the same transfer and each identifies the other by public key.
2. **Limits** — ✅ built. `setRate`; the node up/down limiters with their policy memo;
   the inbound limiter in both fetch paths (new machinery — nothing throttled
   downloads before); the `readStall` discount; `fetch_rate_kib` and the two
   settings keys. Tests: resolution (override → config → unlimited) including
   `0` overriding a configured cap; a throttled fetch records **no stall**;
   friends bypass the quotas but not the node limit; adjusting a rate
   mid-transfer neither resets the bucket nor bursts; the inbound cap actually
   binds the *sum* of the parallel chunk workers, not each of them. The watchdog
   test failed on the first implementation and produced the bracketing rule now
   in §Two hazards.
3. **API** — ✅ built. the listing (union, grouped by hash, scoped and paged), summary,
   live, detail, the limits GET/POST, forget stats. Tests: the envelope and
   `selectable_total` agree with the page under every scope and filter; a hash
   in both library and cache yields **one** row with both flags; an absent rate
   field changes nothing while `null` reverts to config; the empty-filter
   guardrail; federation-off answers without a node; **posting limits leaves
   every seeding setting untouched** (the regression this endpoint split exists
   to prevent).
4. **Page** — ✅ built. The module, switcher, state pills, rows with progress
   bars, pinned transfers, the ⋯ menu, the info panel, the limits modal with its
   floor warning, the live poll, the dashboard card and nav link.

   Verified in a real browser against a running server holding 60 library files
   and 130 cached blobs — not by reasoning about the markup. That is what caught
   the three defects below, none of which any unit test was going to find:

   | Found by running it | Why it mattered |
   |---|---|
   | `since` serialized as `-62135596800` with federation off | the zero `time.Time`; the strip would have dated the session to the year 1. Now omitted rather than stamped. |
   | The summary reported the config cap while a stored override said otherwise | `SwarmRates` read whatever the last *blob request* had left behind, and nothing but a transfer ever resolved. So the page lied — and worse, a cap set here would not have applied until the next transfer. `SwarmRates` now resolves before answering, and a write calls `RefreshRates` to take effect at once. |
   | Untagged cache rows printed their hash twice, and a filtered empty list claimed "no files in the library yet" while sixty sat behind the filter | both only visible on a real cache, where most rows carry no tags at all. |

   Tests: the page's judgement calls — the text decisions no Go test reaches —
   are `tests/js/swarm-page.test.mjs`, which lifts each function's shipped source
   out of `swarm.js` (it cannot be imported: page DOM at module scope) and pins
   the two defects above plus the reason-not-seeding order (row facts before
   node switches), the query builder's absent-vs-empty `q`, and `0 KiB/s`
   reading as *unlimited* rather than as a throttled node. Server-side, the
   omitted `since` and the sort whitelist — every order, its tie-break on hash,
   and an unknown token falling back to the default — are in
   `api/swarm_handlers_test.go` and `database/swarm_list_test.go`.

   A second pass with the page in front of us found two more, both style:
   the search box and the sort dropdown were **unstyled native controls** (a
   white select on a dark page), and every row's grid sized its own chips
   column, so the bars and byte counts sat at a different x on each row. Fixed by
   the shared `.admin-search` / `.admin-select` in `admin-shell.css` (see
   `docs/ui/shells.md` §Admin shell) and a fixed 19rem chips track. Unclassed
   prose links were UA blue on every admin page, and are now styled with them.
5. **Persisted per-peer totals** — ✅ built (2026-08-07). `swarm_peer_traffic`
   (mig 042) + the second pending map and `DrainPeerTraffic`; both ledgers
   written from one drain in one transaction; `GET /api/admin/swarm/peers` and
   its `peers/forget`; the disclosure on the page. Tests: both halves land from
   one flush and a peer-only drain still writes; increments never assignments;
   unplaced deltas fold into one bucket that sorts last; the identity join
   (local name over heard name, friend / member / blocked / gone) and that a
   node nothing knows any more keeps every byte; the un-flushed counterparty
   still appears; forgetting either ledger leaves the other standing; the
   empty-selection guardrail. Verified in a browser against a seeded node:
   `[Forget]` removed a row, the panel re-totalled, and the strip's all-time
   figure — the other ledger — did not move.

Steps 1–5 are the deliverable, and are done.

---

## Decisions

| Date | Decision | Why |
|---|---|---|
| 2026-08-06 | **`/admin/cache` stays; the two pages are lenses, not duplicates.** | Owner's framing, and the better one: the page follows the admin's *job* — configure the cache on one, configure how the node works in the swarm on the other — exactly as `/admin/library`'s three lenses do over one body of content. I had read the shared rows as duplication; overlap in the *view* is what a lens is. |
| 2026-08-06 | **A lens may duplicate a view, never an editor.** | The rule that falls out of the framing above, and it decides the rest of the design in one line: both pages show cached blobs, but only one page changes any given thing. It is also why the rate knobs are here and not also on `/admin/settings`, and why the three sharing rules (one remover, one cache index, one retention UI) are build requirements. |
| 2026-08-06 | **The list is every blob with bytes — drafts and trashed included.** | Owner call. They occupy the disk and the page claims to say what this node holds; each carries the state chip that explains why it is not moving, instead of being silently absent. |
| 2026-08-06 | **No per-file rate limits.** Two node-wide knobs, up and down, and nothing else. | Owner call, reversing the first draft. A cap protects the *line*, which every transfer shares, so the node-wide sum is the only figure that means anything; a per-file cap only redistributes it, and needs a per-hash limiter table, a DB resolver in the fetch path and a per-row editor to do so. Fairness between peers — the real version of that question — is already the F7 member quotas' job. |
| 2026-08-06 | **The node's caps become runtime knobs, edited on the swarm page and nowhere else.** | Owner call. A rate cap is the setting one needs to change *while* the link is saturated, so it must not need a restart; and a knob in two places is a knob whose copies eventually disagree, so the `/admin/settings` card does not get it. |
| 2026-08-06 | **They get their own endpoint rather than riding `/api/admin/settings/madnetwork`.** | Found reading that handler: `seed_enabled`, `seed_cache`, `hide_unavailable` and `autoapprove_downloads` decode as plain bools with hard-coded defaults, so a second client posting only rate fields would switch seeding on and autoapprove off. Its own comment records the last time that bug shipped. |
| 2026-08-06 | **The sizing guidance ships in the UI, not only in this doc** — conversion, ¾-of-uplink suggestion, and a warning below ~256 KiB/s. | Owner asked for it. A too-small cap does not make a small contributor, it makes a *rejected* one: peers' stall watchdogs fire, `worseThanPeers` de-ranks the node, and it pays for bytes that end up wasted. The modal warns and offers the honest alternative (seeding off) but never refuses — the operator's line, the operator's call. |
| 2026-08-06 | **Both session and all-time traffic**, from two sources: memory and `SUM(swarm_traffic)`. | Owner call. No separate node-total counters — two stores of one number is the drift this doc exists to prevent. |
| 2026-08-06 | **Counting lives in `federation` (memory), persistence in `api`.** | The cache index's split, for the same reason: transferring must not require a database, and `federation` must not gain an edge to it. Also keeps `PeerStore` untouched, so `tests/mesh/` needs no re-vet. |
| 2026-08-06 | **Traffic accounting is separate from `TransferStats`.** | They measure different things — the wire vs. the swarm's behaviour — and mesh tests assert on the latter's exact semantics. Counting wire bytes there would change what those assertions mean. |
| 2026-08-06 | **The stall watchdog discounts time blocked on our own rate limiter.** | Otherwise a throttle retires healthy holders: `readStall` cannot tell "slow peer" from "we are holding the tokens". A limit we imposed is never evidence against a peer. |
| 2026-08-06 | **The inbound cap throttles listening to the mesh, and is not worked around.** | Silently ignoring a limit while someone is listening would make it mean nothing at the one moment it binds. The modal says so and gives the floor. |
| 2026-08-06 | **Library files are not deletable here.** | Owner call. Deletion of our own content stays where the recording context makes it a safe decision; the row links there. |
| 2026-08-06 | **Partials are shown but never indexed.** | The cache doc's rule holds: unverified bytes are not described, advertised or retained. They render as a pinned block from `ActiveTransfers()` plus one directory walk. |
| 2026-08-06 | **Bespoke page module, not a `file-list.js` scope.** | Progress bars, a 2-second patch loop into windowed rows, and a two-origin union with per-origin actions are three things the scope contract cannot express. Everything else is reused: `virtual-list`, `row-menu`, `toast`, the shared player, the bulk bar, `share-depth`. |
| 2026-08-06 | **`swarm_traffic` rows outlive the blobs they describe.** | The bytes really moved; a node's contribution history should not be erasable as a side effect of clearing a cache. Only *Forget stats* deletes one, and it says what that costs. |
| 2026-08-06 | **Per-peer totals are live-only for now**, persisted history deferred. | The common question is "who is pulling from us right now", which the in-memory table answers for free. A `hashes × peers` table is a growth commitment that should wait for a real want. *(Superseded 2026-08-07 — the want arrived, and the table that answers it is per counterparty, not per pair.)* |
| 2026-08-07 | **Persist per-peer totals (mig 042), one row per counterparty.** | Owner call: step 5. It is the companion to the F7 member quotas — those bound what a member *may* cost us, and nothing said what one *had*. The size objection that deferred it applies to `hashes × peers`, not to this: one row per node in the community, which is bounded by the community. |
| 2026-08-07 | **Unplaced requesters get one shared bucket row, not a row each.** | Owner call. A keyed row set is bounded by the community; an address-keyed one is sized by whoever chooses to talk to us, so N forged keys would be N rows — the sybil shape the class quotas exist to answer. Dropping them silently was the worse alternative: the panel's figures would fail to add up with nothing on screen explaining it. |
| 2026-08-07 | **The panel lives on `/admin/swarm` only.** | Owner call. Traffic belongs on the traffic page, beside the live per-peer view it extends and the rate knobs it informs. `/admin/network` owns the same nodes as *trust decisions*; a byte column there would put one number under two owners, and the quotas it would sit beside have no UI at all today. |
| 2026-08-07 | **No name column — the key is the identity, the name is joined at read time.** | Everything else here addresses a node by its public key for the same reason (heard names are claims, and the frontier rotation recycles source ids). A node we no longer hold any row for shows as its key and keeps its bytes. |
| 2026-08-07 | **Blob history and peer history are independent ledgers; forgetting one never rewrites the other.** | They answer different questions, and neither is derived from the other. Making *Forget stats* on a blob also debit its peers would need per-pair rows — the table this design refuses — so the honest alternative is two ledgers, a Forget on each side, and a page that never sums across them. |
