# Playlists, Favorites & Queue Editing

Status: **step 1 (backend) implemented** on `aidev` (2026-06-10) — migration 015,
`database/playlists.go`, `/api/playlists` + `/api/favorites` endpoints, Go tests.
Steps 2–4 (queue editing UI, `/playlists` page, favorites/quick-add) pending.
Branch: aidev
Builds on: `docs/plans/persistent-shell-playback.md` (the shell-owned queue and
the `PlayerController`'s reserved mutation API — this phase is the payoff that
queue model was designed for).
Roadmap: **Phase 5** — the final phase of the UI roadmap (Phases 1–4 done).

## Why

The shell gives every listening page one persistent, playlist-ready queue — but
nothing persists and nothing is editable. Users can't keep a set of tracks, mark
favorites, or even see/reorder what's coming up next. This phase adds:

1. **Playlists** — named, per-user, server-persisted track lists.
2. **Favorites** — *just another playlist* (a per-user system playlist) with a
   one-tap **Like** button as its quick-add.
3. **Queue editing** — the current queue becomes a first-class temporary
   playlist: viewable from the player bar, reorderable, savable as a regular
   playlist.

## Decisions (owner, 2026-06-10)

1. **Favorites = a system playlist.** Same table, same item model, `kind =
   'favorites'`, one per user, auto-created lazily, not deletable/renamable. The
   Like button toggles membership. No separate "likes" concept.
2. **Per-user, private-only in v1.** No sharing of any kind yet. The schema adds
   no visibility column now; sharing later is an additive migration. A user can
   only add tracks they can access (server-checked at add time with the same
   visibility rule as the listing endpoints).
3. **Deleted tracks in playlists:**
   - **Trashed** (`files.deleted_at` set): the playlist item **stays**, rendered
     **grayed** — base metadata (title / artist / album) visible, not playable.
     Restore from Trash brings it back to life in place.
   - **Hard-deleted** (row pruned from the DB): the item disappears from the
     playlist (FK `ON DELETE CASCADE`) — no metadata exists to show. *Documented
     intent:* if the server ever retains metadata past hard delete, playlist
     entries should then stay visible (grayed) instead of vanishing.
4. **The current queue is a temporary "current" playlist.** Editable exactly
   like a regular playlist (reorder / remove / clear), with its own menu, and
   savable as a new regular playlist. It persists in **localStorage only**
   (survives reload in the same browser; no server writes while listening, no
   cross-device sync — revisit if sharing/sync ever lands).
5. **Replace-with-undo for the dirty queue.** Default click behavior is
   unchanged: clicking a track in an album makes the album the queue. If the
   user has *manually edited* the queue, a track click still replaces it — but a
   toast offers **"Undo — restore my queue"** (restores the previous
   tracks + position). Uniform behavior, no silent loss.
6. **New shell-native `/playlists` page** — the third listening page (library,
   upload, playlists), inside the persistent shell so playback continues while
   managing playlists.
7. **Quick-add everywhere:** a player-bar button opens the current queue; the
   library rows can add a **song, album, or whole artist** to the queue (and to
   playlists/favorites) without replacing what's playing.
8. **The player bar gets its own Like button** (owner, 2026-06-10): a heart in
   the player bar toggles favorites for the **currently playing** track, synced
   with the row hearts. It lives in shell chrome (the player-bar partial), so it
   works on every listening page regardless of which view built the queue.

## Goals

1. Server-persisted per-user playlists: create, rename, delete, append, remove,
   reorder, play.
2. Favorites with one-tap Like from the library rows and the player bar.
3. Queue panel: see/edit what's playing next from any listening page; save the
   queue as a playlist; queue survives a reload (localStorage).
4. Controller mutation API (`enqueue` / `insertAt` / `removeAt` / `move` /
   `clear` + `'queuechange'`) — implemented for real (it was designed in
   Phase 1, reserved for this phase).

## Non-goals

- No sharing, collaboration, or public playlists (v1 is private-only).
- No smart/auto playlists (recently played, most played) — future candidates.
- No cross-device queue sync (localStorage only — Decision §4).
- cmus stays out (paused view, unchanged).
- Admin pages: untouched (out of the shell; no playlist affordances).

---

## Data model (migration `015_playlists.sql`)

