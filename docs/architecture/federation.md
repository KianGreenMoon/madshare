# Madnetwork federation — design

> **Status: agreed 2026-07-18; F0 (groundwork), F1 (friendship), F2 (catalog),
> F3 (direct transfer), F4 (swarm), F5 (depth & scope), Availability & node
> health, and all of F6 — friend-list records, distrust marks, network map, the
> naming split, contradicted-claim reports and underlay de-peering — are built.**
> **F7 (reach) is in progress.** Its first five items are built (2026-07-31): the
> mesh classes on `Audience`, the mutual-edge membership walk, serving members
> while refusing outsiders, the three-value scope vocabulary with migration 035,
> and the bounded frontier pull that finally makes other people's libraries
> visible here (§Discovery beyond the friend ring, migration **036**). The posture
> is therefore running in **both** directions — **everything to our community,
> nothing outside it** — and `meshlab reach` is the measurement that says so.
> **Item 8 followed the same day** (migration **037**): the /madnetwork page now
> lands on discovery lanes and search instead of an alphabet, which is what a
> community's whole published output needed, and **item 7 with it**: the map
> navigates a community — view radius, whole-component search, branches, paths —
> instead of loading all of it. **Item 10's freshness half landed 2026-08-01**
> (migration **038**): reaching the community's libraries had left them judged by
> a window sized for the friendship ping, so most of what item 5 pulled was
> hidden most of the time (§Availability, "Two clocks, two windows"). **Item 6
> (per-member quotas) and item 10's weighting half landed the same day** — the
> browse now counts popularity in branches wherever it orders anything by it,
> including the version a crossing's Play button acts on (§Trust graph, "Where
> the weighting applies"). **Item 9 (listener-node tokens) landed 2026-08-01, so
> F7 is COMPLETE**: a home server signs "this bearer is my user until T", and any
> node that can place the issuer in its own community honours it — one hour,
> renewed at half-life, buying membership and never friendship (§Principals &
> access, "The capability token"). The one item in §Open questions is a
> design-time detail to settle with a real network to watch, not a blocker.
> Federation
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
  these edges. A **direct friend** is one we friended by hand; the unqualified
  word is ambiguous in ordinary speech and is avoided below for that reason.
- **Community** — *the* word for what this network is for (declared 2026-07-31).
  Our community is our whole connected component of the friendship graph: our
  direct friends, their friends, their friends' friends, outward with **no
  radius and no size limit**. When this project says "we share with our friends",
  it means the community in this sense, not the handful of nodes one admin typed
  in. **We share everything with our community and nothing outside it** — that
  sentence is the design, and the rest of this document is how it is enforced.
  "Madnetwork" and "our community" are the same set; the first names the
  technology, the second names the people.
- **Sharing scope** — who a recording is shared with. **Three values, and no
  ladder** (decided 2026-07-30, superseding the per-hop "trust depth"):
  **Madnetwork** = our whole community, **Direct friends** = only the nodes this
  admin friended by hand, **Local** = nobody on the network.
  **Shipped default: Madnetwork** (declared 2026-07-31) — a node shares its
  library with its community out of the box, which is what the network is *for*;
  §Sharing scope states the posture that follows from it. "Direct friends" is the
  opt-in *restriction*, for a node that wants its content to stop at the people it
  chose personally. There is no "friends of friends" value because there is no
  ladder — a per-hop scope was a promise we could not keep, see §Sharing scope,
  "Why the ladder collapsed". The stored column and its constants are unchanged
  (`DepthUnlimited` / `DepthFriends` / `DepthPrivate`), so this is a vocabulary
  and UI decision, not a schema one; `DepthFriends` is the constant behind
  **Direct friends** and keeps its name.
