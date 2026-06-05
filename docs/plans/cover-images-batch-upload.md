# Cover Images & Batch Upload — Implementation Plan

**Status:** Ready to implement. All decisions are locked.  
**Branch target:** `aidev` → merge to `develop`  
**Module path:** `daemonlord.ygg/madshare`

---

## Locked Decisions (do not re-ask)

| Topic | Decision |
|---|---|
| Resize modes | Generate **both** `_crop` (square center-crop) and `_fit` (fit inside square, padded) at every size |
| Output format | Preserve source format — PNG source → PNG variants, JPEG source → JPEG variants |
| Fit padding | White for JPEG, transparent for PNG |
| Cover priority | Explicit uploaded file **beats** embedded tag art. Fill-if-missing: never overwrite an existing cover |
| Async processing | Yes — goroutine worker pool, DB-backed job queue |
| Worker clamp | Any `max_parallel_workers < 1` or invalid → silently treated as 1; max accepted value = 32. Warn on clamp, never fatal |
| Artist covers | **Deferred** — schema reserves space but no implementation in this plan |
| Upload page | `/upload` route in webui; one cover preview card per detected album group |
| Aura effect | Client-side canvas only (Phase 6, last) |
| Manual cover upload | API already exists (`POST /api/albums/{album}/image`); extend to trigger async job; also exposed in the upload page UI |

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
Phase 4 — Batch & folder upload (API)
    ↓
Phase 5 — Upload page (/upload, webui)
    ↓
Phase 6 — Aura effect (frontend, client-side)
```

---

## Phase 1 — Image Variant System

### Goal
Establish the full image processing pipeline: resize library, config section, DB schema, async worker pool, and a status query API. Everything else builds on top of this.

---

### 1a. Add `disintegration/imaging` dependency

```bash
go get github.com/disintegration/imaging
```

Verify it appears in `go.mod` and `go.sum`.

---

### 1b. Config: add `[images]` section

**File to modify:** `config/config.go`

Add a new struct after `StorageConfig`:

```go
type ImagesConfig struct {
    // MaxParallelWorkers controls the goroutine pool for image variant generation.
    // Any value < 1 is clamped to 1. Max accepted value is 32.
    MaxParallelWorkers int `toml:"max_parallel_workers"`
}
```

Add field to `Config`:

```go
type Config struct {
    Listen   []ListenConfig `toml:"listen"`
    WebUI    WebUIConfig    `toml:"webui"`
    Database DatabaseConfig `toml:"database"`
    Storage  StorageConfig  `toml:"storage"`
    Auth     AuthConfig     `toml:"auth"`
    Images   ImagesConfig   `toml:"images"`  // ← add this
}
```

Add default in `defaults()`:

```go
Images: ImagesConfig{
    MaxParallelWorkers: 2,
},
```

Add validation in `config.Load` (after existing storage validation, before `validateListeners`):

```go
if c.Images.MaxParallelWorkers < 1 {
    log.Printf("config: images.max_parallel_workers %d is invalid, using 1", c.Images.MaxParallelWorkers)
    c.Images.MaxParallelWorkers = 1
}
if c.Images.MaxParallelWorkers > 32 {
    log.Printf("config: images.max_parallel_workers %d exceeds maximum, using 32", c.Images.MaxParallelWorkers)
    c.Images.MaxParallelWorkers = 32
}
```

Use `log.Printf` (warning), not `return error` — worker count is non-fatal.

Update `madshare.toml.example` with:

```toml
[images]
max_parallel_workers = 2   # concurrent image resize workers; invalid/low values clamp to 1
```

Update `CLAUDE.md` config section to document `[images]`.

---

### 1c. DB migration 009

**File to create:** `database/migrations/009_image_variants.sql`

```sql
-- Extend album_images with variant tracking columns.
-- object_key retains its original meaning (original image path) for backward compat.
-- base_key is the 16-char SHA-256 prefix used to derive all variant paths.
-- source_ext is ".jpg", ".jpeg", ".png", or ".webp" — the extension of the original file.
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

