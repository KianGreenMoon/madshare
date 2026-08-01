# Madnetwork Page — Library Parity & Availability

The `/madnetwork` page grows from a bespoke read-only drill-down into a full
sibling of the library page (`/`): same row anatomy, same actions (hearts, "⋯"
quick-add menus, playlists), same search behavior, shared code. On top of the
parity work it gains two madnetwork-specific behaviors: **materialize** (the
renamed download-to-library flow) and **availability** (tracks held only by a
friend who is currently unreachable drop out of the view at the next refresh).

This is the **UI** design. The server-side mechanics referenced here are
documented with the backend:

- Catalog merge, the availability predicate, node-health/liveness, the swarm:
  `docs/architecture/federation.md` (§Catalog, §Distribution, §Availability &
  node health).
- Remote playlist/favorite items (migration 029, API surface, re-pointing):
  `docs/api/playlists.md` §"Remote (madnetwork) items".
- Queue semantics: `docs/ui/player-and-queue.md`.

## Principles

- **One browse core, two data sources.** The library and madnetwork pages render
  through the same shared components; only the data adapter differs. No parallel
  re-implementations of rows, menus, or search.
- **"Materialize" is the word.** Everywhere the UI copies remote content into
  this server's library it says *Materialize* — button labels, menu items,
  progress, toasts. "Download" now exclusively means *save the file to the
  user's device* (a library action).
- **The view shows what is reachable — and degrades quietly.** A row is backed by
  our own library, by a complete local cache, or by a **reachable** friend.
  Tracks held only by an unreachable friend are dropped, but only at a **refresh
  boundary** (page load or a new search), never live under the cursor — so the
  list is stable while you use it and a transient blip can't strobe it.
- **Remote entries in playlists are first-class but honestly labeled.** They
  play (streamed relay) and survive in playlists, but carry a warning that they
  are not local and may become unavailable.
- **Nothing is hidden; things are ordered** (decided 2026-07-26). Weak or
  unverifiable structure is ranked down and labeled, never suppressed. Someone
  hunting a rare record is a normal user of this page, and a rule that protects
  the artist list by deleting the long tail from it has destroyed the thing the
  network is for. The only content the page withholds is content nobody
  reachable can serve (§Availability), which is a fact about the world rather
  than a judgement about a claim.

## Shared browse core

Extract the library page's presentation layer out of `app.js` into shared
modules consumed by both pages:

| Piece | Contents |
|-------|----------|
| `browse-rows.js` | `buildArtistRow` / `buildAlbumRow` / `buildTrackRow` (num, playing icon, title/meta, heart, ⋯, duration, chevron), disc headers, the playing-row highlight helpers. |
| `quick-add.js` | `quickAddItems` (Play next / Add to queue / Add to playlist…), `addToPlaylistMenu`, `queueAdd` — parameterized so a source can append trailing items (Materialize). |
| `browse-search.js` | The debounced search-view machinery (input wiring, artist/album/track result sections, view switching). |

Each page passes a **source adapter**:

```js
{
  artists({ q, cursor }),   // → { items, next_cursor }  (madnetwork: no cursor yet)
  albums(artistRef),        // library ref = ids; madnetwork ref = names
  tracks(albumRef),
  search(q),                // → { artists, albums, tracks }
  trackObj(row),            // → controller track {url, key, title, artist, dur}
  extraTrackItems(track),   // trailing ⋯ items (library: Download; mn: Materialize)
  extraEntityItems(ref),    // trailing ⋯ items (mn: Materialize all)
}
```

The library page keeps its virtualized artist scroller; the madnetwork artist
list renders whole for now (the merged catalog is small; virtualization can
adopt the same `createVirtualList` later without API changes). The
madnetwork-only ⓘ source/versions panel stays, appended by the madnetwork
page after the shared track row is built.

**Track identity for hearts/queues** is a single string key used by
`favorites.js` and the playing-row highlight:

- local appearance: `ts:<tagset_id>` (unchanged semantics)
- remote track: `mn:<hash>` — the default version's ladder-best rendition hash.

