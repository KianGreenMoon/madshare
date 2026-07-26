# Madnetwork federation — design

> **Status: agreed 2026-07-18; F0 (groundwork), F1 (friendship), F2 (catalog),
> F3 (direct transfer), F4 (swarm) and F5 (depth & scope) are built.** The remaining items in
> §Open questions are design-time details to settle during the respective
> milestones, not blockers. Federation
> is auth Phase 4 (`docs/architecture/auth.md` §8) and the milestone the native
> client (`docs/ui/native-client.md`) exists to use.

## Goal & vocabulary

**Madnetwork** is the peer-to-peer federation of madshare nodes: node A can browse,
stream, and download node B's shared library, and nodes jointly distribute the
bytes swarm-style. Guiding stance: **minimum restriction for people inside the
network, nothing for people outside**, and the network itself is **transparent by
default** — its social graph is visible to its members.

- **Node** — one madshare instance, identified by its Yggdrasil keypair. Servers
  and personal madplayer instances are both nodes; a madplayer is just a node
  that is usually single-user and intermittently online.
- **Friend** — a mutual trust relationship between two nodes, established by
  exchanging node cards (address + public key). The trust graph is built from
  these edges.
- **Trust depth** — how far along the friendship chain something is shared:
  `0` = direct friends only, `1` = friends of friends, `2` = one hop further,
  `∞` = the whole reachable madnetwork. **Default: ∞** (transparent network);
  the knob exists so an admin can tighten as the network grows.
- **Gossip** — information spread node-to-node rather than from a central place:
  each node tells its friends, who tell theirs. Three distinct uses, deliberately
  kept apart. **Friend-list gossip** (F6) — B tells A whom B is friends with, so
  A can see the graph past its own friend list; the network map, branch snipping
  and distrust marks all read it, and `Audience.Distance` (F7) is a hop count in
  it. **Freshness-hint gossip** (F7) — a friend relays *its* friends' `last_seen`
  as a second-hand claim, so availability survives past one hop without pinging
  strangers (§Availability). **Catalog-delta gossip** (deferred) — pushing
  library changes instead of pulling snapshots; an optimisation, unrelated to the
  other two. Despite the name none of these is a push protocol here: they ride
  the existing periodic pull (§Catalog), and the word describes how information
  travels, not the transport. Because a friend list names third parties who never
  agreed to be named, its payload is a privacy decision as much as a protocol one
  (§Open questions).
- **Full peer** — a node: participates in catalog exchange and the swarm.
- **Thin client** — a browser user. Thin clients are *not* madnetwork
  participants; they are local users of exactly one home node, which acts as
  their gateway.
- **Listener node** (planned) — a madplayer: a person's device that runs a node
  and swarms like a full peer, but signs in to a home server with **user
  credentials** instead of being friended, and **publishes no catalog** — its
  library stays private to the device. Consumption is one-way; the only route
  from that library into the network is an ordinary upload to the home server.
  See §Principals & access.

## Identity & transport

- **Identity = the Yggdrasil node key** (ed25519). The derived `.ygg` address is
  self-certifying — proving you hold the address proves you hold the key — so the
  trusted-peer table is just a table of peer keys/addresses. No PKI.
- **Transport = yggdrasil-go embedded as a library**, routing madshare's
  federation protocol over the mesh, without a system TUN (mobile madplayer
  must not need `VpnService`/`NetworkExtension`). **Confirmed by the F0 spike
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
- **Local yggstack fork (F4).** yggstack's `YggdrasilNIC.writePacket` read every
  outbound packet into a **single shared `writeBuf`**, which gVisor drives from
  several goroutines at once — a data race that stays dormant with one
  connection but is triggered reliably by the swarm's parallel chunk fetches
  (F4). We carry a one-line fix in a local fork (`third_party/yggstack`, wired
  by a `replace` directive): each `writePacket` call takes its own buffer from a
  `sync.Pool` (the mesh write path below it — `ipv6rwc`/`core.WriteTo` — is
  already mutex-guarded, so per-call buffers suffice and keep sends parallel).
  Drop the `replace` if the fix lands upstream.
- **Build option:** the `nofederation` build tag (mirroring `nowebui`) compiles
  all federation code and its dependencies (yggdrasil, gVisor) out, producing
  a standalone server; such a build aborts startup if the config enables
  federation.
- The same key signs application-layer artifacts where needed (capability
  tokens, distrust marks). Plain reads between direct friends need no extra
  signing — the channel already authenticates both ends.

## Principals & access

- **Node-level trust is the default relationship.** A friend node is trusted as a
  unit; its internal user model is its own business.
- **The node-key → local-user mapping is being removed** (decided 2026-07-26;
  built and still present — `federation_peers.user_id`, `PeerAudience`, the
  user-mapping control on `/admin/network`). It let an admin bind a friend node's
  key to a local account so that node was answered with that account's rights.
  It came from misreading "authorize the node as a user": the requirement was
  never *a node acting as an account*, it is the **listener node** below — a
  person who signs in with credentials, from a device that happens to also be a
  mesh node. Two consequences to handle when it goes: the `GuestOnly` half of the
  audience is derived from it today (see the open detail under §Sharing scope),
  and the removal needs a migration.
- **Unmapped friends are not a special case** (decided 2026-07-18): a friend
  node *without* a user mapping is treated as a **default regular-user
  identity** — it may see and fetch whatever a plain `user`-role local account
  may. The mapping is the per-friend *override* (more or less than the
  default), not a prerequisite. Deliberately a rule, not a magic local account
  row — nothing to log into, rename, or accidentally delete. Since F5 this is
  enforced as the `GuestOnly` half of the audience (§Sharing scope): unmapped
  and mapped-with-`content.access` friends see the full published set, a friend
  mapped to an account without it sees only guest-accessible recordings — in the
  catalog and at the byte endpoints alike.
- **Thin clients have no madnetwork access by default.** Madnetwork browsing is a
  new permission (working name `madnetwork.access`), granted to admin by default
  and grantable to trusted local users. The header section for the madnetwork
  library is server-side gated like every other link.
- **Planned — split `madnetwork.access` in two** (raised 2026-07-26). One
  permission gates two things whose costs are nothing alike: *looking* at the
  merged catalog, which reads rows that were synced anyway, and *making this node
  fetch and cache remote bytes for you*, which spends its disk (the madnetwork
  cache has no eviction) and its bandwidth. The permission was created for the
  second and is being spent on the first. Listener nodes sharpen the mismatch:
  one browses through the server but fetches for itself, so it wants the cheap
  half and never the expensive one. Proposed shape — keep `madnetwork.access`
  meaning **browse** (no rename, no migration, no role churn: 027 already grants
  it to admin and the stackable `madnetwork` role) and add **`madnetwork.relay`**
  for the stream/materialize path; grant browse widely, relay narrowly. A
  per-user cache quota is the natural companion and the honest answer to overuse
  — the permission is a blunt instrument standing in for one.