```sql
CREATE TABLE playlists (
  id         INTEGER PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name       TEXT    NOT NULL,
  kind       TEXT    NOT NULL DEFAULT 'regular',   -- 'regular' | 'favorites'
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX idx_playlists_user ON playlists(user_id);
-- exactly one favorites playlist per user
CREATE UNIQUE INDEX idx_playlists_favorites ON playlists(user_id) WHERE kind = 'favorites';

CREATE TABLE playlist_items (
  id          INTEGER PRIMARY KEY,
  playlist_id INTEGER NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
  file_id     INTEGER NOT NULL REFERENCES files(id)     ON DELETE CASCADE,
  position    INTEGER NOT NULL,
  added_at    INTEGER NOT NULL
);
CREATE INDEX idx_playlist_items_list ON playlist_items(playlist_id, position);
```

- **Items reference `files.id`** (not hash): trashed files still have a row, so
  trashed items resolve to grayed metadata; a hard delete cascades the item away
  — both halves of Decision §3 fall out of the schema for free.
- **Duplicates are allowed** in regular playlists (a list may repeat a track) —
  hence item-level `id` for removal/reorder addressing. Favorites are deduped in
  the handler (Like is a toggle), not by schema.
- `position` is a plain integer ordering; reorder rewrites the list's positions
  in one transaction. Compaction of holes (after cascaded deletes) happens on
  the next reorder/write; reads just `ORDER BY position`.
- Remember the migration gotchas: bumping the latest migration breaks the
  `database_test.go` latest-version assertions, and new Repository methods break
  the `api` package's `fakeRepo`.

## API

