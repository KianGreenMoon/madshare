# Upload & Covers — Implementation Plan

**Status:** Ready to implement. All decisions are locked.  
**Branch target:** `aidev` → merge to `develop`  
**Module path:** `daemonlord.ygg/madshare`

---

## Locked Decisions (do not re-ask)

| Topic | Decision |
|---|---|
| Resize modes | Generate **both** `_crop` (square center-crop) and `_fit` (fit inside square, padded) at every size |
| Output format | JPEG and PNG only. JPEG source → JPEG variants; PNG source → PNG variants. WebP is **not accepted** |
| Fit padding | White for JPEG, transparent for PNG |
| Cover priority | Explicit uploaded file **beats** embedded tag art. Fill-if-missing: never overwrite an existing cover |
| Async processing | Yes — goroutine worker pool, DB-backed job queue |
| Image worker pool | `image_processing_workers` is **optional** (sizes the CPU-bound variant-resize pool). Unset/`0` → **auto** = `runtime.NumCPU()`; explicit `≥1` honored as-is; `<0` → auto + warn. No upper bound, never fatal. Decoupled from upload concurrency on purpose (resize is CPU-bound, uploads are I/O-bound) |
| Artist covers | **Deferred** — schema reserves space but no implementation in this plan |
| Upload page | `/upload` route in webui; one cover preview card per detected album group |
| Aura effect | Client-side canvas only (Phase 6, last) |
| Manual cover upload | API already exists (`POST /api/albums/{album}/image`); extend to trigger async job; also exposed in the upload page UI |
| Upload mechanism | Parallel single-file `POST /files/upload` requests from client; no batch endpoint |
| Server concurrency | `server_max_parallel_workers` (global, in `[storage]`) + `user_max_parallel_workers` (per-user, in `[storage]`); 0/unset = unlimited |
| Admin exception | **None** — both `server_max_parallel_workers` and `user_max_parallel_workers` apply to every user, including admins. (The `Identity` model has no admin signal to key a bypass on; revisit if a role field is added.) |
| Rate limit response | 429 with JSON `{"error":"…","code":"upload_limit"}`; client auto-decrements workers and re-queues |
| UI config file | `webui.toml` — separate file, read once at startup; values served via `GET /api/ui/config` |
| Client concurrency | Controlled by user via slider in upload UI; defaults/max come from `webui.toml` |
| Batch file types | All file types accepted (audio, images, etc.); unsupported types rejected inline per file |
| Upload code layout | **No subpackage.** Upload handlers live in a new `api/upload_handlers.go` in the existing `api` package (following the `*_handlers.go` convention). A subpackage would force exporting the shared `handler` struct + helpers (`audit`, `sanitizeFilename`, `extractTagsOrEmpty`, `tagsToMetadata`, `writeJSON`, MIME allowlists) for no isolation gain. Revisit only if upload grows independent machinery — resumable/chunked uploads, a tus-style protocol, or federation push |
| WebP covers | **Deferred** — rejected at the upload boundary in v0; clean non-breaking addition later (see §1k) |

---

## Phase Execution Order

Phases depend on each other strictly in this order. Do **not** start a phase until the previous is complete and its tests pass.

```
Phase 1 — Image variant system (library, config, DB, worker, status API)
    ↓
Phase 2 — Embedded cover extraction during audio upload
    ↓
Phase 3 — Manual cover upload extension (server-side)
    ↓
Phase 4 — Upload concurrency & rate limiting
    ↓
Phase 5 — Upload page (/upload, webui)
    ↓
Phase 6 — Aura effect (frontend, client-side)
```

---

## Phase 1 — Image Variant System

### Goal
Establish the full image processing pipeline: resize library, config, DB schema, async worker pool, and a status query API. Everything else builds on top of this.

---

### 1a. Add `disintegration/imaging` dependency

```bash
go get github.com/disintegration/imaging
```

Verify it appears in `go.mod` and `go.sum`.

---

### 1b. Config: extend `[storage]` and create `webui.toml`

**Do not add an `[images]` section.** All new server-side params go in the existing `[storage]` section.

**File to modify:** `config/config.go`

Extend `StorageConfig` with three new fields:

```go
type StorageConfig struct {
    FilesDir                 string `toml:"files_dir"`
    MaxUploadMB              int    `toml:"max_upload_mb"`
    ServerMaxParallelWorkers int    `toml:"server_max_parallel_workers"` // 0 = unlimited
    UserMaxParallelWorkers   int    `toml:"user_max_parallel_workers"`   // 0 = unlimited; admins bypass
    ImageProcessingWorkers   int    `toml:"image_processing_workers"`    // 0 = auto (runtime.NumCPU()); CPU-bound variant-resize pool
}
```

Add defaults in `defaults()`:

```go
Storage: StorageConfig{
    // existing defaults unchanged …
    ServerMaxParallelWorkers: 0,
    UserMaxParallelWorkers:   0,
    ImageProcessingWorkers:   0, // 0 = auto; resolved to runtime.NumCPU() in Load
},
```

Add validation in `config.Load` after existing storage validation:

```go
// image_processing_workers: 0/unset → auto (number of CPUs); explicit ≥1 honored;
// negative is invalid → auto + warn. runtime.NumCPU() always returns ≥1.
if c.Storage.ImageProcessingWorkers == 0 {
    c.Storage.ImageProcessingWorkers = runtime.NumCPU()
} else if c.Storage.ImageProcessingWorkers < 0 {
    log.Printf("config: storage.image_processing_workers %d is invalid, using auto (NumCPU=%d)", c.Storage.ImageProcessingWorkers, runtime.NumCPU())
    c.Storage.ImageProcessingWorkers = runtime.NumCPU()
}
if c.Storage.ServerMaxParallelWorkers < 0 {
    log.Printf("config: storage.server_max_parallel_workers %d is invalid, using 0 (unlimited)", c.Storage.ServerMaxParallelWorkers)
    c.Storage.ServerMaxParallelWorkers = 0
}
if c.Storage.UserMaxParallelWorkers < 0 {
    log.Printf("config: storage.user_max_parallel_workers %d is invalid, using 0 (unlimited)", c.Storage.UserMaxParallelWorkers)
    c.Storage.UserMaxParallelWorkers = 0
}
```