- When a thin client with the permission plays a non-local file, **the server
  fetches it into a cache directory and relays it** — as *cache-through
  streaming*: chunks are fetched in sequential priority and served to the
  browser as they arrive, while the complete file lands in the cache in
  parallel. Never build the blocking download-fully-then-play version.

### Listener nodes — madplayer (planned, not built)

A madplayer is a person's own device: a player that also runs a federation node.
It joins the network **as a person rather than as a friend**, which makes it a
third kind of participant beside the full peer and the thin client (decided
2026-07-26; supersedes the node-key → local-user mapping above).

- **Credentials, not friendship.** It signs in to a home server with an ordinary
  account — session or API token, the same auth a browser uses. No node card, no
  admin accept, no `federation_peers` row. Its rights are that account's rights,
  so federation still adds no parallel permission system.
- **The content flow is one-way, by construction.** It consumes — browse,
  stream, materialize, bounded by the account's ACLs. It publishes **nothing**:
  its local library is never catalogued, advertised or pulled. That library is
  unmoderated personal content on somebody's phone, and the network has no basis
  to vouch for it. This is a property of where the content lives, not a setting
  to relax later.
- **The one way in is an upload.** A user holding `file.upload` uploads from the
  device to the home server, through the review bucket like any other upload.
  What the network then sees is the *server's* published content under the
  server's identity — reviewed, fingerprinted, attributable. The device is never
  the publisher.
- **It is a full swarm member regardless.** Its own key, on the mesh, fetching
  chunks from many holders and seeding back what it fetched, discovered like any
  other node. Safe for exactly the reason cache blobs are exempt from the
  fingerprint rule: serving a hash claims *possession of bytes*, never an
  identity, so a seeder asserts nothing anyone has to trust. One-way publication
  and two-way swarming are not in tension — the swarm carries bytes, the catalog
  carries claims, and only the second needs vouching for.
- **Token-carrying, not relay-only** (decided 2026-07-26). Fetching everything
  through the home server would have been the cheaper first version and was
  rejected: madplayer is unbuilt, so it gets built properly. This makes **F7
  capability tokens a prerequisite** — to its home server's friends a madplayer
  is a stranger, and a stranger reaches only the guest-open swarm. The token is
  how a home server says "this bearer is mine", which makes madplayer the
  motivating case for delegated issuance rather than an abstract one.
- **Thin clients stay out of the swarm** (decided 2026-07-26). A browser user
  remains a pure consumer relayed by its home node. Browser tabs have no durable
  storage, no stable address and no lifetime; enrolling them would complicate
  the swarm and buy nothing.
- **Future — the home node as introducer.** Both ends are on yggdrasil, so a
  server could broker a direct connection to its own listener users instead of
  carrying their traffic. Recorded as madplayer's direction; not part of this
  plan.

Client-side behaviour — playlist sync, and what the app does with items the
server cannot resolve — is in `docs/ui/native-client.md`.

## Sharing scope (F5, built)

- Node-level default scope per admin: share with madnetwork / friends / nothing
  (madshare.org). On top of that, content carries a **share depth** (see
  vocabulary): an admin can mark parts of the library friends-only (`0`),
  friends-of-friends (`1`), etc. Default is the node's default scope, default
  `∞`.
