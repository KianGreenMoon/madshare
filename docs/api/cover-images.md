# Cover Image API

Endpoints for querying album cover variant readiness and the UI upload
configuration.

Cover images use a content-addressed **source/derivative split** (full design:
`docs/architecture/variants.md`). The **source original** — a JPEG/PNG — is stored
under `<files_dir>/images/<image_hash>/original<ext>`, keyed by the **full**
`sha256` of its bytes; it is a regenerate seed and is **never served**. An
`image_processing_jobs` row is enqueued and a worker pool (`imageproc.Pool`)
generates eight square **derived variants** (crop + fit at 64 / 150 / 300 / 600 px)
under `<variants_dir>/images/<image_hash>/<recipe><ext>` — served at `/images`.
The status endpoint reports whether that generation has finished.

The album-cover endpoint `GET /api/albums/{album_id}/image` serves a derived
**crop variant** — never the original — and 404s until variants are ready (the UI
shows its placeholder in the gap). An optional `?size=` query param picks which
crop is served: `thumb` (64 px), `small` (150 px), `medium` (300 px), or the
default `large` (600 px) when omitted/unknown. The library and admin thumbnails
request `?size=small`; cmus and the mobile app use the `large` default. Artist
images keep **no variant pipeline** (deferred): their original is stored and
served directly under the flat `<image_hash><ext>` key (and ignore `?size=`).

**Storage model.** These endpoints address albums/artists by entity id in the URL
(the by-name `POST` write paths resolve-or-create the entity), but covers are
stored keyed by the **album/artist entity id** (`album_images.album_id` /
`artist_images.artist_id`), not by name strings. A cover therefore attaches to a
stable id: an admin rename of the album/artist keeps the cover attached with no
re-upload. The on-disk `<image_hash>/` layout and the `image_processing_jobs`
queue are keyed by the full image hash. See
`docs/architecture/artist-album-model.md` (cover re-keying).

**Startup recovery.** Before listeners accept traffic, idempotent passes upgrade
and recover covers:

- `db.SplitImageSources` is the data half of the source/derivative split: for
  every album cover still keyed by a legacy 16-char `base_key` it recomputes the
  full image hash from the stored bytes, writes the source original out to
  `<files_dir>/images/<image_hash>/original<ext>`, re-keys the row (with
  `variants_ready=0`), and drops the old `<base_key>/` directory. Regeneration of
  its variants then falls to `RequeueStuckImageJobs` below.
- `db.ResetStaleJobs` returns any `running` job to `pending` — recovers jobs that
  were in flight when the process died.
- `db.RequeueStuckImageJobs` re-enqueues a job for every `album_images` row at
  `variants_ready=0` that has **no** job in the queue. This recovers the rare
  case where the cover row was claimed but the subsequent `EnqueueImageJob`
  errored (a DB-level failure), and regenerates the covers `SplitImageSources`
  just re-keyed; the source original is written *before* the row is claimed, so a
  fresh job is all that is needed. An `image_hash` whose job is already
  `pending`/`running` is skipped (in flight), and one marked `failed` is left
  terminal — it was retried `maxImageJobRetries` times (typically a
  corrupt/mislabelled embedded cover), so re-enqueuing it would retry corrupt
  images on every restart. Albums sharing one cover (same `image_hash`) collapse
  to a single job.
- `db.ReconcileImageOrphans` removes album-cover directories — variant dirs under
  `<variants_dir>/images/<image_hash>/` **and** the matching source-original dirs
  under `<files_dir>/images/<image_hash>/` — that no `album_images`/`artist_images`
  row and no active (`pending`/`running`) job references. These accumulate when a
  cover is replaced with different bytes or from a distinct-art race loser.
  Artist covers are flat `<image_hash><ext>` files (no variant directory), so the
  directory sweep never touches them.

---

## `GET /api/albums/{album_id}/image/status`

Returns the variant-readiness state and every **derived** variant URL for an album
cover.

### Request

```
GET /api/albums/{album_id}/image/status
```

| Parameter  | In   | Required | Description |
|------------|------|----------|-------------|
| `album_id` | path | yes      | The album entity id (`albums.id`); browse DTOs carry it. Must be a positive integer. |

### Access control

