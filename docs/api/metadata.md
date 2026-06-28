# Metadata Editing API

Editing the tags of an already-uploaded file.

This is the server side of the edit-tags modal (shared `track-edit.js`): after
files are uploaded and grouped into albums (by their embedded tags), the user can
correct what tag extraction got wrong. The modal shows the four base tags plus
the track number, with an **"Extended edit"** disclosure for the rarely-touched
tags (year, track total, disc number, genre, composer, comment).

---

## `GET /api/files/{hash}/metadata`

Returns the full editable tag set for one file, for the edit modal to prefill
before editing (so it can save the extended fields without clobbering ones the
user never sees). Requires `metadata.edit`. The response body is identical to the
`PATCH` echo below; `404` when no file matches `hash`.

The owner-scoped twin `GET /api/my/uploads/{hash}/metadata` returns the same shape
for the uploader's own staged (draft/returned) files; gated by `file.upload` +
ownership, `404` on anything the caller may not see (same guard as the PATCH).

---

## `PATCH /api/files/{hash}/metadata`

Updates the tags on one file's `media_metadata` row.

### Request

```
PATCH /api/files/{hash}/metadata
Content-Type: application/json
```

| Parameter | In   | Required | Description |
|-----------|------|----------|-------------|
| `hash`    | path | yes      | The file content hash (the `hash` returned by `POST /files/upload`). |

Body — a JSON object. **Every field is optional**; only keys that are *present*
are written. Pointer/presence semantics:

- **absent key** → the column is left unchanged.
- **`"field": ""`** (empty string) → the column is cleared (stored `NULL`).
- **`"field": "value"`** → the column is set to `value`.

Numeric fields are sent **as strings** so they share the same trichotomy: `""`
clears the column, and a value must be a non-negative integer (a malformed value
is a `400`). Any unrecognised key is **ignored**.

```json
{
  "title": "Breed",
  "album": "Nevermind",
  "album_artist": "Nirvana",
  "artist": "Nirvana",
  "track_number": "3",
  "year": "1991",
  "genre": "Grunge"
}
```

| Field          | Type           | Description |
|----------------|----------------|-------------|
| `title`        | string         | Track title. |
| `album`        | string         | Album title. |
| `album_artist` | string         | Album artist (the field albums are grouped by). |
| `artist`       | string         | Track artist. |
| `track_number` | string (int)   | Track number on the disc. |
| `track_total`  | string (int)   | Total tracks on the disc. |
| `disc_number`  | string (int)   | Disc number for multi-disc releases. `NULL` (untagged), `0`, and `N` are three *distinct* discs — see `docs/architecture/disc-numbering.md`. |
| `year`         | string (int)   | Release year. |
| `genre`        | string         | Genre tag. |
| `composer`     | string         | Composer tag. |
| `comment`      | string         | Free-text comment. |

Only the four base fields (`title`/`album`/`album_artist`/`artist`) re-resolve the
artist/album entity FKs; the extended fields are stored as-is.

### Access control

Requires the `metadata.edit` permission (the same gate as cover uploads).
Anonymous requests get `401`; an authenticated user lacking the permission gets
`403`. The gate is a pass-through only when auth is not configured.

### Behaviour

- Writes the supplied fields to the file's `media_metadata` row and returns the
  resulting base fields. An empty body (no fields) is a no-op that still echoes
  the current values.
- **Editing a track's tags reclassifies that track.** The `album_artist`/`album`
  strings resolve to artist/album entities, so changing them moves the track to
  whichever artist/album its new tags resolve to (its `artist_id`/`album_id` are
  re-resolved in the same transaction). The file's raw tag text is the overlay's
  input — it is not silently migrated. To rename an artist/album *as a whole*
  (keeping every track), use the entity rename below instead.
- This endpoint **does not touch `album_images`.** Covers attach to the stable
  album/artist **entity id**, not to tag strings, so a re-tagged track simply
  joins its new album's existing cover (if any), and an entity *rename* keeps its
  cover with no re-upload. (Pre-entity, covers were string-keyed and a rename
  orphaned them; that re-POST dance is gone — see
  `docs/architecture/artist-album-model.md`.)
- The change is recorded in the audit log as action `metadata.edit`, target
  `file:<hash>`.

### Response

`200 OK` with `Content-Type: application/json`:

```json
{
  "ok": true,
  "hash": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
  "title": "Breed",
  "album": "Nevermind",
  "album_artist": "Nirvana",
  "artist": "Nirvana",
  "genre": "Grunge",
  "composer": "",
  "comment": "",
  "track_number": 3,
  "track_total": 12,
  "disc_number": 1,
  "year": 1991
}
```

The echoed fields reflect the row *after* the update: string fields are `""` when
cleared or never-set, numeric fields are `null` when unset (and a JSON number
otherwise). This is also the exact shape returned by the two `GET` endpoints.

### Error responses

| Status | Condition |
|--------|-----------|
| 400    | Malformed JSON body, or a numeric field that is not a non-negative integer. |
| 401    | Anonymous request (auth configured). |
| 403    | Authenticated but missing `metadata.edit`. |
| 404    | No file matches `hash`. |
| 500    | Storage or database error. |

---

## Examples

```bash
# Rename a track's title only (other tags untouched)
curl -X PATCH -H "Content-Type: application/json" \
  -d '{"title":"Breed"}' \
  "http://localhost:3000/api/files/<hash>/metadata"

# Re-tag a mis-detected album/artist (moves the track between album groupings)
curl -X PATCH -H "Content-Type: application/json" \
  -d '{"album":"Nevermind","album_artist":"Nirvana"}' \
  "http://localhost:3000/api/files/<hash>/metadata"

# Clear a wrongly-extracted album tag
curl -X PATCH -H "Content-Type: application/json" \
  -d '{"album":""}' \
  "http://localhost:3000/api/files/<hash>/metadata"
```