Add `"runtime"` to the imports in `config/config.go` (used by the auto-default above).

Update `madshare.toml.example` `[storage]` block:

```toml
[storage]
files_dir     = "./data/files"
max_upload_mb = 500
# server_max_parallel_workers = 0   # total concurrent uploads from all users (0 = unlimited)
# user_max_parallel_workers   = 0   # concurrent uploads per user (0 = unlimited; admins always exempt)
# image_processing_workers    = 0   # cover-image resize goroutines; 0 = auto (number of CPUs)
```

**New file: `webui.toml.example`** (copy to `webui.toml` to use; add `webui.toml` to `.gitignore`):

```toml
# webui.toml — UI-side upload configuration. Read once at startup.
# Copy to webui.toml and edit as needed.

[upload]
default_parallel_workers = 3   # default concurrent upload connections shown to the user
max_parallel_workers     = 10  # ceiling the user can raise the slider to in the upload UI
```

**New file: `config/webui_config.go`**

```go
package config

type WebUIUploadConfig struct {
    DefaultParallelWorkers int `toml:"default_parallel_workers"`
    MaxParallelWorkers     int `toml:"max_parallel_workers"`
}

type WebUIConfig struct {
    Upload WebUIUploadConfig `toml:"upload"`
}

// LoadWebUI reads the webui.toml at path. If the file does not exist, built-in
// defaults are returned with no error. Returns an error only on parse failure.
func LoadWebUI(path string) (*WebUIConfig, error)
```

Defaults when file is absent or field is zero:
- `DefaultParallelWorkers = 3`
- `MaxParallelWorkers = 10`

Validation (non-fatal, log and clamp):
- `DefaultParallelWorkers < 1` → clamp to 1
- `MaxParallelWorkers < 1` → clamp to 1
- `MaxParallelWorkers < DefaultParallelWorkers` → clamp `MaxParallelWorkers` up to `DefaultParallelWorkers`

Add a `-webui-config` flag to `madshare.go` alongside the existing `-config` flag. Load `WebUIConfig` in `main` immediately after `config.Load`; pass it into `api.Deps`.

**New API endpoint: `GET /api/ui/config`**

Public — no auth required (the UI needs it before login). Returns `WebUIConfig` as JSON:

```json
{
    "upload": {
        "default_parallel_workers": 3,
        "max_parallel_workers": 10
    }
}
```

Register in `api.RegisterAPI`:

```go
r.Get("/api/ui/config", h.getUIConfig)
```

Add `webUIConfig *config.WebUIConfig` field to `handler` struct; wire from `Deps.WebUIConfig`.

Update `CLAUDE.md` config section to document the three new `[storage]` fields and `webui.toml`.

---

### 1c. DB migration 009

**File to create:** `database/migrations/009_image_variants.sql`

```sql
-- Extend album_images with variant tracking columns.
-- object_key retains its original meaning (original image path) for backward compat.
-- base_key is the 16-char SHA-256 prefix used to derive all variant paths.
-- source_ext is ".jpg" or ".png" — the extension of the original file (WebP not accepted).
-- variants_ready is 0 until the async worker has generated all variants.
ALTER TABLE album_images ADD COLUMN base_key TEXT;
ALTER TABLE album_images ADD COLUMN source_ext TEXT;
ALTER TABLE album_images ADD COLUMN variants_ready INTEGER NOT NULL DEFAULT 0;

-- Artist images reserved for future use (no worker implementation in this plan).
ALTER TABLE artist_images ADD COLUMN base_key TEXT;
ALTER TABLE artist_images ADD COLUMN source_ext TEXT;
ALTER TABLE artist_images ADD COLUMN variants_ready INTEGER NOT NULL DEFAULT 0;

-- Async job queue for image variant generation.
CREATE TABLE image_processing_jobs (
    id          INTEGER PRIMARY KEY,
    cover_type  TEXT    NOT NULL,  -- "album" (artist deferred)
    -- For album: "<album_artist>\x1f<album_title>" (unit separator, not slash, to avoid ambiguity)
    subject_key TEXT    NOT NULL,
    base_key    TEXT    NOT NULL,
    status      TEXT    NOT NULL DEFAULT 'pending', -- pending | running | done | failed
    error       TEXT,              -- last error message if status=failed
    retry_count INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL,
    started_at  INTEGER,
    finished_at INTEGER
);

CREATE INDEX idx_imgproc_status ON image_processing_jobs(status, created_at);
```

**Startup reset query** (added to `database/database.go` or `database/images.go` — see §1f):

```sql
UPDATE image_processing_jobs SET status = 'pending', started_at = NULL
WHERE status = 'running';
```

Run this in `DB.Open` (or equivalent startup path) before returning the DB handle to the application.

---

### 1d. Variant definitions

**File to create:** `media/images.go`

Package `media`. Define constants and the `ProcessImage` function.

```go
// Variant names. Each maps to one output file inside the base_key directory.
// Crop variants are center-cropped squares. Fit variants preserve aspect ratio
// inside a square, padded with white (JPEG) or transparent (PNG).
const (
    VariantOriginal   = "original"
    VariantThumbCrop  = "thumb_crop"   // 64×64, crop
    VariantThumbFit   = "thumb_fit"    // 64×64, fit
    VariantSmallCrop  = "small_crop"   // 150×150, crop
    VariantSmallFit   = "small_fit"    // 150×150, fit
    VariantMediumCrop = "medium_crop"  // 300×300, crop
    VariantMediumFit  = "medium_fit"   // 300×300, fit
    VariantLargeCrop  = "large_crop"   // 600×600, crop
    VariantLargeFit   = "large_fit"    // 600×600, fit
)

// variantSizes maps each non-original variant name to its target pixel dimension.
var variantSizes = map[string]int{
    VariantThumbCrop:  64,
    VariantThumbFit:   64,
    VariantSmallCrop:  150,
    VariantSmallFit:   150,
    VariantMediumCrop: 300,
    VariantMediumFit:  300,
    VariantLargeCrop:  600,
    VariantLargeFit:   600,
}

// AllVariants lists every variant name in a stable order.
var AllVariants = []string{
    VariantOriginal,
    VariantThumbCrop, VariantThumbFit,
    VariantSmallCrop, VariantSmallFit,
    VariantMediumCrop, VariantMediumFit,
    VariantLargeCrop, VariantLargeFit,
}

const maxImageDimension = 8000 // reject images wider or taller than this
```

