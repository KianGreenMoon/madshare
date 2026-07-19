# Player, Queue & Shuffle — Listening UI Behavior

The behavior reference for the web UI's playback layer: the persistent player,
the play queue, shuffle/repeat semantics, and how state survives navigation and
reloads. The persistent shell this rides on is described in `docs/ui/shells.md`.

## The pieces

| Piece | File | Role |
|-------|------|------|
| Player core | `webui/static/js/player.js` | Owns the `<audio>` element and the player-bar DOM (play/pause, seek, volume, shuffle/repeat buttons). No queue knowledge. |
| Controller | `webui/static/js/player-controller.js` | **ES-module singleton** (`getController()`), created by `shell.js`. Owns the queue, current index, shuffle/undo/persistence, Media Session, auth-error probing. |
| Queue math | `webui/static/js/queue-ops.js` | Pure, DOM-free index/permutation helpers. Tested: `node --test tests/js/queue-ops.test.mjs`. |
| Queue panel | `webui/static/js/queue-panel.js` | The editable queue list opened from the player-bar queue button. |
| Shell | `webui/static/js/shell.js` | Client router. Everything outside `<main>` (header, player bar, `<audio>`, queue panel) survives page swaps, so playback is continuous across the listening pages (`/`, `/playlists`, `/upload`). |

Admin pages are **outside** this system: they are full-load pages with their own
page-local preview player and no queue UI.

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

- Hearts (library rows, search rows, the player-bar Like button for the current
  track) toggle membership in the per-user **Favorites** playlist; all hearts
  share one liked-set cache (`favorites.js`) so they never disagree.
- Row "⋯" menus on artist/album/track rows offer *Play next*, *Add to queue*
  (both mark the queue dirty), *Add to playlist…*, and *Like* on tracks.
  Album/artist menus collect their tracks from the browse endpoints first.
- The `/playlists` page plays through the same controller: *Play all* /
  play-from-row call `setQueue` (trashed entries are excluded from the queue
  and rendered grayed "— in Trash").

Endpoint reference: `docs/api/playlists.md`.
