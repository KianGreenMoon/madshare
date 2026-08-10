# Madnetwork trust — friendship, the graph, transparency and defense

> **How nodes come to know and judge each other.** Part of the federation design;
> the spine, the vocabulary and the build plan are in
> [`federation.md`](federation.md), and what the graph then *grants* is in
> [`federation-access.md`](federation-access.md) — this document builds the
> graph, that one reads it.
>
> **Status: built.** All of F6 (friend-list records, distrust marks, the network
> map, the naming split, contradicted-claim reports, underlay de-peering) plus
> forgetting, on-demand rescan, map-at-scale (F7 item 7) and branch weighting (F7
> item 10).
>
> The governing rule, because everything here is one step from an access decision:
> **the graph decides *whether* a node is in our community, never *how much* it
> gets** — [`federation-access.md`](federation-access.md) §"Why the ladder
> collapsed".

## Friendship (F1, built)

- **Node card** — the out-of-band introduction two admins exchange (chat,
  mail, any channel they trust): a small JSON blob `{"madshare_node_card":
  <protocol>, "name": "…", "public_key": "<hex>"}`, exported (copy/download)
  from `/admin/network`. It deliberately carries only identity — underlay
  connectivity is `[federation]` config's business (public mesh or explicit
  `peers`/`listen`), not the card's. `[federation].name` sets the display name
  (host name when unset); identity is always the key.
- **Trusted-peer table** (`federation_peers`, migration 026): one row per known
  node — key (identity; the mesh address is derived, never stored), local
  label, state, `last_seen`, and the optional **user mapping** (`user_id`) that
  binds a personal madplayer node to a local account (`federation-access.md`
  §Principals & access). States: `pending_outgoing` (we imported their card) ·
  `pending_incoming` (their node introduced itself, awaiting our accept) ·
  `friend` · `blocked` (with the pre-block state remembered for unblock).
- **Pairing handshake** (`POST /madnetwork/v0/pair` on the mesh): a node
  introduces itself with `{protocol, name, public_key}`. No signatures — the
  mesh address is derived from the node key, so the connection's source address
  *is* proof of key possession; the handler additionally verifies the claimed
  key derives to exactly that source address. Receiving a pair request from a
  `pending_outgoing` peer proves mutual intent → both flip to `friend`; from
  an unknown key it records `pending_incoming` for the admin. A background
  **refresh loop** (1-minute tick, nudged on import/accept) retries outbound
  pairings and pings friends, so both sides converge through any offline window
  and `last_seen` stays fresh. **Friending is deliberate** by construction: a
  node becomes a friend only after *both* admins acted — and accepting an
  incoming request shows the full key so the admin can check it against the card
  received out-of-band (never a blind one-click).
- **Blocking:** a blocked peer is refused the *entire* protocol surface (even
  ping, HTTP 403) by the mesh-side auth wrapper. Unblock returns the peer to its
  pre-block state. Since F6 a block also publishes a **distrust mark** carrying
  its reason, drops the peer from our published friend list, snips the branch
  behind it **on the map**, and cuts the underlay link wherever we dialled it
  (built 2026-07-30; an inbound link is the documented exception). Snipping
  stops at the drawing today — the records behind a cut branch are still
  stored, relayed and admitted, which §Forgetting is about.
- **Admin surface:** `/admin/network` (own card, import form, peer list with
  accept/block/unblock/remove/rename/user-mapping; pending-request badge on the
  dashboard; the F6 network map) over `/api/admin/federation*`, all gated
  `federation.manage`.

### The trust graph is a graph (built 2026-07-31)

Two nodes that are both friends of a third must be able to friend **each
other**, and a node must be able to sit in several branches at once. Nothing in
the state machine ever forbade it — a pair request from an unknown key is
answered the same way whoever sends it — but two things made it look forbidden
from the admin's seat, and both are now fixed.

- **Friending by key.** The import form and `POST /api/admin/federation/peers`
  take `{"public_key": …, "name": …}` as well as a card, and the network
  map's detail panel offers **Ask to be friends…** on any node it draws that
  we have no row for. The map already carries every node's key — that is the
  whole identity, and a card adds only a claimed name — so a friend of a
  friend is friendable without their admin exporting anything. Both surfaces
  show the full key, and the map's confirm says outright that a name out there
  is hearsay. Nothing about *deliberate* changes: this sends a request, the far
  side still records a `pending_incoming` its admin must accept. A **mesh
  address is refused with its own message**, because an address is derived from
  a key and cannot be turned back into one.
- **Pairing says why it has not converged.** Every failure in `pairWith` used to
  be a silent `return`, so an unreachable node, a refusing node and a node whose
  admin has not clicked Accept were one indistinguishable `pending_outgoing`.
  Each attempt now records a `PairAttempt{At, Result, Error}` — in memory on
  the node, since it describes the last try rather than the friendship —
  carried on the peer row as `last_attempt` and rendered on the card: *request
  delivered, waiting for their admin* (the common case, and not a fault),
  *nothing answered*, or the far node's own refusal text. It is logged too, but
  only **when the outcome changes**: the sweep retries every minute, and a node
  that is merely switched off must not write a line a minute about it.