**Startup reset query** (added to `database/database.go` or a new `database/images.go` — see §1f):

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
// ProcessImage decodes data (JPEG, PNG, or WebP) and generates all image
// variants. The output format matches the input: PNG in → PNG variants,
// JPEG in → JPEG variants. WebP is decoded and re-encoded as PNG.
//
// Returns an error if the image cannot be decoded, exceeds maxImageDimension
// in either dimension, or any variant cannot be encoded.
func ProcessImage(data []byte, sourceMIME string) (ImageSet, string, error)
// Second return value is the canonical extension: ".jpg", ".png", ".webp" (webp stays webp for original, png for variants).
```

Implementation notes:
- Use `imaging.Decode` (reads from `bytes.Reader`)
- Reject if `img.Bounds().Max.X > maxImageDimension || img.Bounds().Max.Y > maxImageDimension`
- For each non-original variant:
  - If name ends in `_crop`: `imaging.Fill(img, size, size, imaging.Center, imaging.Lanczos)`
  - If name ends in `_fit`: `imaging.Fit(img, size, size, imaging.Lanczos)` then pad to square: create a new `size×size` NRGBA image (white for JPEG output, transparent for PNG), draw the fit result centered on it
- Encode each result:
  - For JPEG source: `imaging.EncodeJPEG` quality 85
  - For PNG source: `imaging.EncodePNG`
  - WebP source: decode fine; encode variants as PNG (treat as PNG for output)
- Original variant: store raw `data` bytes unchanged
- Extension rules: JPEG → `.jpg`; PNG → `.png`; WebP original stays `.webp`, variants are `.png`

---

### 1e. Image storage helpers

**File to create:** `media/imagestore.go` (or add to `media/images.go`)

```go
// BaseKey computes the 16-character hex prefix of the SHA-256 of data.
// This is stable: same image bytes always produce the same base_key.
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

// FinishImageJob marks a job done or failed and sets finished_at.
// On done: also sets variants_ready=1 on the corresponding album_images row.
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
// imagesDir is the absolute path to the images directory (same as handler.imagesDir).
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
6. Call `FinishImageJob(ctx, job.ID, err)`
7. On error: increment `retry_count`, set `status=failed` if `retry_count >= 3`, else set back to `pending`

Retry policy: a job that fails 3 times is marked `status=failed` and not retried. Log the error with the job ID.

**Wire up in `madshare.go`:**
- After `db.Open`, call `db.ResetStaleJobs(ctx)`
- Create `imageproc.NewPool(db, imagesDir, cfg.Images.MaxParallelWorkers)`
- `go pool.Start(ctx)` — cancelled by the same context as the HTTP servers
- Pass `pool` (or just `pool.Notify()`) to the API handler deps so it can signal after enqueuing a job

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

**File to modify:** `api/handlers.go` — `uploadFile` function.

After the `h.repo.InsertFile` call succeeds, add cover extraction logic:

```go
// Save embedded cover as album cover if we have album metadata and no cover exists.
if tags.CoverImage != nil &&
    tags.Album != "" &&
    !isAllowedImageMIME(tags.CoverImage.MIMEType) == false {
    h.maybeSaveEmbeddedCover(ctx, tags)
}
```

Extract `maybeSaveEmbeddedCover` as a private method on `*handler`:

