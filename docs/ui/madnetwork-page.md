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

**Reachability display.** The sync-status strip lists friends with a **"last
seen"** indicator; a friend outside the freshness window is greyed. The ⓘ panel's
holder list shows the same — an unreachable holder is greyed rather than removed,
so the provenance stays legible. (Freshness is minutes-wide; see the backend doc.)

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

### Open

- **Volume from a single honest branch.** Branch weighting answers the sybil
  farm but not one friend with fifty thousand badly tagged albums: still one
  voice, still fifty thousand rows in the tail. Clustering a branch's
  uncorroborated entries together, rather than interleaving them through the
  whole tail, is the likely answer.
- **Threshold at small scale.** With three friends almost nothing is
  corroborated, so a fixed "needs two branches" rule would demote the entire
  network. The tiering must relax toward *show everything at equal rank* as the
  graph shrinks, rather than treating a small network as a suspicious one.
- **Conflicting placements** — the same recording filed under different
  artists or albums by different nodes. Reading the "order, never hide"
  principle: the majority placement leads and the alternatives stay reachable
  from the track's expansion, where holders and versions already live. Recorded
  as inference, not yet a decision.
- **Cost.** The signal is another aggregate over `federation_catalog` alongside
  the grouping already there; the branch count needs the F6 friend graph, so
  before F6 it degrades to a per-peer count.

## Out of scope

- Per-content share scope and transfer tokens (federation F5).
- Remote cover art (the catalog carries no images; note placeholder stays).
- Virtualized/paginated madnetwork artist list (adopt when catalogs grow).
- Album/artist "Download as zip" on the library page.

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
