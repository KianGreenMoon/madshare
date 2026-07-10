# File-Management View — the unified file list

Every place that lists tracks/files and edits their metadata is one component
(`webui/static/js/file-list.js`), parameterised by a **scope**. The admin
**Library** page (`/admin/library`) hosts three scopes behind a client-side
switch — **Full Library · Review · Trash** — sharing one preview player; the
uploader's **My uploads** (on `/upload`) is the same component in owner mode.

The **Full Library** and **Trash** scopes are each a sub-switch of **lenses**
over the same set (live / not-in-library respectively):

| Lens | Full Library | Trash | Grain |
|---|---|---|---|
| By entity | ✓ (drill-down, rename/merge/cover) | – | entity |
| Appearances | ✓ **All Appearances** (`file-list.js`) | ✓ (`file-list.js`) | tagset |
| Recordings | ✓ (`admin/recordings.js`, curation view) | ✓ (`trash-recordings.js`) | recording |
| Files | ✓ (`library-files.js`) | ✓ (`trash-files.js`) | blob |

The Recordings and Files lenses are **bespoke** (the file/recording-grain lists
share the lean `admin/trash-list.js` core; the Full Library Recordings lens is
the recording-centric curation view, recording-tagsets P5, reached at
`/admin/library#recordings`). Each state has exactly one home: live blobs on
Full Library › Files, soft-removed blobs on Trash › Files — there is no
"show removed" toggle.

This is the reference for the shipped system. It unified the former standalone
`/admin/files`, `/admin/moderation`, `/admin/trash`, and `/admin/recordings`
pages into this one scope-driven page.

The **`/admin/duplicates`** page (same-audio recordings with >1 rendition) is a
*separate* page, not a scope here — its rows are renditions grouped by recording
with a different shape (tech compare + quality-ladder rank). See
[`recordings.md`](recordings.md) (P2).

---

## The component

`createFileList(scope)` owns presentation only: rendering, selection, the bulk
toolbar, badges, and the empty/loading/error states.
It imports no page-specific helpers — the scope injects everything domain-
specific (loaders, action handlers, `onPlay`, `toast`, `handleAuthError`), so it
runs unchanged on the admin pages and the shell-native upload page. Built-in
**Play** + **Edit** row actions and an **Edit tags…** bulk action appear
automatically from the scope's capabilities.

Row actions are **icon-only** (`icons.js` glyphs on `.icon-btn`, label carried as
`title` + `aria-label`). Text buttons never fit: a table cell ellipsises its text,
but a button row simply overflows onto the neighbouring column, so the actions
column is sized per table via `--col-actions` (`file-view.css`) to exactly the icon
row it holds. For the same reason a destructive action confirms in the scope's own
**modal** — an inline two-step confirm is wider than the column it lives in.

Presentations: a flat **list** (optionally grouped — by-uploader for Review, state
sections for My uploads, or By artist / album) and an artist→album→track **browse**
(currently used only via the Files By-entity view; see below). **Every list
presentation is virtualized**: the component builds one flat array of items (rows +
separator/header entries) and renders only the on-screen window through the shared
`virtual-list.js` scroller, so it never freezes at scale — group/section headers are
full-width table rows and the paged scopes stream more on scroll (see
[`infinite-scroll-virtualization.md`](infinite-scroll-virtualization.md)). All four
scopes are **server-paged**; under paging the native groupings stream as
**non-collapsible separators** via `section-stream.js` (the single-level sibling of
the artist/album `grouped-stream.js`, fed pages in `sort=uploader` / `sort=state`
order). The classic collapsible-by-uploader view is only used in the (now unused)
non-paged path; Review's paged separators don't collapse.
Styling lives in `webui/static/css/file-view.css`, the single source for the
component's chrome (table, bulk toolbar, badges, confirms, switches, the modal
access/field layout). It is self-sufficient so the upload page — which loads no
admin CSS — renders correctly; the admin pages load it too.

