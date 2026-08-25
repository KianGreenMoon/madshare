# Full-node mode — a madplayer as an ordinary member (DRAFT)

Status: **draft, 2026-08-22.** The decisions marked *(owner)* were settled in
conversation that day; the Open questions at the bottom need answers before
anything is built.

What this fixes: a person who will not run a CLI server has **no way into
madnetwork at all** — no home server means no token, means outsider. madplayer
embeds the whole madshare app, so the distance to "a real node with a friendly
face" is small, and the experimental pairing surface has already proven the
mechanism end to end: madshare `app.Pairing` (EXPERIMENTAL, v0.8.11) on one
side, madplayer `internal/backend/pairing.go` + `internal/ui/pairing.go`
(2026-08-17, gated `ui.pairingEnabled`) on the other. A device paired through
it is a full member — a gossiped edge, a place on the map, holders of its own.
This plan graduates that test surface into a supported mode, with a deliberate
sharing story on top. It is the intended use of that surface, not a new
mechanism.

**This does not touch the listener/household path.** Tokens, the tracker, and
the one-way rule stay exactly as federation-access.md describes them; pairing
remains how a device *opts into* membership instead. The two participant kinds
stay separate — the separation carries the access model (a token bears the
account's rights; a friend is node-level trust), the graph hygiene (a listener
is in nobody's map), and structural revocation.

## Already true (no work)

- **Membership.** `app.Pairing` exports the card, imports cards/keys, accepts
  and removes peers; a paired player gossips, appears on maps, syncs catalogs.
- **Identity.** The player generates and persists a node key exactly as a
  server does (same PEM `key_file` mechanism). A device that was a
  token-bearing listener and pairs later keeps its key; `serveAudience`
  resolves the friend arm before the token, so its standing upgrades cleanly.
- **Fingerprinting.** The fpcalc gate is satisfied — madplayer's Chromaprint
  build shipped 2026-08-15.
- **Churn tolerance.** Passive availability (two freshness windows, down-marks,
  the underlay kick, fail-open) is precisely the machinery for a node that is
  up a few hours a day. **No active health prober** — that was built once
  (presence, the 10-second rule) and reverted; the swarm already probes at
  fetch time and fails over per chunk.
- **The closed default.** `Network().PublishNothing` pins
  `default_share_depth` to Local; only the seeded cache is served.

## Design decisions

- ***(owner)* Nothing is shared until chosen.** The node default stays pinned
  Local even in full-node mode — membership and sharing are separate axes, and
  the mode changes only the first. Publishing is per-item opt-in: the user
  organizes their library and pins what they mean to share, in full knowledge
  and responsibility, *before* the network sees anything. No "share my whole
  library" toggle, ever — a regular user does not know what is on their PC,
  and a node that publishes junk or private files gets blocked by its friends.
  This rule is load-bearing, not just polite: **madplayer scans land
  `ReviewApproved` by design** (no review queue on a personal player), so the
  share-depth default is the *only* gate between a player's disk and the
  catalog.
- ***(owner)* Desktop only.** Phones stay listener nodes. A phone as a member
  drains its battery holding the mesh up, churns everybody's availability
  machinery, and wastes discovery-budget slots on a node that is asleep most
  of the day. Mobile builds do not offer the mode. (The phone-side network
  policy is its own plan, in madplayer's repo where the work is:
  `../madplayer/docs/plans/mobile-seeding-controls.md`.)
- **Opt-in publishing rides the existing pin.** `recordings.share_depth`
  already overrides the node default per recording (NULL = inherit = Local;
  pinned Friends/Madnetwork opens exactly that item). No migration, no new
  scope values. What is missing is a *surface*: the facade has no share-depth
  setter and no "what am I publishing" view.
- **The sharing surface hangs off `Library()`, not `Network()`** (leaning, not
  settled). Organize-then-share is the whole model: the user should be able to
  curate scopes *before* the mesh is up, and `share_depth` is a DB column that
  needs no running node. `Network()` stays the mesh surface.
- ***(owner)* Background presence: tray mode + autostart.** Membership is
  presence — a node that dies when the player window closes is exactly the
  churny neighbour the availability machinery has to keep writing off. On
  desktop the app can keep the node alive in the system tray when the window
  is closed, and register itself for login autostart (XDG autostart `.desktop`
  on Linux, the Startup mechanism on Windows — per-platform, added 2026-08-22
  mid-draft by the owner). Both are **opt-in switches**, surfaced next to the
  pairing setup so the choice is made where its consequence is visible: "stay
  reachable for your friends". Playback stays foreground; what survives in the
  tray is the mesh — seeding, catalog sync, gossip.
- **Bootstrap connectivity is a first-run step.** Pairing happens over the
  mesh, so the mesh must reach *somebody* first: a pasted underlay peer URI
  (public ygg peer, or the friend's `listen`), or LAN multicast (already on in
  madplayer). A player with a home server can also ask it
  `GET /api/madnetwork/peering`; a server-less user pastes one URI once. The
  UI must make this a guided step, not a config file.

## Work items — madshare

- **W1. Stabilize `app.Pairing`.** Settle the method set and drop the
  EXPERIMENTAL marker. Open parity question below (block/rename); whatever is
  decided, the surface keeps mirroring acts of `/admin/network`, never
  inventing new ones.
- **W2. The sharing arm on the facade.** A per-recording share-depth setter
  (three-valued, same `ValidDepth` rule as the admin PATCH) plus a published
  listing — the facade twin of the Recordings lens's scope chip and of
  `PublishedCatalog`'s selection, so the UI can show "these N items are what
  the network sees". Reuses `SetRecordingAccess` / `visibleTagset` machinery;
  no new query semantics. **Built 2026-08-26** (`Instance.SetShareDepth` /
  `ShareDepths` / `Published`, tagset-addressed since that is what browse rows
  carry; `database/sharing.go`; embedding.md §"The madnetwork browse and the
  publish picker").
- **W3. Nothing for availability.** Verify with the mesh-lab scenario below;
  no code expected.
- **W4. The community browse on the facade** *(added 2026-08-26 — the first
  live pairing found it missing: a paired player held the friend's catalog in
  its own tables and had no way to show it, because the client's browse merges
  only signed-in servers over HTTP; pairing bought standing without a view)*.
  `Instance.Madnetwork()` — the in-process twin of
  `/api/madnetwork/{artists,albums,tracks,search}`, sharing ONE code path with
  those handlers (`api.MadnetworkBrowse`) so the merge rules the client must
  not re-derive are not re-derived here either. **Built 2026-08-26.**

## Work items — madplayer

- **P1. Graduate the pairing UI** from `ui.pairingEnabled` to a real,
  discoverable feature (keep card copy/paste; QR later, if ever). **Built
  2026-08-26**: `prefs.NodeMode` is the switch, the "Paired nodes" page exists
  exactly while the mode is on, phones do not offer it (madplayer
  `docs/design.md` §"Node mode").
- **P2. The publish picker.** Browse own library, pin scope per album/track,
  and a "Published" view listing exactly what is shared — the visible half of
  the responsibility the default protects. (The facade half is W2, built.)
- **P2b. The paired browse source** *(added 2026-08-26, W4's client half)*: a
  library source over `Instance.Madnetwork()`, merged like the per-server
  madnetwork sources, so a paired player browses the community through its own
  node with no sign-in.
- **P3. First-run connect step** (peer URI paste / multicast / peering pull
  from a home server, per the decision above).
- **P4. Desktop-only gating** of the whole mode in mobile builds.
- **P5. Tray mode + autostart.** Close-to-tray keeps the embedded node
  running (playback UI gone, mesh alive); an opt-in autostart registration per
  platform (XDG `.desktop` entry on Linux, Startup shortcut/registry on
  Windows), and a clean "quit fully" act that also stops the node. Unregister
  must be as easy as register.
- **P6. Key backup** (owner, 2026-08-22). Export/import of the node identity
  key from the UI, offered during pairing setup, with the plain warning that
  the key IS the identity: lose it and every friendship must be re-paired,
  every published claim is orphaned. First-run gets an "I already have a key"
  restore path. Cheap now, a support disaster to explain later — a Windows
  user will not know `federation.key` exists.

## Tests

- **Mesh-lab: the intermittent member.** A paired node that is up a few hours
  and gone the rest: it enters and leaves browse views on the right windows,
  down-marks retire and recover it, its slot in the pull rotation neither
  wedges nor starves others, and the underlay kick reconnects it promptly
  after a wake.
- **Publishing is exactly the pinned set.** A player with 1000 approved local
  tracks and 3 pinned Madnetwork serves 3 catalog entries and 3 blobs' bytes
  — and 404s the other 997 (catalog, holdings, and byte endpoints all).
- **Listener → member upgrade.** Same key, pair with the former home server:
  friend standing wins, token becomes irrelevant, nothing double-counts.

## Sequencing (owner, 2026-08-22)

- **Alpha may break; beta may not.** While the project is alpha, the mesh
  protocol may change incompatibly — every node is still upgraded by one
  hand. At the first beta/release, the additive-only compatibility rule
  becomes binding (and gets a mechanism, e.g. a capability field in the node
  card), because from that point un-upgraded desktop nodes exist forever.
- **F10 (merkle verification) stays parked, on its own triggers — it is not
  a scale answer.** (The first draft of this section filed it under scale;
  that was wrong.) It is decided-and-parked in
  `docs/architecture/federation-swarm.md` §"Merkle verification (F10,
  decided-and-parked 2026-08-09)" (+ the parked row in
  `docs/plans/work-queue.md`), with named triggers: video support, or a
  measured all-partials reassembly gap. What it buys is verification
  granularity, video-scale blobs, partial holders serving proofs, and the
  collapse of the whole-file fallback. Neither trigger has fired.
- **The growth limit is a NEW question with no design doc yet.** Three linked
  facts set it: (1) *catalog replication* — every member replicates whole
  catalogs from up to `discovery_cap` (200) sources on the pull rotation;
  (2) *the merged view* is per-request SQL over those denormalized rows in
  one SQLite file — 200 nodes × 20k-track libraries is millions of rows, on
  exactly the weak hardware full-node mode invites in; (3) *holder discovery*
  reaches only as far as cached catalogs do — there is no tracker and no DHT.
  Per-node cost is capped by design, so what actually grows past
  ~`discovery_cap` community nodes is **invisibility** (the frontier rotation
  recycles the cold) and **browse latency**. Before full-node mode targets
  communities of that order, this needs its own design doc. Candidate
  directions, none chosen: search-on-demand across members instead of
  replicate-everything; a holder-query hop ("who has hash H?" asked of
  friends) so bytes can be found for content whose catalog was never pulled;
  smarter cap spending (weight the rotation by observed liveness/overlap).

## Ideas held for thought (2026-08-22 — recorded, not decided)

Two risks with no mechanical fix; the owner regards the friendly environment
as his own responsibility, and these are the smallest software contributions
to it we could name. Neither is scheduled.

- **A community's answer to a bad node: attribution, not reputation.** The
  mechanism already exists — `BranchMap` knows which direct friend carries
  node X into your view. Show it: *"reaches you through: \<friend\>"* on a
  node's card and at block time. That converts "the network has a bad node"
  into "**my friend** vouches for a bad node", which is a conversation humans
  can actually have, and every remedy is already built (talk to them, demote
  guest-only, unfriend — branch-snipping does the rest). Deliberately NOT:
  shared blocklists or reputation scores — censorship-and-drama machinery;
  the trust model's shape is branch-snipping plus social pressure. The
  claim-reports table (F6) already carries the transparency half. The rest is
  a short community-norms page — the human layer.
- **Legal visibility: honesty at the moment of choice.** One plain sentence
  in the scope picker when pinning to Madnetwork — *"visible to your whole
  community, attributable to your node, redistributed by others"* — the same
  honesty on the cache-seeding switch (*"you redistribute what you fetched"*),
  and a plain-language "what sharing means" page in `docs/ui/` (cross-client
  wording; both UIs must say the same thing). No content policing — wrong
  layer, impossible anyway; the defaults (nothing shared, guests off) already
  protect the naive. This is wording, not machinery, and it is the difference
  between an informed user and a burned one.

## Open questions (owner)

1. **Peer-management parity.** `app.Pairing` today has import/accept/remove
   only. A personal node plausibly needs **block** (self-defense is not
   optional) and rename; guest-only demotion is probably server-vocabulary it
   can skip. Which of the `/admin/network` acts does v1 get?
2. **Publish granularity.** Per recording, per album, or folder/source-based
   ("this directory is my shared shelf")? The organize-then-share model hints
   at the last, but it is also the most new machinery.
3. **Disk defaults.** A member pulls catalogs (discovery cap 200) and fills
   the madnetwork cache. Should madplayer's full-node mode ship with cache
   retention *on* by default (a personal machine, unlike a server, has an
   operator who will never look at a retention knob)?
4. **Sleep/resume.** A laptop suspends mid-everything. Gossip records age out
   on `GraphTTL` and republish on wake, token/hint math is age-based or
   skew-tolerant — expected fine, but the intermittent-member scenario should
   include a suspend-length gap to confirm no timestamp arm misbehaves.
5. **Windows.** The release packaging has no Windows target; madplayer owns
   its own build story. Nothing in this plan blocks on it, but the audience
   named as the motivation ships only when that exists.
6. **Backbone thinness.** Desktop nodes are outbound-only leaves; when nearly
   everyone is a leaf hanging off a few public ygg peers, community traffic
   routes through strangers' relays — throughput and a privacy surface nobody
   chose. Does the mode want operator guidance ("somebody in the community
   runs a `listen` node"), and should the first-run connect step prefer a
   friend's `listen` URI over public peers when it has one?
7. **Client-side resource defaults.** The server ships transfer limits
   opt-in/unlimited (mechanism yes, guessed defaults no). An *autostarted
   background node on a machine nobody administers* argues the other way:
   madplayer's full-node mode plausibly wants conservative defaults of its
   own (a modest seed rate, cache retention on). Which defaults, and is the
   owner willing to bend the no-guessed-defaults rule for the client half?
8. **The update channel is a security mechanism.** The mesh port faces the
   whole yggdrasil internet from every user's desktop — HTTP parsing, the
   gVisor netstack, our three fork patches. Servers get patched by admins;
   desktops need an update story (in-app check? distro packages?), which is
   tangled with the still-undecided module-rename/distribution question.
