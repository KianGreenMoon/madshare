# File-Management View — the unified file list

Every place that lists files and edits their metadata is one component
(`webui/static/js/file-list.js`), parameterised by a **scope**. The admin
**Library** page (`/admin/library`) hosts three scopes behind a client-side
switch — **All files · Review · Trash** — sharing one preview player; the
uploader's **My uploads** (on `/upload`) is the same component in owner mode.

This is the reference for the shipped system. Design, alternatives, and phase
history: `docs/plans/file-management-view.md`. It **supersedes**
`docs/plans/admin-files-rework.md` (the old standalone `/admin/files` page) and
the Files/Moderation/Trash split in `docs/plans/admin-panel-rework.md`.

---

## The component

`createFileList(scope)` owns presentation only: rendering, selection, the bulk
toolbar, badges, inline two-step confirms, and the empty/loading/error states.
It imports no page-specific helpers — the scope injects everything domain-
specific (loaders, action handlers, `onPlay`, `toast`, `handleAuthError`), so it
runs unchanged on the admin pages and the shell-native upload page. Built-in
**Play** + **Edit** row actions and an **Edit tags…** bulk action appear
automatically from the scope's capabilities.

Presentations: a flat **list** (optionally grouped — collapsible-by-uploader for
Review, fixed state sections for My uploads) and an artist→album→track **browse**
(currently used only via the Files By-entity view; see below). Styling lives in
`webui/static/css/file-view.css`, the single source for the component's chrome
(table, bulk toolbar, badges, confirms, switches, the modal access/field layout).
It is self-sufficient so the upload page — which loads no admin CSS — renders
correctly; the admin pages load it too.

### Shared editors

- **Per-file edit** — `track-edit.js`, with an optional **access** section
  (License picker + Guest toggle) shown when the scope is `accessEditable`.
- **Bulk edit** — `bulk-edit.js`, a selection-wide editor. **Blank field = keep**
  each file's value; **Title is excluded** (it is per-track). Setting the
  album/artist tag on a selection *re-tags* those files — it is **not** an entity
  rename.

## The scope catalog

| Scope | Endpoint | Group | Selectable | Access edit | Notes |
|---|---|---|---|---|---|
| **All files** (admin) | `GET /api/files` | – (opt.) | all | ✓ (modal + bulk) | flat list; access is a **read-only column**, edited in the modals. Bulk Move to Trash + Edit tags…. A **Default ⇄ artist/album** sort toggle groups the same flat table (see below). The By-entity drill-down is a separate sub-view (below). |
| **Review** (admin) | `GET /api/admin/moderation` | by uploader (collapsible) | `submitted` | ✗ (tags only) | Approve / Return-with-note / Discard; `show()` gates per state (drafts preview-only), `editable()` gates Edit. |
| **Trash** (admin) | `GET /api/admin/trash` | – | all | ✗ (tags only)¹ | Restore / Delete forever; gained Edit + Play. |
| **My uploads** (owner, `/upload`) | `GET /api/my/uploads` | by state | draft/returned | ✗ (tags only) | state sections, `autoSelect`, Send to approval / Remove; owner-scoped edit endpoint. |