- **Gossip** — information spread node-to-node rather than from a central place:
  each node tells its friends, who tell theirs. Three distinct uses, deliberately
  kept apart. **Friend-list gossip** (F6, designed) — B tells A whom B is friends
  with *and relays what B's own friends said*, so A's view grows past its friend
  list to the whole connected network rather than a fixed radius; the network
  map, branch snipping and distrust marks all read it, and since F7 so does
  **membership** — whether a key belongs to our community at all (§The membership
  rule, which reads only *mutually declared* edges), the one
  access question it may answer. It never answers *how much* a requester gets:
  there is no hop arithmetic anywhere, by design (§Sharing scope, "The graph
  decides whether, never how much"). **Freshness-hint gossip** (F7) — a friend relays
  *its* friends' `last_seen` as a second-hand claim, so availability survives
  past one hop without pinging strangers (§Availability). **Catalog-delta
  gossip** (deferred) — pushing library changes instead of pulling snapshots; an
  optimisation, unrelated to the other two. Despite the name none of these is a
  push protocol here: they ride the existing periodic pull (§Catalog), and the
  word describes how information travels, not the transport. Because a friend
  list names third parties who never agreed to be named, its payload is a
  privacy decision as much as a protocol one; both halves are settled in
  §Friend-list gossip.
- **Full peer** — a node: participates in catalog exchange and the swarm.
- **Madnetwork member** — a node we can place in **our own** connected component of
  the friendship graph: a friend, or a friend of a friend of … us, as vouched for by
  the gossiped records we hold. Membership is what Madnetwork-scoped content is
  shared with; a node we cannot place there is a **guest**, however routable it is
  on the mesh (§Principals & access). Being on Yggdrasil establishes nothing.
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

**Four principals, and every access decision resolves to one of them** (declared
2026-07-30, built 2026-07-31). Two are local and two arrive over the mesh; the
fourth is what F7 added:

| principal | identified by | may reach |
|---|---|---|
| **user** | credentials — session or API token | that account's ACLs |
| **guest** | nothing: an anonymous visitor, **or a node outside our madnetwork** | guest-playable only |
| **befriended node** | its key-derived mesh address matches a `state='friend'` row | Direct-friends *and* Madnetwork scope |
| **madnetwork node** | its key is a member of our community (§The membership rule) | Madnetwork scope — which by default is everything |

The load-bearing part is the second row: **a node outside our madnetwork is a
guest**, not a lesser peer. "Madnetwork" therefore means *our* connected component
of the friendship graph, never "everyone on Yggdrasil" — the mesh is a public
network, and being routable on it establishes nothing. This is what makes the
Madnetwork scope a statement about a bounded set of nodes that somebody we can
trace vouched for.

**The two mesh rows differ less than they look, and deliberately so** (2026-07-31).
Since the node default is Madnetwork, a member reaches the same library a direct
friend does; what a direct friend additionally gets is whatever *this* admin chose
to restrict to hand-picked nodes. Membership is the perimeter, and direct
friendship is the exception inside it — not the other way round.

Three consequences, all of them wanted:

- **Blocking gains teeth over access, not only over sight.** The F6 reachability
  walk never traverses a blocked node, so snipping a branch removes that branch's
  *membership* — and with it its reach. Defense tooling and the access rule become
  one mechanism.
- **Discovery narrows with it.** Only members may pull the Madnetwork-scoped
  catalog; an outsider is told nothing at all (§Discovery beyond the friend ring).
- **Membership rests on signed hearsay, and that is acceptable here for a precise
  reason.** X is in our community because our friend's own signed record names it,
  or a chain of such records does — the graph informing an authorization decision,
  which this design refuses to do for *hop counting*. The difference is direction:
  membership can only **narrow** access relative to "any mesh node", and it never
  grants more than the scope an admin already marked open to the community. A
  forged or over-generous edge therefore yields content already shared with the
  community, never content restricted to direct friends. Distance-based reach had
  the opposite property, which is why it is gone (§Sharing scope).

### The membership rule (declared 2026-07-31, built 2026-07-31)

Once the node default is Madnetwork, **membership is the only thing between a
stranger and the whole library**, so the rule deciding it stops being a
convenience and becomes the perimeter. Three decisions define it.

**1. A member is a key we can reach through *mutually declared* friendships.**
Built as `MemberKeys` in `federation/gossip.go`, memoized per node in
`federation/membership.go`.
`BuildNetworkMap` currently links two nodes from **one** side's claim
(`link(e.Origin, e.Peer)` *and* `link(e.Peer, e.Origin)`, `federation/gossip.go`),
so a single signed record naming 512 invented keys would make all 512 members at
the next sync — no agreement, no record of their own, nothing to block but the
friend who relayed it. For **drawing the map** that is right: an edge somebody
claims is worth seeing. For **deciding access** it is not, so the membership walk
requires the edge to be claimed from **both** ends. The direction data already
exists (`claimed[[2]string{…}]`); only the access walk reads it.

- **Our own direct friends are members unconditionally.** That edge is a local
  fact from `federation_peers`, not hearsay, so a friend who publishes no friend
  list is still a full friend of ours.
- **Further out, a silent node cannot be a member**, because nothing it signs
  names anyone. This is the already-documented "a silent node makes itself a dead
  end" property (§Friend-list gossip) arriving at its logical end, and it is the
  strongest reason `publish_friend_list` defaults to on.
- The practical effect is that every member has *declared itself* part of the
  community — one signed record per member — which is what makes the per-branch
  record bound (§Friend-list gossip, "Anti-flood bounds") count members rather
  than names.

**2. The community has no size limit, and the map is not how you cope with a big
one.** No cap on members, no radius, no admission quota — a community that grows
to thousands is the project succeeding, not a threat. `MaxOriginsPerBranch`
(5000) stays exactly what it was written as: a bound on what one friend can cost
**our store**, never a statement about who belongs. If a real network ever
approaches it, it is a number to **raise**, not a boundary to enforce. The answer
to "the map is unreadable" is a better map (§The network map), not a smaller
network.

**3. Abuse is bounded by cost and by revocation, not by admission.** A branch can
mint members up to whatever `MaxOriginsPerBranch` happens to be — and since that
number exists to protect our store and rises as real networks grow, **the member
count is not the defense and must never be relied on as one**. That is accepted
with open eyes. What is bounded instead:

- **per-requester quotas** — bytes and concurrent transfers per member per round,
  so a forged member is bounded *harm* rather than merely rare (F7, build plan);
- **branch attribution** — every member is traceable on the map to the friend of
  ours it arrived through, so "who let this in" is one glance;
- **one block clears the branch** — the reachability walk never traverses a
  blocked node, so snipping the introducing friendship removes every member behind
  it at once.

So the perimeter is **accountable rather than cryptographic**: we cannot prove a
member deserves to be here, but we can always say who vouched for it and undo that
in one action. For a network built out of person-to-person friendships that is the
honest target; anything stronger means a membership PKI, which this design has
refused from the start.

The rest of the access model is what the four principals are made of, and the
declaration above changed none of it:

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
  capability tokens a prerequisite** — to its home server's friends a madplayer is
  a stranger, and a stranger it cannot place in the component is served nothing at
  all. The token is
  how a home server says "this bearer is mine", and since the scope collapse it is
  the *only* remaining job for a token: not a hop count, but one node vouching for
  one bearer it authenticated itself.
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

### The capability token (F7 item 9, built 2026-08-01)

The token is one signed sentence: **"bearer key K is my user until T"**, issued by
a home server over its own ed25519 node identity — the same key its mesh address
derives from and the same key it signs gossip records with. Four fields, no
delegation chain, no PKI: issuer key, bearer key, expiry, and a `guest_only` bit.

**What a verifier checks**, in `serveAudience`, which is the single gate all four
mesh endpoints already resolve their audience through:

1. The signature verifies against the **issuer** key the token names.
2. The **bearer** key derives to the mesh address the request actually arrived
   from. This is what makes a stolen token worthless — the channel is
   self-certifying, so presenting somebody else's token from your own address
   fails, and presenting it from theirs requires their private key.
3. `now < T`.
4. The issuer is **placeable in our own community** by our own mutual-edge walk.

Only step 4 was a real decision, and it is the one the older text got wrong.
"Verified by that server's *friends*" was written when direct friendship was the
access boundary; item 3 moved that boundary to the community, and leaving the
token behind at the old line would have made a madplayer a second-class citizen
next to the server that vouches for it — reaching that server's four hand-picked
friends while the server itself reaches its whole component. It is still **one
issuer, one hop, no chain**: we place the *issuer* ourselves, from our own graph,
and then accept exactly one claim from it about exactly one bearer. Nothing about
the bearer is ever taken from hearsay, and the token is never re-presented onward.

**What it buys: membership, not friendship** (decided 2026-08-01). A valid token
yields `MemberAudience` — Madnetwork-scoped content, cache blobs included —
narrowed by the token's `guest_only` bit, which is how the home server's own
account ACL travels with the bearer. It does **not** yield the issuer's
`DepthFriends` reach. A recording marked *Direct friends* was restricted to nodes
this admin picked by hand, and a device somebody else enrolled is not one of them,
however much we trust its home server. The counter-argument was considered and
rejected on the doc's own terms: yes, the home server can fetch those bytes and
relay them to its user anyway, but that is a statement about *its* behaviour, and
the whole point of §"Why the ladder collapsed" is that we decide our own. The
token grants precisely what the component could not: a way to place a node that
publishes no friend list and appears in nobody else's.

Note the direction of the `guest_only` bit — it can only ever **narrow**. A token
that says nothing is served as a plain member, so a forged or truncated bit cannot
buy more than membership, and membership is what the bearer's issuer already has.

**Lifetime: one hour, renewed at half-life** (decided 2026-08-01, settling the
last open question). The lifetime looks like the revocation story and mostly is
not, which is why this was stuck: **blocking a home server revokes every token it
ever issued, instantly and without a lifetime being involved at all** — step 4 is
re-evaluated on every single request, so a snipped branch takes its bearers with
it on the next one. What the expiry actually covers is much narrower and belongs
to one node: a home server revoking one of *its own* users — an account disabled,
`madnetwork.access` withdrawn, a phone left in a taxi. That is a one-hour window
on one relationship, and renewal is free because a madplayer is by definition
already signed in to its home server and can ask for a fresh token whenever it
likes. Renewing at half-life rather than at expiry keeps a transient outage from
becoming a service interruption.

The cliff is real and accepted: a device that cannot reach its home server for an
hour stops being served by the network, even while the mesh around it is healthy.
That is the correct failure. The token is a *vouch*, the voucher is unreachable,
and a credential that outlives contact with its issuer is exactly the thing the
short lifetime exists to prevent. A madplayer in that state still plays what it
already holds — its own library is local, and one-way publication means nothing
about it depended on the network in the first place.

**No revocation list, deliberately.** Lifetime is the revocation mechanism for the
narrow case, community standing for the broad one, and between them there is no
gap worth a distributed data structure that every node would have to fetch, trust,
age out and disagree about.

**Where it lives.** `federation/token.go` — the token type, signing, the four
verifier checks, and `tokenAudience`, which `serveAudience` consults after the
friend and member arms and before the guest fallback (a friend or member already
has everything a token buys, so presenting one must never cost them their own
standing). Issuance is `POST /api/madnetwork/token` (`api/madnetwork_token.go`,
gated `madnetwork.access`): an ordinary authenticated call, since the caller is a
person with an account rather than a node with a card. The wire form is
base64url'd signed JSON in a `Madnetwork-Token` header. A bearer is **not** a
friend for quota purposes — it draws on the member budget like every other
non-friend (§"What a member may cost us"), which is the answer to a home server
enrolling a thousand devices: they share one class ceiling.

## Sharing scope (F5 built; collapsed to three values in F7, built 2026-07-31)

An admin marks each recording with one of **three** scopes, and the node has a
default every recording inherits:

| scope | constant | who is served |
|---|---|---|
| **Madnetwork** *(default)* | `DepthUnlimited` | our whole **community** — every member of our component of the friendship graph, at any distance, with no size limit |
| **Direct friends** | `DepthFriends` (`0`) | only the nodes this admin friended by hand |
| **Local** | `DepthPrivate` (`-1`) | nobody — not on the network at all |

**Shipped node default: Madnetwork** (declared 2026-07-31, reversing the
2026-07-30 note that never shipped). A madshare node shares its library with its
community out of the box. This is the point of the project rather than a
liberty taken with it: a friends network whose default is "share with nobody but
the four people you typed in" is a file server with extra steps, and every node
that ships closed makes the network worth less to everyone already in it.

**The posture that comes with it, stated once and plainly.** Installing a
federated node publishes your library to people you will never meet or approve —
your friends' friends' friends, outward with no radius. That is the deal, it is
the deal by design, and the two things that make it defensible are elsewhere in
this document rather than in a caveat here: nothing at all leaves the community
(§Principals & access), and any branch of it can be cut in one action
(§The membership rule). An admin who does not want that has **Direct friends**
per recording or as the node default, and **Local** for content that must not
travel — both one control away on `/admin/settings`.

**Built 2026-07-31** (F7 items 1–4, one change — narrowing the values and
widening the audience are safe only together). `ValidDepth` now accepts exactly
the three constants, migration 035 snapped the stored in-between values, and
members are served. The node default value did not change: it was already `∞`;
what changed is that `∞` now has an audience.

### Why the ladder collapsed (decided 2026-07-30)

The per-hop ladder (`1` = friends of friends, `2` = one hop further) is **gone**.
It promised something the protocol cannot deliver, and saying so is more useful
than keeping the knob:

- **We only control hop 0.** Every scope value is a statement about *our own*
  behaviour — whom we answer. "Three hops" is a statement about *other people's*
  behaviour: once a friend legitimately holds the bytes, nothing we do decides who
  they hand them to ("torrents do not know borders"). A knob whose enforcement
  lives entirely in other people's cooperation reads to an admin as a guarantee
  and is not one.
- **What it would have cost.** Serving a privileged stranger needs a way to tell
  privileged strangers apart, which is a capability token, delegated issuance, a
  lifetime, a renewal cadence and a revocation story — or, worse, an authorization
  decision computed by walking the *gossiped* graph, which would have turned a
  transparency feature into an access grant and made "whoever a friend calls a
  friend" a credential. The riskiest machinery in the project existed to serve the
  one tier that could not be enforced anyway.
- **What survives.** Every sharing decision is answered by us, on our side, from
  data we hold: a friend is authenticated by the mesh address deriving from its
  node key, a **member** is a key we can place in our own gossiped component, and a
  guest is everyone else. Tokens keep exactly one job, where they are airtight: a
  **listener node** presenting "this bearer is mine, signed by its home server"
  (§Principals & access), which is identity delegation, not distance.
- **The graph decides *whether*, never *how much* — and that is the line that
  matters.** Membership is a use of gossiped records in an access decision, which
  this design refuses for hop counting, so the distinction has to be exact.
  Hop-counted reach would have let hearsay decide *how much* a requester gets, and
  a forged edge would have opened direct-friends-only content. Membership can only
  **narrow** who reaches the Madnetwork set relative to "any node on the mesh", and
  the worst a forged or over-generous edge yields is content its admin already
  marked open to the network. One direction is a leak; the other is a bound that
  can be too loose. Only the second is acceptable, and only the second is built.
- **Nothing about the schema changed.** `recordings.share_depth` keeps its column
  and its three constants; `ValidDepth` rejects the in-between values so nothing
  can write one again, and a migration snaps any stored `1…n` to **Direct
  friends** — narrowing, because an admin who reached for "1 hop" was describing a
  bound we can no longer express, and the safe reading of an inexpressible bound is
  the tighter one. Explicit `∞` pins are snapped back to *inherit*, which under the
  Madnetwork default is effect-neutral today and still worth doing: the pin was
  written when `∞` meant "I don't restrict onward sharing", so it must not survive
  a *later* narrowing of the node default as though the admin had insisted on it.
  **When a value's meaning changes, consent does not carry over.**

**Onward sharing is still not controlled, and that is stated rather than
implied.** A member who holds our bytes may re-share them under their own policy,
and since the community is the default audience, "a member" now means rather more
people than it did. The model does not pretend to bound that: our own answer is
the only thing we enforce, and past it the holders' judgement is backed by the
ability to see the graph and snip a branch (§Trust graph). What *is* bounded is
that neither we nor an honest member ever answers a node outside the community at
all, so onward sharing spreads content within the community rather than out of it.

### The audience model

Every mesh request that reveals or delivers library content is answered *for an
audience*, and the same audience decides both halves — **catalog and bytes
together**, so the node never advertises what it would not serve (the F3 rule,
now enforced per requester instead of uniformly):

**Three mesh classes, one per principal** (§Principals & access), and the class is
what the audience *is* — not something a caller derives from loose fields:

| class | resolved from | scope predicate |
|---|---|---|
| **friend** | a `state='friend'` peer row for the connection's key | `Distance: DepthFriends` → everything except Local |
| **member** | the key is a member of our community (§The membership rule) | `Distance: DepthUnlimited` → Madnetwork, which is the default scope |
| **guest** | anything else — any node we cannot place in our community | nothing, unless the node opts in to serving guest-playable |

Built 2026-07-31 (`federation/federation.go`):

```go
type Audience struct {
    Class     Class // outsider (zero value) | guest | member | friend
    Distance  int   // the reach the class earns, compared against the scope
    GuestOnly bool  // a friend demoted by the user mapping, while it exists
}
```

Four constants rather than the three classes above, because "outsider" and
"guest" are the same principal in two node policies: an outsider is served
nothing, a guest is an outsider on a node that opted in. The **zero value is
`ClassOutsider`**, so a forgotten error path denies instead of granting; before
F7 the zero value was a full friend. Predicates are positive — `Serves()`,
`IsFriend()`, `InCommunity()`, `ServesCache()` — and the SQL clause refuses an
audience that serves nothing outright (`audienceClause` returns a constant
`false` and binds nothing), so the fail-closed zero value holds at the storage
layer too and not only in the handlers.

- **Distance** is compared against the content's scope: a recording is in the
  audience's catalog iff `depth >= Distance`. With three scopes, `DepthFriends`
  and `DepthUnlimited` are the only two distances the model needs, and the
  arithmetic that already exists expresses both — a member's `DepthUnlimited`
  matches *only* Madnetwork content, since `ValidDepth` caps depth there and
  nothing else can satisfy the comparison. The predicate's own comment anticipated
  it ("`DepthUnlimited` visible to any reach we ever grow into"), so the value that
  was inert under F5 becomes live by giving members a distance — no protocol or
  schema change (`database/madnetwork_scope.go`).
- **Membership is a lookup, not a computation over hops.** The reachable-key set is
  the **mutual-edge** walk of §The membership rule — F6's `BuildNetworkMap`
  reachability with the both-ends condition and branch snipping, so a blocked node's
  branch is not a member — cached in memory and refreshed with the graph sync. No
  hop count is ever compared, and no credential is presented. **Access and the map
  are two walks over one store**: the map draws every claimed edge because seeing a
  claim is the point, the access walk requires both ends because granting on one
  claim is not.
- **The class must be a value, not an inference.** Two near-misses found while
  designing F7 and fixed when it was built, both the same mistake — a guard that
  means "is a friend" written as the negation of a bit whose meaning was about to
  change:
  - `seedableBlob`'s cache branch was gated on `!aud.GuestOnly`. A stranger was
    `GuestOnly: true`, so the cache was refused by accident rather than by rule,
    and a member — `GuestOnly: false` — would have passed the guard before anyone
    decided members may have it. Now `aud.ServesCache()`. (Members *are* served
    cache blobs since 2026-07-31 — see "the swarm's only boundary" below — which
    makes this a correctness fix rather than a policy one: what the branch may
    never do is serve a **guest**.)
  - `serveAudience` returned `Audience{}` on a store error, which was `Distance 0`
    — *a full friend*. Inert only because every caller checked the `ok` flag.

  So the type carries an explicit class with positive predicates, the zero value
  denies, and the SQL arguments derive from the class instead of being assembled
  at each call site.
- **GuestOnly** is the per-friend half, resolved from the **user mapping**
  (§Principals & access): a friend mapped to a local account inherits that
  account's rights, and since the local model grants either `content.access`
  (the whole library) or nothing beyond the guest-playable/license policy, the
  mapping collapses to exactly this bit. An **unmapped** friend is the *default
  regular-user identity* — `GuestOnly: false`, i.e. the full published set —
  per the 2026-07-18 decision that unmapped is a rule, not a missing row. So the
  mapping is what an admin reaches for to give a friend *less*.

  **Since the scope collapse the mapping is its ONLY source** (2026-07-30).
  Strangers used to arrive as `GuestOnly` — that is how the guest-open swarm was
  expressed — and they now arrive as `Distance: DepthUnlimited` instead, with
  `GuestOnly: false`. So the bit is set by the user mapping and nothing else, and
  the mapping is being removed (§Principals & access). When it goes, `Audience`
  collapses to `Distance` alone unless a plain per-peer *guest-only* flag replaces
  the account binding. That is the open detail; the audience model itself is
  unaffected either way.

**Where depth lives: on the recording.** Access already lives there
(`license`, `guest_playable` — one audio identity, one license,
`docs/architecture/recording-tagsets.md` decision 9), and sharing is ultimately
about *bytes*, which are per-recording renditions; hiding one appearance of a
recording while serving another would leak the same blob under a different name.
`recordings.share_depth` (migration 030) is `NULL` by default, meaning **inherit
the node default** (`madnetwork.default_share_depth`, a runtime setting on
`/admin/settings`, **Madnetwork**). One override level over one node default — no
artist/album inheritance chain, deliberately: a resolution chain would land in
every catalog and blob-serve query for expressiveness nobody has asked for. Bulk
selection in the Recordings and All Appearances lenses covers "a whole artist"
in practice.

**Per-audience snapshots.** The memoized own-catalog (§Catalog) is no longer one
global snapshot: it is memoized **per audience class**, not per peer. The class
space is small and *closed*, which is what keeps this bounded: a friend, a
guest-only friend while the user mapping still exists, and a member. There is no
per-hop class to multiply it, because there are no hops, and an outsider needs no
snapshot at all. The serial keeps its
meaning — each peer stores the serial of the snapshot *it* was served, so the
not-modified check works unchanged.

**Scope is the only authority on the network, and `guest_playable` is not**
(decided 2026-07-30, built 2026-07-31, replacing the guest-open swarm). F5 served
the blobs and manifests of any guest-accessible recording to any mesh node, *even
when its scope said friends-only* — a second way to be open that quietly overrode
the admin's own setting. Now that "Madnetwork" is a scope an admin can simply
choose, that back door has no purpose and is closed: `guest_playable` and the
license policy go back to meaning what they say locally — what an
**unauthenticated visitor of this server** may play.

What survives is one node setting, `madnetwork.serve_guests`, **default off**: it
answers outsiders with guest-playable content at the byte endpoints only, within
the Madnetwork scope, and never from the cache. A guest's distance is
`DepthUnlimited` rather than F5's `0`, so an outsider can never be served
something restricted to direct friends — **a stranger must never outrank a
member.**

What each mesh class is served:

| | friend | member | guest / outsider node |
|---|---|---|---|
| Local-scoped content | no | no | no |
| Direct-friends-scoped blobs, manifests | **yes** | no | no |
| Madnetwork-scoped blobs, manifests | **yes** | **yes** | no |
| Catalog | **yes** (full scope) | **yes** (Madnetwork subset = the default) | no |
| Holdings (what our cache seeds) | **yes** | **yes** | no |
| Cache blobs | **yes** | **yes** | no |
| guest-playable, ignoring scope | — | — | only if the node opts in (default **off**) |

- **An outsider node gets nothing by default.** Not even guest-playable: over the
  mesh, "unauthorized" means unauthorized (decided 2026-07-30, and the reason F5's
  guest-open swarm is withdrawn rather than merely narrowed). The capability
  survives as an explicit node setting, off by default, for the one case it was
  ever wanted — somebody deliberately running a public archive node whose whole
  point is handing guest-playable content to anyone. That is that operator's
  choice to make, and it should take a switch to make it.
- **The swarm's only boundary is the community** (declared 2026-07-31, replacing
  "cache blobs never leave the friend ring"). Holdings and cache blobs are served
  to **members**, not just to direct friends. The swarm exists to make a track
  reachable from *whoever has the bytes*, and a boundary drawn at direct friendship
  cut the swarm at a line the rest of the model does not use: it made a member's
  fetch succeed or fail on the accident of which node happened to have cached it,
  which is the one thing content-addressed distribution is supposed to stop caring
  about. One boundary, drawn once, in the same place for discovery, for bytes and
  for seeding.

  The cost is real and accepted. Seeding a cache blob means re-serving content this
  node did not publish and whose licence it cannot vouch for, and a holdings list
  says *somebody here fetched this* to a wider audience than before. Both stay
  inside the community, both are what `seed_cache` (default on) turns off for an
  operator who does not want them, and neither is ever offered to a guest — a
  stranger is told nothing about our cache, as before.
