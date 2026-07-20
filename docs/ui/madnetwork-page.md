# Madnetwork Page — Library Parity & Presence

The `/madnetwork` page grows from a bespoke read-only drill-down into a full
sibling of the library page (`/`): same row anatomy, same actions (hearts, "⋯"
quick-add menus, playlists), same search behavior, shared code. On top of the
parity work it gains two madnetwork-specific behaviors: **materialize** (the
renamed download-to-library flow) and **presence** (friends that drop off the
mesh take their tracks with them — fast).

Related docs: `docs/architecture/federation.md` (F2 catalog, F3 transfer, F4
swarm), `docs/ui/player-and-queue.md` (queue semantics), `docs/api/playlists.md`.

## Principles

- **One browse core, two data sources.** The library and madnetwork pages render
  through the same shared components; only the data adapter differs. No parallel
  re-implementations of rows, menus, or search.
- **"Materialize" is the word.** Everywhere the UI copies remote content into
  this server's library it says *Materialize* — button labels, menu items,
  progress, toasts. "Download" now exclusively means *save the file to the
  user's device* (a library action).
- **The madnetwork view shows what is reachable.** Rows are backed by an online
  holder, by our own library, or by a complete local cache — never by a friend
  who is currently gone.
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

**Track identity for hearts/queues** becomes a single string key used by
`favorites.js` and the playing-row highlight:

- local appearance: `ts:<tagset_id>` (unchanged semantics)
- remote track: `mn:<hash>` — the default version's ladder-best rendition hash.

## Row actions

**Favorites live on the row, not in the menu.** The inline heart button is the
one favorites control on track rows, on both pages (madnetwork hearts keyed
`mn:<hash>`). The library's current redundant "Add to Favorites" ⋯ item is
dropped along with the extraction.

**Library track ⋯ menu** (order): Play next · Add to queue · Add to playlist… ·
**Download** (last). Download is a plain browser save of the track's resolved
rendition file — an `<a download>` click on the same-origin `/files/...` URL;
no new endpoint. Track rows only (no album/artist zip in v0).

