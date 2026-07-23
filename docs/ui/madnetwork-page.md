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
