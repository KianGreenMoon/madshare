# Player, Queue & Shuffle — Listening UI Behavior

The behavior reference for the playback layer: the persistent player, the play
queue, shuffle/repeat semantics, and how state survives navigation and reloads.
The persistent shell this rides on is described in `docs/ui/shells.md`.

This is a **cross-client contract**, not just a description of the web UI. A
second client (`docs/ui/madplayer.md`) implements these semantics; where the two
disagree, the queue a person shares between their devices stops making sense.
The queue index arithmetic is the one piece that is still client-side —
`webui/static/js/queue-ops.js`, pure and unit-tested, is the reference
implementation of the rules below.

## The pieces

| Piece | File | Role |
|-------|------|------|
| Player core | `webui/static/js/player.js` | Owns the `<audio>` element and the player-bar DOM (play/pause, seek, volume, shuffle/repeat buttons). No queue knowledge. |
| Controller | `webui/static/js/player-controller.js` | **ES-module singleton** (`getController()`), created by `shell.js`. Owns the queue, current index, shuffle/undo/persistence, Media Session, auth-error probing. |
| Queue math | `webui/static/js/queue-ops.js` | Pure, DOM-free index/permutation helpers. Tested: `node --test tests/js/queue-ops.test.mjs`. |
| Queue panel | `webui/static/js/queue-panel.js` | The editable queue list opened from the player-bar queue button. |
| Shell | `webui/static/js/shell.js` | Client router. Everything outside `<main>` (header, player bar, `<audio>`, queue panel) survives page swaps, so playback is continuous across the listening pages (`/library`, `/playlists`, `/upload`, `/madnetwork*`, `/settings`). |

Admin pages are **outside** this system: they are full-load pages with their own
page-local preview player and no queue UI.

## What a queue track is

One shape covers both catalogs, which is what lets a queue hold local and remote
tracks at once:

| Field | Meaning |
|---|---|
| `url` | what to play. Library: `/files/<hash>/<name>`, the recording's ladder-best rendition, resolved server-side. Remote: `/api/madnetwork/stream/<hash>`, the cache-through relay — **unless** the chosen version is held by this node, which plays its direct `/files/` URL and skips the relay hop |
| `tagsetId` | the local **appearance** id, or null for a remote-only track |
| `remoteLike` | `{hash, title, artist, album}` for a remote-only track, else null — the display text captured at add time, so an item survives its source going away |
| `rowKey` | which row this track *is*, for highlighting and pause-vs-restart |
| `title` / `artist` / `dur` | display text; `artist` is the track's performer |

**Two identities, deliberately distinct.** `rowKey` answers *"is this the row I
am looking at?"* and `trackKey` answers *"what does the heart toggle?"*:

- `rowKey` — `ts:<tagset_id>` for a library appearance, `url:<url>` when there is
  no tagset, and on the network page a text triple of `artist␟album␟title`
  (merged catalog rows have no ids to share). Clicking the row whose `rowKey`
  matches the current track toggles pause; clicking any other row — *including a
  different appearance of the same audio* — starts fresh.
- `trackKey` (`favorites.js`) — `ts:<tagset_id>`, else `mn:<remoteLike.hash>`.
  This is the like key, and it is what the favourites cache, the row hearts and
  the player-bar heart all agree on.

## The queue

- **Stable by default.** Browsing never changes the queue. It changes only when
  the user clicks a track (`setQueue` — the clicked view becomes the queue) or
  explicitly edits it (queue panel, quick-add menus).
- **Manual edits mark the queue "dirty".** Reorder, remove, *Add to queue*,
  *Play next* — all set a dirty flag.
- **Replace-with-undo.** Clicking a track always replaces the queue (uniform
  behavior), but if the queue was dirty, the previous queue (including its
  un-shuffled original order) is stashed and a toast offers
  **"Undo — restore my queue"**.