`ImageSet` type:

```go
// ImageSet holds the encoded bytes for each variant, keyed by variant name.
// Call ProcessImage to obtain one.
type ImageSet map[string][]byte
```

`ProcessImage` signature and behaviour:

```go
// ProcessImage decodes data (JPEG or PNG only) and generates all image
// variants. The output format matches the input: PNG in → PNG variants,
// JPEG in → JPEG variants.
//
// Returns an error if the image cannot be decoded, if the MIME type is not
// image/jpeg or image/png, if it exceeds maxImageDimension in either dimension,
// or if any variant cannot be encoded.
func ProcessImage(data []byte, sourceMIME string) (ImageSet, string, error)
// Second return value is the canonical extension: ".jpg" or ".png".
```

Implementation notes:
- Return an error immediately if `sourceMIME` is not `"image/jpeg"` or `"image/png"`
- Use `imaging.Decode` (reads from `bytes.Reader`)
- Reject if `img.Bounds().Max.X > maxImageDimension || img.Bounds().Max.Y > maxImageDimension`
- For each non-original variant:
  - If name ends in `_crop`: `imaging.Fill(img, size, size, imaging.Center, imaging.Lanczos)`
  - If name ends in `_fit`: `imaging.Fit(img, size, size, imaging.Lanczos)` then pad to square: create a new `size×size` NRGBA image (white for JPEG output, transparent for PNG), draw the fit result centered on it
- Encode each result:
  - For JPEG source: `imaging.EncodeJPEG` quality 85
  - For PNG source: `imaging.EncodePNG`
- Original variant: store raw `data` bytes unchanged
- Extension rules: JPEG → `.jpg`; PNG → `.png`

---

### 1e. Image storage helpers

**File to create:** `media/imagestore.go` (or add to `media/images.go`)

```go
// BaseKey computes the 16-character hex prefix of the SHA-256 of data.
// Same image bytes always produce the same base_key.
func BaseKey(data []byte) string

// VariantPath returns the on-disk relative path for a variant.
// Format: "<base_key>/<variant><ext>" — e.g. "a3f1c8d2e4b7f901/small_crop.jpg"
func VariantPath(baseKey, variant, ext string) string

// VariantURL returns the /images/ URL for a variant.
// Format: "/images/<base_key>/<variant><ext>"
func VariantURL(baseKey, variant, ext string) string
```

---

### 1f. DB methods for image variants

**File to create:** `database/images.go`

New methods on `*DB`. Also add them to the `Repository` interface in `database/repo.go`.

```go
// EnqueueImageJob inserts a new image_processing_jobs row with status=pending.
// Idempotent: if a pending or running job already exists for the same base_key,
// does nothing (returns nil).
EnqueueImageJob(ctx context.Context, coverType, subjectKey, baseKey string, now int64) error

// ClaimImageJob atomically sets one pending job to running and returns it.
// Returns nil job (no error) if the queue is empty.
ClaimImageJob(ctx context.Context) (*ImageJob, error)

// FinishImageJob records the outcome of a claimed job. It owns the full
// done/retry/failed decision so the worker never has to touch retry_count:
//   - jobErr == nil: status='done', finished_at=now, and variants_ready=1 on
//     the corresponding album_images row.
//   - jobErr != nil: increment retry_count; if the new retry_count >= 3, set
//     status='failed', error=jobErr.Error(), finished_at=now; otherwise set
//     status='pending', error=jobErr.Error(), started_at=NULL so another
//     worker re-claims it.
FinishImageJob(ctx context.Context, id int64, jobErr error) error

// ResetStaleJobs sets all status='running' jobs back to 'pending'.
// Called once at startup before workers are launched.
ResetStaleJobs(ctx context.Context) error

// SetAlbumCover inserts or replaces the album_images row with the given
// base_key, source_ext, object_key (original path), and mime_type.
// variants_ready is set to 0.
SetAlbumCover(ctx context.Context, artist, album, baseKey, sourceExt, objectKey, mimeType string, now int64) error

// GetAlbumCoverStatus returns the base_key, source_ext, variants_ready flag
// and whether any row exists, for the given album.
GetAlbumCoverStatus(ctx context.Context, artist, album string) (baseKey, sourceExt string, variantsReady bool, found bool, err error)

// HasAlbumCover returns true if album_images has any row for the album
// (regardless of variants_ready). Used for fill-if-missing logic.
HasAlbumCover(ctx context.Context, artist, album string) (bool, error)
```

`ImageJob` struct:

```go
type ImageJob struct {
    ID         int64
    CoverType  string
    SubjectKey string  // "<album_artist>\x1f<album_title>" for albums
    BaseKey    string
    RetryCount int
}
```

Keep existing `UpsertAlbumImage` / `GetAlbumImage` / `UpsertArtistImage` / `GetArtistImage` on `*DB` — they are still used by existing image upload handlers. Do **not** remove them yet; they will be superseded in Phase 3 but removing them breaks existing tests.

---

### 1g. Async worker

**File to create:** `imageproc/worker.go`

Package `imageproc`.

```go
// Pool manages a fixed-size goroutine pool that processes image_processing_jobs.
type Pool struct { /* unexported */ }

// NewPool creates (but does not start) a worker pool.
// imagesDir is the absolute path to the images directory.
func NewPool(db database.Repository, imagesDir string, workers int) *Pool

// Start launches the worker goroutines. Blocks until ctx is cancelled.
// Call in a goroutine: go pool.Start(ctx)
func (p *Pool) Start(ctx context.Context)

// Notify signals that a new job has been enqueued. Workers wake immediately
// rather than waiting for their next poll cycle.
// Safe to call from any goroutine.
func (p *Pool) Notify()
```