**Madnetwork track ⋯ menu** (order): Play next · Add to queue · Add to
playlist… · **Materialize** (last). The inline "⬇ Download"
pill on the row is removed; the ⓘ panel's per-version action row renames its
Download to Materialize (per-version materialize stays — it targets that
version's best rendition). Materialize items render only for users holding
`file.upload` (server enforcement unchanged).

**Madnetwork artist & album ⋯ menus**: Play next · Add to queue · Add to
playlist… · **Materialize all** (last). Additionally the album *tracks view*
shows a visible **Materialize all** button in the breadcrumb bar (the
album-level bulk action shouldn't hide in a menu), permission-gated like the
menu items.

*Materialize all* iterates the entity's tracks, submitting
`POST /api/madnetwork/download` for each default-version best rendition,
skipping tracks already local, with one aggregate progress toast ("Materializing
7 tracks… 3 done, 1 already local"). Submissions are sequential (the server
dedupes per hash and swarm-fetches in parallel internally; the UI must not
hammer the endpoint).

## Remote tracks in favorites & playlists

Playlist items grow a **remote variant** (migration `029_playlist_remote_items.sql`):

- `playlist_items.tagset_id` becomes nullable; new columns `remote_hash TEXT`,
  `remote_title`, `remote_artist`, `remote_album` (display text captured at add
  time — the friend's catalog row may vanish later).
- `CHECK ((tagset_id IS NULL) <> (remote_hash IS NULL))`; per-playlist dedupe on
  `remote_hash` mirrors the tagset dedupe.

API surface:

- `POST /api/playlists/{id}/items` accepts `remote: [{hash, title, artist,
  album}]` alongside `tagset_ids`.
- `GET /api/favorites` returns `tagset_ids` plus `remote` entries;
  `POST /api/favorites/remote/{hash}` (body = display text) toggles a remote
  like. Favorites remain the same system playlist underneath.
- Playlist/favorites listings return remote rows with `kind: "remote"`, a play
  URL of `/api/madnetwork/stream/{hash}`, and `available: bool` — true when the
  hash is local, fully cached, or held by an online friend.

UI behavior:

- Adding a remote track to a playlist or favorites shows a one-time toast:
  *"Not in the local library — may become unavailable."*
- The playlists page renders remote rows with a small "remote" badge; rows with
  `available: false` are dimmed with the same warning as their tooltip. Playback
  and queueing otherwise work exactly like local rows (the streaming relay is
  the URL).

**Re-pointing on materialize:** when a blob lands **approved** in the library,
all `playlist_items` rows whose `remote_hash` matches a rendition of the
approved recording are re-pointed to the approved appearance's tagset
(`tagset_id` set, `remote_*` cleared) — the playlist entry silently becomes a
normal local entry. A read-time fallback resolves any rows the write-time hook
missed (e.g. blobs that arrived by other means). Likes migrate the same way
(favorites are a playlist).

## Own tracks in the view

The madnetwork browse merges **this node's own published snapshot** (the same
`visibleTagset` catalog served to friends) into the artist/album/track merge,
as a holder named after `[federation].name` and flagged `self: true`:

- Play/queue on a track whose best rendition is local uses the local resolved
  URL (no relay hop through our own cache).
- Materialize is omitted for tracks that are already local (nothing to do);
  *Materialize all* skips them.
- The ⓘ panel lists us among holders ("this server").

Rationale: the page answers "what does the mesh see", and our own library is
part of the mesh. It also keeps the page useful when no friends are online.

## Presence — the 10-second rule

Friends that drop off the mesh disappear from the browse within ~10 s, and
return only after being demonstrably back for ~10 s (hysteresis, so a flapping
link doesn't strobe the list).

**Server (federation node):** a dedicated presence prober alongside the 1-min
refresh loop — every **5 s** ping all friends in parallel (3 s timeout, the
existing `/madnetwork/v0/ping`). Per-friend state:

- `online → offline`: no successful ping for **10 s**.
- `offline → online`: first success starts a probation window; the friend flips
  online only after staying reachable for **10 s** (i.e. the next probe also
  succeeds). Successful pings keep feeding `last_seen` as today.

Presence is in-memory node state (not persisted); on startup everyone begins
offline and earns online status through probation. Mesh pings are tiny (a GET
inside an established session), so N friends at 5 s cadence is negligible.

**Visibility rule** (applied server-side in the madnetwork browse queries and
`/api/madnetwork/search`): a rendition is *visible* iff

1. an **online** friend holds it (catalog ∪ holdings), or
2. it is in the **local library**, or
3. it is **fully cached** (complete file in `<data_dir>/cache/madnetwork/`,
   no `.part`) — a cached track is fully playable regardless of who is online.

A version is visible if any of its renditions is; a track if any version is;
albums/artists and their counts are computed post-filter. Offline friends also
drop out of the ⓘ holder lists and the status strip shows them greyed.

**Client:** while `/madnetwork` is active, poll `/api/madnetwork/summary`
every 5 s (existing endpoint + per-friend `online` flag). When the online-set
fingerprint changes, re-fetch the current panel (drill level is preserved; a
vanished album/artist falls back one level with a toast). Polling stops on
teardown.

**Future outlook — presence beyond direct friends (F5+):** the 5 s prober does
not scale to a transitive friend-of-friend network, and it doesn't have to.
Madnetwork rides Yggdrasil, so *any* node whose key we learn (from a
depth-shared catalog) is directly addressable — the friendship chain governs
trust and authorization, not connectivity. The plan for deeper networks is
therefore two-tier: **gossiped liveness hints** piggybacking on catalog sync
(each node shares its direct friends' `last_seen` — cheap, coarse,
per-hop-stale, and only a claim) for browse-level filtering, plus **direct
on-demand probing** of exactly the holders currently on screen (one mesh RTT to
the holder itself — proof, not hearsay) for the accurate 10 s-grade presence.
Probing the visible working set keeps cost O(what you're looking at) instead of
O(network). No chain-relayed ping-forwarding is ever needed.

**Empty states:** with own tracks merged in, the list truly empties only when
there is nothing to show at all. The "no friends yet" onboarding message stays
for the no-peers case; federation disabled keeps today's behavior (no
`/madnetwork` link, page gated). Transient disconnects never blank the page —
own + cached content remains.

## Search

Madnetwork search behaves exactly like the library's: one input, 2+ chars,
300 ms debounce, a search view with **Artists / Albums / Tracks** sections
(shared `browse-search.js`), Escape/✕ to clear, hit = drill or play. New
endpoint `GET /api/madnetwork/search?q=` mirrors `/api/search`'s shape over the
merged (presence-filtered) catalog. The old artists-only filter box is retired.

## Sorting

Alphabetical (case-insensitive) everywhere, with the **unknown buckets last**:

- Artists: the "Unknown artist" bucket sorts after everything else, then
  `lower(name)`. Remote catalogs carry no normalized ids, so the bucket match is
  on the canonical default strings (best-effort for foreign-language peers).
- Albums: "Other" last, then `year IS NULL, year, lower(title)` (matching the
  library's album order).
- Tracks: unchanged (disc, track number, title).

This matches the local library's existing server-side order (`norm_name`
last-bucket trick in `database/library.go`); the madnetwork queries adopt the
same shape.

## Out of scope

- Per-content share scope and transfer tokens (federation F5).
- Remote cover art (the catalog carries no images; note placeholder stays).
- Virtualized/paginated madnetwork artist list (adopt when catalogs grow).
- Album/artist "Download as zip" on the library page.

## Build order

1. **Shared browse core** — extract `browse-rows.js` / `quick-add.js` /
   `browse-search.js` from `app.js`; library page re-wired onto them with zero
   behavior change (the regression gate for the extraction).
2. **Madnetwork parity** — madnetwork page onto the shared core: hearts, ⋯
   menus, library-style search (`/api/madnetwork/search`), unknown-last sorting,
   Materialize rename, library ⋯ Download item, own-tracks merge.
3. **Remote playlists/likes** — migration 029, playlists/favorites API
   extensions, warning text/badges, re-point on approval.
4. **Presence** — 5 s prober + hysteresis, presence-filtered browse/search,
   summary `online` flags, client polling + panel refresh.
5. **Materialize all** — per-entity bulk submit + aggregate progress.