- **A blocked peer is refused everything**, as before, and since F7 its whole branch
  also stops being members (§The membership rule).

**Tokens are not part of reach any more** (decided 2026-07-30, superseding the
2026-07-25 sequencing note). They existed to identify a privileged stranger at
depth ≥ 1; with the ladder gone there is no privileged stranger, and every requester
resolves to one of the three mesh classes without presenting anything.

What remains for tokens is the **listener node**, and it is now doing a second job
worth naming. A madplayer is a stranger to its home server's friends *and* it is
usually invisible in the graph — it publishes no friend list and appears in nobody
else's, so no membership lookup can place it. The token is therefore both "this
bearer is mine" and the escape hatch for a node the component cannot vouch for by
itself: one issuer we place in our own community, one hop, no chain
(§Principals & access, "The capability token").

**The legal frame** (madshare.org), stated more plainly than before: madnetwork is a
**community of friends** — a mesh of nodes that each had to be deliberately
friended in by somebody, joined transitively, not a public web server. Nothing
federation does exposes anything to the anonymous internet: the mesh is the only
transport, every requester is authenticated by a key it must possess, a node
outside the community is served nothing at all, and a node's own `/files/*` policy
is a separate, local decision.

Inside the community, sharing is the default and restriction is the exception.
That is a deliberate posture rather than an oversight, and it should be read as
what it is: **sharing with the community is publication within the community**.
The content is fetchable by every member — people the admin never approved
individually, vouched for by a chain of friendships they can inspect — and the
admin who runs the node owns that. What it is *not* is publication to the world:
the boundary is enforced on every request, not merely stated. An admin who wants a
narrower line has **Direct friends** per recording or node-wide, and **Local** for
content that must never travel.

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
- **Blocking:** a blocked peer is refused the *entire* protocol surface (even
  ping, HTTP 403) by the mesh-side auth wrapper. Unblock returns the peer to its
  pre-block state. Since F6 a block also publishes a **distrust mark** carrying
  its reason, drops the peer from our published friend list, snips the branch
  behind it **on the map**, and cuts the underlay link wherever we dialled it
  (built 2026-07-30; an inbound link is the documented exception). Snipping stops
  at the drawing today — the records behind a cut branch are still stored,
  relayed and admitted, which §Forgetting is about.
- **Admin surface:** `/admin/network` (own card, import form, peer list with
  accept/block/unblock/remove/rename/user-mapping; pending-request badge on the
  dashboard; the F6 network map) over `/api/admin/federation*`, all gated
  `federation.manage`.

#### The trust graph is a graph (built 2026-07-31)

Two nodes that are both friends of a third must be able to friend **each other**,
and a node must be able to sit in several branches at once. Nothing in the state
machine ever forbade it — a pair request from an unknown key is answered the same
way whoever sends it — but two things made it look forbidden from the admin's
seat, and both are now fixed.

- **Friending by key.** The import form and `POST /api/admin/federation/peers`
  take `{"public_key": …, "name": …}` as well as a card, and the network map's
  detail panel offers **Ask to be friends…** on any node it draws that we have no
  row for. The map already carries every node's key — that is the whole identity,
  and a card adds only a claimed name — so a friend of a friend is friendable
  without their admin exporting anything. Both surfaces show the full key, and the
  map's confirm says outright that a name out there is hearsay. Nothing about
  *deliberate* changes: this sends a request, the far side still records a
  `pending_incoming` its admin must accept. A **mesh address is refused with its
  own message**, because an address is derived from a key and cannot be turned
  back into one.
- **Pairing says why it has not converged.** Every failure in `pairWith` used to
  be a silent `return`, so an unreachable node, a refusing node and a node whose
  admin has not clicked Accept were one indistinguishable `pending_outgoing`. Each
  attempt now records a `PairAttempt{At, Result, Error}` — in memory on the node,
  since it describes the last try rather than the friendship — carried on the peer
  row as `last_attempt` and rendered on the card: *request delivered, waiting for
  their admin* (the common case, and not a fault), *nothing answered*, or the far
  node's own refusal text. It is logged too, but only **when the outcome changes**:
  the sweep retries every minute, and a node that is merely switched off must not
  write a line a minute about it.

Verification: `federation/friendgraph_test.go` friends A–B and B–C on a chain
underlay where A and C have no link, then friends A–C over two hops; `meshlab
friend A B` does the same with real processes on a running lab.

### Names are a convenience, the key is the identity (built 2026-07-30)

Self-naming stays exactly as it is: `[federation].name`, falling back to the host
name, is what a node says in the pair handshake — "hello, my name is …" and
nothing more. What needed fixing was the receiving side, where **three different
names were collapsed into one `federation_peers.name` column**:

| | what it is | who owns it |
|---|---|---|
| self-name | what we call ourselves on the wire | this node's config |
| heard name | what a peer calls *itself* | the peer — a claim, refreshable |
| local label | what *we* choose to call that peer | this admin, always wins |

Before migration 033 the column was seeded from the card, then overwritten by a
rename — which destroyed the claim — and afterwards never refreshed at all,
because `pairWith` backfilled the name only while it was empty. So a peer that
renamed itself stayed under its old name forever, and an admin who renamed a peer
could no longer see what that peer calls itself.

**Built shape.** `federation_peers.heard_name` holds the claim and `name` is the
local label; nothing writes both. The label is written *only* by an admin rename
(`RenamePeer`, and clearing it is allowed), the claim *only* by
`UpdateFederationPeerHeardName` from a contact, and `Peer.Label()` resolves
`local label ?? heard name ?? empty`. `peerLabelExpr` is the SQL twin for the
browse surfaces that only ever show a name; `peerLabel` in `admin/network.js` is
the client one.

**A peer is never named by a blank**, which needs one more step than `Label()`,
because 033 left *every* row unlabelled: until an admin renames a peer or we hear
its name, `Label()` is empty. `Peer.Display()` is the rendering form —
`Label() ?? short key`, the Go twin of `peerLabel` — and it is what log lines and
stats rows use. `Label()` deliberately keeps returning empty, because the network
map's `displayName` has a better fallback than the key: the name the *graph* uses
for that node. Pinned by `TestPeerDisplayNeverBlank`.

- **Refreshed on every successful contact.** `GET /madnetwork/v0/ping` now
  answers with the node's own `name`, so the 1-minute refresh loop keeps every
  friend's claim current — a node renaming itself is heard within a minute
  instead of never. Pairing (both directions) refreshes it too. The field is
  additive, so a peer that does not send one simply leaves the last claim
  standing, and a write only happens when the name actually changed. This is also
  the first field of the NodeInfo-style health card §Availability sketches.
- **Which name we publish and show.** Gossiped edges carry `Label()` — the
  publisher's own label for that friend, which is what a `GraphEdge` name has
  always meant. On the map, `displayName` resolves best-evidence-first: our label,
  then what the node told *us* directly, and only then the name the graph gossips
  about it, which is hearsay from third parties.
- **The migration cannot classify old rows** — a value seeded from a card is
  indistinguishable from one an admin typed — so 033 moves `name` into
  `heard_name` and starts every row with no label. A peer that had been renamed
  reverts to its own name on the next contact and can be renamed again in two
  clicks; the alternative would pin a name the admin never chose and reproduce
  exactly the bug being fixed. Failure visible and recoverable beats failure
  silent and permanent.
- **Wherever a node is shown, its mesh address or key is rendered beside the
  name** — peer cards and the F6 network map above all. The peer card also shows
  `calls itself “…”` whenever a local label is hiding a different claim: the label
  wins, but it must never make the peer's own name unreadable.

- **On the map this matters more than in the peer list.** Once friend-list gossip
  lands, most nodes on the graph are ones we have no relationship with, and their
  names arrive *second-hand from a friend*. A name there is hearsay about a
  stranger, so nothing may be identified by it.
- **Impersonation is a naming problem, not a hole.** Any node may call itself
  anything, including exactly what a friend calls itself. There is no fix at the
  name layer and none is needed — the address is the identity, the name is a
  label, and the UI must never let the second stand in for the first.
- **Sanitize peer-supplied names** — *done 2026-07-30.* Detail below.
- **Capped at 64 runes** — *done 2026-07-26, `MaxPeerNameRunes`; the naming split
  above is what remains of this entry.* 64 clears a DNS label (63 octets), so no realistic
  host name is ever truncated, while staying far below anything that could
  disrupt a layout. The previous cap counted **bytes** (`name[:100]`), which made
  the effective limit depend on the script — 100 characters of ASCII, 50 of
  Cyrillic or German umlauts, 25 of emoji — and cut a 3-byte character in half at
  that boundary, storing invalid UTF-8 for CJK names. The UI truncates further
  for display (~24 characters with the full value on hover); that is a rendering
  choice, not a storage limit.

#### Name sanitization (built 2026-07-30)

**This is not an XSS fix, and must never be sold as one.** The admin UI already
renders names safely — `el()` in `webui/static/js/admin/shared.js` assigns
`textContent` and appends string children as text nodes, and its `html:` escape
hatch carries trusted icon markup only. Escaping stays the defense against
injection. Sanitizing is about **display integrity**: a name should render as
what it is, and two different nodes should not be able to render identically.
Recording the distinction because the failure mode is somebody later deciding
the sanitizer makes escaping unnecessary.

`CleanPeerName` is the single choke point and stays that way — every name passes
it, whether from a node card (`ParseCard`), a pair request
(`handlePair`/`pairWith`), a gossiped friend list (`ParseGraphRecord`), an admin
rename (`RenamePeer`), or this node's own `[federation].name`/host name. It and
`CleanMarkReason` are two caps over one `sanitizeLabel`, so a mark's free text —
longer, and read by someone deciding whether an accusation applies to them — gets
the same treatment. The rules, **in this order**, because the order is
load-bearing:

1. **Invalid UTF-8** — drop the offending runes (Go decodes them as `U+FFFD`).
2. **Strip Unicode categories `Cc` and `Cf`.** `Cc` is the control characters:
   C0/C1, newline, tab, DEL. `Cf` is the elegant part — one category test covers
   the bidi overrides (`U+202A`–`U+202E`, `U+2066`–`U+2069`, which can visually
   reverse a rendered name), the zero-width characters (`U+200B`/`200C`/`200D`)
   that make two different names look identical, and `U+FEFF`.
3. **Strip `Co`** (private use): vendor-specific glyphs and tofu.
4. **Normalize to NFC**, which collapses `é` written as one rune against `e` plus
   a combining accent — another way two names render identically while differing
   byte for byte. `golang.org/x/text` is already a direct dependency, so
   `unicode/norm` costs nothing new. It runs *after* the strips, so two characters
   separated by a zero-width joiner still compose once it is gone, and *before*
   the mark bound, since composing removes marks that step would otherwise count.
5. **Bound combining marks** (all of `M`: `Mn`/`Mc`/`Me`) per base character — the
   "Zalgo" stack that renders as a vertical smear over neighbouring rows. Two
   marks per base is generous for every living script. The count is of marks
   *following* a base character, so a precomposed `á` carries two more; that caps
   the rendered stack without having to reason about which scripts precompose. A
   mark with no base — a name opening on a floating diacritic, or one following a
   collapsed space — is dropped.
6. **Then** apply the 64-rune cap. Capping first would let stripped junk consume
   the budget and truncate the real name.
7. **If nothing survives, the name is empty** — display falls back to the short
   key, exactly as an unnamed peer does today. Never render an empty label.

Whitespace collapse runs on the same pass as 5–6: runs fold to a single `U+0020`
and the ends are trimmed. Because `unicode.IsSpace` covers the `Z` categories,
this is also what folds `U+00A0` (no-break space) onto the plain space — one more
pair that renders alike.

**The accepted cost, stated rather than hidden:** stripping all of `Cf` also
removes `U+200C` (ZWNJ), which is orthographically meaningful in Persian and
Arabic, and `U+200D` (ZWJ), which joins emoji families — 👨‍👩‍👧 becomes three
separate people. That is accepted for a *label* that carries no identity role.
The narrower alternative, "strip `Cf` except ZWJ/ZWNJ", reopens precisely the
invisible-difference vector this rule exists to close, so it is not the default.