Verification: `federation/friendgraph_test.go` friends A–B and B–C on a
chain underlay where A and C have no link, then friends A–C over two hops;
`meshlab friend A B` does the same with real processes on a running lab.

### Names are a convenience, the key is the identity (built 2026-07-30)

Self-naming stays exactly as it is: `[federation].name`, falling back to the
host name, is what a node says in the pair handshake — "hello, my name is …"
and nothing more. What needed fixing was the receiving side, where **three
different names were collapsed into one `federation_peers.name` column**:

| | what it is | who owns it |
|---|---|---|
| self-name | what we call ourselves on the wire | this node's config |
| heard name | what a peer calls *itself* | the peer — a claim, refreshable |
| local label | what *we* choose to call that peer | this admin, always wins |

Before migration 033 the column was seeded from the card, then overwritten by a
rename — which destroyed the claim — and afterwards never refreshed at all,
because `pairWith` backfilled the name only while it was empty. So a peer that
renamed itself stayed under its old name forever, and an admin who renamed a
peer could no longer see what that peer calls itself.

**Built shape.** `federation_peers.heard_name` holds the claim and `name` is the
local label; nothing writes both. The label is written *only* by an admin rename
(`RenamePeer`, and clearing it is allowed), the claim *only* by
`UpdateFederationPeerHeardName` from a contact, and `Peer.Label()` resolves
`local label ?? heard name ?? empty`. `peerLabelExpr` is the SQL twin for the
browse surfaces that only ever show a name; `peerLabel` in `admin/network.js` is
the client one.

**A peer is never named by a blank**, which needs one more step than `Label()`,
because 033 left *every* row unlabelled: until an admin renames a peer or we
hear its name, `Label()` is empty. `Peer.Display()` is the rendering form —
`Label() ?? short key`, the Go twin of `peerLabel` — and it is what log lines
and stats rows use. `Label()` deliberately keeps returning empty, because the
network map's `displayName` has a better fallback than the key: the name the
*graph* uses for that node. Pinned by `TestPeerDisplayNeverBlank`.

- **Refreshed on every successful contact.** `GET /madnetwork/v0/ping` now
  answers with the node's own `name`, so the 1-minute refresh loop keeps every
  friend's claim current — a node renaming itself is heard within a minute
  instead of never. Pairing (both directions) refreshes it too. The field is
  additive, so a peer that does not send one simply leaves the last claim
  standing, and a write only happens when the name actually changed. It is also
  the first field of the NodeInfo-style health card sketched in
  `federation.md` §Availability.
- **Which name we publish and show.** Gossiped edges carry `Label()` — the
  publisher's own label for that friend, which is what a `GraphEdge` name has
  always meant. On the map, `displayName` resolves best-evidence-first: our
  label, then what the node told *us* directly, and only then the name the graph
  gossips about it, which is hearsay from third parties.
- **The migration cannot classify old rows** — a value seeded from a card is
  indistinguishable from one an admin typed — so 033 moves `name` into
  `heard_name` and starts every row with no label. A peer that had been renamed
  reverts to its own name on the next contact and can be renamed again in two
  clicks; the alternative would pin a name the admin never chose and reproduce
  exactly the bug being fixed. Failure visible and recoverable beats failure
  silent and permanent.
- **Wherever a node is shown, its mesh address or key is rendered beside the
  name** — peer cards and the F6 network map above all. The peer card also
  shows `calls itself “…”` whenever a local label is hiding a different
  claim: the label wins, but it must never make the peer's own name unreadable.

- **On the map this matters more than in the peer list.** Once friend-list
  gossip lands, most nodes on the graph are ones we have no relationship with,
  and their names arrive *second-hand from a friend*. A name there is hearsay
  about a stranger, so nothing may be identified by it.
- **Impersonation is a naming problem, not a hole.** Any node may call itself
  anything, including exactly what a friend calls itself. There is no fix at the
  name layer and none is needed — the address is the identity, the name is a
  label, and the UI must never let the second stand in for the first.
- **Sanitize peer-supplied names** — *done 2026-07-30.* Detail below.
- **Capped at 64 runes** — *done 2026-07-26, `MaxPeerNameRunes`; the naming
  split above is what remains of this entry.* 64 clears a DNS label (63 octets),
  so no realistic host name is ever truncated, while staying far below anything
  that could disrupt a layout. The previous cap counted **bytes**
  (`name[:100]`), which made the effective limit depend on the script — 100
  characters of ASCII, 50 of Cyrillic or German umlauts, 25 of emoji — and cut
  a 3-byte character in half at that boundary, storing invalid UTF-8 for CJK
  names. The UI truncates further for display (~24 characters with the full
  value on hover); that is a rendering choice, not a storage limit.

#### Name sanitization (built 2026-07-30)

**This is not an XSS fix, and must never be sold as one.** The admin UI already
renders names safely — `el()` in `webui/static/js/admin/shared.js` assigns
`textContent` and appends string children as text nodes, and its `html:` escape
hatch carries trusted icon markup only. Escaping stays the defense against
injection. Sanitizing is about **display integrity**: a name should render as
what it is, and two different nodes should not be able to render identically.
Recording the distinction because the failure mode is somebody later deciding
the sanitizer makes escaping unnecessary.

