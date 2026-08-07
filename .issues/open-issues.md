# Open Issues — Madshare API (from tester review, 2026-05-27)

> **Verification pass, 2026-08-07** (at `91f99f2`). Every item still carrying an
> `open` status was re-checked against the tree rather than re-read. Outcome:
>
> - **Closed as fixed:** the yggstack inbound-reader SPOF (local patch 2 + the
>   `InboundHealthy` watchdog) and `GetFileByHash` reading lifecycle off the
>   oldest tagset (`0ea9b13`, one day after that finding was written).
> - **Closed as not a defect:** "fresh install 500s the storage panel" — measured,
>   it does not; `diskUsage` has walked up to the nearest existing ancestor on
>   ENOENT since the feature commit, so the finding was wrong when written.
> - **Confirmed still open, in code:** 15 items (listed per row below).
> - **Stale references repaired:** `docs/architecture/soft-delete.md` (deleted —
>   now `gc-model.md`), "until federation is implemented" (it is).
>
> Rule this pass followed: a finding is only closed when the *code* shows it
> closed. Nothing was closed for being old.
>
> **A follow-up attempt to FIX three of the confirmed rows was rolled back the
> same day** — see "An attempt was built and rolled back" at the end of the
> madnetwork-playback section. The standing rule that came out of it: *do not fix
> an issue that has not been reproduced.* An entry in this file being confirmed
> present in the code is **not** the same as its failure having been observed.

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
| TODO | **Auto-clear trash** — add `[storage] trash_ttl_days = 0` (0 = disabled); background goroutine sweeps files where `deleted_at < now() − ttl_days×86400` and hard-deletes them. Design note in `docs/architecture/gc-model.md` (§ near line 482 — the old `soft-delete.md` was folded into it and deleted). **Re-verified 2026-08-07:** no `trash_ttl_days` key exists anywhere in the config or the code; the quarantine window is still purely manual. Note the sibling deferral in `docs/architecture/madnetwork-cache.md` — the cache **retention daemon** (age + size ceiling) is designed and unbuilt for the same reason; if either is picked up, they want one shape. | **deferred (verified still absent)** |
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
| **Low** | **Per-origin license trust** — the auto-derive policy applies to all files regardless of upload origin. The admin should be able to trust licenses set by their own server's uploaders but not by federated ones. Design sketch: add an `uploaded_by_origin` concept to `file_uploads` (local vs. federated node key); the auto-derive `accessClause` branch would add `AND f.origin = 'local'` (or a configurable per-federation trust flag). **Re-verified 2026-08-07 — federation is now shipped (F0–F8), so the framing "until federation is implemented" is dead, but the hazard is still LATENT rather than live:** the cached catalog *does* carry a peer's `license`/`guest_playable` (`database/madnetwork.go`), yet `POST /api/madnetwork/download` copies only the remote **tagset text** onto the local draft — it writes no `license` at all (grep: no `License` in `api/madnetwork_transfer_handlers.go`), and license/guest are recording-level fields the uploader bulk-edit path refuses outright (400). So no foreign license is ever written locally today and auto-derive cannot fire on one. **This item's real job now is to keep it that way:** the moment any import path copies a remote `license` onto a local recording, a peer starts deciding who may play our bytes anonymously. Check this row before building that. | open (latent — guard, not a live bug) |

## Future ideas (low priority, not yet planned)

| Priority | Idea | Notes |
|---|---|---|
| **Low** | **Notify admin about missing/unplayable tracks** — when a client-side `audio error` fires (track unavailable), surface a way to report it to the admin so they can run a prune/integrity check. E.g. a POST to a reporting endpoint that logs the offending file hash. **Deferred** — needs design: (a) auth/rate-limiting on the report endpoint to avoid abuse as a DoS/oracle, (b) decide if reports auto-trigger prune or just create a notification queue, (c) avoid leaking internal file paths to unauthenticated callers. **Re-verified 2026-08-07:** no reporting endpoint exists (`/api/admin/federation/reports` is the unrelated F6 claim-contradiction surface); `player.js:202` still handles `audio error` purely client-side. **Raised in value since it was filed** — the madnetwork playback investigation at the end of this file found two failure modes that are *silent by construction* (a stranded reader serving zeros, and a truncated response with a satisfied `Content-Length`); in both the client is the only party that knows something went wrong, and it currently tells nobody. Note the shape needed there is stronger than a hash: a report wants the decoder's stop offset, since that is what identifies the chunk boundary. | open |
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
documented behaviour, update `docs/api/search.md` §"Search behaviour".

**Re-verified 2026-08-07 — still open, and a sibling decision has since landed
that this must be reconciled with.** The library behaviour is unchanged
(`DB.Search` still matches title OR performer; `TestSearch_MatchesPerformerOnCompilation`
still pins it) and branch `show_albums_tracks_in_search` still exists unmerged.
But the madnetwork artist-credit work (2026-08-06, `docs/ui/artists-and-performers.md`)
answered the *same* question for the network page and answered it **"keep as-is,
and go further"**: `MadnetworkSearchArtists` deliberately drops the album-artist
restriction so a pure performer is a search hit and never a dead end. The two
pages are documented as siblings, so restricting library Tracks to title matches
would now put them in open disagreement — the option-2 column understates its
cost by exactly that. Decide the pair together, or not at all.

## Storage-by-category panel — scope review (2026-06-18)

Reflective "did we forget anything?" pass over the v0.4.5 storage-usage-by-category
feature (`GET /api/admin/storage`, `adminStorageStats`/`storageStats`,
`StorageByteBreakdown`, `storage.DirSize`; design `docs/architecture/storage.md`).
The four review findings from the developer+tester pass were already fixed
(commits `2655773`/`6232d19`/`c609f7c`/`0ce11fe`, folded into the moved `v0.4.5`
tag). The items below are blind spots found afterwards — **none fixed yet**,
logged for a later session.

**Re-verified 2026-08-07.** One row is withdrawn (it never reproduced), the
other six stand. The panel has grown a **fifth category** since this pass —
`cache` (the madnetwork download cache, a DB sum via `MadnetworkCacheBytes`) —
and the cover tree has moved out of `files_dir` into its own `variants_dir`
(`docs/architecture/variants.md`), so the paths below read `h.imagesDir` now.
Neither change touches the substance of any row.

| Severity | Issue | Status |
|---|---|---|
| Low | ~~**Fresh install 500s the whole storage panel.**~~ `storage.Local.Stats()` `statfs`es `baseDir` = `files_dir/audio`, which is not created until the first upload — so the reasoning was that ENOENT → `Stats()` errors → `adminStorageStats` 500s and the dashboard card stays hidden until the first upload. | **withdrawn — not a defect (measured 2026-08-07).** Reproduced directly against a `NewLocal` on a path whose parents do not exist: `Stats()` returns `HasVolume:true` with real figures and `err=<nil>`. `diskUsage` (`api/storage/diskusage_unix.go`) loops on ENOENT up to the nearest existing ancestor, with a comment saying exactly why ("the base directory may not exist yet on a fresh install… free space is a property of the mount"). That has been there since `5b224a3`, the **feature commit itself** — i.e. five days *before* this row was written. The finding was wrong when filed, not fixed since. Kept rather than deleted as the standing reminder that a code-read finding is a hypothesis until it is run. |
| Low | **Image sizing doesn't scale — the exact concern that motivated the hybrid design.** Audio/review/trash moved to an indexed DB `SUM(byte_size)` precisely to avoid walking a big tree, but images are **still an uncached full `DirSize` walk on every dashboard load** (8 variants per cover × every album → ~400k `stat()` calls on a 50k-album library). The doc's "image set is small (few files)" rationale doesn't hold at scale. Honest fix (deferred at design time): track image bytes in the DB — a `byte_size` on cover variants or a running total in `settings` — so images become an indexed sum too. The deferral *is* the unsolved half of the original big-storage question. | **open — confirmed 2026-08-07.** `storageStats` still calls `storage.DirSize(h.imagesDir)` inline on every request with no memo; it is now the **only** walked category (the new `cache` one is a DB sum, which is the shape this row is asking for). |
| Info | **"audio" and "images" measure different *kinds* of bytes.** Audio = logical DB sum (one blob per hash — dedup never double-stores, confirmed — but **excludes** orphan audio blobs with no DB row). Images = physical disk walk (which **includes** orphan/stale image dirs). So orphan audio falls into the "other disk usage" segment while orphan images land in "images". They sit side-by-side as if comparable but aren't quite; orphans are really the Verify & Prune view's job (`docs/architecture/prune-job.md`). Acceptable if we know it. | **open — confirmed 2026-08-07**, and now 4-against-1: audio, review, trash and cache are all DB sums, images alone is a disk walk. The odd one out is a single category, which makes converting it the clean resolution of this row *and* the one above. |
| Info | **Madshare's own DB isn't counted.** `madshare.db` + WAL/SHM is real app footprint but belongs to no category, so it folds into "other" and "Madshare total" understates the true footprint. Small for a media server, but worth a deliberate note (or a "database" category). | **open — confirmed 2026-08-07.** The category list is exactly `audio, review, trash, images, cache`; no `database` entry. |
| Info | **Panel fetches once per page load — no live refresh.** We optimised *server* freshness (dropped the cache) but the *client* fetches once on dashboard load: figures don't move while a prune/upload runs until a manual reload. If "watch it update" matters, add a poll or a refresh button. | **open — confirmed 2026-08-07.** `admin/dashboard.js` calls `fillStorage()` exactly once (line ~280) and the file contains no `setInterval`/`setTimeout`. Precedent now exists for the fix: `/admin/swarm` refreshes its peer panel on open and throttles to 8 s while open. |
| Info | **Storage view gated behind a destructive permission.** `GET /api/admin/storage` requires `file.delete`; a moderate-only admin can't see it (reuses the storage-management route group). Probably intended — just be deliberate about whether a read-only stats view should need a delete permission. | **open — confirmed 2026-08-07** (`api/api.go:480`, `r.With(fileDelete).Get("/storage", …)`). Still an unmade decision rather than a bug. |
| Info | **Detail rows can look self-contradictory.** The *bar* is clamped, but the rows still show raw "Madshare total" (logical bytes) vs "Disk used" (FS-allocated); on a compressing/sparse FS the total can read *larger* than disk-used, which looks wrong to a human even though it's correct. Consider a tooltip/footnote. | **open — confirmed 2026-08-07** (the `storageStats` doc comment still states the logical-vs-allocated split as a deliberate choice; nothing surfaces it to the reader). |

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
mismatches are reported as a real problem in practice.

