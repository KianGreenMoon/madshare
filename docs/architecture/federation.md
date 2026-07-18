# Madnetwork federation — design (draft)

> **Status: draft — agreed 2026-07-18.** All session decisions are folded in;
> the remaining items in §Open questions are design-time details to settle
> during the respective milestones, not blockers. Federation is auth Phase 4
> (`docs/architecture/auth.md` §8) and the milestone the native client
> (`docs/ui/native-client.md`) exists to use. No code yet — doc-first.

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
- **Sync mechanism = pull-and-cache (decided).** On connect and periodically, a
  node pulls a friend's catalog as **"changed since serial N" deltas** and
  keeps a local copy. A friend's library stays browsable while they are
  offline, shown with a **"last seen"** indicator — no TTL-based hiding.
  Push/gossip of deltas is a later optimization, not v1.
- **Playback needs a holder, not the origin.** Because the swarm is keyed by
  content hash, an offline friend's tracks stay playable whenever *any*
  reachable node holds the hash. With network scale (many redundant
  libraries), most entries have multiple holders — availability improves as
  the network grows.
- **Merged view.** The madnetwork page shows the **deduplicated union** of all
  synced catalogs: a recording offered by many nodes appears once, holders
  aggregated. Which friend a tagset came from is **not surfaced while
  browsing** — provenance stays stored (it drives trust, moderation, and a
  details/inspect view) but does not shape the browsing experience.
  Cross-catalog dedup keys on *claimed* recording identity (hint-level
  fingerprint match — sufficient for display; real verification happens
  locally on download, and a wrong hint-match simply splits then; local truth
  wins). Identical tagset text offered by many nodes collapses to one entry,
  and the **trust-weighted** carrier count (see §Trust graph, "Mislabeling")
  **is** the tagset popularity score.

## Distribution (the swarm)

- **Swarm ID = content hash.** Blobs are already content-addressed; two
  independently uploaded identical files hash identically and are automatically
  seeders of the same swarm — no coordination, no `.torrent` files. Different
  encodings of the same audio are different swarms; the recordings overlay
  above chooses which rendition (which swarm) to fetch.
- **Protocol: a lean chunk-exchange protocol over ygg**, not the BitTorrent wire
  protocol/DHT — we control both endpoints. Fixed-size chunks, Merkle-tree
  verification per chunk, multi-source fetch, sequential-priority mode for
  streaming. A single-seeder swarm degenerates to a direct transfer — no
  slower than a plain download, so the swarm path is the only path.
- **Tracker = the catalog.** "Who has hash H" comes from the holders list in
  catalog entries; no DHT.
- **Only nodes swarm.** Thin clients never talk to peers (see §Principals).
- **Authorization in the swarm:**
  - Between **direct friends**, the channel identity is sufficient — no tokens.
  - At **depth ≥ 1**, seeders serve strangers-inside-the-network via
    **capability tokens**: the sharing node signs "peer key K may download hash
    H until T". A seeder verifies the signature against a node it trusts and
    verifies the connection is K (self-certifying channel — a leaked token is
    useless to others). Tokens are issued automatically when an entitled member
    starts a fetch and renewed transparently; members never notice them, only
    outsiders hit the wall. Any trusted holder may issue tokens to *its own*
    friends within the share depth — authority delegates along the friendship
    chain, no central issuer.
  - **Guest-playable content is an open swarm** — no token.
- **Seeding policy:** everything a node holds — library and listen-cache — seeds
  by default ("who cares" is the default privacy stance at node granularity;
  the cache reveals only that *someone on this node* listened). Advanced
  settings: upload rate cap, "don't seed the cache" toggle, seeding off
  entirely (for constrained users).

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
- **F1 — Friendship.** Node cards (export/import: address + key), pairing
  handshake, trusted-peer table (+ user-level mapping to local accounts), block/
  unblock (local effect only), admin network page (list form).
- **F2 — Catalog.** Pull-and-cache catalog sync with direct friends (serial
  deltas, "last seen"), madnetwork library section (merged/deduplicated view)
  + `madnetwork.access` permission + gated header link, tagset payload +
  provenance storage, the "N versions" crossing UI (open question 1).
- **F3 — Direct transfer.** Fetch-by-hash from a friend (chunked, verified),
  cache-through streaming relay for thin clients, download-to-library through
  the review bucket (streamlined approve-by-default modal) + local fingerprint
  verification, ladder-based rendition selection.
- **F4 — Swarm.** Multi-source chunk fetch, holders lists in the catalog,
  seeding (rate cap, cache-seed toggle), swarm scope = direct friends
  (channel-auth only, no tokens yet).
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

1. **Catalog crossing — one tagset text, two different recordings.** Same
   artist/album/title attached to different audio (different masters, live vs.
   studio, or a mislabel) must display sanely in the merged view. Starting
   ideas: **never merge recordings** (fingerprint is truth; tags are labels);
   dedup tagsets by textual identity; show one row for the shared text with an
   **"N versions" expansion** (like the renditions dropdown, one level up);
   default pick = the recording with the most **trust-weighted** holders (the
   quality ladder cannot rank across different audio). To settle with the
   madnetwork browse UI (F2).
2. Token lifetime / renewal cadence; exact chunk size & Merkle parameters.
3. Gossip payload details for F6 (what exactly a friend-list/distrust message
   carries).

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