`CleanPeerName` is the single choke point and stays that way — every name
passes it, whether from a node card (`ParseCard`), a pair request
(`handlePair`/`pairWith`), a gossiped friend list (`ParseGraphRecord`), an admin
rename (`RenamePeer`), or this node's own `[federation].name`/host name. It and
`CleanMarkReason` are two caps over one `sanitizeLabel`, so a mark's free text
— longer, and read by someone deciding whether an accusation applies to them
— gets the same treatment. The rules, **in this order**, because the order is
load-bearing:

1. **Invalid UTF-8** — drop the offending runes (Go decodes them as `U+FFFD`).
2. **Strip Unicode categories `Cc` and `Cf`.** `Cc` is the control characters:
   C0/C1, newline, tab, DEL. `Cf` is the elegant part — one category test
   covers the bidi overrides (`U+202A`–`U+202E`, `U+2066`–`U+2069`, which
   can visually reverse a rendered name), the zero-width characters
   (`U+200B`/`200C`/`200D`) that make two different names look identical, and
   `U+FEFF`.
3. **Strip `Co`** (private use): vendor-specific glyphs and tofu.
4. **Normalize to NFC**, which collapses `é` written as one rune against `e`
   plus a combining accent — another way two names render identically while
   differing byte for byte. `golang.org/x/text` is already a direct dependency,
   so `unicode/norm` costs nothing new. It runs *after* the strips, so two
   characters separated by a zero-width joiner still compose once it is gone,
   and *before* the mark bound, since composing removes marks that step would
   otherwise count.
5. **Bound combining marks** (all of `M`: `Mn`/`Mc`/`Me`) per base character —
   the "Zalgo" stack that renders as a vertical smear over neighbouring rows.
   Two marks per base is generous for every living script. The count is of marks
   *following* a base character, so a precomposed `á` carries two more; that
   caps the rendered stack without having to reason about which scripts
   precompose. A mark with no base — a name opening on a floating diacritic,
   or one following a collapsed space — is dropped.
6. **Then** apply the 64-rune cap. Capping first would let stripped junk consume
   the budget and truncate the real name.
7. **If nothing survives, the name is empty** — display falls back to the
   short key, exactly as an unnamed peer does today. Never render an empty
   label.

Whitespace collapse runs on the same pass as 5–6: runs fold to a single
`U+0020` and the ends are trimmed. Because `unicode.IsSpace` covers the `Z`
categories, this is also what folds `U+00A0` (no-break space) onto the plain
space — one more pair that renders alike.

**The accepted cost, stated rather than hidden:** stripping all of `Cf` also
removes `U+200C` (ZWNJ), which is orthographically meaningful in Persian and
Arabic, and `U+200D` (ZWJ), which joins emoji families — 👨‍👩‍👧
becomes three separate people. That is accepted for a *label* that carries no
identity role. The narrower alternative, "strip `Cf` except ZWJ/ZWNJ", reopens
precisely the invisible-difference vector this rule exists to close, so it is
not the default.

**Homoglyphs remain unsolved and that is fine.** Cyrillic `а` against Latin `a`
cannot be filtered without mixed-script heuristics that punish legitimate
multilingual names. The answer stays the one this whole section rests on: the
mesh address is displayed next to the name, and identity is the key.

Existing rows keep their unsanitized names — the sweep is not worth a
migration, because a name refreshed from its peer on the next contact (planned
above) heals itself.

The tests are a golden table (`TestSanitizePeerName`): a `U+202E` reversal, a
friend's name padded with `U+200B` into a second peer, an embedded newline, a
private-use glyph, a Zalgo stack, a decomposed `é`, an emoji family and a
Persian ZWNJ name (both documenting the loss), and names that sanitize to
nothing. `TestSanitizeCapsLast` pins rule 6 by padding a full-length name with
200 zero-width spaces and asserting the name survives whole.

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
    suppression list — config re-adds the peer at startup, and a minute later
    it is cut again, for as long as the block stands. Two limits, both real:
    yggdrasil's `core.RemovePeer` (the underlay call — not `Node.RemovePeer`,
    which forgets a peer *row*) reports "not configured" for a link on a shared
    segment (that is the case the parenthesis above describes), and an
    **inbound** link is skipped entirely because yggdrasil v0.5.14 *panics* when
    asked to remove one (nil cancel func; see `.issues/open-issues.md`) and
    exposes no handle for it anyway. A blocked node that dialled us therefore
    keeps its transit until it disconnects, while getting nothing from the
    application.
  - Blocks are **published as signed distrust marks**, relayed network-wide like
    the friend records and carrying a short reason: "see whom the network does
    not trust, and why." Every block publishes one — there are no private
    blocks. Readers factor them in manually; nothing is automatic, and the
    accepted risk of a public ledger is spelled out in §Friend-list gossip.
  - Blocking a node also snips the *branch* behind it — nodes reachable only
    through the blocked node drop out of our view; nodes also connected via
    other friends remain.
- **Stolen-key scenario:** the same mechanism — block the compromised key,
  publish the distrust mark; the network routes/trusts around it.