None — public, matching `GET /api/albums/{album_id}/image`. The upload UI calls it
for every detected album group, including before login.

### Response

HTTP 200 with `Content-Type: application/json`.

```json
{
  "has_cover": true,
  "variants_ready": false,
  "image_hash": "e2ce7eb8c06762a9a8cc7938f405211a21b4bb9a0fea442d427801bbbfa842e9",
  "source_ext": ".jpg",
  "variants": {
    "thumb_crop":  "/images/e2ce…842e9/thumb_crop.jpg",
    "thumb_fit":   "/images/e2ce…842e9/thumb_fit.jpg",
    "small_crop":  "/images/e2ce…842e9/small_crop.jpg",
    "small_fit":   "/images/e2ce…842e9/small_fit.jpg",
    "medium_crop": "/images/e2ce…842e9/medium_crop.jpg",
    "medium_fit":  "/images/e2ce…842e9/medium_fit.jpg",
    "large_crop":  "/images/e2ce…842e9/large_crop.jpg",
    "large_fit":   "/images/e2ce…842e9/large_fit.jpg"
  }
}
```

| Field            | Type    | Description |
|------------------|---------|-------------|
| `has_cover`      | boolean | Whether an `album_images` row exists for the album. |
| `variants_ready` | boolean | `true` once the worker has generated all variants. |
| `image_hash`     | string  | Full SHA-256 of the source original; `""` when no cover or for legacy rows predating variants. |
| `source_ext`     | string  | `.jpg` or `.png` — the extension of both the original and its variants. |
| `variants`       | object  | Map of **derived** variant name → `/images/…` URL. The source `original` is **never** included — it is not served. |

Behaviour:

- **No cover** (`has_cover: false`): `variants_ready` is `false`, `image_hash` is
  `""`, and `variants` is `{}`.
- **Legacy rows** (cover exists but `image_hash` is empty, written before variants
  existed): `variants` is `{}` — there are no deterministic variant paths.
- **Not yet ready** (`variants_ready: false` with an `image_hash`): the variant
  URLs are still returned. They are deterministic and some files may already exist
  partially, so the client is responsible for not displaying images until
  `variants_ready` is `true`. A typical client polls this endpoint every ~2 s
  until ready.

### Variants

The eight **derived** variants — all served at `/images`. The source `original`
is **not** a variant: it is stored under `<files_dir>/images` and never served.

| Name          | Size      | Mode | Padding (fit only) |
|---------------|-----------|------|--------------------|
| `thumb_crop`  | 64×64     | crop | — |
| `thumb_fit`   | 64×64     | fit  | white (JPEG) / transparent (PNG) |
| `small_crop`  | 150×150   | crop | — |
| `small_fit`   | 150×150   | fit  | white / transparent |
| `medium_crop` | 300×300   | crop | — |
| `medium_fit`  | 300×300   | fit  | white / transparent |
| `large_crop`  | 600×600   | crop | — |
| `large_fit`   | 600×600   | fit  | white / transparent |

`crop` is a center-cropped square; `fit` preserves aspect ratio inside the square
and pads the remainder. `GET /api/albums/{album_id}/image` serves one of the crop
variants, selected by `?size=` (`thumb`/`small`/`medium`/`large`, default
`large` → `large_crop`).

### Error responses

| Status | Condition |
|--------|-----------|
| 500    | Internal storage error. |

---

## `GET /api/ui/config`