- **Enforcement honesty:** depth is technically enforced at hop 0 — *we* decide
  whom we serve and for whom we issue tokens. Beyond that, depth limits are
  honored by trusted nodes following the protocol. That is acceptable by
  construction: the only nodes that ever hold the content are trusted ones, and
  a friend who re-shares applies their own judgment anyway ("torrents do not
  know borders"). What the depth knob really controls is how far *we* actively
  push visibility and issue access.

### The audience model

Every mesh request that reveals or delivers library content is answered *for an
audience*, and the same audience decides both halves — **catalog and bytes
together**, so the node never advertises what it would not serve (the F3 rule,
now enforced per requester instead of uniformly):

```go
type Audience struct {
    Distance  int   // friendship hops: 0 = a direct friend
    GuestOnly bool  // only guest-accessible recordings
}
```

- **Distance** is compared against the content's share depth: a recording is in
  the audience's catalog iff `depth >= Distance`. Depth `0` (friends only) serves
  a direct friend and nobody beyond; `DepthPrivate` (`-1`) serves nobody at all,
  including direct friends — it is the "not on the network" mark. Until
  transitive reach turns on (F7) every authenticated requester is at distance 0,
  so the ladder above 0 is inert *by construction rather than by omission*: the
  depth is stored, enforced, and carried on the catalog wire today, so F7 adds
  reach without a protocol break or a schema change.
- **GuestOnly** is the per-friend half, resolved from the **user mapping**
  (§Principals & access): a friend mapped to a local account inherits that
  account's rights, and since the local model grants either `content.access`
  (the whole library) or nothing beyond the guest-playable/license policy, the
  mapping collapses to exactly this bit. An **unmapped** friend is the *default
  regular-user identity* — `GuestOnly: false`, i.e. the full published set —
  per the 2026-07-18 decision that unmapped is a rule, not a missing row. So the
  mapping is what an admin reaches for to give a friend *less*.

  **Open detail — what `GuestOnly` reads once the mapping goes** (2026-07-26).
  The mapping is being removed (§Principals & access), and it is the only thing
  that sets this bit today. It is also the only **per-friend** restriction in the
  model: share depth is per *content*, so it cannot express "friend X sees less
  than friend Y". Either that axis is dropped — every friend then sees the whole
  published set within depth, and `Audience` collapses to `Distance` alone — or a
  plain per-peer *guest-only* flag replaces the account binding, keeping the
  capability without pretending a node is a user. The audience model itself is
  unaffected either way; only where the bit comes from changes.

**Where depth lives: on the recording.** Access already lives there
(`license`, `guest_playable` — one audio identity, one license,
`docs/architecture/recording-tagsets.md` decision 9), and sharing is ultimately
about *bytes*, which are per-recording renditions; hiding one appearance of a
recording while serving another would leak the same blob under a different name.
`recordings.share_depth` (migration 030) is `NULL` by default, meaning **inherit
the node default** (`madnetwork.default_share_depth`, a runtime setting on
`/admin/settings`, default `∞`). One override level over one node default — no
artist/album inheritance chain, deliberately: a resolution chain would land in
every catalog and blob-serve query for expressiveness nobody has asked for. Bulk
selection in the Recordings and All Appearances lenses covers "a whole artist"
in practice.

**Per-audience snapshots.** The memoized own-catalog (§Catalog) is no longer one
global snapshot: it is memoized **per audience class**, not per peer. At F5 there
are exactly two classes (full and guest-only at distance 0), so the cost is
bounded and the serial keeps its meaning — each peer already stores the serial of
the snapshot *it* was served, so the not-modified check works unchanged.

**Guest-playable is an open swarm.** A recording that is guest-accessible
(explicitly flagged or via the license policy) serves its **blobs and manifests
to any mesh node**, friend or stranger — no friendship, no token. This is the
deliberate exception in §Sharing scope's legal frame: guest-playable content is
open to everyone and the admin who flags it owns that choice, exactly as it is
already open to anonymous HTTP callers on `/files/*`. Everything else stays
default-deny toward strangers: the **catalog** and **holdings** endpoints remain
friends-only (a stranger gets no listing — they must already know the hash), and
**cache blobs are never open** (this node cannot vouch for the license of
something it merely fetched). A blocked peer is refused everything, as before.

**Tokens ship with F7.** Capability tokens exist to serve
strangers-inside-the-network at depth ≥ 1, and a friend-of-a-friend cannot
*discover* that we hold a hash until friend-list/catalog gossip lands. Building
the issuer before the discovery path would ship a signed credential nothing
presents, so tokens follow the gossip that gives them a counterparty rather than
preceding it (decided 2026-07-25; the gossip itself became F6 and the tokens F7
when that phase was split on 2026-07-26). Depth enforcement at hop 0 — the part
that is real today — is F5.
- The **legal frame** (madshare.org): sharing among friends is private sharing;
  non-friends cannot listen. Default-deny toward the outside world is preserved
  end-to-end — an outsider gets no catalog, no stream, no swarm chunks.
  Guest-playable content is the deliberate exception (open to everyone, no
  token), and the admin who flags it owns that choice.

**Planned — no fingerprint, no publication (not built; decided 2026-07-26).**
The published set gains a third condition beside "approved and live" and the
audience filter: a rendition is publishable only when its blob has an
`audio_fingerprints` row. Requiring `fpcalc` to *start* a federated node closes
the tool-missing case; this closes the per-file remainder — a blob that was never
successfully fingerprinted (ingested before the tool existed, corrupt audio, a
codec `fpcalc` chokes on) would otherwise still be advertised with an audio
identity nothing local ever verified. Publishing it asks friends to trust a
grouping claim this node cannot itself stand behind.

Design notes for the implementation:

- **Rendition-level, not recording-level.** Each rendition is a distinct blob;
  one being fingerprinted says nothing about another, and a `recording_pinned`
  rendition can join a recording by hand without ever being analysed. A recording
  whose renditions are all unfingerprinted then has nothing to advertise and
  drops out of the catalog on its own — no separate recording-level rule.
- **One place, both halves.** The condition belongs in `visibleTagset` /
  `selfPublishedClause` (`database/madnetwork_scope.go`), so the catalog,
  `BlobVisibleTo` and the `/madnetwork` self-merge inherit it together. Never
  advertise what you would not serve applies here exactly as it does to depth.
- **Holdings are untouched.** `GET /madnetwork/v0/holdings` advertises cache
  hashes — "I hold these bytes", not "this is that recording". There is no
  identity claim to back, and the fetcher verifies the hash regardless.
- **Say it in the UI.** A recording silently missing from the network page is a
  support question; the Recordings lens should show *why* (no fingerprint yet /
  analysis failed) next to the existing scope chip. Most cases resolve
  themselves — the startup backfill re-analyses anything lacking a fingerprint —
  so the state is usually "not yet", and it should read that way.
- **Expect the serial to churn** on a node that federates for the first time with
  a large unanalysed library: entries appear as the backfill lands, so friends
  re-pull the snapshot repeatedly for a while. Harmless at the 15-minute sync
  cadence, worth knowing before someone reports it as a bug.

## Friendship (F1, built)

- **Node card** — the out-of-band introduction two admins exchange (chat, mail,
  any channel they trust): a small JSON blob
  `{"madshare_node_card": <protocol>, "name": "…", "public_key": "<hex>"}`,
  exported (copy/download) from `/admin/network`. It deliberately carries only
  identity — underlay connectivity is `[federation]` config's business (public
  mesh or explicit `peers`/`listen`), not the card's. `[federation].name` sets
  the display name (host name when unset); identity is always the key.
- **Trusted-peer table** (`federation_peers`, migration 026): one row per known
  node — key (identity; the mesh address is derived, never stored), local
  label, state, `last_seen`, and the optional **user mapping** (`user_id`) that
  binds a personal madplayer node to a local account (§Principals & access).
  States: `pending_outgoing` (we imported their card) · `pending_incoming`
  (their node introduced itself, awaiting our accept) · `friend` · `blocked`
  (with the pre-block state remembered for unblock).
- **Pairing handshake** (`POST /madnetwork/v0/pair` on the mesh): a node
  introduces itself with `{protocol, name, public_key}`. No signatures — the
  mesh address is derived from the node key, so the connection's source address
  *is* proof of key possession; the handler additionally verifies the claimed
  key derives to exactly that source address. Receiving a pair request from a
  `pending_outgoing` peer proves mutual intent → both flip to `friend`; from an
  unknown key it records `pending_incoming` for the admin. A background
  **refresh loop** (1-minute tick, nudged on import/accept) retries outbound
  pairings and pings friends, so both sides converge through any offline
  window and `last_seen` stays fresh. **Friending is deliberate** by
  construction: a node becomes a friend only after *both* admins acted — and
  accepting an incoming request shows the full key so the admin can check it
  against the card received out-of-band (never a blind one-click).
- **Blocking (local effect, F1):** a blocked peer is refused the *entire*
  protocol surface (even ping, HTTP 403) by the mesh-side auth wrapper.
  Unblock returns the peer to its pre-block state. Distrust marks, branch
  snipping, and de-peering arrive with F6.
- **Admin surface:** `/admin/network` (own card, import form, peer list with
  accept/block/unblock/remove/rename/user-mapping; pending-request badge on the
  dashboard) over `/api/admin/federation*`, all gated `federation.manage`.

## Trust graph, transparency & defense

- **Transparency:** nodes gossip their friend lists (within trust depth), so
  every admin can see the reachable network as a graph — who is connected to
  whom — in an admin UI (network map).