- **Mislabeling / spam (the "rickroll" problem)** — a tagset claiming one
  thing attached to audio that is another. Layered defense, mostly structural:
  1. Because tagsets attach to **recordings** (audio identity), a mislabel on
     known audio lands on the *true* recording and becomes a visibly absurd
     **minority label** next to the dominant honest tagsets — it does not
     create a fake track. Auto-flag tagsets that conflict with a recording's
     dominant label. The attack surface shrinks to rare/unknown audio.
  2. **Popularity is trust-weighted, never raw counts** (sybil resistance):
     carriers are weighted by trust distance, and nodes reachable only through
     one friendship edge count as **one branch**, not many voices. A sybil farm
     inflates nothing and dies with a single snip. *Built — see "Where the
     weighting applies" below.*
  3. **Attribution:** every tagset carries signed provenance + the friend path
     that delivered it. Detect → details → block → branch snipped,
     distrust mark published. A troll gets each admin at most once and grows
     more visible with every hit.
  4. **Independent ground truth (reuse):** the review card runs the existing
     tag-suggestions machinery (local fingerprint → AcoustID → MusicBrainz)
     and **warns on mismatch** ("tagset says X; fingerprint says Y"), with the
     preview player right there. Optional (needs the AcoustID key), but an
     oracle outside the social graph entirely.
  5. **No global view to poison:** your catalog is your friends' choices bounded
     by your depth knob. Trolls can flood their own corner of the network; they
     cannot dilute yours — which is exactly why rating stays local/manual and
     never network-global.
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
(`federation/branches.go`, `Node.BranchMap` → node key → the direct friends
it reaches us through). Deliberately the **same walk** the diagram is drawn
from: a holder in a track's ⓘ panel links straight to its node on that map, so
a ranking explained by one graph beside a diagram drawn from another would be
two answers to one question (`TestBranchesMatchTheMap` pins it). It is a
separate entry point from `NetworkMap` only because that one also groups
distrust marks and derives a mesh address per node — ed25519 work that belongs
on a diagram an admin opens occasionally, not on a search-as-you-type. Memoized
on the membership TTL, for the same reason and with the same safe direction of
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
  people actually press. It is now ordered by voices, holders only as a
  tiebreak.
- **The *Most held* and *Missing here* lanes** — the two SQL ranks by a raw
  holder count. *Missing here* is the first ranked lane on the page, which makes
  it the more valuable of the two to an attacker.
- **Not the other three lanes, on purpose.** *From direct friends* is already
  branch-weighted by construction, since every direct friend is the root of its
  own branch — re-weighting it would replace that with the wider count and
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
  not — several nodes, one voice. Sending it always would put "5 nodes · 5
  branches" on every row, which is wallpaper, not transparency.

What this does **not** answer, and does not pretend to: **volume from a single
honest branch**. One friend with fifty thousand badly tagged albums is still one
voice and still fifty thousand rows. That is a clustering problem, tracked in
`docs/ui/madnetwork-page.md` §Open.

### Contradicted identity claims (built 2026-07-30)

A peer's catalog makes claims this node can *check*, and when a check fails the
admin hears about it with the evidence attached. This is the "Detect →
details" arm layer 3 promises; the "→ block → snip → publish the mark"
half is F6's existing toolkit. A false audio identity is worth singling out
because it is **provable** — unlike a tasteless tagset, it is arithmetic —
which is exactly what makes it fair to put in front of an admin as grounds for
blocking.

What is checkable, cheapest first:

- **Against blobs we already hold — no download, no request** (`held_blob`).
  For a hash in our own library we know the true fingerprint. A peer advertising
  that hash with a materially different one is contradicting bytes we can hash
  ourselves. The check is a SQL join over the *overlap*, so it costs a
  comparison per hash both sides have and nothing per hash only one of us has.
  This case is **airtight**: identical bytes cannot fingerprint differently.
- **Against a materialized download** — the same check, reached from the other
  side. The pipeline re-fingerprints fetched audio before it joins a recording
  (`federation.md` §Catalog), which simply makes the download one more blob we
  hold, and the next sync round compares the origin's standing claim against it.
  That is why the checks read the *cached* catalog rather than a freshly
  received snapshot: a peer's claims stand still while our own library moves,
  and a not-modified sync round must still re-check. No separate code path, one
  rule read once.
- **Against the peer's own grouping — needs no wire claim at all**
  (`grouping`). A `recording_key` asserts "these renditions are the same audio".
  Hold two of them and the assertion is testable locally without the peer's
  cooperation: *both* fingerprints in that comparison are ours.

**The threshold is the local one.** A contradiction is a start-aligned bit-error
rate above `database.maxBitErrorRate` (0.10) — the same number
`ResolveRecording` groups renditions by. Reusing it makes a finding explainable
in one sentence: *the claim would not group with our own bytes by the very
standard this node uses to decide that two files are the same audio.* Under 16
compared words the check declines to answer, because a claim we cannot check is
not a claim we distrust.

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
gossip and F7 reach land — an honest relay repeating someone else's claim,
which makes the *origin* of a claim a separate question from its *carrier*. Only
the same-hash case above is airtight; the fuzzier ones are BER comparisons
against a threshold and must be worded as such. Present a conflict and its
provenance, never a verdict.

