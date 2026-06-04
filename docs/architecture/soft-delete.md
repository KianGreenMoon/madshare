# Soft Delete & Trash Bucket

## Overview

Instead of immediately destroying a file on admin delete, the system marks it as
*trashed* (`deleted_at` timestamp in the DB). Trashed files are hidden from all
user-facing listings and access-checked endpoints. Admins can review trashed
files in a dedicated Trash tab, restore them to the library, or permanently
remove them (blob + DB row).

A re-upload of a trashed file automatically restores it instead of creating a
duplicate.

---

## Database — migration 007

```sql
ALTER TABLE files ADD COLUMN deleted_at INTEGER DEFAULT NULL;
CREATE INDEX idx_files_deleted ON files(deleted_at);
```

`NULL` = live file. Non-null = Unix timestamp of soft deletion.

---

## Model changes (`database/models.go`)

Add `DeletedAt sql.NullInt64` to the `File` struct.

Add `DeletedAt sql.NullInt64` to `FileListEntry` (needed by the Trash tab to
show deletion time) — or introduce a separate `TrashedFileEntry` type if the
fields diverge further.

---

## Database layer (`database/files.go`, `database/repo.go`)

| Method | Change |
|---|---|
| `GetFileByHash` | Scan `deleted_at` into `File.DeletedAt`. Returns soft-deleted files (upload handler needs to detect them). |
| `listFiles` / `listArtists` / `listAlbums` / `listTracks` | Add `f.deleted_at IS NULL` filter to all listing queries. Applies to both filtered and unfiltered variants. |
| `ListFileRefs` | **No change** — used by prune, which treats soft-deleted files as normal (blob still on disk). |
| `SoftDeleteFileByHash` | New: `UPDATE files SET deleted_at = ? WHERE hash = ?`. Returns filenames. Does not touch the blob. |
| `HardDeleteFileByHash` | Renamed from current `DeleteFileByHash`: deletes the DB row + cascade. Used only by the Trash tab's permanent-delete path. |
| `RestoreFileByHash` | New: `UPDATE files SET deleted_at = NULL WHERE hash = ?`. |
| `ListTrashedFiles` | New: returns files where `deleted_at IS NOT NULL`, ordered by `deleted_at DESC`. |
| `FileAccessibleByHash` | Add `AND deleted_at IS NULL` so soft-deleted files are inaccessible to non-admins at the per-hash check level. |

**Repository interface** (`repo.go`): replace `DeleteFileByHash` with
`SoftDeleteFileByHash`, `HardDeleteFileByHash`, `RestoreFileByHash`,
`ListTrashedFiles`.

---

## Upload handler (`api/handlers.go`)

After `GetFileByHash` returns a match, branch on `existing.DeletedAt.Valid`:

- **Soft-deleted**: call `RestoreFileByHash`, then `RecordUpload` with the new
  filename. Respond as a normal successful upload.
- **Live**: existing dedup path — just `RecordUpload`.

---

## API endpoints (`api/admin_handlers.go`, `api/api.go`)

All new endpoints are registered under `/api/admin/` with the `file.delete`
permission guard.

| Endpoint | Handler | Notes |
|---|---|---|
| `DELETE /api/admin/files/{hash}` | `adminDeleteFile` | **Changed**: calls `SoftDeleteFileByHash`. Does **not** delete the blob. |
| `GET /api/admin/trash` | `adminTrashList` | New: calls `ListTrashedFiles`. |
| `DELETE /api/admin/trash/{hash}` | `adminTrashHardDelete` | New: `HardDeleteFileByHash` + `storage.DeleteAll`. |
| `POST /api/admin/trash/{hash}/restore` | `adminTrashRestore` | New: calls `RestoreFileByHash`. |

---

## `/files/*` soft-delete gate

The static file server has no DB awareness. Wrap it with a middleware that:

1. Extracts the first path segment after `/files/`.
2. If it matches a 64-char hex hash: look up the file in the DB.
   - If `deleted_at IS NOT NULL` and the requester lacks `file.delete`
     permission → `404 Not Found`.
3. If the segment is anything else (e.g. `images/...`): pass through unchanged.

Admins hold `file.delete` and pass through, so the Trash tab can use standard
`/files/<hash>/<filename>` URLs for in-browser playback.

---

## Audit log

Use distinct action names so the audit trail is unambiguous:

| Action | Trigger |
|---|---|
| `file.trash` | Soft delete (move to trash) |
| `file.delete` | Hard delete from trash (permanent) |
| `file.restore` | Restore from trash |

---

## Admin web UI (`webui/html/admin.html`)

- Rename the existing "Delete" button to **"Move to Trash"** on all file rows.
- Add a **Trash** tab alongside the existing tabs (Library, Prune, Access).
  - Lists trashed files with: filename, size, deletion date.
  - Two actions per row: **Restore** and **Delete Forever**.
  - Audio player preview works via the normal `/files/<hash>/...` URL (admin
    auth passes the gate middleware).

---

## Test changes (`api/handlers_test.go`, `api/admin_handlers_test.go`)

- Rename `fakeRepo.DeleteFileByHash` stub → `SoftDeleteFileByHash`; add stubs
  for `HardDeleteFileByHash`, `RestoreFileByHash`, `ListTrashedFiles`.
- Update `TestAdminDeleteFile_Success` and `TestAdminDeleteFile_BlobAlreadyMissing`
  to assert soft-delete behaviour (blob still on disk, row still present with
  `deleted_at` set).
- Update `database_test.go` migration assertion: latest = `007`.
- Add tests for restore-on-reupload path in upload handler tests.

---

## TODO (deferred)

- **Auto-clear trash**: config key `[storage] trash_ttl_days = 0` (0 = disabled).
  Background goroutine sweeps files where
  `deleted_at < now() - trash_ttl_days * 86400` and hard-deletes them.
  Design when implementing: sweep interval, logging, whether a dry-run API is
  needed.