Worker loop per goroutine:
1. `ClaimImageJob` from DB
2. If no job: wait on a channel (or timer, poll interval = 5s) — `Notify()` unblocks immediately
3. Load original image bytes from disk: `<imagesDir>/<objectKey>` where objectKey = `VariantPath(baseKey, "original", sourceExt)`
4. Call `media.ProcessImage(data, mimeType)`
5. Write each variant file to `<imagesDir>/<VariantPath(baseKey, variant, ext)>` using `os.WriteFile` with `0o644`; create directory `<imagesDir>/<baseKey>/` first with `os.MkdirAll`
6. Call `FinishImageJob(ctx, job.ID, err)` — this method owns the done/retry/failed
   decision (see §1f); the worker does **not** touch `retry_count` itself
7. On error, also log it with the job ID for observability

Retry policy: implemented inside `FinishImageJob` (§1f). A job that fails 3 times is marked `status=failed` and not retried.

**Wire up in `madshare.go`:**
- After `db.Open`, call `db.ResetStaleJobs(ctx)`
- Create `imageproc.NewPool(db, imagesDir, cfg.Storage.ImageProcessingWorkers)`
- `go pool.Start(ctx)` — cancelled by the same context as the HTTP servers
- Pass `pool` (or just `pool.Notify`) to the API handler deps so it can signal after enqueuing a job

**`Deps` struct in `api/api.go`:** add `ImagePool interface { Notify() }` field. Nil-safe: if nil, skip notify (allows tests to omit it).

---

### 1h. Status API endpoint

**Route:** `GET /api/albums/{album}/image/status?artist=<artist>`  
**Handler:** `(h *handler) getAlbumImageStatus`  
**Auth:** none (same access as `GET /api/albums/{album}/image`)

Response shape:

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

When `has_cover = false`: `variants_ready = false`, `base_key = ""`, `variants = {}`.  
When `variants_ready = false`: include `variants` URLs anyway — they are deterministic and the files may exist partially. The UI is responsible for not displaying broken images until ready.

Register the route in `api.RegisterAPI` immediately after the existing `GET /api/albums/{album}/image` line:

```go
r.Get("/api/albums/{album}/image/status", h.getAlbumImageStatus)
```

---

### 1i. Image file serving update

The `/images/*` file server in `api.RegisterAPI` already serves `h.imagesDir` via `http.FileServer`. It will automatically serve the new `<base_key>/` subdirectories without changes. Verify the existing `noListFS` wrapper (which returns 404 for directory listings) also blocks listing of `<base_key>/` directories — it should, since `noListFS` intercepts all directory opens. No code change needed here.

---

### 1j. Tests for Phase 1

- `media/images_test.go`:
  - `TestProcessImage_JPEG`: load a small JPEG fixture, call `ProcessImage`, assert all 9 variant names present, all byte slices non-empty, output can be decoded by `imaging.Decode`
  - `TestProcessImage_PNG`: same for PNG; assert output is PNG (check magic bytes: `\x89PNG`)
  - `TestProcessImage_OversizeRejected`: construct a 8001×100 image, assert error
  - `TestVariantPath`: assert `VariantPath("abc", "small_crop", ".jpg") == "abc/small_crop.jpg"`

- `imageproc/worker_test.go`:
  - `TestPool_ProcessesJob`: insert a real job into a test DB, run `pool.Start` with a short context, assert `variants_ready = 1` after context cancel
  - Use the real `media.ProcessImage` (not mocked) — fixture image from `testdata/`

- `database/images_test.go`:
  - `TestEnqueueImageJob_Idempotent`: calling twice for same base_key inserts only one row
  - `TestResetStaleJobs`: insert a running job, call reset, assert it becomes pending
  - `TestFinishImageJob_SetsVariantsReady`: finish a job for an album with status=done, assert `variants_ready = 1` in `album_images`

- `config/webui_config_test.go`:
  - `TestLoadWebUI_Defaults`: no file → defaults (3, 10)
  - `TestLoadWebUI_MaxClamped`: `max_parallel_workers < default_parallel_workers` → max clamped up

- `config/config_test.go` (extend existing):
  - `TestLoad_ImageWorkersAuto`: `image_processing_workers` unset/`0` → resolved to `runtime.NumCPU()`
  - `TestLoad_ImageWorkersExplicit`: explicit `4` → stays `4`
  - `TestLoad_ImageWorkersNegative`: `-1` → resolved to `runtime.NumCPU()` (and warns, non-fatal)

---

### 1k. Deferred — WebP support (documented path, do not implement now)

WebP cover art is **rejected at the upload boundary in v0**: `ProcessImage`
returns an error for any MIME other than `image/jpeg`/`image/png`, and
`image/webp` / `.webp` are removed from the manual-upload allowlists (§3a). This
keeps every cover to a single extension (`source_ext` describes both the
original and all variants).

This section records exactly what a later, **non-breaking** WebP addition
requires, so it stays a known quantity:

- **Decode:** add `golang.org/x/image/webp` (decode-only; no cgo) and blank-import
  it so `imaging.Decode` transparently handles WebP. No encoder is needed — WebP
  variants are re-encoded as PNG.
- **Dual extension:** a WebP cover keeps its original as `.webp` but its variants
  become `.png`, so a single `source_ext` no longer describes both. Introduce a
  `variant_ext` concept — either a new `album_images.variant_ext` column, or
  derive it in code (`variantExt := source_ext; if source_ext == ".webp" { variantExt = ".png" }`).
- **Threading:** that split must reach (1) `ProcessImage`'s returned extension,
  (2) the worker's per-variant `VariantPath` calls, and (3) the status API, which
  builds the `original` URL from `source_ext` but every other variant URL from
  `variant_ext`.
- **Boundary:** re-add `image/webp` / `.webp` to `allowedImageMIMETypes` /
  `allowedImageExtensions`, and add `image/webp` to `mimeToExt` (mapping to
  `.webp` for the original).
- **No backfill:** since v0 stores zero WebP rows, enabling this later needs no
  data migration — it is purely additive.

---

## Phase 2 — Embedded Cover Extraction During Audio Upload

### Goal
When an audio file is uploaded, extract any embedded cover art from its tags and automatically save it as the album cover if none exists yet.

---

### 2a. Extend `media.Tags` with cover image

**File to modify:** `media/extract.go`

Add to the `Tags` struct:

```go
// CoverImage holds the embedded cover art extracted from the audio tags.
// Nil when the file has no embedded art or extraction fails.
CoverImage *CoverData
```

Add `CoverData` type (same file):

```go
type CoverData struct {
    MIMEType string // e.g. "image/jpeg"
    Data     []byte
}
```