**Re-verified 2026-08-07 — this is a settled decision, not an open issue,** and
the stray `| open |` at the end of it was a copy-paste artefact from the table
above. `unicode_lower` is still the only folding in `DB.Search`, and no
diacritic/`ß` mismatch has been reported since — the stated revisit trigger has
not fired. Status: **decided (won't do the cheap way; precomputed deferred
indefinitely)**.

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

**Re-verified 2026-08-07, row by row, against the code named in each.** The
first High was already marked fixed; one Low is now **fixed** (`GetFileByHash`,
by GC-model P3 `0ea9b13` on 2026-07-15 — the day *after* this review, which is
why it was never credited here). **The remaining five stand unchanged in the
tree**, each confirmed at the exact call site the row cites.

**Then RE-REPRODUCED 2026-08-07** — because a row confirmed present in the code
is not a row whose failure has been observed, and the month-old reproductions in
this section are not evidence about today's tree (one of the seven findings had
already been fixed the next day and nobody noticed for a month). Fresh throwaway
tests drive the **real entry points** — `AbsorbRenditions`, `SplitRendition`,
`ResolveRecording` — on a real in-memory DB and assert on the resulting rows.
Parked outside the repo at `<scratchpad>/wave2_repro_test.go`; each row below now
carries its own recipe, so they can be rebuilt from this file alone.

Result: **all three reproduce, but one is milder than this section claims.** The
two dedup rows destroy rows outright with no recovery path; the blobless-appearance
row turns out to be a *soft* loss — see its row for the correction and the
severity change. The earlier claim in this note that "two of them
(blobless-appearance loss, playlist-item loss) destroy curated user data
silently" was **half wrong**: only the playlist one does.

| Severity | Issue | Status |
|---|---|---|
| **High** | **Prune blob-loss destroys preserved appearances / strands zero-tagset recordings.** `hardDeleteFilesTx` (`database/files.go:811`, reached via prune → `HardDeleteFileByHash`, `database/prune.go:239`) deletes tagsets by `origin_file_id` instead of re-pointing them to a surviving rendition the way `HardDeleteRemovedFile` does (`database/trash_files.go:197`). Reproduced: (a) an absorbed blob goes corrupt → prune removes it → the absorbed-and-preserved appearance (the whole point of absorb) is destroyed although the kept rendition still plays — the design says blob-loss with survivors = "the recording just lost a rendition"; (b) when the surviving rendition is an orphan (its appearance was deduped), pruning the origin blob of the recording's only appearance leaves the recording with a live file and ZERO tagsets — the invariant violation the shared cascade exists to prevent; the blob then vanishes from every surface until the next restart, when `ReconcileTagsets` step 2 manufactures a nameless filename appearance (the P7-outlawed shape, now reachable at recording grain). Additionally the deleted tagsets may belong to *other* recordings (a `MoveTagset`-moved appearance whose origin file stayed behind), which are never repaired (`recIDs` is collected from the files' recordings only). | **fixed (2026-07-14)** — `hardDeleteFilesTx` now re-points affected tagsets to a surviving rendition of *their own* recording (live-first, the `HardDeleteRemovedFile` ordering) instead of deleting by `origin_file_id`; no survivor → origin `NULL` (blobless is legal since P7d, and an emptied recording is repaired away). `recIDs` additionally collects the recordings of moved appearances (UNION over `tagsets.recording_id`), so cross-recording holders get repaired too. Pinned by `TestHardDeleteFile_{RepointsPreservedAppearance,OrphanSurvivorKeepsAppearance,MovedAppearanceSurvives}` (lifecycle_test.go). |
| ~~**High**~~ → **Low/Med** (re-rated 2026-08-07 on measured evidence) | **Recording GC strands blobless (hand-authored) appearances.** `SplitRendition` and `ResolveRecording` move only tagsets with `origin_file_id = the moved file`; a P7d `CreateAppearance` row (origin NULL) stays behind on the emptied recording. The resolver variant fires from the **background analysis worker** with no human in the loop — install fpcalc on an established library → the startup backfill regroups singletons → hand-added appearances are affected unattended. ~~Curated human input lost without confirm or trace.~~ | **open — REPRODUCED on BOTH paths 2026-08-07, but the damage is SOFT and this row overstated it.** The mechanism holds: `recordings.go:81` (`ResolveRecording`) and `:343` (`SplitRendition`) both `UPDATE tagsets SET recording_id=? WHERE origin_file_id=?`, which an `origin_file_id IS NULL` row matches neither. **Recipes:** (a) one file + `CreateAppearance` on its recording, then `SplitRendition(f1)`; (b) two same-fingerprint files, `CreateAppearance` on **A's** recording — A is the one the resolver empties — then `ResolveRecording(a)`. **Observed (b):** `after resolve, OLD recording A: tagset 3 "Hand Added" origin_file=0 approved TRASHED`. **Three corrections to the original text:** (1) `repairRecordingTx` **does not exist** — the function is `reapRecordingsTx` (`database/purge.go:196`); (2) it **trashes** tagsets on a fileless recording (`UPDATE … SET deleted_at`) and only `DELETE`s the recording row when it has neither tagsets nor files, so the husk survives and nothing is cascade-deleted; (3) the appearance is therefore **not lost and not traceless** — verified in the same run: it is listed by `ListTrashedAppearancesPage` and `RestoreTagset` returns true and clears the mark. ~~Caveat on "recoverable": restoring returns it to a recording that now has no files, so it comes back dormant, not library-visible.~~ **CORRECTION, measured later the same day — "recoverable" was wrong, and this row was under-rated, not over-rated.** Restoring puts a *live* appearance back on a recording with **no file rows**, which is exactly reaper P2's target, so the next reap (startup, and after every delete/purge/move) trashes it again: `RestoreTagset = true; trashed now=0` → `reap: … trashed 1 appearance(s)` → `trashed=1`. **It bounces.** The row is stuck in Trash unless the moderator knows to `Move…` it onto a recording that still has a rendition *first*, then restore. So the undo does not work, and the earlier Low/Med re-rating (and my advice to deprioritise it) was wrong — measured on the single operation, not on the system. **FIXED 2026-08-07**, and the reaper was NOT touched: P2 is the documented GC model doing its job on a husk that split/resolve **manufacture**. The defect is the unfinished P7 rule — these two were the last places moving appearances by `origin_file_id` (provenance) instead of by recording (structure). The two paths get **different answers, by owner decision**: `ResolveRecording` **moves them along** (fingerprint proves same audio, the path is the unattended background worker, and identity collisions are allowed rather than deduped — never destroy a curated row on a path with no human); `SplitRendition` **refuses** with a new `SplitRenditionOutcome{Found, NewRecordingID, StrandedAppearances}` → 409 + a message, because a split *asserts* a different composition and carrying a curator's appearance across would file it under the very thing the moderator just declared separate — and there is a human present to ask. The refusal is narrow: it counts file **rows**, not live renditions, so a recording keeping a soft-removed sibling is dormant rather than a husk and splits cleanly. Tests: `TestResolveRecording_StrandedAppearancesFollowTheAudio` (asserts it survives a subsequent `Reap` — the bounce is the point), `TestSplitRendition_RefusesToStrandAppearances`, `_AllowedWhenARenditionRemains`, `_SoftRemovedSiblingIsNotAHusk`; the first two verified to fail before the fix. |
| **Medium** | **Appearance dedup treats trashed/pending appearances as kept keys.** `loadAppearances` (`database/absorb.go:187`, shared by absorb + `MergeRecordings`) ignores `deleted_at` and `review_state`. Reproduced: absorb drops a LIVE approved appearance as a "duplicate" of a TRASHED one with the same identity key → the recording becomes library-invisible (only the Trash copy remains). Also: an absorbed/merged file's *submitted* appearance (another uploader's pending review entry) can be hard-deleted silently — it leaves the queue without approve/return/deny. Inconsistent with `AttachDraftTagset`/`MoveTagset`, whose collision checks are live-only. | **open — code confirmed AND failure REPRODUCED 2026-08-07.** `loadAppearances` (`database/absorb.go:187`) still selects `WHERE t.recording_id = ?` with no `deleted_at` and no `review_state` predicate; beyond the identity key it resolves only the nameless/meaningful flag. **Recipe:** two files, same artist+album, `groupIntoRecording`, trash the *kept* rendition's appearance (`UPDATE tagsets SET deleted_at=…`), then `AbsorbRenditions(rec, keep=f1, absorb=[f2])`. Pass 1 seeds `keptKeys` from the non-absorbed appearance **including the trashed one**; pass 2 then drops the live one as its duplicate. **Observed:** `before absorb: 1 library-visible appearance(s)` → `after absorb: 0 library-visible appearance(s)`, with the only surviving row being the TRASHED one. **The live approved appearance is hard-deleted** (it goes through `deleteTagsetIDsTx`), so it is not recoverable; the recording can only be brought back by restoring the trashed appearance from Trash. **FIXED 2026-08-07** — `loadAppearances` now resolves a `live` flag (`deleted_at IS NULL AND review_state='approved'`) and dedup reasons about live appearances **only, in both directions**: a non-live row neither SEEDS a kept key (the bug above) nor IS DROPPED (the second half of this row — a submitted appearance was being hard-deleted out of the review queue with no approve/return/deny). That is the rule `AttachDraftTagset`/`MoveTagset` already used, which is why the disagreement was a bug rather than a policy. Deliberately only the tagset's own two marks, **not** `visibleTagset`'s third leg (a surviving rendition) — absorb is mid-flight removing renditions and would be reading its own uncommitted work. Applied to **both** consumers: absorb's two passes, and `MergeRecordings`, which had the identical bug. Merge needs one asymmetry: a non-live appearance still MOVES (its source recording is going away and a row left behind is reaped with it), it just claims no key — so the target may end up holding a live and a trashed appearance of one identity, the state `MoveTagset` already permits. Tests: `TestAbsorb_TrashedAppearanceIsNotAKeptKey`, `TestAbsorb_PendingAppearanceIsNotDropped`, `TestMergeRecordings_TrashedTargetAppearanceIsNotAKeptKey`, `TestMergeRecordings_NonLiveAppearancesMoveWithTheSource` — all four verified to fail before the fix. |
| **Medium** | **Appearance dedup silently deletes playlist/favorites entries.** `playlist_items.tagset_id` is `ON DELETE CASCADE` (migration 025) and `deleteTagsetIDsTx` / merge's drop-set hard-delete the duplicate tagset without re-pointing references. Reproduced: a track in a user playlist disappears from it after an admin absorb, although an identical appearance survives on the recording. Fix shape: re-point `playlist_items` to the surviving tagset inside the dedup tx (the `HardDeleteRemovedFile` re-point precedent). | **open — code confirmed AND failure REPRODUCED 2026-08-07.** `deleteTagsetIDsTx` (`database/absorb.go:224`) is still a bare chunked `DELETE FROM tagsets WHERE id IN (…)` with no re-point, reached from absorb (`absorb.go:148`) and `MergeRecordings` (`curate.go:449`). **Recipe:** two files, same artist+album (→ identical appearance key), `groupIntoRecording`, put the *absorbed* file's tagset in a playlist via `CreatePlaylist`, then `AbsorbRenditions(rec, keep=f1, absorb=[f2])`. **Observed:** `before absorb: playlist has 1 item(s), pointing at tagset 2` → `{Found:true RenditionsRemoved:1 AppearancesDropped:1}` → `after absorb: playlist has 0 item(s); recording still has 1 live appearance(s)`. **Hard delete, no trace, not recoverable** — and an identical appearance was available to re-point to. **Sharpest of the three:** it is the only one that hits an ordinary user's own data rather than an admin's curation. **FIXED 2026-08-07** — new `repointPlaylistItemsTx`, called from the top of `deleteTagsetIDsTx` so neither caller (absorb, merge) can skip it. Every entry pointing at a dying appearance moves to a surviving appearance of the **same recording** — sound because all appearances of a recording describe the same audio — preferring the same identity key (so the saved row still reads the same), then primary, then oldest; live approved only, and never one that is itself dying. Two no-re-point cases are deliberate: no survivor → the CASCADE stands (nothing to point at), and the playlist already holding the survivor → the entry is dropped rather than duplicated, mirroring `RepointRemotePlaylistItems`. Favorites need no separate handling — a favorites list IS a playlist row (mig 015). Tests: `TestAbsorb_DroppedAppearanceKeepsItsPlaylistEntries` (verified to fail before the fix) + `TestAbsorb_RepointDoesNotDuplicateAPlaylistEntry`. |
| Low/Med | **`/admin/duplicates` cannot see the shapes it exists to fix.** `ListDuplicateRecordings` (`database/recordings.go:332`) still roots on the pre-P7 `t.origin_file_id = f.id` INNER join: a recording whose second live rendition is an orphan (exactly what merge/absorb produce) is not listed at all, and a single-blob recording carrying two live own tagsets (byte-dup draft) lists the same file twice as two "renditions". | **open — confirmed 2026-08-07.** `ListDuplicateRecordings` (`database/recordings.go:203`) still roots on `JOIN tagsets t ON t.origin_file_id = f.id` in **both** the outer select and the `HAVING COUNT(*) > 1` subquery, so the count that decides whether a recording is listed at all is a count of *provenance links*, not of renditions. |
| Low | ~~**`GetFileByHash` reads lifecycle off the oldest tagset.**~~ `ORDER BY t.id LIMIT 1`: a blob whose oldest appearance is trashed but which carries a newer live approved one reads back as trashed, sending the upload dedup path down the restore/re-stage branch for a blob that is live in the library. Should mirror `reprTagset`'s live-first/primary-first precedence. | **fixed — verified 2026-08-07.** Closed by `0ea9b13` (GC-model P3, 2026-07-15 — one day after this review, which is why it went uncredited): the join became `LEFT JOIN tagsets t ON t.id = reprTagset` and the `ORDER BY t.id` was dropped. `reprTagset` sorts own-appearance-first, then `(rt.deleted_at IS NULL) DESC`, then `is_primary`, then id — i.e. exactly the live-first precedence this row asked for. The same commit also made `deleted_at` read `COALESCE(f.deleted_at, t.deleted_at)`, so a soft-removed rendition is no longer masked by a live appearance. |
| Low | **A pending appearance can still fall out of the review queue.** The P7d claim "`origin_file_id` never reaches NULL on a live draft" does not cover: `MoveTagset` (no review-state check) moves a draft/submitted appearance cross-recording; its origin recording is later hard-deleted; `deleteRecordingFilesTx` deliberately SET-NULLs the cross-recording appearance → `reviewFrom`'s INNER JOIN (`database/review.go:127`) drops it — unapprovable and invisible while still `submitted`. | **open — confirmed 2026-08-07.** Both halves still hold: `reviewFrom` (`database/review.go:126`) is still `FROM tagsets m JOIN files f ON f.id = m.origin_file_id` (INNER), and `MoveTagset` (`database/curate.go:521`) still loads only `recording_id, album_id, album_artist_id, disc_number, track_number` — it never reads `review_state`, so nothing can refuse the move. |
| Info | **Identity dedup is not enforced on resolver moves or `ApproveSubmission`** (collision is flagged, not blocked — per design). Consequence to expect: installing fpcalc on an established library mass-groups renditions and identical approved appearances pile up per recording; cleanup is manual via the duplicates page (see the lens gap above). Note also `UpdateTagsetMetadata` can create identity collisions unguarded. | **open (by design) — re-verified 2026-08-07**: `UpdateTagsetMetadata` (`database/metadata.go:245`) still writes the patch with no collision check, as designed. The pile-up remains cleanable only through `/admin/duplicates`, which is the row above — **so this Info row is gated on that Low/Med fix**, not independent of it: today the escape hatch cannot see the shapes the pile-up produces. |