```go
func (h *handler) maybeSaveEmbeddedCover(ctx context.Context, tags *media.Tags) {
    artist := tags.AlbumArtist
    if artist == "" {
        artist = tags.Artist
    }
    // Skip if we can't identify the album or artist.
    if tags.Album == "" || artist == "" {
        return
    }
    // Fill-if-missing: skip if a cover already exists.
    has, err := h.repo.HasAlbumCover(ctx, artist, tags.Album)
    if err != nil || has {
        return
    }
    ext, ok := mimeToExt(tags.CoverImage.MIMEType)
    if !ok {
        return
    }
    baseKey := media.BaseKey(tags.CoverImage.Data)
    objectKey := media.VariantPath(baseKey, media.VariantOriginal, ext)
    destPath := filepath.Join(h.imagesDir, objectKey)
    if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
        log.Printf("embedded cover: mkdir %s: %v", filepath.Dir(destPath), err)
        return
    }
    if err := os.WriteFile(destPath, tags.CoverImage.Data, 0o644); err != nil {
        log.Printf("embedded cover: write %s: %v", destPath, err)
        return
    }
    now := time.Now().Unix()
    if err := h.repo.SetAlbumCover(ctx, artist, tags.Album, baseKey, ext, objectKey, tags.CoverImage.MIMEType, now); err != nil {
        log.Printf("embedded cover: db upsert: %v", err)
        return
    }
    subjectKey := artist + "\x1f" + tags.Album
    if err := h.repo.EnqueueImageJob(ctx, "album", subjectKey, baseKey, now); err != nil {
        log.Printf("embedded cover: enqueue job: %v", err)
    }
    if h.imagePool != nil {
        h.imagePool.Notify()
    }
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
    case "image/webp":
        return ".webp", true
    }
    return "", false
}
```

Add `imagePool interface{ Notify() }` field to `handler` struct. Wire it from `Deps.ImagePool` in `api.go`.

---

### 2c. Upload response: include cover status

Extend both upload response JSON objects (new file path and dedup path):

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
`cover_processing`: `cover_found && !has` (i.e., we just queued a new job)  
For the dedup path: re-check `HasAlbumCover` only if you intend to fill-if-missing on re-upload. Simpler: set both to `false` on dedup — embedded art is not re-processed for duplicate files.

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

## Phase 4 — Batch & Folder Upload (API)

### Goal
Extend `POST /files/upload` to accept multiple audio files (and optional cover images) in a single request. Support folder upload via `webkitdirectory` browser attribute, which sends relative paths embedded in filenames.

---

### 4a. New endpoint: `POST /files/upload-batch`

Do **not** modify the existing `POST /files/upload` (single-file). Add a new route `POST /files/upload-batch` to keep backward compatibility.

**Route registration** in `api.RegisterAPI`:

```go
r.With(d.protect(auth.PermFileUpload)).Post("/files/upload-batch", h.uploadFileBatch)
```

**Request format:** multipart/form-data with any number of `file` fields. Each field may be an audio file or an image file (detected by extension/MIME). Filenames may contain a relative path prefix (from `webkitdirectory`): e.g. `MyAlbum/track01.mp3`, `MyAlbum/cover.jpg`.

**Response:**

```json
{
    "results": [
        {
            "filename": "MyAlbum/track01.mp3",
            "ok": true,
            "existed": false,
            "hash": "...",
            "size": 12345,
            "cover_found": true,
            "cover_processing": true
        },
        {
            "filename": "MyAlbum/cover.jpg",
            "ok": true,
            "role": "cover",
            "album": "MyAlbum",
            "artist": ""
        },
        {
            "filename": "MyAlbum/bad.exe",
            "ok": false,
            "error": "unsupported media type"
        }
    ],
    "album_groups": [
        {
            "group": "MyAlbum",
            "track_count": 1,
            "cover_base_key": "a3f1c8d2e4b7f901",
            "cover_source_ext": ".jpg",
            "cover_variants_ready": false
        }
    ]
}
```

**Server-side logic for `uploadFileBatch`:**

1. Parse multipart form with `r.ParseMultipartForm(h.maxUploadSize * int64(maxBatchFiles))`. Define `maxBatchFiles = 500`.
2. Iterate `r.MultipartForm.File["file"]` headers.
3. For each file header:
   a. Extract the relative path from the filename (preserve it in the result `filename` field; use `filepath.Base` for on-disk name).
   b. Determine the top-level directory group: `strings.SplitN(relPath, "/", 2)[0]` — if the filename contains no `/`, the group is `""` (ungrouped).
   c. Classify by extension:
      - Audio extension → audio file; process via `processOneUpload` (extracted from `uploadFile` logic, reusable)
      - Image extension (`cover`, `folder`, `front`, `albumart`, `artwork` filename stems, case-insensitive) → cover candidate for the group
      - Anything else → reject with `ok: false, error: "unsupported media type"` — do not abort the whole batch