In `ExtractTags`, after building the `Tags` struct, add:

```go
if pic := m.Picture(); pic != nil && len(pic.Data) > 0 {
    t.CoverImage = &CoverData{
        MIMEType: pic.MIMEType,
        Data:     pic.Data,
    }
}
```

`m.Picture()` is available on the `tag.Metadata` interface from `dhowden/tag`.

---

### 2b. Upload handler: save extracted cover

**File organization:** as part of this work, move `uploadFile` (and the new
`maybeSaveEmbeddedCover` / `mimeToExt` helpers, plus the Phase 4 limiter check)
out of `api/handlers.go` into a new **`api/upload_handlers.go`**, following the
existing `*_handlers.go` convention (see locked decisions — no subpackage). The
shared helpers it calls (`audit`, `sanitizeFilename`, `extractTagsOrEmpty`,
`tagsToMetadata`, `writeJSON`, MIME allowlists) stay where they are — same
package, no exports needed.

In `uploadFile`, after the `h.repo.InsertFile` call succeeds, add cover
extraction logic:

```go
coverProcessing := false
if tags.CoverImage != nil && tags.Album != "" {
    coverProcessing = h.maybeSaveEmbeddedCover(ctx, tags)
}
```

`maybeSaveEmbeddedCover` returns `true` only when it actually queued a new variant
job (cover was missing and was saved + enqueued). The caller uses that return
value to populate `cover_processing` in the response (§2c). Extract it as a
private method on `*handler`:

```go
// maybeSaveEmbeddedCover saves the embedded art as the album cover when none
// exists yet. Returns true if a new variant job was queued, false otherwise.
func (h *handler) maybeSaveEmbeddedCover(ctx context.Context, tags *media.Tags) bool {
    artist := tags.AlbumArtist
    if artist == "" {
        artist = tags.Artist
    }
    if tags.Album == "" || artist == "" {
        return false
    }
    has, err := h.repo.HasAlbumCover(ctx, artist, tags.Album)
    if err != nil || has {
        return false
    }
    ext, ok := mimeToExt(tags.CoverImage.MIMEType)
    if !ok {
        return false
    }
    baseKey := media.BaseKey(tags.CoverImage.Data)
    objectKey := media.VariantPath(baseKey, media.VariantOriginal, ext)
    destPath := filepath.Join(h.imagesDir, objectKey)
    if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
        log.Printf("embedded cover: mkdir %s: %v", filepath.Dir(destPath), err)
        return false
    }
    if err := os.WriteFile(destPath, tags.CoverImage.Data, 0o644); err != nil {
        log.Printf("embedded cover: write %s: %v", destPath, err)
        return false
    }
    now := time.Now().Unix()
    if err := h.repo.SetAlbumCover(ctx, artist, tags.Album, baseKey, ext, objectKey, tags.CoverImage.MIMEType, now); err != nil {
        log.Printf("embedded cover: db upsert: %v", err)
        return false
    }
    subjectKey := artist + "\x1f" + tags.Album
    if err := h.repo.EnqueueImageJob(ctx, "album", subjectKey, baseKey, now); err != nil {
        log.Printf("embedded cover: enqueue job: %v", err)
        return false
    }
    if h.imagePool != nil {
        h.imagePool.Notify()
    }
    return true
}
```

Add `mimeToExt` helper (same file or `library_handlers.go`):

```go
func mimeToExt(mime string) (string, bool) {
    switch mime {
    case "image/jpeg":
        return ".jpg", true
    case "image/png":
        return ".png", true
    }
    return "", false
}
```

Add `imagePool interface{ Notify() }` field to `handler` struct. Wire it from `Deps.ImagePool` in `api.go`.

---

### 2c. Upload response: include cover status

Extend upload response JSON:

```json
{
    "ok": true,
    "existed": false,
    "hash": "...",
    "filename": "...",
    "size": 12345,
    "cover_found": true,
    "cover_processing": true
}
```

`cover_found`: `tags.CoverImage != nil && artist != "" && album != ""`  
`cover_processing`: the `bool` returned by `maybeSaveEmbeddedCover` (true only when a new job was just queued — i.e. cover was missing, saved, and enqueued)  
For the dedup path: set both to `false` — embedded art is not re-processed for duplicate files.

---

### 2d. Tests for Phase 2

- `api/handlers_test.go`:
  - `TestUploadFile_ExtractsCover`: upload a test MP3 with embedded cover (fixture in `testdata/`); assert `album_images` row exists and `image_processing_jobs` has one pending row; assert `cover_found: true` in response
  - `TestUploadFile_NoCoverOnDedup`: upload same file twice; assert only one job enqueued
  - `TestUploadFile_FillIfMissing_SkipsExisting`: pre-insert an `album_images` row, upload file with embedded cover for same album; assert no new job and no overwrite
  - `TestUploadFile_NoCoverWhenAlbumEmpty`: upload audio file with embedded cover but no album tag; assert no `album_images` row

---

## Phase 3 — Manual Cover Upload Extension

### Goal
The existing `POST /api/albums/{album}/image` handler saves a single original file. Extend it to also save with `base_key`/`source_ext`, and enqueue an async variant job.

---

### 3a. Rewrite `saveImageUpload` to use new schema

**File to modify:** `api/library_handlers.go`

Replace the body of `uploadAlbumImage` to use `SetAlbumCover` + `EnqueueImageJob` instead of `UpsertAlbumImage`:

```go
func (h *handler) uploadAlbumImage(w http.ResponseWriter, r *http.Request) {
    album := chi.URLParam(r, "album")
    artist := r.URL.Query().Get("artist")
    r.Body = http.MaxBytesReader(w, r.Body, maxImageSize)

    data, mimeType, ext, err := h.readImageUpload(r)
    if err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    baseKey := media.BaseKey(data)
    objectKey := media.VariantPath(baseKey, media.VariantOriginal, ext)
    destPath := filepath.Join(h.imagesDir, objectKey)
    if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
        http.Error(w, "cannot create images dir", http.StatusInternalServerError)
        return
    }
    if err := os.WriteFile(destPath, data, 0o644); err != nil {
        http.Error(w, "cannot save image", http.StatusInternalServerError)
        return
    }

    now := time.Now().Unix()
    ctx := r.Context()
    if err := h.repo.SetAlbumCover(ctx, artist, album, baseKey, ext, objectKey, mimeType, now); err != nil {
        http.Error(w, "storage error", http.StatusInternalServerError)
        return
    }
    subjectKey := artist + "\x1f" + album
    if err := h.repo.EnqueueImageJob(ctx, "album", subjectKey, baseKey, now); err != nil {
        log.Printf("enqueue image job: %v", err)
        // Non-fatal: original is saved, variants will be missing until manually triggered.
    }
    if h.imagePool != nil {
        h.imagePool.Notify()
    }

    h.audit(ctx, "metadata.image", "album:"+artist+"/"+album, "manual upload")
    writeJSON(w, http.StatusOK, map[string]any{"ok": true, "processing": true})
}
```

Extract `readImageUpload(r *http.Request) (data []byte, mimeType, ext string, err error)` from the existing `saveImageUpload` logic. This replaces `saveImageUpload` entirely — delete the old function.

**Two corrections to carry over from the old `saveImageUpload`:**

1. **Drop WebP from the allowlists.** The package-level `allowedImageMIMETypes`
   and `allowedImageExtensions` (in `api/library_handlers.go`) currently include
   `image/webp` / `.webp`. Since `ProcessImage` rejects WebP, remove those two
   entries so the manual-upload endpoint rejects WebP up front rather than
   accepting a file that the worker will fail to process (and leave stuck at
   `status=failed`).
2. **Return the *canonical* extension, not the raw uploaded one.** The old code
   used the raw file extension (so `.jpeg` stayed `.jpeg`). `readImageUpload`
   must return the canonical `.jpg`/`.png` derived from the canonical MIME type
   (e.g. via `mimeToExt(canonicalMIME)`), because the status API, worker, and
   `VariantPath` all assume `original.jpg` / `original.png`. A `.jpeg` upload
   must yield `ext == ".jpg"`.

Response changes from `{"ok": true}` to `{"ok": true, "processing": true}`.

---

### 3b. Existing `getAlbumImage` handler

Keep it working: it serves the original via `serveImageFile`. After Phase 3, the object_key stored in `album_images` is now `<base_key>/original<ext>` rather than the old flat `<hash16><ext>` format. Existing old-format rows in the DB will still work via `serveImageFile` as long as they are not reprocessed (the file is still at the old path). This is acceptable — old rows stay on old paths forever.

New rows always use the subdirectory format. No migration of old image files is needed.

---

### 3c. Tests for Phase 3

- `api/handlers_test.go` or `api/library_handlers_test.go`:
  - `TestUploadAlbumImage_EnqueuesJob`: POST a JPEG image; assert `image_processing_jobs` has one pending row and response contains `"processing": true`
  - `TestUploadAlbumImage_OverwritesExisting`: POST twice; assert only one pending job (idempotent enqueue); assert `album_images` has updated `base_key`

---

## Phase 4 — Upload Concurrency & Rate Limiting

### Goal
Enforce server-side upload slot limits: a global cap across all users (`server_max_parallel_workers`) and a per-user cap (`user_max_parallel_workers`). Both caps apply to everyone (no admin bypass — see locked decisions). Return 429 when any limit is exceeded; the client handles backoff by reducing its worker count and re-queuing.

---

### 4a. `UploadLimiter`

**File to create:** `api/upload_limiter.go`

```go
// UploadLimiter is a concurrency gate for upload requests.
// It tracks in-flight uploads globally and per user.
// Zero values for serverMax/userMax mean unlimited.
// Thread-safe.
type UploadLimiter struct {
    mu           sync.Mutex
    serverMax    int
    userMax      int
    globalCount  int
    perUser      map[string]int
}

func NewUploadLimiter(serverMax, userMax int) *UploadLimiter

var (
    ErrServerLimit = errors.New("server upload limit reached")
    ErrUserLimit   = errors.New("user upload limit reached")
)

// Acquire attempts to claim an upload slot for the given user.
// Returns ErrServerLimit or ErrUserLimit without blocking.
func (l *UploadLimiter) Acquire(userID string) error

// Release returns an upload slot. Call exactly once after a successful Acquire.
func (l *UploadLimiter) Release(userID string)
```

`Acquire` implementation:
1. Lock
2. If `serverMax > 0 && globalCount >= serverMax` → return `ErrServerLimit`
3. If `userMax > 0 && perUser[userID] >= userMax` → return `ErrUserLimit`
4. Increment `globalCount` and `perUser[userID]`; unlock; return nil

`Release`: decrement both counters under lock; guard against going below 0.

---

### 4b. Wire into the upload handler

**File to modify:** `api/api.go` — add `UploadLimiter *UploadLimiter` to `Deps`.  
**File to modify:** `api/upload_handlers.go` — add `limiter *UploadLimiter` to the `handler` struct (the struct is declared in `api/handlers.go`; the limiter check lives with `uploadFile` in `upload_handlers.go`); wire from `Deps`.

In `uploadFile`, before the multipart parse:

```go
// auth.FromContext returns *auth.Identity (nil when anonymous). Identity.UserID
// is an int64 field, so format it as the string key the limiter uses. Anonymous
// requests collapse to the "" key — only reachable when auth is unconfigured,
// since /files/upload is otherwise gated by protect(auth.PermFileUpload).
userID := ""
if id := auth.FromContext(r.Context()); id != nil {
    userID = strconv.FormatInt(id.UserID, 10)
}
if h.limiter != nil {
    if err := h.limiter.Acquire(userID); err != nil {
        writeUploadLimitError(w, err)
        return
    }
    defer h.limiter.Release(userID)
}
```

`writeUploadLimitError`:

```go
func writeUploadLimitError(w http.ResponseWriter, err error) {
    msg := "server upload limit reached"
    if errors.Is(err, ErrUserLimit) {
        msg = "user upload limit reached"
    }
    w.Header().Set("Content-Type", "application/json")
    w.Header().Set("Retry-After", "1")
    w.WriteHeader(http.StatusTooManyRequests)
    _ = json.NewEncoder(w).Encode(map[string]any{
        "error": msg,
        "code":  "upload_limit",
    })
}
```

**Wire in `madshare.go`:**