## Row actions

**Favorites live on the row, not in the menu.** The inline heart button is the
one favorites control on track rows, on both pages (madnetwork hearts keyed
`mn:<hash>`). The library's redundant "Add to Favorites" ⋯ item is dropped along
with the extraction.

**Library track ⋯ menu** (order): Play next · Add to queue · Add to playlist… ·
**Download** (last). Download is a plain browser save of the track's resolved
rendition file — an `<a download>` click on the same-origin `/files/...` URL;
no new endpoint. Track rows only (no album/artist zip in v0).

**Madnetwork track ⋯ menu** (order): Play next · Add to queue · Add to
playlist… · **Materialize** (last). The inline "⬇ Download" pill on the row is
removed; the ⓘ panel's per-version action row renames its Download to Materialize
(per-version materialize targets that version's best rendition). Materialize items
render only for users holding `file.upload` (server enforcement unchanged) and are
omitted for tracks that are already local.

**Madnetwork artist & album ⋯ menus**: Play next · Add to queue · Add to
playlist… · **Materialize all** (last). Additionally the album *tracks view*
shows a visible **Materialize all** button in the breadcrumb bar (the
album-level bulk action shouldn't hide in a menu), permission-gated like the
menu items.

*Materialize all* iterates the entity's tracks, submitting
`POST /api/madnetwork/download` for each default-version best rendition, skipping
tracks already local, with a persistent page-level progress line (`#mnBulk` —
survives panel re-render) and one aggregate completion toast ("Materializing
7 tracks… 3 done, 1 already local"). Submissions are **sequential** (the server
dedupes per hash and swarm-fetches in parallel internally; the UI must not hammer
the endpoint). Only one bulk run at a time.

## Remote tracks in favorites & playlists

Remote madnetwork tracks can be liked and added to playlists. The schema,
API surface, `available` flag, and re-pointing rules are backend concerns —
see `docs/api/playlists.md` §"Remote (madnetwork) items". UI behavior:

- Adding a remote track to a playlist or to favorites shows a **one-time toast**:
  *"Not in the local library — may become unavailable."*
- The playlists page renders remote rows with a small **"remote" badge**; rows
  the server reports `available: false` are **dimmed** with that same warning as
  their tooltip. Playback and queueing otherwise work exactly like local rows
  (the streaming relay is the URL).
- Queue tracks carry a `remoteLike` meta flag so the player-bar heart and the
  queue-panel "save" split the local (`ts:`) and remote (`mn:`) payloads
  correctly.
- When a remote track is later materialized and approved, its playlist/favorite
  rows are re-pointed to the new local appearance server-side and quietly lose
  the badge on the next load — the user does nothing.

## Own tracks in the view

The madnetwork browse merges **this node's own published library** into the
artist/album/track merge, so the page answers "what does the mesh see" and stays
useful even with no friends online. The merge is a server-side UNION (see
`docs/architecture/federation.md` §Catalog). What the UI shows:

- Own tracks appear as a holder named after `[federation].name`, flagged
  `self: true` in the ⓘ panel ("this server").
- Play/queue on an own track uses the direct local `/files/...` URL (no relay
  hop through our own cache).
- Materialize is omitted for own/already-local tracks; *Materialize all* skips
  them.
- Own tracks are **always available** — they never depend on anyone's liveness.

## Availability

> Replaces the reverted **10-second presence** feature (phase 4). The old design
> ran a 5 s prober with a 10 s hysteresis and mutated the list live; it was
> unstable on a real mesh and backed out in full. The corrected model — slow,
> passive liveness; hide only at refresh boundaries; never fail dark — is
> designed backend-side in `docs/architecture/federation.md` §Availability & node
> health. This section is the UI half.

The server decides, **at request time**, which tracks are available (held by a
reachable friend, or local, or fully cached) and returns only those in the
browse/search responses. The page's job is simply to **render what the server
sent** — there is no client-side presence logic, no background poll, and no live
mutation of the list.

**Refresh boundaries.** The available set is re-evaluated only when the client
re-fetches, which happens on exactly two user actions:

1. **Page (re)load / navigating into `/madnetwork`** — a fresh browse fetch.
2. **A new search** — `/api/madnetwork/search` returns the currently-available
   matches.

Between those, the list is frozen: a friend dropping off mid-scroll does **not**
make rows vanish under the cursor; their exclusively-held tracks simply won't be
there after the next reload or search. This is the deliberate trade the owner
chose — avoid a "big pile of dead links" (unavailable tracks are hidden, not
shown-then-broken) *without* the live flapping that sank the 10 s rule.

**What is never hidden.** Local, fully-cached, and this node's own tracks always
render regardless of anyone's reachability — a transient disconnect can never
blank content you can actually play. Only tracks whose *only* holders are
unreachable friends drop out.

**Reachability display.** The sync-status strip lists nodes with a **"last
seen"** indicator; one outside its freshness window is greyed. The ⓘ panel's
holder list shows the same — an unreachable holder is greyed rather than removed,
so the provenance stays legible.

Freshness is minutes-wide, and **how many minutes depends on the node** (F7 item
10; `federation.md` §Availability, "Two clocks, two windows"). A direct friend is
pinged every minute, so three minutes of silence greys it. A member reached only
by the catalog rotation is on a fifteen-minute clock, so its window is
correspondingly wider — judging it by the friend's window is what hid most of the
community's library for most of every quarter hour. Nothing about this is visible
in the UI beyond the greying being *right*: the server sends a verdict, never the
arithmetic behind it.

**Empty states.** With own tracks merged in, the list truly empties only when
there is nothing to show at all. The "no friends yet" onboarding message stays
for the no-peers case; federation disabled keeps today's behavior (no
`/madnetwork` link, page gated).

## Search

Madnetwork search behaves exactly like the library's: one input, 2+ chars,
300 ms debounce, a search view with **Artists / Albums / Tracks** sections
(shared `browse-search.js`), Escape/✕ to clear, hit = drill or play. The endpoint
`GET /api/madnetwork/search?q=` mirrors `/api/search`'s shape over the merged,
availability-filtered catalog. The old artists-only filter box is retired.

## Sorting

Alphabetical (case-insensitive) everywhere, with the **unknown buckets last** —
"Unknown artist" after all artists, "Other" after all albums — matching the local
library's existing order. Tracks order by disc, track number, then title. The
ordering is applied server-side (same shape as `database/library.go`); remote
catalogs carry no normalized ids, so the bucket match is best-effort on the
canonical default strings.

## Planned — ranking rare and unverifiable structure

Today the hierarchy is built *out of* text: `GROUP BY lower(akey), lower(alb)`
over `federation_catalog`. Tags do not describe the structure, they **are** the
structure. The local library can afford that because tags are moderated on
ingest; the network cannot, because the text arrives from strangers. One
mistagged album is enough to put a record under the wrong artist, and a node
with a badly tagged library — or a hostile one — reshapes the artist list.

The principle to build on: **structure that came from audio is evidence,
structure that came from text is a claim.** Anchor the page on the verifiable
part; let the claims decorate it.

**Mistake and attack are the same signal, deliberately.** A troll's fabricated
album and an honest typo both produce *a claim nobody else makes*. Keying the
remedy on that rather than on intent means it needs no judgement, no urgency and
no blocking to work — and it treats a clumsy friend exactly like an enemy, which
is right, because their effect on the artist list is identical.

### The ordering signal

Two inputs, both already part of the design's vocabulary:

- **Corroboration, counted per branch, never per node.** Nodes reachable only
  through one friend are **one voice** (federation.md §Trust graph, the sybil
  rule). A farm of fifty nodes behind a single friendship edge corroborates
  nothing, and dies with one snip.
- **Trust distance of the nearest holder**, which cuts the other way and is why
  it is worth having: a **direct friend is somebody this admin deliberately
  added**, so a claim only they make is credible rather than suspect. Distant
  *and* uncorroborated is where the tail belongs.

Together they are a **sort key, not a filter**. Corroborated structure leads;
everything else follows, labeled.

### Label it "rare", because that is what it usually is

The tail is presented as **"rare album" / "rare artist"**, not as anything
suspicious. It is honest — one holder three hops out *is* rare — and it is
useful, because sometimes rare is exactly what the user came for. The same
number that keeps a flood from outranking real music also powers a genuinely
good badge. A holder count belongs next to it.

**Rarity is relative, and marked only where it is news** (decided 2026-07-26).
If everything in view is rare, nothing is: the badge means *rarer than what
surrounds it*, so the rule is

> mark an item when its corroboration is materially below its context's — never
> otherwise, and never inherited downward.

A rare artist's albums are not each labeled rare; their rarity is already
explained by the artist's, and repeating it on every descendant turns the badge
into wallpaper. Same for a rare album's tracks. But an unusual track *inside an
ordinary album* is exactly worth marking, because there the rarity is news about
that track rather than about its parent — it is rare **for this album**, which is
what the badge should say.

Two things fall out of the one rule, which is why it is worth stating this way:

- **Small networks mark nothing.** With three friends almost everything is
  single-source, so nothing stands out from its context and no badge appears.
  A small network is not a suspicious one, and it needs no special case.
- **Rarity inside a rare parent still shows** when it is genuinely uneven — three
  albums held by one node under an artist whose fourth is held by five is a real
  difference, and the rule catches it without another clause.

*Implementation consequence:* "is rare" is a property of a row **in the list it
appears in**, not of the row itself, so it cannot be precomputed or cached per
entry. The browse already builds each list server-side per request, which is
where the sibling distribution is available.

### Album identity: overlap, not a hash

An album identifier hashed from its recordings is brittle in the ordinary case,
never mind the adversarial one: deluxe editions, bonus tracks, regional
pressings and a one-track holding versus a ten-track one all yield different
hashes for what everyone calls one album. Identity should **emerge from recording
overlap** — two album claims sharing recordings, with similar text, are probably
one album — which degrades into "probably related" where a hash simply fails.

The anchor is per-recording identity: the content hash (identical bytes) today,
and the **fingerprint claim once it is on the catalog wire** (federation.md F6,
added there for contradicted-claim reports). Fingerprints matter more than hashes
here: a hash only matches when one node fetched the bytes from the other, while a
fingerprint matches two independent rips of the same recording — so album linkage
works between libraries that never exchanged a byte.

**No MusicBrainz** (decided 2026-07-26). Release ids would make album identity
exact where present, but it is a separate, optional external service and nothing
in the hierarchy may depend on it.

### Reporting trash

A user who finds garbage reports it from the row; the report reaches the admin
with the **source node or nodes** attached, next to a Block action — the same
inbox as the contradicted-claim reports (federation.md §Trust graph), and
manual in exactly the same way. Nothing about a peer's service changes because a
user complained; blocking stays a decision a person makes.

### Browsing a single node

Wanted, lower priority: a view of one node's shared library on its own — "show
me what *this* friend has". Useful in its own right, and it is where a node's
offering is complete, uncorroborated entries included, so it doubles as the
place a curious user or an admin goes to judge a source after a report. It does
**not** carry the ranking above: within one node's shelf there is nothing to
corroborate against, so its own catalog order is the right order.

### Conflicting placements are not a special case

The same recording filed under different artists or albums by different nodes is
usually **not corruption at all**: a track credited "A feat. B" legitimately
belongs under A for one tagger and under B for another, and the local model
already declines to settle this — the library artist list is album-artist only,
and multi-credit "feat." parsing is deferred (`docs/architecture/artist-album-model.md`).
The network view must not try to solve upstream of the local library.

So placements get the same treatment as everything else on this page rather than
a vote (decided 2026-07-26): weight them, lead with the best-corroborated one,
and **show every other placement that is also well corroborated** — two popular
placements are two legitimate answers, not a winner and a loser. A placement only
one branch asserts still appears, marked rare like anything else. One rule for
the whole page, no majority-takes-all step to explain.

### Open

- **Volume from a single honest branch** — likely answer agreed 2026-07-26, not
  yet specified. Branch weighting answers the sybil farm but not one friend with
  fifty thousand badly tagged albums: still one voice, still fifty thousand rows.
  Cluster a branch's low-corroboration entries together rather than interleaving
  them through the whole tail, so one source occupies one region of the list
  instead of every other line of it.
- **Cost.** The signal is another aggregate over `federation_catalog` alongside
  the grouping already there. The branch count needs the friend graph, which F6
  built: `federation.BuildNetworkMap` already resolves a key to the branches it
  is reachable through, so this can read branches rather than degrade to a
  per-peer count.
- **What "materially below" means numerically** — a ratio against the sibling
  median, a fixed gap, or a quantile. Deliberately unspecified until there is a
  real catalog to look at; picking a constant now would be picking it blind.

## Discovery — the page was a phone book, and F7 made that worse

*(Raised by the owner 2026-07-31: "it currently just shows all library in
alphabetic order." **Built the same day** — this section is what shipped, not a
plan. The API surface is `GET /api/madnetwork/discover` (every lane's eight-row
digest in one round trip, empty lanes omitted) and `GET
/api/madnetwork/lane?name=&offset=` ("See all"), with `?source=<id|self>` on
`artists`/`albums`/`tracks`/`search` for a single node's shelf.)*

The page today is one thing: an A→Z drill-down over the merged catalogs. That was
right while the merged set was a handful of friends' libraries, and it stops being
right the moment F7 lands, because **the community's whole published output**
arrives behind the same alphabet.

The reason it fails is not size, it is the question being asked. **On your own
library you browse, because you already know what is in it — you are navigating
memory. On the network you have no memory to navigate**, so an alphabet is the
one ordering that helps least: it answers "show me everything, in an order
unrelated to anything I care about". Nobody looks for music by scrolling to the
letter R. The local page and the network page have looked alike since the parity
work, and this is the place where the parity was the mistake.

### What replaces it

**The drill-down is demoted, not deleted.** It becomes one lane called *Browse
all*, keeping its current behaviour exactly — including the rare/corroboration
ordering above, which is about *trust*, not discovery, and is orthogonal to
everything here. What changes is that it stops being the landing view.

The landing view is a small set of **lanes**, each answering a question a person
actually arrives with:

| lane | the question | cost |
|---|---|---|
| **Not in your library** | *what can I get that I don't have?* | ~free — the self-merge already tags rows with a `self` holder (§Own tracks in the view); this lane is the rows without one |
| **New on the network** | *what appeared since I last looked?* | a `first_seen` column on `federation_catalog` — it is a cache table, so a migration there costs nothing |
| **Most held** | *what does my community actually have?* | the holder count already computed for version ordering, **branch-weighted** (one branch = one voice) |
| **Rare finds** | *what is nearly gone?* | the same corroboration signal, read from the other end — the badge designed above becomes a destination |
| **From your friends** | *what do the people I chose personally have?* | direct friends only: the smallest, highest-trust slice, and the one lane that needs no trust arithmetic at all |
| **By node** | *what does that node have?* | §Browsing a single node, already sketched |

**Search is promoted to the primary affordance**, not a filter on a browse tree.
On a network you *find*; the search box belongs at the top of the landing view
with the lanes underneath it, which is the opposite of the library page's
arrangement and correct for the opposite reason.

### The rules these lanes must obey

- **Every lane is computed from what THIS node can see** — its own synced
  catalogs, its own friend graph, its own branch weighting. There is no
  network-global chart, no shared counter, nothing to game from outside, and
  nothing that could grow into the automatic reputation score the design refuses
  (federation.md §Trust graph). "Most held" means *most held among nodes I can
  see*, and the UI should say so rather than imply a chart.
- **Lanes rank, they never hide.** Same rule as the rarity ordering: the long tail
  is reachable through *Browse all* and through search, always. A lane is a
  shortcut, never a gate.
- **Availability applies unchanged** (§Availability). A lane is computed over the
  available set at request time, so an unreachable friend's exclusives drop out of
  the lanes exactly as they drop out of the drill-down.
- **Nothing here is a recommendation.** No inferred taste, no "because you
  listened to", no profile. Every lane is a plain fact about the catalog, and a
  person can see why a row is in one.

### Scale stops being optional

"Virtualized/paginated madnetwork artist list (adopt when catalogs grow)" has sat
in *Out of scope* since the parity work. **F7 is the growth it was waiting for** —
the community's whole output, not a few friends' — so the windowing already built
for the admin lenses (`virtual-list.js`) has to come to *Browse all* in the same
phase, along with keyset paging on the artist query. A lane is naturally bounded
(a screenful) and needs none of it; the alphabet is not, and does.

### Settled (2026-07-31)

The three questions this section left open are answered; the answers are the
build's specification.

**A lane is eight rows and a "See all", not a card shelf.** Each lane renders as
a heading plus up to eight ordinary track rows — the same `browse-rows.js` rows
the drill-down uses, so hearts, the ⋯ menu, Materialize and the ⓘ versions panel
work inside a lane with no new component and no second code path. The deciding
argument is the one thing the merged catalog does not have: **cover images**. A
horizontal shelf is a row of artwork, and a row of artwork with no artwork is ten
grey squares. "See all" opens the lane full-page, in the same rank order, without
the cap below.

**`first_seen` is per `(source, entry)`, carried across the atomic replace, and
aggregated as a minimum.** The column lives on `federation_catalog`; a sync reads
the existing values for that source before the replace and re-stamps the rows that
survive, so re-advertising an unchanged entry never makes it new again. A logical
track's date is the **earliest** date any source showed it to us, so a track we
already know from A does not resurface as new when B also starts offering it. The
label is *new to us*: a node reached last week publishing a record from 1974 is
new here, and the page says "new on the network", never "new music".

**"From your friends" becomes "From your direct friends".** federation.md §Goal &
vocabulary settled the word the same day: *community* is the whole connected
component, and **direct friend** is the node an admin friended by hand. The lane
means the second, so it uses the second's name in full. It is the only lane whose
membership an admin controls personally, which is exactly why it exists.

### Lane definitions

Each lane is one ranking over the merged, availability-filtered set. `holders` is
the number of distinct **nodes** offering a logical track; `branches` is the
number of distinct direct friends those nodes are reachable through
(federation.md §Trust graph — one branch is one voice).

| lane | shown when | ranked by |
|---|---|---|
| `missing` — **Not in your library** | no self row in the group | `holders` desc |
| `new` — **New on the network** | some source has a `first_seen` | `first_seen` desc |
| `held` — **Most held here** | always | `branches` desc, then `holders` desc |
| `rare` — **Only one node has it** | `holders = 1` and no self row | that node's `last_seen` desc |
| `friends` — **From your direct friends** | some holder is a direct friend | friend `holders` desc |
| `nodes` — **By node** | — | not a track lane; see below |

`held` is the only lane needing the friend graph. It is ranked in two steps —
SQL takes the top candidates by `holders`, then the branch weighting re-sorts
them — and the two-step is exact rather than approximate, because `branches ≤
holders` always holds, so the top *K* by holders necessarily contains the top *K*
by branches. With no graph (federation off, nothing gossiped yet) it degrades to
one source = one voice, which is the same rule with a smaller world.

`rare` needs no branch arithmetic at all: one holder is one branch, whatever the
graph says. Ordering it by the holder's `last_seen` puts the rarities that are
**fetchable right now** first, which is the only thing that distinguishes them
from each other.

**The per-source cap, and where it does not apply.** On the landing view `new` and
`rare` cap how much any one node may contribute, filling the lane round-robin
across sources: a node reached for the first time makes its entire library new to
us at once, and without a cap that one node owns the lane until something newer
happens — which on a quiet community is days. The quota adapts to how many nodes
are in the candidate set (`ceil(limit / sources)`, at least one) and the lane
still fills from the remainder in rank order if the quota cannot fill it, so a
one-node network sees no difference. **"See all" is never capped**, which is the
"lanes rank, they never hide" rule kept literally: the cap is a property of the
eight-row digest, not of the ranking — and for the same reason *whether there is
more* is decided on the ranking, before any capping runs.

`missing`, `held` and `friends` are *not* capped. Their rankings are corroboration
counts, so volume from a single node cannot lift a row in them at all — a cap
there would only mean showing worse answers.

### Browsing a single node

The **By node** lane lists the nodes whose catalogs this node holds — the same set
as the status strip, whose chips are the other entrance — and opening one enters
*Browse all* restricted to that node, this node's own library included as a node
like any other. It carries no ranking: within one shelf there is nothing to
corroborate against, so its own order is the right order (§Browsing a single node
above). This is also where an admin looks at a source after a report, so it
deliberately shows the node's offering complete, uncorroborated entries and all.

A node's shelf **never folds our own set in** — browsing a node means seeing what
that node offers, and we are a different node. The corollary caught a real bug:
asking for OUR shelf on a node that publishes nothing to the network has to
answer with nothing, and answering with the merged catalog instead is the one
answer that is certainly wrong. The view's `includeRemote`/`includeOwn` pair
therefore admits a view that includes *neither* half, backed by a well-typed
empty row source rather than a special case at each call site.

## Out of scope

- Per-content share scope and transfer tokens (federation F5).
- Remote cover art (the catalog carries no images; note placeholder stays).
- Album/artist "Download as zip" on the library page.
- Recommendations of any kind — see the rules above; this is a permanent
  exclusion, not a deferral.

## Build order

1. **Shared browse core** — `browse-rows.js` / `quick-add.js` /
   `browse-search.js` extracted from `app.js`; library page re-wired onto them
   with the redundant Favorites ⋯ item dropped (heart is the one control).
   *(shipped)*
2. **Madnetwork parity** — madnetwork page on the shared core: hearts, ⋯ menus,
   library-style search (`/api/madnetwork/search`), unknown-last sorting,
   Materialize rename, library ⋯ Download item, own-tracks merge (`self` holder,
   local `url`, `tagset_id`). *(shipped)*
3. **Remote playlists/likes** — migration 029 (nullable `tagset_id` XOR
   `remote_hash`), playlists/favorites API extensions, canonical `ts:`/`mn:` like
   keys, warning text + `remote` badge/dimming, re-pointing on every approval
   path + startup. *(shipped)*
4. **Materialize all** — per-entity bulk submit (sequential, skip-local) with a
   persistent progress line, artist/album ⋯ items + a visible tracks-view button,
   aggregate completion toast. *(shipped)*
5. **Availability** — the reworked replacement for the reverted 10 s presence.
   Backend: netstack hardening → slow/passive `last_seen` → request-time
   availability predicate + self-health watchdog (fail open). UI: render the
   server's available set; hide unavailable only at page-load / search
   boundaries; grey (don't remove) unreachable holders; local/cached/own always
   shown. *(shipped — phases 0–3; config knob + real-mesh verification are the
   remaining phase 4. See `docs/architecture/federation.md` §Availability & node
   health and `docs/plans/availability.md`.)*
6. **Discovery** — the landing view above: lanes over the merged catalog, search
   promoted to the top, the A→Z drill-down demoted to *Browse all* and windowed
   with `virtual-list.js` + keyset paging. Needs `first_seen` on
   `federation_catalog` (a cache-table migration) and the branch weighting from
   federation.md §Trust graph; everything else reads aggregates the page already
   computes. *(**shipped 2026-07-31** as F7 item 8 — §Settled, §Lane
   definitions, §Browsing a single node. Migration 037, `GET
   /api/madnetwork/discover` + `/lane`, `?source=` on the browse endpoints, a
   keyset-paged + windowed Browse all, and By node with it.)*
7. **Ranking rare and unverifiable structure** — the corroboration sort key and
   the "rare" badge (§Planned — ranking rare…). Independent of 6 and orthogonal
   to it: 6 decides *what a person is shown first*, 7 decides *what order claims
   appear in once shown*. *(planned)*
