# Bulk Operations API

Acting on **many rows at once** from the file-management surfaces: the admin
library and Trash views (the shared `file-list.js`), the review queue
(`admin/moderation.js`) and the uploader's own staging tab (`mine-list.js`) —
the latter two rebuilt as bespoke modules in the recording-tagsets rework. Each
surface has a bulk toolbar that operates over either a **hand-picked selection**
or the whole **"Select all N matching"** set, and posts to one of the four
endpoints below.

There are four bulk endpoints, one per surface:

| Endpoint | Surface | Row identity | Actions | Gate |
|----------|---------|--------------|---------|------|
| `POST /api/admin/appearances/bulk` | live library — the All Appearances lens **and** the By-entity deletes (`/admin/library`) | `tagset_ids` | `trash`, `edit` | `file.delete` **or** `metadata.edit` (per action) |
| `POST /api/admin/trash/bulk` | Trash · Appearances (`/admin/library#trash`) | `tagset_ids` | `restore`, `delete`, `edit` | `file.delete` **or** `metadata.edit` (per action) |
| `POST /api/admin/moderation/bulk` | review queue (`/admin/library#review`) | `tagset_ids` | `approve`, `return`, `discard` | `content.moderate` (`discard` also needs `file.delete`) |
| `POST /api/my/uploads/bulk` | "My uploads" staging tab | `tagset_ids` | `submit`, `remove` | `file.upload` (owner-scoped) |

The recordings curation view (`/admin/library#recordings`, recording-tagsets P5) adds
two set-shaped operations of its own, addressed by **`recording_ids`** and
documented in `docs/architecture/recording-tagsets.md`: `POST
/api/admin/recordings/trash` (`{recording_ids}`, whole-recording soft trash,
`file.delete`) and `POST /api/admin/recordings/merge` (`{target_id,
source_ids}`, `content.moderate`). They act on explicit id lists only — a
merge or whole-recording trash is always a deliberate hand-picked selection,
so the filter/`all:true` machinery below does not apply to them.

**Every surface addresses the appearance** by **`tagset_id`** — since the
recording-tagsets rework the catalog unit is the tagset, not the file, and a
byte-dup blob can host several appearances
(`docs/architecture/recording-tagsets.md`, `docs/architecture/moderation.md`).
There is no hash-addressed bulk dialect anymore (GC model P3): the By-entity
delete-album/-artist paths pin the `appearances/bulk` filter with
`artist_id`/`album_id` instead.

All four share the same **target-set resolution** and **guardrail** described
next; the differences are the id list, which actions they accept, and which set
the filter resolves to. The concept lives in
`docs/architecture/file-list-scaling.md`; this page is the request/response
reference.

---

## Common concepts

### Target set: the id list **xor** `filter`

Every bulk request names its target set in exactly **one** of two ways
(supplying both, or neither, is a `400`):

- **the id list** — **`tagset_ids`**, an explicit array of a hand-picked
  selection (each a positive integer). The surfaces are tagset-addressed
  because their rows are appearances, not blobs (recording-tagsets P7c): one
  blob can host several appearances, and an absorbed/purged one has no hash at
  all. The list is capped at **5000** entries to bound the request body; over
  the cap is a `400`.
- **`filter`** — an object the server resolves to the matching set on its side
  (the "Select all N matching" path). No cap — it is a server-side `WHERE`.

```json
"filter": { "q": "beatles", "field": "artist" }
```