```go
limiter := api.NewUploadLimiter(
    cfg.Storage.ServerMaxParallelWorkers,
    cfg.Storage.UserMaxParallelWorkers,
)
// pass to api.Deps.UploadLimiter
```

---

### 4c. Identity wiring notes

- Use `auth.FromContext(ctx)` (not `IdentityFromContext`) to get `*auth.Identity`.
- `Identity.UserID` is an `int64` **field** (not a method); format with
  `strconv.FormatInt` for the limiter's string key.
- There is **no admin bypass** (see locked decisions). If a role/admin signal is
  added to `Identity` later, reintroduce the bypass here and in `Acquire`.

---

### 4d. Tests for Phase 4

- `api/upload_limiter_test.go`:
  - `TestUploadLimiter_ServerLimit`: serverMax=1, acquire twice, assert second returns `ErrServerLimit`
  - `TestUploadLimiter_UserLimit`: userMax=1, acquire twice with same userID, assert second returns `ErrUserLimit`
  - `TestUploadLimiter_DifferentUsers`: userMax=1, acquire with different userIDs, assert no error
  - `TestUploadLimiter_Release`: acquire then release, assert next acquire succeeds
  - `TestUploadLimiter_Unlimited`: serverMax=0, userMax=0, many acquires, no errors

- `api/handlers_test.go`:
  - `TestUploadFile_ServerLimitReturns429`: pre-saturate limiter at serverMax=1, assert 429 with `code: "upload_limit"`
  - `TestUploadFile_UserLimitReturns429`: same for userMax=1, same-user second request

---

## Phase 5 — Upload Page (`/upload`, webui)

### Goal
A new web UI page at `/upload` where users can upload files and folders of any type, see per-album cover previews, replace covers manually, and track async processing status. Client-side parallel workers are user-adjustable.

---

### 5a. Route registration

**File to modify:** `webui/webui.go`

Register the new page in `Register`:

```go
r.Get("/upload", renderTemplate("upload.html"))
```

Prefer template with no server-side data injection — all state comes from API calls.

---

### 5b. Template: `webui/html/upload.html`

Full vanilla HTML/CSS/JS. No external dependencies.

**Layout (3-zone, responsive):**

```
┌──────────────────────────────────────────────┐
│  nav: [← Library]           Madshare / Upload│
├──────────────────────────────────────────────┤
│  DROP ZONE                                   │
│  ┌──────────────────────────────────────┐   │
│  │  Drag files or a folder here         │   │
│  │  [Browse files]  [Browse folder]     │   │
│  └──────────────────────────────────────┘   │
├──────────────────────────────────────────────┤
│  Workers: [———●———————] 3          [Upload]  │
├──────────────────────────────────────────────┤
│  Upload queue                                │
│  ┌──────────────────────────────────────┐   │
│  │ filename.mp3    3.2 MB   [========  ]│   │
│  │ track02.mp3     4.1 MB   [====      ]│   │
│  │ bad.exe         rejected             │   │
│  └──────────────────────────────────────┘   │
│  [Clear]                                     │
├──────────────────────────────────────────────┤
│  Album groups detected                       │
│  ┌────────┐  ┌────────┐                     │
│  │ [art]  │  │ [art]  │                     │
│  │ Album1 │  │ Album2 │                     │
│  │ 3 trks │  │ 5 trks │                     │
│  │[Cover] │  │[Cover] │                     │
│  └────────┘  └────────┘                     │
└──────────────────────────────────────────────┘
```

**Parallel workers control:**
- On page load, call `GET /api/ui/config` to obtain `default_parallel_workers` and `max_parallel_workers`
- Render `<input type="range" min="1" max="{max}">` defaulting to `default_parallel_workers`
- Label shows the current value live: "Workers: N"
- The JS upload queue uses this value as the concurrency cap for outstanding `POST /files/upload` requests

**Upload queue & worker logic:**
- Files added to a JS array (the queue), rendered as a list
- N concurrent "worker slots" — each picks the next pending file from the queue and calls `POST /files/upload`
- Use `file.webkitRelativePath || file.name` as the filename in the request's `Content-Disposition`
- On success or non-429 error: slot picks the next queued file
- On 429 response:
  - Decrement current worker count by 1 (floor at 1); update slider display
  - Re-queue the failed file at the front of the queue
  - Log: `"Server limit hit — workers reduced to N"`
- Progress per file via `XMLHttpRequest` `upload.onprogress`

**Client-side album grouping:**
- Extract directory prefix from `file.webkitRelativePath`: `path.split('/').slice(0, -1).join('/')`; empty string for files with no directory
- Files sharing the same prefix are one album group
- Cover candidates within a group: files whose base name (no extension, lowercased) is one of `cover`, `folder`, `front`, `albumart`, `artwork`, `album` AND whose MIME type is `image/jpeg` or `image/png`
- When all audio files in a group have finished (succeeded or failed):
  - If any upload succeeded and returned album+artist tags: use the first non-empty pair
  - If a cover candidate exists for the group AND album+artist is known AND the user has `metadata.edit` permission (from the `GET /api/auth/me` result loaded on page init): POST it to `POST /api/albums/{album}/image?artist={artist}`; if the user lacks that permission, skip silently — the card shows a grey placeholder with no error
  - If album+artist is not known (all audio errored or had no tags): skip cover upload; show "no album info" note in card
- If multiple cover candidates exist in a group: use the largest by byte size

**Cover preview card (per album group):**
- Rendered when files are added to the queue (before upload starts), keyed by directory prefix
- Shows `medium_crop` variant URL if `variants_ready: true`, else a grey placeholder with a spinner overlay
- Album name + artist (derived from first successful upload response in the group; falls back to directory name)
- Track count
- "Replace cover" button — opens `<input type="file" accept="image/jpeg,image/png">` → POSTs to `POST /api/albums/{album}/image?artist={artist}`; refreshes card after response

**Polling:**
- When a card has `variants_ready: false`, poll `GET /api/albums/{album}/image/status?artist={artist}` every 2 seconds
- Stop polling when `variants_ready: true`; replace placeholder with real image
- Stop polling if the page becomes hidden (`document.visibilitychange` → `document.hidden`)
- Stop polling if the status endpoint returns 404 (album was deleted)

