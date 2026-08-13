# Madnetwork access — principals, scope and audience

> **Who gets served what.** Part of the federation design; the spine, the
> vocabulary and the build plan are in [`federation.md`](federation.md), the graph
> these rules read is in [`federation-trust.md`](federation-trust.md), and the
> byte endpoints they gate are in [`federation-swarm.md`](federation-swarm.md).
>
> **Status: built** (F5, and F7 items 1–4, 6 and 9). The posture in one
> sentence: **everything to our community, nothing outside it.**

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
guest**, not a lesser peer. "Madnetwork" therefore means *our* connected
component of the friendship graph, never "everyone on Yggdrasil" — the mesh is
a public network, and being routable on it establishes nothing. This is what
makes the Madnetwork scope a statement about a bounded set of nodes that
somebody we can trace vouched for.

**The two mesh rows differ less than they look, and deliberately so**
(2026-07-31). Since the node default is Madnetwork, a member reaches the same
library a direct friend does; what a direct friend additionally gets is whatever
*this* admin chose to restrict to hand-picked nodes. Membership is the
perimeter, and direct friendship is the exception inside it — not the other
way round.

Three consequences, all of them wanted:

- **Blocking gains teeth over access, not only over sight.** The F6 reachability
  walk never traverses a blocked node, so snipping a branch removes that
  branch's *membership* — and with it its reach. Defense tooling and the
  access rule become one mechanism.
- **Discovery narrows with it.** Only members may pull the Madnetwork-scoped
  catalog; an outsider is told nothing at all (`federation.md` §Discovery
  beyond the friend ring).
- **Membership rests on signed hearsay, and that is acceptable here for a
  precise reason.** X is in our community because our friend's own signed record
  names it, or a chain of such records does — the graph informing an
  authorization decision, which this design refuses to do for *hop counting*.
  The difference is direction: membership can only **narrow** access relative to
  "any mesh node", and it never grants more than the scope an admin already
  marked open to the community. A forged or over-generous edge therefore yields
  content already shared with the community, never content restricted to direct
  friends. Distance-based reach had the opposite property, which is why it is
  gone (§Sharing scope).

### The membership rule (declared 2026-07-31, built 2026-07-31)

Once the node default is Madnetwork, **membership is the only thing between a
stranger and the whole library**, so the rule deciding it stops being a
convenience and becomes the perimeter. Three decisions define it.

**1. A member is a key we can reach through *mutually declared* friendships.**
Built as `MemberKeys` in `federation/gossip.go`, memoized per node in
`federation/membership.go`. `BuildNetworkMap` currently links two nodes from
**one** side's claim (`link(e.Origin, e.Peer)` *and* `link(e.Peer, e.Origin)`,
`federation/gossip.go`), so a single signed record naming 512 invented keys
would make all 512 members at the next sync — no agreement, no record of their
own, nothing to block but the friend who relayed it. For **drawing the map**
that is right: an edge somebody claims is worth seeing. For **deciding access**
it is not, so the membership walk requires the edge to be claimed from **both**
ends. The direction data already exists (`claimed[[2]string{…}]`); only the
access walk reads it.

- **Our own direct friends are members unconditionally.** That edge is a local
  fact from `federation_peers`, not hearsay, so a friend who publishes no friend
  list is still a full friend of ours.
- **Further out, a silent node cannot be a member**, because nothing it signs
  names anyone. This is the already-documented "a silent node makes itself a
  dead end" property (`federation-trust.md` §Friend-list gossip) arriving at
  its logical end, and it is the strongest reason `publish_friend_list` defaults
  to on.
- The practical effect is that every member has *declared itself* part of the
  community — one signed record per member — which is what makes the
  per-branch record bound (`federation-trust.md` §Friend-list gossip,
  "Anti-flood bounds") count members rather than names.

**2. The community has no size limit, and the map is not how you cope with a big
one.** No cap on members, no radius, no admission quota — a community that
grows to thousands is the project succeeding, not a threat.
`MaxOriginsPerBranch` (5000) stays exactly what it was written as: a bound on
what one friend can cost **our store**, never a statement about who belongs. If
a real network ever approaches it, it is a number to **raise**, not a boundary
to enforce. The answer to "the map is unreadable" is a better map
(`federation-trust.md` §The network map), not a smaller network.

**3. Abuse is bounded by cost and by revocation, not by admission.** A branch
can mint members up to whatever `MaxOriginsPerBranch` happens to be — and
since that number exists to protect our store and rises as real networks grow,
**the member count is not the defense and must never be relied on as one**. That
is accepted with open eyes. What is bounded instead:

- **per-requester quotas** — bytes and concurrent transfers per member per
  round, so a forged member is bounded *harm* rather than merely rare (F7, build
  plan);
- **branch attribution** — every member is traceable on the map to the friend
  of ours it arrived through, so "who let this in" is one glance;
- **one block clears the branch** — the reachability walk never traverses a
  blocked node, so snipping the introducing friendship removes every member
  behind it at once.

So the perimeter is **accountable rather than cryptographic**: we cannot prove a
member deserves to be here, but we can always say who vouched for it and undo
that in one action. For a network built out of person-to-person friendships that
is the honest target; anything stronger means a membership PKI, which this
design has refused from the start.

