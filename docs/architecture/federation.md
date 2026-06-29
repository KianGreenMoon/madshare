# Federation (design notes — not yet designed)

> **Status: notes & open questions, not a design.** This file is the **seed** for
> the federation design session, not its conclusion. Federation is auth **Phase 4**
> (`docs/architecture/auth.md` §8), deferred. It is the prerequisite for the native
> client (`docs/ui/native-client.md`) and the milestone that client exists to use.
> Per the project's doc-first workflow, the real design (consult → agree decisions →
> fill this doc → implement) happens **before** any federation code. What's below is
> what's already established plus the genuinely open problems — so the design session
> starts with an agenda, not a blank page.

## Goal

**Peer-to-peer federation between madshare nodes** — node A can browse and stream
node B's library, subject to B's sharing rules. This is the milestone. (Plain
"authenticate to a remote server as a thin client" is an accepted *fallback* and is
trivial — same API, different base URL — but it is **not** federation and not the
target.)

## What federation builds on (assets already in place)

- **Content-addressed blobs.** Files are keyed by content hash with dedup. "Do you
  have hash X?" / "stream me hash X" is the natural cross-node primitive. Provenance
  hook exists: `files.uploaded_by` + (planned) an origin-node reference (auth.md §8).
- **Recordings / renditions overlay** (`docs/architecture/recordings.md`). Same-audio
  grouping by acoustic fingerprint already designed *to be shared* across nodes
  (recordings.md §"Federation"): a cross-node **fingerprint index** lets a node learn
  a peer has the same recording in a better rendition, and the quality-ladder
  negotiation (`RankRenditions`) maps onto "fetch the rendition that fits my
  bandwidth." The storage is here; the index is the federation problem.
- **Per-instance auth / RBAC** with a single `content.access` permission and the
  guest gate. This is the **local enforcement point**; federation extends it across
  the node boundary (see *The hard problem* below).
- **Yggdrasil transport + self-certifying identity.** The project deploys over
  Yggdrasil (`.ygg`); the native client embeds a Yggdrasil node as a library. The
  node key derives the mesh address, so **the address is proof of node identity** —
  this is the natural server identity keypair auth.md §8 anticipates, for free.

## Node identity & transport

- **Identity = the Yggdrasil node key.** Its derived `.ygg` address is
  self-certifying, so a "trusted-peer table" is a table of peer mesh addresses /
  public keys. No separate PKI to bootstrap.
- **Transport = Yggdrasil as a library**, routing madshare's HTTP API over the mesh
  — preferably **without a system TUN** (a TUN on mobile needs Android `VpnService` /
  iOS `NetworkExtension` entitlements; library-as-transport avoids that). *Verify the
  yggdrasil-go library API supports this before designing on the assumption.*
- **Open:** the mesh already authenticates + encrypts the channel, but do we *also*
  want app-layer **signed requests** for provenance / non-repudiation / audit
  (auth.md §8, §9 "federation approvals")? Probably yes for write/approve actions,
  maybe unnecessary for reads over the authenticated channel. Decide.

## The hard problem: cross-node authorization (sharing scope)

This is the core undesigned piece. Today authorization is *per-instance* roles plus
one `content.access` permission and a guest gate. Federation has to answer **"which
remote node/principal may pull which content from me?"** without collapsing the local
model.

Starting material from auth.md §8 / §9:

- **Library-wide sharing scope** (`library.share`): **madnetwork / friends / none**,
  sitting *above* per-user ACLs. The first-cut model: a node declares a scope, and a
  per-peer **trusted-peer table** refines it (allow/deny specific peers).
- The **guest gate** ("anonymous public" playable) is the existing hook for
  "publicly federated" content.

Genuinely open questions for the session:

- **Identity granularity.** Is the federated principal the *node* (B's whole server
  vouches for its users) or a *user-across-nodes* (a remote user identity maps to a
  local one)? Node-level is far simpler and probably the v1 — a peer is trusted as a
  unit; its internal user model is its own business.
- **Mapping remote → local permission.** A request from a trusted peer resolves to
  *what* local permission/scope? Default-**deny** (matches the enforced default-deny
  on `/files/*` and listings); a peer sees only what its scope + the content's
  guest/share flags allow.
- **Provenance & moderation.** Pulled/mirrored content carries an origin-node
  reference; ties into the existing moderation/spam controls
  (`docs/architecture/moderation.md`) and the audit log's "federation approvals".
- **Revocation.** Removing a peer from the trusted table must cleanly cut access
  (and stop replication, below).

## Catalog & content exchange

- **Metadata vs. stream.** Likely a **hybrid**: pull/cache a peer's *catalog
  metadata* (so a friend's library is browsable) and **stream audio on demand** by
  hash. Live-query-only is simplest but useless when the peer is offline. The shape
  of that metadata payload — per-recording **tagsets** (the several album/artist
  appearances of one audio identity), with origin-node provenance so peer
  appearances stay trust-weighted and revocable — is designed in
  `docs/architecture/recording-tagsets.md` (draft).
- **Availability / replication.** A peer (especially mobile) is often offline, so
  remote content is only reachable while the peer is up — unless we **replicate**.
  Future model: **subscribe / favourite → replicate** the content you care about so
  it survives the source going offline. (This also reuses the content-hash dedup —
  a replicated blob is just another holder of hash X.)
- **Fingerprint index.** The cross-node recording index (recordings.md) lets "I have
  this recording, you have a better rendition" work; designing *that* index (gossip?
  pull-on-demand?) is part of this.

## Discovery

- **v1: manual peer exchange.** Paste/scan a peer's `.ygg` address (a "node card").
  No directory, no DHT — keep it lean.
- **Later: announce / gossip** of known peers and catalog deltas. Deliberately **not**
  ActivityPub (web/JSON-LD-heavy, poor fit) — a lean custom protocol over the mesh.

## Topology asymmetry (don't design it away)

Federation will have a **backbone of always-on desktop/server nodes** plus
**intermittent mobile peers** (a phone serves only while foregrounded — see
`docs/ui/native-client.md`). Design for this asymmetry from the start: mobile peers
are mostly *consumers* and occasional sources; durable availability comes from the
backbone + the subscribe→replicate model, not from expecting phones to be reachable.

## Open questions (the session's agenda)

1. Node-level vs. user-level federated identity (lean node-level for v1?).
2. App-layer signed requests — for which actions (writes/approvals only)?
3. Sharing-scope model: is `library.share` (madnetwork/friends/none) + a per-peer
   allow/deny table enough for v1, or do we need per-album/artist federated scopes?
4. Catalog sync mechanism: pull-on-connect + cache TTL? push deltas? gossip?
5. Replication trigger & accounting (subscribe/favourite → mirror); storage caps.
6. Cross-node fingerprint index shape (recordings.md).
7. Discovery beyond manual: what, if anything, in v1.
8. yggdrasil-go library-as-transport (no TUN) — confirm the API.

## Related

- `docs/architecture/auth.md` §8 (federation hooks), §9 (sharing scope, audit), §10
  (Phase 4).
- `docs/architecture/recordings.md` §"Federation" (cross-node fingerprint index,
  rendition negotiation).
- `docs/architecture/recording-tagsets.md` (draft — the per-recording **tagset**
  metadata payload federated catalog sync carries; origin-node provenance, trust
  weighting, and the access-never-imported-from-a-tagset constraint).
- `docs/ui/native-client.md` (the client that consumes federation; topology
  asymmetry).
- Yggdrasil deployment context: the project already runs over the `.ygg` mesh.