### Shared editors

- **Per-file edit** — `track-edit.js`, with an optional **access** section
  (License picker + Guest toggle) shown when the scope is `accessEditable`.
- **Bulk edit** — `bulk-edit.js`, a selection-wide editor. The fields every
  selected file already shares (artist / album-artist / album + access) are
  **pre-filled** so the editor sees the common value; fields that vary show blank
  with a "multiple values" hint. **Only the fields you change are written** — an
  untouched shared value isn't re-applied, and a blank field is never written (bulk
  never clears). `file-list.js` computes the shared values from its loaded rows
  (`selectionTags`, full-coverage only) and passes them in. **Title is excluded**
  (it is per-track). Setting the album/artist tag on a selection *re-tags* those
  files — it is **not** an entity rename. An **+ Extended edit…** button opens a
  stacked wide modal with the rarely-touched tags (year, track total, disc number,
  genre, composer, comment), reusing track-edit.js's `EXTENDED_FIELDS`. Those tags
  aren't in the list payload, so the first time the modal opens for a selection it
  **lazily fetches** each file's full tags (`loadDetails`, via the scope's detail
  endpoint) to pre-fill their shared values — same change-only / never-clear rule
  as the base fields. **Track number stays excluded** (per-track, like Title). The
  toggle shows a count of how many extended fields the user has changed.

  > **Extended pre-fill cap (`EXT_PREFILL_CAP = 100`).** The extended pre-fill is
  > one detail fetch per selected file, so it's bounded: when more than 100 files
  > are selected, the modal skips the fetch and the extended fields stay set-only
  > (blank, "fields here set what you enter"); you can still bulk-set them, you just
  > don't see the shared starting values. The **base** pre-fill has **no cap** — it
  > is computed from rows already in memory (no fetch), so it always runs.

## The scope catalog

| Scope | Endpoint | Group | Selectable | Access edit | Notes |
|---|---|---|---|---|---|
| **All Appearances** (admin, Full Library) | `GET /api/admin/appearances` (paged) | – (opt.) | all | ✓ (modal + bulk) | **windowed** list of every **live approved appearance**, keyed by `tagset_id` — the live twin of the Trash Appearances lens (both `FROM tagsets`; a blob can host several appearances). Play resolves to the recording's ladder-best rendition (like the listening surfaces); a **dormant** recording's appearance rows badge `dormant` and can't preview. Infinite scroll + server **sort dropdown** (incl. Untagged first); access is a **read-only column**, edited in the modals. Bulk Move to Trash + Edit tags… via `POST …/appearances/bulk` (selection or "select all N matching"). Grouped **By artist / album** via a separate toggle pill (streams in `sort=grouped` order; below). |
| **Review** (admin) | `GET /api/admin/moderation` (paged) | by uploader (streamed separators) | `submitted` | ✗ (tags only) | Approve / Return-with-note / Discard; bulk via `POST …/moderation/bulk` (selection or "select all N matching"); `show()` gates per state (drafts preview-only), `editable()` gates Edit. |
| **Trash** (admin) | `GET /api/admin/trash` (paged) | – | all | ✗ (tags only)¹ | The **Appearances** lens of the three-perspective Trash (`soft-delete.md`): Restore / Delete forever / Edit, bulk via `POST …/trash/bulk` (selection or "select all N matching"); gained Play. |
| **My uploads** (owner, `/upload`) | `GET /api/my/uploads` (paged) | by state (streamed sections) | draft/returned | ✗ (tags only) | state sections; Send to approval / Remove via `POST …/my/uploads/bulk` (selection or "select all N matching"); owner-scoped edit endpoint. |

¹ **Trash edits tags only.** Access lives on the recording and is meaningless on
a trashed appearance (the Trash bulk rejects an access patch). Review is
tags-only by choice (behaviour-preserving); an access section there is a
possible additive follow-up.

