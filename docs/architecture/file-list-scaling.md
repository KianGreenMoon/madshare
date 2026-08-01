# File-List Scaling — server-side pagination + bulk operations

The admin flat file list (`/admin/library`, today the Full Library › All
Appearances lens) freezes at ~1500 rows without windowing. This doc is the design for fixing it: server-side **pagination + filter +
sort** for the flat list, a transactional **bulk-action endpoint** so large
groups can be deleted/edited in one request, and the decoupling that makes both
possible. The grouped/drill-down experience stays in the **By-entity Browse**
view, which is already lazy.

It builds on [`file-management-view.md`](file-management-view.md) (the shared
`file-list.js` component) and the listing DTO in
[`metadata.md`](../api/metadata.md).

**Scope.** This doc covers the **admin All-files flow only**, using *numbered
pagination*. The smoother **infinite-scroll** treatment — which the *public
library* (`/`) also needs, since it can grow far larger — is a separate effort
designed in
[`infinite-scroll-virtualization.md`](infinite-scroll-virtualization.md). Both
sit on the same paginated backend; this flow just renders one page at a time
rather than a virtualized window.

> **Presentation note:** the numbered pager this doc designed is superseded by
> infinite scroll. The offset **backend** described here (the `{total, items}`
> envelope, `CountFiles`-style counting, the filter/sort maps) stays the source
> of truth; the lists render through the shared virtualized scroller and load
> pages by infinite scroll, and the grouped "By artist / album" view streams on
> that windowed list. See
> [infinite-scroll-virtualization.md](infinite-scroll-virtualization.md).
>
> **Endpoint note:** the bulk dialect is **tagset-addressed** — the concrete
> request/response contract of the four bulk endpoints
> (`POST /api/admin/appearances/bulk`, `…/trash/bulk`, `…/moderation/bulk`,
> `POST /api/my/uploads/bulk`) lives in [docs/api/bulk.md](../api/bulk.md).
> Each resolves an explicit `tagset_ids` list **or** a state-scoped `filter`
> with the `all:true` guardrail, then applies the action server-side. DB
> resolvers: `AppearanceIDsByFilter`, `TrashedAppearanceIDsByFilter`,
> `PendingReviewTagsetIDsByFilter`, `UploadTagsetIDsByUserFilter`.
> The paged staging lists (`GET /api/admin/moderation`, `GET /api/my/uploads`,
> `GET /api/admin/trash`) return `{ total, items }` (the two staging lists also
> add `selectable_total` — the actionable subset count, so the banner reflects
> only the rows the bulk actions touch). The two bespoke groupings (moderation
> by uploader, My-uploads by state) survive paging as **non-collapsible
> streamed separators** via `section-stream.js` (`createSectionStream`,
> unit-tested) — the single-level sibling of `grouped-stream.js`, fed pages in
> `sort=uploader` / `sort=state` order.

---

## The problem (why it freezes — nothing is lazy)

The flat list loads and renders the **entire** `files` table at once:

- **Backend.** `GET /api/files` (`api/handlers.go` `listFiles`) calls
  `db.ListFiles` (`database/files.go` `listFiles`) — a single
  `SELECT … ORDER BY f.created_at DESC` with **no `LIMIT`/`OFFSET`**, returning
  every row as one JSON array. There is no count helper.
- **Fetch.** `loadFilesList` (`webui/static/js/admin/files.js`) pulls all rows
  into `allFiles` and builds a `fileByURL` index.
- **Render.** `table()` (`webui/static/js/file-list.js`) builds one `<tr>` per
  file **synchronously** — ~7 `<td>` + spans + a checkbox + 2–3 action buttons,
  each with its own `addEventListener`. At 1500 files that is ~25k–40k DOM nodes
  and ~6k listeners created on the main thread in one shot → the multi-second
  freeze. `groupedTable()` builds the same full tree.
- **Interaction cost.** `syncSelectionUI()` runs after every render **and on
  every checkbox toggle**, doing several full-tree `querySelectorAll` passes over
  all rows (O(n) per click). The filter box rebuilds the whole table per
  keystroke.

Bulk deletes don't scale either: `bulkTrashHashes` / `deleteHashes`
(`admin/files.js`) loop **one HTTP `DELETE` per hash, sequentially** — 1500
files = 1500 round-trips — and "select all" only covers the rows currently in
memory.

