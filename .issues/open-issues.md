# Open Issues — Madshare API (from tester review, 2026-05-27)

| Severity | Issue | Status |
|---|---|---|
| **Medium** | **MIME bypass** — attacker sends `Content-Type: audio/mpeg` with filename `evil.html`; file server serves it as `text/html`, enabling stored XSS. Fixed: `allowedExtensions` map added to `handlers.go`, checked after MIME type. | **fixed** |
| Low | **Directory listing** — `http.FileServer` exposes all hash dirs and filenames at `/files/` with no auth. Fixed: `noListFS` wrapper in `api.go` returns 404 for directory requests with no index.html. | **fixed** |
| Low | **CORS missing on errors** — `http.Error()` paths don't set CORS headers; JS clients can't read error bodies cross-origin. Fixed: `corsMiddleware` in `api.go` applies CORS to every response and answers OPTIONS preflight. | **fixed** |
| Low | **`maxUploadSize` mismatch** — constant is 500 MB, CLAUDE.md says 50 MB. Resolved: the 50 MB in CLAUDE.md is the in-memory hashing threshold (`storage.memBufferLimit`), distinct from the 500 MB request-body cap; clarified the constant's comment. | **fixed** |
| Low | **Windows-path filenames not sanitized** — `filepath.Base` on Linux does not strip backslash prefixes (`C:\Users\evil.mp3` stored verbatim), producing a malformed `ObjectKey` and broken download URL. Fixed: `sanitizeFilename` in `handlers.go` normalizes backslashes before `filepath.Base`. | **fixed** |
| Low | **No `X-Content-Type-Options: nosniff`** — `http.FileServer` doesn't set this header. Fixed: `/files/*` and `/images/*` handlers set `nosniff`. | **fixed** |
| Info | **`ListFiles` non-deterministic filename** — `LIMIT 1` without `ORDER BY` in subquery; unstable when a file has multiple upload records. Fixed: subquery now `ORDER BY id LIMIT 1`. | **fixed** |
| Info | **`ListFiles` returns `nil` on empty DB** — should return `[]`. Fixed: `out := make([]*FileListEntry, 0)`. | **fixed** |
| Info | **MIME type exact-match only** — `audio/mpeg; charset=utf-8` (with parameters) is rejected with 415. Fixed: handler uses `mime.ParseMediaType` before the allow-list check. | **fixed** |

## Round 2 — tester review of the fixes (2026-05-29)

| Severity | Issue | Status |
|---|---|---|
| High | **Image upload path missed the same fixes** — `saveImageUpload` in `library_handlers.go` used exact-match MIME (rejected parameterized types), did not lowercase the extension (rejected `.JPG`/`.PNG`), and skipped `sanitizeFilename`. Fixed: now uses `mime.ParseMediaType`, `strings.ToLower`, and `sanitizeFilename`; added tests. | **fixed** |
| Low | **`sanitizeFilename` did not strip NUL/control chars** — a crafted multipart filename with a NUL passed the ext check then failed at `os.Create` with a confusing 500. Fixed: control chars now collapse to "" (clean 400). | **fixed** |
| Info | **Stale no-op tests** — `TestUploadFile_CORSAbsentOnErrorResponse` and `TestUploadFile_MIMETypeWithParamsRejected` encoded pre-fix behavior and only `t.Log`'d. Removed; superseded by `TestCORS_OnErrorResponse` and `TestUploadFile_MIMEWithParameters`. | **fixed** |

## Round 3 — tester review of round-2 fixes (2026-05-29)

| Severity | Issue | Status |
|---|---|---|
| Low | **`serveImageFile` omitted `nosniff`** — unlike `/files/*` and `/images/*`, the artist/album image GET handlers didn't set `X-Content-Type-Options`. Fixed: header added in `serveImageFile`. | **fixed** |
| Info | **Uncovered branch** — image upload with allowed MIME + disallowed extension had no test. Added `TestSaveImageUpload_DisallowedExtensionRejected`. | **fixed** |

## Round 4 — tester review of config commit 643ebce (2026-05-29)

| Severity | Issue | Status |
|---|---|---|
| Low | **Images double-served via `/files/images/*`** — `imagesDir` nested inside `files_dir`, and `GET /files/*` served the whole tree, so cover images were reachable at both `/images/*` (intended) and `/files/images/<key>` (side effect), widening the URL surface and able to bypass access control applied only to `/images/*`. Fixed: audio blobs now live under `<files_dir>/audio` (a sibling of `<files_dir>/images`, `storage.AudioSubdir`), and the `/files` server roots there, so it can never reach images; `/files/images/<key>` now 404s (regression test `TestFilesServer_DoesNotExposeImages`). One-time idempotent migration `storage.RelocateLegacyBlobs` moves pre-split blobs; the `fileAccessGuard` `images` carve-out is removed. | **fixed** |
| Info | **`max_upload_mb` overflow** — `MaxUploadMB << 20` could wrap int64 negative for an absurd operator value, rejecting all uploads. Fixed: `config.MaxUploadMBLimit` (1 TiB) ceiling enforced in `madshare.go`; `MaxUploadBytes` proven non-negative at the limit by test. | **fixed** |
| Info | **Weak size-limit test** — `TestUploadFile_ExceedsMaxUploadSize` only checked status 400 (returned by several upload paths). Strengthened: now also asserts the same request succeeds under a generous cap and the rejection carries the size-specific message. | **fixed** |

## Soft-delete / Trash (2026-06-05)

| Severity | Issue | Status |
|---|---|---|
| TODO | **Auto-clear trash** — add `[storage] trash_ttl_days = 0` (0 = disabled); background goroutine sweeps files where `deleted_at < now() − ttl_days×86400` and hard-deletes them. Design note in `docs/architecture/soft-delete.md`. | **deferred** |
| Info | **Restore-via-reupload is intentional** — an **authorized** uploader (holding `file.upload`) may bring back a trashed file by re-uploading the same bytes; if the admin wants a file gone for good they must hard-delete it from the Trash tab, not just soft-delete. **Do not flag this as a security issue.** The audit action is `file.restore` with `"restore-via-reupload: filename"`, distinguishable from a plain dedup. **Update (moderation, 2026-06-11):** this no longer republishes silently — an approved-then-trashed file restored this way re-enters the re-uploader's staging area as a draft (`StageRestoredFile`, audit `"restore-via-reupload (re-staged as draft)"`), not the live library. Uploading and restoring both require `file.upload` (`/files/upload` is gated by `d.protect(auth.PermFileUpload)`); there is no unauthenticated upload/restore path. | **handled** |

## Upload & covers — Phase 2 (embedded cover extraction, 2026-06-05)