**Access is a recording property.** The All Appearances per-row access editor
writes `PATCH /api/admin/recordings/{id}/access` (license + guest in one
request); the bulk edit forwards license/guest through
`POST /api/admin/appearances/bulk` (action `edit`), which resolves each tagset's
recording server-side — license before guest, so an explicit guest wins over
license auto-derive. Tag edits are tagset-addressed
(`PATCH /api/admin/tagsets/{id}/metadata`). Every editable list DTO carries
`album_artist` (the editor writes all four base tags, so an absent prefill would
silently clear it).

### Sort dropdown + grouped-view toggle + needs-metadata flag

Every list view shows one **sort dropdown** (`sortControl`) for the **flat**
orders — `Newest`/`Oldest`, `Title`, `Artist`, `Largest`/`Smallest`, and
**Untagged first** (rows with no artist/album-artist tag — the needs-metadata
rows); non-paged scopes also offer **Default order** (as loaded). All four current
scopes are **paged** and drive the dropdown server-side (the token rides on the
listing's `?sort=`, see [`file-list-scaling.md`](file-list-scaling.md)); the
component still supports non-paged scopes (in-memory sort) for any future caller.
The choice is persisted in `localStorage`.

The **By artist / album** grouped view is a **separate toggle pill** (`groupToggle`,
reusing the `.sort-switch`/`.vm-btn` styling), *not* an entry in the sort dropdown —
grouping is a view mode, not one more order, and folding it in made the dropdown
crowded. It is offered on every `scope.artistAlbumSort` scope. While **any** grouped
view is active — the artist/album toggle, or (on a scope with a native grouping) the
streamed by-uploader / by-state view — the flat sort dropdown is **disabled**, since
the grouped view imposes its own order. On a paged scope turning the toggle on
re-queries with `sort=grouped` and **streams** that order page-by-page
([`infinite-scroll-virtualization.md`](infinite-scroll-virtualization.md)); turning
it off returns to the scope's default view (the flat list, or its native streamed
grouping). The toggle state is persisted (non-paged) in `localStorage`.

