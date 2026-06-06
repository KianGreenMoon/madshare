# Authentication & authorization

**Status:** implemented through Phase 3 (content access); federation (Phase 4) deferred
**Builds on:** the listener/route-group architecture (`docs/architecture/listeners-and-config.md`)
**Concept source:** `madshare.org` (server-side access rules, default-deny, sharing scopes)

## 1. Goals & scope

From `madshare.org` and the planning discussion:

- One bootstrap **admin**; admins create other users.
- Users cannot upload, delete, or edit metadata without the matching permission.
- **Default-deny on reads:** unauthorized users may not play or download media,
  *except* files explicitly marked guest-playable (open / non-copyrighted).
- Per-file **`license`** metadata and an opt-in policy to auto-derive the
  guest-playable flag from it.
- **Custom access groups** deciding who may listen to what (artist / album / file).
- Federation (server-to-server trust) is acknowledged but **deferred** — the
  model must not preclude it.

Two clients exist today (browser web UI, programmatic API callers) and a third
later (peer servers). The design serves the first two now.

## 2. The two-layer model

Keep these orthogonal — conflating them tangles the schema:

- **Layer A — Capabilities** (*what actions you may perform*, global): upload,
  delete, edit metadata, manage users/roles, manage federation, play, download.
  Modelled as **fine-grained permissions bundled into roles**.
- **Layer B — Content access** (*what media you may see/play*, per-resource):
  ACL grants from access-groups to content scopes, plus the guest-playable flag
  (a grant to the "anonymous public" principal).

A user with the `listener` role (Layer A: may play/download *in principle*) can
still only reach the specific albums/artists/files that Layer B grants them.
Both checks must pass.

## 3. Authentication

### 3.1 Mechanisms

| Client | Mechanism |
|---|---|
| Browser (web UI) | **Session cookie** — opaque random token, server-side & revocable. `HttpOnly`, `SameSite=Lax`, `Secure` when served over TLS. Same-origin (per the listener work) means the cookie rides along on `/files/*` stream requests with no extra plumbing. |
| API / native clients | **Personal access tokens** — `Authorization: Bearer <token>`. |
| Peer servers | server keypair, signed requests — **deferred** (§8). |

Both cookie and token resolve to the same `(user, permissions)` identity, so
authorization downstream is mechanism-agnostic.

### 3.2 Password hashing

**argon2id** (`golang.org/x/crypto/argon2`), per-user random salt, tuned params,
stored as a self-describing encoded string (algo + params + salt + hash) so
params can be raised later without breaking existing hashes. Not bcrypt.

### 3.3 Sessions & tokens — storage

- **Sessions:** the cookie carries a 256-bit random value; the DB stores only its
  SHA-256 hash (so a DB leak doesn't yield live sessions). Row holds `user_id`,
  `created_at`, `expires_at`, `last_seen_at`. Logout deletes the row; "log out
  everywhere" deletes all rows for a user. Sliding or fixed expiry (start fixed,
  e.g. 30 days).
- **API tokens:** raw form shown to the user **once** (e.g. `mad_<base64url>`); DB
  stores the SHA-256 hash, plus `name`, `created_at`, `last_used_at`,
  `expires_at` (nullable), `revoked_at` (nullable).

### 3.4 First-admin bootstrap (first-run-only)

Read an initial admin credential from config/env, used **only when the `users`
table is empty**:

```toml
[auth]
initial_admin_user = "admin"
# Prefer the env var over writing the password into the file:
# MADSHARE_INITIAL_ADMIN_PASSWORD=...
initial_admin_password = ""
```

Startup logic:

1. If `users` is empty **and** an initial password is provided → create the admin
   user (role `admin`), set `password_change_required = true`. Log that it was
   created; never log the password.
2. If `users` is empty **and** no initial password → **fatal**: refuse to start.
   (An auth-gated server with no admin would be unusable; failing loud is safer
   than a half-open state.)
3. If `users` is non-empty → ignore the config value entirely, and **warn** if
   `initial_admin_password` is still set (stale secret at rest).