4. After all audio files are processed, for each group:
   a. Determine album/artist from the metadata extracted across tracks in the group (take the first non-empty album/album_artist seen)
   b. If an explicit cover image was found for the group: call `SetAlbumCover` + `EnqueueImageJob` (overrides any embedded cover)
   c. If no explicit cover but at least one track had embedded art: already handled in `processOneUpload` via `maybeSaveEmbeddedCover`

**Cover file detection rules (step 3c above):**

A file is treated as an album cover candidate if ALL of:
- Its MIME type is in `allowedImageMIMETypes`
- Its extension is in `allowedImageExtensions`
- Its base filename stem (lowercased, no extension) is one of: `cover`, `folder`, `front`, `albumart`, `artwork`, `album`

If multiple qualifying image files exist in one group, use the largest by byte size (highest quality assumption).

**`processOneUpload` helper:** extract the core logic of `uploadFile` into a private function callable from both `uploadFile` and `uploadFileBatch`. Signature:

```go
type uploadResult struct {
    OK           bool
    Existed      bool
    Hash         string
    Filename     string
    Size         int64
    Error        string
    CoverFound   bool
    CoverProcessing bool
}

func (h *handler) processOneUpload(ctx context.Context, file io.ReadSeeker, header *multipart.FileHeader) uploadResult
```

`uploadFile` becomes a thin wrapper that calls `processOneUpload` and writes the JSON response.

---

### 4b. Tests for Phase 4

- `api/handlers_test.go`:
  - `TestUploadBatch_MixedFiles`: POST a batch with 2 MP3s + 1 cover.jpg + 1 .exe; assert 2 audio results ok, 1 cover result, 1 rejected, 1 album group
  - `TestUploadBatch_CoverPriorityOverEmbedded`: batch with an audio file that has embedded cover AND an explicit `cover.jpg` for the same group; assert `SetAlbumCover` is called once and uses the explicit file's `base_key`
  - `TestUploadBatch_UngroupedFiles`: files with no directory prefix land in group `""` with no cover auto-detection
  - `TestUploadBatch_EmptyBatch`: POST with no files; return 200 with empty `results` and `album_groups`

---

## Phase 5 — Upload Page (`/upload`, webui)

### Goal
A new web UI page at `/upload` where users can upload files and folders, see per-album cover previews, replace covers manually, and track async processing status.

---

### 5a. Route registration

**File to modify:** `webui/webui.go`

Register the new page in `Register`:

```go
r.Get("/upload", renderTemplate("upload.html"))
```

Or a dedicated handler if it needs to inject data (prefer template with no server-side data injection — all state comes from API calls).

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
│  Upload queue                                │
│  ┌──────────────────────────────────────┐   │
│  │ filename.mp3    3.2 MB   [========  ]│   │
│  │ track02.mp3     4.1 MB   [====      ]│   │
│  │ bad.exe         rejected             │   │
│  └──────────────────────────────────────┘   │
│  [Upload all]  [Clear]                       │
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

