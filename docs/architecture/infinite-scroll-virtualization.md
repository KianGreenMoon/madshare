# Infinite Scroll + Virtualization — large lists everywhere

The public **library** (`/`) and several admin lists render *every* row at once
and fetch *unbounded* result sets. That is fine today but will freeze the page
once a list grows large — and a federated library is meant to grow **very**
large. This doc designs the durable answer: a reusable **virtualized
infinite-scroll** list, fed by **cursor-paginated** browse endpoints.

It is a separate effort from
[`file-list-scaling.md`](file-list-scaling.md), which fixes the *admin All-files*
flow with **numbered pagination**. The two share the same idea (the server
returns bounded chunks) but differ in presentation: numbered pages there,
seamless windowed scrolling here. The plan is to build the scroller as a shared
module so the admin flat list can later adopt it over the very same paginated
endpoint.

---

## The problem (two halves)

**1. Unbounded queries.** The library browse queries return the whole set, with
no `LIMIT`:

- `db.ListArtists` / `ListArtistsGuest` (`database/library.go`) — **one row per
  album-artist in the library.** This is the genuinely unbounded one: a large
  federated library could have tens of thousands of artists.
- `db.ListAlbumsByArtistID`, `db.ListTracksByAlbumID` — usually bounded by nature
  (an artist's albums, an album's tracks), **except** the Unknown-artist /
  Other-album buckets, which can accumulate thousands of orphan rows.
- Search is already capped (`LIMIT 50`, `database/library.go` `search`).

**2. Render-everything.** `app.js` builds every row in one synchronous loop —
`renderArtistList`, `renderAlbumList`, `renderTrackList` each do
`items.forEach(… wrap.appendChild(row))`. As in the admin list, the freeze is
**DOM node count**, not data size: thousands of rows × several nodes each, laid
out in one main-thread burst. The same is true of the admin in-memory scopes
(`file-list.js` `table()`).

## What virtualization is (the mechanism)

Keep in the DOM **only the rows currently on screen** (plus a small buffer);
represent everything above and below as empty spacer `<div>`s sized to the rows
they stand in for, so the scrollbar geometry is correct.

```
   total = N rows, each ROW_H px  →  content height = N · ROW_H

   ┌─ viewport (fixed-height scroll container) ───────────┐
   │  ░ top spacer  (height = firstIndex · ROW_H) ░       │   ← no nodes
   │  ┌─────────────────────────────────────────────┐     │
   │  │ row[firstIndex] … row[lastIndex]             │     │   ← only ~viewport+buffer
   │  │ (the window: ~15–40 real rows)               │     │     rows exist in the DOM
   │  └─────────────────────────────────────────────┘     │
   │  ░ bottom spacer (height = (N-lastIndex) · ROW_H) ░   │   ← no nodes
   └──────────────────────────────────────────────────────┘
```

On scroll, recompute `firstIndex = floor(scrollTop / ROW_H)` (minus buffer),
render that slice, resize the two spacers. DOM stays ~constant regardless of N,
so the freeze is structurally impossible. **Infinite scroll** is then just: when
the window approaches the end of what's been fetched, request the next chunk and
extend the data array (and `N`).

**Uniform row height is the key simplifier.** With a fixed `ROW_H`, all the index
math is `O(1)` arithmetic — no per-row measurement, the part that makes general
virtualization fiddly. The library's artist/album/track rows are uniform; we keep
them uniform (see "Variable-height rows" below for the track-list separator
case).

## Backend: cursor-paginated browse endpoints

For an append-as-you-scroll feed, **keyset (cursor) pagination** beats
offset/limit: it is `O(1)` at any depth, and it is **stable while the underlying
set changes** (no row skipped or doubled when an insert shifts offsets). This is
the opposite trade-off from the admin pages, where jump-to-page wants offset
(hence `file-list-scaling.md` uses offset).

Shape (applies to the listing endpoints `app.js` consumes):

