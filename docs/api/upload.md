# File Upload API

How files enter the library: the single-file upload endpoint, content
deduplication, restore-on-reupload, and embedded cover-art extraction.

There is **no batch endpoint** — clients upload one file per request and manage
their own queue/concurrency. The upload page (Phase 5) drives many of these
requests in parallel.

---

## `POST /files/upload`

Uploads a single file via `multipart/form-data`.

### Request

```
POST /files/upload
Content-Type: multipart/form-data
```

| Part   | Required | Description |
|--------|----------|-------------|
| `file` | yes      | The file to upload. Accepted by its **filename extension** (see below); the part's `Content-Type` is not used to gate. |

```bash
curl -X POST -F "file=@./song.mp3" http://localhost:3000/files/upload
```

### Access control

Requires the `file.upload` permission **when authentication is configured**.
With no auth backend (open embedding / tests) the gate is a pass-through. Default
upload access and how it is granted are described in
`docs/architecture/auth.md`.

### Accepted types

v0 accepts **audio only** (video is deferred). The **filename extension** is the
gate (case-insensitive); each accepted extension maps to a canonical MIME type
that the server persists and serves:

| Extension | Canonical MIME |
|-----------|----------------|
| `.mp3`    | `audio/mpeg`   |
| `.ogg`    | `audio/ogg`    |
| `.flac`   | `audio/flac`   |
| `.wav`    | `audio/wav`    |
| `.mp4`, `.m4a` | `audio/mp4` |
| `.aac`    | `audio/aac`    |
| `.opus`   | `audio/opus`   |

The extension is the security-relevant guard — it determines what the file
server later advertises (reinforced by `X-Content-Type-Options: nosniff` on the
file routes). The part's declared `Content-Type` is **not** consulted: browsers
leave it empty for FLAC/M4A/OPUS (sent as `application/octet-stream`), and
`curl -F` defaults to `application/octet-stream` — gating on it would reject
valid audio. The canonical MIME above is stored instead. The same allow-list is
served to the upload page at `GET /api/ui/config` (`accepted_audio`) so the
browser can flag disallowed files before upload.

The filename is sanitised to a safe base name first (Windows and Unix path
components stripped, control characters rejected), so a client-supplied path
like `C:\Users\evil.mp3` or `../../etc/track.mp3` is stored as just `track.mp3`.

### Size limits

- `storage.max_upload_mb` (default 500 MiB) caps the request body. Exceeding it
  returns `400`.
- Uploads up to ~50 MB are hashed in memory; larger ones are spooled to the
  cache directory while hashing. This threshold is internal and distinct from
  `max_upload_mb`.

### Content addressing & metadata

The file is identified by the SHA-256 of its bytes (the `hash`). On first
ingest of a given hash the server:

1. extracts audio tags (ID3 / MP4 / FLAC / OGG via `dhowden/tag`),
2. stores the blob at `<files_dir>/audio/<hash>/<filename>`,
3. inserts the `files` / `file_uploads` / `media_metadata` rows,
4. extracts and processes any embedded cover art (see below).

### Response

`201 Created` for a newly stored file, `200 OK` when the bytes already existed
(dedup or restore). Always `application/json`.

With authentication configured, a **new upload stages as a draft** in the
uploader's "My uploads" area (`"pending": true`) instead of entering the
library directly — see `docs/architecture/moderation.md`. Without auth there
is no staging and inserts are immediately approved.

```json
{
  "ok": true,
  "existed": false,
  "pending": true,
  "hash": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
  "filename": "song.mp3",
  "size": 4823192,
  "title": "Breathe",
  "album": "The Dark Side of the Moon",
  "artist": "Pink Floyd",
  "cover_found": true,
  "cover_processing": true
}
```