All endpoints require an authenticated **user** (sessions/tokens) with
`content.access`; playlists are scoped to the identity's user id. Guests have no
user row → no playlists (404/403 on the whole group). Another user's playlist id
→ **404** (not 403 — don't leak existence).

| Action | Endpoint | Body / params | Notes |
|--------|----------|---------------|-------|
| List my playlists | `GET /api/playlists` | — | `[{id, name, kind, track_count, updated_at}]`; lazily creates the favorites row. |
| Create | `POST /api/playlists` | `{name, hashes?: []}` | optional initial items — this is the "save queue as playlist" path. |
| Detail | `GET /api/playlists/{id}` | — | items in order: `{item_id, hash, url, title, artist, album, duration?, status: "ok"\|"trashed"}`. |
| Rename | `PATCH /api/playlists/{id}` | `{name}` | favorites: 403 (not renamable). |
| Delete | `DELETE /api/playlists/{id}` | — | favorites: 403 (not deletable). |
| Append items | `POST /api/playlists/{id}/items` | `{hashes: []}` | server validates each hash: exists, **not trashed**, visible to the user; favorites deduped. |
| Remove item | `DELETE /api/playlists/{id}/items/{itemId}` | — | by item id (duplicates possible). |
| Reorder | `PUT /api/playlists/{id}/items` | `{item_ids: []}` | full ordering; must be a permutation of the list's current item ids. |
| Like toggle | `POST /api/favorites/{hash}` | — | toggles membership in the favorites playlist; returns `{liked: bool}`. |
| Liked set | `GET /api/favorites` | — | `{hashes: []}` — for painting hearts on rows cheaply. |

- **Add-time access check** uses the same file-visibility rule as the listing
  endpoints (and `/files/*` playback enforcement remains the real gate — access
  revoked later simply makes the track fail to play / appear per the browse
  rules).
- Adding an **album or artist** is client-driven: the UI already gets the track
  list (with hashes/urls) from the browse endpoints and posts the hashes — no
  server-side "expand entity" endpoint in v1.

## Frontend

### Player controller (`player-controller.js`)

- Implement the reserved mutation API for real: `enqueue(tracks)`,
  `insertAt(i, tracks)`, `removeAt(i)`, `move(from, to)`, `clear()`, firing
  `'queuechange'` (the index follows the currently-playing track through
  mutations).
- **Dirty flag:** any manual mutation marks the queue dirty; `setQueue` clears
  it. On `setQueue` over a dirty queue, the previous `{tracks, index}` is stashed
  and a toast offers **Undo** (Decision §5).
- **localStorage resume:** persist `{tracks, index}` (throttled, on
  `queuechange`/`'trackchange'`) under a `madshare-queue` key; on shell boot,
  restore **paused**. The existing auth-expiry probe already covers stale URLs.

### Queue panel (shell chrome, all listening pages)

- New **queue button** in the player bar opens a slide-up panel over the current
  page: the queue as a track list with the playing row highlighted; per-row
  remove and drag (or up/down) reorder; **Clear** and **Save as playlist…**
  (name prompt → `POST /api/playlists` with the queue's hashes) in the panel's
  menu. Lives in shell chrome (outside `<main>`), so it survives swaps like the
  player bar itself.

### `/playlists` page (new, shell-native)

- `webui/html/playlists.html` + `webui/static/js/playlists.js`
  (`{init, teardown}` contract, `<body data-page="playlists" data-module=…>`),
  nav link in the header (visible to logged-in users with `content.access`).
- List view (name, kind badge for Favorites, count, updated) → detail view:
  play-all (`controller.setQueue`), play-from-row, rename, delete, remove items,
  reorder; **trashed rows grayed** — metadata shown, click does nothing
  (tooltip "in Trash").
- Reuse library row styles/CSS where possible (per the reuse-over-rewrite rule).

### Library page (`app.js`)

- **Heart** on track rows (painted from `GET /api/favorites`, toggled via
  `POST /api/favorites/{hash}`). The player-bar heart (Decision §8) is shell
  chrome — it follows `'trackchange'` and shares the same liked-set cache so
  row hearts and the player heart never disagree.
- Row-level **add menu** (track / album / artist rows): *Play next* (insertAt
  after current), *Add to queue* (enqueue), *Add to playlist…* (picker over
  `GET /api/playlists` + "New playlist…"), *Like* (track rows). Album/artist
  variants fetch the entity's tracks from the browse endpoints first.
- Default behavior unchanged: clicking a track still `setQueue`s the album
  (with the Undo toast when the queue was dirty).

---

## Phasing

Each step leaves the app working; static checks (`go build ./...`,
`go test ./...`, `node --check`) per step, owner browser-verifies per step.

1. **Backend.** Migration 015, `database/playlists.go` (queries + Repository
   methods), playlist + favorites endpoints, Go tests (ownership/404, access
   check, favorites dedupe + toggle, reorder validation, trashed item status,
   rename/delete guards on favorites). Fix the known test fallout (migration
   assertions, `fakeRepo`).
2. **Queue editing (no backend dependency — parallelizable with 1).**
   Controller mutations + `'queuechange'` + dirty flag + replace-undo toast +
   localStorage resume; queue panel + player-bar button. The queue-logic module
   stays DOM-free (unit-testable).
3. **`/playlists` page** + "Save as playlist" wiring (needs 1 + 2).
4. **Favorites & quick-add.** Hearts (rows + player bar), row add-menus
   incl. album/artist quick-add and the add-to-playlist picker.

## File-by-file (anticipated)

**New**
- `database/migrations/015_playlists.sql`
- `database/playlists.go` — playlist/items/favorites queries
- `api/playlists.go` — handlers (registered in `RegisterAPI`, gated
  `content.access` + user identity)
- `webui/html/playlists.html`, `webui/static/js/playlists.js`
- queue-panel markup/CSS (player-bar partial + `player.css`, or a small
  dedicated file if it grows)

**Changed**
- `webui/static/js/player-controller.js` — mutations, `'queuechange'`, dirty
  flag + undo stash, localStorage persistence
- `webui/html/partials.html` — player-bar queue button + heart; Playlists nav
  link
- `webui/static/js/app.js` — hearts, row add-menus
- `webui/webui.go` — `/playlists` route (full-page render, shell wiring)
- `api` Repository interface + `fakeRepo`, `database_test.go` migration
  assertions

## Testing

- **Go:** endpoint matrix above (step 1); especially: foreign playlist id → 404,
  adding an inaccessible/trashed hash → rejected, favorites Like idempotence,
  reorder with a non-permutation → 400, trashed item surfaces
  `status:"trashed"`, hard delete (prune) removes items via cascade.
- **JS:** queue mutation logic (index tracking through insert/remove/move,
  dirty/undo) — DOM-free unit tests per the Phase 1 consideration.
- **Browser checklist:** queue panel edit while playing (no audio glitch);
  reload → queue restored paused; replace + Undo restores tracks and position;
  save-as-playlist → appears on `/playlists`; play-all from a playlist; Like
  from row and player bar stays in sync; trashed track grays out in playlists
  and back to playable after Trash-restore; `/playlists` deep link + refresh +
  back/forward inside the shell; admin pages unaffected.
- webui assets are compile-time embedded — rebuild/restart to see changes.

## Open questions

- **Queue panel UX detail** (slide-up vs side drawer, drag vs buttons on
  mobile) — decide with a designer pass at implementation time.
- **Hearts on browse rows at scale:** `GET /api/favorites` returns the full
  liked-hash set; fine for thousands, revisit alongside the existing
  server-side-pagination open question if libraries get huge.
- **Playlist export/import** (M3U) — cheap and friendly, not in v1 scope.