**Storage** is `federation_claim_reports` (migration 034): one row per (peer,
kind, hash, other_hash) with an admin disposition (new / dismissed / acted), so
a repeating check refreshes the measurement and re-alarms nobody — and a
dismissal is never overwritten by detection. The evidence travels with the row
(both compared heads, both fingerprinter versions, the BER and the word count),
so a finding survives the catalog replace that produced it. Rows CASCADE with
the peer: forgetting a node forgets what we found about it. The admin surface is
`GET /api/admin/federation/reports` + `PATCH …/reports/{id}`, and a **count
badge on the dashboard** beside the pending-peer one is the whole notification
design; this must not become mail.

**The catalog carries the fingerprint claim, as a bounded head.** The F2 wire
never had one; `CatalogRendition` now has `fingerprint: {algo, version, words,
head}`, additive so an older peer simply contributes nothing checkable. `head`
is the first `federation.ClaimHeadWords` (64) raw sub-fingerprint words, base64
of the same little-endian packing the DB stores — **not** the whole
fingerprint, and the reason is measured rather than guessed: a real fingerprint
is ~950 words (3.8 KB packed) for a four-minute track, a snapshot is re-sent in
full whenever its serial moves, and shipping all of it would add ~5 MB per sync
to a thousand-rendition catalog on a 15-minute cadence between
intermittently-online home servers. 64 words is ~15 s of audio and 2048 compared
bits: the same bytes score 0, unrelated audio lands near 0.5. The comparison is
start-aligned exactly like the local matcher, so a head is the same kind of
evidence measured over less of it. Publishing it leaks nothing new — a friend
already gets the hash and the full tag text — and the browse endpoints strip
it, since a browser has no use for 340 bytes per rendition. This is also what
layer 1's "auto-flag tagsets that conflict with a recording's dominant label"
needs to work across nodes.

The *byte*-level lie needs nothing here: bytes that do not hash to the requested
hash never enter the cache and cost the provider its place in the swarm
(`federation-swarm.md` §Distribution). This item is about claims that survive
byte verification.

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

- **Per edge: key, name, `since`.** The mesh address is never sent — it
  derives from the key (`AddrForKeyHex`). `since` is when the friendship was
  made, a cheap durability signal a five-year-old edge should get credit for
  when trust weighting arrives in F7; it also leaks a timeline of who befriended
  whom, which is the price.
- **Signed by the origin's own node key** — the key that already *is* the
  identity, so no new PKI. A relay carries the bytes untouched: it can withhold
  a record, never forge one.
- **The names are hearsay.** Sanitized and rune-capped on receipt exactly like
  peer names, and the map renders the address beside every one — most nodes on
  the graph are strangers whose names arrived second-hand (§Friendship,
  naming).

**Propagation: friends relay, nobody crawls.** A node opens connections to its
own friends and to nobody else, ever. Each friend serves its whole store, so
records ripple outward one ring per sync round until every store holds the
entire connected component. No hop limit — the radius is unlimited by design.

- Signatures are what make this safe: A can hand me X's record and I verify X
  wrote it without X and I ever meeting.
- **Rejected — dialing nodes directly** (take a key from a friend's list,
  connect to that node, ask it yourself). It costs N² connections per round
  (500 nodes ≈ 250 000 dials) to move a graph that changes monthly; it
  requires opening the friends-only mesh endpoints to strangers; and it routes
  around the trust model by making every node interrogable by anyone. Relaying
  is both cheaper *and* more complete, since it surfaces nodes we could not dial
  at all.
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
digest; a realistic mesh of a few hundred is 10–30 KB; an unchanged graph
costs one small round-trip per friend and moves no payload at all.

**Expiry: 7 days, refreshed every 6 hours.** The origin re-signs on the
heartbeat even when nothing changed; receivers drop a record 7 days after
`issued_at` and stop serving it. Chosen against this network's actual population
— intermittently-online home servers (`federation.md` §Goal) must survive a
weekend offline — while an abandoned key fades from every store inside a week
with nobody acting. Rejected: 24 h/1 h (a two-day trip drops you off the map,
and 24× the chatter for a graph that changes monthly) and 30 d/24 h (a snipped
branch lingers a month in stores that forgot why).

**Publishing is node-level and default-on.** Runtime setting
`madnetwork.publish_friend_list`, default on, matching the network's default-∞
transparency. All edges or none — deliberately no per-peer granularity.

- **The switch means "I publish no record", and nothing more.** A shared edge
  has two ends, and the other end is not yours to silence: friends' records
  still name you, so you stay on the map with visible edges and only your own
  list is missing. The UI text must say precisely that. Anything softer sells an
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
  (`MaxOriginsPerBranch`), and blocking that friend drops everything that
  entered through it;
- at most one accepted new `seq` per origin per minute;
- a record whose origin is named by nobody in our store is junk, and dropped.

Together these bound a sybil farm to display noise: a farm behind one edge is
one branch, and dies with one snip (layer 2 above).

