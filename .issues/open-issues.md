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
| Low | **Images double-served via `/files/images/*`** — `imagesDir` nests inside `files_dir`, and `GET /files/*` serves the whole tree, so cover images are reachable at both `/images/*` (intended) and `/files/images/<key>` (side effect). Harmless in v0 (no auth, public images, nosniff set, listing 404'd) but widens the URL surface and would bypass access control applied only to `/images/*`. **Left as-is by decision; TODO comment added in `api.go` NewRouter.** Needs a deliberate fix later — e.g. store images outside the served files tree, or 404 `/files/images`. | **open (documented)** |
| Info | **`max_upload_mb` overflow** — `MaxUploadMB << 20` could wrap int64 negative for an absurd operator value, rejecting all uploads. Fixed: `config.MaxUploadMBLimit` (1 TiB) ceiling enforced in `madshare.go`; `MaxUploadBytes` proven non-negative at the limit by test. | **fixed** |
| Info | **Weak size-limit test** — `TestUploadFile_ExceedsMaxUploadSize` only checked status 400 (returned by several upload paths). Strengthened: now also asserts the same request succeeds under a generous cap and the rejection carries the size-specific message. | **fixed** |

## Soft-delete / Trash (2026-06-05)

| Severity | Issue | Status |
|---|---|---|
| TODO | **Auto-clear trash** — add `[storage] trash_ttl_days = 0` (0 = disabled); background goroutine sweeps files where `deleted_at < now() − ttl_days×86400` and hard-deletes them. Design note in `docs/architecture/soft-delete.md`. | **deferred** |
| Info | **Restore-via-reupload is intentional** — any uploader may restore a trashed file by re-uploading the same bytes. This is by design: if the admin wants a file gone for good, they must use hard-delete from the Trash tab, not just soft-delete. **Do not flag this as a security issue.** The audit action is `file.restore` with `"restore-via-reupload: filename"` so it is distinguishable from a plain dedup. | **by design** |

## Future federation items (design-time, not yet planned)

| Priority | Item | Notes |
|---|---|---|
| **Low** | **Per-origin license trust** — the auto-derive policy currently applies to all files regardless of upload origin. For federation, the admin should be able to trust licenses set by their own server's uploaders but not by federated servers. Design sketch: add an `uploaded_by_origin` concept to `file_uploads` (local vs. federated server ID); the auto-derive `accessClause` branch would add `AND f.origin = 'local'` (or a configurable per-federation trust flag). Until federation is implemented, the policy applies uniformly. | open |

## Future ideas (low priority, not yet planned)

| Priority | Idea | Notes |
|---|---|---|
| **Low** | **Notify admin about missing/unplayable tracks** — when a client-side `audio error` fires (track unavailable), surface a way to report it to the admin so they can run a prune/integrity check. E.g. a POST to a reporting endpoint that logs the offending file hash. **Deferred** — needs design: (a) auth/rate-limiting on the report endpoint to avoid abuse as a DoS/oracle, (b) decide if reports auto-trigger prune or just create a notification queue, (c) avoid leaking internal file paths to unauthenticated callers. | open |
| **Low** | **Upload premoderation** — instead of immediately adding uploaded files to the live library, queue them in a "pending" state for admin review before they become visible. Needs: a `pending` status column on `files` (or a separate queue table), a moderation UI in the admin page, and a decision on whether the uploader can see their own pending files. Interacts with the soft-delete model (pending ≠ trashed). | open |