**Homoglyphs remain unsolved and that is fine.** Cyrillic `а` against Latin `a`
cannot be filtered without mixed-script heuristics that punish legitimate
multilingual names. The answer stays the one this whole section rests on: the
mesh address is displayed next to the name, and identity is the key.

Existing rows keep their unsanitized names — the sweep is not worth a migration,
because a name refreshed from its peer on the next contact (planned above) heals
itself.

The tests are a golden table (`TestSanitizePeerName`): a `U+202E` reversal, a
friend's name padded with `U+200B` into a second peer, an embedded newline, a
private-use glyph, a Zalgo stack, a decomposed `é`, an emoji family and a Persian
ZWNJ name (both documenting the loss), and names that sanitize to nothing.
`TestSanitizeCapsLast` pins rule 6 by padding a full-length name with 200
zero-width spaces and asserting the name survives whole.

(The rename field in `webui/static/js/admin/network.js` already mirrors the cap
at 64 — `maxlength` counts UTF-16 units rather than runes, so it is marginally
stricter for emoji, which is the harmless direction.)

## Trust graph, transparency & defense

- **Transparency:** nodes gossip their friend lists, so every admin can see the
  reachable network as a graph — who is connected to whom — in an admin UI
  (network map). Not bounded by a radius: signed records relay outward until the
  store holds the whole connected component (§Friend-list gossip below).
- **Blocking ("snipping a branch"):** an admin can block any node key. Blocking
  is **manual** — there is deliberately **no automatic rating/critical-mass
  system in v1** (an automatic reputation score is a weapon for intra-network
  wars; madshare.org's own worry). Instead:
  - A block cuts all application-layer service instantly: no catalog, no
    streams, no chunks, no token issuance; existing tokens expire on their own.
  - Where the peering link is ours (direct ygg peering with that node), we also
    de-peer, so the blocked node loses us as transit. (On shared public-mesh
    segments, transit below the app layer is Yggdrasil's business — the
    app-layer cut is the guaranteed part.) *Built 2026-07-30*, in the refresh
    sweep rather than at block time: a configured peer URI carries no key, so we
    only learn who is behind `tcp://host:port` after the handshake, which means
    the live link list is the only thing that can be matched against the blocked
    set. Re-running it every sweep is also what makes the cut durable without a
    suppression list — config re-adds the peer at startup, and a minute later it
    is cut again, for as long as the block stands. Two limits, both real:
    yggdrasil's `core.RemovePeer` (the underlay call — not `Node.RemovePeer`,
    which forgets a peer *row*) reports "not configured" for a link on a shared
    segment (that is
    the case the parenthesis above describes), and an **inbound** link is skipped
    entirely because yggdrasil v0.5.14 *panics* when asked to remove one (nil
    cancel func; see `.issues/open-issues.md`) and exposes no handle for it
    anyway. A blocked node that dialled us therefore keeps its transit until it
    disconnects, while getting nothing from the application.
  - Blocks are **published as signed distrust marks**, relayed network-wide like
    the friend records and carrying a short reason: "see whom the network does
    not trust, and why." Every block publishes one — there are no private blocks.
    Readers factor them in manually; nothing is automatic, and the accepted risk
    of a public ledger is spelled out in §Friend-list gossip.
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
     farm inflates nothing and dies with a single snip. *Built — see "Where the
     weighting applies" below.*
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
- Reach beyond our own friends **never ships before** the transparency and
  blocking tooling — a network you can see further into than you can defend is
  the wrong order. This is the reason the build plan puts defense in F6 and
  reach in F7, in that order, rather than in one phase: the dependency runs one
  way only, so F6 stands alone and F7 does not.

### Where the weighting applies (F7 item 10, built 2026-08-01)

"One branch is one voice" is a rule about **ordering**, and it is only worth
anything if it reaches every place an order is decided by a count. Item 8 landed
it on the *Most held* lane; this completes it.

The attribution itself is the network map's, exposed as a lookup table
(`federation/branches.go`, `Node.BranchMap` → node key → the direct friends it
reaches us through). Deliberately the **same walk** the diagram is drawn from:
a holder in a track's ⓘ panel links straight to its node on that map, so a
ranking explained by one graph beside a diagram drawn from another would be two
answers to one question (`TestBranchesMatchTheMap` pins it). It is a separate
entry point from `NetworkMap` only because that one also groups distrust marks
and derives a mesh address per node — ed25519 work that belongs on a diagram an
admin opens occasionally, not on a search-as-you-type. Memoized on the
membership TTL, for the same reason and with the same safe direction of
staleness: a branch that just appeared is briefly counted as its own voice,
which understates corroboration rather than inventing it.

The counting rule is one function (`database.BranchMap.Voices`), because a rule
restated per surface is a rule half-applied somewhere. Around it:

- **A version's place in a crossing** — the sharpest one, and the reason this
  half was worth finishing. A track row expands into versions ordered
  most-widely-held first, and `renditions[0]` of the leading version is what
  Play, Queue and Materialize act on. Ordered by raw holders, a farm of keys
  behind one friendship could make its claim the default pick for everyone who
  browses to that track — the "rickroll" attack landing on the one control
  people actually press. It is now ordered by voices, holders only as a tiebreak.
- **The *Most held* and *Not in your library* lanes** — the two SQL ranks by a
  raw holder count. *Not in your library* is the lane the page opens with, which
  makes it the more valuable of the two to an attacker.
- **Not the other three lanes, on purpose.** *From your direct friends* is
  already branch-weighted by construction, since every direct friend is the root
  of its own branch — re-weighting it would replace that with the wider count and
  lose the lane's whole subject. *Only one node has it* is one holder, which is
  one branch whatever the graph says. *New on the network* ranks by date, not by
  agreement. The predicate is `api.laneWeighted`, stated as a list so the answer
  to "which counts are weighted" is readable in one place.

Two properties are worth stating because they are what keep this honest:

- **Degradation is the same rule in a smaller world.** No federation node, no
  graph yet, a failed read, or a holder the graph cannot place: one source, one
  voice. Never a collapse to zero, and never an ordering refusal — a browse is
  not worth failing over a ranking input.
- **Weighting changes the order, never the facts.** A row still reports the
  honest holder count; the branch count appears beside it *only when it is
  lower*, which is the only case where it says something the holder list does
  not — several nodes, one voice. Sending it always would put "5 nodes ·
  5 branches" on every row, which is wallpaper, not transparency.

What this does **not** answer, and does not pretend to: **volume from a single
honest branch**. One friend with fifty thousand badly tagged albums is still one
voice and still fifty thousand rows. That is a clustering problem, tracked in
`docs/ui/madnetwork-page.md` §Open.

### Contradicted identity claims (built 2026-07-30)

A peer's catalog makes claims this node can *check*, and when a check fails the
admin hears about it with the evidence attached. This is the "Detect → details"
arm layer 3 promises; the "→ block → snip → publish the mark" half is F6's
existing toolkit. A false audio identity is worth singling out because it is
**provable** — unlike a tasteless tagset, it is arithmetic — which is exactly what
makes it fair to put in front of an admin as grounds for blocking.

What is checkable, cheapest first:

- **Against blobs we already hold — no download, no request** (`held_blob`). For a
  hash in our own library we know the true fingerprint. A peer advertising that
  hash with a materially different one is contradicting bytes we can hash
  ourselves. The check is a SQL join over the *overlap*, so it costs a comparison
  per hash both sides have and nothing per hash only one of us has. This case is
  **airtight**: identical bytes cannot fingerprint differently.
- **Against a materialized download** — the same check, reached from the other
  side. The pipeline re-fingerprints fetched audio before it joins a recording
  (§Catalog), which simply makes the download one more blob we hold, and the next
  sync round compares the origin's standing claim against it. That is why the
  checks read the *cached* catalog rather than a freshly received snapshot: a
  peer's claims stand still while our own library moves, and a not-modified sync
  round must still re-check. No separate code path, one rule read once.
- **Against the peer's own grouping — needs no wire claim at all** (`grouping`). A
  `recording_key` asserts "these renditions are the same audio". Hold two of them
  and the assertion is testable locally without the peer's cooperation: *both*
  fingerprints in that comparison are ours.

**The threshold is the local one.** A contradiction is a start-aligned bit-error
rate above `database.maxBitErrorRate` (0.10) — the same number
`ResolveRecording` groups renditions by. Reusing it makes a finding explainable in
one sentence: *the claim would not group with our own bytes by the very standard
this node uses to decide that two files are the same audio.* Under 16 compared
words the check declines to answer, because a claim we cannot check is not a claim
we distrust.

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

**Storage** is `federation_claim_reports` (migration 034): one row per
(peer, kind, hash, other_hash) with an admin disposition (new / dismissed /
acted), so a repeating check refreshes the measurement and re-alarms nobody — and
a dismissal is never overwritten by detection. The evidence travels with the row
(both compared heads, both fingerprinter versions, the BER and the word count), so
a finding survives the catalog replace that produced it. Rows CASCADE with the
peer: forgetting a node forgets what we found about it. The admin surface is
`GET /api/admin/federation/reports` + `PATCH …/reports/{id}`, and a **count badge
on the dashboard** beside the pending-peer one is the whole notification design;
this must not become mail.

**The catalog carries the fingerprint claim, as a bounded head.** The F2 wire never
had one; `CatalogRendition` now has `fingerprint: {algo, version, words, head}`,
additive so an older peer simply contributes nothing checkable. `head` is the first
`federation.ClaimHeadWords` (64) raw sub-fingerprint words, base64 of the same
little-endian packing the DB stores — **not** the whole fingerprint, and the reason
is measured rather than guessed: a real fingerprint is ~950 words (3.8 KB packed)
for a four-minute track, a snapshot is re-sent in full whenever its serial moves,
and shipping all of it would add ~5 MB per sync to a thousand-rendition catalog on
a 15-minute cadence between intermittently-online home servers. 64 words is ~15 s
of audio and 2048 compared bits: the same bytes score 0, unrelated audio lands near
0.5. The comparison is start-aligned exactly like the local matcher, so a head is
the same kind of evidence measured over less of it. Publishing it leaks nothing new
— a friend already gets the hash and the full tag text — and the browse endpoints
strip it, since a browser has no use for 340 bytes per rendition. This is also what
layer 1's "auto-flag tagsets that conflict with a recording's dominant label" needs
to work across nodes.

The *byte*-level lie needs nothing here: bytes that do not hash to the requested
hash never enter the cache and cost the provider its place in the swarm
(§Distribution). This item is about claims that survive byte verification.

### Friend-list gossip & the network graph (F6, built 2026-07-26)

Settles the former Open question 1. The goal an admin actually asked for is
**the whole network, not a radius**: every node reachable through any chain of
friendships shows up on the map. The design that delivers that without flooding
is to relay *records*, never views.

**The record.** Each node publishes exactly one document about itself:

```json
{ "protocol": 1,
  "origin":  "<hex ed25519 node key>",
  "seq":     7,
  "issued_at": 1753400000,
  "friends": [ {"key": "a1b2…", "name": "studio", "since": 1750000000} ],
  "sig":     "<ed25519 over the canonical encoding>" }
```

- **Per edge: key, name, `since`.** The mesh address is never sent — it derives
  from the key (`AddrForKeyHex`). `since` is when the friendship was made, a
  cheap durability signal a five-year-old edge should get credit for when
  trust weighting arrives in F7; it also leaks a timeline of who befriended
  whom, which is the price.
- **Signed by the origin's own node key** — the key that already *is* the
  identity, so no new PKI. A relay carries the bytes untouched: it can withhold
  a record, never forge one.
- **The names are hearsay.** Sanitized and rune-capped on receipt exactly like
  peer names, and the map renders the address beside every one — most nodes on
  the graph are strangers whose names arrived second-hand (§Friendship, naming).

**Propagation: friends relay, nobody crawls.** A node opens connections to its
own friends and to nobody else, ever. Each friend serves its whole store, so
records ripple outward one ring per sync round until every store holds the
entire connected component. No hop limit — the radius is unlimited by design.

- Signatures are what make this safe: A can hand me X's record and I verify X
  wrote it without X and I ever meeting.
- **Rejected — dialing nodes directly** (take a key from a friend's list, connect
  to that node, ask it yourself). It costs N² connections per round (500 nodes ≈
  250 000 dials) to move a graph that changes monthly; it requires opening the
  friends-only mesh endpoints to strangers; and it routes around the trust model
  by making every node interrogable by anyone. Relaying is both cheaper *and*
  more complete, since it surfaces nodes we could not dial at all.
- There is no other discovery path to choose from: Yggdrasil does not enumerate
  the mesh, so **the friend graph is the discovery mechanism**.

**Convergence: highest `seq` wins.** A receiver keeps one record per origin. A
copy of a `seq` already held is dropped and *not* re-propagated, so loops die on
their own — no hop counts, no TTL-based loop control. This is link-state
routing's rule, and Yggdrasil's own one layer down. The origin bumps `seq` when
its friend list changes and on the heartbeat below.

**Bandwidth: digest-then-fetch on the catalog cadence.** Sync rides the existing
~15-minute catalog loop. A round exchanges a digest of `{origin, seq}` pairs
(~48 bytes per node), then fetches only the records whose `seq` is missing. The
digest carries a serial and answers `since=` with a not-modified reply, exactly
as `handleCatalog` already does. A 10 000-node network is ~480 KB for a *full*
digest; a realistic mesh of a few hundred is 10–30 KB; an unchanged graph costs
one small round-trip per friend and moves no payload at all.