The **By-entity Browse** view stays fast because it *is* the lazy path: one
bounded level per fetch (`/api/artists` → `/api/albums?artist_id=` →
`/api/tracks?album_id=`). Only the flat list loads everything.

## Decisions

1. **The flat All-files list becomes server-paged**: `GET /api/files` gains
   `limit`, `offset`, `q`, `sort` and returns a `{ total, items }` envelope. The
   client renders one bounded page (~100 rows) with page controls; filter and
   sort round-trip to the server.
2. **The grouped "By artist / album" view streams in server order.** Rather than
   sort the *whole* set in the browser (incompatible with paging — only one page
   is in hand), the server owns the grouping order via a `sort=grouped` token
   (album-artist → album by earliest year → disc → track; see `fileSortOrder`),
   so the grouped view **streams page-by-page exactly like the flat list** through
   the same windowed infinite-scroll. The client inserts the artist/album/disc
   separators as the keys change between rows; an album is buffered to its boundary
   so multi-disc detection and its select-all set are exact, and an artist spans
   pages so its header's count and select-all set are "loaded so far" and grow as
   you scroll. A separator's select-all set holds the **scope's own row keys** —
   the value `rowKey` yields, which for both scopes that turn this view on (All
   Appearances, Trash) is the `tagset_id`, not the blob hash. `createGroupedStream`
   therefore takes `keyOf` from the caller: derive it inside the stream and the
   separator ticks itself, cascades to nothing, and hands the bulk endpoint content
   hashes where it wanted appearance ids. The pure grouping state machine is
   `grouped-stream.js`
   (`createGroupedStream`, unit-tested in `tests/js/grouped-stream.test.mjs`); the
   windowing is [`infinite-scroll-virtualization.md`](infinite-scroll-virtualization.md).
   The Review / Trash / My-uploads scopes now stream the same way (2026-06-27 update
   note): Trash reuses this artist/album stream, while Review and My-uploads stream
   their native by-uploader / by-state separators through `section-stream.js`.
3. **A transactional bulk endpoint** — `POST /api/admin/appearances/bulk`
   ([docs/api/bulk.md](../api/bulk.md)) — runs an action over either an explicit
   tagset-id set **or** "everything matching the current filter", in one
   request. This is what makes "delete a big group" real and what lets
   selection span pages.
4. **Decouple By-entity from the full fetch.** Add `hash` to the `/api/tracks`
   DTO so entity edit/delete resolve a track to its file directly, instead of
   through a whole-table `fileByURL` index that paging would leave incomplete.

Pagination is **offset-based** (`LIMIT`/`OFFSET`), not keyset: an admin tool
wants jump-to-page and arbitrary sort, and offset is fine at the
thousands-to-tens-of-thousands scale this targets. Keyset is a possible future
optimisation if libraries grow much larger.

## API contract

### `GET /api/files` — paginated listing

Query parameters (all optional):