**Drop zone JS:**
- `dragover` / `drop` events — prevent default, read `e.dataTransfer.files`
- `<input type="file" multiple>` for "Browse files"; accepts all file types
- `<input type="file" webkitdirectory multiple>` for "Browse folder"; set `directory` + `mozdirectory` attributes for cross-browser
- All file types are accepted into the queue; the server rejects unsupported ones and the result shows inline

**Access control on "Replace cover" button:**
- On page load, call `GET /api/auth/me`
- If user has permission `metadata.edit`: show "Replace cover" buttons
- If not (or if the call fails / user is anonymous): hide "Replace cover" buttons

**Error handling:**
- If an upload request fails (network, 500): show inline error in the queue row with a retry button for that file
- Per-file 429: handled automatically by the worker backoff (no manual retry needed)

---

### 5c. Static assets

No new static files needed. The upload page uses the same CSS variables and design tokens as `library.html` and `cmus.html` for visual consistency.

---

### 5d. Manual testing checklist (for the verifying agent)

- [ ] Drop a single MP3 → appears in queue → Upload → appears in library
- [ ] Drop a folder with 3 MP3s and a `cover.jpg` → 1 album group card appears → cover thumbnail shows after polling resolves
- [ ] Drop a file with an unsupported extension → uploaded, server returns error inline in queue row
- [ ] "Replace cover" uploads a new image → spinner appears → resolves to new image
- [ ] Drop two separate album folders → two distinct cover cards, each with their own polling
- [ ] `image_processing_workers = 1` in config → variants still appear eventually
- [ ] Set workers slider to 1 → uploads proceed strictly one at a time
- [ ] Simulate 429 (`server_max_parallel_workers = 1`, two tabs uploading simultaneously) → slider auto-decrements in the tab that receives 429
- [ ] Upload page while upload in progress → visually clear per-file progress bars

---

## Phase 6 — Aura Effect (Frontend)

### Goal
A visual ambient glow around the player in `cmus.html` derived from the currently playing track's album cover. Purely client-side.

---

### 6a. Implementation approach

When the player loads a new track:
1. Read the track's album and album_artist from the file list JSON
2. Fetch `GET /api/albums/{album}/image/status?artist={artist}` to get the `small_crop` variant URL
3. If `variants_ready: false` or no cover: skip (no aura, no polling in the player — aura is decorative)
4. Draw the `small_crop` image onto an offscreen `<canvas>` (`crossOrigin = "anonymous"` — works same-origin)
5. Read pixel data via `ctx.getImageData`
6. Sample a 10×10 grid across the image; collect all RGB values
7. Find dominant color: bucket pixels into 8 hue regions (0°, 45°, 90°, 135°, 180°, 225°, 270°, 315°), pick the largest bucket; ignore pixels with saturation < 20% or luminance > 85% or < 10%
8. Set `--aura-h`, `--aura-s`, `--aura-l` CSS custom properties on the player root element
9. CSS applies it: a `radial-gradient` or `box-shadow` behind the album art area using `hsl(var(--aura-h), var(--aura-s)%, var(--aura-l)%)`

**CSS transition:** `transition: --aura-h 1s ease, --aura-s 1s ease, --aura-l 1s ease` — fades between tracks.

**Fallback:** if canvas read fails (e.g. tainted canvas from a future external-image scenario), silently set aura to a neutral dark color. Never throw an uncaught error.

---

### 6b. Files to modify

- `webui/html/cmus.html` — add canvas element (hidden), aura CSS variables, and the JS logic
- No server-side changes

---

## Appendix — Files Created / Modified Summary

| File | Action | Phase |
|---|---|---|
| `config/config.go` | Extend `StorageConfig` with 3 new fields; add `WebUIConfig` if in same file | 1b |
| `config/webui_config.go` | New: `WebUIConfig`, `LoadWebUI` | 1b |
| `madshare.toml.example` | Add new `[storage]` fields | 1b |
| `webui.toml.example` | New file | 1b |
| `madshare.go` | Add `-webui-config` flag; wire limiter, imageproc pool, `ResetStaleJobs`, `WebUIConfig` | 1b, 1g, 4b |
| `database/migrations/009_image_variants.sql` | New migration | 1c |
| `media/images.go` | `ProcessImage`, variant constants | 1d |
| `media/imagestore.go` | `BaseKey`, `VariantPath`, `VariantURL` | 1e |
| `database/images.go` | New DB methods for job queue and cover variants | 1f |
| `database/repo.go` | Add new methods to `Repository` interface | 1f |
| `imageproc/worker.go` | `Pool` type, worker goroutines | 1g |
| `api/api.go` | Add `ImagePool`, `UploadLimiter`, `WebUIConfig` to `Deps`; register status + ui/config routes | 1h, 4b |
| `api/library_handlers.go` | Add `getAlbumImageStatus`, `getUIConfig`; rewrite `uploadAlbumImage` | 1h, 1b, 3a |
| `media/extract.go` | Add `CoverData`, `Tags.CoverImage` | 2a |
| `api/handlers.go` | Add `imagePool`/`limiter` fields to the `handler` struct | 2b, 4b |
| `api/upload_handlers.go` | New: move `uploadFile` here; add `maybeSaveEmbeddedCover`, `mimeToExt`, extend upload response, limiter check | 2b–2c, 4b |
| `api/upload_limiter.go` | New: `UploadLimiter` | 4a |
| `webui/webui.go` | Register `/upload` route | 5a |
| `webui/html/upload.html` | New upload page | 5b |
| `webui/html/cmus.html` | Add aura effect JS and CSS | 6b |

---

## Appendix — API Contract Summary

| Method | Path | Auth | Description |
|---|---|---|---|
| `GET` | `/api/ui/config` | none | UI configuration (worker defaults/max) |
| `POST` | `/files/upload` | `file.upload` | Single file upload (all types); subject to upload limiter |
| `GET` | `/api/albums/{album}/image` | none | Serve original cover image |
| `POST` | `/api/albums/{album}/image` | `metadata.edit` | Upload/replace cover; triggers async variant job |
| `GET` | `/api/albums/{album}/image/status` | none | Variant readiness + all variant URLs |

Query param for album endpoints: `?artist=<album_artist>` (empty string allowed — matches rows with empty album_artist).