- **Blocking ("snipping a branch"):** an admin can block any node key. Blocking
  is **manual** — there is deliberately **no automatic rating/critical-mass
  system in v1** (an automatic reputation score is a weapon for intra-network
  wars; madshare.org's own worry). Instead:
  - A block cuts all application-layer service instantly: no catalog, no
    streams, no chunks, no token issuance; existing tokens expire on their own.
  - Where the peering link is ours (direct ygg peering with that node), we also
    de-peer, so the blocked node loses us as transit. (On shared public-mesh
    segments, transit below the app layer is Yggdrasil's business — the
    app-layer cut is the guaranteed part.)
  - Blocks are **published as signed distrust marks**, visible to friends: "see
    whom your friends don't trust." Friends factor that in manually; nothing is
    automatic.
  - Blocking a node also snips the *branch* behind it — nodes reachable only
    through the blocked node drop out of our view; nodes also connected via
    other friends remain.
- **Stolen-key scenario:** the same mechanism — block the compromised key,
  publish the distrust mark; the network routes/trusts around it.
- **Mislabeling / spam (the "rickroll" problem)** — a tagset claiming one thing
  attached to audio that is another. Layered defense, mostly structural:
  1. Because tagsets attach to **recordings** (audio identity), a mislabel on
     known audio lands on the *true* recording and becomes a visibly absurd
     **minority label** next to the dominant honest tagsets — it does not
     create a fake track. Auto-flag tagsets that conflict with a recording's
     dominant label. The attack surface shrinks to rare/unknown audio.
  2. **Popularity is trust-weighted, never raw counts** (sybil resistance):
     carriers are weighted by trust distance, and nodes reachable only through
     one friendship edge count as **one branch**, not many voices. A sybil
     farm inflates nothing and dies with a single snip.
  3. **Attribution:** every tagset carries signed provenance + the friend path
     that delivered it. Detect → details → block → branch snipped, distrust
     mark published. A troll gets each admin at most once and grows more
     visible with every hit.
  4. **Independent ground truth (reuse):** the review card runs the existing
     tag-suggestions machinery (local fingerprint → AcoustID → MusicBrainz)
     and **warns on mismatch** ("tagset says X; fingerprint says Y"), with the
     preview player right there. Optional (needs the AcoustID key), but an
     oracle outside the social graph entirely.
  5. **No global view to poison:** your catalog is your friends' choices
     bounded by your depth knob. Trolls can flood their own corner of the
     network; they cannot dilute yours — which is exactly why rating stays
     local/manual and never network-global.

**Planned — report contradicted identity claims (not built; decided
2026-07-26).** A peer's catalog makes claims this node can *check*, and when a
check fails the admin should hear about it with the evidence attached. This is
the "Detect → details" arm layer 3 promises and nothing implements; the
"→ block → snip → publish the mark" half is F6's existing toolkit. A false audio
identity is worth singling out because it is **provable** — unlike a tasteless
tagset, it is arithmetic — which is exactly what makes it fair to put in front of
an admin as grounds for blocking.

What is checkable, cheapest first:

- **Against blobs we already hold — no download, no request.** For a hash in our
  own library we know the true fingerprint. A peer advertising that hash with a
  materially different one is contradicting bytes we can hash ourselves. Runs at
  catalog-sync time and costs a comparison. This case is **airtight**: identical
  bytes cannot fingerprint differently.
- **Against a materialized download.** The pipeline already re-fingerprints
  fetched audio before it joins a recording (§Catalog); compare the result with
  what the origin advertised, and the check is free where the work is done.
- **Against the peer's own grouping — needs no wire change at all.** A
  `recording_key` asserts "these renditions are the same audio". Hold two of them
  and the assertion is testable locally without the peer's cooperation.

**Never automatic.** Blocking stays manual, for the reason given above: an
automatic reputation score is a weapon in intra-network wars. A report is
evidence shown to a human — the peer card on `/admin/network` grows a warning
carrying the hash, both fingerprints, and how each was obtained, next to the
Block action already there. Nothing about what the peer is served changes until
an admin decides.

**Say "contradiction", not "lie".** Innocent explanations are more common than
malice: a different chromaprint build (`audio_fingerprints.algo_version` exists
precisely because fingerprints are version-sensitive), a peer that associated a
rendition with the wrong recording through its own sloppiness, or — once F6
gossip and F7 reach land — an honest relay repeating someone else's claim, which
makes the *origin* of a claim a separate question from its *carrier*. Only the
same-hash case above is airtight; the fuzzier ones are BER comparisons against a
threshold and must be worded as such. Present a conflict and its provenance,
never a verdict.

Storage is one row per (peer, hash, claim) with an admin disposition
(new / dismissed / acted on) so a repeating sync re-alarms nobody — a new
migration, 031 at the earliest, since 030 is `share_depth`. A count badge on the
dashboard alongside the pending-peer one is the whole notification design; this
must not become mail.

**Prerequisite: the catalog has to carry the fingerprint claim.** §Catalog
describes entries as carrying one, but the F2 wire never added it —
`CatalogEntry` has tagset text plus renditions with quality facts and nothing
else. It is an additive JSON field, so no protocol break, and an absent claim is
simply uncheckable rather than suspicious. Note this is also what layer 1's
"auto-flag tagsets that conflict with a recording's dominant label" needs to work
across nodes.

The *byte*-level lie needs nothing here: bytes that do not hash to the requested
hash never enter the cache and cost the provider its place in the swarm
(§Distribution). This item is about claims that survive byte verification.
- Transitive reach (depth > 0) **never ships before** the transparency and
  blocking tooling — a network you can see further into than you can defend is
  the wrong order. This is the reason the build plan puts defense in F6 and
  reach in F7, in that order, rather than in one phase: the dependency runs one
  way only, so F6 stands alone and F7 does not.

## Catalog & the madnetwork library

- The madnetwork library is **its own section/page**, permission-gated
  (`madnetwork.access`). The home page stays the local library.
- Catalog entries are per-recording **tagset payloads** as designed in
  `docs/architecture/recording-tagsets.md`: the recording (audio identity,
  fingerprint claim, renditions with quality facts) plus its appearances
  (tagsets), each with **origin-node provenance**. Access is never imported
  from a tagset. Entries also list **known holders** of each rendition's
  content hash — this is the swarm's tracker (see §Distribution).
- **Remote claims are hints, never facts.** A peer's fingerprint or recording
  grouping is used for discovery and display only. On download the bytes are
  verified against the content hash chunk-by-chunk, and the fingerprint is
  recomputed locally (`fpcalc`) before anything merges into a local recording.
  That local recomputation *is* the guarantee, so it is a hard requirement
  rather than an enrichment: a node with `[federation].enabled` **refuses to
  start** when `fpcalc` is not on `PATH` (override:
  `[federation] allow_missing_fingerprinting`, which accepts importing and
  re-publishing unverifiable content). `ffprobe` stays optional — without it the
  published catalog carries no quality facts and friends cannot rank this node's
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
- **Uploads gain the same view.** Because uploads flow through the same review
  cards, an uploaded file's card can additionally show **madnetwork matches**
  for its recording (spotted via fingerprint match against synced catalogs) —
  other tagsets and better renditions — with the same fetch-and-add actions.
- **Quality-upgrade page (optional, unobtrusive).** A dedicated admin page that
  scans local recordings against the synced catalogs and lists the ones for
  which the madnetwork holds a **ladder-better rendition**, with fetch-and-add
  (through the same review flow). Strictly **additive** — the existing
  rendition is never touched automatically; the admin may soft-delete the old
  one (Trash, normal quarantine) to keep only the best. No nagging: a page you
  visit, at most a quiet count badge.
- **Sync mechanism = pull-and-cache (built, F2).** Periodically (15 min) and on
  new friendship, a node pulls a friend's catalog over the mesh
  (`GET /madnetwork/v0/catalog?since=<serial>`, friends only — default-deny
  toward everyone else) and keeps a local copy (`federation_catalog`, one row
  per remote appearance, denormalized text — remote ids are opaque, never
  joined onto local entities). **Snapshot + not-modified**, not row deltas
  (decision 2026-07-18, superseding the earlier "changed since serial N"
  sketch): true per-row deltas would need change tracking across five tables
  (a rename changes catalog text), while a personal-scale catalog is a few
  hundred KB — so the serial is a **content hash over the whole snapshot**;
  an unchanged serial gets a tiny "unchanged" reply, a changed one the full
  snapshot, applied as an atomic replace. The wire format carries the serial,
  so real deltas can arrive later without a protocol break. The serving node
  memoizes its own snapshot (~1 min) so friend syncs don't rebuild it per
  request. A friend's catalog cache is **retained** regardless of reachability
  (never TTL-purged); the friend carries a **"last seen"** indicator, and whether
  their exclusively-held tracks appear in the *merged* madnetwork view is decided
  at request time by the availability predicate (§Availability & node health) —
  storage and visibility are separate concerns. What a node
  publishes is its **whole approved live library** minus what the requesting
  audience may not see — per-content share depth and the per-friend user mapping
  (§Sharing scope, F5); the snapshot is memoized per audience class rather than
  globally. Push/gossip of changes is a later optimization, not v1.
- **Playback needs a holder, not the origin.** Because the swarm is keyed by
  content hash, an offline friend's tracks stay playable whenever *any*
  reachable node holds the hash. With network scale (many redundant
  libraries), most entries have multiple holders — availability improves as
  the network grows.
- **Merged view (built, F2).** The `/madnetwork` page — its own header section,
  gated `madnetwork.access`, **shell-native** so local playback survives
  browsing it — shows the **deduplicated union** of all *friends'* catalogs
  (a blocked peer's cache is kept but hidden; unblock restores the view
  without a resync) as a **drill-down mirroring the local library** (artist →
  album → track, album-artist grouping, case-insensitive merge, the same
  Unknown-artist/Other buckets). Identical tagset text offered by many nodes
  collapses to one row; which friend it came from is **not surfaced while
  browsing** — provenance stays stored and appears only in the track's
  expansion (holders + last seen) and the page's sync-status strip. Since F3
  the expansion carries the version actions — Play, Queue, Download to
  library — acting on the version's **ladder-best rendition** (the server
  sorts each version's renditions by the quality ladder before answering).
  At depth 0 every carrier is a direct friend, so the carrier count is
  trivially trust-weighted; the full weighting (one branch = one voice)
  arrives with transitive reach (F7).
- **Catalog crossing — "N versions" (built, F2; resolves former open question
  1).** The same tagset text on *different claimed recordings* (different
  masters, live vs. studio, or a mislabel) stays **one track row** that
  expands into its **versions**. Recordings are **never merged on text**:
  two claims are folded into one version only when they **share a rendition
  content hash** — proof of identical bytes somewhere — otherwise they stay
  separate versions, each with its renditions and holders. Versions are
  ordered most-widely-held first (the default pick; the quality ladder cannot
  rank across different audio). Hint-level fingerprint matching for display
  dedup of *unshared* rips can refine this later; local verification on
  download (F3) stays the truth either way.

## Direct transfer (F3, built)

- **Wire = plain streaming HTTP with Range** (decision 2026-07-18):
  `GET /madnetwork/v0/blob/{hash}` on the mesh, served via `http.ServeContent`
  (native HEAD/Range; `Content-Disposition` carries the origin filename so a
  download lands under its real name). Between two trusted endpoints,
  "chunked" IS HTTP ranges; **integrity is the content hash itself**, verified
  over the full byte stream on the fetching side — bytes that do not hash to
  the requested hash never enter the cache. The Merkle chunk protocol is
  deferred to F4, where multi-source fetch actually needs per-chunk
  verification.
- **Authorization** (decision 2026-07-18): **a friend may fetch any blob its own
  catalog shows it** — never advertise what you won't serve, and vice versa.
  Published = the same predicate as the local library (live file + an approved
  appearance on its recording); a staged, trashed, or unknown hash is 404 even
  for a friend. Since F5 that predicate is evaluated **for the requester's
  audience** (§Sharing scope): share depth and the per-friend user mapping filter
  the catalog and the byte endpoints from the same rule, and a guest-accessible
  recording additionally serves strangers (the open swarm).
- **Fetching** (`federation.Node.EnsureBlob`): one transfer per hash, joined
  by every concurrent requester; providers come from the cached catalogs
  (friends advertising the hash, most recently seen first — tried in order
  until one delivers verifying bytes). A hash the local library holds
  short-circuits to the local blob; a finished cache file is a cache hit.
  Fetches run on the node's lifetime, not the requester's — a browser
  disconnect never abandons a half-fetched file. Cache:
  `<data_dir>/cache/madnetwork/<hash>` (`.part` while running, renamed only
  after verification; no eviction in v1).
- **Cache-through streaming relay** (`GET /api/madnetwork/stream/{hash}`,
  gated `madnetwork.access`): bytes are relayed to the browser as they arrive
  while the complete file lands in the cache in parallel — never
  download-fully-then-play. The total is known up front (the manifest / the
  origin's Content-Length), so browser range requests work against the growing
  file. Reads are **per-chunk, not front-to-back**: a range for a region not
  yet fetched (a player's tail probe for the MP4 `moov`/duration, or a seek)
  **prioritizes the chunk covering that offset** and is served as soon as it
  lands — it does not wait for the sequential prefix to reach it (see
  §Distribution for the seek-priority mechanism).
- **Download to library** (`POST /api/madnetwork/download {hash}`, gated
  `madnetwork.access` + `file.upload`): fetch + stage, exactly as designed in
  §Catalog — the verified file lands in blob storage and inserts as the
  downloader's **draft** carrying the remote entry's tagset text (what the
  user saw and chose; the origin filename is kept). The existing analysis
  pipeline then ffprobes and fingerprints it **locally** and resolves its
  recording — remote claims stay hints. Bytes the library already holds skip
  the fetch: the remote tagset attaches as a new draft appearance of the held
  recording. The **`autoapprove_downloads`** setting (settings key
  `madnetwork.autoapprove_downloads`, admin card on `/admin/settings`, gated
  `user.manage`, default **off**) lands downloads approved as fetched instead.
  Progress is polled at `GET /api/madnetwork/transfers/{hash}`; the download
  job (dedup per hash) survives the requester.

## Distribution (the swarm, F4 built)

- **Swarm ID = content hash.** Blobs are already content-addressed; two
  independently uploaded identical files hash identically and are automatically
  seeders of the same swarm — no coordination, no `.torrent` files. Different
  encodings of the same audio are different swarms; the recordings overlay
  above chooses which rendition (which swarm) to fetch.
- **Chunk protocol: a lean chunk-exchange over ygg** (built F4), not the
  BitTorrent wire protocol/DHT — we control both endpoints. A holder serves an
  **on-demand manifest** (`GET /madnetwork/v0/manifest/{hash}`): the total size,
  the bulk **chunk size**, a small **lead-ramp** (`lead_sizes`), and the ordered
  per-chunk SHA-256 list. The layout is **adaptive + self-describing**, so a
  fetcher never assumes it and the sizing policy can change without a protocol
  break (decision 2026-07-18, resolves former open question 1):
  - the **bulk chunk size** scales with the file up to a **1 MiB cap** — the cap
    is deliberately modest because it doubles as the **seek granularity** (a seek
    into an un-fetched region waits for the one chunk covering it);
  - a **lead ramp** of small chunks (256 KiB doubling up to the bulk size)
    precedes the bulk, so the **first byte** of a stream — and the first byte
    after a seek to the front — is ready after a *small* chunk regardless of file
    size, while the bulk stays efficient and manifests stay bounded for huge
    files. Older nodes that predate the ramp see a chunk count that doesn't match
    a uniform layout, reject the manifest, and fall back to the whole-file fetch
    — a clean degrade.

  Because
  the swarm id is a flat SHA-256 of the whole file (not a Merkle root — it is
  the same content address used everywhere), the manifest's chunk hashes are not
  cryptographically bound to it; they enable **early per-chunk verification and
  bad-chunk re-fetch**, while the **assembled whole-file hash remains the
  authoritative anchor** (verified before a blob enters the cache). Manifests
  from friends are cross-checkable and a lie only wastes bandwidth (caught by
  the whole-file check) — acceptable because every holder is trusted. Chunks are
  fetched with plain HTTP Range requests (the F3 blob endpoint already serves
  them).
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
  consecutive-failure limit worse than the **best live holder** (streaks reset on
  any success). When some peer is still delivering, that is an absolute limit
  exactly; when every holder is equally deep in failures the fetch is in a bad
  moment rather than facing a bad holder, and none is retired. A *sole* holder
  has nothing to be compared against, so the plain limit applies and the fetch
  still ends.
  Retiring holders is deliberately **not** how a hopeless fetch stops — each
  chunk carries its own **attempt budget**, and exhausting it aborts the transfer
  with every holder still live. Conflating the two is a trap worth naming: when
  the only way to end a fetch is to kill every source, a perfectly healthy source
  gets declared faulty to make the transfer terminate.
  A hung connection is caught by an
  **idle-read watchdog** (~20 s with no bytes) plus a response-header timeout,
  rather than waiting out the whole per-chunk backstop — so a Yggdrasil path
  stall costs seconds, not minutes. A **single-seeder swarm degenerates to a
  direct transfer**,
  and a holder too old to speak the manifest endpoint triggers a **fall-back to
  the F3 whole-file streaming fetch** — so F4 nodes still fetch from F3 nodes.
- **Fast first byte** (built F4): to avoid two serial mesh round-trips before
  playback starts, a fetch **overlaps the manifest probe with a speculative
  chunk-0 fetch** — chunk 0's byte range is derived from the advertised size via
  the deterministic layout (so with the lead ramp the speculative fetch is a
  *small* chunk), then confirmed and per-chunk-verified once the manifest lands
  (dropped if the guess was wrong). Manifest probes and chunk fetches share **one
  pooled mesh connection**, so chunk fetches reuse the manifest's warm path
  instead of paying a fresh handshake; a manifest probe is bounded (20 s) so a
  slow holder cannot stall the transfer. Net effect: first byte after ~one small
  chunk + a round-trip rather than a full bulk chunk.
- **Tracker = the catalog + holdings** (built F4). "Who has hash H" is the union
  of two sources: friends whose **published catalog** advertises the hash as a
  rendition (their library — already synced in F2), and friends advertising it
  in their **download cache** via `GET /madnetwork/v0/holdings` (a flat hash
  list of what a node will seed, pulled per-friend on the same refresh cadence
  as the catalog and cached in `federation_holdings`). The library is already in
  the catalog, so holdings carries only the cache — this is what makes a
  **downloaded blob a discoverable seeder** and lets a popular track spread as
  friends fetch it. Providers are tried most-recently-seen first; no DHT.
- **Only nodes swarm.** Thin clients never talk to peers (see §Principals).
- **Authorization in the swarm:**
  - Between **direct friends**, the channel identity is sufficient — no tokens
    (F4: **swarm scope = direct friends**), filtered by the requester's audience
    since F5.
  - At **depth ≥ 1** (F7), seeders serve strangers-inside-the-network via
    **capability tokens**: the sharing node signs "peer key K may download hash
    H until T". A seeder verifies the signature against a node it trusts and
    verifies the connection is K (self-certifying channel — a leaked token is
    useless to others). Tokens are issued automatically when an entitled member
    starts a fetch and renewed transparently; members never notice them, only
    outsiders hit the wall. Any trusted holder may issue tokens to *its own*
    friends within the share depth — authority delegates along the friendship
    chain, no central issuer.
  - **Guest-playable content is an open swarm** (F5) — no friendship, no token:
    blob and manifest service for a guest-accessible recording answers any mesh
    node. Catalog and holdings stay friends-only, and the download cache is never
    open.
- **Seeding policy** (built F4): everything a node holds — library and
  listen-cache — seeds by default ("who cares" is the default privacy stance at
  node granularity; the cache reveals only that *someone on this node*
  listened). Controls: `seed_enabled` (master on/off — off refuses all blob and
  manifest service, the node consuming without serving) and `seed_cache`
  (whether the download cache is served **and** advertised in holdings), both
  runtime DB settings on `/admin/settings` defaulting **on**; plus a global
  **upload rate cap** `[federation] seed_rate_kib` (a token bucket over the
  blob-serve write path; `0` = unlimited), a static config knob.

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
it fails over between providers. Availability grows with the network:
redundant libraries make most entries multi-holder.

**Availability = redundancy + slow/passive liveness + reactive reachability.**
There is no dedicated high-frequency prober. Three cheap sources feed a per-peer
`last_seen`, and availability is derived from it at request time:

1. **Slow health check.** The existing **1-minute friendship refresh loop**
   already pings every friend; that ping *is* the health check (reuse the mesh
   `GET /madnetwork/v0/ping`, no new endpoint, no new cadence). One round a
   minute is within the connection budget the mesh already carries — it is not
   the 5 s prober that caused the churn.
2. **Passive observation.** Every *successful* mesh interaction refreshes
   `last_seen` — outbound (catalog sync, holdings sync, a delivered blob/chunk)
   and **inbound** (a friend syncing our catalog, fetching a blob, or pinging us
   proves they are alive *and*, by Yggdrasil's symmetric addressability, that we
   can most likely reach them). An in-flight transfer is continuous liveness
   proof for that holder for free.
3. **Reactive reachability.** When a transfer/manifest fetch fails against a
   holder, that failure is recorded (the swarm already fails a chunk in ~20 s and
   fails over); a holder with a recent failure is de-ranked as a provider and
   counts as "not seen" for availability until proven otherwise. This is the
   PeerTube/Mastodon pattern (learn a peer is down by *trying*, back off), not by
   pinging ahead of need.

**Freshness window, not a knife-edge.** A friend is *reachable* if `last_seen` is
within a **minutes-wide** window (`[federation] reachable_window_sec`, default
180 s ≈ 3 refresh rounds, clamped up to a 120 s anti-flap floor), so a single
missed ping never flips it — the flapping came from a 1× margin, this is a
several-× margin by construction. No probation state machine; the window *is* the
hysteresis. Whether hiding is applied at all is the runtime
**`madnetwork.hide_unavailable`** toggle (default on, `/admin/settings`) — off
shows every friend's cached catalog regardless of reachability.

**Availability predicate** (evaluated **at request time** in the browse/search
queries and the remote-playlist availability flag). A rendition is *available*
iff:

1. a **reachable** friend holds it (catalog ∪ holdings, `last_seen` within the
   window), **or**
2. it is in the **local library**, **or**
3. it is **fully cached** (complete file in `<data_dir>/cache/madnetwork/`, no
   `.part`).

A version is available if any rendition is; a track if any version is;
albums/artists and counts are computed over the available set. Local, cached, and
this node's **own** published tracks are *always* available — they never depend
on anyone's liveness. Because the predicate runs per request, each browse/search
fetch is a fresh **snapshot**; there is no server push and no live mutation (the
client re-evaluates only on page load and on a new search — see the UI doc).

**Fail open, never fail dark.** If *this node* cannot reach anyone (see the
self-health watchdog below), the correct response is to **stop filtering** and
show the last-known catalog, not to blank the library — a local fault must never
look like "the whole network is gone". Concretely: availability filtering is
suppressed while the node's own inbound path is suspect.

**Self-health (own inbound path).** This is the more important monitor, and it is
what makes "fail open" decidable. The vendored gVisor netstack runs its entire
inbound path in one goroutine; a single read error kills *all* inbound mesh
traffic permanently (the SPOF logged 2026-07-19 in `.issues/open-issues.md`).
When that happens, every friend goes silent at once even though the network is
fine. The watchdog (issue's option 4): **if every friend has been unreachable for
N consecutive refresh rounds while the yggdrasil core still reports peers up**,
flag the local inbound path as probably dead — surface it on `/admin/network`,
and trip the fail-open above. **Prerequisite:** before trusting any of this,
harden the read loop itself (log-and-continue on transient errors + supervise the
reader; issue's options 2–3) so the SPOF is a recoverable fault rather than a
silent permanent death. That hardening is the real gate on richer liveness, and
it is worth doing on its own regardless of the availability feature.

**No transitive real-time presence — how the big network stays honest.** At
depth ≥ 1 (F7+, friends-of-friends) the answer is deliberately *not* to ping
strangers or relay pings along the chain. Federated systems don't do live
presence at all:

- **Mastodon (ActivityPub)** is push-with-backoff: activities are delivered to
  peer inboxes, delivery failures retry with exponential backoff over days, and
  an instance is marked dead only after prolonged failure. There is no "online
  now" concept; capability/health is a **NodeInfo** document fetched
  occasionally, and reach beyond direct follows comes from **relays**, not
  transitive pinging.
- **PeerTube** adds **redundancy**: instances mirror popular videos, so a video
  stays available when its origin is down — availability is **replication**, not
  liveness. Discovery across the network uses **search indexes / instance lists**
  (SepiaSearch), again not a presence protocol.

We already have the analogues — the swarm's holdings *are* PeerTube redundancy,
and reactive backoff *is* Mastodon's dead-instance handling. So the depth-≥1 plan
is: **gossip coarse freshness hints along the catalog sync** (a friend's catalog
carries *its* friends' `last_seen` as a per-hop-stale *claim*, cheap and already
flowing — the relay pattern), rely on **redundancy** (any reachable holder
serves), and **verify on demand only for the working set actually on screen**
(one mesh RTT to the specific holder, proof not hearsay, cost O(what you are
looking at) not O(network)). A future enrichment of `GET /madnetwork/v0/ping`
into a small **NodeInfo-style health card** (name, version, holdings size,
seed policy) gives the network map real per-node health without any new probing
cadence. No chain-relayed ping-forwarding is ever needed.

## Topology asymmetry (unchanged)

A backbone of always-on server nodes plus intermittent madplayer peers. Mobile
peers are mostly consumers and occasional sources; durable availability comes
from the backbone and (future) subscribe→replicate, not from expecting phones
to be reachable. A phone serves only while foregrounded.

## Build plan

Swarm distribution is wanted from day one in spirit; in build order it is its own
milestone directly after direct transfer works, and tokens ship with depth.

- **F0 — Groundwork.** Embed yggdrasil-go (library-as-transport spike-confirmed
  2026-07-18, see §Identity & transport); node keypair lifecycle; `[federation]`
  config section; federation listener/protocol skeleton; the `nofederation`
  build tag (standalone build, mirrors `nowebui`).
- **F1 — Friendship** (built 2026-07-18, see §Friendship). Node cards
  (export/import), pairing handshake, trusted-peer table (+ user-level mapping
  to local accounts), block/unblock (local effect only), admin network page
  (list form).
- **F2 — Catalog** (built 2026-07-18, see §Catalog). Pull-and-cache catalog
  sync with direct friends (snapshot + not-modified, "last seen"), madnetwork
  library section (merged drill-down) + `madnetwork.access` permission (admin
  default + the stackable `madnetwork` role, migration 027) + gated header
  link, tagset payload + per-peer provenance storage, the "N versions"
  crossing UI.
- **F3 — Direct transfer** (built 2026-07-18, see §Direct transfer).
  Fetch-by-hash from a friend (HTTP Range wire, full-hash verified),
  cache-through streaming relay for thin clients, download-to-library through
  the review bucket + local fingerprint verification via the analysis
  pipeline, ladder-based rendition selection, `autoapprove_downloads`.
- **F4 — Swarm** (built 2026-07-19, see §Distribution). On-demand chunk
  manifest with adaptive, self-describing chunk size (`GET
  /madnetwork/v0/manifest/{hash}`); multi-source parallel chunk fetch with
  per-chunk verification, bad-chunk failover, and F3 whole-file fall-back for
  older peers; the holdings tracker (`GET /madnetwork/v0/holdings` +
  `federation_holdings`, migration 028) unioned with catalog holders so cached
  downloads seed; seeding controls (`seed_enabled`/`seed_cache` DB settings +
  `[federation] seed_rate_kib` token-bucket cap). Swarm scope = direct friends,
  channel-auth only (no tokens yet).
- **Availability & node health** (near-term, not depth-gated; see the section of
  that name). Harden the netstack inbound reader (issue #398) → slow/passive
  per-peer `last_seen` from the existing 1-min refresh + all successful mesh
  traffic → request-time availability predicate (reachable holder ∨ local ∨
  cached) with a minutes-wide freshness window → self-health watchdog +
  fail-open on `/admin/network`. Replaces the reverted 10 s presence feature.
- **F5 — Depth & scope** (built 2026-07-25, see §Sharing scope). Share-depth knob
  (node default + per recording, migration 030), the audience model filtering
  catalog and bytes from one rule, per-friend filtering via the user mapping, and
  the guest-open swarm. Tokens moved out of F5 (they need depth ≥ 1 to have a
  counterparty) and now sit in F7, one phase after the gossip that discovers it.
- **No fingerprint, no publication** (near-term, not depth-gated; see the
  planned item at the end of §Sharing scope). The publishable predicate gains an
  `audio_fingerprints` requirement per rendition, in `visibleTagset` /
  `selfPublishedClause` so catalog and bytes inherit it together, plus the "why
  is this not published" readout in the Recordings lens. Independent of both F6
  and F7, shippable on its own; the startup gate refusing a federated node without
  `fpcalc` (built 2026-07-26) is the other half of the same rule.
- **F6 — Transparency & defense.** Friend-list gossip within depth, the network
  map UI, signed distrust marks, branch snipping, and the stolen-key revocation
  flow. **Changes nothing about who may fetch what** — every requester stays at
  distance 0 throughout, so the wire's access rules are exactly F5's. What it
  adds is sight and reach of *judgement*: an admin can see the graph beyond their
  own friend list, see whom their friends distrust, and cut a branch. Includes
  **contradicted-claim reports** (§Trust graph): the fingerprint claim added to
  the catalog wire, the checks against blobs we already hold and against
  materialized downloads, and the evidence shown on the peer card — the detection
  that makes the blocking tooling in this phase something an admin can act on
  rather than guess with.
- **F7 — Tokens & transitive reach.** The capability tokens that let a seeder
  serve strangers-inside-the-network, with delegated issuance along the
  friendship chain; `Audience.Distance` computed from the gossiped graph instead
  of pinned to 0, so the depth ladder above `DepthFriends` finally does
  something; trust-weighted popularity (one branch = one voice, §Trust graph);
  and gossiped freshness hints for availability at depth ≥ 1 (§Availability),
  never transitive pinging. **Listener nodes land here too** (§Principals &
  access): a madplayer is a stranger to its home server's friends, so the token
  that says "this bearer is mine" is what admits it to the swarm — the concrete
  use case the token design should be built against rather than an abstract
  friend-of-a-friend.
- **Cleanup, any time — remove the node-key → local-user mapping** (§Principals &
  access). Drop `federation_peers.user_id`, `PeerAudience`'s account lookup and
  the `/admin/network` control, once the open detail under §Sharing scope decides
  what — if anything — replaces it as the source of `GuestOnly`. Independent of
  the phases around it; needs a migration.

  **Why the split** (decided 2026-07-26, superseding the single F6): the two
  halves have opposite risk profiles. F6 is additive and observational — new
  endpoints, a new page, no change to what leaves the node — while F7 rewrites
  the access rule that F5 just established and introduces a credential with a
  lifetime, a revocation story and a delegation chain. Shipping them together
  would mean the riskiest change in the project arriving inside its largest
  phase. The ordering is also the doc's own rule from §Trust graph: a network you
  can see further into than you can defend is the wrong order, so defense first
  is not merely convenient sequencing — F7 is *unsafe* without F6, and F6 is
  useful without F7.
- **F8 — Quality upgrades.** Madnetwork-match arm on the upload/download review
  cards (other tagsets + better renditions of the same recording), the
  fingerprint-vs-tagset **mismatch warning** (tag-suggestions machinery reuse),
  and the optional quality-upgrade page scanning the local library against
  synced catalogs.
- **Later (decided-deferred):** subscribe→replicate with storage caps,
  announce/gossip of catalog deltas, S3-backed swarm storage.

## Open questions (design-time details)

1. **Gossip payload details (F6) — the next one to settle**, since gossip is now
   the first thing F6 builds: what exactly a friend-list / distrust message
   carries, how far it propagates, and how a receiver ages it out. Note that the
   friend-list half is a **privacy** decision as much as a protocol one — it
   tells a friend-of-a-friend who my friends are.
2. Token lifetime / renewal cadence (F7).

(Former question 1 — catalog crossing / one tagset text on two recordings —
was settled with F2: see §Catalog, ""N versions"". Former question 2 — chunk
size & Merkle parameters — was settled with F4: adaptive, self-describing chunk
size in the manifest; whole-file hash is the anchor, per-chunk hashes are for
early verification only. See §Distribution.)

Decided-and-deferred: **replication** (subscribe/favourite → mirror, storage
caps) stays out of v1 — manual download-to-library already makes a node a
holder; the automatic version is a clean later add-on.

## Related

- `docs/architecture/auth.md` §8 (federation hooks), §9 (sharing scope, audit),
  §10 (Phase 4).
- `docs/architecture/recordings.md` §"Federation" (cross-node fingerprint index,
  rendition negotiation).
- `docs/architecture/recording-tagsets.md` (the per-recording tagset payload the
  catalog carries; provenance, trust weighting, access never imported).
- `docs/ui/native-client.md` (madplayer — the client that consumes federation).
- `madshare.org` (concept: trust system, sharing scopes, spam/war concern).