| Field | Type | Description |
|-------|------|-------------|
| `q` | string | The search term (same matching the listing's filter box uses). Max 200 chars. |
| `field` | string | Narrows what `q` matches: `artist`, `album`, or `title`. Empty / omitted / any other value = **General** (every field). |

`POST /api/admin/appearances/bulk` additionally accepts `artist_id` /
`album_id` in the filter (the By-entity delete-album / delete-artist path); the
other three resolve on `q` + `field` only.

The filter resolves only to the rows that surface actually owns: live
approved **appearances** for `appearances/bulk`, trashed **appearances** for
`trash/bulk`, **submitted** appearances for `moderation/bulk`, and the
caller's own **draft + returned** appearances for `my/uploads/bulk`.

### The empty-filter guardrail (`all`)

A blank `filter.q` (with no `artist_id`/`album_id` pin) means **"everything in
this surface"**. That is refused with a `400` unless the request also sets
`"all": true` — the explicit confirmation the UI pairs with a strong "act on all
N" dialog. It prevents an accidental trash-the-whole-library. `all` has no effect
in id-list mode or when the filter term is non-empty.

### Per-action authorization

The `appearances/bulk` and `trash/bulk` routes admit **either** `file.delete` **or**
`metadata.edit`, and the handler enforces the gate the chosen action actually
needs (destructive actions → `file.delete`; `edit` → `metadata.edit`). A caller
holding only one capability is `403` for actions requiring the other. The
`moderation/bulk` route requires `content.moderate` outright, with `discard`
additionally requiring `file.delete` (it is a soft delete). `my/uploads/bulk` is
owner-scoped under `file.upload`. Gating is a pass-through only when auth is not
configured.

### Body cap

Each request body is capped at **1 MiB** (a 5000-id list is well under it).

---

## `POST /api/admin/appearances/bulk`

Acts over the **live library** (live approved appearances, one row per
tagset). Backs the All Appearances lens's bulk toolbar and per-row Move to
Trash, the By-entity view's delete-track (a one-element `tagset_ids`), and the
By-entity delete-album / delete-artist paths (an `artist_id`/`album_id`-pinned
filter — a pin scopes the set, so it needs no `all`).

### Request

```json
{
  "action": "trash" | "edit",
  "tagset_ids": [12, 34],
  "filter": { "q": "beatles", "field": "artist", "artist_id": 12, "album_id": 4 },
  "all": false,
  "patch": { "artist": "…", "license": "…", "guest": true }
}
```

| `action` | Effect | Permission |
|----------|--------|------------|
| `trash` | Soft-delete (move to Trash) the resolved appearances. One batched `BulkTrashTagsets` transaction + a single `appearance.bulk_trash` audit row. The blobs and recordings stay. | `file.delete` |
| `edit` | Apply `patch` (tags **and access**) across the set — see [The edit patch](#the-edit-patch). Unlike the Trash edit, access is allowed: license/guest forward to each appearance's **recording** (`BulkSet…ByTagsets`, live approved appearances only). | `metadata.edit` |

### Response

- `trash`: `{ "ok": true, "affected": N }`.
- `edit`: `{ "ok": true, "affected": N, "failed": [{ "tagset_id": 12, "error": "…" }] }`.

## `POST /api/admin/trash/bulk`

Acts over the **Trash · Appearances** lens (`tagsets.deleted_at IS NOT NULL`).
The unit is the appearance, so the id list is **`tagset_ids`** (`field` may be
`artist`/`album`/`title`; no `artist_id`/`album_id`). `delete` skips a live
appearance and reclaims a shared blob only once its last trashed appearance is
gone; `edit` is tags only (access is a recording property).

### Request

```json
{
  "action": "restore" | "delete" | "edit",
  "tagset_ids": [12, 34],
  "filter": { "q": "", "field": "" },
  "all": true,
  "patch": { "title": "…" }
}
```

| `action` | Effect | Permission |
|----------|--------|------------|
| `restore` | Un-delete the resolved set (re-enters its prior review state). One batched `BulkRestoreTagsets` transaction + one `appearance.bulk_restore` audit row. | `file.delete` |
| `delete` | **Permanently** delete (DB rows in one batched transaction; blobs reclaimed storage-aware after commit, a failure only orphans bytes for prune to reconcile). One `file.bulk_delete` audit row. | `file.delete` |
| `edit` | Apply `patch` across the trashed set — same as the library `edit` above. | `metadata.edit` |

### Response

`restore` / `delete`: `{ "ok": true, "affected": N }`. `edit`:
`{ "ok": true, "affected": N, "failed": [...] }` (same shape as the library edit).

> Every bulk action across all endpoints runs under one batched transaction
> per action (chunked) + one summary audit row — never a write-per-row loop, which
> produced `SQLITE_BUSY` under load over large "select all matching" sets. (Bulk
> tag edits re-resolve entities per file, so they share a transaction per chunk
> rather than one `UPDATE`.) See `.issues/open-issues.md` → "Bulk write paths /
> SQLITE_BUSY".

---

## `POST /api/admin/moderation/bulk`

Acts over the **review queue**, addressed by `tagset_ids`. The filter resolves to
**submitted** appearances only (returned ones are deliberately excluded from bulk
selection so a bulk approve right after a return cannot republish them — see
`docs/architecture/moderation.md`).

### Request

```json
{
  "action": "approve" | "return" | "discard",
  "tagset_ids": [17, 42],
  "filter": { "q": "", "field": "" },
  "all": true,
  "note": "Please fix the album tag."
}
```

| `action` | Effect | Permission |
|----------|--------|------------|
| `approve` | Publish the submitted appearances into the library. This is the plain publish — the per-piece drop-bytes / force-new overrides are single-row, expanded-card decisions on `…/{tagsetID}/approve`, not bulk. | `content.moderate` |
| `return` | Send back to the uploader with a **required** `note` (1–1000 bytes). | `content.moderate` |
| `discard` | Trash the appearance (tagset soft delete — keeps the blob). | `content.moderate` **and** `file.delete` |

Applies the same guarded transitions the single-row endpoints
(`…/{tagsetID}/approve`, `…/{tagsetID}/return`, `…/{tagsetID}/discard`) use — one
batched transaction per action plus a single summary audit row — so the
from-state guards are identical. Returns `{ "ok": true, "affected": N }`.

---

## `POST /api/my/uploads/bulk`

Acts over the **caller's own staged appearances** (`draft` + `returned`; a
`submitted` one can't be withdrawn), addressed by `tagset_ids`. Owner-scoped — a
tagset the caller doesn't own is simply not found (counts toward neither
`affected` nor an error).

### Request

```json
{
  "action": "submit" | "remove",
  "tagset_ids": [17, 42],
  "filter": { "q": "", "field": "" },
  "all": true
}
```

| `action` | Effect |
|----------|--------|
| `submit` | Send to approval. Shares the `/api/my/uploads/submit` semantics: a `content.moderate` holder **self-approves** their own non-duplicate submissions, but a **duplicate-flagged** one (its audio already in the library — classification case B/C) always goes to the queue for a human look. |
| `remove` | Discard the staged appearance to Trash (the owner-scoped tagset soft delete). |

### Response

- `submit`: `{ "ok": true, "submitted": N, "approved": <bool>, "flagged": M }`.
  `approved` is true only when the caller can self-approve **and** nothing was
  flagged. When `flagged > 0`, a `warning` string is added (explaining, for a
  moderator, why self-approve was withheld; for a regular uploader, that a
  moderator will look).
- `remove`: `{ "ok": true, "removed": N }`.

---

## The edit patch

`action: "edit"` (on `appearances/bulk` and `trash/bulk`) carries a `patch` object
applied to **every** appearance in the resolved set. It is the same change-only,
never-clear contract as the per-file tag editor: **only the keys present are
written**; an absent key leaves that column untouched across the whole selection
(so a value the selection disagrees on is left alone). At least one tag or access
field must be present, else `400 "nothing to update"`.

```json
"patch": {
  "title": "…", "album": "…", "album_artist": "…", "artist": "…",
  "genre": "…", "composer": "…", "comment": "…",
  "track_number": "3", "track_total": "12", "disc_number": "1", "year": "1991",
  "license": "CC-BY-4.0",
  "guest": true
}
```

- **Tag fields** — the same set the per-file `PATCH /api/files/{hash}/metadata`
  accepts (numeric fields carried as strings; see `docs/api/metadata.md`). The
  four base tags re-resolve each file's artist/album entity FKs, so a bulk
  `album_artist`/`album` edit **reclassifies** every file in the set into that
  album.
- **`license`** (string) / **`guest`** (bool) — the access fields, mirroring the
  per-file access endpoints. They need the content-access store wired; if it is
  not, an access-bearing patch is `400 "access editing unavailable"`. `license`
  is applied before `guest` (an explicit `guest` wins over any license
  auto-derive).

Because the edit re-resolves each appearance's entities, the tag write applies
per appearance (sharing one transaction per chunk, not one `UPDATE`) and can
**partially** succeed: the response is
`{ "ok": true, "affected": N, "failed": [{ "tagset_id": 12, "error": "…" }] }`, and a
single `metadata.bulk_edit` summary audit row records the count. The single-valued
`license`/`guest` instead collapse to one guarded `UPDATE` each.
(Tags don't clear when absent, so a "set the album for these 40 tracks" edit
leaves their differing titles intact.)

---

## Error responses (all endpoints)

| Status | Condition |
|--------|-----------|
| 400 | Malformed JSON; unknown `action`; both/neither of the id list / `filter`; an invalid id or over-cap id list; `q` over 200 chars; empty filter without `"all": true`; `edit` with an empty patch or an access field with no access store; `return` with a missing/over-long `note`. |
| 401 | Anonymous request (auth configured) — `my/uploads/bulk`. |
| 403 | Authenticated but missing the action's permission. |
| 500 | Storage or database error. |

On success every endpoint returns `200` with `"ok": true` and an action-specific
count (`affected` / `removed` / `submitted`).

---

## Examples

```bash
# Move every appearance matching "bootleg" to Trash (admin library)
curl -X POST -H "Content-Type: application/json" -b cookies.txt \
  -d '{"action":"trash","filter":{"q":"bootleg"}}' \
  "http://localhost:3000/api/admin/appearances/bulk"

# Set the album-artist on a hand-picked selection (reclassifies them)
curl -X POST -H "Content-Type: application/json" -b cookies.txt \
  -d '{"action":"edit","tagset_ids":[12,34],"patch":{"album_artist":"Nirvana"}}' \
  "http://localhost:3000/api/admin/appearances/bulk"

# Restore everything in Trash (the whole-set guardrail)
curl -X POST -H "Content-Type: application/json" -b cookies.txt \
  -d '{"action":"restore","filter":{"q":""},"all":true}' \
  "http://localhost:3000/api/admin/trash/bulk"

# Approve all submitted files matching a filter (moderation)
curl -X POST -H "Content-Type: application/json" -b cookies.txt \
  -d '{"action":"approve","filter":{"q":"live at"}}' \
  "http://localhost:3000/api/admin/moderation/bulk"

# Send all of my staged files for review
curl -X POST -H "Content-Type: application/json" -b cookies.txt \
  -d '{"action":"submit","filter":{"q":""},"all":true}' \
  "http://localhost:3000/api/my/uploads/bulk"
```

---

## See also

- `docs/api/metadata.md` — the per-file `PATCH …/metadata` the `edit` patch
  mirrors (tag fields, presence semantics, entity reclassification).
- `docs/architecture/file-list-scaling.md` — server-side paging, the
  "Select all N matching" selection model, and the transactional batch design.
- `docs/architecture/moderation.md` — the review/staging flow `moderation/bulk`
  and `my/uploads/bulk` act on (why returned files are excluded, self-approve,
  duplicate-flagging).
- `docs/architecture/file-management-view.md` — the shared scope-driven file list
  whose bulk toolbar drives these endpoints.
