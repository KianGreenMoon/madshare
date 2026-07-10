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

> **Update (2026-06-26): the numbered pager is superseded by infinite scroll.**
> The offset **backend** described here (the `GET /api/files` envelope, `CountFiles`,
> the bulk endpoint, the filter/sort maps) is unchanged and stays the source of
> truth. Only the **presentation** changed: the All-files list now renders through
> the shared virtualized scroller and loads pages by infinite scroll instead of
> Prev/Next, and the grouped "By artist / album" view returns on that windowed
> list. See ["This pass"](infinite-scroll-virtualization.md) in the virtualization
> doc. The numbered-pager prose below is retained for the backend contract it
> documents.
>
> **Update (2026-06-27): the same treatment now covers Review, My-uploads, and
> Trash.** All three were non-paged (loaded the whole set, looped one HTTP request
> per file for bulk actions). They are now server-paged with the same envelope +
> infinite scroll, and each has its own **filter-or-hashes batch endpoint** so
> "select all N matching" hits one request:
> - `GET /api/admin/moderation`, `GET /api/my/uploads`, `GET /api/admin/trash`
>   return `{ total, items }` (the two staging lists also add `selectable_total` —
>   the actionable subset count, so the banner reflects only the rows the bulk
>   actions touch: submitted for moderation, draft+returned for My-uploads).
> - `POST /api/admin/moderation/bulk` (approve / return / discard),
>   `POST /api/my/uploads/bulk` (submit / remove),
>   `POST /api/admin/trash/bulk` (restore / delete / edit) — each resolves an
>   explicit `hashes` list **or** a state-scoped `filter` (the same `all:true`
>   guardrail as `/api/admin/files/bulk`), then loops the existing per-row logic
>   server-side. DB resolvers: `PendingReviewHashesByFilter`,
>   `UploadHashesByUserFilter`, `TrashedFileHashesByFilter`.
> - The two bespoke groupings (moderation by uploader, My-uploads by state) survive
>   paging as **non-collapsible streamed separators** via `section-stream.js`
>   (`createSectionStream`, unit-tested) — the single-level sibling of
>   `grouped-stream.js`, fed pages in `sort=uploader` / `sort=state` order.
>   Moderation loses its collapse toggle (a collapse can't hide unfetched rows).

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
   so multi-disc detection and its select-all hashes are exact, and an artist spans
   pages so its header's count and select-all set are "loaded so far" and grow as
   you scroll. The pure grouping state machine is `grouped-stream.js`
   (`createGroupedStream`, unit-tested in `tests/js/grouped-stream.test.mjs`); the
   windowing is [`infinite-scroll-virtualization.md`](infinite-scroll-virtualization.md).
   The Review / Trash / My-uploads scopes now stream the same way (2026-06-27 update
   note): Trash reuses this artist/album stream, while Review and My-uploads stream
   their native by-uploader / by-state separators through `section-stream.js`.
3. **A transactional bulk endpoint** — `POST /api/admin/files/bulk` — runs an
   action over either an explicit hash set **or** "everything matching the current
   filter", in one request. This is what makes "delete a big group" real and what
   lets selection span pages.
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

### `POST /api/admin/files/bulk` — transactional bulk action

Gated by `auth.RequirePermission` per action (it lives in the `admin` group).
Body:

```json
{ "action": "trash" | "edit",
  "hashes": ["…", …],                 // explicit set  (mutually exclusive with…)
  "filter": { "q": "beatles" },        // …all rows matching this filter
  "all": false,                        // required true when filter.q is empty
  "patch": { "artist": "…", "license": "…", "guest": true }   // action:"edit" only
}
```

- **`action:"trash"`** — soft-delete (move to Trash). Permission: `file.delete`.
  One `UPDATE files SET deleted_at = ? WHERE …` over the resolved set — a single
  transaction, not N requests.
- **`action:"edit"`** — apply `patch` (tag keys + `license`/`guest`) across the
  set. Permission: `metadata.edit`. Same change-only / never-clear rule as the
  bulk editor (`file-management-view.md`).
- **Set resolution.** `hashes` (explicit) **or** `filter` (server resolves the
  matching set under the same visibility + `q` the listing uses). Exactly one
  must be given. Explicit `hashes` is capped (e.g. 1000) to bound the request
  body; filter-mode has no cap (it's a server-side `WHERE`).
- **Guardrail.** Empty `filter.q` means "the whole (matching) library" — allowed
  only with `"all": true`, and the UI gates it behind a strong confirm showing
  the live `total`. This prevents an accidental delete-everything.
- **Response.** `{ "ok": true, "affected": 1412 }` for the transactional path;
  `action:"edit"` that can partially fail reports `{ "affected": N, "failed":
  [{hash,error}] }`.

`Restore` / `Delete forever` / bulk tag-edit on the Trash scope **now use their
own batch endpoint** — `POST /api/admin/trash/bulk` (same hashes-or-filter +
`all` shape), resolved by `TrashedFileHashesByFilter` over the trashed bucket
(`f.deleted_at IS NOT NULL`). See the 2026-06-27 update note above.

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

Bulk: `BulkSoftDeleteByHashes(ctx, hashes)` and `BulkSoftDeleteByFilter(ctx,
where, args)` — each a single `UPDATE … SET deleted_at` transaction returning the
affected count. (Edit-bulk can stay a server-side loop in one tx initially; the
win there is correctness/atomicity, the delete win is the round-trip collapse.)

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
- **Bulk wiring.** `bulkTrashHashes` / `filesBulkApply` in `admin/files.js` call
  the new `POST /api/admin/files/bulk` (one request) instead of looping. The
  By-entity `deleteAlbum` / `deleteArtist` use filter/hash-set bulk too.
- **By-entity decoupling.** Drop `loadFilesList`-as-index, `fileByURL`,
  `ensureFilesLoaded`, `resolveHashes`; `editTrack` / `deleteTrack` read
  `t.hash` from the (now hash-carrying) `/api/tracks` DTO.

## Selection across pages

Two selection modes, surfaced in the bulk toolbar:

1. **Explicit** — the checked hashes on the current page (today's behaviour). The
   header "select all" checks the visible page and shows "N selected"; a banner
   offers "Select all `<total>` matching" to switch to mode 2.
2. **Filter-mode** — "all `<total>` rows matching the current filter". Bulk
   actions send `{ filter }` (not `hashes`); the count shown is `total`. Changing
   the filter or sort clears the selection.

## Permissions

Unchanged model. The bulk endpoint enforces the same per-action gate the UI
shows (`trash` → `file.delete`, `edit` → `metadata.edit`); the listing's `q` and
visibility honour the existing guest-listing narrowing. No new permission.

## Planned / future

- **Per-group exact counts / select-all-across-pages for the grouped view.** The
  streamed grouped view (Decision 2) shows artist-header counts and select-all
  hashes that are "loaded so far" — exact for an album (it's buffered whole) but
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
  `/api/admin/files/bulk` — explicit vs filter mode, the empty-filter `all`
  guardrail, per-action permission gating, transactional trash.
- **JS**: existing `queue-ops` style unit coverage isn't affected; manual
  browser pass on the paged list (page controls, filter round-trip, select-all-
  matching → bulk trash) per the live-verify workflow.

## See also

- [`file-management-view.md`](file-management-view.md) — the shared component and scope catalog.
- [`../api/metadata.md`](../api/metadata.md) — the per-file + entity edit endpoints.
- [`soft-delete.md`](soft-delete.md) — the Trash / `deleted_at` model the bulk trash writes.
