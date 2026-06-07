# Plan: roles-only access model (drop Layer B)

**Status:** DONE (aidev, 2026-06-07) — both phases implemented and tested.
Decided ahead of the artist/album normalization because both touch the same
access-control SQL; doing this first removes that entanglement entirely.

## Decision

Madshare keeps **one** authorization concept: **roles = capabilities**. The
per-content authorization layer (**Layer B** — access groups + content grants) is
**removed**. There is no per-artist / per-album / per-file user restriction in
v0; an authenticated user with a content capability may reach the whole library.

This supersedes the "roles-vs-access-groups" open question in
`.issues/open-issues.md` and the two-layer model in
`docs/architecture/auth.md` §2/§4.2/§5.2/§5.3.

### What stays (NOT Layer B)

Anonymous (logged-out) access is unchanged and remains default-deny:

- `files.guest_playable` — explicit "the public may stream this" flag.
- `files.license` + the opt-in **license auto-derive** policy
  (`settings` keys, `licenseClause`, `guest_playable_manual`). See
  `docs/architecture/license-access.md`.

These are about the *anonymous principal*, not about access groups, so they are
kept verbatim. The admin per-file guest/license controls and the auto-derive
settings UI stay.

### What is removed

| Area | Removed |
|---|---|
| **DB tables** | `access_groups`, `access_group_members`, `content_grants` |
| **`database/access.go`** | `AccessGroup`/`ContentGrant`/`GroupMember` types and all their CRUD (`CreateAccessGroup`, `ListAccessGroups`, `DeleteAccessGroup`, `AddGroupMember`, `RemoveGroupMember`, `ListGroupMembers`, `AddContentGrant`, `ListContentGrants`, `DeleteContentGrant`); the Layer-B `EXISTS (… content_grants …)` branch of `accessClause` |
| **`database/repo.go`** | nothing in the `Repository` interface (groups live on a separate store) — but the `*Filtered` method signatures change (see below) |
| **`api/access_handlers.go`** | the group/grant handlers: `listGroups`, `createGroup`, `deleteGroup`, `addMember`, `removeMember`, `addGrant`, `deleteGrant`, and their store interface methods. **Keep** `listUsers`, `setGuest`, `setLicense`, `getAutoDerive`, `setAutoDerive` (those are users + anonymous-access, not Layer B) |
| **Web UI** | `webui/html/admin/access.html`, `webui/static/js/admin/access.js`, `webui/static/css/admin-access.css`; the "Access" nav entry in the admin shell/dashboard |
| **Permissions** | collapse `content.play` / `content.download` / `content.all` into a single `content.access` (no scoping left for `content.all` to bypass). Built-in roles keep "can reach the library" via this one permission |
| **Tests** | `database/access_mgmt_test.go`; the group/grant cases in `database/access_test.go` and `api/access_handlers_test.go`; `TestFilteredListings_RespectAccess` reduces to "anonymous sees only guest/free, authenticated sees all" |

## Target access decision

For a request to play/download/list file *F*:

- **Anonymous:** allowed iff `F.guest_playable = 1` **or** the auto-derive policy
  is on and `F.license` is allowlisted with no manual override. (Unchanged.)
- **Authenticated user with `content.access`:** allowed for any non-trashed file.
- **Authenticated user without `content.access`:** treated as the anonymous/guest
  set (sees only guest-playable / free-licensed files). Sensible fallback for a
  user holding zero content capability.

Trashed files (`deleted_at IS NOT NULL`) stay invisible to everyone except
holders of the delete/admin capability, exactly as today
(`docs/architecture/soft-delete.md`).

### Simplified `accessClause`

The clause loses its `content_grants` `EXISTS` branch **and its bind parameter**.
It becomes the guest/anonymous predicate only:

```
accessClause = (f.guest_playable = 1 OR <licenseClause>)
```

No `userID` argument. The handler chooses the path by capability instead of
passing a user id into SQL:

- identity has `content.access` → call the **full-library** query (no clause).
- otherwise (anonymous or capability-less) → call the **guest** query (clause).

So the `*Filtered(… userID sql.NullInt64)` methods become `*Guest(…)` with no
user-id parameter (they only ever serve the anonymous/guest set now). Update the
`Repository` interface, the four `library_handlers.go` call sites, `handlers.go`
(`ListFilesFiltered`), and the api-test `fakeRepo` accordingly.

> The capability-based path selection already exists (the `content.all` bypass);
> this plan just renames the permission and drops the per-user SQL branch.

## Phasing

Split into two always-green commits. The table DROP can't land while code still
queries those tables, so it moves into the second commit alongside the code/UI
removal — the migration that drops tables is therefore `012`, and the
artist/album work shifts to `013`.

### Phase 1 — permission collapse (DONE, commit on `aidev`)

1. **Migration `011_content_access_permission.sql`** — re-seed role permissions:
   delete `content.play`/`content.download`/`content.all` rows, insert
   `content.access` for the built-in roles 1–4. *Does not drop any table* (Layer B
   still works for non-built-in roles until Phase 2). `content.play`/`download`
   were never enforced as gates; only `content.all` was, at the file-access
   bypass.
2. **`auth` package** — the three content-permission constants collapse to
   `PermContentAccess = "content.access"`; the two bypass sites
   (`api/handlers.go` `accessFilter`, `api/api.go` file-access guard) now check
   `content.access`. Tests + comments updated; `database_test.go` migration
   version → 11.

### Phase 2 — remove Layer B (DONE)

3. **Migration `012_drop_access_groups.sql`** — `DROP TABLE content_grants;`
   `DROP TABLE access_group_members;` `DROP TABLE access_groups;` (lands together
   with the code below so nothing queries a dropped table).
5. **`database/access.go`** — strip the `content_grants` branch from
   `accessClause` and drop its bind param; delete the group/grant CRUD + types.
6. **`database/library.go` + `repo.go`** — rename `*Filtered`→`*Guest`, drop the
   `userID` param, simplify the queries (they no longer reference
   `access_group_members`/`content_grants`).
7. **`api`** — delete the group/grant handlers + their store interface methods
   from `access_handlers.go`.
8. **Web UI** — delete the Access page (html/js/css) and its nav entry; verify
   the admin shell and dashboard no longer link it.
9. **Docs** — rewrite `docs/architecture/auth.md` (collapse §2 to one layer,
   delete §5.2/§5.3 Layer-B parts, update §4 permissions table and §6 schema,
   drop the access-group rows). Keep §5.1 (guest/license). Update
   `docs/architecture/license-access.md` cross-refs.
10. **Tests** — remove Layer-B tests; bump the migration-count assertions in
    `database/database_test.go` (table-set + version change); update `fakeRepo`.

## Known test/inter-dependency gotchas

- Each new migration bumps the "latest migration" number asserted in
  `database/database_test.go` (per the migration-gotchas note). The Phase-2 `012`
  migration *drops* tables, so the table-set snapshot test changes too.
- Renaming `*Filtered`→`*Guest` and removing the group code ripples into the api
  `fakeRepo` and any test that constructs grants. Grep `content_grants`,
  `AccessGroup`, `*Filtered` before declaring Phase 2 done.
- `auth.RequirePermission(file.delete)` on the admin group is unaffected.

## Out of scope / deferred

- Per-content restriction can return later as a clean feature on top of the
  normalized artist/album IDs (a `content_grants` keyed by `album_id`/`artist_id`)
  if a real use case appears. Recorded as a future idea, not planned.