**The memo is ordered by when its inputs were read** (fixed 2026-08-01, found
while verifying the member quotas). Two producers write the memoized member set
— the sweep, and a request that found it stale — and they race by
construction, because the sweep reads its peer list at the top of a round and
only turns it into a member set some real work later. The set used to be stamped
with the *write* time and installed unconditionally, so a sweep could overwrite
a newer answer with an older one **stamped fresh**, and a friendship accepted
mid-round could stop being a membership for a whole TTL. A set now ages from
when its inputs were read, and `installMembers` refuses one older than the set
already there. The visible symptom was a mesh test serving a member as an
outsider, where the TTL is a millisecond.

The rest of the access model is what the four principals are made of, and the
declaration above changed none of it:

- **Node-level trust is the default relationship.** A friend node is trusted as
  a unit; its internal user model is its own business.
- **The node-key → local-user mapping is being removed** (decided 2026-07-26;
  built and still present — `federation_peers.user_id`, `PeerAudience`, the
  user-mapping control on `/admin/network`). It let an admin bind a friend
  node's key to a local account so that node was answered with that account's
  rights. It came from misreading "authorize the node as a user": the
  requirement was never *a node acting as an account*, it is the **listener
  node** below — a person who signs in with credentials, from a device that
  happens to also be a mesh node. Two consequences to handle when it goes: the
  `GuestOnly` half of the audience is derived from it today (see the open detail
  under §Sharing scope), and the removal needs a migration.
- **Unmapped friends are not a special case** (decided 2026-07-18): a friend
  node *without* a user mapping is treated as a **default regular-user
  identity** — it may see and fetch whatever a plain `user`-role local account
  may. The mapping is the per-friend *override* (more or less than the default),
  not a prerequisite. Deliberately a rule, not a magic local account row —
  nothing to log into, rename, or accidentally delete. Since F5 this is enforced
  as the `GuestOnly` half of the audience (§Sharing scope): unmapped and
  mapped-with-`content.access` friends see the full published set, a friend
  mapped to an account without it sees only guest-accessible recordings — in
  the catalog and at the byte endpoints alike.
- **Thin clients have no madnetwork access by default.** Madnetwork browsing is
  a new permission (working name `madnetwork.access`), granted to admin by
  default and grantable to trusted local users. The header section for the
  madnetwork library is server-side gated like every other link.
- **Planned — split `madnetwork.access` in two** (raised 2026-07-26). One
  permission gates two things whose costs are nothing alike: *looking* at the
  merged catalog, which reads rows that were synced anyway, and *making this
  node fetch and cache remote bytes for you*, which spends its disk (the
  madnetwork cache has no eviction) and its bandwidth. The permission was
  created for the second and is being spent on the first. Listener nodes sharpen
  the mismatch: one browses through the server but fetches for itself, so it
  wants the cheap half and never the expensive one. Proposed shape — keep
  `madnetwork.access` meaning **browse** (no rename, no migration, no role
  churn: 027 already grants it to admin and the stackable `madnetwork` role) and
  add **`madnetwork.relay`** for the stream/materialize path; grant browse
  widely, relay narrowly. A per-user cache quota is the natural companion and
  the honest answer to overuse — the permission is a blunt instrument standing
  in for one.
- When a thin client with the permission plays a non-local file, **the server
  fetches it into a cache directory and relays it** — as *cache-through
  streaming*: chunks are fetched in sequential priority and served to the
  browser as they arrive, while the complete file lands in the cache in
  parallel. Never build the blocking download-fully-then-play version.

### Listener nodes — madplayer (the madshare half is built)

A madplayer is a person's own device: a player that also runs a federation node.
It joins the network **as a person rather than as a friend**, which makes it a
third kind of participant beside the full peer and the thin client (decided
2026-07-26; supersedes the node-key → local-user mapping above).

