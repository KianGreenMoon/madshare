# File-List Scaling — server-side pagination + bulk operations

The admin **All files** list (`/admin/library` → All files) freezes at ~1500
files. This doc is the design for fixing it: server-side **pagination + filter +
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
2. **The grouped "By artist / album" view is RETAINED but deferred.** It sorts
   the *whole* set in the browser, which is incompatible with server paging (only
   one page is in hand). It is **not** dropped and **not** replaced by Browse — it
   is a planned capability that requires the shared list to be **virtualized**
   (load the full set, render only the visible window), so grouping/filter/select-
   all keep working without freezing. That work is designed in
   [`infinite-scroll-virtualization.md`](infinite-scroll-virtualization.md);
   until it lands, the All-files flat list ships **without** the grouped toggle
   (the flat list + server sort is the interim view). The other scopes (Review /
   Trash / My uploads) keep their current in-memory grouping unchanged — they are
   bounded by nature.
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
| `sort` | `created_desc` | one of `created_desc`, `created_asc`, `title_asc`, `title_desc`, `artist_asc`, `artist_desc`, `size_desc`, `size_asc`, `untagged_first` (rows with no artist/album-artist tag first — the "needs metadata" rows). Unknown → `created_desc`. |

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
(`deleted_at IS NULL AND review_state = 'approved'`) always applies, and the
guest-listing path (`ListFilesGuest` / `accessClause`) still narrows for
capability-less identities.

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

`Restore` / `Delete forever` on the Trash scope are **out of scope here** (Trash
is bounded); they keep their per-hash loops for now. The bulk endpoint is
designed generically so they can move onto it later if needed.

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
    **Untagged first**) instead of the client `artistAlbumSort` grouped toggle —
    the grouped view returns once the list is virtualized (see the plan below);
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

- **Grouped "By artist / album" view via virtualization (planned).** This is the
  one piece deliberately deferred, not dropped. The grouped view needs the whole
  set in the browser to sort across artists, so it returns once the shared list
  is **virtualized** (load the full set, render only the visible window — keeping
  grouping, filter, and select-all working without a freeze). Designed in
  [`infinite-scroll-virtualization.md`](infinite-scroll-virtualization.md); that
  doc carries the admin file-list as a consumer of the shared virtual scroller.
  When it lands, the All-files scope re-enables the grouped toggle on the
  virtualized list, and the same view can extend to the other file-list scopes.
- Moving **Trash restore / delete-forever** and the **Review** bulk actions onto
  the bulk endpoint (bounded scopes; deferred).

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
