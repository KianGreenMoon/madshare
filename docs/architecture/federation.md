# Madnetwork federation — design

> **This document is the spine**: what the network is for, how a node is
> identified and reached, what it publishes to its community, how it decides
> another node is still there, and the build plan for the whole feature. Three
> parts outgrew it and have documents of their own:
>
> | document | what it answers |
> |---|---|
> | [`federation-access.md`](federation-access.md) | **who is served what** — the four principals, the membership rule, listener nodes and their capability tokens, the household, sharing scope, the audience model |
> | [`federation-trust.md`](federation-trust.md) | **how nodes know and judge each other** — friendship and pairing, friend-list gossip, distrust marks, contradicted claims, the network map, branch weighting |
> | [`federation-swarm.md`](federation-swarm.md) | **how bytes move** — direct transfer, the chunk swarm, per-member quotas, F9 (making it a swarm), F10 (merkle, parked) |
>
> **Status** (agreed 2026-07-18). Built: F0–F9 in full. Decided and parked behind explicit
> triggers: F10. The posture runs in both directions — **everything to our
> community, nothing outside it** — and `meshlab reach` is the measurement that
> says so. §Build plan carries every phase, what it settled and what it left; the
> single entry in §Open questions is a design-time number wanting a real network
> to watch, not a blocker.
>
> Federation is auth Phase 4 (`docs/architecture/auth.md` §8) and the milestone
> the native client (`docs/ui/madplayer.md`) exists to use.

## Goal & vocabulary

**Madnetwork** is the peer-to-peer federation of madshare nodes: node A can
browse, stream, and download node B's shared library, and nodes jointly
distribute the bytes swarm-style. Guiding stance: **minimum restriction for
people inside the network, nothing for people outside**, and the network itself
is **transparent by default** — its social graph is visible to its members.

- **Node** — one madshare instance, identified by its Yggdrasil keypair.
  Servers and personal madplayer instances are both nodes; a madplayer is just a
  node that is usually single-user and intermittently online.
- **Friend** — a mutual trust relationship between two nodes, established by
  exchanging node cards (address + public key). The trust graph is built from
  these edges. A **direct friend** is one we friended by hand; the unqualified
  word is ambiguous in ordinary speech and is avoided below for that reason.
- **Community** — *the* word for what this network is for (declared
  2026-07-31). Our community is our whole connected component of the friendship
  graph: our direct friends, their friends, their friends' friends, outward with
  **no radius and no size limit**. When this project says "we share with our
  friends", it means the community in this sense, not the handful of nodes one
  admin typed in. **We share everything with our community and nothing outside
  it** — that sentence is the design, and these four documents are how it is
  enforced. "Madnetwork" and "our community" are the same set; the first names
  the technology, the second names the people.
- **Sharing scope** — who a recording is shared with. **Three values, and no
  ladder** (decided 2026-07-30, superseding the per-hop "trust depth"):
  **Madnetwork** = our whole community, **Direct friends** = only the nodes this
  admin friended by hand, **Local** = nobody on the network. **Shipped default:
  Madnetwork** (declared 2026-07-31) — a node shares its library with its
  community out of the box, which is what the network is *for*;
  `federation-access.md` §Sharing scope states the posture that follows from
  it. "Direct friends" is the opt-in *restriction*, for a node that wants its
  content to stop at the people it chose personally. There is no "friends of
  friends" value because there is no ladder — a per-hop scope was a promise we
  could not keep, see `federation-access.md` §Sharing scope, "Why the ladder
  collapsed". The stored column and its constants are unchanged
  (`DepthUnlimited` / `DepthFriends` / `DepthPrivate`), so this is a vocabulary
  and UI decision, not a schema one; `DepthFriends` is the constant behind
  **Direct friends** and keeps its name.