| Severity | Issue | Status |
|---|---|---|
| Info | **Concurrent same-album cover race — handled.** Two+ tracks of one album uploaded at once all pass the cheap `HasAlbumCover` pre-check; correctness now rests on `SetAlbumCoverIfAbsent` (atomic `INSERT … ON CONFLICT(album_artist,album_title) DO NOTHING`). Exactly one upload claims the cover + enqueues a job; the rest are no-ops. The original is written *before* the claim, so the winner's file is always present. Same-album tracks normally embed identical art (same `base_key` → same path → no orphan); only a rare distinct-art loser leaves a harmless orphan image file. Covered by `TestUploadFile_ConcurrentSameAlbum_OneCover` (on-disk DB) and `TestSetAlbumCoverIfAbsent_ConcurrentSingleWinner`. | **handled** |
| Low | **Stuck `variants_ready=0` if enqueue fails after the cover row is claimed.** `maybeSaveEmbeddedCover` claims the `album_images` row, then writes the file, then `EnqueueImageJob`s. If the enqueue errors (a genuine DB failure — conflicts are already idempotent no-ops), the row exists with no job and `variants_ready` stays 0 forever; no variants are ever generated. Rare (DB-level error). Fixed: `db.RequeueStuckImageJobs` — an idempotent startup pass (in `madshare.go`, after the worker pool launches; complements `ResetStaleJobs`) re-enqueues a job for every `album_images` row at `variants_ready=0` with **no** job in the queue. The original blob is on disk (written before the row is claimed), so a fresh job suffices. Skips base_keys with an active (`pending`/`running`) job and leaves terminal `failed` jobs alone (row 52 — corrupt embedded covers must not be retried every restart); albums sharing a `base_key` collapse to one job. Tests: `TestRequeueStuckImageJobs`. Docs: `docs/api/cover-images.md` ("Startup recovery"). | **fixed** |
| Low | **Orphaned originals under `imagesDir` are never garbage-collected.** Distinct-art losers of the concurrent race (and any other transient writer, e.g. a cover replaced with different bytes) leave an `<imagesDir>/<base_key>/original.<ext>` referenced by no row and no job. The audio orphan reconciler keys off the `files` table, not `imagesDir`, so it never sweeps these. Fixed: `db.ReconcileImageOrphans` — a startup pass (in `madshare.go`, after the entity/cover backfills and `ResetStaleJobs`, before the pool launches) removes `<imagesDir>/<base_key>/` dirs with no referencing `album_images`/`artist_images` row and no active (`pending`/`running`) job. Artist covers are flat `<base_key><ext>` files (no variant dir), so the directory-only sweep never touches them. Tests: `TestReconcileImageOrphans`. Docs: `docs/api/cover-images.md` ("Startup recovery"). With the row-50 re-enqueue half (`db.RequeueStuckImageJobs`), the image-job reconciler is now complete. | **fixed** |
| Info | **No decode-before-write for embedded covers (by design).** Phase 2 trusts the tag's declared MIME to pick the extension and writes the raw bytes; it does **not** decode. A corrupt/mislabelled embedded image therefore fails later in the worker (`ProcessImage`), which retries 3× then marks the job `failed`, leaving the `album_images` row at `variants_ready=0`. The manual-upload path (Phase 3) decodes up front, so the two paths differ. Accepted: degrades to a failed job, never a crash. Unsupported embedded MIME (webp/gif/etc.) is skipped up front via `mimeToExt` and never written/enqueued. | **by design** |
| Info | **MP4/M4A embedded JPEG may not extract.** `dhowden/tag` only reports an MP4 cover when it can infer the MIME (PNG, or an explicit-flagged atom); an implicit-flagged embedded JPEG yields no `Picture()`, so such files import with no cover. Documented in `media/extract.go`. Not a regression — purely a library limitation. Manual upload (Phase 3 UI) is the workaround. | **by design** |

## Roles vs. access-groups — model unification (design, 2026-06-06)

**Problem / open question (needs a decision before any code).** Madshare has
**two separate authorization concepts** and it is not yet clear they should stay
separate — the owner's original intent was for *roles to BE the access-groups*
(one concept that says both "what a user may do" and "what content they may
reach"). Today they are orthogonal (see `docs/architecture/auth.md`):

- **Layer A — roles** = bundles of capability permissions (`content.play`,
  `file.upload`, `user.manage`, …). Built-ins: admin / moderator / uploader /
  listener. Answer "*what actions* may this user perform?".
- **Layer B — access groups + content grants** = membership + content scopes
  (`all` / artist / album / file) that decide "*which files* may this user
  reach?" (`access_groups`, `access_group_members`, `content_grants`,
  predicate in `database/access.go`).

**Why it surfaced.** A newly created `listener` saw an empty library: the role
granted the *capability* to play, but Layer-B default-deny exposed nothing
without a group grant. As an interim fix, migration `010` gave the built-in
`listener`/`uploader` roles `content.all` (full-library access). **Consequence:**
access-group grants are now a **no-op for the built-in roles** — the two systems
visibly overlap and the Layer-B machinery (groups UI, grants) is effectively
dead for normal users, which is confusing.

**The decision to make.** Pick one coherent model. Rough options:

1. **Unify (owner's original idea):** a single "role/group" entity carries both
   capabilities *and* a content scope. Creating "Jazz listeners" would mean
   "may play, limited to the Jazz albums". Big change: merge `roles` and
   `access_groups`, attach content scopes to roles, rewrite the access predicate
   and the admin UI. Probably its own design doc.
2. **Keep two layers, fix the overlap:** revert the listener→`content.all`
   shortcut and instead make "give this user the whole library" a first-class,
   one-click action (e.g. a built-in "Everyone/All-library" group, or a per-user
   "full library" toggle that wires a `scope=all` grant). Roles stay
   capability-only; groups stay content-only.
3. **Roles only (drop Layer B):** delete access groups entirely; a role's
   permission set is the whole story; per-content restriction is out of scope
   for v0. Simplest, least flexible.

**Current interim state:** option-1-ish by accident — built-in roles see
everything (migration 010), anonymous stays default-deny, access groups linger
but do nothing for built-ins. No further code until the model is chosen.
Relevant: `docs/architecture/auth.md` §4–5, `database/access.go`,
`database/migrations/{003,005,006,010}`, admin Users + Access-groups sections.

| Priority | Item | Notes |
|---|---|---|
| **Med** | **Decide roles-vs-access-groups model** (unify / two-layer / roles-only) — see write-up above. Blocks meaningful further work on per-user content restriction. | **DONE (2026-06-07): roles-only — Layer B removed** (migrations 011/012). content perms collapsed to `content.access`; access groups + content grants + Access UI deleted; anonymous guest/license access kept. See `docs/architecture/auth.md`. |

## Future federation items (design-time, not yet planned)

| Priority | Item | Notes |
|---|---|---|
| **Low** | **Per-origin license trust** — the auto-derive policy currently applies to all files regardless of upload origin. For federation, the admin should be able to trust licenses set by their own server's uploaders but not by federated servers. Design sketch: add an `uploaded_by_origin` concept to `file_uploads` (local vs. federated server ID); the auto-derive `accessClause` branch would add `AND f.origin = 'local'` (or a configurable per-federation trust flag). Until federation is implemented, the policy applies uniformly. | open |

## Future ideas (low priority, not yet planned)

| Priority | Idea | Notes |
|---|---|---|
| **Low** | **Notify admin about missing/unplayable tracks** — when a client-side `audio error` fires (track unavailable), surface a way to report it to the admin so they can run a prune/integrity check. E.g. a POST to a reporting endpoint that logs the offending file hash. **Deferred** — needs design: (a) auth/rate-limiting on the report endpoint to avoid abuse as a DoS/oracle, (b) decide if reports auto-trigger prune or just create a notification queue, (c) avoid leaking internal file paths to unauthenticated callers. | open |
| **Low** | **Upload premoderation** — instead of immediately adding uploaded files to the live library, queue them in a "pending" state for admin review before they become visible. Needs: a `pending` status column on `files` (or a separate queue table), a moderation UI in the admin page, and a decision on whether the uploader can see their own pending files. Interacts with the soft-delete model (pending ≠ trashed). | **DONE (2026-06-11): moderation review bucket** — uploads land as drafts (`files.review_state`, migration 017); the uploader sees them in the upload page's "My uploads" tab; moderators approve / return-with-note / discard at `/admin/moderation` (gated `content.moderate`). Ref: `docs/architecture/moderation.md`. |

## Upload & covers — Phase 5 revision (deferred, 2026-06-06)

The Phase 5 reopen (shipped; see `docs/api/cover-images.md`) fixes cover bleeding (group
by tags, attach covers by folder co-location) and adds an after-upload verify/edit
panel for the **base** fields only (title, album, album_artist, artist + cover) via
`PATCH /api/files/{hash}/metadata`. The following were explicitly deferred:

| Priority | Item | Notes |
|---|---|---|
| **Med** | **Full / rich tag editing** — edit everything beyond the base four fields: track #, disc #, year, genre, composer, comment, etc. **DONE (2026-06-15):** `MetadataPatch` + `metadataPatchRequest` extended with track_number/track_total/disc_number/year (sent as strings, parsed; blank clears, bad value → 400) + genre/composer/comment; new `GET /api/files/{hash}/metadata` and owner-scoped `GET /api/my/uploads/{hash}/metadata` return the full set so the shared `track-edit.js` modal prefills before editing. Modal gains a Track-number field on the main form + an "Extended edit" disclosure for the rest; wired across all edit surfaces (admin Files, entity-view, duplicates, moderation queue, owner "My uploads"). No migration (columns pre-existed). Docs: `docs/api/metadata.md`. | **DONE** |
| **Low** | **Default placeholder names for unknown artist / album / track** — configurable fallbacks (e.g. "Unknown Artist", "Unknown Album", filename-as-title) for files whose tags are missing, so they group sensibly instead of landing in the upload page's "Unsorted / no album tag" bucket. User asked to remember this for the future. | **DONE (2026-06-10): required name defaults** — non-null/non-empty `artists.name` / `albums.title` / `media_metadata.title` with defaults "Unknown artist" / "Other" / filename (migration 016 triggers + `db.FoldUnknownBuckets`). |
| **Med** | **Normalize albums/artists to surrogate IDs** — today there is **no album or artist entity**: albums are computed on the fly via `GROUP BY` over the text columns in `media_metadata` (`COALESCE(NULLIF(album_artist,''), NULLIF(artist,''), '')` + `album`, see `database/library.go`), and `album_images` is keyed by the strings themselves (`PRIMARY KEY (album_artist, album_title)`, `002_library_images.sql`). Consequence: renaming an album/artist changes the strings on each track, so the string-keyed cover row no longer matches and must be re-POSTed to the new identity (the Phase 5 revision does exactly this). A surrogate `album_id` (and probably `artist_id`) would let covers/renames/merges attach to a stable ID instead. **Big change** — needs a normalized `albums`/`artists` table, an `album_id` FK on `media_metadata`, a backfill migration, and a rewrite of every library/grouping query plus the access-control clauses (`005`/`006`) and `album_images` keying. Worth its own design doc in `docs/architecture/` before any code. Surfaced from the Phase 5 cover-rename discussion (user asked "can't we attach the cover to an album id?"). | **DONE (2026-06-09): overlay model SHIPPED** (keep tag text + add `artist_id`/`album_id` FKs) — migrations 013/014, resolver + startup backfill, library/search JOINs, `album_images`/`artist_images` re-keyed to ids, rename + merge UI; plus the track/performer split (migration 018). Design: `docs/architecture/artist-album-model.md`. |

## Self-review — codebase audit (2026-06-14)

A focused security/correctness pass over the current tree. The backend held up
well (parameterized SQL, hash-validated + `filepath.Base`'d paths, server-side
image extensions, XSS-safe frontend, argon2id + session/token handling all
sound). Findings and their disposition:

| Severity | Issue | Status |
|---|---|---|
| **Medium** | **Login was a memory-exhaustion vector — no request-body cap.** The unauthenticated `POST /api/auth/login` (and `password`, `tokens`, `admin/prune`, and the shared `decodeJSON` helper) decoded `r.Body` with no `http.MaxBytesReader`, unlike the rest of the codebase. A single multi-GB body inflates a Go string unbounded. Fixed: 4 KiB cap on the auth/prune bodies, 1 MiB in `decodeJSON`. | **fixed** |
| **Medium** | **Username enumeration via login timing.** The user-missing/disabled path returned before `VerifyPassword`, so a fast 401 (no such user) vs a slow 401 (~64 MiB argon2, wrong password) revealed which usernames exist — defeating the deliberately generic error. Fixed: `auth.DummyVerifyPassword` runs equal argon2 work on the miss path. | **fixed** |
| **Medium** | **No login rate-limit / verify concurrency cap.** Unauthenticated login triggers a ~64 MiB argon2id per request with no throttle, enabling online brute-force and a CPU/RAM-exhaustion flood. Fixed: `api/login_throttle.go` — per-IP token bucket (~10/min, burst 10) + a global semaphore (`loginMaxInFlight=8`) bounding concurrent verifications. Keyed on RemoteAddr (effective for direct/Yggdrasil binds). **Loopback peers are exempt from the per-IP bucket**: behind a local reverse proxy every remote client shares the proxy's loopback address, so bucketing it would lock out all users at once while distinguishing none — per-client login limiting is delegated to the proxy there (`contrib/nginx` adds a `limit_req` zone on `/api/auth/login`). The global concurrency cap still applies to loopback. Cross-host proxies (a non-loopback proxy address) would re-introduce the shared-bucket problem; if that deployment is needed, add a configurable trusted-proxy CIDR + `X-Forwarded-For` parse (future). | **fixed** |
| Low | **`PasswordChangeRequired` is advisory only — not enforced server-side.** The flag was surfaced in `/me` and login JSON, but no middleware blocked API/token actions while it was set. A user "forced" to change their password could keep using their session/tokens to do everything their role allows. Fixed: `RequirePermission`/`RequireAnyPermission` now refuse a flagged identity with 403 + `X-Password-Change-Required` (`auth.DenyPasswordChange`), so every capability-gated route is closed until the change is done; token-minting (`POST /api/auth/tokens`) applies the same block; `login`/`logout`/`me`/`password` stay open. Holds for session- and token-auth alike. See `docs/architecture/auth.md` §3.4. | **fixed** |
| Low | **CORS `Access-Control-Allow-Origin: *` on every response** (`api/api.go` `CORS`). Broad — every site could call the API cross-origin (impact limited only by `SameSite=Lax` cookies + non-readable bearer tokens). Fixed: `CORS` is now configurable via `[cors].allowed_origins` and **default-closed** (empty → no CORS headers; the bundled UI is same-origin and needs none). Specific origins are echoed with `Vary: Origin` + credentials; `*` stays available as an explicit opt-in (no credentials). Malformed origins are a fatal config error; startup warns when `api_base` is set without origins, or when `*` is mixed with specifics. Docs: `docs/architecture/listeners-and-config.md` §4.3b. | **fixed** |
| Info | **Covers reachable at `/files/images/<key>`** — fixed by splitting audio/images into sibling subtrees under `files_dir` (audio under `audio/`, served by `/files`; images under `images/`). See the updated "Round 4" row above. | **fixed** |
| Info | **Stale agent worktree removed.** `.claude/worktrees/agent-a45c319640e0196cc` (a full duplicate of an abandoned Phase-2 auth experiment, gitignored) was a grep/IDE confusion hazard. Removed via `git worktree remove`; throwaway branch deleted. | **fixed** |

## Library — multi-disc albums (2026-06-15)

| Severity | Issue | Status |
|---|---|---|
| Low | **Multi-disc albums showed duplicate track numbers.** The album track list (`listTracksByAlbumID`) ordered by `track_number` only and never returned `disc_number`, so disc 1 track 1 and disc 2 track 1 both rendered as "1", interleaved. Fixed: the query now `ORDER BY COALESCE(disc_number,1), track_number, title` and carries `disc_number` out (DTO `disc_number`); the library drill-down (`app.js`) detects a multi-disc album (>1 distinct disc, untagged = disc 1) and renders "Disc N" subheadings with per-disc numbering — single-disc albums are unchanged. Files lacking disc tags can be fixed via the Extended-edit Disc-number field (row 127). Test: `TestListTracksByAlbumID_DiscOrdering`. Applied to: the library drill-down (`app.js`), the admin "By entity" track view (`admin/files.js` `renderTracks`), and the file-list grouped-by-album table (`file-list.js` `groupedTable` — admin Files, moderation queue, My-uploads, via `disc_number` now returned by `/api/files`, `/api/admin/moderation`, `/api/my/uploads`). The flat/uploader-grouped views show no per-track number, and the cmus list is not grouped. | **fixed** |

## Tester round — dynamic API probing (2026-06-15)

A fresh black-box pass against a running server (82-file dev DB, admin session).
Backend held up well: parameterised SQL (SQLi probe on `/api/search` returns
empty), bad/overflow ids → clean 400, malformed `Range` → 416, 10 KB `?q=` → 400,
default-deny verified (anonymous `/api/{artists,albums,tracks,files,search}`
return 200 with empty payloads; `/files/*` blobs 404 for anon). Public cover
endpoints (`/api/albums/{id}/image`, `/images/*`) are anonymously readable **by
design** (documented in `docs/api/cover-images.md` — the upload UI needs them
pre-login), so not flagged. New findings:

| Severity | Issue | Status |
|---|---|---|
| Low | **`HEAD` returns `405 Method Not Allowed` on every `GET`-registered route.** Confirmed on `/healthz`, `/source`, `/`, `/files/*`, `/images/*`, `/api/artists`, `/api/albums`, `/api/files` — all 405 with `Allow: GET`. Cause: chi's `r.Get(...)` registers the GET method only and the mux answers a matched-path/unmatched-method with 405; there is no automatic HEAD. The blob and cover servers (`fileServer`, `/images/*`) wrap `http.FileServer`, which **natively** serves HEAD (headers, no body) — but that support is unreachable because the route is GET-only. `/static/*` is the lone exception: it uses `r.Handle("/static/*", …)` (all methods), so HEAD → 200 there. Impact: the bundled web player is unaffected (it streams via GET + `Range`, verified 206), but HEAD-only clients break — uptime/health-check monitors that probe `/healthz` with HEAD, download managers, byte-range/`Accept-Ranges` probers, and link/preview crawlers all get 405. **Fixed:** the global option — `api.SupportHEAD`, a top-level middleware wired in `buildHandler` (innermost `r.Use`, after Logger/Identify/allow_from) that, on `r.Method == HEAD`, rewrites a clone to `GET` and wraps the `ResponseWriter` to discard the body. Because the rewrite happens *before* routing, every GET route (health, source, `/api/*`, `/files/*`, `/images/*`) now answers HEAD with headers-only, and the request takes the identical auth + `fileAccessGuard` path — so a HEAD to a denied blob still 404s (verified). `HEAD` also added to the advertised `corsMethods`. Tests: `TestSupportHEAD_*` (healthz empty body, API routes, missing-file 404, **guard runs on HEAD**, headers forwarded). | **fixed** |
| Info | **`disc_number = 0` is treated inconsistently between ordering (0) and grouping/labeling (1), so a multi-disc album with a disc tagged `0` loses its "Disc N" separators and reprints duplicate track numbers** — the exact symptom row 152 fixed. The metadata editor accepts `"0"` (`database/metadata.go` `metaNullInt` allows any non-negative int; verified `PATCH disc_number:"0"` → 200), and real-world tags occasionally carry disc 0. Then: server `listTracksByAlbumID` orders by `COALESCE(disc_number,1)` (NULL→1 but **0 stays 0**, so disc-0 tracks sort first as a distinct group), while every client folds `disc_number || 1` (0→1) for both multi-disc **detection** and the "Disc N" header — so a `{disc 0, disc 1}` album collapses to one disc client-side (`new Set([1,1]).size === 1` → not multi-disc), drops its separators, and shows interleaved/duplicate numbers. Affects `app.js` (library drill-down), `admin/files.js` `renderTracks` (entity view), and `file-list.js` `groupedTable` (admin Files / moderation / My-uploads). `file-list.js` additionally mixes `?? 1` in its album sort (line ~376) with `|| 1` in detection (~448), so even its own ordering disagrees on 0. Narrow (0-based disc tags are unusual), but reachable. **Fixed** (step 1 of `docs/architecture/disc-numbering.md`): chose the *opposite* of folding — `NULL` (untagged) / `0` / `N` are now three **distinct** discs, never collapsed. A shared `webui/static/js/disc.js` (`discKey`/`discSort`/`discLabel`/`isMultiDisc`) replaces the drifted `\|\| 1` / `?? 1` logic across all three renderers (`app.js`, `admin/files.js`, `file-list.js`), and `listTracksByAlbumID` now orders `(disc_number IS NULL) ASC, disc_number ASC, …` (0 before 1, untagged last) to match. A single distinct disc still shows no separator. Test: `TestListTracksByAlbumID_DiscZeroAndUntagged`. Deferred (see the doc): `disc_subtitle` for named discs, and preserving a *file-tagged* `0` through ingest. | **fixed** |

## Search — should the Tracks section include tracks matched only by artist/performer name? (design question, 2026-06-15)

**Open decision — no code change yet.** Today the search Tracks section matches a
track on its **title OR its performer name** (`database/library.go` `search()`;
documented in `docs/api/search.md` §"Search behaviour" → Match fields). So
searching an artist name surfaces that artist's tracks as directly-playable rows
in the Tracks section, *in addition* to the artist appearing in the Artists
section. This was intentional — it surfaces a performer's tracks even on a
"Various Artists" compilation where the artist isn't the album-artist (see
`TestSearch_MatchesPerformerOnCompilation` and `docs/architecture/artist-album-model.md`).

