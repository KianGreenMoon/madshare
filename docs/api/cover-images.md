# Cover Image API

Endpoints for querying album cover variant readiness and the UI upload
configuration. Added in Phase 1 of the upload & covers work
(`docs/plans/upload-and-covers.md`).

Cover images are processed asynchronously: a JPEG/PNG original is stored under
`<files_dir>/images/<base_key>/original<ext>`, an `image_processing_jobs` row is
enqueued, and a worker pool (`imageproc.Pool`) generates eight square variants
(crop + fit at 64 / 150 / 300 / 600 px) into the same `<base_key>/` directory.
The status endpoint reports whether that generation has finished.

**Storage model.** These endpoints address albums/artists by name in the URL for
backward compatibility, but covers are stored keyed by the **album/artist entity
id** (`album_images.album_id` / `artist_images.artist_id`), not by the name
strings — the handlers resolve the name to its entity (normalized) on each call.
A cover therefore attaches to a stable id: an admin rename of the album/artist
keeps the cover attached with no re-upload. The on-disk `<base_key>/` layout and
the `image_processing_jobs` queue are unaffected (keyed by `base_key`). See
`docs/plans/artist-album-normalization.md` (Phase 4).

---

## `GET /api/albums/{album}/image/status`

Returns the variant-readiness state and every variant URL for an album cover.

### Request

```
GET /api/albums/{album}/image/status?artist=<album_artist>
```

| Parameter | In    | Required | Description |
|-----------|-------|----------|-------------|
| `album`   | path  | yes      | Album title. **Known limitation:** taken from the path segment, so titles containing `/` do not round-trip, and the empty-string ("Other") bucket cannot be expressed. This mirrors `GET /api/albums/{album}/image`. |
| `artist`  | query | no       | Album artist. Resolved to the album-artist entity; empty string matches the unknown-artist bucket. |

### Access control

None — public, matching `GET /api/albums/{album}/image`. The upload UI calls it
for every detected album group, including before login.

### Response

HTTP 200 with `Content-Type: application/json`.

```json
{
  "has_cover": true,
  "variants_ready": false,
  "base_key": "a3f1c8d2e4b7f901",
  "source_ext": ".jpg",
  "variants": {
    "original":    "/images/a3f1c8d2e4b7f901/original.jpg",
    "thumb_crop":  "/images/a3f1c8d2e4b7f901/thumb_crop.jpg",
    "thumb_fit":   "/images/a3f1c8d2e4b7f901/thumb_fit.jpg",
    "small_crop":  "/images/a3f1c8d2e4b7f901/small_crop.jpg",
    "small_fit":   "/images/a3f1c8d2e4b7f901/small_fit.jpg",
    "medium_crop": "/images/a3f1c8d2e4b7f901/medium_crop.jpg",
    "medium_fit":  "/images/a3f1c8d2e4b7f901/medium_fit.jpg",
    "large_crop":  "/images/a3f1c8d2e4b7f901/large_crop.jpg",
    "large_fit":   "/images/a3f1c8d2e4b7f901/large_fit.jpg"
  }
}
```

| Field            | Type    | Description |
|------------------|---------|-------------|
| `has_cover`      | boolean | Whether an `album_images` row exists for the album. |
| `variants_ready` | boolean | `true` once the worker has generated all variants. |
| `base_key`       | string  | 16-char SHA-256 prefix of the original image; `""` when no cover or for legacy rows predating variants. |
| `source_ext`     | string  | `.jpg` or `.png` — the extension of both the original and its variants. |
| `variants`       | object  | Map of variant name → `/images/…` URL. |

Behaviour:

- **No cover** (`has_cover: false`): `variants_ready` is `false`, `base_key` is
  `""`, and `variants` is `{}`.
- **Legacy rows** (cover exists but `base_key` is empty, written before variants
  existed): `variants` is `{}` — there are no deterministic variant paths.
- **Not yet ready** (`variants_ready: false` with a `base_key`): the variant URLs
  are still returned. They are deterministic and some files may already exist
  partially, so the client is responsible for not displaying images until
  `variants_ready` is `true`. A typical client polls this endpoint every ~2 s
  until ready.

### Variants

| Name          | Size      | Mode | Padding (fit only) |
|---------------|-----------|------|--------------------|
| `original`    | as-is     | —    | — |
| `thumb_crop`  | 64×64     | crop | — |
| `thumb_fit`   | 64×64     | fit  | white (JPEG) / transparent (PNG) |
| `small_crop`  | 150×150   | crop | — |
| `small_fit`   | 150×150   | fit  | white / transparent |
| `medium_crop` | 300×300   | crop | — |
| `medium_fit`  | 300×300   | fit  | white / transparent |
| `large_crop`  | 600×600   | crop | — |
| `large_fit`   | 600×600   | fit  | white / transparent |

`crop` is a center-cropped square; `fit` preserves aspect ratio inside the square
and pads the remainder.

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
names.

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
original is written to `<files_dir>/images/<base_key>/original<ext>`, the
`album_images` row is updated with the new `base_key`/`source_ext` (and
`variants_ready` reset to 0), and a variant job is enqueued. The enqueue is
idempotent per `base_key`, so re-uploading the same image does not double-queue.

Replacing a cover with a *different* image leaves the previous original on disk
as a harmless orphan (see `.issues/open-issues.md`).

### Response

`200 OK`:

```json
{ "ok": true, "processing": true }
```

`processing` is always `true` — poll
`GET /api/albums/{album}/image/status` until `variants_ready` is `true`.

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
(deferred), so the original is stored under the flat key `<base_key><ext>` and no
job is enqueued. Same JPEG/PNG-only validation and the same
`metadata.edit`-OR-`file.upload` gate (with the add-only rule for an
upload-only caller) as the album endpoint. Returns `{ "ok": true }`.

---

## Examples

```bash
# Upload an album cover (triggers variant generation)
curl -X POST -F "image=@./cover.jpg" \
  "http://localhost:3000/api/albums/Dark%20Side/image?artist=Pink%20Floyd"

# Poll an album cover's variant readiness
curl "http://localhost:3000/api/albums/Album%20Title/image/status?artist=Some%20Artist"

# Fetch the UI upload configuration
curl "http://localhost:3000/api/ui/config"
```

## See also

- `docs/api/upload.md` — file upload, deduplication, restore-on-reupload, and
  embedded cover-art extraction (the other way covers enter the system).
- `docs/plans/upload-and-covers.md` — full design and phasing.