- **Edits in the panel:** click row = play, hover **×** = remove, drag or
  Ctrl/Alt+Arrow = reorder, **Clear**, **Save as playlist…** (POSTs the queue's
  tagset ids to `/api/playlists`). The panel auto-closes on clicks outside it (the
  player bar is exempt, so play/pause/seek don't dismiss it).

## Shuffle — reorders the queue itself

Shuffle is **not** "pick a random next track". Toggling it transforms the queue:

- **On:** the current order is snapshotted as the *original order*; the
  currently playing track is pinned to position 1 and the rest are
  Fisher-Yates-shuffled behind it. The queue panel shows this real play order;
  **Next/Prev/track-end simply walk the visible queue**.
- **Off:** the original order is restored, with the current track remaining
  current (playback is never interrupted in either direction — only the order
  changes).
- **Setting a new queue while shuffle is on** (clicking a track in the library)
  shuffles the new queue immediately, clicked track first.
- **Edits while shuffled stay consistent with the original order:** enqueue /
  play-next / remove are mirrored into it (inserts land right after the current
  track's original position — a shuffled position has no exact counterpart);
  panel drag-reorders are temporary-order edits and do **not** touch the
  original.
- **Prev has no play history**: it steps back through the visible (shuffled)
  order, not through what actually played across reshuffles. Known limitation;
  a history stack is a possible follow-up.

## Repeat

Three modes on the player-bar button: **off / all / one** (badge "1"). Repeat
affects what happens when a track *ends*: `one` replays it (suppressed when the
track *errored*, so a broken file can't loop forever), `all` wraps from the last
track to the first, `off` stops at the end of the queue. Manual **Next/Prev
always wrap** regardless of repeat.

## Audio quality (renditions)

When the current track's recording has **more than one rendition** (the same
audio in different encodings — see
[`../architecture/recordings.md`](../architecture/recordings.md)), a quality
dropdown appears on the player bar: **Auto** (the quality ladder's best) plus
each rendition, best-to-worst. On track change `player-controller.js` fetches
`GET /api/tagsets/{id}/renditions` (best-effort; a single-rendition track leaves
the control hidden); picking an option swaps the audio source **in place**,
preserving the playback position and play/pause state. Delivery is plain HTTP
range requests over `/files/*` — no transcoding or segmenting.

## Persistence & resume

The queue persists to `localStorage` (`madshare-queue`): visible order, current
index, dirty flag, **the original (un-shuffled) order, the shuffle state, and
the playback position within the current track**. It is written on every queue
change, on pause (exact position), every ~5 s while playing (a heartbeat —
`timeupdate` itself is far too chatty for localStorage), and on `pagehide`.
On the next load:

- The queue and position are restored **paused** — the player points at the
  track with `preload=none`, so nothing is fetched (and a stale session can't
  pop the login modal) until the user presses play.
- **Pressing play resumes mid-track**: the saved position can't be seeked
  before any data exists, so it is applied the moment the track's metadata
  arrives (`loadedmetadata`) after the play gesture. Explicitly clicking a
  track row instead starts it from the beginning, as usual.
- The shuffle button is re-lit if shuffle was on, and toggling it off still
  restores the true original order (object identity between the two revived
  arrays is re-linked by URL, duplicate-aware — `relinkTracks`).

localStorage only — per-browser, no cross-device sync (a deliberate v1 decision).

## Failure handling

- A media error on the current track marks its rows unavailable and advances.
- An `<audio>` error doesn't expose the HTTP status, so the controller probes
  the track URL with `Range: bytes=0-0`: a **401/403 means the session
  expired** → the login modal opens instead of silently skipping.

## OS integration

The controller owns the Media Session API: lock-screen / notification metadata
(title/artist), play/pause state, and hardware media keys for
play/pause/next/prev — all driven by the same `goNext`/`goPrev` paths as the
on-screen buttons (so they honor the shuffled order too).

## Favorites & quick-add (how they feed the queue)

- Hearts (library rows, network rows, search rows, the player-bar Like button for
  the current track) toggle membership in the per-user **Favorites** playlist;
  all hearts share one liked-set cache (`favorites.js`) keyed by `trackKey`, so
  they never disagree. **The inline heart is the one favourites control** — there
  is no "Like" item in the ⋯ menu, deliberately; Favorites still appears in the
  *Add to playlist…* submenu like any other playlist, marked ♥.
- Row "⋯" menus on artist/album/track rows offer *Play next*, *Add to queue*
  (both mark the queue dirty) and *Add to playlist…*, plus one trailing item that
  depends on the page: **Download** (save to device) in the library,
  **Materialize** (fetch into this server's library) on the network page.
  Album/artist menus collect their tracks from the browse endpoints first.
- The `/playlists` page plays through the same controller: *Play all* /
  play-from-row call `setQueue` (trashed entries are excluded from the queue
  and rendered grayed "— in Trash").

Endpoint reference: `docs/api/playlists.md`.

## Remote (madnetwork) tracks in the queue

A queue may mix local and remote tracks freely, and everything above applies to
both. The differences are all at the edges:

- **Playing** a remote track streams through `GET /api/madnetwork/stream/{hash}`,
  which serves bytes while the swarm fetch is still running and is seek-aware —
  a seek into the tail fetches that chunk rather than waiting out the prefix. To
  the player it is an ordinary Range-servable URL.
- **Liking and playlisting** are by hash: `POST /api/favorites/remote/{hash}`,
  and playlist adds send a `remote: [{hash, title, artist, album}]` array beside
  `tagset_ids`. The queue panel's *Save as playlist…* splits the queue into those
  two payloads by whether a track has a `tagsetId`.
- **A one-time warning** is shown the first time a remote item is saved: *"Not in
  the local library — may become unavailable."* It is honest and it is once —
  repeating it per row would train people to dismiss it.
- **Unavailable rows are shown, never dropped.** The playlists page marks remote
  rows with a badge and dims the ones the server reports `available: false`. When
  the same audio later lands locally, the server re-points the item and the badge
  quietly disappears (`RepointRemotePlaylistItems`) — the user does nothing.

Availability filtering on the *browse* is a different thing and happens only at
refresh boundaries: `docs/ui/madnetwork-page.md` §Availability.