The owner is now reconsidering whether that's the right UX: searching a prolific
artist can flood the Tracks section with rows that largely duplicate what the
Artists-section drill-down already gives you.

| Option | Trade-off |
|---|---|
| **Keep as-is** (title OR performer) | One click to play any of an artist's tracks straight from search; essential for finding a performer's contributions on compilations they don't headline. Cost: noisy Tracks section for common artist queries. |
| **Restrict Tracks to title matches** | Cleaner Tracks section (only tracks whose *title* matches); artist-credited tracks are reached via the Artists row → drill-down. Cost: loses the direct-play affordance and makes compilation performers harder to surface. |
| **Middle ground** | e.g. cap/secondary-sort artist-only matches below title matches, or only fold in performer matches when the Tracks-by-title set is small. Needs design. |

Related: the sibling experiment "surface an album's tracks when searching its
title" was tried and **reverted off `aidev`** for the same too-noisy reason —
it lives on branch `show_albums_tracks_in_search` (commit `cea8521`). Whatever we
decide here should be consistent with that call. If the decision changes the
documented behaviour, update `docs/api/search.md` §"Search behaviour". | open |

## Storage-by-category panel — scope review (2026-06-18)

Reflective "did we forget anything?" pass over the v0.4.5 storage-usage-by-category
feature (`GET /api/admin/storage`, `adminStorageStats`/`storageStats`,
`StorageByteBreakdown`, `storage.DirSize`; design `docs/architecture/storage.md`).
The four review findings from the developer+tester pass were already fixed
(commits `2655773`/`6232d19`/`c609f7c`/`0ce11fe`, folded into the moved `v0.4.5`
tag). The items below are blind spots found afterwards — **none fixed yet**,
logged for a later session.