**Where this stands.** The madshare half is built: capability tokens (§"The
capability token"), the audience a device serves and the tracker that makes it
findable (§"The household"), peering info, and the embedder-facing `Network()`
surface. The client half lives in its own repository and is specified in
`docs/ui/madplayer.md`. What follows is the shape both halves were built to.

- **Credentials, not friendship.** It signs in to a home server with an ordinary
  account — session or API token, the same auth a browser uses. No node card,
  no admin accept, no `federation_peers` row. Its rights are that account's
  rights, so federation still adds no parallel permission system.
- **The content flow is one-way, by construction.** It consumes — browse,
  stream, materialize, bounded by the account's ACLs. It publishes **nothing**:
  its local library is never catalogued, advertised or pulled. That library is
  unmoderated personal content on somebody's phone, and the network has no basis
  to vouch for it. This is a property of where the content lives, not a setting
  to relax later.
- **The one way in is an upload.** A user holding `file.upload` uploads from the
  device to the home server, through the review bucket like any other upload.
  What the network then sees is the *server's* published content under the
  server's identity — reviewed, fingerprinted, attributable. The device is
  never the publisher.
- **It is a full swarm member regardless.** Its own key, on the mesh, fetching
  chunks from many holders and seeding back what it fetched. Safe for exactly
  the reason cache blobs are exempt from the fingerprint rule: serving a hash
  claims *possession of bytes*, never an identity, so a seeder asserts nothing
  anyone has to trust. One-way publication and two-way swarming are not in
  tension — the swarm carries bytes, the catalog carries claims, and only the
  second needs vouching for.

  An earlier revision of this bullet ended "…discovered like any other node",
  and that half was wrong: a node discovered like any other is one a graph walk
  reaches, which is precisely what a listener node is not. Who can reach it, and
  how anyone learns it holds anything, are answered in §"The household".
- **Token-carrying, not relay-only** (decided 2026-07-26). Fetching everything
  through the home server would have been the cheaper first version and was
  rejected: no madplayer existed yet, so there was nothing to be compatible with
  and it could be built properly the first time. This makes **F7 capability
  tokens a prerequisite** — to its home server's friends a madplayer is a
  stranger, and a stranger it cannot place in the component is served nothing at
  all. The token is how a home server says "this bearer is mine", and since the
  scope collapse it is the *only* remaining job for a token: not a hop count,
  but one node vouching for one bearer it authenticated itself.
- **Thin clients stay out of the swarm** (decided 2026-07-26). A browser user
  remains a pure consumer relayed by its home node. Browser tabs have no durable
  storage, no stable address and no lifetime; enrolling them would complicate
  the swarm and buy nothing.
- **Future — the home node as introducer.** Both ends are on yggdrasil, so a
  server could broker a direct connection to its own listener users instead of
  carrying their traffic. Recorded as madplayer's direction; not part of this
  plan.

Client-side behaviour — playlist sync, and what the app does with items the
server cannot resolve — is in `docs/ui/madplayer.md`.

### The capability token (F7 item 9, built 2026-08-01)

The token is one signed sentence: **"bearer key K is my user until T"**, issued
by a home server over its own ed25519 node identity — the same key its mesh
address derives from and the same key it signs gossip records with. Four fields,
no delegation chain, no PKI: issuer key, bearer key, expiry, and a `guest_only`
bit.

**What a verifier checks**, in `serveAudience`, which is the single gate all
four mesh endpoints already resolve their audience through:

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
next to the server that vouches for it — reaching that server's four
hand-picked friends while the server itself reaches its whole component. It is
still **one issuer, one hop, no chain**: we place the *issuer* ourselves, from
our own graph, and then accept exactly one claim from it about exactly one
bearer. Nothing about the bearer is ever taken from hearsay, and the token is
never re-presented onward.

**What it buys: membership, not friendship** (decided 2026-08-01). A valid token
yields `MemberAudience` — Madnetwork-scoped content, cache blobs included —
narrowed by the token's `guest_only` bit, which is how the home server's own
account ACL travels with the bearer. It does **not** yield the issuer's
`DepthFriends` reach. A recording marked *Direct friends* was restricted to
nodes this admin picked by hand, and a device somebody else enrolled is not one
of them, however much we trust its home server. The counter-argument was
considered and rejected on the doc's own terms: yes, the home server can fetch
those bytes and relay them to its user anyway, but that is a statement about
*its* behaviour, and the whole point of §"Why the ladder collapsed" is that we
decide our own. The token grants precisely what the component could not: a way
to place a node that publishes no friend list and appears in nobody else's.

Note the direction of the `guest_only` bit — it can only ever **narrow**. A
token that says nothing is served as a plain member, so a forged or truncated
bit cannot buy more than membership, and membership is what the bearer's issuer
already has.

**Lifetime: one hour, renewed at half-life** (decided 2026-08-01, settling the
last open question). The lifetime looks like the revocation story and mostly is
not, which is why this was stuck: **blocking a home server revokes every token
it ever issued, instantly and without a lifetime being involved at all** —
step 4 is re-evaluated on every single request, so a snipped branch takes its
bearers with it on the next one. What the expiry actually covers is much
narrower and belongs to one node: a home server revoking one of *its own* users
— an account disabled, `madnetwork.access` withdrawn, a phone left in a taxi.
That is a one-hour window on one relationship, and renewal is free because a
madplayer is by definition already signed in to its home server and can ask for
a fresh token whenever it likes. Renewing at half-life rather than at expiry
keeps a transient outage from becoming a service interruption.

The cliff is real and accepted: a device that cannot reach its home server for
an hour stops being served by the network, even while the mesh around it is
healthy. That is the correct failure. The token is a *vouch*, the voucher is
unreachable, and a credential that outlives contact with its issuer is exactly
the thing the short lifetime exists to prevent. A madplayer in that state still
plays what it already holds — its own library is local, and one-way
publication means nothing about it depended on the network in the first place.

**No revocation list, deliberately.** Lifetime is the revocation mechanism for
the narrow case, community standing for the broad one, and between them there is
no gap worth a distributed data structure that every node would have to fetch,
trust, age out and disagree about.

**Where it lives.** `federation/token.go` — the token type, signing, the four
verifier checks, and `tokenAudience`, which `serveAudience` consults after the
friend and member arms and before the guest fallback (a friend or member already
has everything a token buys, so presenting one must never cost them their own
standing). Issuance is `POST /api/madnetwork/token` (`api/madnetwork_token.go`,
gated `madnetwork.access`): an ordinary authenticated call, since the caller is
a person with an account rather than a node with a card. The wire form is
base64url'd signed JSON in a `Madnetwork-Token` header. A bearer is **not** a
friend for quota purposes — it draws on the member budget like every other
non-friend (`federation-swarm.md` §"What a member may cost us"), which is the
answer to a home server enrolling a thousand devices: they share one class
ceiling.

### The household — a listener node's own audience (level 2b, designed 2026-08-09)

The token above is the *outbound* half: it is how a listener node is served by
strangers. Building madplayer's level 2b (`docs/ui/madplayer.md`) established
that the inbound half — "seeding back what it fetched" — had no mechanism
behind it at all, in three separate places:

1. **Nothing ever presents a token.** `TokenHeader` was only ever read.
   `tokenAudience` verifies inbound tokens and `IssueCapabilityToken` signs
   them, but every outbound mesh request in this package sets no header. The
   credential was built from both ends and had no middle.
2. **A listener node cannot find a holder.** `EnsureBlob` sources its providers
   from `MadnetworkBlobProviders`, i.e. from *this* node's cached catalogs and
   holdings. Those are populated by `syncSources`, which pulls from friends and
   members. A node with neither has permanently empty tables and every fetch
   ends in `ErrNoHolder`.
3. **A listener node can place nobody.** `serveAudience` walks friend → member
   → token → guest, and on a peerless node all four fail: no peer rows, a
   walk over an empty peer set, `tokenAudience` needing an issuer *we* can
   place, and `serve_guests` off by default. So it answers every requester as an
   outsider — and nothing would ask it anyway, since it appears in no catalog
   and no `federation_holdings`.

The first two are missing code. The third is a missing *idea*, and it is the one
this section supplies. A listener node needs an audience of its own, and the
rule that gives it one is one sentence:

> **A listener node serves exactly what its home server vouches for: that server,
> and any bearer of a token that server issued. Nothing else, in either
> direction.**

That is the token's own trust relation read backwards. A device already accepts
"this bearer is mine" from its home server in order to be served by third
parties; the same statement, from the same signer, is what it now accepts in
order to serve them. Nothing new is trusted and no new credential exists —
what changes is which node is doing the believing.

The population that can reach a device is therefore exactly the population its
home server has authenticated: itself, and its own users' other devices. Two
madplayers signed in to the same server can hand each other chunks; a member of
that server's wider community cannot. That boundary is not a limitation to relax
later, it is the definition — the device has one relationship on the mesh and
it is one-sided.

**The home-node record.** The device stores its home server as
`federation_home_nodes(public_key, base_url, name, added_at)`. It learns the key
from the `issuer` field of the token it was already asking for, so the record
costs no new exchange and no new round trip.

Deliberately **not** a `federation_peers` row, and this is the load-bearing
part. A peer row publishes a gossip edge, an edge is how a node appears in
everybody else's map, and the entire listener design rests on the device
appearing in nobody's friend list — that is what makes it unplaceable, which
is what the token exists to work around. Friending the home server instead would
make almost all of this fall out of machinery that already exists, at the price
of turning the device into an ordinary community member: an admin accept per
phone, its address gossiped to the whole component, and its availability
counting against a network that `federation.md` §"Topology asymmetry" already
says should not lean on phones. That trade was considered and refused
(2026-08-09).

Nothing else in the codebase reads that table. It is a one-sided trust record,
it is not a relationship, and it is invisible to the graph.

**Two placements fall out of it**, both inside `memberSet`, which is already the
single object every access decision consults:

| What | Becomes true |
|---|---|
| `lookup(addr)` also matches a home node's derived address | the home server itself is placed as a member, so it can pull from the device directly |
| `vouches(key)` also accepts a home node | tokens that server issued are honoured, so its other devices are placed |

Both are additions to an existing set, so the memoisation, the TTL and the
address-index derivation are unchanged.

**It seeds the cache, never the library**, and that holds for two independent
reasons rather than one. `seedableBlob` already splits the two halves: the
library half is gated by `BlobVisibleTo` (the sharing scope) and the cache half
by `policy.Cache && aud.ServesCache()`. A player sets
`madnetwork.default_share_depth = Local`, so every recording it holds resolves
to `DepthPrivate` and the library half refuses everyone — including a home
server it otherwise trusts completely. Belt and braces on the one rule that must
not fail: even a correctly-placed member gets bytes only out of the cache, which
is content the network gave it in the first place.

**Being found** (built 2026-08-09, migration 045). A device pushes its cache
hash list to its home server (`POST /api/madnetwork/holdings`, an ordinary
authenticated call carrying the device's node key), and the server answers hash
queries with those devices alongside its catalog holders. **The home server is
the tracker for its own devices, and only for them.**

Those holdings are *not* re-published: they never enter the server's mesh
catalog and never appear in its own `GET /madnetwork/v0/holdings`. A server that
advertised a device to the wider community would be promising something it
cannot make good — the device serves only what this server vouches for, so a
member following that advertisement gets a 404 and learns nothing except that
the advertiser is unreliable. **An advertisement whose promise the advertiser
cannot keep is worse than no advertisement**, because the swarm's failover
treats a holder that refuses as a holder that is broken (`federation-swarm.md`
§"What a member may cost us" is the same reasoning from the refusing end). That
separation is structural rather than remembered: `handleHoldings` answers from
the node's own cache *directory* and has no route into this table.

Three shapes decided while building it:

- **A push is a complete statement, not a delta**, replacing the device's whole
  set in one transaction — a delta needs both ends to agree about a history
  neither keeps. So an **empty list is meaningful**: it is a swept cache, and it
  must stop the device being offered.
- **Retention is the freshness window and nothing else.** `ListenerHoldingsTTL`
  is three token renewals, derived the way the availability windows are — a
  window follows its observer's cadence — and tied to `TokenRenewAfter` at
  compile time so the two cannot drift. A device that goes away stops pushing
  and stops being advertised; there is no heartbeat endpoint, no sweep, and
  nothing to delete. The reply carries `refresh_after` so a client is told the
  cadence rather than guessing it.
- **The advertisement follows the account**, by `ON DELETE CASCADE`. It exists
  because this server authenticated somebody, so deleting that account withdraws
  it rather than leaving rows pointing at a device nobody can vouch for.

The fetch plan is `GET /api/madnetwork/holders/{hash}` → `{size, holders[]}`,
which is catalog holders ∪ this server's devices. An empty list is a **200,
not a 404**: nobody holding a blob is a normal answer, because the caller's
fallback is the relay this server has always run on its behalf, and a 404 would
read as "no such content" and send a client hunting a bug.

**The fetch plan comes from the home server, not from the device's own tables.**
`EnsureBlobFrom(ctx, hash, size, holders)` takes an explicit provider list and
shares `EnsureBlob`'s dedupe map, its cache directory and its whole swarm path
— the store lookup stays exactly where it is for a server, which discovers its
own holders and must keep doing so. The device gets the list from `GET
/api/madnetwork/holders/{hash}` (catalog holders ∪ this server's listener
devices), and the browse rows it is already rendering carry `holders[].key` per
version, so the common case needs no extra call at all.

The alternative — having the device sync catalogs over the mesh so
`EnsureBlob` works untouched — needs it to be a friend or a member of
somebody, which is the one thing it is not.

**Presenting the token** is the small missing middle: outbound mesh requests set
`Madnetwork-Token` when the node holds one. It is attached to every request
rather than only to the ones expected to need it, which costs nothing —
`serveAudience` resolves a friend or a member before it ever looks at the
header, so a token presented to a node that already knows us is ignored, and a
node that does not know us is exactly the case it was built for.

**Getting onto the mesh at all** needed answering, because yggdrasil peers are
not discovered: `federation/mesh.go` builds its core with `ListenAddress` and
`Peer` and nothing else, so a fresh device with no configured peers sits on an
island. Three paths, kept together because they fail in different situations
(decided 2026-08-09):

- **The home server publishes peering info** — `GET /api/madnetwork/peering`
  (gate `madnetwork.access`) returns its own `[yggdrasil] listen` URIs plus
  `shared_peers`. Signing in is then enough to get onto the mesh, which is the
  only path that asks nothing of a person who does not know what an underlay is.
- **A typed peer list**, exactly as a server operator writes `[yggdrasil]
  peers`. The fallback that always works, and useless on its own to the person
  this client is for.
- **Multicast** — yggdrasil-go ships the module and madshare had simply never
  wired it (only `third_party/yggstack`'s own `cmd` did). A phone on the same
  wifi as its home server then finds it with no configuration and no internet.
  Config key `[yggdrasil] multicast`, **default false** — a divergence from
  upstream, because a server has configured peers and an operator who did not
  ask for LAN auto-peering should not get it, while a player turns it on since
  zero-configuration on a home network is the whole point. Never fatal: a host
  with no multicast route logs and carries on, since discovery is an alternative
  to configured peers rather than a replacement for them.

Four decisions inside the endpoint (built 2026-08-09):

- **Sharing is on by default** (`[yggdrasil] share_peers`, default true). A peer
  URI is not a secret — it is an address whose entire purpose is to be
  dialled, the account holder could read it off this node's config if they ran
  it, and yggdrasil authenticates every peering by key regardless of how it was
  found. The opposite default would leave every device owner pasting underlay
  URIs by hand, which is the problem this solves.
- **`shared_peers` defaults to `peers`** — a node shares the way it connects,
  which is right without anybody deciding anything. An explicit list replaces
  them (for a node whose own uplink is on a private link nobody else can reach),
  and an **explicit empty list shares none while leaving the endpoint on**. That
  middle state is why it is a list rather than a flag, and why the default is
  resolved from `nil` rather than from `len() == 0`.
- **A wildcard bind is rewritten per request.** `listen =
  ["tls://0.0.0.0:12345"]` is a correct bind and a useless address; the host the
  caller just reached us on is by construction a host that reaches us, so it is
  substituted in, keeping the listener's own port. Better than any answer this
  node could derive by enumerating its interfaces, because only the caller knows
  which of them it can see. A `unix://` listener is dropped — a real listener,
  and not an address anybody else can dial.
- **Off and empty are different answers.** Sharing disabled is a **404** ("this
  operator switched it off, stop asking"); sharing enabled with nothing to give
  is a **200 with empty lists**, which is the honest answer for a node that was
  itself only ever reached over the mesh.

**fpcalc stays required** on a player, exactly as on a server (decided
2026-08-09). The startup gate is not relaxed and the player does not set
`allow_missing_fingerprinting`. The consequence is stated rather than hidden: a
device that seeds is re-distributing audio, and an install without fpcalc cannot
join what it fetched to what it already holds. The cost lands on **Android**,
where fpcalc is not a package one installs — the mesh does not come up there
until a Chromaprint build ships with the app, and until then that platform is
level 1.

**What this buys over the relay**, which is worth stating because level 1
already plays network content through `GET /api/madnetwork/stream/{hash}` and
needs none of the above. Three things, in the order they matter: bytes keep
flowing while the home server is merely *unreachable* rather than gone, since
the holders are elsewhere; a fetch parallelises across holders instead of
queueing behind one relay hop that is itself fetching; and the device stops
being a pure consumer, which is the difference between a client and a node and
the reason this level exists at all.

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
community out of the box. This is the point of the project rather than a liberty
taken with it: a friends network whose default is "share with nobody but the
four people you typed in" is a file server with extra steps, and every node that
ships closed makes the network worth less to everyone already in it.

**The posture that comes with it, stated once and plainly.** Installing a
federated node publishes your library to people you will never meet or approve
— your friends' friends' friends, outward with no radius. That is the deal, it
is the deal by design, and the two things that make it defensible are elsewhere
in this document rather than in a caveat here: nothing at all leaves the
community (§Principals & access), and any branch of it can be cut in one action
(§The membership rule). An admin who does not want that has **Direct friends**
per recording or as the node default, and **Local** for content that must not
travel — both one control away on `/admin/settings`.

**Built 2026-07-31** (F7 items 1–4, one change — narrowing the values and
widening the audience are safe only together). `ValidDepth` now accepts exactly
the three constants, migration 035 snapped the stored in-between values, and
members are served. The node default value did not change: it was already `∞`;
what changed is that `∞` now has an audience.

### Why the ladder collapsed (decided 2026-07-30)

The per-hop ladder (`1` = friends of friends, `2` = one hop further) is
**gone**. It promised something the protocol cannot deliver, and saying so is
more useful than keeping the knob:

- **We only control hop 0.** Every scope value is a statement about *our own*
  behaviour — whom we answer. "Three hops" is a statement about *other
  people's* behaviour: once a friend legitimately holds the bytes, nothing we do
  decides who they hand them to ("torrents do not know borders"). A knob whose
  enforcement lives entirely in other people's cooperation reads to an admin as
  a guarantee and is not one.
- **What it would have cost.** Serving a privileged stranger needs a way to tell
  privileged strangers apart, which is a capability token, delegated issuance, a
  lifetime, a renewal cadence and a revocation story — or, worse, an
  authorization decision computed by walking the *gossiped* graph, which would
  have turned a transparency feature into an access grant and made "whoever a
  friend calls a friend" a credential. The riskiest machinery in the project
  existed to serve the one tier that could not be enforced anyway.
- **What survives.** Every sharing decision is answered by us, on our side, from
  data we hold: a friend is authenticated by the mesh address deriving from its
  node key, a **member** is a key we can place in our own gossiped component,
  and a guest is everyone else. Tokens keep exactly one job, where they are
  airtight: a **listener node** presenting "this bearer is mine, signed by its
  home server" (§Principals & access), which is identity delegation, not
  distance.
- **The graph decides *whether*, never *how much* — and that is the line that
  matters.** Membership is a use of gossiped records in an access decision,
  which this design refuses for hop counting, so the distinction has to be
  exact. Hop-counted reach would have let hearsay decide *how much* a requester
  gets, and a forged edge would have opened direct-friends-only content.
  Membership can only **narrow** who reaches the Madnetwork set relative to "any
  node on the mesh", and the worst a forged or over-generous edge yields is
  content its admin already marked open to the network. One direction is a leak;
  the other is a bound that can be too loose. Only the second is acceptable, and
  only the second is built.
- **Nothing about the schema changed.** `recordings.share_depth` keeps its
  column and its three constants; `ValidDepth` rejects the in-between values so
  nothing can write one again, and a migration snaps any stored `1…n` to
  **Direct friends** — narrowing, because an admin who reached for "1 hop" was
  describing a bound we can no longer express, and the safe reading of an
  inexpressible bound is the tighter one. Explicit `∞` pins are snapped back
  to *inherit*, which under the Madnetwork default is effect-neutral today and
  still worth doing: the pin was written when `∞` meant "I don't restrict
  onward sharing", so it must not survive a *later* narrowing of the node
  default as though the admin had insisted on it. **When a value's meaning
  changes, consent does not carry over.**

**Onward sharing is still not controlled, and that is stated rather than
implied.** A member who holds our bytes may re-share them under their own
policy, and since the community is the default audience, "a member" now means
rather more people than it did. The model does not pretend to bound that: our
own answer is the only thing we enforce, and past it the holders' judgement is
backed by the ability to see the graph and snip a branch (`federation-trust.md`
§Trust graph). What *is* bounded is that neither we nor an honest member ever
answers a node outside the community at all, so onward sharing spreads content
within the community rather than out of it.

### The audience model

Every mesh request that reveals or delivers library content is answered *for an
audience*, and the same audience decides both halves — **catalog and bytes
together**, so the node never advertises what it would not serve (the F3 rule,
now enforced per requester instead of uniformly):

**Three mesh classes, one per principal** (§Principals & access), and the class
is what the audience *is* — not something a caller derives from loose fields:

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
  nothing else can satisfy the comparison. The predicate's own comment
  anticipated it ("`DepthUnlimited` visible to any reach we ever grow into"), so
  the value that was inert under F5 becomes live by giving members a distance
  — no protocol or schema change (`database/madnetwork_scope.go`).
- **Membership is a lookup, not a computation over hops.** The reachable-key set
  is the **mutual-edge** walk of §The membership rule — F6's
  `BuildNetworkMap` reachability with the both-ends condition and branch
  snipping, so a blocked node's branch is not a member — cached in memory and
  refreshed with the graph sync. No hop count is ever compared, and no
  credential is presented. **Access and the map are two walks over one store**:
  the map draws every claimed edge because seeing a claim is the point, the
  access walk requires both ends because granting on one claim is not.
- **The class must be a value, not an inference.** Two near-misses found while
  designing F7 and fixed when it was built, both the same mistake — a guard
  that means "is a friend" written as the negation of a bit whose meaning was
  about to change:
  - `seedableBlob`'s cache branch was gated on `!aud.GuestOnly`. A stranger was
    `GuestOnly: true`, so the cache was refused by accident rather than by rule,
    and a member — `GuestOnly: false` — would have passed the guard before
    anyone decided members may have it. Now `aud.ServesCache()`. (Members *are*
    served cache blobs since 2026-07-31 — see "the swarm's only boundary"
    below — which makes this a correctness fix rather than a policy one: what
    the branch may never do is serve a **guest**.)

    That removal had a tail (found and fixed 2026-08-13): dropping the bare
    `!aud.GuestOnly` also dropped the guest-limited-*friend* narrowing from the
    serve path, while the later holdings and partials code kept it — so for a
    while a demoted friend was refused the cache's *advertisement* but served
    its *bytes* (`TestGuestOnlyFriendIsNotServedCacheBlobs` is the repro). The
    rule now has one spelling: `ServesCache()` itself carries the
    `!GuestOnly` conjunct beside the class check, and all three sites read it.
    A cache blob has no local recording row, so a guest-limited audience's
    "guest-accessible only" can never be evaluated for it — deny is the only
    reading that keeps the mapping's promise. This also narrows a guest-only
    *token bearer* off the cache, which was already the advertised behaviour
    (holdings refused them) and is the same argument.
  - `serveAudience` returned `Audience{}` on a store error, which was `Distance
    0` — *a full friend*. Inert only because every caller checked the `ok`
    flag.

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
  Strangers used to arrive as `GuestOnly` — that is how the guest-open swarm
  was expressed — and they now arrive as `Distance: DepthUnlimited` instead,
  with `GuestOnly: false`. So the bit is set by the user mapping and nothing
  else, and the mapping is being removed (§Principals & access). When it goes,
  `Audience` collapses to `Distance` alone unless a plain per-peer *guest-only*
  flag replaces the account binding. That is the open detail; the audience model
  itself is unaffected either way.

**Where depth lives: on the recording.** Access already lives there (`license`,
`guest_playable` — one audio identity, one license,
`docs/architecture/recording-tagsets.md` decision 9), and sharing is ultimately
about *bytes*, which are per-recording renditions; hiding one appearance of a
recording while serving another would leak the same blob under a different name.
`recordings.share_depth` (migration 030) is `NULL` by default, meaning **inherit
the node default** (`madnetwork.default_share_depth`, a runtime setting on
`/admin/settings`, **Madnetwork**). One override level over one node default —
no artist/album inheritance chain, deliberately: a resolution chain would land
in every catalog and blob-serve query for expressiveness nobody has asked for.
Bulk selection in the Recordings and All Appearances lenses covers "a whole
artist" in practice.

**Per-audience snapshots.** The memoized own-catalog (`federation.md` §Catalog)
is no longer one global snapshot: it is memoized **per audience class**, not per
peer. The class space is small and *closed*, which is what keeps this bounded: a
friend, a guest-only friend while the user mapping still exists, and a member.
There is no per-hop class to multiply it, because there are no hops, and an
outsider needs no snapshot at all. The serial keeps its meaning — each peer
stores the serial of the snapshot *it* was served, so the not-modified check
works unchanged.

**Scope is the only authority on the network, and `guest_playable` is not**
(decided 2026-07-30, built 2026-07-31, replacing the guest-open swarm). F5
served the blobs and manifests of any guest-accessible recording to any mesh
node, *even when its scope said friends-only* — a second way to be open that
quietly overrode the admin's own setting. Now that "Madnetwork" is a scope an
admin can simply choose, that back door has no purpose and is closed:
`guest_playable` and the license policy go back to meaning what they say locally
— what an **unauthenticated visitor of this server** may play.

What survives is one node setting, `madnetwork.serve_guests`, **default off**:
it answers outsiders with guest-playable content at the byte endpoints only,
within the Madnetwork scope, and never from the cache. A guest's distance is
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

- **An outsider node gets nothing by default.** Not even guest-playable: over
  the mesh, "unauthorized" means unauthorized (decided 2026-07-30, and the
  reason F5's guest-open swarm is withdrawn rather than merely narrowed). The
  capability survives as an explicit node setting, off by default, for the one
  case it was ever wanted — somebody deliberately running a public archive
  node whose whole point is handing guest-playable content to anyone. That is
  that operator's choice to make, and it should take a switch to make it.
- **The swarm's only boundary is the community** (declared 2026-07-31, replacing
  "cache blobs never leave the friend ring"). Holdings and cache blobs are
  served to **members**, not just to direct friends — one boundary, drawn
  once, in the same place for discovery, for bytes and for seeding. Why
  distribution in particular cannot afford a tighter line is argued where it
  belongs, `federation-swarm.md` §Distribution.

  The cost is real and accepted. Seeding a cache blob means re-serving content
  this node did not publish and whose licence it cannot vouch for, and a
  holdings list says *somebody here fetched this* to a wider audience than
  before. Both stay inside the community, both are what `seed_cache` (default
  on) turns off for an operator who does not want them, and neither is ever
  offered to a guest — a stranger is told nothing about our cache, as before.
- **A blocked peer is refused everything**, as before, and since F7 its whole
  branch also stops being members (§The membership rule).

**Tokens are not part of reach any more** (decided 2026-07-30, superseding the
2026-07-25 sequencing note). They existed to identify a privileged stranger at
depth ≥ 1; with the ladder gone there is no privileged stranger, and every
requester resolves to one of the three mesh classes without presenting anything.
The one surviving use is the **listener node** — a madplayer that no
membership lookup can place, because it publishes no friend list and appears in
nobody else's (§"The capability token").

**The legal frame** (madshare.org), stated more plainly than before: madnetwork
is a **community of friends** — a mesh of nodes that each had to be
deliberately friended in by somebody, joined transitively, not a public web
server. Nothing federation does exposes anything to the anonymous internet: the
mesh is the only transport, every requester is authenticated by a key it must
possess, a node outside the community is served nothing at all, and a node's own
`/files/*` policy is a separate, local decision.

Inside the community, sharing is the default and restriction is the exception.
That is a deliberate posture rather than an oversight, and it should be read as
what it is: **sharing with the community is publication within the community**.
The content is fetchable by every member — people the admin never approved
individually, vouched for by a chain of friendships they can inspect — and the
admin who runs the node owns that. What it is *not* is publication to the world:
the boundary is enforced on every request, not merely stated. An admin who wants
a narrower line has **Direct friends** per recording or node-wide, and **Local**
for content that must never travel.

**Planned — no fingerprint, no publication (not built; decided 2026-07-26).**
The published set gains a third condition beside "approved and live" and the
audience filter: a rendition is publishable only when its blob has an
`audio_fingerprints` row. Requiring `fpcalc` to *start* a federated node closes
the tool-missing case; this closes the per-file remainder — a blob that was
never successfully fingerprinted (ingested before the tool existed, corrupt
audio, a codec `fpcalc` chokes on) would otherwise still be advertised with an
audio identity nothing local ever verified. Publishing it asks friends to trust
a grouping claim this node cannot itself stand behind.

Design notes for the implementation:

- **Rendition-level, not recording-level.** Each rendition is a distinct blob;
  one being fingerprinted says nothing about another, and a `recording_pinned`
  rendition can join a recording by hand without ever being analysed. A
  recording whose renditions are all unfingerprinted then has nothing to
  advertise and drops out of the catalog on its own — no separate
  recording-level rule.
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
  themselves — the startup backfill re-analyses anything lacking a fingerprint
  — so the state is usually "not yet", and it should read that way.
- **Expect the serial to churn** on a node that federates for the first time
  with a large unanalysed library: entries appear as the backfill lands, so
  friends re-pull the snapshot repeatedly for a while. Harmless at the 15-minute
  sync cadence, worth knowing before someone reports it as a bug.


## Related

- [`federation.md`](federation.md) — the spine: goal, vocabulary, transport,
  catalog, availability, build plan.
- [`federation-trust.md`](federation-trust.md) — the graph these rules read:
  friendship, gossip, blocking, the network map.
- [`federation-swarm.md`](federation-swarm.md) — the byte endpoints these
  rules gate, and the per-member quotas that bound what a member may cost us.
- `docs/architecture/auth.md` §8–§10 — the local permission model these
  principals sit beside.
- `docs/ui/madplayer.md` — the listener node's client half.