**Expiry: 7 days, refreshed every 6 hours.** The origin re-signs on the
heartbeat even when nothing changed; receivers drop a record 7 days after
`issued_at` and stop serving it. Chosen against this network's actual
population — intermittently-online home servers (§Goal) must survive a weekend
offline — while an abandoned key fades from every store inside a week with
nobody acting. Rejected: 24 h/1 h (a two-day trip drops you off the map, and 24×
the chatter for a graph that changes monthly) and 30 d/24 h (a snipped branch
lingers a month in stores that forgot why).

**Publishing is node-level and default-on.** Runtime setting
`madnetwork.publish_friend_list`, default on, matching the network's default-∞
transparency. All edges or none — deliberately no per-peer granularity.

- **The switch means "I publish no record", and nothing more.** A shared edge has
  two ends, and the other end is not yours to silence: friends' records still
  name you, so you stay on the map with visible edges and only your own list is
  missing. The UI text must say precisely that. Anything softer sells an
  invisibility that does not exist.
- **It also makes you a dead end** (found while building, 2026-07-26; pinned by
  `TestSilentNodeWallsOffItsFriends`). A silent node still collects and serves
  its friends' records, so it has not left the network — but since nothing it
  publishes names anyone, its friends are never vouched for to each other, and
  the admission rule below refuses them as unattributed. Two friends of a silent
  node cannot discover each other through it, in either direction. That is the
  correct behaviour rather than a gap to patch: admitting a record because it
  *claims* an edge to someone we know is exactly the sybil vector the rule
  exists to close. It is also the strongest argument for the default being on,
  and it belongs in the setting's UI text — the cost is not "my list is
  private", it is "my friends stop seeing past me".

**Anti-flood bounds** (engineering limits, not policy):

- at most 512 edges per record — a longer list is refused, not truncated;
- a per-branch quota on how many origins any single friend may introduce
  (`MaxOriginsPerBranch`), and blocking that friend drops everything that entered
  through it;
- at most one accepted new `seq` per origin per minute;
- a record whose origin is named by nobody in our store is junk, and dropped.

Together these bound a sybil farm to display noise: a farm behind one edge is
one branch, and dies with one snip (layer 2 above).

**These numbers bound our store, never the community** (restated 2026-07-31,
because F7 makes it easy to misread). They cap what one friend can cost *us* in
rows; they are not an opinion about how large a legitimate network may be, and
**membership is not derived from them**. A community that outgrows
`MaxOriginsPerBranch` is a number to raise, not a boundary being defended — and
the reason that stays safe is that membership requires a *mutually declared* edge
(§The membership rule), so the quota counts nodes that put their own signature to
belonging rather than names a friend merely mentioned.

**Distrust marks** are a second document type, independently signed, with their
own lifetime, relayed on the same sync:

```json
{ "protocol": 1, "origin": "…", "seq": 3, "issued_at": 1753400000,
  "marks": [ {"key": "e5f6…", "at": 1753300000,
              "reason": "advertised hash 3a9f… with a fingerprint
                         contradicting our own copy"} ],
  "sig": "…" }
```

- **Every block publishes one — there are no private blocks.** Blocking is a
  social act here by construction. Two consequences of that, both built: the
  block modal states plainly that the mark relays network-wide and is readable
  by its target, and it collects the reason there rather than after the fact;
  and **a key can be blocked without ever having been a peer** (`BlockKey`,
  `POST /api/admin/federation/block`), since the point of seeing past your own
  friend list is being able to act on what you see. A blocked peer also drops
  out of the friend-list record — a block is not a friendship, and publishing it
  as one would misstate the graph.
- **Key, when, and a short reason.** A bare key is an anonymous downvote: the
  reason is what lets a reader judge whether it applies to them, and it pairs
  with the contradicted-claim reports above, which produce exactly this evidence.
  Capped and sanitized on the peer-name rules at a larger cap (280 runes).
- **Relayed network-wide**, so everyone sees whom everyone distrusts — including
  the node being marked.
- **Accepted risk, stated plainly.** This is a global, public accusation ledger,
  carrying free text, with no rebuttal path, readable by its target: the
  intra-network-war warning this section opens with applies to it squarely. It
  was chosen with that understood (2026-07-26). Three containments, none of
  which soften the choice:
  - **marks expire on the record schedule** — unblock, stop refreshing, and the
    mark is gone from every store within 7 days. A ledger that forgets is
    recoverable; a permanent one is not. **Lifting a block does better than
    waiting out the TTL** (built 2026-07-26): clearing the last mark publishes an
    *empty* record rather than simply ceasing to refresh, so the record carrying
    the accusation is superseded on every node at the next sync instead of
    standing for up to a week after the admin withdrew it. The asymmetry is
    deliberate — a node that has never blocked anyone publishes nothing at all,
    since an empty record would cost every store a row to say nothing.
  - **display is branch-weighted** — one branch is one voice (layer 2), so a farm
    publishing 10 000 marks against a key renders as a single entry.
  - **nothing is automatic**, as everywhere else here: a mark is evidence put in
    front of a human beside the Block action, never an input to a score.

**F6 itself changed nothing about who may fetch what.** Every requester stayed at
distance 0 and the wire's access rules remained exactly F5's, which is what made
the phase safe to ship on its own. F7 gives the store one access job — answering
whether a key is in this component at all (§Principals & access) — and no more than
that: `Audience.Distance` never became a hop count, because the ladder that needed
one is gone (§Sharing scope).

**Storage** is a cache, like `federation_catalog`: rebuildable, referenced by
nothing local, dropped and refilled without consequence. Migration 031 holds the
records keyed by origin (`seq`, `issued_at`, payload, signature, which friend it
arrived from, expiry) beside the edges and marks denormalized off them, so
admission checks and the map are queries rather than a scan that decodes every
payload. The payload column is the record **verbatim** — nothing re-encodes it,
because the signature covers the bytes as written and a record may carry fields
this build cannot parse. Migration 032 adds `block_reason` / `blocked_at` to
`federation_peers`, the evidence a published mark carries.

**Wire**: `GET /madnetwork/v0/graph` (digest, `since=`-aware) plus
`POST /madnetwork/v0/graph/fetch` for the raw bytes of named records — a POST
because the request is a list of keys, too many for a query string once a
network is more than a handful of nodes. Friends-only like the catalog, and
additive, so an older peer 404s and simply contributes nothing.

**Admin surface**: `GET /api/admin/federation/graph` (gate `federation.manage`)
answers the computed map — nodes with distance, branch attribution and marks,
plus the edges between them — and `POST /api/admin/federation/block` blocks by
key, since most nodes on the map have no peer row at all. The map computation is
a pure function over peers, edges and marks, so branch snipping and mark
weighting are tested without a mesh.

### Forgetting: what a block or a removal takes with it (built 2026-07-31)

Ending a friendship is instant everywhere it is *enforced* — the peer row changes
state, the mesh door refuses a blocked node, our own published record drops them
on the next sweep. It is not instant in what we **remember about the network**,
and the gap is visible: a friend we removed is still drawn joined to us, and the
strangers we only ever heard about through a node we have now cut are still on
our map, still relayed onward, still admitted. Three concrete gaps, and the third
falls out of the first.

**1. An edge to us is a local fact, not hearsay.** `BuildNetworkMap` links a pair
from either side's claim, including claims *about us*. So a node whose admin
never removed us keeps publishing "friends with you", and we keep drawing it —
for the record's whole 7-day life, refreshed forever if they keep republishing.
The fix is the smallest one in this list and the one everything else rests on:
**any edge with our own key at an end comes from `federation_peers` alone**. If
we are not friends with X, there is no X–us edge on our map, whatever X says.
Independently of removal this closes a small integrity hole: any node already in
our view can publish a friendship with us that does not exist, and today we draw
it — which puts a node we have never met on the inner ring, at distance 1, as a
branch root. Admission (`GraphKnowsKey`) keeps a *complete* stranger's record
out, so this is bounded to nodes someone already vouched for; it is still a claim
about us that we are in a position to know the truth of, and do not check.

The asymmetry is deliberate and stays: *other* nodes' edges remain single-claim
on the map, because an edge somebody claims is worth seeing (§The membership rule
is where mutual claims matter). Our own edges are neither single- nor
mutual-claim. They are not claims at all.

**2. Reachability decides what we keep, not just what we draw.** The map already
refuses to discover through a blocked node, so the branch behind one disappears
from the picture. Nothing else follows it: those records are still offered in our
gossip digest and served to friends who ask, their origins still satisfy
`GraphKnowsKey` so more records from them are still admitted, and they sit in the
store until `expires_at`. A block is complete on screen and half-done underneath.

So the one walk from our key should decide all four: whether a record is drawn,
whether it is **relayed**, whether its origin **counts as known**, and whether it
is **kept**. Unreachable ⇒ dropped, on the sweep that already runs `ExpireGraph`.
This is safe precisely because the graph store is a cache in the sense
`federation_catalog` established — rebuildable from the network, referenced by
nothing local, safe to drop — and it is the same walk F7's membership check needs
memoized anyway, so it is shared work rather than new work.

**3. A removed friend's branch dies with the edge.** Today removal is *worse* than
blocking: the peer row is gone, so nothing marks them as a node not to discover
through, and their one-sided claim (gap 1) keeps the whole branch behind them
alive and admitted. `received_from` is `ON DELETE SET NULL`, so the records they
introduced also lose the attribution that would let an admin see where they came
from — they survive, orphaned, vouched for by nobody. With gap 1 closed there is
nothing new to build here: no edge from us means unreachable, and unreachable is
collected by gap 2. That is the property to preserve — **removal and blocking
should not need separate forgetting machinery**.

**"Unless we have another connection to them"** is the existing multi-source BFS
with its merging branch labels, unchanged: a node also vouched for by another
friend stays, at whatever distance and with whatever branch labels remain. The
condition is already expressed correctly; it simply governs more than drawing.

**What must not be forgotten.** Our own peer rows stay on the map with no edge
drawn — a blocked node an admin cannot see is a block they cannot lift. Our own
distrust marks are ours regardless of who is reachable. And a blocked peer's
cached catalog stays (hidden from browse) so lifting the block restores service
without a re-sync — that is a direct relationship of ours, not branch data;
`RemovePeer` already CASCADEs it away, which is the right difference.

**The costs, stated.** Unblocking or re-friending re-syncs what we dropped, on the
next digest round — `UnblockPeer` already nudges the loop, so it refills promptly
rather than on the 15-minute cadence. And if we were the only path between two
halves of the network, cutting the branch slows *their* convergence, not just our
view: accepted, because we are not a relay for a branch we have severed, and a
network of friendships has other paths by construction.

**Why this stops being cosmetic under F7.** After F7 this same graph decides who
is served, so a branch we believe we cut but still hold is not a stale drawing —
those keys are members, and members get the library. It should therefore land
with or before F7's membership walk, and its test is the one that would otherwise
be missing: remove a friend, and assert the nodes seen only through them are gone
from the map, from the digest, from admission **and** from the served audience.

**Smaller staleness in the same family**, fixed with it: the in-memory
pairing-attempt note (§Friendship) used to survive a `RemovePeer`, so re-importing
a key we once failed to reach showed a "last try" from before the removal.
`RemovePeer` now drops it — best-effort, since failing a removal over a log note
would be the worse trade.

**Admission needs no new rule, and that is the point.** `GraphKnowsKey` admits a
record whose author some record we hold already names. Once the sweep *drops* the
records that named a cut branch, the branch stops being named — so the same
unchanged admission check refuses it on the next round. Dropping and refusing are
the same act seen twice, which is why part 3 costs nothing: an origin re-offered
by a second friend is one that friend's record still names, and it is admitted
because it genuinely is reachable again.

### Refreshing the graph on demand (built 2026-07-31)

Gossip rides the catalog cadence (`sweep`, 15 minutes), and `CatalogSyncedAt` is
bumped on the not-modified path too, so an admin who has just changed something
waits out the timer with no way to say "look again now". `Nudge` does not help:
it wakes the loop, the loop re-checks the same timer, and the graph sync is
skipped. A **Rescan** button on `/admin/network` sets a force flag and nudges;
the sweep then runs `syncGraph` against every friend regardless of the timer.

**Graph only.** Not the catalog, not holdings. The catalog is the expensive one —
its cost scales with a library rather than a friend list — and it already answers
`unchanged` in the common case, so forcing it buys freshness nobody asked for at
the only price worth avoiding. The button exists to refresh *the map*, and the
map is built from graph records.

**What it can and cannot do**, which the UI should say rather than imply.
Convergence is one ring per round: a rescan makes our view as fresh as our
friends' *stores*, not as fresh as the network. A change three hops out still
waits for the nodes in between to run their own rounds. Nothing can shortcut that
without dialling strangers, which is the one thing this design refuses.

Note also what the button is *not* for. A change to **our own** friendships needs
no rescan at all once §Forgetting part 1 lands — our edges come from
`federation_peers`, so removing a friend takes the edge off the map on the next
page load, with no round-trip. The button covers the other case: somebody else's
friendship moved, and we would rather not wait fifteen minutes to hear about it.

**Guarding the serving side: cache, do not refuse.** A friend pulling too often
is the real load, and the button does not create it — `GET /madnetwork/v0/graph`
has always been there. The answer is the one `ownSnapshot` already gives for
catalogs: **memoize the digest** for `Intervals.GraphDigestTTL` and serve the memo
to everyone. An extra pull then costs a mutex and a map read, so the fast caller
gets a cheap yes rather than an error.