```
GET /api/artists?limit=60&cursor=<opaque>
  → { items: [ … ], next_cursor: "<opaque>" | null }
```

- **`cursor`** is opaque to the client (base64 of the last row's sort key +
  tiebreaker). Server decodes it into the keyset predicate.
- **`next_cursor`** is `null` when the last page was returned; the client stops
  fetching.
- No `total` is required for an infinite feed (and counting a huge set every
  request is wasteful). If a count is wanted for display, expose it as a separate
  cheap `COUNT(*)` only where needed.

**The sort key must be unique** so the cursor is unambiguous — append the row
`id` as a tiebreaker. Concretely:

- **Artists.** Current order is
  `ORDER BY a.norm_name = <unknown> ASC, LOWER(a.name) ASC`. Make it stable with
  `… , a.id ASC`; the cursor carries `(is_unknown_bucket, lower_name, id)` and the
  keyset predicate is the standard lexicographic
  `(k1, k2, k3) > (c1, c2, c3)` expansion.
- **Albums / tracks per entity.** Same pattern *if* they ever need paging; in
  practice they're small. The Unknown/Other buckets are the exception — the same
  cursor scheme covers them, so no special case.

DB layer: each `listX` grows a variant taking `(cursor, limit)` that appends the
keyset `WHERE` and a `LIMIT ?`. The guest variants (`accessClause`) and the
`visibleFile` predicate are unchanged — paging composes with them. `limit` is
clamped (e.g. `[1, 200]`, default ~60).

**Repository/test gotcha.** New `Repository` methods break the `api` package's
`fakeRepo` and the `database_test.go` table/version assertions are unaffected (no
migration) — see [`file-management-view.md`](file-management-view.md) references
and `project_migration_repo_gotchas`.

## Frontend: a shared virtual-scroller module

A new dependency-free module, e.g. `webui/static/js/virtual-list.js`:

```
createVirtualList({
  container,                 // the fixed-height scroll element
  rowHeight,                 // uniform ROW_H (px)
  buffer = 8,                // extra rows rendered above/below the viewport
  renderRow(item, index),    // → an element for one row (reused/replaced on scroll)
  fetchMore(cursor),         // → { items, next_cursor }; called near the end
  keyOf(item),               // stable id (selection, dedupe, scroll restore)
})
  → { reset(), refresh(), scrollToTop(), getItems(), destroy() }
```

Responsibilities:

- Maintain `items[]`, `nextCursor`, `loading`. On scroll, compute the visible
  window, render that slice into a single positioned `<div>` between two spacer
  divs (or use `transform: translateY`), and update spacer heights.
- **Prefetch sentinel** — an `IntersectionObserver` on a sentinel near the bottom
  (or "window within `buffer` rows of the end") triggers `fetchMore` once at a
  time; ignore re-entrancy while `loading`.
- **Resize** — recompute on `ResizeObserver` of the container.
- A `loading` row / spinner at the tail; an end-of-list state when
  `next_cursor === null`.

The module is **presentation-only and reusable**: the library uses it for the
artist grid (and the Unknown/Other buckets); the admin flat list *can* later use
it over the offset endpoint by giving it a `fetchMore` that walks `offset`
instead of `cursor` (the module doesn't care which, it just calls `fetchMore`
and appends).

### Integration in `app.js`

`renderArtistList` / `renderAlbumList` / `renderTrackList` switch from
"build all rows" to "mount a `createVirtualList` whose `renderRow` builds one
row." The existing per-row builders are reused verbatim as `renderRow`. Drilling
into a new level calls `reset()` with a fresh `fetchMore`. The track-row
duration backfill (`app.js` lazy `fetchOne` / `CONCURRENCY` batch) keys off the
*rendered* rows, so it naturally only fetches durations for on-screen tracks — a
bonus.

## Variable-height rows (the one real wrinkle)