| Field              | Type    | Description |
|--------------------|---------|-------------|
| `ok`               | boolean | Always `true` on a 2xx response. |
| `existed`          | boolean | `false` when the bytes were newly stored; `true` when they already existed (dedup or restore). |
| `pending`          | boolean | The file is staged (awaiting review) after this request — a fresh draft, a dedup against someone's staged file, or a restore that re-staged. `false` for library-live or still-trashed content. |
| `restored`         | boolean | Dedup/restore path only: this request restored a trashed file (see [Trash-restore policy](#trash-restore-policy)). |
| `trashed`          | boolean | Dedup path only: the content exists but stays soft-deleted (policy did not restore it). |
| `hash`             | string  | SHA-256 of the file contents (the content address). |
| `filename`         | string  | The sanitised filename recorded for this upload. |
| `size`             | integer | File size in bytes. |
| `title`            | string  | Title tag, echoed for client display. Same emptiness rules as `album`. |
| `album`            | string  | Album tag, echoed so the upload page can group tracks and target the cover endpoints. Empty when untagged or on the dedup/restore path (tags are not re-extracted for existing bytes). |
| `artist`           | string  | Effective album artist (`album_artist`, falling back to `artist`); same emptiness rules as `album`. |
| `cover_found`      | boolean | Embedded cover art with usable album + artist context was present in the tags. See [Embedded cover extraction](#embedded-cover-extraction). |
| `cover_processing` | boolean | This upload actually claimed the album cover and queued variant generation. |

### Error responses

| Status | Condition |
|--------|-----------|
| 400    | Body exceeds `max_upload_mb`, malformed multipart, missing `file` part, or invalid filename. |
| 413/415| Filename extension not on the accepted-audio allow-list (`415 Unsupported Media Type`). |
| 429    | A concurrency limit was reached — see [Concurrency limits](#concurrency-limits). |
| 500    | Storage or database error. |

---

## Concurrency limits

Uploads are gated by two server-side caps so a single client (or the whole
fleet) cannot saturate the server:

- `storage.server_max_parallel_workers` — total concurrent uploads across **all**
  users.
- `storage.user_max_parallel_workers` — concurrent uploads **per user**.

`0` (the default) means that dimension is unlimited. Both caps apply to every
user — **there is no admin bypass** (the identity model has no role signal to key
one on). A slot is held only for the duration of a single `POST /files/upload`
and released as soon as it completes (success or error).

When a request would exceed either cap it is rejected **without blocking** with
`429 Too Many Requests`, a `Retry-After: 1` header, and this body:

```json
{ "error": "server upload limit reached", "code": "upload_limit" }
```

`error` is `"user upload limit reached"` when the per-user cap is the one hit.
The `code` is always `upload_limit`. The upload page (Phase 5) treats this code
as backpressure: it reduces its parallel-worker count and re-queues the file
rather than surfacing an error. The client-side worker ceiling itself comes from
`GET /api/ui/config` (see `docs/api/cover-images.md`); the server caps above are
independent and authoritative.

---

## Deduplication

Files are content-addressed by hash, so **the same bytes are stored once**.
Re-uploading identical content does not write a second blob — it returns
`200` with `existed: true` and records the (possibly new) filename against the
existing file, so the same audio can be known under multiple names.

Embedded cover art is **not** re-processed on the dedup path: a duplicate upload
returns `cover_found: false` and `cover_processing: false` regardless of what the
file's tags contain, because the cover (if any) was already handled when the
bytes were first ingested.

---

## Restore on re-upload (soft-deleted files)

Madshare uses **soft delete**: an admin "delete" only marks a file as trashed
(sets `deleted_at`); the blob and its rows are kept until a hard delete from the
Trash tab. Full design: `docs/architecture/soft-delete.md`.

> **Re-uploading a soft-deleted file does not re-upload anything.** Because the
> bytes are already on disk, the server simply **clears the `deleted_at` mark**
> (`RestoreFileByHash`). No blob is rewritten and no content is transferred
> beyond what is needed to compute the hash. The response is `200` with
> `existed: true, restored: true`, and the action is recorded in the audit log
> as `file.restore` (distinct from a plain dedup, which records `file.upload`
> / `dedup:`).

Where the restored file lands depends on its pre-trash review state
(`docs/architecture/moderation.md`):

- **Previously approved** (with auth configured): the restore is demoted to
  the **restorer's draft** — it lands in their "My uploads" staging area
  (`"pending": true`), not the library. A re-upload must not let any
  `file.upload` holder republish trashed content past the review queue.
- **Trashed while pending**: state and owner survive — the file re-enters the
  queue (or the original owner's staging) where it was.
- **Auth unconfigured**: restored to the library as-is (no staging exists).

If an operator wants a file gone for good, they must **hard-delete** it from
the Trash tab rather than relying on soft delete.

---

## Embedded cover extraction

When a **new** audio file is ingested, Madshare reads any embedded cover art
(e.g. an ID3 `APIC` frame) and uses it as the **album cover** — but only when the
album does not already have one. This is the *fill-if-missing* rule.

### Behaviour

- **Fill-if-missing, never overwrite.** If the album already has a cover (from an
  earlier track or a manual upload), the embedded art is ignored. An explicitly
  uploaded cover therefore always beats embedded tag art.
- **Album + artist required.** The cover is keyed on the album title and the
  *effective album artist* — the album-artist tag (`TPE2`) when present,
  otherwise the track artist (`TPE1`/`Artist`). A file with embedded art but no
  album (or no artist) is skipped: `cover_found` is `false`.
- **Supported formats only.** Only `image/jpeg` and `image/png` embedded covers
  are accepted; any other embedded format (WebP, GIF, …) is skipped without
  queuing a job. `cover_found` is still `true` (art was present), but
  `cover_processing` is `false`.
- **Size cap.** Embedded covers larger than 10 MB are skipped (matching the
  manual-upload cap), guarding against oversized art inflating storage.
- On a successful fill, the original is stored at
  `<files_dir>/images/<base_key>/original<ext>`, an `image_processing_jobs` row
  is enqueued, and the worker pool generates the square variants. Readiness is
  polled via `GET /api/albums/{album}/image/status` — see
  `docs/api/cover-images.md`.

### `cover_found` vs `cover_processing`

| `cover_found` | `cover_processing` | Meaning |
|:-------------:|:------------------:|---------|
| `true`  | `true`  | Embedded art present and **this upload set the album cover** (a variant job was queued). |
| `true`  | `false` | Embedded art present but **not used** — the album already had a cover, the format was unsupported, the art exceeded the size cap, or a concurrent upload won the race. |
| `false` | `false` | No usable embedded art (none present, or no album/artist context). Also the value on any dedup/restore response. |

### Concurrency

Several tracks of the same album are commonly uploaded at once, all carrying the
embedded cover. The fill-if-missing decision is resolved **atomically** at the
database level (`SetAlbumCoverIfAbsent` — an `INSERT … ON CONFLICT DO NOTHING`),
so exactly one upload claims the cover and queues a single job; the rest report
`cover_processing: false`. Tracks normally embed identical art, so the shared
content address means no duplicate files are written.

### Notes / limitations

- **MP4/M4A:** the tag library only reports an MP4 cover when it can infer the
  image MIME type. An MP4 with an implicit-flagged embedded **JPEG** may extract
  no cover; such albums import without art. Use the manual cover upload
  (`POST /api/albums/{album}/image`) as a fallback.
- Embedded covers are written as-is without re-decoding at upload time; a corrupt
  embedded image fails later in the variant worker (the job is retried then
  marked failed) rather than failing the upload.

---

## `POST /api/files/check` — pre-upload existence check

An **advisory** check so a client can skip uploading content the server already
has. Gated on `file.upload` (same as upload — a by-hash existence oracle must not
be anonymous).

### Request

```
POST /api/files/check
Content-Type: application/json

{ "hash": "<sha256-hex>" }     # 64 lowercase hex chars (SHA-256 of the raw bytes)
```

### Response

```json
{ "status": "absent" | "present" | "pending" | "trashed" }
```

| `status`  | Meaning | Client action |
|-----------|---------|---------------|
| `absent`  | No content with this hash | upload it |
| `present` | Content exists and is live | skip (duplicate) |
| `pending` | Content exists but is staged, awaiting review (`docs/architecture/moderation.md`) | skip ("already uploaded — awaiting review") |
| `trashed` | Content exists but is soft-deleted | per the [trash-restore policy](#trash-restore-policy) |

| Status | Condition |
|--------|-----------|
| 200    | `{ "status": … }` |
| 400    | missing/empty hash, or not 64 hex chars |
| 401/403| not authenticated / lacks `file.upload` |

**Advisory only.** The endpoint only *reads*; it never mutates state and is **not**
the dedupe authority. `POST /files/upload` always re-hashes every received file
(`storage.HashUpload`) and dedupes on the server-computed value, so a wrong or
malicious client hash can at worst skip its own upload — it can never cause a
duplicate or a wrong dedupe.

---

## Trash-restore policy

What happens when uploaded content matches a **trashed** file is an admin policy:

| Mode | On a trashed match… |
|------|---------------------|
| `reupload_restores` **(default)** | `POST /files/upload` restores the file from the re-sent bytes (the dedup response carries `"restored": true`). |
| `inform` | the file stays trashed (`"trashed": true, "restored": false`); the UI tells the uploader to ask an admin. |
| `uploader_restore` | the file stays trashed on reupload, but the uploader may restore it directly via the endpoint below. |

Either restore route re-stages a previously approved file as the restorer's
draft when auth is configured — see [Restore on
re-upload](#restore-on-re-upload-soft-deleted-files).

- **Read/set (admin):** `GET` / `POST /api/admin/settings/trash-policy`
  (`user.manage`). Body: `{ "policy": "reupload_restores" | "inform" |
  "uploader_restore" }`. The current policy is also returned in
  `GET /api/ui/config` as `trash_restore_policy` so the upload UI can act on it.
- **Uploader restore:** `POST /api/files/{hash}/restore` (`file.upload`). Succeeds
  **only** when the policy is `uploader_restore` — otherwise `403`; 404 if no
  trashed file matches. Response `{ "ok": true, "staged": bool }` — `staged`
  reports that the restore was demoted to the restorer's draft.

---

## See also

- `docs/api/cover-images.md` — cover variant status and UI config endpoints.
- `docs/architecture/moderation.md` — staging / review queue (where uploads land).
- `docs/architecture/soft-delete.md` — trash / restore model.
- `docs/architecture/auth.md` — upload permission and access control.