A per-peer cooldown answering 429 was considered and rejected. `syncGraph` treats
every non-200 as "an older peer without the endpoint" and returns, so a refusal is
indistinguishable from absence: a mistuned cooldown would stop two honest nodes
converging with nothing anywhere to say why. Refusals are how a network quietly
stops working, and this one would buy nothing a cache does not.

The rest of the surface is already bounded, and stating it is what makes the cache
sufficient rather than optimistic: both endpoints are friends-only, `rateAdmits`
accepts one record per origin per `Intervals.GraphAccept`, `MaxOriginsPerBranch`
caps what one friend may introduce, and the per-record bounds cap a single
document. The attacker set is people we deliberately let in and can block — this
is buggy-friend protection, not anonymous denial of service.

`POST /graph/fetch` is the one surface a cache cannot cover, since every caller
asks for a different set and it is the only one serving bulk payload. If it ever
needs bounding, the tool is the token bucket already carrying `seed_rate_kib`
over the blob write path, not a cooldown.

### The network map (requirements declared 2026-07-31, **built the same day**)

The map is how an admin *sees* the community, and since the community is
unbounded (§The membership rule) the map has to scale by **showing less at a
time**, never by the network being smaller. A node-link diagram is the right
picture and stays — it is what a human reads intuitively — so what it needs is
scale and navigation, not a different metaphor.

- **A view radius, defaulting to 3–4 hops.** Loading the whole component is the
  wrong default once a community is large: the map opens on the neighbourhood
  around us and expands on demand. **This is a rendering setting and nothing
  else** — it never limits who is served, never appears in a scope, never reaches
  the library. Said plainly because a radius that leaks into access is exactly the
  ladder this design threw away: the map's radius is about *what an admin looks
  at*, `share_depth` is about *whom we answer*, and the two must never be the same
  number.
- **Zoom / scale, not truncation.** Pulling back shows more of the graph at less
  detail; pushing in resolves names and marks. A farm renders as a visibly dense
  clump — which is *information*, and the reason for not hiding it behind a cap.
- **Find any node, and any branch.** Search by key, by address, by name (a name is
  hearsay, so results carry the key), and by branch — "everything that arrived
  through this friend" — which is the unit blocking actually operates on.
- **Show all connections between two nodes.** Given two keys, render the paths
  joining them. This answers the question an admin actually has when something
  looks wrong: *how is this node connected to me, and through whom* — the same
  question a block is the answer to.
- **Reachable from the library.** The madnetwork library's ⓘ expansion lists a
  track's holders; each holder links into the map, positioned and selected, with
  the block action to hand. Discovery of a bad actor starts from the content that
  exposed it, not from an admin remembering to go look at a diagram.

**What it is, concretely** (F7 item 7, `federation/mapview.go` — pure functions
over an already-built `NetworkMap`, tested without a mesh):

- `GET /api/admin/federation/graph?radius=N` trims to the view (`TrimMap`;
  default 3, `radius=0` = the whole component) and reports `shown`/`hidden`.
  `radius` keeps meaning the component's true reach, so the map can say there is
  more out there. **Nodes we hold a peer row for are never trimmed away** — a
  pending pairing or a blocked key is a decision of ours, not a rendering
  casualty.
- `GET …/graph/find?q=` searches the **whole** component (`FindNodes`: key,
  mesh address, name; `matched` says which answered, and a name hit is labelled
  hearsay) — a search that could only reach the drawn part would make the radius
  a cost rather than a convenience. `?branch=<key>` (`BranchNodes`) lists
  everything that arrived through one direct friend.
- `GET …/graph/paths?from=&to=` (`Paths`) is breadth-first and bounded by
  `MaxPathResults`/`MaxPathLength`, so a cyclic graph cannot produce
  exponentially many and the truncation drops the *longest* rather than an
  arbitrary set; `from` defaults to this node, and truncation is reported.
- The **zoom resolves names** by counting how many nodes share the *frame*, not
  how many exist. Counting the whole graph — which it did first — means a large
  community can never resolve at all, since no reachable zoom level changes a
  total.