The album **track list** isn't strictly uniform: multi-disc albums insert quiet
**"Disc N"** separator rows (`disc.js`; `app.js` `renderTrackList`), which are a
different height. Options, simplest first:

1. **Don't virtualize bounded lists.** A single album's track list is almost
   always small; only the *artist grid* and the *Unknown/Other buckets* truly
   need windowing. Render track lists as today, and only swap in the virtual list
   where the count crosses a threshold (e.g. > 200). This keeps the disc
   separators trivial and sidesteps variable heights for the common case.
2. **Make separators part of a uniform row** (render the disc label as a pinned
   sub-header outside the scroll math), so every scrollable row is one height.
3. **Measured/variable-height virtualization** — a height cache; only if 1–2
   prove insufficient. This is the fiddly general case we otherwise avoid.

Recommended: **option 1** — virtualize the unbounded surfaces, leave naturally
bounded ones alone, gated by a row-count threshold. It gets the scaling win with
the least complexity.

## Selection, focus, scroll restoration

- **Selection** (admin reuse) lives in a `Set` of `keyOf(item)`, not in the DOM —
  a checkbox for an off-screen row simply isn't rendered; it re-applies its
  checked state from the Set when scrolled back in. "Act on the whole set" uses
  **filter-mode bulk** (`file-list-scaling.md`), which never needs the rows
  materialized. So virtualization and large bulk actions are orthogonal.
- **Keyboard focus** — when the focused row recycles out of the window, move
  focus to a stable anchor (the container) so focus is never lost into a detached
  node. Roving-tabindex over the *rendered* slice.
- **Scroll restoration** — on back/drill-up, restore `scrollTop`; since rows are
  uniform, position is just `index · ROW_H`.

## Accessibility

A windowed list is not a complete DOM list, so screen readers can't see the full
set. Mitigations: keep the scroll container an ordinary scrollable region with a
clear label; announce loading/end via `aria-live`; ensure each rendered row is
fully operable; the search box (already `LIMIT 50` and small) remains the
non-windowed way to jump to a specific item. Document this trade-off; it is the
standard one for any virtualized list.

## Phasing

1. **Backend** — cursor-paginate `/api/artists` (the unbounded one) + the
   Unknown/Other bucket paths; clamp `limit`; stable sort keys. Guest variants
   composed.
2. **Module** — `virtual-list.js` (uniform-height windowing + prefetch sentinel),
   with unit-testable index math.
3. **Library** — `app.js` artist grid (and large track buckets via the threshold)
   on the module.
4. **Later (optional)** — admin flat list adopts the same module over the offset
   endpoint from `file-list-scaling.md`; cursor-paginate albums/tracks only if a
   real library shows they need it.

## Testing

- **DB**: keyset paging returns each row exactly once across chunk boundaries,
  stable under inserts; guest narrowing + `visibleFile` still apply; `limit`
  clamping; the Unknown/Other bucket cursors.
- **JS**: `virtual-list.js` index math (firstIndex/lastIndex/spacer heights for a
  given `scrollTop`, `rowHeight`, `buffer`) as a `node --test` unit (mirrors
  `tests/js/queue-ops.test.mjs`); prefetch fires once near the end and stops at
  `next_cursor === null`.
- **Browser**: live pass on a seeded large library — smooth scroll, bounded DOM
  node count (verify in devtools), drill-down + back scroll restoration, search
  still works.

## See also

- [`file-list-scaling.md`](file-list-scaling.md) — admin All-files numbered pagination + bulk actions (shares the bounded-chunk backend idea).
- [`artist-album-model.md`](artist-album-model.md) — the entity overlay the browse endpoints list (artists/albums by id, sort keys, Unknown/Other buckets).
- [`disc-numbering.md`](disc-numbering.md) — the multi-disc separator rule behind the track-list variable-height wrinkle.
- [`../api/search.md`](../api/search.md) — the already-capped search path (the non-windowed jump-to-item).