**Cover preview card (per album group):**
- Shows `medium_crop` variant URL if `variants_ready: true`, else a grey placeholder with a spinner overlay
- Album name + artist (derived from first track's metadata in the batch result)
- Track count
- "Replace cover" button — opens `<input type="file" accept="image/jpeg,image/png,image/webp">` → POSTs to `POST /api/albums/{album}/image?artist={artist}`; refreshes card after response

**Polling:**
- When a card has `variants_ready: false`, poll `GET /api/albums/{album}/image/status?artist={artist}` every 2 seconds
- Stop polling when `variants_ready: true`; replace placeholder with real image (use `small_crop` variant for the card thumbnail)
- Stop polling if the page becomes hidden (`document.visibilitychange` → `document.hidden`)

**Drop zone JS:**
- `dragover` / `drop` events — prevent default, read `e.dataTransfer.files`
- `<input type="file" multiple>` for "Browse files"
- `<input type="file" webkitdirectory multiple>` for "Browse folder"; set `directory` + `mozdirectory` attributes for cross-browser
- Client-side pre-filter before adding to queue: reject files whose extension is not in the allowed audio or image set; show inline rejection message per file
- Store files in a JS array (the queue); render queue list
- "Upload all" button: POST all files to `POST /files/upload-batch` as a single multipart request (use `FormData`, append each file as `file` field, use `file.webkitRelativePath || file.name` as filename)
- Handle upload progress via `XMLHttpRequest` `upload.onprogress` per request (one request for the whole batch)
- On response: parse `results` and `album_groups`; render album group cards

**Access control on "Replace cover" button:**
- On page load, call `GET /api/auth/me`
- If user has permission `metadata.edit` (check `permissions` array in response): show "Replace cover" buttons
- If not (or if the call fails / user is anonymous): hide "Replace cover" buttons

**Error handling:**
- If the whole upload request fails (network, 500): show a banner error with a retry button
- Per-file errors in `results[].error`: shown inline in the queue list
- If polling returns 404 for the album status: stop polling (album was deleted)

---

### 5c. Static assets

No new static files needed. The upload page uses the same CSS variables and design tokens as `library.html` and `cmus.html` for visual consistency.

---

### 5d. Manual testing checklist (for the verifying agent)

- [ ] Drop a single MP3 → appears in queue → Upload → appears in library
- [ ] Drop a folder with 3 MP3s and a `cover.jpg` → 1 album group card appears → cover thumbnail shows after polling resolves
- [ ] Drop a file with an unsupported extension → inline rejection, not added to queue
- [ ] "Replace cover" uploads a new image → spinner appears → resolves to new image
- [ ] Drop two separate album folders → two distinct cover cards, each with their own polling
- [ ] `max_parallel_workers = 1` in config → variants still appear eventually
- [ ] Page while upload in progress → visually clear progress bar

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
4. Draw the `small_crop` image onto an offscreen `<canvas>` (CORS: `crossOrigin = "anonymous"` — works same-origin)
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
| `config/config.go` | Add `ImagesConfig`, `Config.Images`, default, validation | 1b |
| `madshare.toml.example` | Document `[images]` section | 1b |
| `CLAUDE.md` | Document `[images]` config | 1b |
| `database/migrations/009_image_variants.sql` | New migration | 1c |
| `media/images.go` | `ProcessImage`, variant constants, `BaseKey`, `VariantPath`, `VariantURL` | 1d–1e |
| `database/images.go` | New DB methods for job queue and cover variants | 1f |
| `database/repo.go` | Add new methods to `Repository` interface | 1f |
| `imageproc/worker.go` | `Pool` type, worker goroutines | 1g |
| `madshare.go` | Wire pool, call `ResetStaleJobs` | 1g |
| `api/api.go` | Add `ImagePool` to `Deps`, register status route | 1h |
| `api/library_handlers.go` | Add `getAlbumImageStatus`; rewrite `uploadAlbumImage`; delete `saveImageUpload` | 1h, 3a |
| `media/extract.go` | Add `CoverData`, `Tags.CoverImage` | 2a |
| `api/handlers.go` | Add `maybeSaveEmbeddedCover`, `imagePool` field, extend upload response | 2b–2c |
| `api/handlers.go` | Extract `processOneUpload`, add `uploadFileBatch` | 4a |
| `webui/webui.go` | Register `/upload` route | 5a |
| `webui/html/upload.html` | New upload page | 5b |
| `webui/html/cmus.html` | Add aura effect JS and CSS | 6b |

---

## Appendix — API Contract Summary

| Method | Path | Auth | Description |
|---|---|---|---|
| `POST` | `/files/upload` | `file.upload` | Single file upload (unchanged) |
| `POST` | `/files/upload-batch` | `file.upload` | Batch + folder upload |
| `GET` | `/api/albums/{album}/image` | none | Serve original cover image |
| `POST` | `/api/albums/{album}/image` | `metadata.edit` | Upload/replace cover; triggers async job |
| `GET` | `/api/albums/{album}/image/status` | none | Variant readiness + all variant URLs |

Query param for album endpoints: `?artist=<album_artist>` (empty string allowed — matches rows with empty album_artist).