## Federation — yggstack netstack inbound reader is a single point of failure (2026-07-19) — **FIXED**

> **Closed 2026-08-07 (verification pass).** This was still marked "open
> (accepted for now)" but the tree has carried the fix for weeks — options **2**
> and **4** from the list at the bottom of this entry were both built, and the
> entry was simply never updated.
>
> - **Option 2 — log-and-continue with backoff.** Local yggstack patch **#2**
>   ("inbound-reader resilience — issue #398", `third_party/yggstack/MADSHARE-PATCH.md`,
>   `src/netstack/yggdrasil.go:72`): the reader loop no longer `break`s on a read
>   error. It logs and continues with 50 ms→1 s backoff, and exits only on
>   `Close()` or a terminal `types.ErrClosed` — which is the transient/terminal
>   split step (1) called a prerequisite, resolved by making the *shutdown
>   signal* the terminal condition rather than trying to enumerate the error set.
> - **Option 4 — independent liveness watchdog.** `(*YggdrasilNetstack).InboundReaderAlive()`
>   (`src/netstack/netstack.go:27`) exposes the reader goroutine's liveness;
>   `federation/mesh.go:156` forwards it and `federation/node.go:278` wires it to
>   `Node.InboundHealthy()`, which drives the availability **fail-open** — a node
>   whose inbound path is dead shows its last-known catalog behind a banner
>   instead of blanking. Note this went *further* than option 4 proposed: the
>   entry suggested inferring inbound death from "every friend unreachable",
>   which the availability design later rejected as ambiguous (a self-ping cannot
>   test it either — `HandleLocal:true` loops local traffic). The reader flag is
>   the only unambiguous signal.
>
> Options 3 (supervise/restart) and 5 (upstream it) were **not** done. Neither is
> load-bearing now: a reader that survives its errors does not need a supervisor,
> and the watchdog covers a reader that dies for reasons we did not anticipate by
> surfacing it. Upstreaming stays worthwhile for the reason this entry gave — the
> fork now carries **three** patches, and each one raises the cost of a bump.
> Tracked there, not here.
>
> The detection hint below is kept: it is still the right way to tell this class
> of failure from a transient stall, and it now also describes what
> `inbound_healthy:false` on the madnetwork summary means.

**Original entry (2026-07-19), for the record:**

Found while investigating the madnetwork stream stalls fixed in `af30f04`
(idle-read watchdog + resilient chunk retry). **Not the cause of those stalls** —
they were transient, this one would be permanent and total — but it is a real
fragility in the vendored netstack. Owner decision (2026-07-19): **leave the code
as-is for now**, log it here for future analysis.

| Severity | Issue | Status |
|---|---|---|
| Medium | **A single `ipv6rwc.Read` error permanently kills all inbound mesh traffic.** `third_party/yggstack/src/netstack/yggdrasil.go` (~line 46) runs the embedded netstack's **entire inbound path in one goroutine**: `for { rx, err := nic.ipv6rwc.Read(nic.readBuf); if err != nil { log.Println(err); break } … dispatcher.DeliverNetworkPacket(…) }`. The `break` ends the loop for good, so **one** read error stops packet delivery for the whole node — every mesh connection (friend pings, catalog sync, holdings, blob/manifest/chunk fetches, and serving other nodes) hangs forever until the process restarts. Federation dies silently while the rest of madshare keeps serving happily; the only trace is one `log.Println` line. This is **upstream yggstack behavior**, not something our fork introduced (our one local patch is the `writePacket` data race — see `third_party/yggstack/MADSHARE-PATCH.md`). Not observed in practice so far. | **fixed** — local patch #2 + `InboundReaderAlive`; see the box above. |

**Detection hint (for whoever hits this).** Symptom = *all* madnetwork traffic on
one node dead until restart, while the node otherwise runs fine and the local
yggdrasil peering looks up. Distinguish from the transient path stalls fixed in
`af30f04`: those fail a single chunk in ~20 s and recover; this one never
recovers. Look for a stray read error in the node log just before it went quiet.

**Possible analysis / fixes when we pick this up.**

1. **Characterise the error set first.** Determine what `ipv6rwc.Read` can
   actually return and which of those are *terminal* (core stopped / NIC closed
   during `Stop()`, where exiting is correct) vs *transient*. Without that split,
   any "just don't break" fix risks a hot spin loop on a permanent error. This
   is the prerequisite for every option below.
2. **Log-and-continue with backoff** — keep the loop alive on transient errors,
   exit only on the shutdown signal. Smallest change; needs the error split from
   (1) plus a backoff so a permanent failure doesn't burn a core.
3. **Supervise the reader** — treat reader exit as a node-level fault: surface it
   (health flag / loud log / admin `/admin/network` indicator) and optionally
   restart the netstack or the whole embedded node, instead of failing silently.
   More robust than (2) and it also covers a reader that dies for reasons we
   didn't anticipate.
4. **Independent liveness watchdog** — the refresh loop already pings friends
   every minute; if *every* friend is unreachable for N consecutive rounds while
   the yggdrasil core still reports peers up, that is a strong signal the local
   inbound path is dead. Cheap, needs no yggstack change at all, and is worth
   doing regardless of which of (2)/(3) we choose.
5. **Upstream it.** The right long-term home is upstream yggstack. Our fork
   already carries one patch and each additional one raises the cost of bumping
   the dependency (`MADSHARE-PATCH.md` must be re-applied), so prefer upstreaming
   over accumulating local patches — with (4) as the local safety net meanwhile.

## Federation — the 10-second presence feature was reverted (unstable) (2026-07-21)