**These numbers bound our store, never the community** (restated 2026-07-31,
because F7 makes it easy to misread). They cap what one friend can cost *us* in
rows; they are not an opinion about how large a legitimate network may be, and
**membership is not derived from them**. A community that outgrows
`MaxOriginsPerBranch` is a number to raise, not a boundary being defended —
and the reason that stays safe is that membership requires a *mutually declared*
edge (`federation-access.md` §The membership rule), so the quota counts nodes
that put their own signature to belonging rather than names a friend merely
mentioned.

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
  out of the friend-list record — a block is not a friendship, and publishing
  it as one would misstate the graph.
- **Key, when, and a short reason.** A bare key is an anonymous downvote: the
  reason is what lets a reader judge whether it applies to them, and it pairs
  with the contradicted-claim reports above, which produce exactly this
  evidence. Capped and sanitized on the peer-name rules at a larger cap (280
  runes).
- **Relayed network-wide**, so everyone sees whom everyone distrusts —
  including the node being marked.
- **Accepted risk, stated plainly.** This is a global, public accusation ledger,
  carrying free text, with no rebuttal path, readable by its target: the
  intra-network-war warning this section opens with applies to it squarely. It
  was chosen with that understood (2026-07-26). Three containments, none of
  which soften the choice:
  - **marks expire on the record schedule** — unblock, stop refreshing, and
    the mark is gone from every store within 7 days. A ledger that forgets is
    recoverable; a permanent one is not. **Lifting a block does better than
    waiting out the TTL** (built 2026-07-26): clearing the last mark publishes
    an *empty* record rather than simply ceasing to refresh, so the record
    carrying the accusation is superseded on every node at the next sync instead
    of standing for up to a week after the admin withdrew it. The asymmetry is
    deliberate — a node that has never blocked anyone publishes nothing at
    all, since an empty record would cost every store a row to say nothing.
  - **display is branch-weighted** — one branch is one voice (layer 2), so a
    farm publishing 10 000 marks against a key renders as a single entry.
  - **nothing is automatic**, as everywhere else here: a mark is evidence put in
    front of a human beside the Block action, never an input to a score.

**F6 itself changed nothing about who may fetch what.** Every requester stayed
at distance 0 and the wire's access rules remained exactly F5's, which is what
made the phase safe to ship on its own. F7 gives the store one access job —
answering whether a key is in this component at all (`federation-access.md`
§Principals & access) — and no more than that: `Audience.Distance` never
became a hop count, because the ladder that needed one is gone
(`federation-access.md` §Sharing scope).

**Storage** is a cache, like `federation_catalog`: rebuildable, referenced by
nothing local, dropped and refilled without consequence. Migration 031 holds the
records keyed by origin (`seq`, `issued_at`, payload, signature, which friend it
arrived from, expiry) beside the edges and marks denormalized off them, so
admission checks and the map are queries rather than a scan that decodes every
payload. The payload column is the record **verbatim** — nothing re-encodes
it, because the signature covers the bytes as written and a record may carry
fields this build cannot parse. Migration 032 adds `block_reason` / `blocked_at`
to `federation_peers`, the evidence a published mark carries.

**Wire**: `GET /madnetwork/v0/graph` (digest, `since=`-aware) plus `POST
/madnetwork/v0/graph/fetch` for the raw bytes of named records — a POST
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

Ending a friendship is instant everywhere it is *enforced* — the peer row
changes state, the mesh door refuses a blocked node, our own published record
drops them on the next sweep. It is not instant in what we **remember about the
network**, and the gap is visible: a friend we removed is still drawn joined to
us, and the strangers we only ever heard about through a node we have now cut
are still on our map, still relayed onward, still admitted. Three concrete gaps,
and the third falls out of the first.

**1. An edge to us is a local fact, not hearsay.** `BuildNetworkMap` links a
pair from either side's claim, including claims *about us*. So a node whose
admin never removed us keeps publishing "friends with you", and we keep drawing
it — for the record's whole 7-day life, refreshed forever if they keep
republishing. The fix is the smallest one in this list and the one everything
else rests on: **any edge with our own key at an end comes from
`federation_peers` alone**. If we are not friends with X, there is no X–us
edge on our map, whatever X says. Independently of removal this closes a small
integrity hole: any node already in our view can publish a friendship with us
that does not exist, and today we draw it — which puts a node we have never
met on the inner ring, at distance 1, as a branch root. Admission
(`GraphKnowsKey`) keeps a *complete* stranger's record out, so this is bounded
to nodes someone already vouched for; it is still a claim about us that we are
in a position to know the truth of, and do not check.

The asymmetry is deliberate and stays: *other* nodes' edges remain single-claim
on the map, because an edge somebody claims is worth seeing
(`federation-access.md` §The membership rule is where mutual claims matter).
Our own edges are neither single- nor mutual-claim. They are not claims at all.

**2. Reachability decides what we keep, not just what we draw.** The map already
refuses to discover through a blocked node, so the branch behind one disappears
from the picture. Nothing else follows it: those records are still offered in
our gossip digest and served to friends who ask, their origins still satisfy
`GraphKnowsKey` so more records from them are still admitted, and they sit in
the store until `expires_at`. A block is complete on screen and half-done
underneath.