| Param | Default | Meaning |
|---|---|---|
| `limit` | `100` | page size; clamped to `[0, 500]`. `0` = count-only (empty `items`, `total` still set — used by the dashboard count). |
| `offset` | `0` | rows to skip; clamped to `>= 0`. |
| `q` | `""` | case-insensitive substring filter (see below). |
| `sort` | `created_desc` | one of `created_desc`, `created_asc`, `title_asc`, `title_desc`, `artist_asc`, `artist_desc`, `size_desc`, `size_asc`, `untagged_first` (rows with no artist/album-artist tag first — the "needs metadata" rows), `grouped` (the "By artist / album" view order: album-artist → album by earliest year → disc → track, empty buckets last; a window `MIN(year)` orders each album so a per-track year gap can't reorder it). Unknown → `created_desc`. |

Response is an **envelope** (was a bare array):

```json
{ "total": 1532, "limit": 100, "offset": 0, "items": [ { …fileItem }, … ] }
```

`items[]` is the existing `fileItem` shape unchanged (`api/handlers.go`), **plus
`hash` is already present** there. `total` is the count of rows matching the
visibility + `q` filter (ignoring `limit`/`offset`), so the client can render
"page N of M" and "Select all N matching".

**`q` semantics.** Matches the same fields the row shows — title, artist,
album-artist, album, and filename — via SQLite `LIKE` with the existing
`ESCAPE '\\'` metacharacter handling (mirror `database/library.go` `search`).
Visibility is unchanged: the `visibleFile` predicate
(`f.deleted_at IS NULL AND m.deleted_at IS NULL AND m.review_state = 'approved'`,
where `m` is the file's **representative appearance** — its `tagsets` row, since
review/trash moved onto the tagset in migration 024,
`docs/architecture/recording-tagsets.md`) always applies, and the guest-listing
path (`ListFilesGuest` / `accessClause`) still narrows for capability-less
identities.

**Backward-compatibility.** The bare-array → envelope change is breaking, but
the only consumers are internal: `admin/files.js` (the All-files scope) and
`admin/dashboard.js` (`fillCount('countFiles', …)`, which switches to
`?limit=0` and reads `total`). Per the project's pre-release "prune legacy
outright" stance, the envelope becomes the only shape — no dual-format. The
`api/*_test.go` tests that decode `/api/files` into a slice
(`access_handlers_test.go`, `review_test.go`, `handlers_test.go`) are updated to
the envelope.

### The transactional bulk endpoints

The full request/response contract — actions, target-set resolution
(`tagset_ids` **xor** `filter`), the empty-filter `all:true` guardrail, the
per-action permission gates, and the partial-failure response shape — is
documented in [docs/api/bulk.md](../api/bulk.md) for all four endpoints
(`appearances/bulk`, `trash/bulk`, `moderation/bulk`, `my/uploads/bulk`).
The design principles that page implements are this document's: one request
per bulk action (a single transaction or a server-side loop — never N HTTP
round-trips), selection that can span pages via filter-mode, and a strong
confirm before an empty-filter whole-set action.

### `GET /api/tracks` — add `hash`

Add `"hash"` to the track DTO (`api/library_handlers.go`, the
`album_id`-addressed tracks handler) so the By-entity view resolves a track to
its file by hash directly. This removes the All-files scope's role as the
By-entity hash index — `ensureFilesLoaded` / `fileByURL` / the
`resolveHashes(tracks)` path in `admin/files.js` are replaced by reading
`t.hash`.

## Database layer

`database/files.go`:

- Generalise `listFiles` into `listFilesPage(ctx, opts)` taking
  `{ where, args, orderBy, limit, offset }`. The existing `ListFiles` /
  `ListFilesGuest` become thin callers (no filter, default sort, no limit) so
  their current callers and tests keep working where they don't need paging.
- Add `CountFiles(ctx, where, args)` — `SELECT COUNT(*) FROM files f … WHERE
  visibleFile [AND where]` (same JOINs only where the filter needs them; the `q`
  filter touches `media_metadata`, already left-joined).
- A shared **filter builder** turns `q` into the `(LOWER(title) LIKE … OR …)`
  predicate + args, reused by both the page query and the count, and by the bulk
  endpoint's filter-mode — one definition of "what `q` matches".
- A shared **sort map** turns the `sort` token into a safe `ORDER BY` fragment
  (allow-listed columns only — never interpolate user input).

Bulk: `BulkTrashTagsets(ctx, tagsetIDs)` — one chunked `UPDATE … SET
deleted_at` transaction returning the affected count; filter-mode resolves the
id set first (`AppearanceIDsByFilter`). (Edit-bulk stays a server-side loop in
one tx; the win there is correctness/atomicity, the delete win is the
round-trip collapse.)

**Migration gotcha.** No schema change — so no new migration, and the
`database_test.go` version/table assertions are untouched. New `Repository`
methods (`ListFilesPage`, `CountFiles`, the bulk ones) **do** break the `api`
package's `fakeRepo` — update it (see `project_migration_repo_gotchas`).

## UI changes (`file-list.js` + `admin/files.js`)

Keep the component shared; add a **paged mode** rather than rewriting it.

- **Scope flag.** A scope opts in with `paged: true` and a
  `loadPage({ limit, offset, q, sort }) → { total, items }` loader (the All-files
  scope uses it; other scopes keep `load()` unchanged). In paged mode the
  component:
  - renders **page controls** (Prev / Next, "page N of M", total count) instead
    of building the whole table;
  - routes the **filter box** to a debounced server reload (resetting to offset
    0), not an in-memory `visibleFiles()` rebuild;
  - renders a **sort control** (the allow-listed `sort` tokens, including
    **Untagged first**) plus the separate **By artist / album** toggle, which
    re-queries with `sort=grouped` and streams that order (the dropdown is disabled
    while grouping is on, since grouping imposes its own order);
  - selection stays a per-page `Set`, **plus** a "Select all N matching" banner
    that flips selection into **filter-mode** so a bulk action hits
    `POST …/bulk` with the filter, not an enumerated list.
- **Quick wins, folded in.** While touching the render path: use **event
  delegation** (one listener on the `<table>` for play/edit/trash/checkbox via
  `data-*`) instead of per-row listeners, and make `syncSelectionUI` update only
  the toggled row + counts rather than walking the whole tree. These also help
  the still-in-memory scopes.
- **Bulk wiring.** `appearancesBulkCall` in `admin/files.js` posts to
  `POST /api/admin/appearances/bulk` (one request) instead of looping. The
  By-entity `deleteAlbum` / `deleteArtist` use the same endpoint with an
  entity-pinned filter (`artist_id` / `album_id`).
- **By-entity decoupling.** Drop `loadFilesList`-as-index, `fileByURL`,
  `ensureFilesLoaded`, `resolveHashes`; `editTrack` / `deleteTrack` read
  `t.hash` from the (now hash-carrying) `/api/tracks` DTO.

## Selection across pages

Two selection modes, surfaced in the bulk toolbar:

1. **Explicit** — the checked rows (tagset ids) on the current page. The
   header "select all" checks the visible page and shows "N selected"; a banner
   offers "Select all `<total>` matching" to switch to mode 2.
2. **Filter-mode** — "all `<total>` rows matching the current filter". Bulk
   actions send `{ filter }` (not an id list); the count shown is `total`.
   Changing the filter or sort clears the selection.

## Permissions

Unchanged model. The bulk endpoint enforces the same per-action gate the UI
shows (`trash` → `file.delete`, `edit` → `metadata.edit`); the listing's `q` and
visibility honour the existing guest-listing narrowing. No new permission.

## Planned / future

- **Per-group exact counts / select-all-across-pages for the grouped view.** The
  streamed grouped view (Decision 2) shows artist-header counts and select-all
  key sets that are "loaded so far" — exact for an album (it's buffered whole) but
  growing for an artist until you've scrolled past all of it. A small server-side
  per-group aggregate (counts + cover flags) or a group-scoped "select all
  matching" (reusing `FileFilter.ArtistID`/`AlbumID`) would make both exact up
  front. Deferred — the loaded-so-far behaviour is acceptable for the common edit
  flows (select an album, or individual tracks). This applies equally to the
  streamed by-uploader (moderation) and by-state (My-uploads) separators.
- **Edit-all-matching for the staging scopes.** Moderation and My-uploads support
  filter-batch approve/return/discard/submit/remove, but *tag editing* across "all
  matching" is not wired (there is no edit-by-filter endpoint for staged files), so
  the component disables "Edit tags…" in select-all mode there; explicit-selection
  edit still works. Trash does support edit-all (`/trash/bulk` `action:"edit"`).

## Testing

- **DB**: `listFilesPage` limit/offset/sort/filter correctness + `CountFiles`
  matches the filtered set; bulk soft-delete affected counts; filter `q`
  escaping.
- **API**: `/api/files` envelope shape, clamping (`limit` 0 / >500 / negative
  `offset`), guest-listing still narrows, `sort` allow-list (unknown → default);
  `/api/admin/appearances/bulk` — explicit vs filter mode, the empty-filter
  `all` guardrail, per-action permission gating, transactional trash.
- **JS**: existing `queue-ops` style unit coverage isn't affected; manual
  browser pass on the paged list (page controls, filter round-trip, select-all-
  matching → bulk trash) per the live-verify workflow.

## See also

- [`file-management-view.md`](file-management-view.md) — the shared component and scope catalog.
- [`../api/metadata.md`](../api/metadata.md) — the per-file + entity edit endpoints.
- [`gc-model.md`](gc-model.md) — the Trash / `deleted_at` model the bulk trash writes.