This keeps the secret from being a permanent at-rest credential: it matters for
exactly one startup and is inert thereafter.

## 4. Layer A — RBAC (capabilities)

### 4.1 Permissions (code-defined constants)

A fixed set of permission strings lives in code (so typos are compile-checked and
the set is documented in one place):

```
user.manage         create/edit/disable/delete users, assign roles (IMPLEMENTED, §10.2a)
role.manage         create/edit roles
file.upload         upload new files
file.delete         delete any file        (owners may always delete their own uploads)
metadata.edit       edit media_metadata / license / guest_playable
library.share       set library-wide sharing scope (madnetwork/friends/none)
federation.manage   manage trusted peers (future)
content.play        stream media
content.download    download media
content.all         bypass Layer-B ACLs (see entire library)
```

### 4.2 Roles = bundles (seed data, extensible)

`roles` are named permission bundles; the three named roles are seed rows, and
custom roles are allowed later (`role.manage`).

| Role | Permissions |
|---|---|
| `admin` | all |
| `moderator` | `file.delete`, `metadata.edit`, `content.play/download/all` (+ federation approvals later) |
| `uploader` | `file.upload`, `content.play/download/all` |
| `listener` | `content.play`, `content.download`, `content.all` |

A user may hold multiple roles; effective permissions = union.

**`listener` = "may listen to the whole library."** As of migration `010`, the
built-in `listener` and `uploader` roles hold `content.all`, so any *authenticated*
user sees, plays and downloads the entire library — Layer-B grants are not needed
for them. This matches the owner's intent that "listener" is simply the
full-library listening role. Default-deny still applies to **anonymous**
(not-logged-in) requests, which see only guest-playable / free-licensed files.
Access groups + content grants (§5) remain for **custom roles** that deliberately
lack `content.all` (e.g. a future "restricted listener" who may reach only
certain artists/albums).

## 5. Layer B — content access

### 5.1 Guest-playable & license (per file)

New `files` columns (§6):

- `uploaded_by` — owner (nullable; null for pre-auth/federated files).
- `guest_playable` — bool, **default 0**. The explicit access decision: may the
  anonymous public stream/download this file?
- `license` — controlled-vocabulary metadata (e.g. `CC0-1.0`, `CC-BY-4.0`,
  `public-domain`, `all-rights-reserved`, `unknown`). Distinct from
  `guest_playable`; may be pre-filled from tags (unverified) or set by an editor.

**Auto-derivation policy (opt-in):** an admin setting names a free-license
allowlist. When enabled, any file whose `license` is on the allowlist is
guest-accessible. The check is live at query time — no writes to `guest_playable`
occur. Toggling the policy or its allowlist takes effect immediately. A manual
override (`guest_playable_manual = 1`) always wins regardless of the policy.
The admin opts in and owns the legal risk (tag-sourced licenses are unverified).
See `docs/architecture/license-access.md` for implementation details.

### 5.2 Access groups & grants (ACLs)

```
access_groups(id, name)
access_group_members(group_id, user_id)
content_grants(id, group_id, scope_type, scope_artist, scope_album, scope_file_id)
   scope_type ∈ {all, artist, album, file}
```

`scope_type=all` grants the whole library to the group; `artist`/`album` match by
the metadata identifiers the library already uses; `file` targets one file id.

### 5.3 The access decision

For a request to play/download file *F*:

- **Anonymous:** allowed iff `F.guest_playable = 1` (explicit admin grant) **or**
  the auto-derive policy is enabled and `F.license` is on the allowlist and no
  explicit override is set (`guest_playable_manual = 0`).
- **Authenticated user U:** allowed iff U has `content.play` (or `content.download`)
  **and** one of: U holds `content.all`; *F* is guest-accessible (either branch
  above); or some group U belongs to has a `content_grant` covering *F* (`all`,
  or matching artist/album, or that file). Otherwise **deny** (404, not 403, to
  avoid leaking existence).

The same predicate filters library listings (`/api/artists|albums|tracks`) so
users only see what they may reach.