¹ **Trash edits tags only.** `UpdateFileMetadata` resolves by hash with no
`deleted_at` filter (so a tag can be corrected before restore), but the access
endpoints (`SetGuestPlayable`/`SetLicense`) filter `deleted_at IS NULL` — and
access is meaningless on a non-served file. Review is tags-only by choice
(behaviour-preserving); the backend would allow access there (the access routes
are `metadata.edit`-gated, the files aren't deleted), so it is a possible
additive follow-up.

Access writes hit their own endpoints (`POST /api/admin/files/{hash}/license`
then `…/guest`, so an explicit guest wins over license auto-derive); the bulk
applier loops PATCH-metadata + the access endpoints, writing only the filled
fields. Every editable list DTO carries `album_artist` (the editor writes all
four base tags, so an absent prefill would silently clear it — the trash DTO was
extended for this).

### Grouped sort + needs-metadata flag

`scope.artistAlbumSort` adds a **Default ⇄ "By artist / album"** toggle (persisted
in `localStorage`). In grouped mode the same `.files-table` is sorted
**album-artist → album → track# (then title)** with **separator rows** woven in —
a tinted band per artist, a thin indented line per album — each carrying a
**group-select checkbox** wired into the selection Set, so a whole artist/album
can be bulk-edited. Grouping is by `album_artist ?? artist` (a Various-Artists
compilation stays under one band); empty artist/album fall into Unknown / Other
buckets, sorted last. Enabled on **All files, Review, Trash, and My uploads** (in
grouped mode it overrides a scope's native uploader/state grouping). The sort
needs the track number, so `track_number` (+ `year`) is on the `/api/files`,
`reviewItem`, and `trashItem` DTOs.

A file with **neither an artist nor an album-artist** tag gets a calm amber
flag (`.fl-needs-meta` — tint + left accent) in every scope and both sort modes,
so moderators / admins / uploaders can see which rows want metadata first.

### Grouped "Add cover" (add-only)

When `scope.allowCoverAdd` is set, each grouped separator whose entity has **no
cover yet** sprouts an unobtrusive **Add cover** button (`cover-edit.js`, a
self-contained JPEG/PNG ≤10 MB picker that POSTs to the artist/album cover
endpoints). It is **add-only**: the button is hidden once a cover exists (and on
the Unknown/Other fallback buckets), so replacing a cover stays in the By-entity
view. Whether a group already has a cover comes from `artist_has_image` /
`album_has_image` on every list DTO — the grouping key (`album_artist ?? artist`,
album) is exactly the entity those flags join on (`media_metadata.artist_id` /
`album_id` resolve from the same `effectiveArtist`/album), so the
representative row's flag governs the whole group. Enabled on **All files,
Review, Trash** (gated `metadata.edit`) and **My uploads** (every uploader holds
`file.upload`). The cover routes accept **`metadata.edit` OR `file.upload`**
(`RequireAnyPermission`); the handler then enforces add-only for a
`file.upload`-only caller — uploading a missing cover is allowed, overwriting one
returns **403** (`coverReplaceBlocked`). So an uploader can dress a staged draft
that has no art, but cannot touch an existing cover.

## The Library page (Hybrid nav)

`/admin/library` (`webui/html/admin/library.html` + `library.js`) folds the
former `/admin/files`, `/admin/moderation`, and `/admin/trash` pages into one
secondary-nav entry. `library.js` boots auth once, owns the single shared player
(injected into each scope as `play(items, index, highlight)`; next/prev/ended are
generic), builds only the scopes the admin can use (Review needs
`content.moderate`, Trash needs `file.delete`), and swaps panels in place. Each
scope is an exported factory — `createFilesScope` / `createReviewScope` /
`createTrashScope` (in `admin/files.js` / `moderation.js` / `trash.js`) — and all
scope modals coexist in `library.html`. The dashboard cards deep-link via
`#review` / `#trash`.

## Out of scope (the entity axis)

Artist/album **rename**, **merge**, and **cover replace** operate on entities, not
per-file tags. They stay in the **By-entity** drill-down inside the All-files
scope (`admin/files.js`, its own renderer over `/api/artists`,
`/api/albums?artist=`, `/api/tracks?…` + the rename/merge/cover/delete endpoints
in `docs/api/metadata.md` and `docs/api/cover-images.md`). The component's own
browse presentation is not used there; folding the entity affordances into it is
an optional future step. (The one entity action the list does carry is the
grouped **add-only** cover above — adding a *missing* cover, never replacing.)

## Permissions

The component reads `identity.permissions` and the scope; it adds no new
permission. `metadata.edit` → Edit (tags) + the access section + Edit tags…;
`file.delete` → Move to Trash / Restore / Delete forever; `content.moderate` →
the Review scope + Approve/Return/Discard; `file.upload` → My uploads. A scope or
action the caller can't use is simply not rendered; the API enforces the same
gates server-side.

## See also

- `docs/plans/file-management-view.md` — design + phase history + the review mockups.
- `docs/architecture/moderation.md` — the review state machine the Review scope drives.
- `docs/api/metadata.md` — the per-file tag edit + entity rename/merge endpoints.