- **Gossip** — information spread node-to-node rather than from a central
  place: each node tells its friends, who tell theirs. Three distinct uses,
  deliberately kept apart. **Friend-list gossip** (F6, built) — B tells A whom
  B is friends with *and relays what B's own friends said*, so A's view grows
  past its friend list to the whole connected network rather than a fixed
  radius; the network map, branch snipping and distrust marks all read it, and
  since F7 so does **membership** — whether a key belongs to our community at
  all (`federation-access.md` §The membership rule, which reads only *mutually
  declared* edges), the one access question it may answer. It never answers *how
  much* a requester gets: there is no hop arithmetic anywhere, by design
  (`federation-access.md` §Sharing scope, "The graph decides whether, never how
  much"). **Freshness-hint gossip** (F7) — a friend relays *its* friends'
  `last_seen` as a second-hand claim, so availability survives past one hop
  without pinging strangers (§Availability). **Catalog-delta gossip**
  (deferred) — pushing library changes instead of pulling snapshots; an
  optimisation, unrelated to the other two. Despite the name none of these is a
  push protocol here: they ride the existing periodic pull (§Catalog), and the
  word describes how information travels, not the transport. Because a friend
  list names third parties who never agreed to be named, its payload is a
  privacy decision as much as a protocol one; both halves are settled in
  `federation-trust.md` §Friend-list gossip.
- **Full peer** — a node: participates in catalog exchange and the swarm.
- **Madnetwork member** — a node we can place in **our own** connected
  component of the friendship graph: a friend, or a friend of a friend of …
  us, as vouched for by the gossiped records we hold. Membership is what
  Madnetwork-scoped content is shared with; a node we cannot place there is a
  **guest**, however routable it is on the mesh (`federation-access.md`
  §Principals & access). Being on Yggdrasil establishes nothing.
- **Thin client** — a browser user. Thin clients are *not* madnetwork
  participants; they are local users of exactly one home node, which acts as
  their gateway.
- **Listener node** — a madplayer: a person's device that runs a node and
  swarms like a full peer, but signs in to a home server with **user
  credentials** instead of being friended, and **publishes no catalog** — its
  library stays private to the device. Consumption is one-way; the only route
  from that library into the network is an ordinary upload to the home server.
  The madshare half is built (`federation-access.md` §"The household"); the
  client half is madplayer's `docs/design.md` (in its own repository).

## Identity & transport

- **Identity = the Yggdrasil node key** (ed25519). The derived `.ygg` address is
  self-certifying — proving you hold the address proves you hold the key —
  so the trusted-peer table is just a table of peer keys/addresses. No PKI.
- **Transport = yggdrasil-go embedded as a library**, routing madshare's
  federation protocol over the mesh, without a system TUN (mobile madplayer must
  not need `VpnService`/`NetworkExtension`). **Confirmed by the F0 spike
  (2026-07-18):** upstream `yggdrasil-go` (v0.5.14) `core.Core` plus the
  importable `github.com/yggdrasil-network/yggstack/src/netstack` wrapper
  (gVisor userspace TCP/IP) served HTTP between two in-process nodes over the
  mesh — no TUN, no root. `netstack.ListenTCP` returns a standard
  `net.Listener` (drops into `startListeners` like any other listener) and
  `DialContext` plugs into an `http.Transport` for outbound calls. Dependency
  choice: **upstream yggdrasil-go + yggstack's netstack package** — yggstack
  is an official yggdrasil-network project (not a third-party fork) and its
  master tracks the latest core release, so the update-lag risk is low; the
  wrapper is ~2 small files we could vendor if it ever stalls.
- **Local yggstack fork.** `third_party/yggstack`, wired by a `replace`
  directive, carries **three** patches upstream does not have — all documented
  in `third_party/yggstack/MADSHARE-PATCH.md`, all to be re-applied on any bump,
  and the whole fork to be dropped once they land upstream:
  1. **A data race in `YggdrasilNIC.writePacket`**, which read every outbound
     packet into a *single shared* `writeBuf` while gVisor drives writes from
     several goroutines at once. Dormant on one connection, triggered reliably
     by the swarm's parallel chunk fetches, so F4 is what found it. Each call
     now takes its own buffer from a `sync.Pool`; the write path below it
     (`ipv6rwc`/`core.WriteTo`) is already mutex-guarded, so per-call buffers
     suffice and sends stay parallel.
  2. **Inbound-reader resilience** (upstream issue #398): one read error used to
     `break` the reader goroutine and kill *all* inbound mesh traffic
     permanently. It now log-and-continues with a 50 ms→1 s backoff and
     exposes `InboundReaderAlive()`, which is the signal §Availability's
     fail-open watchdog reads.
  3. **Netstack teardown**: upstream has no `Close`, so every stopped node
     leaked its gVisor stack and nine goroutines. Guarded by
     `federation/TestStopReleasesNetstack`.
- **Build option:** the `nofederation` build tag (mirroring `nowebui`) compiles
  all federation code and its dependencies (yggdrasil, gVisor) out, producing a
  standalone server; such a build aborts startup if the config enables
  federation.
- The same key signs application-layer artifacts where needed (capability
  tokens, distrust marks). Plain reads between direct friends need no extra
  signing — the channel already authenticates both ends.

## Catalog & the madnetwork library

- The madnetwork library is **its own section/page**, permission-gated
  (`madnetwork.access`). The home page stays the local library.
- Catalog entries are per-recording **tagset payloads** as designed in
  `docs/architecture/recording-tagsets.md`: the recording (audio identity,
  fingerprint claim, renditions with quality facts) plus its appearances
  (tagsets), each with **origin-node provenance**. Access is never imported from
  a tagset. Entries also list **known holders** of each rendition's content hash
  — this is the swarm's tracker (see `federation-swarm.md` §Distribution).
- **Remote claims are hints, never facts.** A peer's fingerprint or recording
  grouping is used for discovery and display only. On download the bytes are
  verified against the content hash chunk-by-chunk, and the fingerprint is
  recomputed locally (`fpcalc`) before anything merges into a local recording.
  That local recomputation *is* the guarantee, so it is a hard requirement
  rather than an enrichment: a node with `[federation].enabled` **refuses to
  start** when `fpcalc` is not on `PATH` (override: `[federation]
  allow_missing_fingerprinting`, which accepts importing and re-publishing
  unverifiable content). `ffprobe` stays optional — without it the published
  catalog carries no quality facts and friends cannot rank this node's
  renditions, which is poorer output, not unverified input.
- **Download to library — through the review bucket.** Right to listen = right
  to download. By default the **ladder-best rendition** is fetched
  (`RankRenditions` across local + remote). The download does **not**
  auto-approve — **not even for the admin**. The madnetwork library page is a
  browsing surface only: download = fetch + stage. Every download enters the
  existing moderation pipeline (`docs/architecture/moderation.md`) as the
  downloader's draft in the **same staging bucket as uploads**, because that is
  where the editing machinery already lives (`track-edit.js` modal, preview
  player, ladder compare) — verifying inside the library view would be the
  wrong place. The review card keeps *Approve* as its default single action —
  the lazy path is one click (`content.moderate` holders as today) — while the
  rich path lets the downloader verify the file, browse the recording's other
  tagsets, and see other renditions the madnetwork holds that may be better. A
  further tagset downloaded for audio we already hold goes through the same
  one-click approval and attaches as another appearance of the **same local
  recording**. For the very lazy there is a setting **`autoapprove_downloads`**:
  skip the bucket entirely and land the file unchanged, exactly as fetched.
  Default **on for madplayer** (personal node — the owner is the only reviewer
  anyway), **off for servers**.
- **What the synced catalogs are then worth to our own library** — the review
  card's madnetwork arm and the quality-upgrade page — is F8, built and
  designed in §Quality upgrades. Two properties are settled there and hold
  everywhere: the network's opinion is **advisory** (it never writes a state or
  blocks a submission), and adopting a better rendition is **additive** (the
  rendition we hold is never replaced automatically).
- **Sync mechanism = pull-and-cache (built, F2).** Periodically (15 min) and on
  new friendship, a node pulls a catalog over the mesh (`GET
  /madnetwork/v0/catalog?since=<serial>`, served to our community —
  default-deny toward everyone outside it) and keeps a local copy
  (`federation_catalog`, rooted since F7 item 5 on the *source* it came from,
  one row per remote appearance, denormalized text — remote ids are opaque,
  never joined onto local entities). **Snapshot + not-modified**, not row deltas
  (decision 2026-07-18, superseding the earlier "changed since serial N"
  sketch): true per-row deltas would need change tracking across five tables (a
  rename changes catalog text) — so the serial is a **content hash over the
  whole snapshot**; an unchanged serial gets a tiny "unchanged" reply, a
  changed one the full snapshot. Measured size (2026-08-13): **~920 B per
  entry** — ~900 KiB at 1k tracks, ~4.4 MiB at 5k, ~17.7 MiB at 20k — about a
  third of which is the per-rendition fingerprint claim head
  (`ClaimHeadWords` = 64 words, base64), so "a few hundred KB" holds only for
  small libraries; the wire format carries the serial, so real deltas can
  arrive later without a protocol break. The serving node memoizes its own
  snapshot (~1 min) so friend syncs don't rebuild it per request.
  **Applied as a diff, wholesale only in effect** (decision 2026-08-13,
  federation-explained #3 dig; owner-approved): the snapshot IS the resulting
  table state, but `ReplaceSourceCatalog` writes only rows that changed —
  existing rows are read and digested, changed ones upserted, vanished ones
  deleted. The old delete-everything-reinsert spelling paid the snapshot
  arrangement's cost in bytes written: one new track in a 20k-entry catalog
  rewrote ~18 MiB of WAL where the diff writes ~20 KiB (measured, ~900×) —
  flash wear and checkpoint pressure every 15-minute cycle once a community
  uploads actively. Under the diff, `first_seen` preservation (migration 037)
  is **structural** — a surviving row is untouched or updated by an upsert
  that never sets `first_seen`, so the carry-map is gone. Two deliberate edges
  (both owner calls, 2026-08-13): a `first_seen = 0` row ("unknown", the 037
  backfill) **keeps 0 forever** — the old code re-dated it on the next changed
  sync, which later dropped stale entries into *New on the network* as if
  fresh, while the `new` lane's `first_at > 0` filter reads 0 as the unknown
  it is; and a **duplicate entry key** in the (remote, untrusted) snapshot
  resolves **last-wins** through the upsert — the wholesale spelling failed
  the whole transaction on the primary key, wedging that source's sync until
  the remote fixed its catalog. Holdings replace stays wholesale on purpose:
  two-column rows, negligible churn. A friend's catalog cache is **retained** regardless of reachability
  (never TTL-purged); the friend carries a **"last seen"** indicator, and
  whether their exclusively-held tracks appear in the *merged* madnetwork view
  is decided at request time by the availability predicate (§Availability &
  node health) — storage and visibility are separate concerns. What a node
  publishes is its **whole approved live library** minus what the requesting
  audience may not see — per-content share depth and the per-friend guest-only
  demotion (`federation-access.md` §Sharing scope, F5); the snapshot is memoized
  per audience class rather than globally. Push/gossip of changes is a later
  optimization, not v1.
- **Playback needs a holder, not the origin.** Because the swarm is keyed by
  content hash, an offline friend's tracks stay playable whenever *any*
  reachable node holds the hash. With network scale (many redundant libraries),
  most entries have multiple holders — availability improves as the network
  grows.
- **Merged view (built, F2).** The `/madnetwork` page — its own header
  section, gated `madnetwork.access`, **shell-native** so local playback
  survives browsing it — shows the **deduplicated union** of all *friends'*
  catalogs (a blocked peer's cache is kept but hidden; unblock restores the view
  without a resync) as a **drill-down mirroring the local library** (artist →
  album → track, album-artist grouping, case-insensitive merge, the same
  Unknown-artist/Other buckets). Identical tagset text offered by many nodes
  collapses to one row; which friend it came from is **not surfaced while
  browsing** — provenance stays stored and appears only in the track's
  expansion (holders + last seen) and the page's sync-status strip. Since F3 the
  expansion carries the version actions — Play, Queue, Download to library —
  acting on the version's **ladder-best rendition** (the server sorts each
  version's renditions by the quality ladder before answering). While every
  carrier was a direct friend the count was trivially trust-weighted; since
  catalogs travel past the friend ring (F7 item 5) the ordering counts
  **branches, not holders** (F7 item 10, `federation-trust.md` §Trust graph
  "Where the weighting applies") — a farm behind one friendship cannot make
  its claim the version everyone's Play button lands on.
- **Catalog crossing — "N versions" (built, F2; resolves former open question
  1).** The same tagset text on *different claimed recordings* (different
  masters, live vs. studio, or a mislabel) stays **one track row** that expands
  into its **versions**. Recordings are **never merged on text**: two claims are
  folded into one version only when they **share a rendition content hash** —
  proof of identical bytes somewhere — otherwise they stay separate versions,
  each with its renditions and holders. Versions are ordered most-widely-held
  first (the default pick; the quality ladder cannot rank across different
  audio). Hint-level fingerprint matching for display dedup of *unshared* rips
  can refine this later; local verification on download (F3) stays the truth
  either way.

### Discovery beyond the friend ring (F7 item 5, built 2026-07-31)

Once scope decides who may *fetch*, the thing standing between an admin and "the
whole network's libraries are reachable" is no longer authorization — it is
**knowing a hash exists**. This is the real content of F7, and it has two halves
that ship separately:

- **Serving discovery to members — built 2026-07-31** (item 3).
  `handleCatalog` and `handleHoldings` now answer any member, so our library is
  discoverable by our community.
- **Pulling from beyond the friend ring — built 2026-07-31** (item 5). The
  sweep pulls catalogs and holdings from a *bounded frontier* of the community,
  not from friends alone, and `MadnetworkBlobProviders` reads the same set.
  Until this landed, item 3 was a one-sided opening: symmetric across nodes,
  useless on any single one.

**The endpoint change was one line of policy**, as predicted. `handleCatalog`
already answered *for an audience*, so serving a **member** meant passing the
member audience: the snapshot contains exactly the Madnetwork-scoped entries and
nothing else. An **outsider** — a node we cannot place in our component —
keeps its 403, so opening discovery did not open it to the mesh at large. **403
here, 404 at the byte endpoints** (`federation-swarm.md` §Distribution), and
the asymmetry is deliberate: a catalog request names nothing, so refusing it
openly leaks nothing, while a blob or manifest request names a *hash* —
answering 403 would confirm we hold it. Refuse plainly where there is nothing to
confirm; stay silent where there is. Two properties make the member case cheap
rather than alarming:

- **All members are one audience class**, so their snapshot is memoized once and
  served to every one of them, and the existing `since=`-serial not-modified
  reply makes a repeat pull a single small round trip carrying no payload.
- **It reveals exactly what an admin marked Madnetwork** — which under the
  shipped default is the whole published library, and that is the intent
  (`federation-access.md` §Sharing scope). A node whose admin moved the default
  to Direct friends answers a member with an empty catalog instead: also
  correct, and needing no special case, because both answers come out of the
  same predicate.

Holdings are served to the community too (2026-07-31, `federation-swarm.md`
§Distribution — the swarm's only boundary is the madnetwork), and to nobody
outside it: advertising a download cache to strangers would leak what people
here listened to, while inside the community it is what makes a fetched blob a
discoverable seeder.

**Whom to ask is already solved — by F6, for free.** The gossiped graph is a
directory of node keys, and a mesh address derives from a key, so every node on
the map is dialable without any new discovery mechanism. Yggdrasil cannot
enumerate the mesh, but we no longer need it to.

**How much to pull was the phase's one open engineering question**, and it is
answered by bounding rather than by cleverness. Pulling every mapped node's
catalog every cycle is the N² dialling pattern that was rejected for graph
records, and caching the entire network's public library is unbounded storage.
What ships is therefore **bounded and demand-shaped** rather than exhaustive
(`federation/discovery.go`, `syncSources`):

- friends are pulled every round they are due, **unbudgeted** — few, and
  chosen;
- beyond them a **budget per catalog cycle** (`[federation] discovery_budget`,
  default 4) of member catalogs, **least-recently-attempted first**, so the
  frontier expands steadily instead of in one storm;
- a **cap on cached foreign catalogs** (`discovery_cap`, default 200) with the
  coldest evicted, since `federation_catalog` is already declared a droppable
  cache. Friends and blocked peers are never counted by it: a cache that forgets
  the nodes an admin decided about, to make room for strangers, has its
  priorities backwards;
- and an explicit **pull-now** (`POST /api/admin/federation/discover`, the
  network map's *Fetch library now*) for a node an admin is actually interested
  in, jumping both the rotation and the budget — interest beats fairness.

**Rotating on attempts, not on successes.** A node that never answers must still
lose its turn, or one dead key would be retried ahead of every live member
forever. `attempted_at` is therefore written before the request, and the
rotation reads it.

**A member we hold nothing from has no row, and that is where the frontier
starts.** The rotation walks source rows, so a node we have never pulled from is
invisible to it — it is also, by definition, the least-recently-attempted
thing there is, so it is served *first* and the row is created as we try it.
Getting this order wrong is what a live 5-node chain caught: spending the budget
on the members already cached meant the first two nodes reached consumed every
later round as well, and the frontier never moved past them. Two in-process
tests had been green throughout, one of them because it created the very row it
was checking for.

**Where the two halves of visibility divide.** Admission — whom we may cache
at all — is decided once a minute by the sweep's retention walk, because
membership is a graph walk SQL cannot do. Blocking is decided *in the browse
query*, because it is a local act that must take effect the moment an admin
clicks it. Retention keeps a source while it is a direct friend, a member, or a
peer we blocked (kept hidden, so an unblock restores the view with no resync);
everything else is collected, which is how `federation-trust.md` §Forgetting
reaches the catalog cache — a branch a block or a removal cut off stops being
a member, and the same walk that un-draws it on the map drops its cached
library.

**The storage decision (owner, 2026-07-31): a table of its own.** Cached
catalogs used to hang off `federation_peers` with a CASCADE, and every browse
query joined `state = 'friend'`. Migration **036** re-roots them on
`federation_catalog_sources` — one row per node we hold a catalog from — and
moves the catalog sync state (serial, synced-at) there with them. The two tables
now answer two different questions: *a peer row exists because an admin decided
something*, *a source row exists because the sweep pulled from it*. Keeping them
apart is what stops a table an admin reads as "decisions" from filling with
hundreds of nodes nobody chose, and it puts the cache's retention rule on the
cache's own table.

A peer row per member was considered and refused. It looks like the cheaper
option and is not: SQLite cannot alter a `CHECK` constraint, so admitting a
`'member'` state means rebuilding `federation_peers` anyway — the same
migration weight, in exchange for merging two meanings that want to stay apart.
Blocking still hides a cached catalog without deleting it, but as a join to the
peer table rather than as a CASCADE.

**Two heard names, and both are read.** A friend's self-claimed name is
refreshed by the friendship ping onto its *peer* row; a member's by the
discovery ping onto its *source* row. The display chain is admin label →
either heard name → short key, and reading only the source's made friends
render as bare key prefixes while strangers rendered names — backwards, and
again something only the lab showed.

**Freshness for a node we never ping.** The availability window (§Availability)
reads `MAX(source.last_seen, peer.last_seen)` — a friend is pinged every
minute and pulled every fifteen, a member is only ever pulled, so neither clock
alone is the answer and the later one always is. A member's catalog answer *is*
its liveness, including the not-modified reply; the transfer path's
`observePeerAlive` now writes to the source row for the same reason.

*Rejected — relaying catalog entries the way graph records are relayed.*
Records work because a node's friend list is tiny and bounded (512 edges); a
catalog is the whole library, so every node would end up storing every node's
public catalog. A signed **digest** could be relayed cheaply, but the entries
would still have to be fetched from the origin, which is the pull above with an
extra layer. If the frontier rotation proves too slow in practice, digest relay
is the upgrade path — it makes "which node changed" free, and only then does
storing more pay off.

## Availability & node health

> **Supersedes the reverted "10-second presence" feature.** An earlier attempt
> (phase 4 of the madnetwork-page rework) ran a dedicated 5 s prober with a 10 s
> online/offline hysteresis and *hid* offline friends' tracks live. It was
> unstable on a real mesh (download stalls + online/offline flapping) and was
> backed out in full — see `.issues/open-issues.md` ("the 10-second presence
> feature was reverted"). The three mistakes were: a **fast dedicated ping** that
> competed with transfers on the fragile netstack, a **tight hysteresis** (probe
> interval ≈ threshold → flapping), and a **live-mutating client** that made a
> false reading vanish the library. This section is the corrected model. The UI
> half lives in `docs/ui/madnetwork-page.md` §Availability; the build steps are
> in `docs/plans/availability.md`.

**The unit is the track, not the friend.** Because the swarm is keyed by content
hash, a track's availability is the *union over its holders* (catalog ∪
holdings) — "is **any** holder reachable" is far more stable than "is this one
friend online right now", and it is exactly what transfer already computes when
it fails over between providers. Availability grows with the network: redundant
libraries make most entries multi-holder.

**Availability = redundancy + slow/passive liveness + reactive reachability.**
There is no dedicated high-frequency prober. Three cheap sources feed a per-peer
`last_seen`, and availability is derived from it at request time:

1. **Slow health check.** The existing **1-minute friendship refresh loop**
   already pings every friend; that ping *is* the health check (reuse the mesh
   `GET /madnetwork/v0/ping`, no new endpoint, no new cadence). One round a
   minute is within the connection budget the mesh already carries — it is not
   the 5 s prober that caused the churn. The **ping floor** (designed
   2026-08-13, §"Reactive down-mark + the ping floor" below) extends a
   cycle-cadence version of it to every cached member source.
2. **Passive observation.** Every *successful* mesh interaction refreshes
   `last_seen` — outbound (catalog sync, holdings sync, a delivered
   blob/chunk) and **inbound** (a friend syncing our catalog, fetching a blob,
   or pinging us proves they are alive *and*, by Yggdrasil's symmetric
   addressability, that we can most likely reach them). An in-flight transfer is
   continuous liveness proof for that holder for free.
3. **Reactive reachability.** When a transfer/manifest fetch fails against a
   holder, that failure should count against its availability — the
   PeerTube/Mastodon pattern (learn a peer is down by *trying*, back off), not
   by pinging ahead of need. Two halves, both built: the *swarm-internal* one
   (the scheduler de-ranks and fails over inside one transfer) and, since
   2026-08-13, the availability-side **down-mark** (§"Reactive down-mark + the
   ping floor" below). The second was parked at first because `last_seen` is
   monotonic and cannot be moved back, so the knowledge died with the transfer;
   it needed a column of its own rather than a different way of writing that
   one.

**Freshness window, not a knife-edge.** A friend is *reachable* if `last_seen`
is within a **minutes-wide** window (`[federation] reachable_window_sec`,
default 180 s ≈ 3 refresh rounds, clamped up to a 120 s anti-flap floor), so a
single missed ping never flips it — the flapping came from a 1× margin, this
is a several-× margin by construction. No probation state machine; the window
*is* the hysteresis. Whether hiding is applied at all is the runtime
**`madnetwork.hide_unavailable`** toggle (default on, `/admin/settings`) — off
shows every friend's cached catalog regardless of reachability.

**Availability predicate** (evaluated **at request time** in the browse/search
queries and the remote-playlist availability flag). A rendition is *available*
iff:

1. a **reachable** node holds it (catalog ∪ holdings, `last_seen` within that
   node's window — 180 s for one we ping, three catalog cycles for one we only
   pull from; see "Two clocks, two windows" below), **or**
2. it is in the **local library**, **or**
3. it is **fully cached** (complete file in `<data_dir>/cache/madnetwork/`, no
   `.part`) — *the one arm not built:* the request-time queries have no cheap
   way to ask the filesystem, so it wants a table of complete cache hashes and
   therefore its own migration. Until then a cached-but-unreachable track hides,
   which is wrong in the safe direction (it is still fetchable, just not
   offered).

A version is available if any rendition is; a track if any version is;
albums/artists and counts are computed over the available set. Local, cached,
and this node's **own** published tracks are *always* available — they never
depend on anyone's liveness. Because the predicate runs per request, each
browse/search fetch is a fresh **snapshot**; there is no server push and no live
mutation (the client re-evaluates only on page load and on a new search — see
the UI doc).

**Fail open, never fail dark.** If *this node* cannot reach anyone (see the
self-health watchdog below), the correct response is to **stop filtering** and
show the last-known catalog, not to blank the library — a local fault must
never look like "the whole network is gone". Concretely: availability filtering
is suppressed while the node's own inbound path is suspect.

**Self-health (own inbound path, built).** This is the more important monitor,
and it is what makes "fail open" decidable. The vendored gVisor netstack ran its
entire inbound path in one goroutine, where a single read error killed *all*
inbound mesh traffic permanently (the SPOF logged 2026-07-19 in
`.issues/open-issues.md`) and every friend went silent at once even though the
network was fine. Both halves are now built:

- **The read loop was hardened first**, because a watchdog over a silently dead
  reader only reports the fault it should have prevented. The yggstack fork's
  reader log-and-continues with a 50 ms→1 s backoff instead of `break`ing on
  one read error, and exits only on `Close()`/terminal `ErrClosed` — the
  inbound-reader resilience patch in `third_party/yggstack/MADSHARE-PATCH.md`.
- **The signal is `InboundReaderAlive()`**, exposed by that same patch and read
  through `Node.InboundHealthy()` → `inbound_healthy` on the madnetwork
  summary. It is the only *unambiguous* one available. A self-ping cannot test
  the inbound path (`HandleLocal: true` loops local traffic back without
  touching it), and the originally sketched heuristic — *every friend
  unreachable for N refresh rounds while the yggdrasil core still reports peers
  up* — was **rejected as ambiguous**: it cannot tell a dead local reader from
  a genuinely absent set of friends, which is precisely the distinction
  fail-open exists to make.

Unhealthy ⇒ cutoff 0 (no filtering at all) plus `inbound_healthy: false` on
the summary, so the UI shows the last-known catalog behind a banner instead of
blanking.

**No transitive real-time presence — how the big network stays honest.** Now
that catalogs travel past the friend ring (F7 item 5) many holders are nodes no
friendship pings — their liveness is whatever their last catalog answer said
— and the answer is deliberately *not* to start pinging strangers or to relay
pings along the chain. Federated systems don't do live presence at all:

- **Mastodon (ActivityPub)** is push-with-backoff: activities are delivered to
  peer inboxes, delivery failures retry with exponential backoff over days, and
  an instance is marked dead only after prolonged failure. There is no "online
  now" concept; capability/health is a **NodeInfo** document fetched
  occasionally, and reach beyond direct follows comes from **relays**, not
  transitive pinging.
- **PeerTube** adds **redundancy**: instances mirror popular videos, so a video
  stays available when its origin is down — availability is **replication**,
  not liveness. Discovery across the network uses **search indexes / instance
  lists** (SepiaSearch), again not a presence protocol.

We already have the analogues — the swarm's holdings *are* PeerTube
redundancy, and reactive backoff *is* Mastodon's dead-instance handling. So the
plan is: **gossip coarse freshness hints** (a friend tells us how recently it
saw *its* friends — a claim, not a probe of ours), rely on **redundancy** (any
reachable holder serves), and **verify on demand only for the working set
actually on screen** (one mesh RTT to the specific holder, proof not hearsay,
cost O(what you are looking at) not O(network)). The hints ride the **one-minute
ping** rather than the catalog sync they were first sketched on — see the next
section for why the catalog is far too slow to feed a window measured in
minutes. A future further enrichment of `GET /madnetwork/v0/ping` into a small
**NodeInfo-style health card** (version, holdings size, seed policy) gives the
network map real per-node health without any new probing cadence. No
chain-relayed ping-forwarding is ever needed.

### Two clocks, two windows (F7 item 10, built 2026-08-01)

Everything above was written when every source we cached was a **friend**. Item
5 made most of them **members**, and the two are not on the same clock:

- a friend is **pinged every minute**, so a 180 s window is three missed rounds;
- a member is only ever **pulled from** — `discovery_budget` nodes per sweep,
  each due once per catalog cycle — so its `last_seen` advances at best every
  fifteen minutes, and more slowly as the frontier fills.

Judging the second by the first is a category error, and it was measured as one
on 2026-08-01: a two-hop member's tracks were visible for about **three minutes
in every fifteen**. Item 5 pulled the community's libraries and the availability
filter then hid nearly all of them again. Visibility only — the bytes stayed
fetchable the whole time, because `MadnetworkBlobProviders` never consulted the
window — but the browse is where the feature lives, so from the page it looked
like the community had no library.

The correction has two layers, wanted for different reasons.

**Layer A — the window measures how recently we would have noticed.** There is
not one freshness window; there is one *per class of observer*, and both carry
the same 3× anti-flap margin over the cadence that feeds them. A node we ping
every minute is judged against `reachable_window_sec` (180 s). A node whose only
clock is the catalog pull is judged against three catalog cycles (45 min,
`federation.PullFreshnessWindow`). `reachClause` picks between them per row
rather than per query, because a single browse mixes both classes:

```sql
AND MAX(s.last_seen, COALESCE(p.last_seen, 0)) >=
    CASE WHEN COALESCE(p.state,'') = 'friend' OR s.hinted_at >= <pingedSince>
         THEN <tightCutoff> ELSE <pullCutoff> END
```

This alone makes the bug go away, and it is the honest reading of what the
stored timestamp means. What it costs is precision in one direction: a member
that died two minutes ago keeps its tracks on the page until its turn in the
rotation comes round. That is the safe direction — a stale offer fails over to
another holder or fails one fetch, while the alternative hid a whole community's
library — but it is still a worse answer than we can give.

**Layer B — a friend's ping carries what it knows.** The refresh loop already
contacts every friend once a minute for exactly this purpose, so the hint rides
that request rather than the catalog: `GET /madnetwork/v0/ping?hints=1`,
answered only to friends, carrying **ages in seconds** for the nodes the
responder pings itself. Ages rather than timestamps, because two nodes need not
agree on the clock and an age composes across a hop without them having to. The
caller applies each hint to the source row it already holds for that key. So a
member two hops out — our friend's friend, which is most of a small community
— is refreshed once a minute by our friend's own first-hand ping, lands back
inside the tight window on its own merit, and is hidden within three minutes of
actually going away.

**A node may vouch only for what it touches itself.** A hint covers the
responder's *friends*, never the sources it merely pulls from. This is the whole
of the trust rule and also the whole of the engineering one: a friend's
knowledge of a node it only pulls from is already fifteen minutes stale, so
relaying it could never satisfy a 180 s window in the first place. One hop,
first-hand, bounded by the friend list (`MaxFreshnessHints`, the `MaxGraphEdges`
bound) — and beyond that ring layer A is the answer, not a deeper relay.
*Rejected: hints propagated with accumulated age.* It is a second gossip
protocol — propagation, ageing, hop counting, a store of hints-about-hints —
delivering liveness that is still bounded below by the pull cadence at the first
relay that was not a friend.

**A hint is evidence, not a second clock.** It writes `last_seen` like every
other observation (monotonic, so an out-of-order hint cannot age a node), which
is what keeps one column answering "when was this node last known alive" no
matter who observed it. `hinted_at` (migration **038**) records something
different and necessary: *when a fast observer last reported on this source*,
which is what decides the window. Folding the two together would invert the fix
— a hinted member that dies goes on being hinted (its friend keeps relaying a
frozen observation), so the row must stay on the tight window and disappear in
three minutes rather than lingering for another forty-five.

**The class asks who is watching *now*, not who once did.** `hinted_at` is read
against one *ping* window, not the pull window, and the difference is a whole
failure mode. When the **member** dies the hints keep arriving with a frozen
observation, so the row stays tight and is hidden in three minutes — correct.
When the **voucher** dies the hints stop, and within a ping window the row drops
back to the pull clock *our own rotation still refreshes*, so a perfectly
healthy member stays visible — also correct, and the opposite of what a longer
horizon would do. Reading the class off a hint from forty minutes ago would hide
a node we can reach because somebody else stopped talking about it.

**A friend can lie about its friends**, and the network already lives with that:
heard names, gossiped edges and distrust marks are all a friend's word. A false
liveness claim costs one failed fetch that fails over, which is strictly less
than what a false *edge* costs, and hints are accepted only from friends, only
about members, and only for sources we already cache — a hint about a node we
hold no catalog from changes no row and creates none.

### Reactive down-mark + the ping floor (built 2026-08-13, migration 048)

Owner decisions 2026-08-13, from the "make availability flatter" consultation.
Two gaps it closed, one per half:

- **Negative evidence evaporated.** Positive contact feeds `last_seen`
  (`observePeerAlive`), but when `dialHolder` timed out on a dead member the
  scheduler de-ranked it for *that transfer* and the knowledge died — the browse
  kept showing the member's exclusively-held tracks for up to 45 minutes, and
  every user who clicked Play re-paid the discovery. This is liveness source 3
  above, promised at design time and parked in `docs/plans/availability.md`'s
  gotchas ("`last_seen` is monotonic — reactive failure needs a separate
  signal").
- **The pull window is a constant but the rotation cadence was not.** The sweep
  runs every minute with `discovery_budget` = 4 → ~240 member pulls/hour of
  capacity, while 200 cached sources due every 15 minutes demand ~800/hour: at
  the `discovery_cap` default the real pull interval stretched to **~50
  minutes, longer than the 45-minute window**, so unhinted (3+-hop) members
  flapped ~45-visible-of-50 — the measured 2026-08-01 bug returning at scale in
  a slower rhythm. Hints mask it in small communities, which is why it was never
  seen live.

**The down-mark.** One new column in `federation_nodes`' observations group
(migration **048**): `unreachable_at INTEGER NOT NULL DEFAULT 0` — an
*observation* ("I tried first-hand and could not connect"), never a judgment,
which is why it sits beside `last_seen`/`hinted_at` and not in the trust group.

- **Write sites, connect-class failures only:** the transfer path
  (`rangeBlob`, `fetchFrom`, the manifest/have probes), a catalog pull, the
  pings — each through one of the two funnels in `federation/reachability.go`,
  `observeReply` (a holder) or `observeControl` (a key). Connect-class = dial
  timeout / refused / no route. A read stall, a corrupt chunk, or a slow body
  is a *quality* fact the scheduler owns; and **any HTTP answer — including a
  429 — is proof of life** and advances `last_seen` (fixed in this build: only
  a *verified chunk* used to, so a member protecting itself with quotas looked
  exactly like a dead one to the nodes it throttled).
  **How the class is decided:** `dialMesh` wraps every mesh dial and tags its
  errors `errMeshDial`, so `connectFailure` reads the class off the finished
  request instead of guessing at OS and gVisor error text — "did we get as far
  as an answer" is a question the dialer can answer exactly. A dial *we*
  cancelled (a hedge losing its race, a transfer abandoned) is excluded on the
  same grounds a hedge's loser is never blamed; a dial *timeout* is not.
- **The self-protection guard is relative**, the `worseThanPeers` shape: a
  failure is recorded only when some *other* node answered us within the tight
  window at failure time. "One silent while others answer" is evidence about
  them; "everyone silent" is evidence about us and marks nobody.
  (`InboundHealthy()` alone was rejected as the gate — it guards the inbound
  half only, and an outbound-path fault would still paint the community dead.)
  The state is one `(time, key)` pair in memory, and holding one rather than a
  set errs toward marking less: a node that is both our newest success and our
  newest failure — a flapping link — is never marked. The window is
  `3 × Intervals.Refresh`, derived from the ping cadence rather than read from
  `reachable_window_sec`, because it asks a question about the mesh and not
  about what an operator wants displayed.
- **Predicate: the newest first-hand fact wins, but the mark may only shorten
  the PULL window, never the ping window.** A node reads unavailable when
  `unreachable_at > last_seen` **and** `last_seen` is already outside the
  tight window. Without the second clause, one failed dial against a friend
  pinged 30 seconds ago flips it off the page — a 1× margin, exactly the
  knife-edge the reverted presence feature died of. Friends and hinted members
  keep their pure 3-minute window logic; the mark's whole value lives in the
  45-minute corridor. There is no explicit clearing and no state machine
  ("the window is the hysteresis" holds): any later positive contact advances
  `last_seen` past the mark.
- **First-hand and local, forever.** Never gossiped, never hinted — the same
  argument that keeps distrust marks advisory: a relayed "X is down" is a
  defamation lever. Hints continue to carry positive ages only.
- **Not fed into `MadnetworkBlobProviders` ordering.** The scheduler already
  owns live holder ranking inside a transfer; one mechanism per job.
- Subject to the same suppression as the rest of the filter: `hide_unavailable`
  off or fail-open (cutoff 0) means marks are not applied either.

**The ping floor** (`pingFloor`, at the end of `syncSources`). The existing
1-minute sweep additionally pings (`GET /madnetwork/v0/ping`, no `hints` param)
every cached member source whose `last_seen` is older than one catalog cycle and
which this round's pulls did not already reach — **outside the pull budget**,
which exists to bound full-snapshot downloads, not liveness bytes. Success is
an ordinary observation (`last_seen` advances); failure is a down-mark write
site, so a dead far member is hidden within one cycle instead of surviving to
its rotation turn hours later.

- **Effect:** observation cadence ≤ 15 minutes for every cached source at any
  frontier fill, so the 45-minute window keeps its 3× anti-flap margin *by
  construction* and the flap corridor closes. Detection latency becomes ≤ 15
  min for everyone, ≤ 3 min for friends/hinted, ~instant for anything actually
  in use (the down-mark). The two windows themselves are unchanged — the fix
  flattens the observation *floor*, not the windows, because flattening
  windows without cadence re-creates a measured bug in one direction or the
  other.
- **Cost:** ≤ `discovery_cap` tiny requests per cycle (~0.2 req/s worst case),
  spread by the same rotation order the pulls use. This deliberately moves the
  "the 1-min sweep + passive touches are the whole liveness budget" line one
  notch, and the move is recorded here on purpose: it is **not** the reverted
  5-second prober, whose three mistakes were fast cadence, knife-edge
  hysteresis, and a live-mutating client — this is cycle-cadence, keeps the 3×
  margin, and changes nothing about the request-time predicate model. Listener
  nodes are untouched (they cache no sources and run no pull sweep).
- **Spread, not a burst** (`floorBudget`): a round may ping the source list
  divided by the rounds in a cycle — 14 of 200 at the defaults — so the cost is
  a handful of tiny requests per minute and every source is still reached once
  per cycle by construction. Each ping is bounded by `Timeouts.Connect` rather
  than the control client's 15 s, because a floor ping asks one question and
  the answer over a working path is one small reply.
- **The floor keeps its own clock** (in memory, keyed by node key, pruned to
  the live source set). Not `attempted_at`, which is the *pull* rotation's and
  would cost a node its turn there; and not `last_seen`, which only moves on
  success — so a node that neither answers nor earns a mark (the guard refused
  it, or it accepted the connection and then said nothing) would be retried
  every single round while the genuinely due ones starved behind it.

**Build notes** (details in `docs/plans/availability.md` §Phase 5): migration
048 breaks the `database_test.go` version assertions (known gotcha); the SQL
predicate change in `reachClause` lands together with its Go twin
`database.ReachableAt` (which took a `SourceReach` struct — two of the three
inputs are int64 timestamps a positional call could swap) and api
`reachWindows.ok`, or the summary strip and the holder greying disagree with
the browse; the new `PeerStore` method means `go vet -tags tests
./tests/mesh/...`. Every surface that judges a source now carries the mark:
the browse and lanes (SQL), the node strip, the ⓘ holder list, the F8 match
arm and the upgrades page.

### The underlay kick (built 2026-08-15)

The down-mark reads a connect-class failure as evidence about the *remote*
node. The kick reads the same failure in the other direction: if any of our
own configured peerings is currently down, this is the moment to redial it —
somebody just wanted the mesh and did not get it.

The gap it closes is not madshare's. yggdrasil redials a lost peering on a
backoff that doubles per failed attempt and caps, by default, at over an hour
(`core/link.go` `defaultBackoffLimit`), and a netstack dial never touches that
schedule. Measured (`federation/dialrecovery_measure_test.go`, the 2026-08-15
tester report run as a scenario): after a 90 s outage, every fetch kept
failing for another **38 s after the link was physically back** — 19 fresh
attempts, none of which could shorten the wait, because pressing Play dials
*through* the mesh, not *at* the peering. The result was a track reading "not
found" for minutes while the network was fine.

Mechanism: `Node.kickUnderlay()` fires from both connect-class funnels
(`observeReply`/`observeControl`, reachability.go), throttled to one kick per
`Intervals.Kick` (default 10 s), and calls `Mesh.KickPeers()` — which
re-offers every peer the Mesh was given (config plus runtime `AddPeer`s) to
the core. For a link in backoff that is yggdrasil's own kick channel and an
immediate redial; for a healthy link it is a structural no-op (nothing listens
on a connected link's kick channel, and the send never blocks), which is what
makes firing on *every* connect-class failure safe.

Three deliberate choices:

- **The down-mark's relative guard is not consulted.** The guard separates
  "evidence about them" from "evidence about us" — and the kick is most
  valuable precisely in the about-us case, when everything is failing because
  our own uplink is the peering in backoff. Opposite polarity, same trigger.
- **The throttle sits above yggdrasil's floor.** 10 s against the 5 s
  `minimumBackoffLimit` that bounds the `?maxbackoff=` URI parameter, so
  demand-driven dialling is never more aggressive than the most aggressive
  schedule an operator could configure by hand.
- **KickPeers keeps its own peer list** rather than reading `core.GetPeers()`:
  a multicast-discovered link is keyed by (URI, source interface) and
  `PeerInfo` does not carry the interface, so re-adding it from there would
  mint a second, interface-less link instead of kicking the one that exists.
  The list holds exactly the peers that were added with no interface — the
  shape a re-add matches.

The `?maxbackoff=` URI parameter remains the operator-side complement (it
caps the schedule itself; the kick only skips a wait when there is demand),
and it works in `madshare.toml` today — peer URIs pass to the core verbatim.

## Quality upgrades (F8, designed and built 2026-08-02)

Every phase up to here was about *reaching* other libraries. F8 is about what
that reach is worth to the library we already have. The synced catalogs know
things about our own recordings — better encodings of them, other people's
names for them, and occasionally that our tags are wrong — and none of it
reaches the surfaces where somebody is already making a decision. Three items
land it there: an arm on the review card, a mismatch warning beside it, and a
page that scans the library for renditions the network holds and we do not.

Underneath all three is one question, and it is the phase's real design content:
**when do two libraries mean the same recording?**

### The audio-identity join

Decided 2026-08-02: **hash first, fingerprint head second, text never.** Two
stages, both of them arithmetic this node already performs elsewhere, and both
reading the *cached* catalog — no network call, so the join is cheap enough to
sit on a request path.

1. **Shared rendition hash.** A cached entry that advertises a hash we hold is
   about our recording, with no inference at all. The join is the whole bound:
   it considers only hashes present on both sides, so the work is proportional
   to the overlap rather than to either library — the same cost profile as
   `checkHeldBlobClaims`, whose SQL shape it reuses. It also reaches further
   than it first appears, because **a materialized download is the same bytes**:
   every track this node ever fetched from the network is joined for free, and
   those are exactly the tracks most likely to have better renditions out there.
2. **Fingerprint head.** For entries stage 1 did not match: shortlist by
   duration within `recordingDurationTolerance` (7 s — the window the local
   resolver already shortlists by), then compare our full fingerprint against
   the entry's 64-word `FingerprintClaim` head with `compareHeads` at
   `maxBitErrorRate` (0.10 — the threshold the local resolver already groups
   by). Reusing both numbers is what keeps a finding explainable in one
   sentence: *this is the same audio by the very standard this node uses to
   decide that two files are the same audio.* This is the stage that finds a
   **re-encode**, and a re-encode is where a better rendition lives — without
   it the upgrade page would mostly find nothing, since a node holding our exact
   bytes by definition holds nothing better.

**Text is never a join, and that is a security property, not fastidiousness.**
Tagsets attach to recordings because audio identity is the only claim a receiver
can check; matching on artist/title would hand the join to whoever picks the
name. It would also put that weakness at the *worst* possible surface — the
review card exists to catch mislabels, so a match arm steerable by a chosen name
would be an attacker's way to tell a moderator "the network agrees with this
file". The whole point of `federation-trust.md` §Trust graph layer 1 is that a
mislabel lands on the true recording; a text join would let it land on a
fabricated one.

Degradation is by stage and stays quiet: no local fingerprint, or a remote entry
that carries no claim, means stage 1 only for that pairing. An absent claim is
uncheckable, not suspicious — the F6 rule, unchanged.

**Freshness does not gate the join.** A match found in the catalog of a node
that has gone quiet is still a true fact about the network, and hiding it would
repeat the mistake §Availability was written to fix. Staleness is a *display*
concern here, handled exactly as holders are handled on `/madnetwork`: the row
is greyed, not withheld. This matches the standing rule that playlists and
favourites are never freshness-gated — a saved or surfaced item is
intentional.

### The match arm on the review card (item 1)

`GET /api/admin/moderation/{tagsetID}/classify` already answers *what would an
approve change*, in the library's own terms: the case (A/B/C), the appearance
collision, and the ladder compare of the submitted blob against the recording's
current best. The arm extends that same answer outward — what the **network**
says about this recording — and nothing else about the endpoint changes:

- **Other tagsets.** The names other nodes give this audio, branch-weighted by
  `BranchMap.Voices` exactly as `/madnetwork` orders versions, so a farm behind
  one friendship cannot manufacture a consensus for a moderator to defer to.
  This is `federation-trust.md` §Trust graph layer 1 finally rendered: a
  mislabel arriving here is visible as a minority label next to the dominant
  honest ones.
- **Better renditions.** Remote renditions of the matched recording, ranked by
  `RankRenditions` against ours — the same ladder, fed the catalog's claimed
  tech fields. Claims, and labelled as claims: nothing is verified until bytes
  arrive, at which point the analysis pipeline re-derives all of it locally.

The arm is **advisory and takes no action**. The one thing it offers is adopting
a remote tagset's text into the existing edit modal, which is a form fill, not a
decision — the moderator still approves. Materializing a better rendition
deliberately does *not* live here: mid-review is the wrong moment to start a
transfer, and item 3 is the surface built for it.

Because downloads already stage as the downloader's draft, the *download* review
card is the same card. "Upload and download review cards" is one implementation.

### The mismatch warning (item 2)

Decided 2026-08-02: **both oracles.** `federation-trust.md` §Trust graph names
the external one (layer 4) and the internal one (layer 1) as separate defenses,
and they fail in opposite directions, so the pair is worth more than either.

- **The network's dominant label** comes free with item 1's join and needs no
  configuration, no key and no external service. Its blind spot is stated
  plainly in the UI: it only speaks about audio the network already knows, and a
  wrong label held by one honest branch reads as agreement.
- **AcoustID → MusicBrainz** is the oracle outside the social graph entirely,
  via the `tagsource` machinery built for tag suggestions (shared limiter,
  shared 15-minute cache, shared `Deps.AcoustID` client). Its blind spot is that
  it is off unless an admin configured a key.

Decided the same day: the external lookup fires **when a moderator expands one
card** — one row, one lookup. That respects the process-global 1 req/s
serializing limiter without ever queueing a page of rows against it, and it puts
the warning in front of someone without requiring them to know to press for it.
A queue nobody opens costs nothing.

Both warn and neither acts. No auto-flag writes a state, no submission is
blocked, nothing is scored — the no-automatic-reputation rule covers this
surface too. The moderator gets the preview player, the two verdicts and the
tags side by side.

### The upgrade scan (item 3)

Decided 2026-08-02 (owner, over a recommended manual job): **the scan re-runs on
every catalog sync.** The reasoning is `checkClaims`' own, which already runs on
*both* sync paths including not-modified — *their* catalog stands still while
*our* library moves, so every upload and every materialized download is new
material for old claims to be checked against. An upgrade is the same shape of
fact, and a maintenance page nobody remembers to run is a page that reports
nothing.

The cost that choice buys has to be paid down deliberately, because the sweep is
the one loop that must stay cheap, and the fingerprint stage is the only
unbounded thing in this phase. It is bounded by making steady-state work
proportional to **what changed**, not to the library:

- **Stage 1 always runs, whole source.** It is a hash join; its cost is the
  overlap, which is the same reason `checkHeldBlobClaims` was safe to put here.
- **Stage 2 runs only over new material on either side** since this source's
  last scan: cached rows whose `first_seen` is newer than the watermark (the
  column F7 item 8 preserves across a sync — structurally, since the
  2026-08-13 diff-apply) and local
  recordings fingerprinted since it. Every older pairing was compared on an
  earlier round and its answer has not changed. The first scan of a source pays
  the full pass once.
- **Stored findings are re-ranked every round** regardless — that set is
  small, and re-ranking is what makes a finding *disappear* when we materialize
  it or when our own best rendition changes.

Findings live in their own table (migration **039**) rather than being
recomputed per view, for the reason the disposition column implies: a finding is
a comparison made at a moment, and **dismissing one has to survive the next
scan**. Detection never overwrites a disposition — the rule
`federation_claim_reports` already follows. A finding whose hash no longer
appears in any cached catalog is swept.

The page (`/admin/upgrades`, gated `content.moderate`) is **unobtrusive by
design** — a page an admin visits, at most a quiet count badge, never a
notification — and lists findings newest-first with the local rendition beside
the claimed remote one. Its action is **additive**: materialize the better
rendition onto the same recording via the existing `POST
/api/madnetwork/download`, which stages it through the review bucket like every
other download and re-verifies the bytes locally on arrival. The old rendition
is left alone — soft-deleting it is a separate, manual act on the Recordings
lens. Nothing here deletes anything, and nothing replaces a blob a human did not
choose to replace.

## Topology asymmetry (unchanged)

A backbone of always-on server nodes plus intermittent madplayer peers. Mobile
peers are mostly consumers and occasional sources; durable availability comes
from the backbone and (future) subscribe→replicate, not from expecting phones
to be reachable. A phone serves only while foregrounded.

## Build plan

Swarm distribution is wanted from day one in spirit; in build order it is its
own milestone directly after direct transfer works, and tokens ship with depth.

- **F0 — Groundwork.** Embed yggdrasil-go (library-as-transport
  spike-confirmed 2026-07-18, see §Identity & transport); node keypair
  lifecycle; `[federation]` config section; federation listener/protocol
  skeleton; the `nofederation` build tag (standalone build, mirrors `nowebui`).
- **F1 — Friendship** (built 2026-07-18, see `federation-trust.md`
  §Friendship). Node cards (export/import), pairing handshake, trusted-peer
  table (+ user-level mapping to local accounts — replaced 2026-08-13 by the
  per-peer guest-only flag, migration 047), block/unblock (local effect
  only), admin network page (list form).
- **F2 — Catalog** (built 2026-07-18, see §Catalog). Pull-and-cache catalog
  sync with direct friends (snapshot + not-modified, "last seen"), madnetwork
  library section (merged drill-down) + `madnetwork.access` permission (admin
  default + the stackable `madnetwork` role, migration 027) + gated header link,
  tagset payload + per-peer provenance storage, the "N versions" crossing UI.
- **F3 — Direct transfer** (built 2026-07-18, see `federation-swarm.md`
  §Direct transfer). Fetch-by-hash from a friend (HTTP Range wire, full-hash
  verified), cache-through streaming relay for thin clients, download-to-library
  through the review bucket + local fingerprint verification via the analysis
  pipeline, ladder-based rendition selection, `autoapprove_downloads`.
- **F4 — Swarm** (built 2026-07-19, see `federation-swarm.md` §Distribution).
  On-demand chunk manifest with adaptive, self-describing chunk size (`GET
  /madnetwork/v0/manifest/{hash}`); multi-source parallel chunk fetch with
  per-chunk verification, bad-chunk failover, and F3 whole-file fall-back for
  older peers; the holdings tracker (`GET /madnetwork/v0/holdings` +
  `federation_holdings`, migration 028) unioned with catalog holders so cached
  downloads seed; seeding controls (`seed_enabled`/`seed_cache` DB settings +
  `[federation] seed_rate_kib` token-bucket cap). Swarm scope = direct friends,
  channel-auth only (no tokens yet).
- **Availability & node health** (built 2026-07-23, not depth-gated; see the
  section of that name). Hardened netstack inbound reader (issue #398) →
  slow/passive per-peer `last_seen` from the existing 1-min refresh + all
  successful mesh traffic → request-time availability predicate (reachable
  holder ∨ local ∨ cached) with a minutes-wide freshness window
  (`[federation] reachable_window_sec`, runtime `madnetwork.hide_unavailable`
  toggle) → self-health via `InboundReaderAlive()` + fail-open banner.
  Replaces the reverted 10 s presence feature. Deferred: the cached-blob
  exception in the predicate, which wants its own migration.
- **F5 — Depth & scope** (built 2026-07-25, see `federation-access.md`
  §Sharing scope). Share-depth knob (node default + per recording, migration
  030), the audience model filtering catalog and bytes from one rule, per-friend
  filtering via the user mapping (itself replaced by the per-peer guest-only
  flag on 2026-08-13, migration 047), and the guest-open swarm. Two parts of it
  were **superseded on 2026-07-30**: the per-hop ladder collapsed to three scopes,
  and the guest-open swarm was withdrawn in favour of scope being the network's
  only authority (`federation-access.md` §Sharing scope, "Why the ladder
  collapsed"). What survived is the part that mattered — one audience value
  deciding catalog and bytes together.
- **No fingerprint, no publication** (near-term, not depth-gated; see the
  planned item at the end of `federation-access.md` §Sharing scope). The
  publishable predicate gains an `audio_fingerprints` requirement per rendition,
  in `visibleTagset` / `selfPublishedClause` so catalog and bytes inherit it
  together, plus the "why is this not published" readout in the Recordings lens.
  Independent of both F6 and F7, shippable on its own; the startup gate refusing
  a federated node without `fpcalc` (built 2026-07-26) is the other half of the
  same rule.
- **F6 — Transparency & defense** (built 2026-07-31, see `federation-trust.md`
  §Trust graph). **Changes nothing about who may fetch what** — every
  requester stays at distance 0 throughout, so the wire's access rules are
  exactly F5's. What it adds is sight and reach of *judgement*: an admin can see
  the graph beyond their own friend list, see whom the network distrusts, and
  cut a branch.

  *Built 2026-07-26 — see `federation-trust.md` §Friend-list gossip for the
  design and its consequences.* Signed per-node friend-list records relayed by
  friends (unlimited radius, highest-sequence-wins, digest-then-fetch on the
  catalog cadence, 7-day expiry against a 6-hour heartbeat, migration 031);
  distrust marks published on every block with a reason (migration 032),
  superseded network-wide when the block is lifted; and the **network map** on
  `/admin/network` — a node-link diagram over `GET
  /api/admin/federation/graph`, laid out on rings by hop distance, carrying
  branch attribution, the address beside every hearsay name, branch-weighted
  mark display, and Block by key for the strangers that make up most of it.
  Branch snipping falls out of the map's reachability walk: it never traverses
  through a blocked node.

  *Naming built 2026-07-30.* Sanitization (`federation-trust.md` §Name
  sanitization): one `sanitizeLabel` behind `CleanPeerName` and
  `CleanMarkReason`, so a name or a mark reason renders as what it is and two
  nodes cannot render identically. And the naming split (`federation-trust.md`
  §Friendship): `heard_name` beside the local label (migration 033), the claim
  refreshed from the ping reply on the existing 1-minute cadence, and `local
  label ?? heard name ?? short key` everywhere a node is shown.

  *Contradicted-claim reports built 2026-07-30* (`federation-trust.md`
  §Contradicted identity claims): the fingerprint head on the catalog wire, the
  held-blob and grouping checks on the sync cadence (migration 034), the
  evidence on the peer card and the count on the dashboard — the detection
  that makes the blocking tooling something an admin can act on rather than
  guess with.

  *Underlay de-peering built 2026-07-30* (`federation-trust.md` §Trust graph,
  blocking): the sweep matches the live link list against the blocked set by key
  and drops the links we dialled, so a blocked node also loses us as transit.
  Inbound links are the documented exception (an upstream panic, no handle).

  **F6 is complete.**
- **Forgetting stale graph data** — *built 2026-07-31* (see
  `federation-trust.md` §Forgetting). Ending a friendship was instant where it
  is enforced and slow in what we remembered. All three parts landed, and the
  third cost nothing as designed: `walkGraph` skips every gossiped edge touching
  our own key, so our edges come from `federation_peers` alone; `ReachableKeys`
  + `DropUnreachableGraph` collect what is no longer reachable on the sweep that
  already runs `ExpireGraph`; and with the branch's records gone,
  `GraphKnowsKey` refuses it on the next round with no code of its own. Our own
  edges are now drawn `Mutual` — they are facts, not claims to be weighed.
  Removal also drops the in-memory pairing note. No migration: the graph store
  is a cache. The admin surface says what an action takes with it, on the block
  and remove confirmations. **Prerequisite for F7 item 2**, which turns the same
  walk into an access decision.
- **Refreshing the graph on demand** — *built 2026-07-31* (see
  `federation-trust.md` §Refreshing the graph on demand). A **Rescan** button
  on `/admin/network` forces `syncGraph` past the 15-minute catalog timer —
  graph only, coalescing, and honest in the UI that it buys our friends'
  freshness rather than the network's. Its counterpart on the serving side is a
  memoized graph digest (`Intervals.GraphDigestTTL`, 30 s), the `ownSnapshot`
  pattern rather than a cooldown: a friend that pulls too often gets a cheap
  answer, never a 429 that `syncGraph` would read as a missing endpoint. One
  thing only a real mesh showed — the button silently doing nothing — is
  recorded with the section.
- **F7 — Reach: the community's libraries** (**COMPLETE** — items 1–8 and
  10 built 2026-07-31 / 2026-08-01, item 9 on 2026-08-01). Rescoped 2026-07-30
  when the depth ladder collapsed, and given its posture 2026-07-31:
  **everything to our community, nothing outside it** (§Goal & vocabulary,
  "Community"). What made this phase risky — a credential with a lifetime, a
  delegation chain and a revocation story, plus an authorization decision
  computed from *gossiped* edges — is gone, because the tier it existed to
  serve is gone. What is left is mostly reuse:
  1. **The four principals as mesh classes** (`federation-access.md`
     §Principals & access, `federation-access.md` §The audience model) —
     *built 2026-07-31*. `Audience.Class` (outsider = zero value = deny, guest,
     member, friend) with positive predicates; both leaking guards rewritten —
     `seedableBlob`'s cache branch is now `aud.ServesCache()` and
     `serveAudience`'s error return denies instead of reading as a full friend
     — and `audienceClause` refuses a non-serving audience in SQL, so the
     fail-closed zero value holds at the storage layer too.
  2. **The membership walk** (`federation-access.md` §The membership rule) —
     *built 2026-07-31*. `MemberKeys` (`federation/gossip.go`, pure) is the
     access-side twin of `BuildNetworkMap`: same store, same branch snipping,
     plus the **mutual-edge** condition, with our own `federation_peers` friends
     admitted unconditionally. Memoized on the node with a mesh-address index
     (`federation/membership.go`) and recomputed on the sweep from the same
     peers+edges the retention walk reads — two walks, one read, so the store
     and the perimeter cannot drift.
     `TestMapDrawsAOneSidedEdgeThatMembershipRefuses` pins the one place the two
     walks disagree. A memo-ordering race found while verifying item 6 was fixed
     with it (`federation-access.md` §The membership rule, "The memo is ordered
     by when its inputs were read").
  3. **Serve members, refuse outsiders** — *built 2026-07-31*.
     Madnetwork-scoped blobs, manifests, catalog, **holdings and cache blobs**
     to members (`federation-swarm.md` §Distribution — the swarm's only
     boundary is the madnetwork); an outsider gets 404 on bytes and 403 on the
     two listings. `guest_playable` no longer overrides scope on the mesh;
     serving guests survives as `madnetwork.serve_guests`, default off.
  4. **Tighten the vocabulary** — *built 2026-07-31*. `ValidDepth` accepts
     exactly the three constants, migration **035** snaps stored `1…n` to
     **Direct friends** *and* explicit `∞` back to inherit
     (`federation-access.md` §Sharing scope, "Nothing about the schema
     changed"), the node default stays `∞` and is now *named* Madnetwork, and
     `share-depth.js` / the access modal / the bulk bar / the settings card
     offer three choices instead of a ladder — with `DepthFriends` labelled
     **Direct friends** everywhere, since "Friends" reads as the community and
     would understate what it restricts.

  Items 1–4 landed as **one change**, as planned: narrowing the values and
  widening the audience are only safe together. On their own they bought a
  one-sided opening — every node served its community and no node could see it
  — which item 5 closed.
  5. **Discovery beyond the friend ring** — *built 2026-07-31*. The bounded
     frontier pull that actually makes other people's libraries visible: friends
     unbudgeted, a few members per cycle rotating on last attempt, foreign
     catalogs capped and coldest-evicted, and a pull-now that jumps both. Cached
     catalogs moved off `federation_peers` onto `federation_catalog_sources`
     (migration **036**), because a node we pull from and a node an admin
     decided about are two different facts. `meshlab reach`, red past distance 1
     since it was written, is the acceptance test.
  6. **Abuse controls for members** — *built 2026-08-01*
     (`federation-swarm.md` §Distribution, "What a member may cost us").
     `seed_rate_kib` was one bucket for everyone, written when a requester was
     always a friend. Now bytes and concurrent serves are each bounded twice,
     per requester and across all non-friends together, with direct friends
     outside both — which is the anti-starvation rule as much as the
     anti-abuse one. The class ceiling is more than `federation-access.md` §The
     membership rule promised, and it is what actually answers a sybil farm: a
     per-identity limit is precisely what N forged keys defeat. Refusal is a
     429, which the swarm reads as "ask another holder" rather than as a fault.
     All four knobs default to unlimited by owner decision — the feature is
     opt-in, and the doc says plainly what that costs.
  7. **Map at scale** (`federation-trust.md` §The network map) — **BUILT
     2026-07-31.** The 3–4-hop default view, zoom that resolves names instead
     of cropping, node/address/name and branch search over the whole component,
     all-paths-between-two-nodes, and the holder → map jump from the library's
     ⓘ expansion. Not access work at all, but it is what makes the revocation
     half of the membership model usable, so it shipped with the phase rather
     than after it.
  8. **A madnetwork page that can hold a community's library** — **BUILT
     2026-07-31.** (`docs/ui/madnetwork-page.md` §Discovery). `/madnetwork` was
     an A→Z drill-down, which was right for a few friends' catalogs and the
     wrong shape for everything the community publishes: on your own library you
     browse because you remember it, on the network you have nothing to
     remember. It now lands on discovery lanes over the merged catalog with
     search above them, the alphabet demoted to *Browse all* and finally
     windowed (keyset-paged + `virtual-list.js`). Built here because F7 is what
     made it urgent — serving members without it meant opening the network's
     libraries into a surface nobody could find anything in.

     Six lanes, eight rows each, a per-source cap on the two a single node's
     volume could otherwise own, `?source=` for one node's shelf, and migration
     **037** (`federation_catalog.first_seen` — otherwise every sync re-dates a
     source's whole library; preserved structurally since the 2026-08-13
     diff-apply, see §Catalog). Its *Most
     held* lane is the first place branch weighting reaches the browse, so it
     lands part of item 10 early — deliberately, because a popularity lane a
     sybil farm can lift is worse than none. Item 10 finished the job the same
     week: gossiped freshness hints, then the same weighting on *Missing here*
     and on version ordering.
  9. **Listener-node tokens** (`federation-access.md` §Principals & access,
     "The capability token"): a home server signs "this bearer is mine until T",
     verified against the self-certifying channel by any node that can place the
     *issuer* in its own community. One issuer, one hop, no chain — the only
     surviving use of a token. **BUILT 2026-08-01** (`federation/token.go`; no
     migration — a token verifies from its own bytes, so issuing one creates
     no state to store, expire or replicate).

     Three decisions settled it, and two of them corrected text that predated
     item 3. The issuer is honoured if we can place it in our **community**, not
     merely in our friend list — the older wording was written when direct
     friendship was the access boundary, and keeping it would have made a
     madplayer reach strictly less than the server vouching for it. The bearer
     gets **membership, never friendship**: a recording restricted to
     hand-picked nodes stays off a device this admin never picked, and the fact
     that its home server could fetch and relay those bytes anyway is a
     statement about *its* behaviour, which `federation-access.md` §"Why the
     ladder collapsed" is precisely about not pretending to control. And the
     **lifetime is one hour**, renewed at half-life, which stopped being a hard
     question once it was clear the expiry is not the main revocation mechanism:
     the issuer's standing is re-checked on every request, so blocking a home
     server takes its bearers with it instantly, and the hour only has to cover
     a node revoking *its own* user.
  10. **Trust-weighted popularity** (one branch = one voice,
      `federation-trust.md` §Trust graph), which only becomes meaningful once
      carriers are not all direct friends. **BUILT 2026-08-01**
      (`federation-trust.md` §Trust graph, "Where the weighting applies"). No
      migration: the attribution is computed from the gossiped graph at request
      time, which is also what keeps a block instant — snip the branch and its
      voices are gone from the next ranking, with nothing cached to invalidate.

     The half that mattered was not a lane at all: a track row's **versions**
     were ordered by raw holder count, and `renditions[0]` of the leading one is
     what Play, Queue and Materialize act on — so the mislabel defense of
     `federation-trust.md` §Trust graph point 2 was missing from the one
     control people press. Ordered by voices now, with *Missing here* joining
     *Most held* as the second weighted lane and the other three deliberately
     left alone (each for a stated reason, in `laneWeighted`). The counting rule
     became one function so no surface can apply half of it.

     Its other half — **freshness for holders we never ping** — is **built
     2026-08-01** (§Availability, "Two clocks, two windows", migration
     **038**). It turned out to be a bug report rather than an enhancement: item
     5 made most sources members, and members were still judged by a window
     sized for the one-minute friendship ping, so a two-hop member's tracks
     showed for about three minutes in every fifteen. Two layers answer it —
     the window is now chosen per row by the cadence of whatever observes that
     node, and a friend's ping reply carries first-hand ages for the nodes *it*
     pings, so a friend-of-a-friend is watched once a minute and earns the tight
     window rather than being granted the wide one. Never transitive pinging,
     and never a relayed hint: one hop, first-hand, or nothing.

     What neither half claims to answer is **volume from a single honest
     branch** — one friend with fifty thousand badly tagged albums is one
     voice and fifty thousand rows. That is clustering, not weighting, and it
     stays open in `docs/ui/madnetwork-page.md` §Open.
- **Cleanup — remove the node-key → local-user mapping: DONE 2026-08-13**
  (`federation-access.md` §Principals & access). Replaced by the plain
  per-peer guest-only flag (migration 047 backfills the effective audience and
  drops `user_id`); `PeerAudience` is gone entirely — the audience derives
  from the peer row in `serveAudienceKey` — and the `/admin/network` control
  is a checkbox.

  **Why the split** (decided 2026-07-26, superseding the single F6): the two
  halves have opposite risk profiles. F6 is additive and observational — new
  endpoints, a new page, no change to what leaves the node — while F7 rewrites
  the access rule that F5 just established and introduces a credential with a
  lifetime, a revocation story and a delegation chain. Shipping them together
  would mean the riskiest change in the project arriving inside its largest
  phase. The ordering is also the doc's own rule from `federation-trust.md`
  §Trust graph: a network you can see further into than you can defend is the
  wrong order, so defense first is not merely convenient sequencing — F7 is
  *unsafe* without F6, and F6 is useful without F7.
- **F8 — Quality upgrades** (designed **and built** 2026-08-02, §Quality
  upgrades — the four shaping decisions and the reasoning are there, not
  here). Three items over one shared **audio-identity join** (hash, then
  fingerprint head; never text):
  1. **Match arm on the review card** — other tagsets for this recording,
     branch-weighted, plus remote renditions ranked against ours. Advisory.
  2. **Fingerprint-vs-tagset mismatch warning** — two oracles, the network's
     dominant label and AcoustID/MusicBrainz, the external one fired when a
     moderator expands a card. Warns, never acts.
  3. **Quality-upgrade scan and page** — re-run on every catalog sync beside
     `checkClaims`, with the fingerprint stage bounded to what changed since a
     per-source watermark; findings stored (migration 039) with dispositions
     that survive a rescan; materializing is additive, via the existing download
     path.
- **F9 — Making it a swarm** (designed 2026-08-09 and **complete 2026-08-13**:
  items 1 and 2 built the same day, item 3 on the 12th, item 4 on the 13th
  — `federation-swarm.md` §Distribution
  "Making it a swarm" carries the four items, the decisions each took and the
  traps; not repeated here). F4 parallelises a fetch across holders but does not
  let the swarm grow, because a downloader seeds nothing until it finishes. Four
  items: **partial seeding** (a chunk-availability endpoint + serving Ranges out
  of the `.part`) ✅, **holdings announce** — a direct push to friends,
  first-hand and never relayed, which is why it is *not* gossip ✅, a
  **scheduler** that dispatches on in-flight bytes and measured throughput
  instead of round-robin, gives the dial its own deadline, selects the
  (chunk, holder) PAIR now that a holder may be partial, reworks
  `worseThanPeers` with it and carries the two manifest hardenings ✅ (a dead
  holder in a plan went from ~150× the clean fetch to about five seconds of
  overlapped dial) — and **endgame hedging + pipelining** ✅, which race a
  second copy of the chunk a slow holder is sitting on (4.84 s → 108 ms on the
  tail) and bound what one holder may be asked for at once. That last part is
  the one place F9 contradicted its own design: it asked for a request-depth
  FLOOR per holder and the measurement produced a ceiling, because past two
  concurrent chunks a capped link spends `Timeouts.PerChunk` rather than
  bandwidth — and a plan of four holders with one answering reaches that on its
  own. Item 3 must not be designed before item 1 lands; item 4 was independent
  of everything and was deliberately not pulled forward.
- **F10 — Merkle verification** (decided **and parked** 2026-08-09,
  `federation-swarm.md` §Distribution "Merkle verification" — the two
  decisions, the costs and the one counterintuitive turn are there). Fixed 16
  KiB leaves and a per-blob merkle root beside the content hash, taking BT v2's
  *verification structure* without its identity model or its wire protocol. It
  unwelds verification, transfer and seek granularity — so it **buys**
  chunk-size flexibility rather than costing it — scales to video, and lets a
  partial holder serve proofs. It does **not** buy trust: a peer-supplied root
  is exactly as trustworthy as a peer-supplied chunk list, so nobody should
  reach for this to fix a lying manifest. Picked up on either of two triggers:
  **video support**, or a **measured** all-partials reassembly failure.
- **Later (decided-deferred):** subscribe→replicate with storage caps,
  announce/gossip of catalog deltas (the *library* sibling of F9 item 2's
  holdings announce — see the discovery-speed question in
  `.issues/open-issues.md`, which argues the pair should be decided together),
  S3-backed swarm storage.

## Open questions (design-time details)

1. **How much of the frontier to pull and cache** (F7, §Discovery beyond the
   friend ring). The shape shipped with item 5; the *numbers* are still guesses.
   Four member catalogs per 15-minute cycle and a cap of 200 foreign catalogs
   are what a node runs with today, tunable per node as `[federation]
   discovery_budget` / `discovery_cap`. Nobody can pick them from first
   principles — they want a real network to observe, and the thing to watch is
   whether the rotation fills the frontier faster than the network changes. If
   it does not, the recorded upgrade path is signed catalog-digest relay, which
   makes "which node changed" free.

(Former question 1 — catalog crossing / one tagset text on two recordings —
was settled with F2: see §Catalog, ""N versions"". Former question 2 — chunk
size & Merkle parameters — was settled with F4: adaptive, self-describing
chunk size in the manifest; whole-file hash is the anchor, per-chunk hashes are
for early verification only. See `federation-swarm.md` §Distribution. Former
question 3 — gossip payload, propagation and ageing — was settled 2026-07-26
ahead of F6: see `federation-trust.md` §Friend-list gossip, including the
privacy half. Former question 4 — listener-node token lifetime and renewal
cadence — was settled 2026-08-01 with F7 item 9: one hour, renewed at
half-life, because community standing already revokes a whole issuer instantly
and the expiry only has to cover a home server revoking its own user. See
`federation-access.md` §Principals & access, "The capability token".)

Decided-and-deferred: **replication** (subscribe/favourite → mirror, storage
caps) stays out of v1 — manual download-to-library already makes a node a
holder; the automatic version is a clean later add-on.

## Related

- `docs/architecture/auth.md` §8 (federation hooks), §9 (sharing scope,
  audit), §10 (Phase 4).
- `docs/architecture/recordings.md` §"Federation" (cross-node fingerprint
  index, rendition negotiation).
- `docs/architecture/recording-tagsets.md` (the per-recording tagset payload the
  catalog carries; provenance, trust weighting, access never imported).
- `docs/ui/madplayer.md` (madplayer — the client that consumes federation).
- `madshare.org` (concept: trust system, sharing scopes, spam/war concern).
