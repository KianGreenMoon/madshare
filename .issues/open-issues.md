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
| Low | **Multi-disc albums showed duplicate track numbers.** The album track list (`listTracksByAlbumID`) ordered by `track_number` only and never returned `disc_number`, so disc 1 track 1 and disc 2 track 1 both rendered as "1", interleaved. Fixed: the query now `ORDER BY COALESCE(disc_number,1), track_number, title` and carries `disc_number` out (DTO `disc_number`); the library drill-down (`app.js`) detects a multi-disc album (>1 distinct disc, untagged = disc 1) and renders "Disc N" subheadings with per-disc numbering — single-disc albums are unchanged. Files lacking disc tags can be fixed via the Extended-edit Disc-number field (row 127). Test: `TestListTracksByAlbumID_DiscOrdering`. Not yet applied to the admin entity view / cmus list. | **fixed** |

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
