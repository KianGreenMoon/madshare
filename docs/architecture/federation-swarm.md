# Madnetwork distribution — direct transfer and the swarm

> **How bytes move between nodes.** Part of the federation design; the spine, the
> vocabulary and the build plan are in [`federation.md`](federation.md), who may
> fetch what is in [`federation-access.md`](federation-access.md), and the
> operator's view of live transfers is [`swarm-admin.md`](swarm-admin.md).
>
> **Status:** everything here is built — F3 (direct transfer), F4 (multi-source
> chunk fetch), the F7 per-member quotas, and **F9 complete**: partial seeding,
> the holdings announce, the scheduler and the endgame. F10 (merkle verification)
> is decided and parked behind two triggers.

## Direct transfer (F3, built)

- **Wire = plain streaming HTTP with Range** (decision 2026-07-18): `GET
  /madnetwork/v0/blob/{hash}` on the mesh, served via `http.ServeContent`
  (native HEAD/Range; `Content-Disposition` carries the origin filename so a
  download lands under its real name). Between two trusted endpoints, "chunked"
  IS HTTP ranges; **integrity is the content hash itself**, verified over the
  full byte stream on the fetching side — bytes that do not hash to the
  requested hash never enter the cache. The Merkle chunk protocol is deferred to
  F4, where multi-source fetch actually needs per-chunk verification.
- **Authorization** (decision 2026-07-18): **a friend may fetch any blob its own
  catalog shows it** — never advertise what you won't serve, and vice versa.
  Published = the same predicate as the local library (live file + an approved
  appearance on its recording); a staged, trashed, or unknown hash is 404 even
  for a friend. Since F5 that predicate is evaluated **for the requester's
  audience** (`federation-access.md` §Sharing scope): the recording's scope and
  the per-friend user mapping filter the catalog and the byte endpoints from the
  same rule. F5 additionally served a guest-accessible recording to any mesh
  node (the open swarm); F7 withdraws that and answers a **member** of our
  component with the Madnetwork-scoped set instead.
- **Fetching** (`federation.Node.EnsureBlob`): one transfer per hash, joined by
  every concurrent requester; providers come from the cached catalogs (friends
  advertising the hash, most recently seen first — tried in order until one
  delivers verifying bytes). A hash the local library holds short-circuits to
  the local blob; a finished cache file is a cache hit. Fetches run on the
  node's lifetime, not the requester's — a browser disconnect never abandons a
  half-fetched file. Cache: `<data_dir>/cache/madnetwork/<hash>` (`.part` while
  running, renamed only after verification; no eviction in v1).