**Soft-deleted (trashed) files** are an additional invisible class: a file with
`deleted_at IS NOT NULL` is excluded from all listings and blocked at
`/files/*` for any identity that lacks `content.all`, regardless of the access
group state. Identities holding `content.all` (admin, moderator) pass through
so the Trash tab can preview them. See `docs/architecture/soft-delete.md`.

## 6. Schema (new migration)

A new `database/migrations/003_auth.sql` (sketch; the migration runner still owns
`schema_migrations`):

```sql
CREATE TABLE users (
  id                       INTEGER PRIMARY KEY,
  username                 TEXT    NOT NULL UNIQUE,
  password_hash            TEXT    NOT NULL,      -- argon2id encoded string
  password_change_required INTEGER NOT NULL DEFAULT 0,
  disabled                 INTEGER NOT NULL DEFAULT 0,
  created_at               INTEGER NOT NULL
);

CREATE TABLE sessions (
  token_hash   TEXT    NOT NULL PRIMARY KEY,      -- sha256 of cookie value
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL
);
CREATE INDEX idx_sessions_user ON sessions(user_id);

CREATE TABLE api_tokens (
  id           INTEGER PRIMARY KEY,
  user_id      INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name         TEXT    NOT NULL,
  token_hash   TEXT    NOT NULL UNIQUE,           -- sha256 of raw token
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER,
  expires_at   INTEGER,
  revoked_at   INTEGER
);

CREATE TABLE roles (
  id       INTEGER PRIMARY KEY,
  name     TEXT    NOT NULL UNIQUE,
  built_in INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE role_permissions (
  role_id    INTEGER NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
  permission TEXT    NOT NULL,
  PRIMARY KEY (role_id, permission)
);
CREATE TABLE user_roles (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  role_id INTEGER NOT NULL REFERENCES roles(id) ON DELETE CASCADE,
  PRIMARY KEY (user_id, role_id)
);

CREATE TABLE access_groups (
  id   INTEGER PRIMARY KEY,
  name TEXT    NOT NULL UNIQUE
);
CREATE TABLE access_group_members (
  group_id INTEGER NOT NULL REFERENCES access_groups(id) ON DELETE CASCADE,
  user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  PRIMARY KEY (group_id, user_id)
);
CREATE TABLE content_grants (
  id           INTEGER PRIMARY KEY,
  group_id     INTEGER NOT NULL REFERENCES access_groups(id) ON DELETE CASCADE,
  scope_type   TEXT    NOT NULL,                  -- all|artist|album|file
  scope_artist TEXT,
  scope_album  TEXT,
  scope_file_id INTEGER REFERENCES files(id) ON DELETE CASCADE,
  created_at   INTEGER NOT NULL
);

-- Layer-B columns on the existing files table.
ALTER TABLE files ADD COLUMN uploaded_by    INTEGER REFERENCES users(id);
ALTER TABLE files ADD COLUMN guest_playable INTEGER NOT NULL DEFAULT 0;
ALTER TABLE files ADD COLUMN license        TEXT;

-- Seed built-in roles + their permissions (admin/moderator/uploader/listener).
```

`audit_log` (§9) is a candidate for the same migration.

## 7. Enforcement on the listener/route-group architecture

Authorization is **middleware composed in `buildHandler`** (`madshare.go`),
inserted into the chain after `Recoverer`/`CORS` and before the route groups:

1. **`identify`** (always runs, never rejects): resolves the session cookie or
   Bearer token to a user and stashes `(user, permissions)` — or "anonymous" — in
   the request context.
2. **`requirePermission(perm)`**: per-route guard; 401 if anonymous, 403 if the
   permission is absent. This is what fills the existing `// r.Use(adminGate)`
   TODO in `api.RegisterAdmin`, and wraps the upload (`file.upload`), delete
   (`file.delete`), and metadata (`metadata.edit`) handlers.
3. **Content-access check**: per-resource, inside the read/stream/list handlers,
   applying the §5.3 predicate.