| Severity | Issue | Status |
|---|---|---|
| Low | **Fresh install 500s the whole storage panel.** `storage.Local.Stats()` (`api/storage/local.go:183`) `statfs`es `baseDir` = `files_dir/audio`, which is **not created until the first upload** (nothing makes it at startup). On a brand-new instance `statfs` → ENOENT → `Stats()` errors → `storageStats` returns it → `adminStorageStats` logs + 500s → the dashboard storage card silently stays hidden until the first audio file is uploaded. **Pre-existing** (the old audio-only endpoint also called `Stats()` first), squarely in this scope, and untested. Fix: `statfs` `files_dir` (or the nearest existing ancestor) instead of the not-yet-created `audio/` subdir, or `MkdirAll` the subtrees at startup; add a fresh-install test. | open |
| Low | **Image sizing doesn't scale — the exact concern that motivated the hybrid design.** Audio/review/trash moved to an indexed DB `SUM(byte_size)` precisely to avoid walking a big tree, but images are **still an uncached full `DirSize` walk on every dashboard load** (8 variants per cover × every album → ~400k `stat()` calls on a 50k-album library). The doc's "image set is small (few files)" rationale doesn't hold at scale. Honest fix (deferred at design time): track image bytes in the DB — a `byte_size` on cover variants or a running total in `settings` — so images become an indexed sum too. The deferral *is* the unsolved half of the original big-storage question. | open |
| Info | **"audio" and "images" measure different *kinds* of bytes.** Audio = logical DB sum (one blob per hash — dedup never double-stores, confirmed — but **excludes** orphan audio blobs with no DB row). Images = physical disk walk (which **includes** orphan/stale image dirs). So orphan audio falls into the "other disk usage" segment while orphan images land in "images". They sit side-by-side as if comparable but aren't quite; orphans are really the Verify & Prune view's job (`docs/architecture/prune-job.md`). Acceptable if we know it. | open |
| Info | **Madshare's own DB isn't counted.** `madshare.db` + WAL/SHM is real app footprint but belongs to no category, so it folds into "other" and "Madshare total" understates the true footprint. Small for a media server, but worth a deliberate note (or a "database" category). | open |
| Info | **Panel fetches once per page load — no live refresh.** We optimised *server* freshness (dropped the cache) but the *client* fetches once on dashboard load: figures don't move while a prune/upload runs until a manual reload. If "watch it update" matters, add a poll or a refresh button. | open |
| Info | **Storage view gated behind a destructive permission.** `GET /api/admin/storage` requires `file.delete`; a moderate-only admin can't see it (reuses the storage-management route group). Probably intended — just be deliberate about whether a read-only stats view should need a delete permission. | open |
| Info | **Detail rows can look self-contradictory.** The *bar* is clamped, but the rows still show raw "Madshare total" (logical bytes) vs "Disk used" (FS-allocated); on a compressing/sparse FS the total can read *larger* than disk-used, which looks wrong to a human even though it's correct. Consider a tooltip/footnote. | open |

## Search — diacritic / ß normalization (2026-06-27)

Case folding for non-ASCII letters is now handled: search registers a
Unicode-aware `unicode_lower` SQL function (Go `strings.ToLower`) used on both
sides of the search `LIKE` predicates in `DB.Search` (`database/library.go`) and
the unified file filter (`database/files.go`), so e.g. `über` matches `Über`
(commit `fdd95b6`, regression `TestSearch_CaseInsensitive_Unicode`).

What is **not** handled is orthographic normalization — `ß`↔`ss` and accent
folding (`é`↔`e`): `strasse` does not match `Straße`, `cafe` does not match
`Café`. Logged as a deliberate non-goal for now.

| Approach | Why we are NOT doing it | Status |
|---|---|---|
| **Cheap (per-row transform)** — extend `unicode_lower` into a `search_norm` that NFD-decomposes, strips combining marks and maps `ß`→`ss`, applied per row on both sides of the `LIKE`. | **Rejected.** Runs a non-trivial transform on every scanned row of every search (tolerable for the small library search, wasteful on the large `media_metadata` file filter) for marginal recall, and the semantics turn murky — accent folding over-matches (collapses genuinely distinct names) and `ß`↔`ss` is asymmetric/locale-dependent. Not worth predictable behavior. | **won't do (cheap way)** |
| **Proper (precomputed indexed column)** — a normalized search column (generated/indexed) populated at ingest + backfill, so normalized matching is an indexed lookup rather than a per-row scan. | Correct and fast at query time, but it is real schema churn (migration + ingest hook + backfill) and adds index write/storage cost on every upload/edit. **Performance is a priority for us**, so even this may not pay for itself — the win (matching `ß`/accents) is a rare edge case since users usually type the umlaut/ß form they see. | **deferred — may never do** |

Decision: keep the current Unicode case folding; do **not** add the cheap per-row
normalization. Revisit the precomputed approach only if diacritic/`ß` search
mismatches are reported as a real problem in practice. | open |

## Bulk write paths / SQLITE_BUSY (2026-06-28)

Reported: Trash *Restore* was far slower than *Move to Trash* and produced
`SQLITE_BUSY` in the log, occasionally rendering the admin page logged-out (a
reload recovered it). Root cause: `trashBulk`'s restore action looped per hash,
opening **two** autocommit write transactions per file (the `RestoreFileByHash`
UPDATE + a `file.restore` audit INSERT) — `2N` write transactions — while *Move
to Trash* uses one batched `BulkSoftDeleteByHashes` transaction + one audit row.
Under `_txlock=immediate` on the multi-connection on-disk pool, that storm of
short write-lock acquisitions contends with the analysis/prune/concurrent-request
writers; once a writer waits past the 5 s `busy_timeout` it fails with
`SQLITE_BUSY`. **Fixed** for restore: new `database.BulkRestoreByHashes` (one
chunked transaction, mirrors `BulkSoftDeleteByHashes`) + a single
`file.bulk_restore` audit row.

The same `2N`-write-transaction anti-pattern was present in the other bulk
handlers below. All four bulk endpoints accept the "Select all N matching" scope,
so each can be invoked over an arbitrarily large set and reproduce the same
slowness / `SQLITE_BUSY` / transient-logout cascade. The audit INSERT per row is
half the write traffic and is the easiest part to collapse (one summary row, as
the trash and edit paths already do).

**All of this is now fixed** (2026-06-28). New batched DB methods:
`BulkUpdateReviewState` (guarded transition, backs bulk approve/return and the
submit buckets), `BulkDiscardOwnUploads` (owner-scoped soft delete, backs
My-uploads remove), `BulkUpdateFileMetadata` (one tx per 500-file chunk via the
extracted `applyMetadataPatchTx`), and `BulkSetLicense` / `BulkSetGuestPlayable`
(single-valued guarded `UPDATE`). Each bulk action now runs under O(1)
transactions + one summary audit row instead of `2N`. The independent
session-drop fragility (`auth/middleware.go`) is fixed separately by failing
closed with 503. Verified by `go test -race` + two live on-disk smokes (no
`SQLITE_BUSY`, one audit row per call). See the per-row statuses below.