So the one walk from our key should decide all four: whether a record is drawn,
whether it is **relayed**, whether its origin **counts as known**, and whether
it is **kept**. Unreachable ⇒ dropped, on the sweep that already runs
`ExpireGraph`. This is safe precisely because the graph store is a cache in the
sense `federation_catalog` established — rebuildable from the network,
referenced by nothing local, safe to drop — and it is the same walk F7's
membership check needs memoized anyway, so it is shared work rather than new
work.

**3. A removed friend's branch dies with the edge.** Today removal is *worse*
than blocking: the peer row is gone, so nothing marks them as a node not to
discover through, and their one-sided claim (gap 1) keeps the whole branch
behind them alive and admitted. `received_from` is `ON DELETE SET NULL`, so the
records they introduced also lose the attribution that would let an admin see
where they came from — they survive, orphaned, vouched for by nobody. With gap
1 closed there is nothing new to build here: no edge from us means unreachable,
and unreachable is collected by gap 2. That is the property to preserve —
**removal and blocking should not need separate forgetting machinery**.

**"Unless we have another connection to them"** is the existing multi-source BFS
with its merging branch labels, unchanged: a node also vouched for by another
friend stays, at whatever distance and with whatever branch labels remain. The
condition is already expressed correctly; it simply governs more than drawing.

**What must not be forgotten.** Our own peer rows stay on the map with no edge
drawn — a blocked node an admin cannot see is a block they cannot lift. Our
own distrust marks are ours regardless of who is reachable. And a blocked peer's
cached catalog stays (hidden from browse) so lifting the block restores service
without a re-sync — that is a direct relationship of ours, not branch data;
`RemovePeer` already CASCADEs it away, which is the right difference.

**The costs, stated.** Unblocking or re-friending re-syncs what we dropped, on
the next digest round — `UnblockPeer` already nudges the loop, so it refills
promptly rather than on the 15-minute cadence. And if we were the only path
between two halves of the network, cutting the branch slows *their* convergence,
not just our view: accepted, because we are not a relay for a branch we have
severed, and a network of friendships has other paths by construction.

**Why this stops being cosmetic under F7.** After F7 this same graph decides who
is served, so a branch we believe we cut but still hold is not a stale drawing
— those keys are members, and members get the library. It should therefore
land with or before F7's membership walk, and its test is the one that would
otherwise be missing: remove a friend, and assert the nodes seen only through
them are gone from the map, from the digest, from admission **and** from the
served audience.

**Smaller staleness in the same family**, fixed with it: the in-memory
pairing-attempt note (§Friendship) used to survive a `RemovePeer`, so
re-importing a key we once failed to reach showed a "last try" from before the
removal. `RemovePeer` now drops it — best-effort, since failing a removal over
a log note would be the worse trade.

**Admission needs no new rule, and that is the point.** `GraphKnowsKey` admits a
record whose author some record we hold already names. Once the sweep *drops*
the records that named a cut branch, the branch stops being named — so the
same unchanged admission check refuses it on the next round. Dropping and
refusing are the same act seen twice, which is why part 3 costs nothing: an
origin re-offered by a second friend is one that friend's record still names,
and it is admitted because it genuinely is reachable again.

### Refreshing the graph on demand (built 2026-07-31)

Gossip rides the catalog cadence (`sweep`, 15 minutes), and `CatalogSyncedAt` is
bumped on the not-modified path too, so an admin who has just changed something
waits out the timer with no way to say "look again now". `Nudge` does not help:
it wakes the loop, the loop re-checks the same timer, and the graph sync is
skipped. A **Rescan** button on `/admin/network` sets a force flag and nudges;
the sweep then runs `syncGraph` against every friend regardless of the timer.

**Graph only.** Not the catalog, not holdings. The catalog is the expensive one
— its cost scales with a library rather than a friend list — and it already
answers `unchanged` in the common case, so forcing it buys freshness nobody
asked for at the only price worth avoiding. The button exists to refresh *the
map*, and the map is built from graph records.

**What it can and cannot do**, which the UI should say rather than imply.
Convergence is one ring per round: a rescan makes our view as fresh as our
friends' *stores*, not as fresh as the network. A change three hops out still
waits for the nodes in between to run their own rounds. Nothing can shortcut
that without dialling strangers, which is the one thing this design refuses.

Note also what the button is *not* for. A change to **our own** friendships
needs no rescan at all once §Forgetting part 1 lands — our edges come from
`federation_peers`, so removing a friend takes the edge off the map on the next
page load, with no round-trip. The button covers the other case: somebody else's
friendship moved, and we would rather not wait fifteen minutes to hear about it.

**Guarding the serving side: cache, do not refuse.** A friend pulling too often
is the real load, and the button does not create it — `GET
/madnetwork/v0/graph` has always been there. The answer is the one `ownSnapshot`
already gives for catalogs: **memoize the digest** for
`Intervals.GraphDigestTTL` and serve the memo to everyone. An extra pull then
costs a mutex and a map read, so the fast caller gets a cheap yes rather than an
error.