- **Cache-through streaming relay** (`GET /api/madnetwork/stream/{hash}`, gated
  `madnetwork.access`): bytes are relayed to the browser as they arrive while
  the complete file lands in the cache in parallel — never
  download-fully-then-play. The total is known up front (the manifest / the
  origin's Content-Length), so browser range requests work against the growing
  file. Reads are **per-chunk, not front-to-back**: a range for a region not yet
  fetched (a player's tail probe for the MP4 `moov`/duration, or a seek)
  **prioritizes the chunk covering that offset** and is served as soon as it
  lands — it does not wait for the sequential prefix to reach it (see
  §Distribution for the seek-priority mechanism).
- **Download to library** (`POST /api/madnetwork/download {hash}`, gated
  `madnetwork.access` + `file.upload`): fetch + stage, exactly as designed in
  `federation.md` §Catalog — the verified file lands in blob storage and
  inserts as the downloader's **draft** carrying the remote entry's tagset text
  (what the user saw and chose; the origin filename is kept). The existing
  analysis pipeline then ffprobes and fingerprints it **locally** and resolves
  its recording — remote claims stay hints. Bytes the library already holds
  skip the fetch: the remote tagset attaches as a new draft appearance of the
  held recording. The **`autoapprove_downloads`** setting (settings key
  `madnetwork.autoapprove_downloads`, admin card on `/admin/settings`, gated
  `user.manage`, default **off**) lands downloads approved as fetched instead.
  Progress is polled at `GET /api/madnetwork/transfers/{hash}`; the download job
  (dedup per hash) survives the requester.

## Distribution (the swarm, F4 built)

- **Swarm ID = content hash.** Blobs are already content-addressed; two
  independently uploaded identical files hash identically and are automatically
  seeders of the same swarm — no coordination, no `.torrent` files. Different
  encodings of the same audio are different swarms; the recordings overlay
  (`recordings.md`) chooses which rendition — which swarm — to fetch.
- **Chunk protocol: a lean chunk-exchange over ygg** (built F4), not the
  BitTorrent wire protocol/DHT — we control both endpoints. A holder serves an
  **on-demand manifest** (`GET /madnetwork/v0/manifest/{hash}`): the total size,
  the bulk **chunk size**, a small **lead-ramp** (`lead_sizes`), and the ordered
  per-chunk SHA-256 list. The layout is **adaptive + self-describing**, so a
  fetcher never assumes it and the sizing policy can change without a protocol
  break (decision 2026-07-18, resolves former open question 1):
  - the **bulk chunk size** scales with the file up to a **1 MiB cap** — the
    cap is deliberately modest because it doubles as the **seek granularity** (a
    seek into an un-fetched region waits for the one chunk covering it);
  - a **lead ramp** of small chunks (256 KiB doubling up to the bulk size)
    precedes the bulk, so the **first byte** of a stream — and the first byte
    after a seek to the front — is ready after a *small* chunk regardless of
    file size, while the bulk stays efficient and manifests stay bounded for
    huge files. Older nodes that predate the ramp see a chunk count that doesn't
    match a uniform layout, reject the manifest, and fall back to the whole-file
    fetch — a clean degrade.

  Because the swarm id is a flat SHA-256 of the whole file (not a Merkle root
  — it is the same content address used everywhere), the manifest's chunk
  hashes are not cryptographically bound to it; they enable **early per-chunk
  verification and bad-chunk re-fetch**, while the **assembled whole-file hash
  remains the authoritative anchor** (verified before a blob enters the cache).
  A lying manifest therefore costs bandwidth and a failed transfer, never the
  wrong file. F4 accepted that on the grounds that "every holder is trusted",
  which was written when the swarm was direct friends only; **F7 widened it to
  the whole community and the sentence did not move with it**, so since F9 item 3
  a manifest is taken only when **two holders describe the blob the same way**
  (`fetchAgreedManifest`) and a chunk that fails against it no longer condemns
  its sender on the word of whoever supplied the reference. Chunks are fetched
  with plain HTTP Range requests (the F3 blob endpoint already serves them).
- **Multi-source fetch, sequential-priority + seek** (built F4): chunks are
  dispatched lowest-index-first (so the streaming prefix grows in order) but
  fetched by a small worker pool **in parallel across all advertising holders**.
  The transfer tracks **per-chunk readiness**, so the relay can serve an
  out-of-order region the instant its chunk lands; a streaming read of a
  not-yet-fetched offset **promotes the covering chunk to the front of the
  dispatch queue** (seek-priority), which keeps a tail probe or seek from
  waiting out the whole file. Failed chunks are re-queued with a resilient
  policy: a **corrupt** chunk (wrong bytes) retires its holder immediately —
  wrong bytes are evidence about the holder, and no amount of bad luck produces
  them — while a **transient** error (an unreachable/stalled mesh path) is
  weaker evidence, because it describes the holder *and* the moment. So
  retirement is **relative**: a holder is retired once it is a
  consecutive-failure limit worse than the **best live holder** (streaks reset
  on any success). When some peer is still delivering, that is an absolute limit
  exactly; when every holder is equally deep in failures the fetch is in a bad
  moment rather than facing a bad holder, and none is retired. A *sole* holder
  has nothing to be compared against, so the plain limit applies and the fetch
  still ends. Retiring holders is deliberately **not** how a hopeless fetch
  stops — each chunk carries its own **attempt budget**, and exhausting it
  aborts the transfer with every holder still live. Conflating the two is a trap
  worth naming: when the only way to end a fetch is to kill every source, a
  perfectly healthy source gets declared faulty to make the transfer terminate.
  A hung connection is caught by an **idle-read watchdog** (~20 s with no bytes)
  plus a response-header timeout, rather than waiting out the whole per-chunk
  backstop — so a Yggdrasil path stall costs seconds, not minutes. A
  **single-seeder swarm degenerates to a direct transfer**, and a holder too old
  to speak the manifest endpoint triggers a **fall-back to the F3 whole-file
  streaming fetch** — so F4 nodes still fetch from F3 nodes.
- **Fast first byte** (built F4): to avoid two serial mesh round-trips before
  playback starts, a fetch **overlaps the manifest probe with a speculative
  chunk-0 fetch** — chunk 0's byte range is derived from the advertised size
  via the deterministic layout (so with the lead ramp the speculative fetch is a
  *small* chunk), then confirmed and per-chunk-verified once the manifest lands
  (dropped if the guess was wrong). Manifest probes and chunk fetches share
  **one pooled mesh connection**, so chunk fetches reuse the manifest's warm
  path instead of paying a fresh handshake; a manifest probe is bounded (20 s)
  so a slow holder cannot stall the transfer. Net effect: first byte after ~one
  small chunk + a round-trip rather than a full bulk chunk.
- **Tracker = the catalog + holdings** (built F4). "Who has hash H" is the union
  of two sources: nodes whose **published catalog** advertises the hash as a
  rendition (their library — already synced in F2), and nodes advertising it
  in their **download cache** via `GET /madnetwork/v0/holdings` (a flat hash
  list of what a node will seed, pulled on the same refresh cadence as the
  catalog and cached in `federation_holdings`). The library is already in the
  catalog, so holdings carries only the cache — this is what makes a
  **downloaded blob a discoverable seeder** and lets a popular track spread as
  the community fetches it. Providers are tried most-recently-seen first; no
  DHT.
- **Only nodes swarm.** Thin clients never talk to peers (see
  `federation-access.md` §Principals).
- **The swarm's only boundary is the madnetwork** (declared 2026-07-31). Inside
  the community every node is a peer of every other for distribution purposes:
  same blobs, same manifests, same holdings, same seeding, whether the holder is
  a direct friend or a node five friendships away. Distribution is where "one
  community, one boundary" is most load-bearing, because the whole point of
  content addressing is that *which* holder answers should not matter — a
  swarm that could only draw on direct friends would fail a fetch that the
  community as a whole could trivially satisfy. Direct friendship still buys one
  thing: content an admin restricted to it (`DepthFriends`), which is a
  publishing choice, not a distribution tier.
- **Authorization in the swarm:**
  - Between **direct friends**, the channel identity is sufficient — no tokens
    (F4), filtered by the requester's audience since F5.
  - To a **member of our community** (F7) — `federation-access.md` §The
    membership rule — blob, manifest, **holdings and cache** service all
    answer, for **Madnetwork-scoped** content, which under the shipped default
    is the whole published library. No token: membership is a lookup, and the
    scope already says "our community". The Madnetwork-scoped *catalog* is
    served too (`federation.md` §Discovery beyond the friend ring).
  - To an **outsider** — any node we cannot place in our community —
    nothing, and 404 rather than 403, so a hash's existence is never confirmed.
    *Here* only: the catalog answers an outsider with a plain 403, because that
    request names no hash and so has nothing to confirm (`federation.md`
    §Discovery beyond the friend ring). (Together these replace the token-gated
    depth ≥ 1 tier *and* F5's guest-playable open swarm; see
    `federation-access.md` §Sharing scope.)
  - A **listener node** presents a **capability token** — its home server's
    signed "bearer key K is mine until T", which any node that can place the
    *issuer* in its own community honours. It buys the bearer the **member**
    audience and never the issuer's direct-friend reach, and the bearer draws on
    the member quota rather than a friend's exemption (`federation-access.md`
    §"The capability token").
- **Seeding policy** (built F4): everything a node holds — library and
  listen-cache — seeds by default to the whole community ("who cares" is the
  default privacy stance at node granularity; the cache reveals only that
  *someone on this node* listened). Controls: `seed_enabled` (master on/off —
  off refuses all blob and manifest service, the node consuming without serving)
  and `seed_cache` (whether the download cache is served **and** advertised in
  holdings), both runtime DB settings on `/admin/settings` defaulting **on** —
  `seed_cache` is the switch for an operator unwilling to re-serve content this
  node did not publish; plus the node's two **rate caps** — `[federation]
  seed_rate_kib` over the blob-serve write path and `fetch_rate_kib` over the
  fetch read path (`0` = unlimited). Since the swarm admin page
  (`swarm-admin.md`) both are *defaults* rather than fixed values: the settings
  keys `swarm.up_rate_kib` / `swarm.down_rate_kib` override them at runtime,
  resolved override → config → unlimited, and the buckets are adjusted in
  place so a change keeps its tokens. Nobody bypasses these two, friends
  included — they describe this node's pipe, not who deserves what, unlike the
  member quotas below. A throttled read **suspends** the idle-stall watchdog for
  the length of its own pause, because a limit we imposed must never be counted
  against a holder.

  **A cached blob is never also a library blob** (fixed 2026-08-02). The two
  branches of `seedableBlob` answer under different rules — the library branch
  applies the recording's sharing scope, the cache branch answers the whole
  community — so a hash present in both would be served under whichever rule
  is looser, and narrowing a recording would silently fail to stop this node
  seeding it. Rather than teach the cache branch about scope, the duplicate is
  deleted: `EvictCachedBlob` runs as each fetched blob lands in the library, and
  `database.EvictCachedMadnetworkBlobs` sweeps at startup for nodes that already
  hold duplicates. Nothing is lost, because `EnsureBlob` resolves the library
  before the cache. The invariant this restores is `federation-access.md`
  §Sharing scope's headline one — catalog and bytes read a single rule —
  and the leak it closes was wider than scope: an unapproved download sitting in
  the staging bucket has no published appearance, so the library branch refuses
  it, and only the cache copy was making it reachable at all.

### What a member may cost us (F7 item 6, built 2026-08-01)

`seed_rate_kib` was written when a requester was always a friend, and one bucket
for everyone was the whole of the policy. Item 3 changed who may ask: this node
serves its entire community, and membership deliberately has **no admission
cap** (`federation-access.md` §The membership rule). So the question is no
longer *who gets in* but *what one of them can cost*, and the answer is four
bounds over two resources (`federation/quota.go`).

**Friends are outside all of it.** A direct friend is an admin's decision and is
served exactly as before, under the global cap alone. Everyone else — members,
guests, a pending peer nobody has accepted — draws on the member budget. That
split is the anti-starvation rule as much as the anti-abuse one: without it the
nodes an admin actually chose queue behind the ones the graph let in.

**Two resources, and concurrency is the sharper one.** Bytes are obvious and
already have a global cap. Concurrent serves are what a swarm client multiplies
*by design* — our own fetcher opens parallel Range requests across holders —
so they are what one member most easily costs us in goroutines, file handles and
netstack connections. Both get the same treatment:

|                        | per requester              | all non-friends together |
| ---------------------- | -------------------------- | ------------------------ |
| bytes/sec              | `per_member_rate_kib`      | `member_rate_kib`        |
| concurrent blob serves | `per_member_max_transfers` | `member_max_transfers`   |

**Why a class ceiling, when `federation-access.md` §The membership rule
promised only per-requester quotas.** Because a per-identity limit is exactly
what a sybil farm defeats: N forged keys buy N quotas, and the member count was
already declared not to be the defense. The per-requester half is fairness
*within* the class — one member cannot take the whole budget — and the
ceiling is the actual bound on harm. The other two defenses are unchanged and
still do the work of *ending* an abuse rather than merely surviving it: every
member is traceable on the map to the friend that introduced it, and one block
cuts the branch.

**Refusal is a 429, and that is a feature.** A requester over quota is told to
go away, and the swarm on the other side does exactly the right thing with that
— it fails over to another holder, under a retirement rule that is relative
(`worseThanPeers`), so a busy node is de-ranked rather than condemned. Being
unable to serve right now is honest information, not an error. The check runs
*before* the blob is looked up, so it confirms nothing about whether we hold the
hash — it is a fact about us. Manifests are deliberately **not** counted: a
manifest is a small memoized JSON, and refusing one would stop a member from
even planning a fetch it is entitled to make.

**All four default to `0` — unlimited — by owner decision (2026-08-01).**
The honest consequence: shipped this way the feature protects nobody who does
not edit `madshare.toml`, and the first time it matters is exactly the first
time nobody has configured it. The case for it is that a real small network —
a handful of friends, a three-node lab — wants none of this, and a default
tuned for the adversarial case is a permanent tax on the common one. Numbers
here would have been guesses of the same quality as `discovery_budget`
(`federation.md` §Open questions) with worse failure modes when wrong. The
knobs exist and are documented; choosing them is an operator's call.

### Making it a swarm (F9, designed 2026-08-09; items 1–2 built the same day, item 3 on 2026-08-12, item 4 on 2026-08-13 — COMPLETE)

F4 parallelises a fetch across holders. It does not make the swarm *grow*: every
downloader is a pure leech until it finishes, so ten nodes pulling a new track
leave the origin carrying all ten transfers. The claim two sections up — that
holdings "lets a popular track spread as the community fetches it" — is true
only *after* each fetch completes. F9 closes that, and fixes the two scheduling
defects that the measurements of 2026-07-24 and 2026-08-09 left open.

Four items. Items 1 and 3 are one job; item 2 is what makes item 1 worth having;
item 4 is independent of all of them.

#### Item 1 — Partial seeding

**The defect.** A fetch writes `<hash>.part`, and nothing can see it.
`seedableBlob` resolves `filepath.Join(cacheDir, hash)` only, and
`cacheHoldings` skips `.part` files via the hash-shape check. So a node 90 %
through a download serves nothing of it, and a new seeder appears only at 100 %.
This is the single largest divergence from torrent behaviour and the reason a
flash crowd on a new track does not disperse.

**What it needs.** A way to say *which chunks I have*, and a serve path that
honours it — BitTorrent's bitfield plus `HAVE`, in our idiom:

- `GET /madnetwork/v0/have/{hash}` → the **byte ranges** this node holds
  complete, e.g. `[[0, 786432], [2097152, 3145728]]`.
- `handleBlob` learns to serve a Range out of a `.part` when the covering chunks
  are complete.
- `cacheHoldings` learns to advertise partials, so a partial holder is
  discoverable at the existing sync cadence (see "the advertisement path"
  below).

**Byte ranges rather than a chunk bitmap** (decided 2026-08-09, owner's
question). A bitmap is the obvious BitTorrent-shaped answer and it is the wrong
one here, for a reason worth writing down because it is not obvious:

*Our swarm id does not pin the chunk layout.* BitTorrent's infohash is a hash of
the metadata dict **including `piece length`**, so the piece table is part of
swarm identity and two peers physically cannot disagree about what piece 5 is.
Our swarm id is the flat whole-file SHA-256 (§Distribution, "the manifest's
chunk hashes are not cryptographically bound to it"), so it says nothing about
chunking. The layout is a *policy output* — deterministic from the size, so
two nodes running the same code agree, but two nodes running **different**
`maxChunkSize` do not, and the design explicitly reserves the right to change
that policy "without a protocol break".

That reservation is safe today only because **the wire never speaks chunk
indices**: `fetchChunk` sends `Range: bytes=start-end`, and the layout never
leaves the fetcher. A bitmap would be the first thing to put chunk indices on
the wire, and it would quietly convert a documented freedom into a compatibility
break. Byte ranges keep the freedom: they are layout-independent by
construction, they match the idiom the fetch path already uses, and they need no
layout pinned or validated in the reply.

They are also *smaller* for us, which is the part that inverts the BitTorrent
intuition. Dispatch is sequential-priority, so a madshare partial is a
near-prefix — usually one range, at most a few. Bitmaps win when pieces are
scattered, which is what rarest-first does and what we deliberately do not do.
Only seek-priority fragments a partial at all; cap the reply to the largest N
ranges and the worst case is bounded.

`transfer.availLocked` already computes exactly this shape (it walks `chunkOK`
forward for the contiguous extent from an offset), so the readiness data needs
exposing, not building.

**The advertisement path** (found 2026-08-09 while starting the build). On its
own this item is **inert**: a fetcher's holders come from
`MadnetworkBlobProviders` = catalog ∪ holdings ∪ listener devices, and
`cacheHoldings` skips `.part` via the hash-shape check, so a partial holder is
in none of the three and nothing would ever call `/have`. (An earlier draft of
this section claimed item 1 "still helps holders already named in a plan" — no
such case could be constructed; the claim was wrong.) So item 1 carries its own
minimum advertisement: **holdings reports partials**, distinguishably from
complete blobs. That is discoverable at the existing 15-minute sync cadence,
which is too slow for a 20 MB track and exactly what item 2 exists to fix —
but it makes item 1 a complete and testable feature rather than an endpoint with
no caller.

**Built 2026-08-09.** `/have`, `handleBlob` serving out of a `.part`, holdings
carrying a `partial` list, and the fetch-side change that had to ship with them.
Three decisions taken during the build are worth keeping:

- **A 416 is a fact about the chunk, not about the holder** (`errChunkAbsent`).
  Before this, a partial seeder answering "I have not reached chunk 7" was an
  ordinary transient failure, so it accumulated a streak and was retired like a
  broken node — the retirement rule would have thrown out exactly the holders
  this item exists to recruit. Its streak is now left alone, and deliberately
  not *reset* either: a holder already in trouble should not launder it by
  happening to lack a chunk. The attempt still counts against the CHUNK, which
  is what keeps a swarm of partials that between them lack a chunk terminating
  instead of looping.
- **The store unions complete and partial holdings; only the wire keeps them
  apart.** Persisting the distinction would mean a migration for a column no
  query reads. It is safe because of the previous point: the worst case is one
  fast refusal from a live node, which is nothing like the connect timeout a
  genuinely dead holder costs (that being the whole lesson of the stale-holder
  fix). Ranking complete holders above partial ones is item 3's business, and
  that is where the column belongs if it is ever wanted. *(It never was: item 3
  asks `/have` and gets a live answer, so the distinction is never persisted.)*
- **The fetcher does not call `/have` yet, and the endpoint ships anyway.** The
  lazy path — dispatch, take a 416, move on — gets nearly all the benefit
  for a fraction of the code, and the eager path (consult `/have`, dispatch only
  covered chunks) is pair-selection, which is item 3's rewrite. Shipping the
  server half early is not premature here but the opposite: **a federated
  protocol endpoint has to exist on the network before the code that depends on
  it**, or item 3 can only use it against nodes upgraded after item 3. *(Item 3
  now calls it, three days later — and this is why it can expect an answer.)*

**Three decisions this must settle.**

*The scope gate.* `seedableBlob`'s two branches answer under different rules,
and there is a standing invariant (fixed 2026-08-02) that a cached blob is never
also a library blob. A `.part` is a third case, and the temptation is a third
branch. Do not: **a `.part` seeds under the cache branch's rule, exactly.** It
is a cache object that is not finished, and it can never be a library blob —
it has no published appearance and cannot acquire one until it lands and
verifies. That keeps "catalog and bytes read one rule" intact instead of adding
a rule that would have to be kept in agreement with two others.

*The zero-fill trap.* `fetchSwarm` pre-truncates the `.part` to the full size so
chunks can be written at their offsets. A `.part` is therefore **full-length and
mostly zeros from the first moment**, and a naive `http.ServeContent` on it
would hand over megabytes that verify at neither chunk nor file level. Serving
must be gated on `chunkOK`, never on the file's extent. This is the one way item
1 can silently poison the swarm, so it wants its own test.

*What is safe to serve — and it is not every `.part`* (corrected 2026-08-09
while building; the first draft of this paragraph was wrong). In the **swarm**
path each chunk has passed its own SHA-256 before `fetchChunk` writes it, so
those bytes are as trustworthy as a finished blob's and re-seeding them is sound
— the fetcher downstream still verifies the assembled whole-file hash, which
remains the authoritative anchor, so nothing about the trust model changes.

The **F3 whole-file fallback has no such guarantee.** It streams straight into
the part file and checks the hash only at the end, so its `progress` watermark
counts bytes *received*, not bytes *proven*. Seeding those would make this node
re-broadcast whatever a bad holder sent it — the exact failure per-chunk
verification exists to prevent, arriving through the door we just opened. **A
transfer running in sequential mode therefore advertises nothing**, and since
that is a silent-wrongness bug rather than a visible one it is pinned by
`TestCompleteRangesRefusesUnverifiedSequentialBytes`.

#### Item 2 — Announce, rather than wait to be pulled

**The defect.** Holdings sync on the catalog cadence — 15 minutes. Fetch a
track, go offline in ten, and nobody ever learned you held it. A swarm's peer
set is ephemeral by nature and our tracker is a slow *pull*; the two are
structurally mismatched. It is also what makes item 1 nearly worthless on its
own: a partial seeder that is discovered a quarter of an hour later has usually
finished.

**Built 2026-08-09** (`federation/announce.go`, `POST /madnetwork/v0/announce`).

**It is not gossip, and the sketch that said it was got two things wrong.** The
freshness hints in `freshness.go` were named as the sibling to imitate — but
they are not a gossip record either; they ride the one-minute ping as a query
parameter. And this item's own rule that an announce is **never relayed** means
a gossip record would never propagate past the first hop, which is not gossip at
all. What is left when a record may not be relayed is a **direct push**, so that
is what it is, riding the same refresh round the pings do rather than opening a
cadence of its own.

**Nothing is signed, and that is stronger rather than weaker.** The sketch said
"a small signed record". The mesh address derives from the node key, so the
connection is already self-certifying: the receiver attributes the announce to
the key it is *talking to* and ignores any identity in the body. A signature
would add a second, weaker way to say the same thing — one that could be
replayed by whoever collected it.

**The decisions, as taken:**

- *May an announce mint a holdings row?* **Yes** — and the freshness hints'
  "no" is not inconsistent with it. A hint is about a THIRD party, so accepting
  one would let hearsay claim what only first-hand contact may; an announce is a
  node speaking about **itself**. That is also exactly why it must never be
  relayed: relayed, it becomes hearsay and the permission has to flip back.
- *An unattributable announce is refused.* A guest or a capability-token bearer
  arrives with no key, and a claim about nobody cannot mint anything. A listener
  node's holdings have their own path — the household tracker at `POST
  /api/madnetwork/holdings`, scoped to the device's own account.
- *Friends only.* A member still learns from the holdings pull. Pushing to the
  wider community would mean dialling nodes the frontier rotation deliberately
  budgets, and reach past the first hop is somebody else's announce to make.
- *Additions only; the 15-minute pull stays as the correcting sweep.* An
  increment cannot express a removal, so a blob evicted from a peer's cache
  lingers in our index until the next full sync. The asymmetry is the right way
  round: a stale positive costs one fast 404 from a live node, where a holder we
  never heard of costs the swarm a whole source. Hence `AddSourceHoldings`
  beside `ReplaceSourceHoldings` — same table, opposite promise.
- *Completions are a delta, partials are read live.* Only finished fetches need
  remembering, because what we hold PARTIALLY can be recomputed from the
  transfer table at announce time, and what we hold whole is already the entire
  cache directory — announcing all of that every minute is the traffic this
  exists to avoid. The pending set is also dropped whether or not the sends
  succeed: an announce is an optimisation over a sync that is still running, so
  losing one costs at most the fifteen minutes it would have saved, and a retry
  queue would be state to age, bound and reason about for that.
- *Nothing is announced when seeding is off*, because the blob endpoint would
  refuse to serve any of it. Never advertise what you will not hand over.
- **The trap, from the 2026-08-09 stale-holder fix, and it is real:**
  `EnsureCatalogSource` sets `first_seen` and **not** `last_seen`, so a source
  minted by an announce and not explicitly touched is stale on arrival and
  filtered straight back out by `StaleHolderWindow` — recorded as a holder and
  never used. `handleAnnounce` calls `TouchCatalogSourceSeen` for exactly this
  reason, and `TestAnnounceMintsAHolderAndMarksItSeen` pins it.

**No migration**, as predicted: `AddSourceHoldings` is a new method over the
existing `federation_holdings` and `federation_catalog_sources` tables.

Relationship to the Build plan's deferred *"announce/gossip of catalog deltas"*:
**different object**. That one gossips *library* changes; this gossips
*holdings*. They are siblings — both replace polling for a big thing with
gossiping a small one — and the discovery-speed question in
`.issues/open-issues.md` (2026-08-09) argues they should be decided together.

#### Item 3 — A scheduler that is not speed-blind (built 2026-08-12)

**The defect**, open since 2026-07-24 and re-verified 2026-08-07:
`chunkPlan.pickProvider` was plain round-robin with no throughput input, so a
slow holder kept taking half the dispatches and was discovered only by burning
`Timeouts.PerChunk` repeatedly. The 2026-08-09 measurement put a number on the
worst case: **one dead holder in a plan cost ~150× the entire clean fetch**,
and that is `PerChunk` (2 min), not `ChunkStall` (20 s), because a dead holder
never connects and the idle-read watchdog is never armed.

**Item 1 forced this change regardless.** With partial holders, "pick a
provider, then a chunk" is simply wrong — not every holder can serve every
chunk. The scheduler selects the *pair*. That is why 1 and 3 were one job:
committing to a dispatch shape twice would have been the waste.

**What it measures** — the four inputs the design named, all four built:

1. **Per-provider throughput**, EWMA over completed chunks (`ProviderStats.Rate`;
   `Bytes` accumulated but had no time dimension). The clock brackets
   `fetchChunk` alone, so it is a throughput sample and not a queueing one.
2. **In-flight bytes per provider**, and dispatch goes to the **fewest**. This is
   the whole scheduler: a holder that is not delivering keeps its dispatches, so
   its outstanding total stays high and it stops being chosen — no timeout has
   to expire and nobody has to decide it is slow.
3. **`Timeouts.Connect`, 5 s**, bounding the dial alone. "Never connected" and
   "connected then slow" were sharing one two-minute budget, and only the second
   deserves it. As predicted, this alone removes most of the dead-holder cost.
4. **429 as timed backoff** (`errChunkBusy`), never as slowness or as a failure.

**Three things the design did not anticipate, decided while building:**

- **A failure has to cost a pause**, or fewest-outstanding-bytes inverts. A
  holder that fails *instantly* has nothing outstanding, so the fastest way to
  look idle would be to keep refusing, and one broken holder would take every
  dispatch. Hence `Timeouts.Retry` (500 ms, doubling per consecutive failure,
  capped) — the same knob a 429 draws on, and what a 416 costs when a partial
  holder is the only one left to ask.
- **"Never asked" outranks "fastest"** among idle holders. Ranking by measured
  throughput alone would let the first holder to be measured keep the work for
  the whole transfer, because an unmeasured holder can never win a comparison it
  has no number for. It is one free sample each, not a preference — and a holder
  that HAS been asked and still has no number is one that only ever failed, so it
  sinks to the bottom. That is where the two readings of "no rate" stop agreeing,
  and why the rule is written on `lastTried` rather than on `rate == 0`.
- **Coverage deprioritises, it does not exclude.** `/have` is probed per holder
  in the background at plan start (never blocking dispatch — the answer is an
  optimisation, the first chunk is what a listener is waiting for), and a 416 is
  remembered. But a partial holder is a GROWING thing, so "does not have chunk 7"
  is only true until it isn't: when no other holder will take a chunk, the one
  known to lack it is asked anyway. Being wrong costs one fast refusal; never
  asking costs a source for the rest of the transfer. This also answers the note
  left in `syncHoldings` — the store still unions complete and partial holdings
  under no distinguishing column, because a scheduler that wants to know asks the
  holder and gets a live answer rather than a fifteen-minute-old one.

**`worseThanPeers` was reworked in the same commit**, as required. The rule reads
a 0-failure streak as "this peer is doing fine", which was only sound because
round-robin guaranteed every live holder was handed work. A holder not asked
within `Timeouts.PerChunk` — the time one ask may take — is no longer a
benchmark. Note the direction: excluding it makes retirement *less* aggressive,
because an idle holder sitting at 0 would otherwise keep the strict absolute rule
in force against a holder merely having the same bad moment as everybody else.

**Two manifest hardenings rode along with the retirement rework** (the defect is
written up in `.issues/open-issues.md`, "a lying manifest retires every honest
holder"). They belong to item 3 because both live in the code it was already
rewriting, and because F10 — which would be the thorough answer — is parked
behind triggers that have not fired.

- *Cross-check the manifest.* `fetchAgreedManifest` (was `fetchAnyManifest`,
  which took the **first** valid answer) probes holders in parallel and takes the
  manifest **two of them describe the same way**, returning as soon as the second
  vote lands. Two edges decide the design: **agreement excludes the filename**,
  because a blob's name is a fact about the holder's copy — the library seeder
  knows it by name, a node that fetched it has it under its hash — and reading
  that as disagreement would make every mixed swarm look like a lie; and **a sole
  voice is believed**, because a partial holder cannot BUILD a manifest
  (`buildManifest` reads the whole file), so a swarm of one complete holder and
  several partials has exactly one voice by construction and refusing it would
  refuse item 1 itself. Two holders answering *differently* with no majority ends
  the swarm attempt: nothing here can say which is lying, so it gives way to the
  whole-file path rather than picking a side.
- *Attribute blame correctly.* A corrupt chunk still retires its sender — no
  amount of environmental bad luck produces wrong bytes. But a corrupt chunk from
  a **second distinct holder** says something else: the accusation comes from the
  MANIFEST's sender and the two are different nodes, so the reference is the
  likelier liar. The attempt ends with `errManifestSuspect`, the first holder is
  **reinstated** (in the readout too, or it reports an honest node as dropped for
  bytes it never got wrong), and the whole-file fallback takes over carrying its
  own reference.

Neither changes what a completed fetch guarantees — the assembled whole-file
hash is the anchor and always was, so this is denial-of-service, never
corruption.

**What it cost**, measured on one machine by `TestStaleHoldersCostAFetch` (four
nodes, 2 MiB, three holders, one of them progressively replaced by a key nothing
answers to), before and after:

| scenario | before | after |
|---|---:|---:|
| 0 stale (3 live) | 1.299s | 103ms |
| 1 stale (2 live) | 12.054s | 545ms |
| 2 stale (1 live) | 18.064s | 1.069s |
| 3 stale (0 live) | 24.004s | 2.002s (fails either way) |

A ghost absorbs **two** dispatches rather than `providerFailureLimit`, run after
run: one before anybody has delivered anything, and one more when the live
holders are all loaded and it is briefly the least-loaded thing in the plan. It
is never retired and does not need to be — it is simply not chosen while anyone
else is delivering, which is also why the clean case got faster rather than
paying for the machinery.

**A bug the rewrite surfaced, unrelated to scheduling:** the whole-file fallback
built its own `http.Client`, which is the one place `WithCapabilityToken` cannot
reach. A listener node whose only standing is its home server's vouch would have
been served 404 the moment a fetch fell back to that path — precisely the failure
§"The household" predicts when a request builder forgets the header, which is why
the token is presented by a `RoundTripper`. It uses the pooled blob client now.

**Rarest-first remains deferred**, and the measurement to justify it has still
not been taken. With complete holders every chunk has identical rarity, which is
why F4 has no piece picker; item 1 creates genuinely rare chunks, but the swarms
this serves are small and fewest-outstanding-bytes was enough to make every
scenario above terminate quickly.

#### Item 4 — Pipelining and endgame (built 2026-08-13)

**The defect.** Each worker fetches one chunk, waits for the whole response,
then asks for the next. Over ygg — multi-hop, high RTT — every chunk pays a
full round trip of dead air, and `maxChunkWorkers` caps the parallelism at 8.

##### Endgame / hedging — built as designed

What item 3 could not do is get back the chunk **already** in a slow holder's
hands: nothing re-dispatched it, so a transfer's tail was as slow as its slowest
live holder while every other holder sat idle and the file was otherwise
complete. A chunk now gets a second copy (`maxChunkCopies` = 2) in exactly two
situations:

1. **a reader is blocked on it.** `prioritize` can reorder the queue, but the
   chunk a stalled stream is waiting for has usually left it — it is in flight,
   on whichever holder happens to be slow — so the seek-priority hook now marks
   an in-flight chunk instead of failing silently, and the mark outranks starting
   a new one. A reader waiting *now* is worth more than a chunk nobody has asked
   for yet.
2. **there is nothing else for a worker to do**, the queue being empty. The
   classic endgame.

**Neither needs a timing constant, and that is a change from the design.**
"Re-dispatch any chunk in flight longer than *k* × the median chunk time" was the
suggestion; it is not needed, because every case it catches arrives at (2)
anyway, and it would spend a duplicate while chunks nobody had fetched at all
were still queued. What is left is two structural questions — *is anyone waiting
for this*, and *is there anything better to do* — neither of which anybody has
to tune.

**Cancelling the loser is load-bearing, not tidiness.** `fetchSwarm` waits for
every worker, so an abandoned fetch would hold the transfer open for exactly as
long as hedging was meant to save; the plan owns each attempt's context for this
reason. And the cancelled copy is **not blamed**: it was stopped by us, on
purpose, so counting it would make the second-fastest holder in a swarm collect a
failure streak for being second — the one way hedging could be worse than not
hedging.

Measured (`TestChaosSlowHolderDoesNotOwnTheTail`, two holders, one throttled to
128 KiB/s so its chunks stay *inside* the per-chunk budget — the regime nothing
before this could help): **4.84 s → 108 ms**, three chunks rescued out of three
hedged. Verified against the feature disabled first, where the same scenario
reports `hedges=0/0` and takes the full 4.84 s.

##### Pipelining — measured, and the answer was a ceiling, not a floor

The design asked for "a request queue depth ≥ 2 per holder, so the pipe stays
full across the RTT". Two workers per holder is already what `workers =
len(holders) × 2` produces, so what was actually open was whether *more* helps.
It does not. Over a 300 ms-RTT link capped at 512 KiB/s (4 MiB, one holder):

| per-holder depth | elapsed |
|---|---:|
| 1 | 12.36 s |
| 2 | 12.30 s |
| 4 | 12.80 s (retries appear) |
| 8 | **transfer failed** |

The dead air is real and it is not the bottleneck: a link with any queueing in it
absorbs the gap, so depth 1 and depth 2 are indistinguishable. Past that,
concurrency stops buying bandwidth and starts spending **`Timeouts.PerChunk`** —
eight chunks sharing one capped link each take eight times as long, so they blow
the per-chunk budget together, the swarm collapses, and the whole-file fallback
inherits a link it cannot use either.

**That is reachable in an ordinary plan.** The worker count is derived from how
many holders were ADVERTISED and bounded only in total, so four holders of which
one answers put all eight workers on the survivor. So the shipped depth stays at
two and gains a **ceiling**, `maxHolderRequests`, which holds however many
workers exist. `TestChaosOneLiveHolderIsNotFloodedWithWorkers` pins it: with the
cap, 9/9 chunks on the swarm path; without it, `mode=swarm→whole→whole`, no
bytes, 35 retries.

This is the one place F9 contradicted its own design, and it is worth
remembering why the design was wrong: it reasoned about an idle *connection*,
and the thing that runs out first is a *deadline*.

##### What it cost to read

`TransferStats` gained `hedges`/`hedges_won` and a per-holder `lost`. The last
one is not decoration: a holder that was asked for three chunks and beaten to
all three otherwise disappears from the readout entirely — no bytes, no
failures, no row — and that is precisely the holder somebody reading the stats
is trying to find.

#### Deliberately not in F9

- **Choking / unchoking / tit-for-tat.** BitTorrent's reciprocity engine answers
  strangers who might freeload. Our swarm boundary is the community, every
  member is vouched for, and the freeload case is already answered by the F7
  member quotas. Here it would mostly penalise a node with a small library.
- **A DHT.** Analysed and set aside on 2026-08-09; the reasoning lives in
  `.issues/open-issues.md` §"library discovery is slow". Short form: wrong
  query shape for browse, slower per lookup than the local index it would
  replace, and a global one contradicts the audience model in three places.
- **Super-seeding.** Item 1 delivers nearly all of its benefit.
- **Merkle-rooted chunk hashes.** Unchanged from F4: the swarm id is the flat
  whole-file SHA-256, per-chunk hashes exist for early verification, and the
  assembled hash stays the anchor. Nothing in F9 needs that revisited.

#### Build order

Planned in dependency order — **item 4** (standalone, any time) → **item 1** →
**item 2** → **item 3** — and built in the opposite one: items 1 and 2 on
**2026-08-09**, item 3 on **2026-08-12**, item 4 last on **2026-08-13**, having
been deliberately not pulled forward (owner's call, 2026-08-09).

Building item 4 last cost nothing and bought one thing the plan could not have
predicted: item 3's dispatch rule is what made the endgame's trigger obvious. A
worker with nothing to do is only a *reliable* signal that the tail has arrived
once the scheduler stops handing out work to holders that are not delivering —
before that, "the queue is empty" and "the transfer is nearly done" were
different statements.

Items 1 and 3 were one job in the sense that item 3 must not be designed before
item 1 lands — pair-selection is only meaningful once holders can be partial —
but item 1 did not wait on anything. Item 2 without item 1 still shortens the
path for completed fetches, so the two orderings differ only in how fast item 1's
payoff arrives.

#### What F9 left standing

- **Rarest-first** is still not built and still not measured (item 3).
- **A holder is never reconsidered** once retired within a transfer, which the
  2026-07-24 write-up listed as a defect. It matters much less now: retirement is
  relative, a 416 and a 429 do not cause it, and a holder that is merely slow is
  passed over rather than condemned — so the population that gets retired at all
  is smaller than the rule was written for.
- **The chunk layout is still policy, not protocol identity**, which is what F10
  would change. Nothing in F9 needed it revisited.

No migration was needed: `federation_holdings` and `federation_catalog_sources`
already carried what item 2 writes.

**Items 1 and 2 are verified over a real mesh**, not only by unit tests —
`TestPartialHolderAnnouncesItselfAndSeedsIntoALiveSwarm` (`swarmlive_test.go`):
three genuine yggdrasil cores, the gVisor netstack, the real HTTP-over-mesh path
and the real audience gate. One node holds a blob whole, one holds its first
half as an in-flight `.part`, and the third learns of the partial holder *from
its announce* and then takes chunks off it. Both halves of the assertion were
checked against a disabled feature before being trusted: with partial seeding
off the partial holder delivers 0 bytes, collects 404s and is **dropped**; with
the announce off the discovery never happens.

It is deliberately ONE test over three nodes rather than two tests over five.
Starting real cores is the expensive part, and an earlier split added five to
the package — enough extra load to tip an unrelated gossip test into its
timeout. That is the standing hazard in this package (`.issues`, "mesh tests can
flake under load"), and the lesson for the next real-mesh test is to count the
cores it adds, not just the seconds it takes.

### Merkle verification (F10, decided-and-parked 2026-08-09)

Raised by the owner while building F9 item 1: should we adopt BitTorrent v2
wholesale, on the grounds that it is proven by time and will answer problems we
cannot yet name? **Decision: yes in substance, parked as F10, with explicit
triggers — not folded into F9.** Recorded here in full because the reasoning
has one counterintuitive turn that a future reader will otherwise re-derive
wrongly.

**"Copy BT v2" bundles three separable decisions**, and only one of them is
actually open:

1. *The identity model* — infohash = SHA-256 of the info dict, which commits
   to the file tree. **Not ours to copy.** Our content address is the whole-file
   SHA-256 and it is the upload dedup key, the storage path, the cache filename,
   the DB identity, the catalog advertisement and a playlist's `remote_hash`. It
   is not changeable, and it is not the weak part.
2. *The verification structure* — merkle trees over fixed 16 KiB leaves.
   **This is the open question.**
3. *The wire protocol* — peer messages, `hash request`, extension negotiation.
   Already declined in F4 ("we control both endpoints").

#### The two decisions

**A — flat chunk list, hardened** (what F9 ships on). On-demand manifest of
per-chunk SHA-256, adaptive layout, whole-file hash as the anchor, plus the two
cheap hardenings folded into item 3. Costs: verification granularity is the
chunk (256 KiB–1 MiB, welded to seek granularity); the layout is *in* the
protocol, so a sizing-policy change breaks manifest compatibility; **it does not
scale to video** (deferred, not cancelled — a 4 GB file at 16 KiB granularity
would need 8 MB of flat hashes, so fine-grained verification of a large file is
simply unavailable); and **a blob that exists only as partials cannot be
reassembled**, because `buildManifest` reads the whole file, so no partial
holder can produce one. BitTorrent has no such failure mode, metadata being
independent of data.

**B — merkle verification, our identity.** Fixed 16 KiB leaves, a tree per
blob, the root stored per blob and advertised in the catalog beside the content
hash; any byte range verifies against the root with ~log₂(n) sibling hashes.
The whole-file SHA-256 remains the identity and the final anchor.

**The non-obvious part: B does not cost chunk-size flexibility, it buys more of
it.** The owner offered to pay that price and does not have to. A leaf is a
*verification* unit, not a *transfer* unit, so once verification is leaf-aligned
the chunk layout leaves the protocol entirely: request any size from anyone,
verify the covered leaves, seek at 16 KiB instead of 1 MiB, keep the lead ramp
as a pure dispatch decision, and change the sizing policy forever with **zero**
protocol impact — where today "the policy can change without a protocol break"
holds only while chunk indices stay off the wire. Today verification, transfer
and seek granularity are one welded number; B unwelds them. It also scales to
video (~576 bytes of proof for a 4 GB blob), lets partial holders serve proofs,
and isolates a bad block to 16 KiB.

Its costs are real: a **second identifier per blob** — migration, storage,
catalog protocol change, a backfill over every existing blob, and a permanent
"root unknown" state for anything predating it or arriving from an older node
— and more work than F9 items 1–4 combined.

#### The turn in the reasoning: merkle does not buy trust

This is what the question started from, and it does not survive scrutiny, so it
must not be the reason anyone reaches for F10 later.

BitTorrent's root is trustworthy because the infohash — the thing you used to
*find* the swarm — commits to it, self-verifying by construction. **Ours would
not be**: the root arrives in the catalog, from a peer. A peer-supplied merkle
root is exactly as trustworthy as a peer-supplied chunk list; a liar lies about
the root instead. The fix is identical in both worlds — cross-check the claim
across independent sources and treat disagreement as a contradicted claim, which
`federation_claim_reports` (F6) already exists to carry.

The root *is* a better-shaped fact than today's manifest — one stable value
per content hash, asserted independently by many catalogs, rather than a
per-transfer trust decision made by whichever holder answers first. But that
follows from putting it in the catalog, not from it being a tree. **B buys
granularity, scale and protocol simplicity. It does not buy trust.**

"Proven by time" is worth something here and less than it sounds: we would
inherit the data structure's pedigree, not the protocol's scrutiny, because we
are not adopting their wire protocol. Merkle-over-fixed-leaves is a construction
nobody will find a hole in; the bugs in bespoke systems live in the protocol
around it, and we are writing that either way.

#### Triggers, and what F9 must not assume

F10 is picked up when **either** of these lands, and the decision is then made
on evidence rather than on a fear we can already name:

- **video support** — a flat list cannot verify a multi-GB blob finely; or
- **a measured case of the all-partials reassembly gap** — a blob the
  community collectively holds but no single node can produce a manifest for.

What survives either way, and is therefore safe to build now: **`/have` byte
ranges** (item 1) are correct under both designs — indeed byte offsets are
what B generalises. What **reverses** under B: item 1's rule that *partials
serve bytes, never manifests*. Under merkle a partial holder can serve proofs
for the leaves it holds, which is exactly what closes the reassembly gap.


## Related

- [`federation.md`](federation.md) — the spine: the catalog that advertises
  these hashes, availability, and the build plan F3/F4/F9/F10 belong to.
- [`federation-access.md`](federation-access.md) — who a blob endpoint
  answers: audience, scope, the capability token.
- [`swarm-admin.md`](swarm-admin.md) — the operator's view: live transfers,
  per-counterparty traffic, the two rate knobs.
- [`madnetwork-cache.md`](madnetwork-cache.md) — the directory these fetches
  land in, and what keeps it from growing without bound.