Serves the parsed `webui.toml` (the upload page's client-side worker controls).

### Request

```
GET /api/ui/config
```

No parameters.

### Access control

None — public. The upload UI needs these values before login. When the server
was started without a `webui.toml`, built-in defaults are returned.

### Response

HTTP 200 with `Content-Type: application/json`.

```json
{
  "upload": {
    "default_parallel_workers": 3,
    "max_parallel_workers": 10
  }
}
```

| Field                            | Type    | Default | Description |
|----------------------------------|---------|---------|-------------|
| `upload.default_parallel_workers`| integer | 3       | Initial value of the upload page's concurrency slider. |
| `upload.max_parallel_workers`    | integer | 10      | Ceiling the slider can be raised to. |

Values are clamped at load time (`config.LoadWebUI`): each falls back to its
default when zero, sub-1 values clamp to 1, and `max_parallel_workers` is raised
to `default_parallel_workers` if configured below it.

---

## `POST /api/albums/{album}/image`

Uploads (or replaces) an album cover and triggers asynchronous variant
generation. Extended in Phase 3 of the upload & covers work.

### Request

```
POST /api/albums/{album}/image?artist=<album_artist>
Content-Type: multipart/form-data
```

| Part / param | In       | Required | Description |
|--------------|----------|----------|-------------|
| `image`      | body     | yes      | The cover file (`multipart/form-data`). Max 10 MB. |
| `album`      | path     | yes      | Album title (same path-segment limitation as the status route). |
| `artist`     | query    | no       | Album artist; resolved to the album-artist entity (empty = unknown-artist bucket). |

**Accepted formats: JPEG and PNG only.** WebP and any other type are rejected
with `400`. The extension is canonicalised to `.jpg` / `.png` (a `.jpeg` upload
is stored as `original.jpg`), since the worker and status route assume those
names. (`GET .../image` and `.../image/status` are **id**-addressed —
`{album_id}` — while this write path stays **name**-addressed, since it
resolve-or-creates the entity before it has a browsable id.)

### Access control

Requires **`metadata.edit` OR `file.upload`** (`RequireAnyPermission`;
pass-through when auth is not configured). A caller holding only `file.upload`
(an uploader) may set a cover **only when none exists** — trying to overwrite an
existing cover returns **403** (`coverReplaceBlocked`), so replacing stays a
`metadata.edit` action. This lets an uploader add art to a staged draft from the
grouped "Add cover" affordance ([file-management view](../architecture/file-management-view.md))
without granting them the power to change covers already on the library.

### Behaviour

Unlike embedded cover extraction (which only fills a missing cover), a manual
upload **always replaces** the current cover — *explicit beats embedded*. The
source original is written to `<files_dir>/images/<image_hash>/original<ext>`
(keyed by the full SHA-256 of the bytes), the `album_images` row is updated with
the new `image_hash`/`source_ext` (and `variants_ready` reset to 0), and a variant
job is enqueued. The enqueue is idempotent per `image_hash`, so re-uploading the
same image does not double-queue.

Replacing a cover with a *different* image leaves the previous original (and its
variants) on disk until the next startup, when `db.ReconcileImageOrphans` sweeps
unreferenced `<image_hash>/` directories in **both** the source and variants trees
(see "Startup recovery" above).

### Response

`200 OK`:

```json
{ "ok": true, "processing": true }
```

`processing` is always `true` — poll
`GET /api/albums/{album_id}/image/status` until `variants_ready` is `true`.

### Error responses

| Status | Condition |
|--------|-----------|
| 400    | Missing `image` part, body over 10 MB, or unsupported type/extension (incl. WebP). |
| 401    | Anonymous (auth configured). |
| 403    | Authenticated but holding neither `metadata.edit` nor `file.upload`; or a `file.upload`-only caller trying to **replace** an existing cover. |
| 500    | Storage or database error. |

---

## `POST /api/artists/{artist}/image`

Uploads an artist image. Artist covers have **no variant pipeline yet**
(deferred), so the original is stored and served directly under the flat key
`<image_hash><ext>` (no source/derivative split) and no job is enqueued. Same JPEG/PNG-only validation and the same
`metadata.edit`-OR-`file.upload` gate (with the add-only rule for an
upload-only caller) as the album endpoint. Returns `{ "ok": true }`.

---

## Examples

```bash
# Upload an album cover (triggers variant generation)
curl -X POST -F "image=@./cover.jpg" \
  "http://localhost:3000/api/albums/Dark%20Side/image?artist=Pink%20Floyd"

# Poll an album cover's variant readiness (id-addressed; 42 = albums.id)
curl "http://localhost:3000/api/albums/42/image/status"

# Fetch the UI upload configuration
curl "http://localhost:3000/api/ui/config"
```

## See also

- `docs/api/upload.md` — file upload, deduplication, restore-on-reupload, and
  embedded cover-art extraction (the other way covers enter the system).
- `docs/architecture/artist-album-model.md` — the entity model covers are keyed
  to; WebP and artist-cover variants are deferred (`docs/plans/roadmap.md`).