A per-peer cooldown answering 429 was considered and rejected. `syncGraph`
treats every non-200 as "an older peer without the endpoint" and returns, so a
refusal is indistinguishable from absence: a mistuned cooldown would stop two
honest nodes converging with nothing anywhere to say why. Refusals are how a
network quietly stops working, and this one would buy nothing a cache does not.

The rest of the surface is already bounded, and stating it is what makes the
cache sufficient rather than optimistic: both endpoints are friends-only,
`rateAdmits` accepts one record per origin per `Intervals.GraphAccept`,
`MaxOriginsPerBranch` caps what one friend may introduce, and the per-record
bounds cap a single document. The attacker set is people we deliberately let in
and can block — this is buggy-friend protection, not anonymous denial of
service.

`POST /graph/fetch` is the one surface a cache cannot cover, since every caller
asks for a different set and it is the only one serving bulk payload. If it ever
needs bounding, the tool is the token bucket already carrying `seed_rate_kib`
over the blob write path, not a cooldown.

**One thing only a real mesh showed: the button silently did nothing.**
`rateAdmits` refuses a second record per origin inside `Intervals.GraphAccept`,
which is exactly the set an admin presses Rescan for — while the UI reported
success. `ResyncGraph` now clears that map first: the limit bounds what a peer
may *push* at us unsolicited, and a local permission-gated act is not that. The
toast reports the change in node count rather than claiming a refresh, so an
honest "nothing moved" is distinguishable from a refresh that did not happen.

### The network map (requirements declared 2026-07-31, **built the same day**)

The map is how an admin *sees* the community, and since the community is
unbounded (`federation-access.md` §The membership rule) the map has to scale by
**showing less at a time**, never by the network being smaller. A node-link
diagram is the right picture and stays — it is what a human reads intuitively
— so what it needs is scale and navigation, not a different metaphor.

- **A view radius, defaulting to 3–4 hops.** Loading the whole component is
  the wrong default once a community is large: the map opens on the
  neighbourhood around us and expands on demand. **This is a rendering setting
  and nothing else** — it never limits who is served, never appears in a
  scope, never reaches the library. Said plainly because a radius that leaks
  into access is exactly the ladder this design threw away: the map's radius is
  about *what an admin looks at*, `share_depth` is about *whom we answer*, and
  the two must never be the same number.
- **Zoom / scale, not truncation.** Pulling back shows more of the graph at less
  detail; pushing in resolves names and marks. A farm renders as a visibly dense
  clump — which is *information*, and the reason for not hiding it behind a
  cap.
- **Find any node, and any branch.** Search by key, by address, by name (a name
  is hearsay, so results carry the key), and by branch — "everything that
  arrived through this friend" — which is the unit blocking actually operates
  on.
- **Show all connections between two nodes.** Given two keys, render the paths
  joining them. This answers the question an admin actually has when something
  looks wrong: *how is this node connected to me, and through whom* — the same
  question a block is the answer to.
- **Reachable from the library.** The madnetwork library's ⓘ expansion lists a
  track's holders; each holder links into the map, positioned and selected, with
  the block action to hand. Discovery of a bad actor starts from the content
  that exposed it, not from an admin remembering to go look at a diagram.

**What it is, concretely** (F7 item 7, `federation/mapview.go` — pure
functions over an already-built `NetworkMap`, tested without a mesh):

- `GET /api/admin/federation/graph?radius=N` trims to the view (`TrimMap`;
  default 3, `radius=0` = the whole component) and reports `shown`/`hidden`.
  `radius` keeps meaning the component's true reach, so the map can say there is
  more out there. **Nodes we hold a peer row for are never trimmed away** — a
  pending pairing or a blocked key is a decision of ours, not a rendering
  casualty.
- `GET …/graph/find?q=` searches the **whole** component (`FindNodes`: key,
  mesh address, name; `matched` says which answered, and a name hit is labelled
  hearsay) — a search that could only reach the drawn part would make the
  radius a cost rather than a convenience. `?branch=<key>` (`BranchNodes`) lists
  everything that arrived through one direct friend.
- `GET …/graph/paths?from=&to=` (`Paths`) is breadth-first and bounded by
  `MaxPathResults`/`MaxPathLength`, so a cyclic graph cannot produce
  exponentially many and the truncation drops the *longest* rather than an
  arbitrary set; `from` defaults to this node, and truncation is reported.
- The **zoom resolves names** by counting how many nodes share the *frame*, not
  how many exist. Counting the whole graph — which it did first — means a
  large community can never resolve at all, since no reachable zoom level
  changes a total.
- The library's holder links are `/admin/network#node=<key>`, gated client-side
  on `federation.manage`; the page selects, centres, and expands the radius when
  the node sits outside it.


## Related

- [`federation.md`](federation.md) — the spine: goal, vocabulary, transport,
  catalog, availability, build plan.
- [`federation-access.md`](federation-access.md) — what the graph is then
  allowed to decide: membership, scope, audience.
- [`federation-swarm.md`](federation-swarm.md) — where branch weighting lands
  on a fetch, and what a blocked branch stops being served.
- `docs/ui/madnetwork-page.md` — the browse surface holder attribution links
  into.