- The library's holder links are `/admin/network#node=<key>`, gated client-side
  on `federation.manage`; the page selects, centres, and expands the radius when
  the node sits outside it.

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
  new friendship, a node pulls a catalog over the mesh
  (`GET /madnetwork/v0/catalog?since=<serial>`, served to our community —
  default-deny toward everyone outside it) and keeps a local copy
  (`federation_catalog`, rooted since F7 item 5 on the *source* it came from, one row
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
  While every carrier was a direct friend the count was trivially
  trust-weighted; since catalogs travel past the friend ring (F7 item 5) the
  ordering counts **branches, not holders** (F7 item 10, §Trust graph "Where
  the weighting applies") — a farm behind one friendship cannot make its claim
  the version everyone's Play button lands on.
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

### Discovery beyond the friend ring (F7 item 5, built 2026-07-31)

Once scope decides who may *fetch*, the thing standing between an admin and "the
whole network's libraries are reachable" is no longer authorization — it is
**knowing a hash exists**. This is the real content of F7, and it has two halves
that ship separately:

- **Serving discovery to members — built 2026-07-31** (item 3). `handleCatalog`
  and `handleHoldings` now answer any member, so our library is discoverable by
  our community.
- **Pulling from beyond the friend ring — built 2026-07-31** (item 5). The sweep
  pulls catalogs and holdings from a *bounded frontier* of the community, not
  from friends alone, and `MadnetworkBlobProviders` reads the same set. Until
  this landed, item 3 was a one-sided opening: symmetric across nodes, useless on
  any single one.

**The endpoint change was one line of policy**, as predicted. `handleCatalog`
already answered *for an audience*, so serving a **member** meant passing the
member audience: the snapshot contains exactly the Madnetwork-scoped entries and
nothing else. An **outsider** — a node we cannot place in our component — keeps
its 403, so opening discovery did not open it to the mesh at large. **403 here,
404 at the byte endpoints** (§Distribution), and the asymmetry is deliberate: a
catalog request names nothing, so refusing it openly leaks nothing, while a blob
or manifest request names a *hash* — answering 403 would confirm we hold it.
Refuse plainly where there is nothing to confirm; stay silent where there is. Two
properties make the member case cheap rather than alarming:

- **All members are one audience class**, so their snapshot is memoized once and
  served to every one of them, and the existing `since=`-serial not-modified reply
  makes a repeat pull a single small round trip carrying no payload.
- **It reveals exactly what an admin marked Madnetwork** — which under the shipped
  default is the whole published library, and that is the intent (§Sharing scope).
  A node whose admin moved the default to Direct friends answers a member with an
  empty catalog instead: also correct, and needing no special case, because both
  answers come out of the same predicate.

Holdings are served to the community too (2026-07-31, §Distribution — the swarm's
only boundary is the madnetwork), and to nobody outside it: advertising a download
cache to strangers would leak what people here listened to, while inside the
community it is what makes a fetched blob a discoverable seeder.

**Whom to ask is already solved — by F6, for free.** The gossiped graph is a
directory of node keys, and a mesh address derives from a key, so every node on the
map is dialable without any new discovery mechanism. Yggdrasil cannot enumerate the
mesh, but we no longer need it to.

**How much to pull was the phase's one open engineering question**, and it is
answered by bounding rather than by cleverness. Pulling every mapped node's
catalog every cycle is the N² dialling pattern that was rejected for graph
records, and caching the entire network's public library is unbounded storage.
What ships is therefore **bounded and demand-shaped** rather than exhaustive
(`federation/discovery.go`, `syncSources`):

- friends are pulled every round they are due, **unbudgeted** — few, and chosen;
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
invisible to it — it is also, by definition, the least-recently-attempted thing
there is, so it is served *first* and the row is created as we try it. Getting
this order wrong is what a live 5-node chain caught: spending the budget on the
members already cached meant the first two nodes reached consumed every later
round as well, and the frontier never moved past them. Two in-process tests had
been green throughout, one of them because it created the very row it was
checking for.

**Where the two halves of visibility divide.** Admission — whom we may cache at
all — is decided once a minute by the sweep's retention walk, because membership
is a graph walk SQL cannot do. Blocking is decided *in the browse query*, because
it is a local act that must take effect the moment an admin clicks it. Retention
keeps a source while it is a direct friend, a member, or a peer we blocked (kept
hidden, so an unblock restores the view with no resync); everything else is
collected, which is how §Forgetting reaches the catalog cache — a branch a block
or a removal cut off stops being a member, and the same walk that un-draws it on
the map drops its cached library.

**The storage decision (owner, 2026-07-31): a table of its own.** Cached catalogs
used to hang off `federation_peers` with a CASCADE, and every browse query joined
`state = 'friend'`. Migration **036** re-roots them on
`federation_catalog_sources` — one row per node we hold a catalog from — and
moves the catalog sync state (serial, synced-at) there with them. The two tables
now answer two different questions: *a peer row exists because an admin decided
something*, *a source row exists because the sweep pulled from it*. Keeping them
apart is what stops a table an admin reads as "decisions" from filling with
hundreds of nodes nobody chose, and it puts the cache's retention rule on the
cache's own table.

A peer row per member was considered and refused. It looks like the cheaper
option and is not: SQLite cannot alter a `CHECK` constraint, so admitting a
`'member'` state means rebuilding `federation_peers` anyway — the same migration
weight, in exchange for merging two meanings that want to stay apart. Blocking
still hides a cached catalog without deleting it, but as a join to the peer table
rather than as a CASCADE.

**Two heard names, and both are read.** A friend's self-claimed name is
refreshed by the friendship ping onto its *peer* row; a member's by the discovery
ping onto its *source* row. The display chain is admin label → either heard name
→ short key, and reading only the source's made friends render as bare key
prefixes while strangers rendered names — backwards, and again something only the
lab showed.

**Freshness for a node we never ping.** The availability window (§Availability)
reads `MAX(source.last_seen, peer.last_seen)` — a friend is pinged every minute
and pulled every fifteen, a member is only ever pulled, so neither clock alone is
the answer and the later one always is. A member's catalog answer *is* its
liveness, including the not-modified reply; the transfer path's `observePeerAlive`
now writes to the source row for the same reason.

*Rejected — relaying catalog entries the way graph records are relayed.* Records
work because a node's friend list is tiny and bounded (512 edges); a catalog is the
whole library, so every node would end up storing every node's public catalog. A
signed **digest** could be relayed cheaply, but the entries would still have to be
fetched from the origin, which is the pull above with an extra layer. If the
frontier rotation proves too slow in practice, digest relay is the upgrade path —
it makes "which node changed" free, and only then does storing more pay off.

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
  audience** (§Sharing scope): the recording's scope and the per-friend user mapping
  filter the catalog and the byte endpoints from the same rule. F5 additionally
  served a guest-accessible recording to any mesh node (the open swarm); F7
  withdraws that and answers a **member** of our component with the
  Madnetwork-scoped set instead.
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
  of two sources: nodes whose **published catalog** advertises the hash as a
  rendition (their library — already synced in F2), and nodes advertising it
  in their **download cache** via `GET /madnetwork/v0/holdings` (a flat hash
  list of what a node will seed, pulled on the same refresh cadence
  as the catalog and cached in `federation_holdings`). The library is already in
  the catalog, so holdings carries only the cache — this is what makes a
  **downloaded blob a discoverable seeder** and lets a popular track spread as
  the community fetches it. Providers are tried most-recently-seen first; no DHT.
- **Only nodes swarm.** Thin clients never talk to peers (see §Principals).
- **The swarm's only boundary is the madnetwork** (declared 2026-07-31). Inside
  the community every node is a peer of every other for distribution purposes:
  same blobs, same manifests, same holdings, same seeding, whether the holder is
  a direct friend or a node five friendships away. Distribution is where "one
  community, one boundary" is most load-bearing, because the whole point of
  content addressing is that *which* holder answers should not matter — a
  swarm that could only draw on direct friends would fail a fetch that the
  community as a whole could trivially satisfy. Direct friendship still buys
  one thing: content an admin restricted to it (`DepthFriends`), which is a
  publishing choice, not a distribution tier.
- **Authorization in the swarm:**
  - Between **direct friends**, the channel identity is sufficient — no tokens
    (F4), filtered by the requester's audience since F5.
  - To a **member of our community** (F7) — §The membership rule — blob, manifest,
    **holdings and cache** service all answer, for **Madnetwork-scoped** content,
    which under the shipped default is the whole published library. No token:
    membership is a lookup, and the scope already says "our community". The
    Madnetwork-scoped *catalog* is served too (§Discovery beyond the friend ring).
  - To an **outsider** — any node we cannot place in our community — nothing, and
    404 rather than 403, so a hash's existence is never confirmed. *Here* only: the
    catalog answers an outsider with a plain 403, because that request names no
    hash and so has nothing to confirm (§Discovery beyond the friend ring).
    (Together these
    replace the token-gated depth ≥ 1 tier *and* F5's guest-playable open swarm; see
    §Sharing scope.)
  - A **listener node** presents a **capability token** — the one surviving use:
    its home server signs "bearer key K is mine until T", and any node that can
    place that *issuer* in its own community verifies the signature *and* that the
    connection really is K (self-certifying channel, so a leaked token is useless
    to anyone else). One issuer, one hop, no delegation chain. It buys the bearer
    the **member** audience — the Madnetwork scope and cache blobs, narrowed by
    the token's `guest_only` bit — and never the issuer's direct-friend reach
    (§Principals & access, "The capability token").
- **Seeding policy** (built F4): everything a node holds — library and
  listen-cache — seeds by default to the whole community ("who cares" is the
  default privacy stance at
  node granularity; the cache reveals only that *someone on this node*
  listened). Controls: `seed_enabled` (master on/off — off refuses all blob and
  manifest service, the node consuming without serving) and `seed_cache`
  (whether the download cache is served **and** advertised in holdings), both
  runtime DB settings on `/admin/settings` defaulting **on** — `seed_cache` is
  the switch for an operator unwilling to re-serve content this node did not
  publish; plus a global
  **upload rate cap** `[federation] seed_rate_kib` (a token bucket over the
  blob-serve write path; `0` = unlimited), a static config knob.

### What a member may cost us (F7 item 6, built 2026-08-01)

`seed_rate_kib` was written when a requester was always a friend, and one bucket
for everyone was the whole of the policy. Item 3 changed who may ask: this node
serves its entire community, and membership deliberately has **no admission cap**
(§The membership rule). So the question is no longer *who gets in* but *what one
of them can cost*, and the answer is four bounds over two resources
(`federation/quota.go`).

**Friends are outside all of it.** A direct friend is an admin's decision and is
served exactly as before, under the global cap alone. Everyone else — members,
guests, a pending peer nobody has accepted — draws on the member budget. That
split is the anti-starvation rule as much as the anti-abuse one: without it the
nodes an admin actually chose queue behind the ones the graph let in.

**Two resources, and concurrency is the sharper one.** Bytes are obvious and
already have a global cap. Concurrent serves are what a swarm client multiplies
*by design* — our own fetcher opens parallel Range requests across holders — so
they are what one member most easily costs us in goroutines, file handles and
netstack connections. Both get the same treatment:

|                        | per requester              | all non-friends together |
| ---------------------- | -------------------------- | ------------------------ |
| bytes/sec              | `per_member_rate_kib`      | `member_rate_kib`        |
| concurrent blob serves | `per_member_max_transfers` | `member_max_transfers`   |

**Why a class ceiling, when §The membership rule promised only per-requester
quotas.** Because a per-identity limit is exactly what a sybil farm defeats: N
forged keys buy N quotas, and the member count was already declared not to be the
defense. The per-requester half is fairness *within* the class — one member cannot
take the whole budget — and the ceiling is the actual bound on harm. The other two
defenses are unchanged and still do the work of *ending* an abuse rather than
merely surviving it: every member is traceable on the map to the friend that
introduced it, and one block cuts the branch.

**Refusal is a 429, and that is a feature.** A requester over quota is told to go
away, and the swarm on the other side does exactly the right thing with that — it
fails over to another holder, under a retirement rule that is relative
(`worseThanPeers`), so a busy node is de-ranked rather than condemned. Being
unable to serve right now is honest information, not an error. The check runs
*before* the blob is looked up, so it confirms nothing about whether we hold the
hash — it is a fact about us. Manifests are deliberately **not** counted: a
manifest is a small memoized JSON, and refusing one would stop a member from even
planning a fetch it is entitled to make.

**All four default to `0` — unlimited — by owner decision (2026-08-01).** The
honest consequence: shipped this way the feature protects nobody who does not edit
`madshare.toml`, and the first time it matters is exactly the first time nobody
has configured it. The case for it is that a real small network — a handful of
friends, a three-node lab — wants none of this, and a default tuned for the
adversarial case is a permanent tax on the common one. Numbers here would have
been guesses of the same quality as `discovery_budget` (§Open questions) with
worse failure modes when wrong. The knobs exist and are documented; choosing them
is an operator's call.

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

1. a **reachable** node holds it (catalog ∪ holdings, `last_seen` within that
   node's window — 180 s for one we ping, three catalog cycles for one we only
   pull from; see "Two clocks, two windows" below), **or**
2. it is in the **local library**, **or**
3. it is **fully cached** (complete file in `<data_dir>/cache/madnetwork/`, no
   `.part`) — *the one arm not built:* the request-time queries have no cheap way
   to ask the filesystem, so it wants a table of complete cache hashes and
   therefore its own migration. Until then a cached-but-unreachable track hides,
   which is wrong in the safe direction (it is still fetchable, just not offered).

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

**Self-health (own inbound path, built).** This is the more important monitor, and
it is what makes "fail open" decidable. The vendored gVisor netstack ran its
entire inbound path in one goroutine, where a single read error killed *all*
inbound mesh traffic permanently (the SPOF logged 2026-07-19 in
`.issues/open-issues.md`) and every friend went silent at once even though the
network was fine. Both halves are now built:

- **The read loop was hardened first**, because a watchdog over a silently dead
  reader only reports the fault it should have prevented. The yggstack fork's
  reader log-and-continues with a 50 ms→1 s backoff instead of `break`ing on one
  read error, and exits only on `Close()`/terminal `ErrClosed` — the
  inbound-reader resilience patch in `third_party/yggstack/MADSHARE-PATCH.md`.
- **The signal is `InboundReaderAlive()`**, exposed by that same patch and read
  through `Node.InboundHealthy()` → `inbound_healthy` on the madnetwork summary.
  It is the only *unambiguous* one available. A self-ping cannot test the inbound
  path (`HandleLocal: true` loops local traffic back without touching it), and the
  originally sketched heuristic — *every friend unreachable for N refresh rounds
  while the yggdrasil core still reports peers up* — was **rejected as ambiguous**:
  it cannot tell a dead local reader from a genuinely absent set of friends, which
  is precisely the distinction fail-open exists to make.

Unhealthy ⇒ cutoff 0 (no filtering at all) plus `inbound_healthy: false` on the
summary, so the UI shows the last-known catalog behind a banner instead of
blanking.

**No transitive real-time presence — how the big network stays honest.** Now that
catalogs travel past the friend ring (F7 item 5) many holders are nodes no
friendship pings — their liveness is whatever their last catalog answer said — and
the answer is deliberately *not* to start pinging strangers or to relay pings along
the chain. Federated systems don't do live presence at all:

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
and reactive backoff *is* Mastodon's dead-instance handling. So the plan is:
**gossip coarse freshness hints** (a friend tells us how recently it saw *its*
friends — a claim, not a probe of ours), rely on **redundancy** (any reachable
holder serves), and **verify on demand only for the working set actually on
screen**
(one mesh RTT to the specific holder, proof not hearsay, cost O(what you are
looking at) not O(network)). The hints ride the **one-minute ping** rather than
the catalog sync they were first sketched on — see the next section for why the
catalog is far too slow to feed a window measured in minutes. A future further
enrichment of `GET /madnetwork/v0/ping` into a small **NodeInfo-style health
card** (version, holdings size, seed policy) gives the network map real per-node
health without any new probing cadence. No chain-relayed ping-forwarding is ever
needed.

### Two clocks, two windows (F7 item 10, built 2026-08-01)

Everything above was written when every source we cached was a **friend**. Item 5
made most of them **members**, and the two are not on the same clock:

- a friend is **pinged every minute**, so a 180 s window is three missed rounds;
- a member is only ever **pulled from** — `discovery_budget` nodes per sweep, each
  due once per catalog cycle — so its `last_seen` advances at best every fifteen
  minutes, and more slowly as the frontier fills.

Judging the second by the first is a category error, and it was measured as one on
2026-08-01: a two-hop member's tracks were visible for about **three minutes in
every fifteen**. Item 5 pulled the community's libraries and the availability
filter then hid nearly all of them again. Visibility only — the bytes stayed
fetchable the whole time, because `MadnetworkBlobProviders` never consulted the
window — but the browse is where the feature lives, so from the page it looked
like the community had no library.

The correction has two layers, wanted for different reasons.

**Layer A — the window measures how recently we would have noticed.** There is
not one freshness window; there is one *per class of observer*, and both carry the
same 3× anti-flap margin over the cadence that feeds them. A node we ping every
minute is judged against `reachable_window_sec` (180 s). A node whose only clock is
the catalog pull is judged against three catalog cycles (45 min,
`federation.PullFreshnessWindow`). `reachClause` picks between them per row rather
than per query, because a single browse mixes both classes:

```sql
AND MAX(s.last_seen, COALESCE(p.last_seen, 0)) >=
    CASE WHEN COALESCE(p.state,'') = 'friend' OR s.hinted_at >= <pingedSince>
         THEN <tightCutoff> ELSE <pullCutoff> END
```

This alone makes the bug go away, and it is the honest reading of what the stored
timestamp means. What it costs is precision in one direction: a member that died
two minutes ago keeps its tracks on the page until its turn in the rotation comes
round. That is the safe direction — a stale offer fails over to another holder or
fails one fetch, while the alternative hid a whole community's library — but it is
still a worse answer than we can give.

**Layer B — a friend's ping carries what it knows.** The refresh loop already
contacts every friend once a minute for exactly this purpose, so the hint rides
that request rather than the catalog: `GET /madnetwork/v0/ping?hints=1`, answered
only to friends, carrying **ages in seconds** for the nodes the responder pings
itself. Ages rather than timestamps, because two nodes need not agree on the clock
and an age composes across a hop without them having to. The caller applies each
hint to the source row it already holds for that key. So a member two hops out —
our friend's friend, which is most of a small community — is refreshed once a
minute by our friend's own first-hand ping, lands back inside the tight window on
its own merit, and is hidden within three minutes of actually going away.

**A node may vouch only for what it touches itself.** A hint covers the
responder's *friends*, never the sources it merely pulls from. This is the whole
of the trust rule and also the whole of the engineering one: a friend's knowledge
of a node it only pulls from is already fifteen minutes stale, so relaying it could
never satisfy a 180 s window in the first place. One hop, first-hand, bounded by
the friend list (`MaxFreshnessHints`, the `MaxGraphEdges` bound) — and beyond that
ring layer A is the answer, not a deeper relay. *Rejected: hints propagated with
accumulated age.* It is a second gossip protocol — propagation, ageing, hop
counting, a store of hints-about-hints — delivering liveness that is still bounded
below by the pull cadence at the first relay that was not a friend.

**A hint is evidence, not a second clock.** It writes `last_seen` like every other
observation (monotonic, so an out-of-order hint cannot age a node), which is what
keeps one column answering "when was this node last known alive" no matter who
observed it. `hinted_at` (migration **038**) records something different and
necessary: *when a fast observer last reported on this source*, which is what
decides the window. Folding the two together would invert the fix — a hinted
member that dies goes on being hinted (its friend keeps relaying a frozen
observation), so the row must stay on the tight window and disappear in three
minutes rather than lingering for another forty-five.

**The class asks who is watching *now*, not who once did.** `hinted_at` is read
against one *ping* window, not the pull window, and the difference is a whole
failure mode. When the **member** dies the hints keep arriving with a frozen
observation, so the row stays tight and is hidden in three minutes — correct. When
the **voucher** dies the hints stop, and within a ping window the row drops back to
the pull clock *our own rotation still refreshes*, so a perfectly healthy member
stays visible — also correct, and the opposite of what a longer horizon would do.
Reading the class off a hint from forty minutes ago would hide a node we can reach
because somebody else stopped talking about it.

**A friend can lie about its friends**, and the network already lives with that:
heard names, gossiped edges and distrust marks are all a friend's word. A false
liveness claim costs one failed fetch that fails over, which is strictly less than
what a false *edge* costs, and hints are accepted only from friends, only about
members, and only for sources we already cache — a hint about a node we hold no
catalog from changes no row and creates none.

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
- **Availability & node health** (built 2026-07-23, not depth-gated; see the
  section of that name). Hardened netstack inbound reader (issue #398) →
  slow/passive per-peer `last_seen` from the existing 1-min refresh + all
  successful mesh traffic → request-time availability predicate (reachable holder
  ∨ local ∨ cached) with a minutes-wide freshness window
  (`[federation] reachable_window_sec`, runtime `madnetwork.hide_unavailable`
  toggle) → self-health via `InboundReaderAlive()` + fail-open banner. Replaces
  the reverted 10 s presence feature. Deferred: the cached-blob exception in the
  predicate, which wants its own migration.
- **F5 — Depth & scope** (built 2026-07-25, see §Sharing scope). Share-depth knob
  (node default + per recording, migration 030), the audience model filtering
  catalog and bytes from one rule, per-friend filtering via the user mapping, and
  the guest-open swarm. Two parts of it were **superseded on 2026-07-30**: the
  per-hop ladder collapsed to three scopes, and the guest-open swarm was withdrawn
  in favour of scope being the network's only authority (§Sharing scope, "Why the
  ladder collapsed"). What survived is the part that mattered — one audience value
  deciding catalog and bytes together.
- **No fingerprint, no publication** (near-term, not depth-gated; see the
  planned item at the end of §Sharing scope). The publishable predicate gains an
  `audio_fingerprints` requirement per rendition, in `visibleTagset` /
  `selfPublishedClause` so catalog and bytes inherit it together, plus the "why
  is this not published" readout in the Recordings lens. Independent of both F6
  and F7, shippable on its own; the startup gate refusing a federated node without
  `fpcalc` (built 2026-07-26) is the other half of the same rule.
- **F6 — Transparency & defense** (built 2026-07-31, see §Trust graph). **Changes
  nothing about who may fetch what** —
  every requester stays at distance 0 throughout, so the wire's access rules are
  exactly F5's. What it adds is sight and reach of *judgement*: an admin can see
  the graph beyond their own friend list, see whom the network distrusts, and cut
  a branch.

  *Built 2026-07-26 — see §Friend-list gossip for the design and its
  consequences.* Signed per-node friend-list records relayed by friends
  (unlimited radius, highest-sequence-wins, digest-then-fetch on the catalog
  cadence, 7-day expiry against a 6-hour heartbeat, migration 031); distrust
  marks published on every block with a reason (migration 032), superseded
  network-wide when the block is lifted; and the **network map** on
  `/admin/network` — a node-link diagram over `GET /api/admin/federation/graph`,
  laid out on rings by hop distance, carrying branch attribution, the address
  beside every hearsay name, branch-weighted mark display, and Block by key for
  the strangers that make up most of it. Branch snipping falls out of the map's
  reachability walk: it never traverses through a blocked node.

  *Naming built 2026-07-30.* Sanitization (§Name sanitization): one
  `sanitizeLabel` behind `CleanPeerName` and `CleanMarkReason`, so a name or a
  mark reason renders as what it is and two nodes cannot render identically. And
  the naming split (§Friendship): `heard_name` beside the local label
  (migration 033), the claim refreshed from the ping reply on the existing 1-minute
  cadence, and `local label ?? heard name ?? short key` everywhere a node is shown.

  *Contradicted-claim reports built 2026-07-30* (§Contradicted identity claims):
  the fingerprint head on the catalog wire, the held-blob and grouping checks on
  the sync cadence (migration 034), the evidence on the peer card and the count on
  the dashboard — the detection that makes the blocking tooling something an admin
  can act on rather than guess with.

  *Underlay de-peering built 2026-07-30* (§Trust graph, blocking): the sweep
  matches the live link list against the blocked set by key and drops the links we
  dialled, so a blocked node also loses us as transit. Inbound links are the
  documented exception (an upstream panic, no handle).

  **F6 is complete.**
- **Forgetting stale graph data** — *built 2026-07-31* (see §Forgetting). Ending a
  friendship was instant where it is enforced and slow in what we remembered. All
  three parts landed, and the third cost nothing as designed: `walkGraph` skips
  every gossiped edge touching our own key, so our edges come from
  `federation_peers` alone; `ReachableKeys` + `DropUnreachableGraph` collect what
  is no longer reachable on the sweep that already runs `ExpireGraph`; and with
  the branch's records gone, `GraphKnowsKey` refuses it on the next round with no
  code of its own. Our own edges are now drawn `Mutual` — they are facts, not
  claims to be weighed. Removal also drops the in-memory pairing note. No
  migration: the graph store is a cache. The admin surface says what an action
  takes with it, on the block and remove confirmations. **Prerequisite for F7
  item 2**, which turns the same walk into an access decision.
- **Refreshing the graph on demand** — *built 2026-07-31* (see §Refreshing the
  graph on demand). A **Rescan** button on `/admin/network` forces `syncGraph`
  past the 15-minute catalog timer — graph only, coalescing, and honest in the UI
  that it buys our friends' freshness rather than the network's. Its counterpart
  on the serving side is a memoized graph digest (`Intervals.GraphDigestTTL`,
  30 s), the `ownSnapshot` pattern rather than a cooldown: a friend that pulls too
  often gets a cheap answer, never a 429 that `syncGraph` would read as a missing
  endpoint.

  One thing only a real mesh showed: the button **silently did nothing**.
  `rateAdmits` refuses a second record per origin inside `Intervals.GraphAccept`,
  which is exactly the set an admin presses Rescan for, while the UI reported
  success. `ResyncGraph` now clears that map first — it bounds what a peer may
  *push* at us unsolicited, and a local permission-gated act is not that. The
  toast reports the change in node count rather than claiming a refresh.
- **F7 — Reach: the community's libraries** (**COMPLETE** — items 1–8 and 10
  built 2026-07-31 / 2026-08-01, item 9 on 2026-08-01). Rescoped 2026-07-30 when
  the depth ladder collapsed, and given its posture 2026-07-31: **everything to
  our community, nothing outside it** (§Goal & vocabulary, "Community"). What made
  this phase risky — a credential
  with a lifetime, a delegation chain and a revocation story, plus an
  authorization decision computed from *gossiped* edges — is gone, because the
  tier it existed to serve is gone. What is left is mostly reuse:
  1. **The four principals as mesh classes** (§Principals & access, §The audience
     model) — *built 2026-07-31*. `Audience.Class` (outsider = zero value = deny,
     guest, member, friend) with positive predicates; both leaking guards
     rewritten — `seedableBlob`'s cache branch is now `aud.ServesCache()` and
     `serveAudience`'s error return denies instead of reading as a full friend —
     and `audienceClause` refuses a non-serving audience in SQL, so the
     fail-closed zero value holds at the storage layer too.
  2. **The membership walk** (§The membership rule) — *built 2026-07-31*.
     `MemberKeys` (`federation/gossip.go`, pure) is the access-side twin of
     `BuildNetworkMap`: same store, same branch snipping, plus the **mutual-edge**
     condition, with our own `federation_peers` friends admitted unconditionally.
     Memoized on the node with a mesh-address index (`federation/membership.go`)
     and recomputed on the sweep from the same peers+edges the retention walk
     reads — two walks, one read, so the store and the perimeter cannot drift.
     `TestMapDrawsAOneSidedEdgeThatMembershipRefuses` pins the one place the two
     walks disagree.

     *Memo ordering fixed 2026-08-01*, found while verifying item 6. Two
     producers write the memo — the sweep, and a request that found it stale —
     and they race by construction, because the sweep reads its peer list at the
     top of a round and only turns it into a member set some real work later. The
     set was stamped with the *write* time and installed unconditionally, so a
     sweep could overwrite a newer answer with an older one **stamped fresh**, and
     an accepted friendship could stop being a membership for a whole TTL. A set
     now ages from when its inputs were read, and `installMembers` refuses a set
     older than the one already there. Its visible symptom was a mesh test
     serving a member as an outsider, where the TTL is a millisecond.
  3. **Serve members, refuse outsiders** — *built 2026-07-31*. Madnetwork-scoped
     blobs, manifests, catalog, **holdings and cache blobs** to members
     (§Distribution — the swarm's only boundary is the madnetwork); an outsider
     gets 404 on bytes and 403 on the two listings. `guest_playable` no longer
     overrides scope on the mesh; serving guests survives as
     `madnetwork.serve_guests`, default off.
  4. **Tighten the vocabulary** — *built 2026-07-31*. `ValidDepth` accepts exactly
     the three constants, migration **035** snaps stored `1…n` to **Direct
     friends** *and* explicit `∞` back to inherit (§Sharing scope, "Nothing about
     the schema changed"), the node default stays `∞` and is now *named*
     Madnetwork, and `share-depth.js` / the access modal / the bulk bar / the
     settings card offer three choices instead of a ladder — with `DepthFriends`
     labelled **Direct friends** everywhere, since "Friends" reads as the
     community and would understate what it restricts.

  Items 1–4 landed as **one change**, as planned: narrowing the values and
  widening the audience are only safe together. On their own they bought a
  one-sided opening — every node served its community and no node could see it —
  which item 5 closed.
  5. **Discovery beyond the friend ring** — *built 2026-07-31*. The bounded
     frontier pull that actually makes other people's libraries visible: friends
     unbudgeted, a few members per cycle rotating on last attempt, foreign
     catalogs capped and coldest-evicted, and a pull-now that jumps both. Cached
     catalogs moved off `federation_peers` onto `federation_catalog_sources`
     (migration **036**), because a node we pull from and a node an admin decided
     about are two different facts. `meshlab reach`, red past distance 1 since it
     was written, is the acceptance test.
  6. **Abuse controls for members** — *built 2026-08-01* (§Distribution, "What a
     member may cost us"). `seed_rate_kib` was one bucket for everyone, written
     when a requester was always a friend. Now bytes and concurrent serves are
     each bounded twice, per requester and across all non-friends together, with
     direct friends outside both — which is the anti-starvation rule as much as
     the anti-abuse one. The class ceiling is more than §The membership rule
     promised, and it is what actually answers a sybil farm: a per-identity limit
     is precisely what N forged keys defeat. Refusal is a 429, which the swarm
     reads as "ask another holder" rather than as a fault. All four knobs default
     to unlimited by owner decision — the feature is opt-in, and the doc says
     plainly what that costs.
  7. **Map at scale** (§The network map) — **BUILT 2026-07-31.** The 3–4-hop
     default view, zoom that resolves names instead of cropping, node/address/name
     and branch search over the whole component, all-paths-between-two-nodes, and
     the holder → map jump from the library's ⓘ expansion. Not access work at all,
     but it is what makes the revocation half of the membership model usable, so
     it shipped with the phase rather than after it.
  8. **A madnetwork page that can hold a community's library** — **BUILT
     2026-07-31.**
     (`docs/ui/madnetwork-page.md` §Discovery). `/madnetwork` was an A→Z
     drill-down, which was right for a few friends' catalogs and the wrong shape
     for everything the community publishes: on your own library you browse
     because you remember it, on the network you have nothing to remember. It now
     lands on discovery lanes over the merged catalog with search above them, the
     alphabet demoted to *Browse all* and finally windowed (keyset-paged +
     `virtual-list.js`). Built here because F7 is what made it urgent — serving
     members without it meant opening the network's libraries into a surface
     nobody could find anything in.

     Six lanes, eight rows each, a per-source cap on the two a single node's
     volume could otherwise own, `?source=` for one node's shelf, and migration
     **037** (`federation_catalog.first_seen`, carried across the atomic replace —
     otherwise every sync re-dates a source's whole library). Its *Most held* lane
     is the first place branch weighting reaches the browse, so it lands part of
     item 10 early — deliberately, because a popularity lane a sybil farm can lift
     is worse than none. Item 10 finished the job the same week: gossiped
     freshness hints, then the same weighting on *Not in your library* and on
     version ordering.
  9. **Listener-node tokens** (§Principals & access, "The capability token"):
     a home server signs "this bearer is mine until T", verified against the
     self-certifying channel by any node that can place the *issuer* in its own
     community. One issuer, one hop, no chain — the only surviving use of a
     token. **BUILT 2026-08-01** (`federation/token.go`; no migration — a token
     verifies from its own bytes, so issuing one creates no state to store,
     expire or replicate).

     Three decisions settled it, and two of them corrected text that predated
     item 3. The issuer is honoured if we can place it in our **community**, not
     merely in our friend list — the older wording was written when direct
     friendship was the access boundary, and keeping it would have made a
     madplayer reach strictly less than the server vouching for it. The bearer
     gets **membership, never friendship**: a recording restricted to hand-picked
     nodes stays off a device this admin never picked, and the fact that its home
     server could fetch and relay those bytes anyway is a statement about *its*
     behaviour, which §"Why the ladder collapsed" is precisely about not
     pretending to control. And the **lifetime is one hour**, renewed at
     half-life, which stopped being a hard question once it was clear the expiry
     is not the main revocation mechanism: the issuer's standing is re-checked on
     every request, so blocking a home server takes its bearers with it
     instantly, and the hour only has to cover a node revoking *its own* user.
  10. **Trust-weighted popularity** (one branch = one voice, §Trust graph), which
     only becomes meaningful once carriers are not all direct friends. **BUILT
     2026-08-01** (§Trust graph, "Where the weighting applies"). No migration:
     the attribution is computed from the gossiped graph at request time, which
     is also what keeps a block instant — snip the branch and its voices are
     gone from the next ranking, with nothing cached to invalidate.

     The half that mattered was not a lane at all: a track row's **versions**
     were ordered by raw holder count, and `renditions[0]` of the leading one is
     what Play, Queue and Materialize act on — so the mislabel defense of
     §Trust graph point 2 was missing from the one control people press. Ordered
     by voices now, with *Not in your library* joining *Most held* as the second
     weighted lane and the other three deliberately left alone (each for a stated
     reason, in `laneWeighted`). The counting rule became one function so no
     surface can apply half of it.

     Its other half — **freshness for holders we never ping** — is **built
     2026-08-01** (§Availability, "Two clocks, two windows", migration **038**).
     It turned out to be a bug report rather than an enhancement: item 5 made
     most sources members, and members were still judged by a window sized for
     the one-minute friendship ping, so a two-hop member's tracks showed for
     about three minutes in every fifteen. Two layers answer it — the window is
     now chosen per row by the cadence of whatever observes that node, and a
     friend's ping reply carries first-hand ages for the nodes *it* pings, so a
     friend-of-a-friend is watched once a minute and earns the tight window
     rather than being granted the wide one. Never transitive pinging, and never
     a relayed hint: one hop, first-hand, or nothing.

     What neither half claims to answer is **volume from a single honest
     branch** — one friend with fifty thousand badly tagged albums is one voice
     and fifty thousand rows. That is clustering, not weighting, and it stays
     open in `docs/ui/madnetwork-page.md` §Open.
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

1. **How much of the frontier to pull and cache** (F7, §Discovery beyond the friend
   ring). The shape shipped with item 5; the *numbers* are still guesses. Four
   member catalogs per 15-minute cycle and a cap of 200 foreign catalogs are what
   a node runs with today, tunable per node as `[federation] discovery_budget` /
   `discovery_cap`. Nobody can pick them from first principles — they want a real
   network to observe, and the thing to watch is whether the rotation fills the
   frontier faster than the network changes. If it does not, the recorded upgrade
   path is signed catalog-digest relay, which makes "which node changed" free.

(Former question 1 — catalog crossing / one tagset text on two recordings —
was settled with F2: see §Catalog, ""N versions"". Former question 2 — chunk
size & Merkle parameters — was settled with F4: adaptive, self-describing chunk
size in the manifest; whole-file hash is the anchor, per-chunk hashes are for
early verification only. See §Distribution. Former question 3 — gossip payload,
propagation and ageing — was settled 2026-07-26 ahead of F6: see §Friend-list
gossip, including the privacy half. Former question 4 — listener-node token
lifetime and renewal cadence — was settled 2026-08-01 with F7 item 9: one hour,
renewed at half-life, because community standing already revokes a whole issuer
instantly and the expiry only has to cover a home server revoking its own user.
See §Principals & access, "The capability token".)

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