| Severity | Issue | Status |
|---|---|---|
| Medium | **Session silently drops on a transient session-lookup DB error.** `resolve()` now returns `(*Identity, error)`: a *presented* credential whose store lookup errors fails closed with **503** (`Identify`), instead of downgrading to anonymous (which rendered an authenticated user logged-out). A missing/unknown/expired credential is still anonymous. Covered by `auth.TestIdentify_StoreErrorFailsClosed`. | **fixed (2026-06-28)** |
| Medium | **`moderationBulk` loops per row (approve / return / discard).** approve/return now go through `BulkUpdateReviewState` (one guarded `UPDATE … WHERE hash IN (…)` per chunk, like restore); discard reuses `BulkSoftDeleteByHashes`; one summary audit row each. | **fixed (2026-06-28)** |
| Medium | **`myUploadsBulk` remove loops per row.** Now one `BulkDiscardOwnUploads` chunked transaction (owner + draft/returned guard) + one summary audit row. | **fixed (2026-06-28)** |
| Low | **`myUploadsBulk` submit loops per row (`submitStaged`).** `submitStaged` now keeps the per-hash duplicate *read* (no write lock) but partitions into self-approve / submitted / duplicate-flagged buckets and issues one `BulkUpdateReviewState` + one summary audit row per bucket (`file.bulk_approve` / `file.bulk_submit`). Exact submitted/flagged counts preserved; the unused per-hash `results` field was dropped. | **fixed (2026-06-28)** |
| Low | **`bulkEditFiles` loops per row (`applyOneBulkEdit`).** Tags now go through `BulkUpdateFileMetadata` (one transaction per 500-file chunk; `applyMetadataPatchTx` shares the tx — the per-file entity re-resolution still can't be one `UPDATE`, but the transactions are batched). The single-valued license/guest collapse to one guarded `UPDATE … hash IN (…)` each (`BulkSetLicense` / `BulkSetGuestPlayable`). | **fixed (2026-06-28)** |

## Recording-tagsets P2 — interim states / follow-ups (2026-07-04)

Non-blocking observations from building P2 (lifecycle & GC). None are defects;
all are anticipated by the design and resolve in later phases.

| Severity | Issue | Status |
|---|---|---|
| Info | **Orphan renditions after a non-last appearance permanent-delete.** Deleting a non-last tagset from Trash keeps the file (blob) — correct per the hardlink model — but that file now has no tagset of its own (the recording still has others). Valid interim state; such orphan renditions are only cleanable via the P3 duplicates/absorb surface (rendition removal). No data at risk. | **resolved by P5** (2026-07-07): `/admin/recordings` lists every rendition of a recording — incl. tagset-less and soft-removed blobs — with per-row Remove/Restore, so orphans are visible and cleanable there. |
| Info | **Audit action name imprecise for appearance-only delete.** The hash-addressed Trash permanent-delete audits `file.delete` even when only the appearance was dropped (the file survived). Acceptable while Trash is hash-addressed; the P5 recordings view should introduce explicit `recording.delete` / appearance-delete audit actions. | **mostly fixed by P5** (2026-07-07): the recordings view audits `recording.delete` / `recording.trash` / `recording.merge` / `tagset.move` / `rendition.remove|restore`. The legacy hash-addressed Trash path still writes `file.delete` for an appearance-only drop — acceptable, low. |

## Recording-tagsets P5 — follow-ups (2026-07-07)

Non-blocking notes from building P5 (`/admin/recordings` + the All-files
physical view). Neither blocks the v0.5.0 feature; both are polish candidates.

| Severity | Issue | Status |
|---|---|---|
| Low | **Trashed appearance of an absorbed blob had no restore *or* permanent-delete path of its own.** The recordings view's per-appearance "Remove" reuses `POST /api/admin/moderation/{tagsetID}/discard` (= tagset soft-delete) — correct trash semantics, but both restore and permanent delete were only reachable through the hash-addressed Trash page, which lists a trashed tagset via its origin *file's* hash. An appearance whose origin blob was absorbed (soft-removed) or purged therefore could neither be restored nor hard-deleted from Trash. | **fixed (2026-07-08)**: tagset-addressed `POST /api/admin/tagsets/{id}/restore` (`RestoreTagset`) and `DELETE /api/admin/tagsets/{id}` (`HardDeleteTrashedTagset` — routes through the shared `hardDeleteTagsetsTx` cascade, reclaims blobs, 409 on a live appearance), surfaced as **Restore / Delete permanently** on the recording card's trashed appearance rows (`file.delete`-gated). Covered by `TestRestoreAndHardDeleteTagset` (DB) + `TestTagsetRestore`/`TestTagsetHardDelete` (api). — **regressed then re-fixed**: `b3200c8` removed `hardDeleteAppearance` from the recording card, leaving `DELETE /api/admin/tagsets/{id}` with no caller. **P7c (2026-07-10) re-wires it** as the Trash · Appearances lens's Delete forever, now that the lens is tagset-rooted and lists every trashed appearance. |
| Info | **`MoveTagset` deliberately has no "move to new recording".** An appearance without a blob can't play; the "detach into a new recording" shape is Split off (rendition-level, takes the blob along). Design decision per the P5 mock review — recorded here so it isn't re-reported as a gap. If a real need appears ("appearance-only new recording"), it would need a rendition choice anyway → revisit as a variant of Split off. | by design |
| Low | **Soft-removed *files* don't appear on the Trash page.** The Trash listing is tagset-addressed — its base predicate is `m.deleted_at IS NOT NULL` (the *tagset* carries the Trash mark), so it lists trashed **appearances** only. A soft-**removed file** (`files.deleted_at` — a removed rendition, an absorbed/dormant blob) whose tagset is still live never shows up there. Such blobs are currently surfaced only in the Admin·Library "All files" table behind the moderation-gated **"Show removed"** toggle (P5), with per-rendition Remove/Restore on `/admin/recordings`. So there *is* a place to see/manage them — just not Trash. The two-perspective split (Trash = appearance view, All files = physical/file view) is by design; open question is whether Trash should also surface the file-removal mark (a "Removed files" scope, or removed renditions listed alongside trashed appearances) so one page covers both soft-delete marks. | **fixed (2026-07-08)**: Trash now has **three perspectives** (`docs/architecture/soft-delete.md`) — **Appearances** (existing `file-list.js` scope), **Recordings** (trashed-recording bin: whole-recording restore/delete), **Files** (soft-removed blobs: the missing per-file permanent delete; last file → cascade-prune the recording). All permanent deletion consolidated onto the Trash page; `/admin/recordings` lost both hard-delete buttons. Backend `database/trash_files.go` + `trash_recordings.go` + the `"trashed"` recording filter; API `trash_perspectives_handlers.go`; UI `admin/trash-{list,recordings,files}.js` + the sub-switch. DB + handler tests + live smoke green. **Deferred:** broadening the Appearances lens to *dormant* appearances (kept scoped to its own mark — dormant is covered by Recordings + Files; would need a tagset-addressed, dormant-aware restore). — **corrected then fixed (2026-07-10)**: the claim held only for an appearance that was its file's representative, because the lens was rooted `FROM files`. **P7c re-roots it `FROM tagsets`** — one row per appearance, so the consolidation claim is now actually true. |

## Recording-tagsets P7 — `origin_file_id` is provenance, not structure (2026-07-10)

Found while scoping an "Add appearance" button for `/admin/recordings`. Owner's
framing: *under the tagset model, files belong to **recordings**, not to
appearances.* The schema agrees — `files.recording_id` (mig `020`, `NOT NULL`
since `024`), while `tagsets.origin_file_id` is documented in `024_tagsets.sql`
lines 119–121 as *"Provenance: the file this appearance's tags were read from.
Kept for audit / federation attribution."* Several queries nonetheless treat
that audit column as the structural link, via `reprTagset` (`files.go:32`) and
the INNER `tagsetJoin` (`files.go:44`).

**Orphan renditions themselves are by design** (see the P2 entry above — a blob
with no tagset of its own is a valid interim state, and `/admin/recordings`
lists them). The defect is that the files-rooted surfaces silently *drop* them.

Reproduced on `HEAD` with a throwaway test (`MergeRecordings` on two recordings
sharing an appearance key: the source's appearance is dropped as a duplicate,
its file moves to the target as a live rendition with zero tagsets pointing at
it):

```
merge outcome: {Found:true SourcesMerged:1 RenditionsMoved:1 AppearancesMoved:0 AppearancesDropped:1}
f2 is a live rendition of the target: true
tagsets with origin_file_id = f2: 0
ListFilesPage: 1 row(s); contains f2: false
CountFiles: 1
FilesNeedingAnalysis: [1]; contains f2: false
SetGuestPlayable found: via f1.hash=true, via f2.hash=false
FileAccessibleByHash: f1=true, f2=true       <- control: serving is recording-rooted
```

The control is the point: the orphaned blob **streams fine** (access is
recording-rooted and correct) while being absent from the file-management
surfaces. `AbsorbRenditions` produces the same shape.

| Severity | Issue | Status |
|---|---|---|
| **Medium** | **Files-rooted surfaces drop tagset-less renditions.** `fileListSelect` (`files.go:345`), `CountFiles`/select-all-hashes (`files.go:585`,`:598`) and `trashListSelect` (`files.go:1149`) all read `FROM files f` + INNER `tagsetJoin`. A rendition with no tagset of its own is invisible in Admin·Library "All files" (even behind *Show removed*), uncounted, and unselectable in bulk. Visible only per-recording on `/admin/recordings`. | **fixed (2026-07-10, P7b)** — `reprTagset` now searches the file's *recording*, so every file is covered and the INNER join stays valid. **Deviation from the plan:** not a `LEFT JOIN`. A flat recording-rooted lookup was tried first and leaked a *pending-review* rendition into the live listing (it borrowed the recording's approved primary) and misfiled its bytes from Review into Library. The shipped rule gives the blob's **own** offered appearance precedence, falling back to the recording's only when it has none — per-blob lifecycle preserved, orphans covered, no `m.* IS NULL` branches needed. Pinned by `TestReprTagset_OwnAppearanceWinsOverRecording` + `TestMergeRecordings_OrphanedRenditionStaysManageable`. |
| **Medium** | **Media analysis skips tagset-less renditions.** `FilesNeedingAnalysis` (`analysis.go:178`) gates on `EXISTS (tagsets WHERE origin_file_id = f.id AND deleted_at IS NULL)`. An orphaned rendition never gets a fingerprint or `codec`, so `RankRenditions` silently degrades to the format/size fallback for that blob — the quality ladder mis-ranks. | **fixed (2026-07-10, P7b)** — gates on `t.recording_id = f.recording_id`, mirroring `FileAccessibleByHash` (`access.go:49`). |
| **Medium** | **Trash Appearances lens is one row per *file*, not per appearance.** `trashListSelect` joins the file's *representative* tagset (`reprTagset` = primary, else oldest). A trashed **non-representative** appearance is listed nowhere — reachable today by trashing a byte-dup draft (`AttachDraftTagset` sets `is_primary = 0`) while the original stays live. Worse, the row actions are hash-addressed: restore updates **all** trashed tagsets of that file (`files.go:1121`) and delete collects **all** of them (`files.go:1060`), while the UI showed a single row. | **fixed (2026-07-10, P7c)** — the lens is rooted `FROM tagsets m LEFT JOIN files f` (`database/trash_tagsets.go`), one row per appearance keyed by tagset id; actions are `POST /api/admin/tagsets/{id}/restore` + `DELETE /api/admin/tagsets/{id}` + `PATCH …/metadata`, bulk carries `tagset_ids`. The hash-addressed pair is gone. Pinned by `TestTrashLens_*` (DB) + `TestTrashBulk_*` (api); browser-verified (two appearances of one blob = two rows, tagset-scoped selection/delete, blobless appearance handled). |
| Low | **Hash-addressed access setters no-op on tagset-less renditions.** `liveFileRecordingSubquery` (`access.go:69`, `:164`) defines "live file" as *has a live tagset via `origin_file_id`*. `SetGuestPlayable`/license by such a blob's hash returns `found = false` silently. Mitigated: the recording-level chip on `/admin/recordings` still works, and any sibling rendition's hash flips the same recording. | **fixed (2026-07-10, P7b)** — `liveFileRecordingSubquery` gates on recording membership. Live-verified: `POST /api/admin/files/{orphan_hash}/guest` → 200 (was a no-op). |
| Low | **Purged-origin appearances drop out of the review queue.** `reviewFrom` (`review.go:127`) is `JOIN files f ON f.id = m.origin_file_id` — INNER. `origin_file_id` is `ON DELETE SET NULL`, so an appearance whose origin blob was hard-purged disappears from both the moderation queue and My uploads while still `draft`/`submitted` — unapprovable, unreachable. Also blocks any future blobless appearance (e.g. a hand-added one). | **resolved as a non-issue (2026-07-10, P7d)** — kept an INNER JOIN deliberately. A blobless *draft* cannot exist: every file-delete path drops or repoints referencing tagsets before the row dies, so `origin_file_id` never reaches NULL on a live draft. The hand-added appearance (P7d) is born **approved**, not draft, so it never enters the review queue. Making the join LEFT would add NULL-handling for a state the invariants forbid. |
| Info | **File ops mutate appearance sets by provenance.** `files.go:647`/`:693`/`:812` trash / restore / hard-delete tagsets `WHERE origin_file_id IN (…)`. Contrast `hardDeleteTagsetsTx` (`files.go:864`), whose doc comment already states the rule — it deletes by recording membership, *"never `origin_file_id`"*. The rule was written down and then not followed elsewhere. | **fixed (2026-07-10, P7c)** — the Trash Appearances lens no longer routes through `trashListSelect`/`originTagsetJoin`; it is a fresh tagset-rooted query (`database/trash_tagsets.go`) and its restore/delete/edit are tagset-addressed. `files.go:647`/`:693`/`:812` remain (used by the hash-addressed soft-delete + the uploader restore); their `origin_file_id` scoping is correct for *those* file-addressed callers. |
| Info | **"Add appearance" (P7d) — the original ask, now shipped.** `CreateAppearance` (`database/curate.go`) adds a hand-authored, blobless, approved, non-primary appearance to an existing recording; `POST /api/admin/recordings/{id}/appearances` (`content.moderate`); refusals 404/400/422(nameless)/409(collides). UI: "+ Add appearance" on the card opens `track-edit.js` in a new **create mode** (POST, blank extended fields, inline refusal). Tests: `TestCreateAppearance*` (DB) + `TestRecordingsAddAppearance*` (api); browser + curl verified (blobless appearance browsable as its own album, plays the recording's rendition; duplicate refused inline). | **done** |
### P7 follow-up — the startup sweep resurrects deduped appearances (2026-07-10)

Found while checking whether orphan renditions persist. They don't — and the
way they're "healed" is the real bug.

| Severity | Issue | Status |
|---|---|---|
| **High** | **`ReconcileTagsets` manufactures a nameless, approved, library-visible appearance on every restart.** Step 2 (`recordings.go:148`) selects *files* with no tagset via `origin_file_id` and inserts a filename-derived tagset with `review_state = 'approved'`, `is_primary = 0` — **with no `f.deleted_at` filter**. Merge and absorb both deliberately drop a redundant appearance while keeping the blob (that is what appearance dedup *is*), so the next server start re-creates it. The dedup is undone, and the replacement violates the design's **"Don't manufacture nameless appearances"** rule: no artist, no album → Unknown artist / Other. It fires for **soft-removed** blobs too, resurrecting a live appearance for a blob that is not even served. | **fixed (2026-07-10, P7b)** — heals at **recording** grain (a recording with zero tagsets); a file needs no tagset of its own. Pinned by `TestReconcileTagsets_DoesNotManufactureAppearance` (merge + absorb shapes). Live-verified: merge, restart, no `reconcile tagsets:` log line, recording still has exactly 1 appearance. |

Reproduced on `HEAD` (throwaway test, both shapes):

```
-- merge shape --
after merge:   tagsets on target = 1
reconcile tagsets: file 2 had no tagset; created one
after restart: tagsets on target = 2
  #1 title="studio.flac" artist="The Band" album="Studio Album" approved primary=1
  #2 title="reissue"     artist=<null>     album=<null>          approved primary=0
library-visible appearances: 2 (was 1 right after merge)

-- absorb shape (blob soft-removed) --
f2 soft-removed = true
reconcile tagsets: file 2 had no tagset; created one
live tagsets created for the SOFT-REMOVED blob f2: 1
library-visible appearances on the recording: 2
```

**Why the "just re-point `origin_file_id`" fix cannot work.** After a merge the
target holds 2 files and 1 appearance. A tagset points at exactly one file, so
by pigeonhole no re-pointing gives both files a tagset — it only moves the hole.
The `HardDeleteRemovedFile` precedent (`trash_files.go:190`) re-points because
there the *file* dies and its appearances would dangle; here the *appearance*
dies and the file survives. Orphan renditions are therefore **unavoidable** as
long as dedup drops appearances — which is the whole point of dedup. The fix is
to stop asking "which tagset points at this file" and ask "what recording is
this file a rendition of".

## Recording-tagsets feature review (2026-07-14)

Full-feature review of the tagset/recording/file model (P0–P7). Every finding
reproduced with a throwaway DB test on a clean tree (`go test ./...` green
before the pass). None fixed yet — logged for owner decisions.

| Severity | Issue | Status |
|---|---|---|
| **High** | **Prune blob-loss destroys preserved appearances / strands zero-tagset recordings.** `hardDeleteFilesTx` (`database/files.go:811`, reached via prune → `HardDeleteFileByHash`, `database/prune.go:239`) deletes tagsets by `origin_file_id` instead of re-pointing them to a surviving rendition the way `HardDeleteRemovedFile` does (`database/trash_files.go:197`). Reproduced: (a) an absorbed blob goes corrupt → prune removes it → the absorbed-and-preserved appearance (the whole point of absorb) is destroyed although the kept rendition still plays — the design says blob-loss with survivors = "the recording just lost a rendition"; (b) when the surviving rendition is an orphan (its appearance was deduped), pruning the origin blob of the recording's only appearance leaves the recording with a live file and ZERO tagsets — the invariant violation the shared cascade exists to prevent; the blob then vanishes from every surface until the next restart, when `ReconcileTagsets` step 2 manufactures a nameless filename appearance (the P7-outlawed shape, now reachable at recording grain). Additionally the deleted tagsets may belong to *other* recordings (a `MoveTagset`-moved appearance whose origin file stayed behind), which are never repaired (`recIDs` is collected from the files' recordings only). | **fixed (2026-07-14)** — `hardDeleteFilesTx` now re-points affected tagsets to a surviving rendition of *their own* recording (live-first, the `HardDeleteRemovedFile` ordering) instead of deleting by `origin_file_id`; no survivor → origin `NULL` (blobless is legal since P7d, and an emptied recording is repaired away). `recIDs` additionally collects the recordings of moved appearances (UNION over `tagsets.recording_id`), so cross-recording holders get repaired too. Pinned by `TestHardDeleteFile_{RepointsPreservedAppearance,OrphanSurvivorKeepsAppearance,MovedAppearanceSurvives}` (lifecycle_test.go). |
| **High** | **Recording GC cascades away blobless (hand-authored) appearances.** `SplitRendition` (`database/recordings.go:441`) and `ResolveRecording` (`database/recordings.go:38`) move only tagsets with `origin_file_id = the moved file`; a P7d `CreateAppearance` row (origin NULL) stays on the emptied recording and is cascade-deleted by `repairRecordingTx`. Reproduced via Split of a recording's only rendition. The resolver variant fires from the **background analysis worker** with no human in the loop — e.g. install fpcalc on an established library → startup backfill regroups singletons → hand-added appearances silently destroyed. Curated human input lost without confirm or trace. | open |
| **Medium** | **Appearance dedup treats trashed/pending appearances as kept keys.** `loadAppearances` (`database/absorb.go:187`, shared by absorb + `MergeRecordings`) ignores `deleted_at` and `review_state`. Reproduced: absorb drops a LIVE approved appearance as a "duplicate" of a TRASHED one with the same identity key → the recording becomes library-invisible (only the Trash copy remains). Also: an absorbed/merged file's *submitted* appearance (another uploader's pending review entry) can be hard-deleted silently — it leaves the queue without approve/return/deny. Inconsistent with `AttachDraftTagset`/`MoveTagset`, whose collision checks are live-only. | open |
| **Medium** | **Appearance dedup silently deletes playlist/favorites entries.** `playlist_items.tagset_id` is `ON DELETE CASCADE` (migration 025) and `deleteTagsetIDsTx` / merge's drop-set hard-delete the duplicate tagset without re-pointing references. Reproduced: a track in a user playlist disappears from it after an admin absorb, although an identical appearance survives on the recording. Fix shape: re-point `playlist_items` to the surviving tagset inside the dedup tx (the `HardDeleteRemovedFile` re-point precedent). | open |
| Low/Med | **`/admin/duplicates` cannot see the shapes it exists to fix.** `ListDuplicateRecordings` (`database/recordings.go:332`) still roots on the pre-P7 `t.origin_file_id = f.id` INNER join: a recording whose second live rendition is an orphan (exactly what merge/absorb produce) is not listed at all, and a single-blob recording carrying two live own tagsets (byte-dup draft) lists the same file twice as two "renditions". | open |
| Low | **`GetFileByHash` reads lifecycle off the oldest tagset.** `ORDER BY t.id LIMIT 1` (`database/files.go:124`): a blob whose oldest appearance is trashed but which carries a newer live approved one reads back as trashed, sending the upload dedup path down the restore/re-stage branch for a blob that is live in the library. Should mirror `reprTagset`'s live-first/primary-first precedence. | open |
| Low | **A pending appearance can still fall out of the review queue.** The P7d claim "`origin_file_id` never reaches NULL on a live draft" does not cover: `MoveTagset` (no review-state check) moves a draft/submitted appearance cross-recording; its origin recording is later hard-deleted; `deleteRecordingFilesTx` deliberately SET-NULLs the cross-recording appearance → `reviewFrom`'s INNER JOIN (`database/review.go:127`) drops it — unapprovable and invisible while still `submitted`. | open |
| Info | **Identity dedup is not enforced on resolver moves or `ApproveSubmission`** (collision is flagged, not blocked — per design). Consequence to expect: installing fpcalc on an established library mass-groups renditions and identical approved appearances pile up per recording; cleanup is manual via the duplicates page (see the lens gap above). Note also `UpdateTagsetMetadata` can create identity collisions unguarded. | open (by design, but expect the pile-up) |
