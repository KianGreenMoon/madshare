# Admin Files Rework — Entity-Aware Library Management

Status: **planned** (design; no code yet)
Branch: aidev
Builds on: `docs/plans/admin-panel-rework.md` (the admin shell + the existing
`/admin/files` page) and the artist/album normalization backend
(`docs/plans/artist-album-normalization.md`, complete).
Roadmap: **Phase 4** — independent of the persistent shell, parallelizable.

## Why

`/admin/files` today is **file-centric**: a flat table of files with per-row
metadata edit (`PATCH /api/files/{hash}/metadata`), two-step delete, access
controls (guest toggle + license), and a page-local preview player
(`webui/static/js/admin/files.js`). That's good for "fix one track," but the
library is organized as **artist → album → track** entities now, and two shipped
backend capabilities have **no UI at all**:

- **Rename** an artist/album as a whole (tracks + cover follow the entity).
- **Merge** duplicate artist/album spellings together (destructive).

So an admin can't currently fix "Beatles" vs "The Beatles", or rename an album,
without per-track tag surgery — and per-track edits *reclassify* a single track
rather than moving the whole entity. The rework makes the page **entity-aware**:
manage artists and albums as first-class objects (rename, merge, delete,
drill-in), with the existing per-file editing underneath.

This is a **front-end / UI rework**. The endpoints all exist; **no backend
change** (confirm only that the admin role has `metadata.edit`).

## Existing backend (all live, gated `metadata.edit`)

From `api/api.go` and `docs/api/metadata.md`:

| Action | Endpoint | Body | Notes |
|--------|----------|------|-------|
| Edit one track's tags | `PATCH /api/files/{hash}/metadata` | `{title?, artist?, album?, album_artist?}` | pointer semantics: absent = unchanged, `""` = clear. **Reclassifies that one track** between groupings. |
| Rename artist | `POST /api/artists/{artist}/rename` | `{name}` | Whole entity; tracks + cover follow. |
| Rename album | `POST /api/albums/{album}/rename?artist=<artist>` | `{title}` | Whole entity; cover stays attached. |
| Merge artists | `POST /api/artists/{artist}/merge` | `{into}` | **Destructive**: folds source into target, deletes source. Target cover wins. |
| Merge albums | `POST /api/albums/{album}/merge?artist=<artist>` | `{into_artist, into_album}` | **Destructive**: repoints tracks, deletes source album. |
| Delete file (→ trash) | `DELETE /api/admin/files/{hash}` | — | existing two-step delete. |
| Guest / license | `POST /api/admin/files/{hash}/{guest,license}` | — | existing access controls. |

**Cover semantics (important for the UI copy):** since the Phase-4 cover re-key,
covers are keyed by the album/artist **entity id**, so an **entity rename keeps
the cover attached**. A **per-track tag edit** still reclassifies just that track
(its cover stays with the original album). The page should steer admins to the
**rename** action for whole-album/artist renames and reserve per-track edits for
genuinely mis-tagged single tracks. (This supersedes the old cover-orphan note in
admin-panel-rework.md Decision §1.)

## Goals

1. Surface **artists and albums as first-class rows** with **rename**, **merge**,
   and **delete** actions — not only the flat per-file table.
2. Keep all current per-file capabilities: metadata edit, two-step delete, guest
   toggle, license, and the **page-local preview player** (no change — it already
   matches the "admin stays out of the shell, uses its own preview player"
   decision in `persistent-shell-playback.md`).
3. Make destructive merges safe: explicit confirm with a clear "this deletes the
   source and moves N tracks into <target>" summary.
4. Disambiguate **rename-the-entity** vs. **re-tag-one-track** in the UI so admins
   stop using per-track edits to rename albums.

## Non-goals

- No backend changes (endpoints exist; confirm `metadata.edit` on admin role).
- Not part of the persistent shell — `/admin/*` remains separate full-load pages
  with the page-local preview player.

## Cover editing (now in scope — implemented)

Originally deferred; pulled in by request. The backend already shipped the
endpoints (no backend change needed), so this is UI-only:

- **Album cover:** `POST /api/albums/{album}/image?artist=<artist>` — multipart
  field `image`, JPEG/PNG only, ≤ 10 MB. Replaces the current cover ("explicit
  beats embedded") and enqueues async variant generation.
- **Artist cover:** `POST /api/artists/{artist}/image` — same multipart contract.
  **No variant pipeline** (flat `<base_key><ext>` object key); the responsive
  variants exist for albums only.
- **Display:** the browse DTOs already carry `has_image`; `GET …/image` serves the
  original directly, so an entity-row thumbnail updates immediately on upload
  (album variants only feed the public responsive UI). A `cb=` cache-bust forces
  a replaced cover to refetch.
- Artist/album rows show a 40px cover thumbnail (or a ♪ placeholder) and an
  **Add cover / Cover…** action (gated `metadata.edit`); the empty-name bucket is
  not uploadable (addressed by name, like rename).
- **Still out of scope:** cover *removal/clear*, cropping, and a processing/“variants
  ready” progress indicator (the thumbnail uses the original, so it’s not needed).

---

## Target UX

An **entity-oriented browse** of the library for admins, mirroring the public
drill-down but with management affordances:

```
/admin/files
  ┌ view toggle:  [ By entity ▾ ]  [ All files ]      filter: ____   preview ▶
  │
  │  By entity (default):  Artists ─▶ Albums ─▶ Tracks   (drill-down)
  │    Artist row:   name · N albums · M tracks   [Rename] [Merge…] [Delete…]
  │    Album row:    title · artist · K tracks · cover✓   [Rename] [Merge…] [Delete…]
  │    Track row:    title · #/dur · guest? · license     [Edit] [Play] [Delete]
  │
  │  All files (today's flat table) stays available as a fallback view for
  │  "find this one hash" / bulk-ish work.
```

- **Rename** (artist/album): inline edit or small modal → `POST …/rename`. Copy:
  "Renames the whole <album>; its cover and all tracks stay attached."
- **Merge** (artist/album): pick a **target** entity (typeahead over existing
  names) → confirm modal showing "Move N tracks from <source> into <target> and
  delete <source>. The target's cover is kept." → `POST …/merge`. Destructive →
  two-step / typed confirm.
- **Delete**: entity delete = delete its files (→ trash) — reuse the existing
  file-delete path per track, or an entity-level convenience that trashes the
  set; decide at impl (see open questions). Per-track delete is unchanged.
- **Edit (track)**: the existing per-track metadata form, with an inline hint
  pointing at **Rename** when the user is really trying to rename a whole
  album/artist.
- **Preview**: the existing page-local player — play any track while editing.
  Unchanged.

### Addressing entities: by name vs. by id (OPEN)

The rename/merge endpoints address entities by **current name**
(`/api/artists/{artist}/…`, `?artist=`), and the listing DTOs
(`artistItem`/`albumItem`) are **name-only** today. That's workable, but names
containing `/` and the empty-string bucket are fragile. Two paths:

- **(a) Use names as-is** (no backend change). Encode carefully; handle the
  empty-name bucket explicitly. Fastest.
- **(b) Surface entity `id` in the listing DTOs** and add `?artist_id=` /
  `?album_id=` browse params, then drive the UI by id. More robust, small backend
  addition. The normalization plan flagged this as "decide in the UI rework."

**Recommendation:** start with **(a)** for rename (low risk, single entity) but do
**(b)** before wiring **merge**, since merge is destructive and must target an
unambiguous entity. Confirm at impl.

---

## File-by-file (anticipated)

**Changed**
- `webui/html/admin/files.html` — add the entity/all-files view toggle and the
  entity drill-down container; keep the existing table as the "All files" view.
- `webui/static/js/admin/files.js` — add entity browse (artists/albums/tracks),
  rename/merge/delete actions + confirm modals; keep the preview player, metadata
  edit, access controls. Likely split helpers into the page or `admin/shared.js`
  if it grows large.
- `webui/static/css/admin-files.css` — entity rows, merge/confirm modal styling.
- `docs/plans/admin-panel-rework.md` — flip the "Future UI work" section pointer
  to "addressed by docs/plans/admin-files-rework.md".

**Possibly changed (only if Decision = id-based, path (b))**
- `api/*` listing handlers/DTOs — add entity `id` to `artistItem`/`albumItem` and
  `?artist_id=`/`?album_id=` browse params. (This is the one place this phase
  *might* touch the backend; keep it additive/non-breaking.)

**Backend behavior: none** otherwise.

---

## Testing

- Rename artist/album → name changes everywhere, **cover stays attached**, tracks
  follow; reload persists.
- Merge artist/album → source's tracks land on target, **source deleted**, target
  cover preserved (source cover only fills a gap); confirm modal shows the right
  N/target; destructive guard works.
- Per-track edit still reclassifies a single track (and the "use Rename for whole
  albums" hint shows).
- Delete (entity + per-track) → files go to trash; restore via the existing trash
  page still works.
- Preview player: play/pause/seek any track while editing (page-local, no shell).
- Permission matrix: `metadata.edit`-less admin sees no rename/merge/edit;
  server still 403s. Names with `/` and the empty-name bucket addressable (per the
  id-vs-name decision).
- Remember: webui assets are **compile-time embedded** — rebuild/restart.

## Decisions

1. **Entity-aware default view** (artists/albums/tracks drill-down) with the
   existing flat **All files** table kept as a fallback.
2. **Page-local preview player stays** — admin is out of the persistent shell.
3. **Cover editing is in scope** (implemented; see "Cover editing" above) —
   artist/album cover upload via the existing `POST …/image` endpoints. Cover
   *removal*, cropping, and variant-progress UI remain out of scope.
4. **Merge is destructive → typed/two-step confirm** with an N-tracks/target
   summary.

## Open questions

- **Name vs. id addressing** (path a/b above) — recommend id-based before merge.
- **Entity delete semantics** — "delete album" = trash all its files (loop the
  existing per-file delete) vs. a new entity-level convenience. Lean: reuse the
  per-file path (no new endpoint) and just batch it client-side.
- Whether the **All files** flat view is still worth keeping once the entity view
  lands, or becomes a pure "search by hash" utility.
- **Scaling the browse endpoints (very large libraries).** The UI now loads
  lazily — only `/api/artists` on entry, then `/api/albums?artist=` /
  `/api/tracks?…` per drill-in, and the full `/api/files` list only on first need
  (All-files tab or an entity-view track Edit). But each browse endpoint still
  returns the **entire** list for its level in one response, and the filter boxes
  are **client-side**. Fine for hundreds–low-thousands; a library with tens of
  thousands of artists/tracks would want **server-side pagination + search** on
  `/api/artists|albums|tracks` (and `/api/files`). That's a backend change, out of
  scope for this UI slice — noted so it isn't forgotten if the library grows.