4. **CSRF**: state-changing requests authenticated *by cookie* must carry a CSRF
   token (double-submit cookie, or a required custom header that simple
   cross-site form posts can't set). Bearer-token requests are exempt (the token
   isn't ambient). This is the concrete answer to the CORS question parked in the
   listeners doc: with same-origin cookies, tighten CORS and add CSRF.

The route-group `serve` lists remain **topology**; this middleware is the actual
protection.

Login/logout/token endpoints (new, in the `api` group):
`POST /api/auth/login`, `POST /api/auth/logout`, `POST /api/auth/password`,
`GET/POST/DELETE /api/auth/tokens`. The web UI gains a login page.

## 8. Federation (deferred)

Not designed here, but the model leaves room: a server identity keypair, a
trusted-peer table, signed inter-server requests, and file **provenance**
(`uploaded_by` null + an origin-server reference) feeding the org doc's
approve/disapprove spam controls. Layer B's "anonymous public" and library
sharing scope (madnetwork / friends / none) are the hooks federated visibility
will extend.

## 9. Additional concerns

- **Audit log** of privileged actions (deletes, prunes, user/role/group changes,
  later federation approvals): `audit_log(id, actor_user_id, action, target,
  detail, created_at)`.
- **Brute-force protection / rate limiting** on `/api/auth/login` and on auth
  failures generally.
- **Credential lifecycle**: forced first-login password change, password change,
  token issue/revoke/expiry, session expiry, "revoke all sessions."
- **Library-wide sharing scope** (`library.share`): madnetwork / friends / none,
  sitting above per-user ACLs — primarily a federation concern (§8).

## 10. Phasing

1. **Authentication** — **IMPLEMENTED** (`auth/` package, migration
   `003_auth.sql`, `/api/auth/*`, admin-page login UI). `users`/`sessions`/
   `api_tokens`, argon2id, first-run admin bootstrap, `Identify` + login/logout/
   me/password/token endpoints, web UI login. The `admin` route group is gated by
   `RequirePermission(file.delete)`. *No read-behavior change.* Deviation from the
   strict phasing: the RBAC tables (`roles`/`role_permissions`/`user_roles`) and
   seeded built-in roles were created in this phase's migration too, so the admin
   gate uses a real permission rather than a throwaway flag — Phase 2 is then pure
   enforcement (no new schema). The change-password UI (incl. the forced
   first-run flow) is implemented.
2. **RBAC (Layer A)** — **IMPLEMENTED** (enforcement; schema already seeded in
   Phase 1). Migration `004_uploaded_by_audit.sql` adds `files.uploaded_by` and
   the `audit_log` table. Gating via `Deps.protect(perm)` (a pass-through when
   `Auth` is unset, so `NewRouter`/tests stay open; active in `buildHandler`
   where `Identify` runs): upload → `file.upload`, cover-image edit →
   `metadata.edit`, admin delete/prune → `file.delete`. Uploads record the
   uploader (`uploaded_by`) and an `audit_log` row; deletes/prunes/image edits
   are audited too (best-effort: an audit write failure logs but never fails the
   action). The library page's upload modal surfaces a 401/403 with a "sign in
   on the Admin page" message. **Deferred within this phase:** the owner-can-
   delete-own rule (delete is currently `file.delete`-only — delete any), and
   login rate limiting + CSRF.

   **(2a) User administration** — **IMPLEMENTED** (`api/user_handlers.go`,
   gated by `user.manage`): full account lifecycle from the admin page's *Users*
   section.
   - `GET /api/admin/users` — list users with their roles, disabled flag, and
     `password_change_required` (also feeds the access-group membership picker).
   - `GET /api/admin/roles` — the assignable roles (the four seeded built-ins).
   - `POST /api/admin/users` — create `{username, password, roles?,
     require_password_change?}`. **Default role is `listener`** (play + download)
     — the common "add a regular listening account" case. Username must match
     `^[A-Za-z0-9][A-Za-z0-9._-]{2,31}$`; password ≥ 8 chars; unknown role → 400;
     duplicate username → 409.
   - `PATCH /api/admin/users/{id}` — change role set and/or `disabled` (both
     optional; pointer-decoded so "absent" ≠ "false").
   - `POST /api/admin/users/{id}/password` — admin password reset.
   - `DELETE /api/admin/users/{id}` — delete (sessions/tokens/roles cascade).

   New DB methods on `*database.DB` (`database/auth.go`): `GetUserByID`,
   `ListRoles`, `AllUserRoles` (one query for all memberships), `SetUserRoles`
   (transactional replace), `SetUserDisabled`, `DeleteUser`,
   `CountEnabledUsersWithRole`. The endpoints' store needs are added to
   `api.ManageStore`.

   **Lock-out guards** (server-enforced, mirrored in the UI): a caller cannot
   delete or disable their **own** account, and the **last enabled `admin`**
   cannot be deleted, disabled, or demoted (`CountEnabledUsersWithRole`).
   Disabling a user and resetting a password both **revoke that user's active
   sessions** immediately (`DeleteUserSessions`). Every action writes an
   `audit_log` row (`user.create|roles|disabled|password|delete`).

   **Deferred:** username rename (unique-constraint + live-session implications),
   and custom roles via `role.manage` (still future).
3. **Content access (Layer B)** — **IMPLEMENTED**:
   - **Done (3a, migration 005):** `access_groups`/`access_group_members`/
     `content_grants` tables, `files.guest_playable`/`license` columns, the §5.3
     access predicate (`database.FileAccessibleByHash`) + management DB methods.
   - **Done (3b):** **default-deny flipped** on `/files/*` play/download via
     `Deps.fileAccessGuard` (content.all bypass, guest_playable, or a group grant;
     404 on denial; cover images not gated; pass-through when auth unconfigured).
   - **Done (3c, migration 006):** management API under `/api/admin/access/*`
     (groups/members/grants — gated by `user.manage`), per-file `/api/admin/files/
     {hash}/{guest,license}` (gated by `metadata.edit`), and the auto-derive
     policy at `/api/admin/settings/autoderive` (`user.manage`). **Listing
     endpoints are now access-filtered**: `/api/files|artists|albums|tracks` use
     `*Filtered` repo queries for non-`content.all` identities (anonymous sees
     only guest-playable), pass-through when auth is unconfigured. The opt-in
     license→guest **auto-derivation** is a DB-backed `settings` key/value policy
     (`access.autoderive.*`); it only ever *grants*, skips files whose
     `guest_playable` was set manually (`files.guest_playable_manual`), and runs
     on a license change or via an explicit "apply now" sweep (`ApplyAutoDerive`).
     The admin page gained an Access-groups section, per-file guest/license
     controls in the files table, and an auto-publish policy panel (all gated by
     the signed-in user's permissions). Auto-derivation storage/permission choices
     were settled with the owner during implementation (DB setting + admin UI;
     `user.manage` for group administration).
   - **Note (still deferred):** a file's uploader is not auto-granted play access
     (only content.all / grant / guest) — revisit owner-play if desired.
   - **(3d, migration 010):** the built-in `listener` and `uploader` roles were
     granted `content.all`, so any authenticated user sees the full library
     (§4.2). This makes access-group grants a no-op for the built-in roles;
     they remain meaningful only for custom roles without `content.all` and for
     constraining nobody else by default. Anonymous default-deny is unchanged.
4. **Federation authn** (future).

## 11. Decisions (settled with the owner)

1. **Auth mechanism:** session cookies (browser) + API tokens (programmatic). ✅
2. **First admin:** config/env initial password, **first-run-only** (created only
   when no users exist; ignored + warned thereafter). ✅
3. **Permission model:** fine-grained permissions bundled into roles (named roles
   are seed bundles; custom roles possible later). ✅
4. **Default-deny timing:** Phase 3, alongside the guest-flag/ACL layer — the
   first point where the open-music exceptions can actually be granted. ✅

Still open (revisit during implementation):

- Session expiry policy (fixed vs sliding) and default lifetime.
- `file.delete` granularity (`delete.own` vs `delete.any`) — proposal: single
  `file.delete` = delete any, plus an implicit owner-can-delete-own rule.
- CSRF strategy (double-submit cookie vs required custom header).
