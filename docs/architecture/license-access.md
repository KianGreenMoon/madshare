# License-based guest access

**Status:** implemented (migration 008)
**Relates to:** `docs/architecture/auth.md` §5.1, §5.3

## Problem with the original design

The first implementation of the auto-derive policy worked by writing `guest_playable = 1`
to the `files` table whenever a matching license was set on a file. A separate
`ApplyAutoDerive` endpoint did the same in bulk over existing files.

This had a correctness problem: the stored flag outlived the policy. Disabling
auto-derive or removing a license from the allowlist had no effect on files that
had already been granted — those files stayed guest-accessible until an admin
manually toggled each one. The flag was also written silently on every
`SetLicense` call, making `guest_playable` a mix of two semantically different
things: explicit admin decisions and implicit policy side effects.

## New design: live query-time check

`guest_playable` now means only one thing: **the admin explicitly allowed this
file for anonymous access** (set via `POST /api/admin/files/:hash/guest`).
License-based access is never written to the database; it is evaluated at query
time as an additional OR branch inside `accessClause`.

The `accessClause` SQL predicate (in `database/access.go`) is the **guest
predicate**, shared by every guest-filtered query (`FileAccessibleByHash`,
`ListFilesGuest`, `ListArtistsGuest`, `ListAlbumsByArtistGuest`,
`ListTracksByAlbumArtistGuest`, `SearchGuest`). It decides what an anonymous /
capability-less request may reach; callers holding `content.access` bypass it
entirely. It takes no bind parameters and has two branches:

```sql
(
  -- 1. Explicit admin grant.
  f.guest_playable = 1

  -- 2. License-based: live check against the current policy in settings.
  --    guest_playable_manual = 0 means the admin has never touched this file's
  --    guest flag, so the policy may apply. If the admin set it explicitly
  --    (to either true or false), their decision wins.
  OR (
    f.guest_playable_manual = 0
    AND f.license IS NOT NULL AND f.license != ''
    AND EXISTS (SELECT 1 FROM settings WHERE key = 'access.autoderive.enabled' AND value = '1')
    AND ',' || COALESCE((SELECT value FROM settings WHERE key = 'access.autoderive.licenses'), '') || ','
        LIKE '%,' || f.license || ',%'
  )
)
```

The LIKE trick (`',' || list || ',' LIKE '%,' || value || ',%'`) is safe here
because the license vocabulary contains no commas.

## Consequences

**Immediate revocation.** Disabling the policy, or removing a license from the
allowlist, takes effect on the next request with no bulk update. There is no
stale state.

**`SetLicense` is now a pure metadata write.** It stores the license string and
returns. There are no access side effects.

**`ApplyAutoDerive` is gone.** The bulk-grant endpoint no longer exists. The
admin UI "Save & apply now" button has been removed; "Save policy" is the only
action.

**Manual override semantics are preserved.** `guest_playable_manual = 1`
indicates the admin explicitly set the file's guest flag. The license branch is
skipped for such files, so the admin's decision — whether grant or deny — always
wins over the policy. A typical new file has `guest_playable_manual = 0`
(default), so the policy applies freely.

## Migration 008

```sql
-- Clear guest_playable flags written by the old auto-derive mechanism.
-- guest_playable_manual = 0 means no explicit admin decision: these flags
-- are now redundant since access is derived live at query time.
UPDATE files SET guest_playable = 0 WHERE guest_playable_manual = 0 AND guest_playable = 1;
```

This is a data-only migration (no schema changes). After it runs, `guest_playable = 1`
only ever exists on files where `guest_playable_manual = 1`.

## Future: per-origin trust for federation

The policy currently applies uniformly to all files regardless of upload origin.
When federation is implemented, an uploader from a remote server could embed a
free license in a file's tags, which would satisfy the allowlist and expose the
file to guests.

The intended resolution is a per-origin trust flag: the admin can trust their
own server's uploaders for license-based access, but not files fetched from
federated peers. The `accessClause` license branch would add an origin condition
(e.g. `AND f.origin = 'local'` or a per-federation trust setting). Tracked in
`.claude/open-issues.md`.