The madnetwork "presence" feature (the *10-second rule*: hide an offline
friend's tracks within ~10 s, show them again ~10 s after it returns) was
built, shipped, twice-patched, and then **reverted in full** because it never
became stable. It is logged here so a future attempt starts from the failure
analysis instead of the same design.

**What it was.** A dedicated prober pinged every friend every 5 s (in-memory
`presenceTracker`, hysteresis: offline after 10 s of silence, online after 10 s
of proven reachability). The merged browse/search/summary and remote-playlist
availability were filtered to online friends, with a fully-cached rendition as
the "playable while everyone's offline" exception. `/madnetwork` polled the
summary every 5 s and re-rendered on any online-set change. Design:
`docs/ui/madnetwork-page.md` §Presence.

**Related commits (all part of the reverted feature):**

- `b76e148` — feat: the 10-second presence rule (the feature itself, phase 4).
- `1daa3f1` — fix attempt 1: drain the `/ping` body so the prober reuses one
  keep-alive connection instead of opening a fresh mesh TCP connection every
  5 s (12× the pre-feature churn), which was suspected of stressing the
  netstack ([[yggstack inbound reader SPOF]] above) and stalling transfers.
  Also fed the presence tracker from delivered chunks.
- `cdcb5c1` — fix attempt 2: remove the "skip a recently-seen peer" logic that
  attempt 1 had added, which had halved the idle probe rate to ~10 s against a
  10 s offline threshold → constant offline/online **flapping** on an
  always-reachable peer.
- The revert commit that adds this entry backs out `b76e148`, `1daa3f1`, and
  `cdcb5c1` (restoring the madnetwork files to their pre-presence state at
  `57ba5e1`), keeping the phase-5 *Materialize all* work (`3a8f7b2`) intact.

| Severity | Issue | Status |
|---|---|---|
| — | **Download stalls after ~768 KiB then intermittently recovers.** Reported against the presence build. 768 KiB = the swarm's lead-ramp watermark (256 + 512 KiB), so the small lead chunks arrive and the first 1 MiB bulk chunk stalls. Suspected cause: the 5 s prober's extra mesh connections competing with in-flight blob/chunk fetches over the fragile gVisor netstack (single inbound reader, SPOF above). **Never reproduced on loopback** (no latency/loss — the swarm transferred cleanly there every time, incl. throttled with the prober firing during it), so the mechanism was never confirmed. Attempt 1 (`1daa3f1`) reduced the connection churn but the owner still saw the problem on the real Yggdrasil mesh. | reverted (feature removed) |
| — | **Presence flaps offline/online on an always-online peer.** Introduced by attempt 1's probe-skip; fixed by `cdcb5c1` (verified steady on loopback), but the owner reported the underlying trouble persisted on the real mesh. | reverted (feature removed) |

**Corrected design (2026-07-23).** A replacement — *availability* rather than
*presence* — is now designed doc-first in `docs/architecture/federation.md`
§"Availability & node health" (backend) + `docs/ui/madnetwork-page.md`
§Availability (UI). It drops the fast prober (slow 1-min health + passive
`last_seen` from traffic that already flows), computes availability per **track**
(union over holders) at request time, hides unavailable only at **refresh
boundaries** (page load / search — no live poll), never fails dark (self-health
watchdog + fail-open), and gates on hardening the netstack reader (issue #398).
Not yet implemented.

**If reattempted — notes for next time** (folded into the corrected design above).

- Presence is a *P4 invention*; before it, `/madnetwork` simply always showed
  every friend's cached catalog (only a 1/min `last_seen`, hiding nothing).
  Reverting restored exactly that. So "no presence" is a perfectly good
  fallback, not a regression.
- Do not add a fast prober that opens connections competing with transfers on
  the single-inbound-reader netstack until that SPOF is addressed (the section
  above). Consider deriving liveness passively from traffic that already flows
  (catalog sync, chunk deliveries) rather than a dedicated high-frequency ping.
- Any online/offline threshold must keep a comfortable margin over the probe
  interval (the flapping came from a 1× margin). And prefer to *degrade*
  (dim/annotate) rather than *hide* tracks on a presence flip, so a false
  offline is cosmetic, not a vanished library.
- The download-stall symptom must be reproduced on a real (lossy/latent) mesh
  or a netstack stress harness before trusting any fix — loopback will not
  show it.

## Federation — swarm provider selection is speed-blind (2026-07-24)

Found by the T2 chaos suite (`federation/chaos_test.go`
`TestChaosSlowAndFastSeeder`), which was written to assert the plan's claim that
"the fast holder must carry the majority of chunks".

`chunkPlan.pickProvider` (`federation/swarm.go`) is plain round-robin over the
live holders, with **no notion of how fast any of them is**. So when one holder
crawls and another is fast, roughly half of all chunk *dispatches* still go to
the slow one, and workers pile up blocked on it.

What saves the transfer is not the scheduler but the failure path: a holder that
cannot deliver a chunk inside `Timeouts.PerChunk` accumulates failures and is
eventually retired. Measured with one holder at 16 KiB/s and one unlimited: the
fast holder ends up carrying all 8 chunks, the slow one 0, and the whole 2 MiB
completes in ~9 s (against >2 min if the slow holder had gated it).

So the *outcome* is correct and the suite asserts it end-to-end. But the
mechanism is "wait for it to time out repeatedly", which means:

- Every slow holder costs several `PerChunk` of wasted worker time before it is
  retired — with the production 2-minute `PerChunk` that is minutes.
- A holder that is slow but *just* fast enough to beat `PerChunk` is never
  retired and keeps taking half the dispatches indefinitely.
- Retirement is permanent for the transfer, so a holder that was briefly
  congested is not reconsidered.

Possible directions, none implemented: track per-provider throughput in the
existing `TransferStats` accounting and weight `pickProvider` by it; or
dispatch a chunk to the provider with the fewest bytes in flight; or keep
round-robin but shrink the effective timeout for a provider that is
demonstrably slower than its peers.

Not urgent — v1 correctness is fine, this is efficiency. Deliberately **not**
"fixed" while writing the tests: the suite asserts what the code claims, and
changing the scheduler is a design decision, not a test fix.

**Update (2026-07-25):** the *retirement* half of this was reworked — see the
`-race` findings section below, item 3. The rule is now relative (a holder is
retired once it is `providerFailureLimit` failures worse than the best live
peer) and termination moved to a per-chunk attempt budget. **Dispatch is still
plain round-robin**, so everything above about the scheduler stands unchanged.

**Re-verified 2026-08-07 — still open, unchanged.** `chunkPlan.pickProvider`
(`federation/swarm.go:955`) is still documented as "returns the next non-dead
holder, round-robin" over an `rr` cursor field, with no throughput input. The
code even carries a forward reference to this entry (`swarm.go:1070`: the
relative-retirement rule "leans on dispatch being round-robin" and will need
revisiting "if `pickProvider` ever becomes speed-aware") — so the two halves are
coupled and a scheduler change must re-read `worseThanPeers`, not just
`pickProvider`.

Two things have happened since that change the *priority* rather than the
diagnosis, in opposite directions:

- **Against fixing it:** the F7 member quotas (2026-08-01) mean a holder can now
  answer **429** deliberately, and the swarm is explicitly designed to read that
  as "ask another holder" and de-rank rather than condemn. A throughput-weighted
  scheduler must not read a quota refusal as slowness, or a busy-but-fast peer
  gets starved by the very mechanism meant to find fast peers.
- **For fixing it:** the per-counterparty ledger built for `/admin/swarm`
  (mig 042) now measures real per-node byte rates and persists them. The "track
  per-provider throughput in the existing `TransferStats` accounting" direction
  listed above no longer needs new accounting — the data exists.

## Federation — findings from the full `-race` mesh run (2026-07-24)

The first end-to-end run of
`MADSHARE_CHAOS=1 go test -race -p 1 -timeout 7200s ./federation/... ./tests/mesh/...`
(80 min, 41 pass / 5 fail). **No data races were reported** — the yggstack
`writePacket` fix (`third_party/yggstack/MADSHARE-PATCH.md`) holds under the 8
parallel chunk workers that used to trip it, and `tests/mesh/netfault` is fully
green. All five failures are timing; the four items below are what they exposed,
in the order they should be worked.

| Severity | Issue | Status |
|---|---|---|
| Medium | **`Node.Stop` never tears down the gVisor netstack** (item 1 below) | **fixed** |
| Low | **`TestChaosRateLimitedSeeder`'s seed cap is not scaled** (item 2) | **not a bug — see item 2** |
| Low | **A healthy holder can be dropped as if faulty** (item 3) | **fixed** |
| Info | **The swarm→whole fallback erases the stats that explain the failure** (item 4) | **fixed** |

**Resolved by the item-1 fix.** The full suite was re-run under `-race` with the
teardown patch in place: **0 failures, 0 data races**, and the `federation`
package went from **4812 s to 1272 s** — the same tests, 3.8× faster, because the
run is no longer dragging ~70 dead netstacks behind it. All five original
failures were the leak. Nothing was calibrated to make this pass.

### 1. `Node.Stop` leaks the netstack (Medium)

`Node.Stop` (`federation/node.go`) cancels the loops, shuts down the mesh HTTP
server and calls `core.Stop()` — but it never touches `n.stack`. Nothing calls
`YggdrasilNIC.Close()`, nothing closes the gVisor `stack.Stack`, and the
`rstPackets` drain goroutine started in `NewYggdrasilNIC`
(`third_party/yggstack/src/netstack/yggdrasil.go`) has no exit path at all. So
every stopped node leaves a live netstack behind. (The inbound *reader* does
exit — the #398 patch treats `types.ErrClosed` as terminal — so this is not a
regression from that patch.)

In the server this is close to harmless: federation stops only at process exit.
In the test suite it is not, because ~30 mesh tests start 2–3 nodes each, so the
run accumulates ~70 dead-but-live netstacks. The run degrades accordingly — the
*same* two-node pairing costs 51–128 s in the early test files and 243–377 s in
the last ones:

| early in the run | | late in the run | |
|---|---|---|---|
| `TestPairRejectsMismatchedKey` | 51 s | `TestSwarmMultiSource` | **fail** at 243 s |
| `TestPingOverMesh` | 54 s | `TestHoldingsTracker` | **fail** at 245 s |
| `TestFriendshipHandshake` | 128 s | `TestSwarmFailover` | **fail** at 375 s |
| | | `TestBlobTransfer` | 278 s |

Three of the five failures are that and nothing more: `TestSwarmMultiSource`,
`TestSwarmFailover` and `TestHoldingsTracker` all died inside `makeFriends` — in
test *setup*, before reaching a single assertion — when `waitFor` hit
`meshDeadline` (30 s × `testTimeoutScale` 8 = 240 s) on the same handshake that
`TestFriendshipHandshake` had completed in 128 s earlier in the same process.

The leak is a code fact; that it *causes* the slowdown is a hypothesis. Confirm
it cheaply before changing anything else — run the three failures standalone
(`go test -race -run 'TestSwarmMultiSource|TestSwarmFailover|TestHoldingsTracker'
./federation/`); if they pass well inside the deadline, the diagnosis holds and
**no timeout needs raising**. Raising `testTimeoutScale` to paper over this would
hide a real leak and make an 80-minute run longer still.

**Fixed** by a **third** local yggstack patch (`MADSHARE-PATCH.md`, "netstack
teardown"): a new `(*YggdrasilNetstack).Close()`, an exit for the RST drain
goroutine, an atomic `dispatcher` (NIC removal calls `Attach(nil)`, which races
the inbound reader — a leak fix that introduced a data race would be a poor
trade), and the `nic.stack` back-reference the upstream constructor never sets,
which made `YggdrasilNIC.Close()` a latent nil-deref panic. `Node.Stop` calls it
before `core.Stop`. Measured leak: **9 goroutines per node**, plus the stack.
Guarded by `federation/TestStopReleasesNetstack`, which fails at 56 goroutines
against an 11 baseline without the patch.

### 2. `TestChaosRateLimitedSeeder`'s 4 KiB/s cap does not scale (Low)

The one behavioural failure. The scenario gives holder C a 4 KiB/s serving cap
(`seed_rate_kib`) and asserts the uncapped holder A takes up the slack. Under
`-race` **both** holders were dropped, the swarm phase aborted, the whole-file
fallback also failed, and the transfer errored after 4 m 15 s.

Every other chaos knob is expressed in `testTimeoutScale` units; the seed cap is
an absolute constant. That is fine in isolation — but the *healthy* side is not
constant. The passing scenarios in the same run measure the whole -race mesh at
roughly 25–40 KiB/s aggregate (4 MiB in 110 s in `TestChaosLatencyTimeToFirstByte`,
2 MiB in 86 s in `TestChaosSlowAndFastSeeder`). At 1× the healthy/capped gap is
one to two orders of magnitude; at 8× it is single-digit, and the scenario stops
testing its own premise.

**Resolved: there was nothing here to fix.** Standalone under `-race` with the
item-1 patch the scenario passes in 95 s — the capped holder is dropped after six
cheap `ResponseHeaderTimeout` failures and the uncapped one carries all 8 chunks
with zero failures — and it passes again in the full-suite re-run, in context.
The scenario's premise holds at both clock scales after all; it was starved by
the leak, like everything else late in that run.

Kept as a warning rather than deleted, because the tempting fix was wrong. The
obvious reading of the original failure was "the 4 KiB/s cap is the one chaos
knob not expressed in `testTimeoutScale` units", and the obvious response was to
scale the cap or shrink the content. Either would have made the scenario pass
while leaving a real leak in place, and would have quietly weakened a test that
was working correctly. **When a chaos scenario fails only in a long run, suspect
the run before the scenario.**

### 3. A healthy holder was dropped as if faulty (Low, design) — **fixed**

The part of item 2 that is not test-only. Holder A was uncapped on a clean link
and still accumulated 8 consecutive failures, hitting `providerFailureLimit` (4)
and being dropped — `context deadline exceeded`, i.e. it could not deliver a
256 KiB chunk inside `Timeouts.PerChunk` (6 s × 8 = 48 s). Nothing about A was
faulty; the fetcher simply could not tell "this holder is dead" from "everything
here is slow right now", and the failure counter is what decided.

**Fixed: retirement is now relative, and termination is separate from it.**

A holder is retired once it is `providerFailureLimit` consecutive failures worse
than the **best live holder** (`chunkPlan.worseThanPeers`). When some peer is
still delivering its streak is 0, so the threshold is exactly the old absolute
rule; when every holder is equally deep in failures the fetch is in a slow
moment rather than facing a bad holder, and nobody is retired. Corrupt bytes
still retire a holder immediately — those are evidence about the holder itself,
not about the moment. A sole holder has no peer to be compared against, so the
absolute limit stands there and a fetch against one dead holder still ends.

That last point generalises into the second half of the change. Retiring holders
used to be the *only* thing that stopped a hopeless swarm fetch — there is no
overall deadline on the swarm path (`Timeouts.Transfer` is applied in
`fetchFrom`, i.e. the whole-file path, only), so the fetch ran until the last
holder was killed. That is precisely why a healthy holder had to be declared
faulty: it was the only way to finish. Termination now has its own mechanism —
each chunk carries an attempt budget (`attemptLimit`, `providerFailureLimit ×
holders`, the same worst case the old rule allowed) and a chunk that exhausts it
aborts the transfer with every holder still live.

Relaxing the retirement rule without that backstop would have turned a failing
fetch into an infinite retry loop, so the two halves are not separable.

Tests: `TestChunkPlanRetirementIsRelative` (out-of-line holder retired, equally
slow holders spared, sole holder still terminates) and
`TestChunkPlanAttemptLimit` (an unfetchable chunk aborts with nobody retired).
**Dispatch is untouched** — still plain round-robin, so the speed-blind
scheduler issue above remains open by design.

**Correction (same day, after the fix in item 1).** An earlier revision of this
entry blamed `readStall`'s per-stream stall timer, reasoning that the
serving-side token bucket lets response headers out instantly so a capped holder
is only discovered by burning the whole per-chunk budget. **That is wrong.**
Re-running the scenario standalone under `-race` shows the capped holder failing
with `net/http: timeout awaiting response headers` — the same fast, cheap
`ResponseHeaderTimeout` path as the link-capped scenario, six times, then
dropped. `throttledResponseWriter` throttles the header write too. The mechanism
above (an absolute budget, not a stall timer) is what the evidence supports.

### 4. The fallback erases the stats that explain the failure (Info) — **fixed**

Reading the failure in item 2 was harder than it should have been. `runTransfer`
reacts to a failed swarm phase by calling `stats.setMode("whole")` and
`t.resetProgress()` before running `runWhole`, so the readout printed by
`describe()` said `mode=whole ttfb=0s chunks=0/0` — implying the swarm path never
ran, when in fact it had fetched two chunks and dropped both holders. The
per-provider byte counts survive (they are cumulative by design, see T1's
`resetProgress` note) and were the only reason the real sequence was
recoverable at all.

**Fixed:** `resetAttempt` now archives what it clears into `TransferStats.Prior`
(`[]AttemptStats`: mode, first byte, chunks, chunks done) instead of dropping
it, and `describe()` renders the path that was actually walked. The same failure
now reads:

```
mode=swarm→whole ttfb=0s elapsed=20.5s chunks=0/0 retries=6 failovers=0 …
  [abandoned swarm] ttfb=520.367656ms chunks=1/9
```

The live counters still describe only the live attempt — that was correct and is
unchanged; a reader who lost the prefix must not be told the file is partly
readable. Only attempts that *reached* something are archived (a chunk count, a
chunk done, or a first byte). Mode alone does not qualify, because `runWhole`
names the mode once and then walks its holders resetting between each, which
would otherwise pad the readout with a blank entry per dead holder.

Test: `TestTransferStatsPriorAttempt`.

## Federation — yggdrasil `RemovePeer` panics on an inbound link (2026-07-30)

Found while building F6's underlay de-peering (blocking a node should also cut the
ygg peering where that link is ours). `core.RemovePeer` dereferences a nil
`context.CancelFunc` for any **incoming** link and takes the process down:

```
panic: runtime error: invalid memory address or nil pointer dereference
  github.com/yggdrasil-network/yggdrasil-go/src/core.(*links).remove.func1()
    src/core/link.go:434
```

`links.remove` calls `state.cancel()` unconditionally (link.go:434), but only
`links.add` ever assigns that field (link.go:254). An inbound link's state is
built at link.go:538 without it, so `cancel` is nil for every peer that dialled
*us*. Reproducible in one call, on yggdrasil-go v0.5.14.

**Worked around, not fixed:** `depeerBlocked` skips `PeerInfo.Inbound` links. That
also happens to be the only thing it *can* do — `PeerInfo` carries no handle, so
nothing identifies an inbound link for removal anyway. Consequence to keep in
mind: a blocked node that dialled us keeps its underlay link (and its transit
through us) until it disconnects, though the app-layer block refuses it
everything. `TestFriendshipHandshake` covers both directions and is what caught
the panic.

Fixing it upstream is a two-line guard (`if state.cancel != nil`) plus, ideally, an
API to drop an inbound peering. We carry no yggdrasil-go fork (only the yggstack
one, `third_party/yggstack`), so this is deliberately not patched locally.

**Re-verified 2026-08-07 — still open, and there is still nothing to upgrade
to.** `go list -m -versions github.com/yggdrasil-network/yggdrasil-go` reports
**v0.5.14 as the newest published version**, which is exactly what `go.mod`
pins — so the panic is unfixed upstream and we are not sitting on a stale
dependency. The workaround is intact and still correctly commented:
`depeerBlocked` (`federation/friendship.go:732`) skips inbound links and says
why at the call site. **The consequence stands and is worth re-reading**: a
blocked node that dialled *us* keeps its underlay link and its transit through
us until it disconnects. Only the app layer refuses it. Re-check this row on any
yggdrasil-go bump — the guard is cheap to drop once upstream ships it.

## Federation — mesh tests can flake under load (2026-07-30) — **RESOLVED 2026-08-01**

> **Resolution:** the two long-running flakes were one bug, and it was in the test
> seam rather than in the product: `MembershipTTL`/`SnapshotTTL` set to
> `time.Millisecond` to mean "no memo" is a *real* TTL, and two in-process mesh
> requests can be closer together than that. Named the seam `noMemo =
> time.Nanosecond` (`meshtimeouts_test.go`) and applied it. Eight consecutive
> clean full-package runs after, against a prior rate of about one failure in
> three — the trail below is kept because the eliminations in it are what made
> the answer findable.

One full `go test ./federation/` run failed at `swarm_test.go:219` ("timed out
waiting for A to see B's pairing request") on a run that took 125 s; two fresh
runs of the same suite passed in ~86 s and 13 s (swarm only). These tests start
several in-process yggdrasil nodes with real handshakes and `waitFor` deadlines,
so a loaded machine can miss a pairing window. Not tracked to any code path — the
change under test that round (`depeerBlocked`) returns immediately when no peer is
blocked, and no swarm test blocks one. Worth generous deadlines rather than a
retry loop if it recurs.

**It recurred, in a different test (2026-08-01).** One full-package run failed at
`community_test.go:78` — `TestMemberIsServedTheMadnetworkScope` saw the catalog it
had just narrowed still carrying both entries, as if the memoized snapshot were
stale, though `scopePair` sets `SnapshotTTL` to 1 ms precisely so it cannot be.
Not reproduced since, in either tree: 12 isolated runs and 2 full-package runs at
HEAD, plus 3 isolated and 2 full-package runs with the F7-item-10 changes, all
green. So it is not attributable to that change and the mechanism is unidentified.

**Investigated again 2026-08-01 (F7 item 6), and the field narrowed.** It recurs
at roughly one full-package run in three, always at the same line, always two
entries where one is wanted. What is now *ruled out*:

- **Not a friend audience.** That was the standing hypothesis, because
  `inScope` admits the restricted entry exactly when `Distance == 0`. But
  `startNodePair` never friends the two nodes, and the only peer row on A is the
  voucher, whose key (`k("voucher")`) is not hex — so `AddrForKeyHex` errors and
  `matchPeerAddr` can never match B to it. `serveAudience` cannot return
  `FriendAudience` here.
- **Not the membership memo, and not graph retention.** Both were real bugs found
  in the same file and both are fixed (see §The membership rule, "Memo ordering",
  and `expireGraph`'s own peer read). This one survived both fixes unchanged.
  (The claim recorded here that they had also *closed* the 404 failures was
  wrong — see the next block.)

What is left is the memoized `ownSnapshot`, whose TTL the test deliberately sets
to 1 ms — yet the intervening mesh round trip should make a rebuild certain, so
the arithmetic does not add up either. Next step is instrumentation, not more
reading: log the resolved `Audience` and the snapshot's age on the catalog path
and run the package in a loop until it trips. Do not raise the deadline — nothing
here is a timeout.

**`TestCacheSeedsToTheCommunityNotOutside` is SOLVED, and it was never a load
problem (2026-08-01, F7 item 10).** It reproduces in **isolation** at about one
run in five — 4/20 on a clean tree, 3/20 with the item-10 changes, so the same
rate either way and not attributable to any recent change. The cause is the test
seam, not the product:

`scopePair`-style setups pass `MembershipTTL: time.Millisecond`, meaning "no
memo". It is not: `Intervals.withDefaults` treats only `<= 0` as absent, so a
millisecond is a real TTL, and two in-process mesh requests over the gVisor
netstack can be **less than a millisecond apart**. The test writes the vouching
graph record between two probes; when the second probe lands inside that window
it is answered from a memo built before the write, so a freshly vouched member is
served as an outsider — the 404 the assertion reports, looking exactly like an
access bug and being nothing of the kind. The standing note that "the intervening
mesh round trip should make a rebuild certain" is the assumption that fails.

Fixed by naming the seam: `noMemo = time.Nanosecond` in `meshtimeouts_test.go`,
with the reasoning attached so nobody writes the millisecond again, applied at
all three `MembershipTTL` sites. 0/20 after, and three clean full-package runs.

**And `TestMemberIsServedTheMadnetworkScope` is the same bug, one memo over.**
Its `SnapshotTTL` was left at `time.Millisecond` for one pass on purpose — that
test needs a full-package run to fail at all (0/20 isolated), so flipping both
TTLs at once would have made the result unattributable. It then failed on the
very next full run, which gave the clean comparison: with `SnapshotTTL: noMemo`
it has survived **eight consecutive full-package runs**, where the prior rate was
about one failure in three. That is p ≈ 0.04 of happening by chance, so the
`ownSnapshot` memo — the standing suspect all along — was indeed serving a
pre-restriction catalog to a request that arrived less than a millisecond after
the store write.

Both flakes therefore had **one cause and one fix**, and no product change was
needed. The instrumentation plan is not required. Worth keeping from the episode:
a "practically zero" duration written as a millisecond is not zero next to an
in-process mesh hop, and the give-away in both cases was a *stale read* dressed
up as an access decision (404/403, or an unfiltered catalog) — which is why they
read as security bugs for a week.

**A third site, 2026-08-01 (F7 item 10).** One full-package run failed at
`gossip_sync_test.go:385` — `TestBlockKeyMarksAStranger` read the published mark
record as `{Key:…ab At:0 Reason:}`, i.e. it caught a record whose mark list had
the right length but not yet the fields, so `waitFor`'s "len == 1" predicate
passed on a half-written publish. Different mechanism from the two above and, on
the face of it, a predicate that under-specifies what it is waiting for rather
than a product bug. Not attributable to the change under test (branch weighting
adds a read path only, and nothing in the publish loop calls it): five isolated
runs and two fresh full-package runs green afterwards. If it recurs, tighten the
predicate to what the assertion actually checks instead of the count.

**Re-verified 2026-08-07 — the suggested tightening was never applied, so this
site is still armed.** `gossip_sync_test.go:379` still waits on
`rec != nil && len(rec.Marks) == 1` and *then* asserts on `rec.Marks[0].Key` /
`.Reason` outside the wait — the predicate still admits a half-written record and
the assertion still reads it. Folding the field checks into the `waitFor`
closure is a two-line change that costs nothing when the record is written whole,
so there is no reason to keep waiting for a recurrence before making it.

**FIXED 2026-08-08 — with one deliberate deviation from the suggestion above.**
Folding the *field checks* into the predicate would have worked, but `waitFor`
`t.Fatalf`s on timeout (`friendship_test.go:530`), so a predicate that also
checked the values would have made the trailing assertion **dead code** — and
turned a genuinely wrong mark from a clear `mark = {...}` failure into a vague
"timed out waiting for the stranger's mark to be published". So the predicate now
waits for **wholeness** (`len(rec.Marks) == 1 && rec.Marks[0].Key != ""`) and the
assertion keeps checking **correctness**. That is the split the finding actually
calls for: the defect was a predicate that under-specified what "published"
means, not an assertion in the wrong place. `TestBlockKeyMarksAStranger` passes in
0.26 s, so the added condition costs nothing when the record is written whole.

**A fourth site, 2026-08-01 (F7 item 9).** One full-package run failed at
`swarm_test.go:242` — `TestSwarmFailover` timed out in `makeFriends` ("timed out
waiting for A to see B's pairing request"), i.e. it spent the whole 30 s
`meshDeadline` without the pair request landing. Green 3/3 isolated and on the
next full-package run.

Not attributable to the change under test, and this one is checkable rather than
merely plausible: Go runs a package's tests in declaration order, and
`go test -list` puts `TestSwarmFailover` at #117 with every new token test at
#121 or later — none of them had executed when it timed out. So it is mesh
*convergence* under whatever the preceding 116 tests left behind, not a new
access rule. Different mechanism again from the three above: no memo and no
half-written record, just a pairing round trip that did not complete inside the
budget.

If it recurs, the thing to measure is whether `meshDeadline` is being consumed by
convergence or by the 250 ms poll interval colliding with the 1-minute refresh
loop — `makeFriends` waits on a loop tick, so a run that just misses one waits a
whole cycle, and 30 s is only half of that.

## Cache seeding overrides a recording's sharing scope (found 2026-08-02, F8 mesh verification)

**FIXED 2026-08-02** — option 2 below, on the owner's call: the duplicate cache
copy is evicted the moment the blob is in the library, so there is nothing left
for the two rules to disagree about. Write path (`Node.EvictCachedBlob`, called
from every branch of the download handler that leaves bytes in the library) plus
a startup sweep (`database.EvictCachedMadnetworkBlobs`) that fixes nodes which
already hold duplicates from before the fix. Details at the end of this entry.

**Was: open. Pre-existing — F8 did not introduce it, but F8 makes it easy to reach.**

`seedableBlob` (`federation/swarm.go`) resolves a blob in two steps: the library
copy, gated by `BlobVisibleTo` (scope-aware), and — if that refuses — the
download cache, gated only by `policy.Cache && aud.ServesCache()`. There is no
scope check on the second branch, deliberately: a cached blob is somebody else's
content and its scope is not ours to declare (§Distribution, and the F7 posture
"the swarm must not care which node happens to hold bytes").

The gap is the case where **one hash is both** — a blob this node materialized
from the network (so it is in `<data_dir>/cache/madnetwork/`) *and* holds in its
library (so a local recording carries a `share_depth`). Scoping that recording to
**Direct friends** stops the catalog advertising it and stops the library branch
serving it, and then the cache branch serves the identical bytes to any member
anyway.

That contradicts the invariant §Sharing scope leads with, and the one
`meshlab check` exists to assert: *catalog and bytes read one rule, so what an
audience is not shown it also cannot fetch.*

**Reproduced on real processes**, and isolated to the cache branch rather than
argued from the code: in a seeded 3-node lab, materialize an upgrade onto `a`
(putting the blob in a's cache *and* library), then run `meshlab check`. The case
`a token buys membership, never friendship` fails with **200, want 404**.
Setting `seed_cache=false` on `a` and re-running flips that same case to **PASS**
— nothing else changed.

**Control experiment, so "pre-existing" is measured rather than asserted.** A
fresh lab is 20/20. The same failure then reproduces using ONLY
`POST /api/madnetwork/download` — the F3 endpoint shipped 2026-07-18 — with no
F8 endpoint called at any point: download b's blob onto `a`, run `meshlab check`,
same case, same `200, want 404`. F8 neither causes nor worsens this; it only
gives the operation a button.

Severity is bounded by who could already get the bytes: in the reproduction they
originated on `b` at Madnetwork scope, so no content reached anyone who could not
have fetched it from `b` directly. The defect is that the local admin's scope
decision is silently ineffective, not (in this case) that new content escaped.
The general case is worse in principle — an admin narrows a recording, gets no
warning, and this node keeps seeding it to the whole community.

Directions, none of them decided:
1. Have the cache branch skip a hash this node also holds as a *library* blob,
   and let the library branch's scope verdict stand — the local decision wins for
   bytes we actually hold, cache relaying keeps working for everything else.
2. Evict the cache entry when a materialized blob lands in the library (it is
   redundant storage the moment the file exists under `files_dir`, which is a
   second reason to do it and would close this as a side effect).
3. Surface it instead of fixing it: the access modal warns that a scope narrower
   than the node default cannot be enforced for a blob still in the seed cache.

Option 2 looks strongest — the duplicate cache copy has no purpose once the blob
is in the library, and deleting it removes the ambiguity rather than adding a
rule about it. Wants an owner decision before anything is built.

### The fix (owner chose option 2)

- `(*federation.Node).EvictCachedBlob(hash)` removes `<cacheDir>/<hash>`, leaving
  an in-flight transfer's `.part` alone. Safe under concurrency: `EnsureBlob`
  resolves the library BEFORE the cache, so a later fetch short-circuits locally,
  and POSIX keeps an open descriptor alive across the unlink.
- Called from every branch of `madnetworkDownload` that ends with the bytes in
  the library — the fresh staging path, the late byte-dup attach, and the early
  "already held" reply (a stream can have cached what the library already has).
  Best-effort: the download succeeded, and a stale cache entry is a leak to fix,
  not a reason to fail the fetch.
- `database.EvictCachedMadnetworkBlobs` at startup is the catch-all, and the only
  thing that helps a node that already materialized tracks. It runs whether or
  not federation is enabled now — a node that switched it off still has the cache
  it filled while it was on. A row in ANY state counts as held, deliberately
  including a trashed one: its bytes stay under `files_dir` for the quarantine
  window, and that is the sharper case, since a trashed blob is invisible to the
  library branch and would otherwise be served from cache.

**The hole was bigger than this entry first described.** Verified on the lab: the
materialized blob had **zero approved appearances** on the node serving it, so
what leaked was not merely a narrowed recording but content that had never been
published at all — a download sitting unreviewed in the staging bucket, seeded to
the whole community.

**Verified on real processes, both halves.** Materializing onto `a` now leaves the
cache empty and the library holding one copy, and `meshlab check`'s
`a token buys membership, never friendship` reads 404 where it read 200 before.
For the sweep: a duplicate was put back by hand, `meshlab restart a` logged
`evicted 1 cached blob(s) the library already holds`, and the cache was empty
again. A fresh lab is 20/20 with the fixed binary.

Note for whoever runs `meshlab check` on a lab where something was materialized:
`pickSubject` can land on that blob, and two cases then fail for lab-state
reasons rather than product ones — `vouched listener node is served…` wants 200
but the blob is unpublished on that node, and two friend cases skip themselves.
Judge the suite on a fresh lab.

## Flake — `TestListenerNodeTokenCarriesTheAccountACL` under full-suite load (2026-08-02)

Seen **once**, during a `go test ./...` run where the heavy packages (api,
database, federation) were competing for the machine:

```
--- FAIL: TestListenerNodeTokenCarriesTheAccountACL (3.03s)
    token_mesh_test.go:167: guest-only bearer fetching guest-playable content = 404, want 200
```

Not reproduced in seven subsequent runs — five of the test alone, two of the
whole federation package (which was green immediately before and after, at 134s
and 264s, the spread itself showing how load-dependent that package is). Noticed
while adding the mesh listener's identity files; that change writes
`federation.key.pub` / `federation.addr` at `StartTransport` and nothing reads
them back, so there is no path from it to token audience resolution — but it is
logged here rather than dismissed, because "unrelated" is a claim and one
observation is not a refutation of it.

Worth a look, since a real 404 came back rather than a timeout: `meshGet`'s
`waitFor` retries only until the mesh answers *at all*, and returns the first
status it gets, whatever it is. So the helper waits for the transport to
converge but not for A to be able to **place the issuer** — if the vouching
gossip record has not been accepted by the time the first response lands, the
token arm of `serveAudience` correctly answers 404 and the test reads it as the
verdict. `MembershipTTL: noMemo` removes the memo but not the acceptance step
(`GraphAccept` is left at its 1-minute default here). The sibling case at
token_mesh_test.go:125 wraps the same assertion in its own `waitFor` and did not
fail.

If that diagnosis holds, the fix is in the test seam rather than the product —
the same shape as the two flakes closed in 3543480.

**Re-verified 2026-08-07 — still open, and the diagnosis checks out against the
source.** `token_mesh_test.go:167` is still a bare
`meshGet(…, guest, token)` asserting 200 with no `waitFor` around it, while the
sibling `TestListenerNodeTokenNeedsAnIssuerWeCanPlace` (line 110) *does* wrap its
status expectation in a `waitFor` for its own last step, which is exactly the
shape missing here. `scopePair` sets `MembershipTTL: noMemo`, which — as
this entry predicted — removes the memo but not the **acceptance** step:
`GraphAccept` is left at its 1-minute default, so `vouchFor` writing the record
does not mean A has placed the issuer by the time the first mesh response lands.
The fix is the one-line seam change (wrap the 200 assertion in `waitFor`, as the
sibling does); no product change is implied. Left unfixed only because it has
been seen once.

**FIXED 2026-08-08 — at TWO sites, not one, because the "safe sibling" was not
safe.** The seam change above was applied at `token_mesh_test.go:166`. But the
sibling this entry cites as the correct shape is only wrapped at its *last* step
(the un-vouch transition, line 132); its **first** assertion — line 127, a bare
`meshGet` expecting 200 immediately after `vouchFor` — is the identical race,
`t.Fatalf` and all. This entry read "the sibling did not fail" as evidence that
the sibling was correctly written, and it is not: it is the same armed seam that
happens not to have tripped yet. Both now wait for A to place the issuer before
asserting 200. **The whole point of the wait is that 404 is the pre-acceptance
answer**, so the failing form and the not-yet form are indistinguishable to a
single `meshGet` — which is exactly why this read as an access bug. All six
`TestListenerNodeToken*` / `TestBlockKeyMarksAStranger` cases pass at unchanged
timings (~3.0 s each), confirming the wait is a no-op once the graph has
converged.

## Madnetwork playback stops mid-track — investigation, no fix (2026-08-07)

Alpha-tester report against **v0.8.0**: an **uncached** madnetwork track plays
~15 s and stops; asking Firefox (140.13.0esr, Windows 10) to fetch it again by
hand makes it play "a little further", repeatedly. Reached over a **direct
yggdrasil node under `[[listen_mesh]]`**. Local and cached files unaffected;
Materialize unaffected. The owner reproduced it separately on v0.8.0 (playing
~1 s) and could not reproduce it afterwards.

Investigated against the live two-node test instance (`v0.8.4`, reached both
through the nginx reverse proxy on :81 and on the node's own mesh address).
**The end-to-end symptom did not reproduce** — every cold stream completed. What
did reproduce, on every single cold stream, is the *stall point* the symptom is
built on, and the delivery path that turns a stall into a dead player.

### The stall point is structural and reproduces every time

The swarm's chunk layout front-loads a **lead ramp** (256 KiB, then 512 KiB)
before switching to 1 MiB bulk chunks, and chunk 0 is additionally
*speculatively prefetched* during the manifest round trip (`speculateChunk0`).
So the first 768 KiB arrive almost instantly, and the reader then has to wait a
**full bulk-chunk fetch** for the first time. Measured gaps at exactly those
boundaries, on cold uncached FLACs:

| Scenario | Route | Outcome | Gap |
|---|---|---|---|
| single stream, full speed | direct mesh | complete | **4.46 s @ 786432** |
| single stream, full speed | nginx :81 | complete | **4.39 s @ 786432** |
| 6 concurrent streams | direct mesh | 4/5 complete | 1.74–2.84 s @ 262144 |
| 6 concurrent streams | direct mesh | complete | **20.35 s @ 9175040** (= `ChunkStall`) |
| 6 concurrent streams | direct mesh | probe gave up @ 6029312 | >30 s (probe's own socket timeout, **not** a server truncation) |
| 5 cold rounds × 6 concurrent | direct mesh | **30/30 completed** | gaps at chunk 1/2, up to 9.7 s |
| real-time paced, 256 s | direct mesh | complete | none |
| player sim, 60 s readahead ×3 | direct mesh | complete, 0 underruns | 1.1 s @ 786432 |

Every gap lands on a chunk boundary. 786432 = 256 KiB + 512 KiB is the **same
"~768 KiB" watermark** already logged under *the 10-second presence feature was
reverted* (2026-07-21), where it was blamed on the 5 s presence prober. **That
attribution is wrong**: the prober was reverted long ago and the stall is still
here on v0.8.4. That row can be reopened as its own defect.

This also explains the two different reported durations. The stop point is fixed
in **bytes**, so the audible duration is just that byte count over the file's
bitrate — 768 KiB is ~15 s at ~420 kbps and ~2 s at hi-res-FLAC rates, and
262144 B is ~16 s at 128 kbps. "15 seconds" and "1 second" are the same stop.

### The leading candidate: the swarm→whole fallback strands the reader

A stall alone self-heals, so the stall is not the failure — the owner made this
point and it is correct. This is:

`api.copyTransfer` opens the part file **once** and holds that descriptor for the
whole response. When the swarm fails, `runTransfer` does
`os.Remove(t.partPath)` and then `runWhole` → `fetchFrom` recreates the same path
with `O_CREATE|O_TRUNC` — **a new inode**. The reader is left pinned to the old,
unlinked one, which `fetchSwarm` had pre-sized to the full length with
`f.Truncate(man.Size)`. Past the last chunk that landed, that orphaned file reads
as **zeros, not EOF**.

Reproduced deterministically (`zz_diag_staleinode_test.go`, kept in the session
scratchpad, replays the exact file lifecycle against the real `transfer`):

```
bytes before the fallback  : correct = true
transfer.Available(262144) : 786432   (says: readable)
next 65536 bytes read      : 65536 of them are 0x00
```

Consequences, and why this fits the report better than anything else:

- **`Content-Length` is satisfied exactly.** No truncation, no `unexpected EOF`,
  no error, no log line — the response looks perfect. The decoder simply stops
  where the real bytes ran out. That is "plays N seconds and stops" with nothing
  anywhere to see.
- **N is the swarm's last landed chunk over the bitrate** — usually the lead ramp,
  262144 or 786432 B, which is the 15 s / 1 s the two reporters measured.
- **A manual re-fetch opens the NEW inode**, gets real bytes, and plays further —
  exactly the reported recovery.
- Local and cached files never take this path; Materialize is unaffected because
  it waits for `Done()` and reads the final renamed file.
- Fluid by construction: it needs the swarm to fail *after* making progress, i.e.
  one bad mesh moment. With a single holder — the common case here, most tracks
  are held by exactly one node — 4 consecutive chunk failures
  (`providerFailureLimit`) retire the sole holder and abort the swarm.

Note the success path is safe: `os.Rename` preserves the inode. It is only the
failure transition that unlinks. `runWhole`'s per-holder retries are also safe —
they `O_TRUNC` the same inode rather than replacing it.

### The two supporting defects

1. **The reader cannot escape a stalled in-flight chunk.** `copyTransfer` blocks
   in `WaitFor` on the *specific* chunk it needs; `chunkPlan.prioritize` only
   reorders `pending`, so a chunk already **in flight** is a no-op and the reader
   waits out `ChunkStall` (20 s) or `PerChunk` (**2 min** if the holder trickles
   bytes — the watchdog resets on any read that returns data).
2. **A failed transfer truncates the response silently.** Separate throwaway Go
   test: once headers are committed, the client gets **200 + full
   `Content-Length` + a short body**, `unexpected EOF`, and **nothing logged** —
   `copyTransfer`'s three exits (client gone / EOF / transfer failed) are one
   bare `return`. The same failure *before* the first byte is a clean 502.
   `player.js` binds only `ended` and `error`, so the client never retries.

### Ruled out

- **Fetch throughput / fetch failure.** For the one truncated direct-route
  stream, the server's own ledger shows `in_cache: true`, `down_bytes` = full
  size, `wasted_bytes: 0` — **the fetch completed; only delivery to the client
  stalled.** Cache-through means the fetch normally finishes in ~25 s while
  playback lasts minutes, so fetch speed cannot itself stop playback.
- **Rate limits.** Both node knobs read `effective_kib: 0` (unlimited, source
  `config`), so the "throttled read looks like a stalled holder" gotcha is not
  in play on this instance.
- **An HTTP write deadline.** No `WriteTimeout`/`IdleTimeout` is set on any
  `http.Server` in `madshare.go`.
- **Netstack inbound death.** `inbound_healthy: true` throughout; a 90 s idle
  socket and a 256 s real-time paced read both survived on the mesh listener.
- **v0.8.0 → v0.8.4 drift.** Nothing in `madnetworkStream` /
  `serveGrowingTransfer` / `copyTransfer` changed; the only fetch-path edits wrap
  readers in `n.wire(...)` for accounting/throttling. The cache and swarm work
  did **not** fix this — the stall still reproduces.

### Separate defect found on the way: the reverse proxy truncates idle streams

Not the tester's bug (they were on the direct route, and this hits cached files
too), but real and equally silent. Holding the socket without consuming, as a
media element does once its readahead buffer is full:

| Idle | Result |
|---|---|
| 45 s | completes |
| 70 s | **truncated**, short by 18596986 |
| 90 s | **truncated**, short by 28410004 |
| 90 s, already-cached blob | **truncated**, short by 16834605 |
| 90 s, direct mesh route | completes |

Consistent with nginx's default 60 s `send_timeout`; the client sees a clean EOF
against a full `Content-Length`, i.e. the same silent truncation as above. Worth
a documented `proxy_*`/`send_timeout` setting in `contrib/nginx`.

| Severity | Issue | Status |
|---|---|---|
| **High** | **The swarm→whole fallback strands the streaming reader on an unlinked inode**, which was pre-sized to the full length and therefore serves **zeros** past the last landed chunk. `Content-Length` is met exactly, nothing errors, nothing is logged. Best fit for the reported symptom. Reproduced deterministically. | **open — re-confirmed 2026-08-07** at both ends of the mechanism: `federation/transfer.go:526` `os.Remove(t.partPath)` on the failed-swarm branch, then `runWhole` → `fetchFrom` re-creating the same path with `O_CREATE\|O_WRONLY\|O_TRUNC` (`transfer.go:594`) = a new inode; and `api/madnetwork_transfer_handlers.go:148` opening the file **once** (`t.Open()` + `defer f.Close()`) and holding it for the whole response. Note `copyTransfer:171` already carries a `continue` commented "offset briefly unavailable (e.g. a swarm→whole-file fallback); re-wait" — the fallback was anticipated in the reader, but as a *timing* gap, not as a file-identity change, which is why the mitigation there does not help. |
| **High** | **A failed transfer truncates the stream silently** — 200 + full `Content-Length` + short body, no log line, no client retry. Makes every variant of this bug invisible. | **open — re-confirmed 2026-08-07.** `copyTransfer` still has four bare `return`s with no logging and no distinction between them: `t.Open()` failure (line 150), `Seek` failure (154), `WaitFor` error — "EOF (done), client gone, or the transfer failed midway", all one branch (161), and a read error (184). The comment at 150 ("headers may be written already — just drop the connection") states the behaviour as intended; the defect is that a *successful completion* and a *failed transfer* leave by the same door. |
| Medium | **The streaming reader cannot escape a stalled in-flight chunk** — `prioritize` is a no-op once a chunk is dispatched; the reader waits `ChunkStall` (20 s) or `PerChunk` (2 min). | **open — re-confirmed 2026-08-07** (`chunkPlan.prioritize` still reorders `pending` only). |
| Medium | **Reopen: the ~768 KiB stall is not the presence prober.** Reproduced on v0.8.4 with the prober long removed; it is the lead-ramp → first-bulk-chunk transition. Benign on a healthy mesh (18/18 cold streams completed), but it is the moment the player's buffer is thinnest. | **open — re-confirmed 2026-08-07.** The layout is unchanged (`speculateChunk0` + lead ramp → bulk, `transfer.go:505`). Cross-reference kept: the corresponding row under *the 10-second presence feature was reverted* (2026-07-21) attributes this same 768 KiB watermark to the prober and that attribution is **wrong** — do not re-derive it from there. |
| Low | **nginx `send_timeout` truncates idle media streams** at ~60 s, for cached and remote blobs alike. Triggered in practice by *pausing* playback, not by readahead. | **open — re-confirmed 2026-08-07, and the near-miss is worth naming.** Both shipped configs (`contrib/nginx/madshare-ssl.conf:81`, `madshare-yggdrasil.conf:57`) already set `proxy_read_timeout 3600s` + `proxy_send_timeout 3600s` under a comment reading "Long timeouts cover slow uploads and long streams" — but those two govern nginx↔**upstream**. The client-facing directive is plain **`send_timeout`** (default 60 s), and it is set in neither file. So the configs look like they cover this and do not; whoever fixes it should add `send_timeout` beside the existing pair rather than assume it is missing by oversight. |

**Not reproduced:** 45 cold streams over the direct mesh route — 30
length-checked (5 rounds × 6 concurrent) plus **15 verified against the content
hash** — served correct bytes every time. The hash check matters: the fallback
bug below yields a stream of the *correct length*, so a length check alone would
score it a pass. Only stalls were seen, all self-healing, the worst being **21.1 s
at byte 262144** (a full `ChunkStall` at the first chunk boundary). The
fallback bug above needs the swarm to fail, which did not happen on a healthy
mesh. That is consistent with the symptom being fluid rather than with it being
absent.

Raw probe data, the harnesses and the throwaway Go test are in the session
scratchpad; none of it was committed.

### An attempt was built and rolled back — it is parked on `wave1-rolled-back` (2026-08-07)

Three fixes for the rows above were written, tested, committed, and then
**reverted on the owner's call, because none of them had been reproduced**. The
work is not lost: it sits on the local branch **`wave1-rolled-back`**, based on
`7636328`, and `aidev` was reset back to that base.

```
65b450e  fix(contrib): set the client-facing send_timeout in the nginx examples
197b1cf  feat(api): name every way a madnetwork stream ends early
908be9e  fix(federation): keep the reader's file across the swarm→whole fallback
```

`git diff 7636328 wave1-rolled-back` — 6 files, +220/−5. Whole suite was green
(`go build`, `go vet`, `go test ./...`, the full `federation` package at 152 s,
and the `nofederation`/`nowebui`/combined tag builds).

**Why it was pulled.** The owner's rule, stated the same day: *don't fix an issue
that has not been reproduced, because then there is no way to be sure the fix
fixes what it is meant to fix.* The three changes each rested on a mechanism read
out of the code, not on an observed failure:

- The **stranded inode** had a Go test that reproduced the *file-lifecycle
  arithmetic* — but that test builds the scenario itself (create, pre-size,
  unlink, observe zeros), so it confirms the theory rather than the bug. The
  end-to-end symptom never occurred here: 45 cold streams all served correct
  bytes.
- The **exit logging** came purely from reading `copyTransfer`.
- The **nginx `send_timeout`** numbers were measured, but in an *earlier* session;
  they were carried forward as if freshly observed.

A fix shipped without a reproduction also costs the reproduction: once the code
changes, a later tester report can no longer be compared against the original
behaviour. That is the concrete reason to park rather than merge.

**What each commit contains, so nobody re-derives it:**

| Commit | Change | Independent of a repro? |
|---|---|---|
| `908be9e` | New `(*transfer).discardPartial()` — resets progress, then `os.Truncate(partPath, 0)` instead of `os.Remove`, so the swarm→whole transition keeps the inode the reader holds. `runTransfer` calls it where the unlink was. Plus `TestSwarmFallbackKeepsTheReadersFile`, which asserts on **content, not length** (the stranded file is exactly the right size) and was verified to fail against the old behaviour: *byte 8192 = 0, want 161 (57344 zero bytes follow)*. | **No.** This is the speculative one — it changes fetch behaviour on the strength of a theory. |
| `197b1cf` | `copyTransfer` names each early exit (open/seek failure, transfer failed, read failed) with bytes delivered, bytes promised and the **stop offset**. Client-gone stays quiet. | **Partly.** It adds no behaviour, only log lines — but it is also not a fix, so merging it proves nothing and only helps if the bug recurs while it is in place. |
| `65b450e` | `send_timeout 3600s` in both `contrib/nginx/*.conf` + a README note that the neighbouring `proxy_read_timeout`/`proxy_send_timeout` pair governs nginx↔madshare, not nginx↔browser. | **Almost.** The measurement exists in this file (45 s completes, 70/90 s truncate); it just was not re-run. Cheapest of the three to re-measure. |

**What would justify bringing each back.** These are the missing facts, not a
plan:

1. **`908be9e`** — an observed swarm→whole fallback that coincides with a stopped
   track. The server-side tell is the existing log line `swarm fetch <hash>
   failed (…); falling back to whole-file` appearing while a listener reports
   silence. Note the reader's own signals cannot show it (see the row above:
   that path completes "successfully" and delivers zeros), so the evidence has to
   come from the fallback log plus a client that stopped, or from a captured
   response body that hashes wrong at full length.
2. **`197b1cf`** — nothing to reproduce; it is instrumentation. If the owner wants
   the next report to arrive with a stop offset attached, this is the piece that
   does it, and it can be judged on that basis alone rather than as a fix.
3. **`65b450e`** — re-run the idle-stream measurement against the current
   instance (hold the socket without consuming for 90 s through the proxy, then
   over the direct mesh route as the control). Two probes.

**If the branch is ever deleted**, the mechanism descriptions in the rows above
plus this table are enough to rebuild all three; the only thing genuinely worth
recovering is the test in `908be9e`, because getting its assertion right —
content rather than length — is the non-obvious part.
