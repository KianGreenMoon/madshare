# Cover Image API

Endpoints for querying album cover variant readiness and the UI upload
configuration. Added in Phase 1 of the upload & covers work
(`docs/plans/upload-and-covers.md`).

Cover images are processed asynchronously: a JPEG/PNG original is stored under
`<files_dir>/images/<base_key>/original<ext>`, an `image_processing_jobs` row is
enqueued, and a worker pool (`imageproc.Pool`) generates eight square variants
(crop + fit at 64 / 150 / 300 / 600 px) into the same `<base_key>/` directory.
The status endpoint reports whether that generation has finished.

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
| `artist`  | query | no       | Album artist (`album_artist`). Empty string matches rows with an empty `album_artist`. |

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

## Examples

```bash
# Poll an album cover's variant readiness
curl "http://localhost:3000/api/albums/Album%20Title/image/status?artist=Some%20Artist"

# Fetch the UI upload configuration
curl "http://localhost:3000/api/ui/config"
```

## See also

- `docs/plans/upload-and-covers.md` — full design and phasing.
- `POST /api/albums/{album}/image` — upload/replace a cover (extended in Phase 3
  to enqueue a variant job).
