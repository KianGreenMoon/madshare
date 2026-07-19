# Madnetwork federation — design

> **Status: agreed 2026-07-18; F0 (groundwork), F1 (friendship), F2 (catalog),
> F3 (direct transfer) and F4 (swarm) are built.** The remaining items in
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
- **Full peer** — a node: participates in catalog exchange and the swarm.
- **Thin client** — a browser user. Thin clients are *not* madnetwork
  participants; they are local users of exactly one home node, which acts as
  their gateway.

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
- **User-level grants** exist for madplayer users: an admin maps a remote user
  (identified by their personal node's key) to a **local user account**. From
  then on *all existing local ACLs apply to them unchanged* — default-deny,
  `content.access`, guest flags, per-recording access. Federation adds no
  parallel permission system. The point of the mapping: a personal node is
  intermittently online, but its owner's *access* to this server's library is
  stable (it's an account); only their seeding is best-effort.
- **Unmapped friends are not a special case** (decided 2026-07-18): a friend
  node *without* a user mapping is treated as a **default regular-user
  identity** — it may see and fetch whatever a plain `user`-role local account
  may. The mapping is the per-friend *override* (more or less than the
  default), not a prerequisite. Deliberately a rule, not a magic local account
  row — nothing to log into, rename, or accidentally delete. Enforcement
  becomes consequential with per-content scope (F5); until then the published
  set is uniform per the F2 decision.
- **Thin clients have no madnetwork access by default.** Madnetwork browsing is a
  new permission (working name `madnetwork.access`), granted to admin by default
  and grantable to trusted local users. The header section for the madnetwork
  library is server-side gated like every other link.
- When a thin client with the permission plays a non-local file, **the server
  fetches it into a cache directory and relays it** — as *cache-through
  streaming*: chunks are fetched in sequential priority and served to the
  browser as they arrive, while the complete file lands in the cache in
  parallel. Never build the blocking download-fully-then-play version.

## Sharing scope

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
- The **legal frame** (madshare.org): sharing among friends is private sharing;
  non-friends cannot listen. Default-deny toward the outside world is preserved
  end-to-end — an outsider gets no catalog, no stream, no swarm chunks.
  Guest-playable content is the deliberate exception (open to everyone, no
  token), and the admin who flags it owns that choice.

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
- Transitive reach (depth > 0) **ships only together with** the transparency and
  blocking tooling — a network you can see further into than you can defend is
  the wrong order (build plan F6).

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
  request. A friend's library stays browsable while they are offline, shown
  with a **"last seen"** indicator — no TTL-based hiding. What a node
  publishes in F2 is its **whole approved live library** (the node-level
  default scope; per-content share depth arrives in F5). Push/gossip of
  changes is a later optimization, not v1.
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
  arrives with transitive reach (F6).
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
- **Authorization** (decision 2026-07-18): **any friend may fetch any
  published blob** — exactly matching what the F2 catalog already shows them
  (never advertise what you won't serve, and vice versa). Published = the same
  predicate as the local library (live file + an approved appearance on its
  recording); a staged, trashed, or unknown hash is 404 even for a friend.
  Per-friend filtering via the user mapping (unmapped = default regular-user
  rights, §Principals & access) arrives with F5, catalog and bytes together.
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
  download-fully-then-play. The total is known up front (the origin's
  Content-Length), so browser range requests work against the growing file: a
  range beyond the downloaded prefix waits for the sequential fetch to reach
  it.
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
  **on-demand manifest** (`GET /madnetwork/v0/manifest/{hash}`): the total
  size, the **chunk size**, and the ordered per-chunk SHA-256 list. The chunk
  size is **adaptive** — chosen from the file size (small files → small chunks,
  large → up to a cap, centred near ~1 MiB) and **written into the manifest**,
  so a fetcher never assumes a layout and the sizing policy can change without a
  protocol break (decision 2026-07-18, resolves former open question 1). Because
  the swarm id is a flat SHA-256 of the whole file (not a Merkle root — it is
  the same content address used everywhere), the manifest's chunk hashes are not
  cryptographically bound to it; they enable **early per-chunk verification and
  bad-chunk re-fetch**, while the **assembled whole-file hash remains the
  authoritative anchor** (verified before a blob enters the cache). Manifests
  from friends are cross-checkable and a lie only wastes bandwidth (caught by
  the whole-file check) — acceptable because every holder is trusted. Chunks are
  fetched with plain HTTP Range requests (the F3 blob endpoint already serves
  them).
- **Multi-source fetch, sequential-priority** (built F4): chunks are dispatched
  lowest-index-first (so the streaming relay's in-order prefix grows and
  `WaitFor(offset)` unblocks) but fetched by a small worker pool **in parallel
  across all advertising holders**. A chunk that errors or fails its per-chunk
  hash is re-queued to a different holder (the offending holder is dropped for
  the rest of the transfer). A **single-seeder swarm degenerates to a direct
  transfer**, and a holder too old to speak the manifest endpoint triggers a
  **fall-back to the F3 whole-file streaming fetch** — so F4 nodes still fetch
  from F3 nodes.
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
    (this is all F4 does: **swarm scope = direct friends**).
  - At **depth ≥ 1** (F5), seeders serve strangers-inside-the-network via
    **capability tokens**: the sharing node signs "peer key K may download hash
    H until T". A seeder verifies the signature against a node it trusts and
    verifies the connection is K (self-certifying channel — a leaked token is
    useless to others). Tokens are issued automatically when an entitled member
    starts a fetch and renewed transparently; members never notice them, only
    outsiders hit the wall. Any trusted holder may issue tokens to *its own*
    friends within the share depth — authority delegates along the friendship
    chain, no central issuer.
  - **Guest-playable content is an open swarm** — no token.
- **Seeding policy** (built F4): everything a node holds — library and
  listen-cache — seeds by default ("who cares" is the default privacy stance at
  node granularity; the cache reveals only that *someone on this node*
  listened). Controls: `seed_enabled` (master on/off — off refuses all blob and
  manifest service, the node consuming without serving) and `seed_cache`
  (whether the download cache is served **and** advertised in holdings), both
  runtime DB settings on `/admin/settings` defaulting **on**; plus a global
  **upload rate cap** `[federation] seed_rate_kib` (a token bucket over the
  blob-serve write path; `0` = unlimited), a static config knob.

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
- **F5 — Depth & tokens.** Share-depth knob (per node default + per content),
  capability tokens with delegated issuance, guest-open swarm.
- **F6 — Transparency & defense.** Friend-list gossip within depth, network map
  UI, signed distrust marks, branch snipping, stolen-key revocation flow.
  Transitive reach (depth > 0) turns on here, not before.
- **F7 — Quality upgrades.** Madnetwork-match arm on the upload/download review
  cards (other tagsets + better renditions of the same recording), the
  fingerprint-vs-tagset **mismatch warning** (tag-suggestions machinery reuse),
  and the optional quality-upgrade page scanning the local library against
  synced catalogs.
- **Later (decided-deferred):** subscribe→replicate with storage caps,
  announce/gossip of catalog deltas, S3-backed swarm storage.

## Open questions (design-time details)

1. Token lifetime / renewal cadence (F5).
2. Gossip payload details for F6 (what exactly a friend-list/distrust message
   carries).

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