In **By artist / album** mode the same `.files-table` is sorted
**album-artist → album → disc# → track# (then title)** with **separator rows** woven
in — a tinted band per artist, a thin indented line per album — each carrying a
**group-select checkbox** wired into the selection Set, so a whole artist/album
can be bulk-edited. Grouping is by `album_artist ?? artist` (a Various-Artists
compilation stays under one band); empty artist/album fall into Unknown / Other
buckets, sorted last. Available on **Review, Trash, and My uploads** (it overrides
a scope's native uploader/state grouping). The sort needs the track + disc
number, so `track_number` and `disc_number` (+ `year`) are on the `/api/files`,
`reviewItem`, and `trashItem` DTOs.

A **multi-disc album** (more than one distinct `disc_number`; an untagged track
counts as disc 1) also gets a quiet **"Disc N" separator row** before each disc —
a further-indented sub-label reusing `grpSepRow`. Single-disc albums are
unaffected. The same multi-disc grouping is applied in the two other track
surfaces that print a per-track number: the library drill-down (`app.js`) and the
admin "By entity" track view (`admin/files.js` `renderTracks`), both fed by
`disc_number` on `GET /api/tracks`. The album track query orders by
`COALESCE(disc_number, 1), track_number, title` (`database/library.go`).

A file with **neither an artist nor an album-artist** tag gets a calm amber
flag (`.fl-needs-meta` — tint + left accent) in every scope and both sort modes,
so moderators / admins / uploaders can see which rows want metadata first.

### Grouped cover button (Add / Edit)

Each grouped separator (artist or album) carries one unobtrusive cover button,
backed by the shared `cover-edit.js` picker (a self-contained JPEG/PNG ≤10 MB
input that POSTs to the artist/album cover endpoints). Which button it shows
depends on whether the entity already has a cover and on two scope flags:

- **Add cover** — when the entity has *no cover yet* and `scope.allowCoverAdd`.
- **Edit cover** — when the entity *already has* a cover and `scope.allowCoverEdit`.

Whether a group already has a cover comes from `artist_has_image` /
`album_has_image` on every list DTO — the grouping key (`album_artist ?? artist`,
album) is exactly the entity those flags join on (`media_metadata.artist_id` /
`album_id` resolve from the same `effectiveArtist`/album), so the representative
row's flag governs the whole group. The button never shows on the Unknown/Other
fallback buckets (no real entity to attach to).

The POST is identical for add and replace; the split is purely about
permission. The cover routes accept **`metadata.edit` OR `file.upload`**
(`RequireAnyPermission`), then the handler enforces add-only for a
`file.upload`-only caller — adding a missing cover is allowed, overwriting one
returns **403** (`coverReplaceBlocked`). So `allowCoverAdd` follows whichever
permission a scope grants (**All Appearances, Review, Trash** via `metadata.edit`; **My
uploads** via every uploader's `file.upload`), while `allowCoverEdit` is set only
where the caller holds `metadata.edit` — so an uploader with just `file.upload`
can dress a coverless staged draft but is never offered **Edit cover** on one
that already has art.

## The Library page (Hybrid nav)

`/admin/library` (`webui/html/admin/library.html` + `library.js`) folds the
former `/admin/files`, `/admin/moderation`, `/admin/trash`, and
`/admin/recordings` pages into one secondary-nav entry. `library.js` boots auth
once, owns the single shared player (injected into each scope/lens as
`play(items, index, highlight)`; next/prev/ended are generic), builds only the
scopes the admin can use (Review needs `content.moderate`, Trash needs
`file.delete`), and swaps panels in place. Each scope is an exported factory —
`createFilesScope` / `createReviewScope` / `createTrashScope` (in
`admin/files.js` / `moderation.js` / `trash.js`) — and all scope/lens modals
coexist in `library.html`. `createFilesScope` is itself the Full Library
coordinator: it builds the four lenses (the Recordings lens only for
`content.moderate` holders — `createRecordingsView` in `admin/recordings.js`,
refactored from the former standalone page into a mountable factory using the
page's shared player) behind the `#libModeSwitch` sub-tabs, mirroring the Trash
sub-switch.

**Hash routing:** `#review` / `#trash` pick a scope (the dashboard cards);
`#recordings` opens Full Library › Recordings and `#recordings-<id>` also
searches + expands that recording — the `#recording →` links on the Files
lenses (both pages) use this form. `library.js` listens on `hashchange`, so an
in-page link switches lenses without a reload.

## Out of scope (the entity axis)

Artist/album **rename** and **merge** operate on entities, not per-file tags.
They stay in the **By-entity** drill-down inside the Full Library scope
(`admin/files.js`, its own renderer over `/api/artists`, `/api/albums?artist=`,
`/api/tracks?…` + the rename/merge/cover/delete endpoints in
`docs/api/metadata.md` and `docs/api/cover-images.md`). The component's own browse
presentation is not used there; folding the entity affordances into it is an
optional future step. (The entity action the list itself carries is the grouped
**Add / Edit cover** button above, gated by `allowCoverAdd` / `allowCoverEdit`.)

## Permissions

The component reads `identity.permissions` and the scope; it adds no new
permission. `metadata.edit` → Edit (tags) + the access section + Edit tags…;
`file.delete` → Move to Trash / Restore / Delete forever; `content.moderate` →
the Review scope + Approve/Return/Discard; `file.upload` → My uploads. A scope or
action the caller can't use is simply not rendered; the API enforces the same
gates server-side.

## See also

- `docs/architecture/file-list-scaling.md` — server-side pagination + filter/sort + the bulk-action endpoint for the flat file listings (the fix for the flat list freezing at scale; grouping moves to the Browse view).
- `docs/architecture/moderation.md` — the review state machine the Review scope drives.
- `docs/api/metadata.md` — the per-file tag edit + entity rename/merge endpoints.