---

## Renaming an artist or album entity

The `PATCH …/metadata` route above edits **one track's** tags, which
*reclassifies* that track (it moves to whichever artist/album its new tags
resolve to). To rename an artist or album **as a whole** — keeping every track
and its cover attached — edit the entity in place instead:

```
POST /api/artists/{artist}/rename
POST /api/albums/{album}/rename?artist=<artist>
```

| Route | Body | Effect |
|-------|------|--------|
| `POST /api/artists/{artist}/rename` | `{"name": "New Name"}` | Renames the artist entity resolved from `{artist}`. |
| `POST /api/albums/{album}/rename` | `{"title": "New Title"}` | Renames the album entity resolved from (`?artist=`, `{album}`). |

The entity is addressed by its **current** display name (same path-segment
scheme as the cover routes, with the same `/`-in-name limitation). The rename is
a one-row update to the entity's display name and dedup key; the tracks and the
cover follow via their foreign keys — no per-track tag rewrite, no cover
re-upload.

Both require the `metadata.edit` permission.

| Status | Condition |
|--------|-----------|
| 200    | Renamed. Body: `{"ok": true, "id": <entity id>, "name"/"title": "<new>"}`. |
| 400    | Missing/empty new name. |
| 404    | No artist/album entity matches the current name. |
| 409    | The new name is already taken by a different entity — that is a *merge*, not a rename (see [Merging](#merging-two-artists-or-albums)). For an album the clash is scoped to the same artist. |

```bash
# Rename an album (cover and all tracks stay attached)
curl -X POST -H "Content-Type: application/json" \
  -d '{"title":"The Dark Side of the Moon"}' \
  "http://localhost:3000/api/albums/Dark%20Side/rename?artist=Pink+Floyd"
```

---

## Merging two artists or albums

When the same artist or album exists under two spellings that normalization did
not unify (e.g. "Beatles" vs "The Beatles"), merge one entity (the **source**)
into another (the **target**). The source's tracks, albums, and covers move onto
the target, then the source is deleted. **This is destructive** (the source
entity is removed) — it is audited as `metadata.merge`.

```
POST /api/artists/{artist}/merge
POST /api/albums/{album}/merge?artist=<artist>
```

The path (and `?artist=` for albums) names the **source**; the body names the
**target**:

| Route | Body | Effect |
|-------|------|--------|
| `POST /api/artists/{artist}/merge` | `{"into": "Target Artist"}` | Moves all of the source artist's tracks/albums onto the target, collapsing albums that share a title (the target's cover wins; the source's fills only a gap), then deletes the source artist. |
| `POST /api/albums/{album}/merge` | `{"into_artist": "...", "into_album": "..."}` | Repoints the source album's tracks onto the target album and its artist, moves the cover only if the target lacks one, then deletes the source album. |

Both require `metadata.edit`. The file's raw tags are left untouched (overlay).

| Status | Condition |
|--------|-----------|
| 200    | Merged. Body: `{"ok": true, "from_id": …, "into_id": …}`. |
| 400    | Missing target, source == target, or the named target does not exist. |
| 404    | The source entity does not exist. |

```bash
# Fold "Beatles" into the canonical "The Beatles"
curl -X POST -H "Content-Type: application/json" \
  -d '{"into":"The Beatles"}' \
  "http://localhost:3000/api/artists/Beatles/merge"
```

### Merging by entity id (preferred for clients)

The name-addressed routes above are convenient for ad-hoc use, but names can
collide and the empty-name/empty-title bucket has no addressable path segment.
Clients (e.g. the admin UI) should use the **id-addressed** variants, which name
both the source and the target by their stable surrogate id from the listing
DTOs (`GET /api/artists` and `GET /api/albums?artist=` both expose `id`):

```
POST /api/artists/merge   {"from_id": 12, "into_id": 4}
POST /api/albums/merge    {"from_id": 87, "into_id": 90}
```

Same effect, gating (`metadata.edit`), and destructiveness as the name-addressed
forms; the source entity is deleted and the merge is audited as `metadata.merge`.

| Status | Condition |
|--------|-----------|
| 200    | Merged. Body: `{"ok": true, "from_id": …, "into_id": …}`. |
| 400    | `from_id`/`into_id` missing or non-positive, or source == target. |
| 404    | An entity id does not exist. |

```bash
# Fold artist #12 into artist #4
curl -X POST -H "Content-Type: application/json" \
  -d '{"from_id":12,"into_id":4}' \
  "http://localhost:3000/api/artists/merge"
```

## See also

- `docs/api/upload.md` — file upload and the `{title, album, artist}` echo the
  verify panel groups on.
- `docs/api/cover-images.md` — cover upload + variant status (covers follow an
  album/artist rename automatically, via the entity id).
- `docs/architecture/artist-album-model.md` — the artist/album entity model that
  rename/merge and entity-keyed covers build on.
- `docs/architecture/file-management-view.md` — the shared file-list view and the
  **bulk** tag editor (this same PATCH applied across a selection; shared values
  pre-filled, only changed fields written, base + extended tags).
- `docs/api/bulk.md` — the bulk endpoints that apply this patch (and trash /
  restore / moderation / staging actions) across a selection or a whole filter.
