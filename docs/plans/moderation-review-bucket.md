# Moderation Review Bucket — Staged Uploads + Approval Queue

Status: **Phases 1–3 implemented** (backend; track-edit.js extraction + the
"My uploads" tab on /upload, browser-verified; the /admin/moderation page +
admin nav/dashboard card + Trash pending-review badge). The Phase-4 auth.md
notes and the upload-rework.md pointer shipped earlier; the Phase-3 browser
pass (approve / return / discard as moderator) still needs a manual run.
Branch: aidev
Builds on: `docs/architecture/soft-delete.md` (Trash), `docs/architecture/auth.md`
(roles-only access model), `docs/plans/admin-files-rework.md` (the entity/track
edit UI being reused), `docs/plans/upload-rework.md` (which first sketched this
idea; its "Review bucket" section is superseded by this doc).

## Why

Today an upload goes live in the library the moment it lands. Two problems:

1. **No metadata staging.** Uploaders can't fix bad tags before the files
   appear publicly; tag fixes are an admin/moderator after-the-fact chore.
2. **No moderation.** An operator who doesn't fully trust every uploader has no
   way to review content before it becomes visible and playable.

This plan adds a two-stage pipeline: uploads land in the uploader's private
**"My uploads"** staging area, the uploader fixes metadata and clicks **Send to
approval**, and a moderator **approves** (→ library), **returns** (→ back to the
uploader with a short note), or **discards** (→ Trash) each submission.

## Decisions (locked, owner-approved 2026-06-10, revised 2026-06-11)

1. **No global switch. Trust is expressed through roles, not settings.** There
   is no "moderation on/off" toggle and no auto-approve setting. Everyone's
   uploads land in the staging area first — including admins and moderators.
   What differs per role is what *Send to approval* does (see #2). Rationale:
   the project already collapsed Layer-B into a roles-only model; a global
   toggle would be a parallel knob fighting the permission system, and it
   couldn't express "trust Alice but not Bob".
2. **One new permission: `content.moderate`** — act on submissions (approve /
   return-with-note / discard). Holders' own *Send to approval* publishes
   immediately (self-approve — they don't queue work for themselves on the
   moderation page). Granted to the built-in `admin` and `moderator` roles.
   **Moderators are the trusted uploaders** — no separate trusted-uploader
   role and no `upload.trusted` permission (owner revision 2026-06-11). To make
   that true out of the box, migration 017 also grants `file.upload` to the
   built-in moderator role (it lacks it today). Plain uploaders always go
   through the queue.
3. **Discard = move to Trash** (the existing soft delete), never a hard delete.
   A moderator who changes his mind restores from the existing `/admin/trash`
   page; restore returns the file to its **pre-trash review state** (so a
   discarded submission re-enters the queue, not the library). Trash stays its
   own page.
4. **Naming:** uploader-side tab = **"My uploads"** (on `/upload`); admin-side
   page = **`/admin/moderation`**, nav label **"Moderation"**.
5. **No withdraw.** Once submitted, the uploader cannot edit or recall a file;
   only a moderator action (return / approve / discard) moves it on. (Owner
   spec: "after that, he can't edit the files".)
6. **The moderation queue is grouped by uploader** (owner spec: "here should be
   differentiation by users"), and a return-note is per file (one note value,
   applied to every file in a selected batch; the My-uploads tab groups files
   sharing a note under one message box).

## State model

New column on `files` (migration **017**), orthogonal to `deleted_at`:

```sql
ALTER TABLE files ADD COLUMN review_state TEXT NOT NULL DEFAULT 'approved'
  CHECK (review_state IN ('draft','submitted','returned','approved'));
ALTER TABLE files ADD COLUMN review_note TEXT;      -- moderator's return message
ALTER TABLE files ADD COLUMN submitted_at INTEGER;  -- last transition to 'submitted'
CREATE INDEX idx_files_review ON files(review_state) WHERE review_state <> 'approved';
```

All existing rows backfill to `approved` via the column default. Who did what,
when is recorded in the existing `audit_log` (no `reviewed_by` column).

```
                         upload (new content)
                                  │
                                  ▼
        ┌─── uploader edits ──► draft ◄────────────────────┐
        │                         │ Send to approval        │ moderator returns
        │                         ▼                         │ (sets review_note)
        │  has content.moderate? ─┼── yes ──► approved      │
        │                         no                        │
        │                         ▼                         │
        └──────────────────── submitted ────────────────────┘
                                  │ moderator approves       (uploader edits again;
                                  ▼                           note shown above files)
                              approved  ──► in the library
```

- `returned` behaves like `draft` for the uploader (editable, resubmittable)
  but keeps the moderator's `review_note` until the next submit clears it.
- **Approve clears `review_note`.**
- **Discard** at any pending state = the existing soft delete (`deleted_at`
  set); `review_state` is *untouched*, which is exactly what makes Trash
  restore return the file to where it was (Decision #3).
- A moderator may act on both `submitted` and `returned` files ("can every
  time change his mind"), and may edit metadata himself (he holds
  `metadata.edit`) before approving. `draft` files are visible in the queue
  for awareness but carry no actions (the uploader hasn't asked for review yet).
- **Auth-unconfigured mode** (`Deps.Auth == nil`, pure-API/tests): no owner, no
  staging — inserts go straight to `approved`, preserving current behavior
  (same conditional pattern as playlists registration).

## Database layer

- `database/models.go` — `File` gains `ReviewState string`, `ReviewNote
  sql.NullString`, `SubmittedAt sql.NullInt64`; the trash list entry gains
  `ReviewState` (badge on the Trash page).
- **Visibility predicate.** Every public-facing listing/access query that today
  filters `f.deleted_at IS NULL` must also require `f.review_state =
  'approved'`. Introduce one shared SQL fragment constant (e.g.
  `const visibleFile = "f.deleted_at IS NULL AND f.review_state = 'approved'"`)
  and use it everywhere instead of scattering the longer condition:
  - `database/files.go` — `listFiles` (both WHERE branches).
  - `database/library.go` — all six drill-down/search WHEREs
    (lines ~28/80/142/198/235/278).
  - `database/access.go` — `FileAccessibleByHash` (guest path),
    `SetGuestPlayable` / `SetLicense` (operate on live+approved only is fine;
    moderation doesn't touch access flags).
  - `database/playlists.go` — add-item / favorite-toggle lookups (`…WHERE hash
    = ? AND deleted_at IS NULL` → also approved), the favorites listing, and
    the `unavailable` flag (a track is unavailable when trashed **or** no
    longer approved).
  - **Unchanged:** `ListFileRefs` (prune must keep treating pending blobs as
    referenced), `GetFileByHash` (upload dedupe needs every row), trash
    queries.
- New `Repository` methods (each breaks the api `fakeRepo` — known gotcha):
  - `ListUploadsByUser(ctx, userID)` — own non-trashed files with
    `review_state <> 'approved'` (+ metadata, state, note, timestamps).
  - `ListPendingReview(ctx)` — all non-trashed, non-approved files joined with
    uploader name, ordered for by-uploader grouping.
  - `UpdateReviewState(ctx, hash, from []state, to state, note *string)` —
    guarded transition (`UPDATE … WHERE hash=? AND deleted_at IS NULL AND
    review_state IN (…)`), returns found; sets/clears `review_note` and
    `submitted_at` as appropriate. One method with a transition table beats
    four near-identical ones.
  - `FileReviewInfo(ctx, hash)` — narrow `(review_state, uploaded_by,
    deleted_at)` lookup for the blob-gate and ownership checks.

## API

### Upload flow (`api/upload_handlers.go`)

- **New inserts**: `review_state = 'draft'` when auth is configured (else
  `approved`). Everything else (tag extraction, embedded-cover claim, entity
  resolution inside `InsertFile`) is unchanged — covers/entities for
  pending-only content are harmless because every listing now filters on
  approved (an entity with zero approved tracks simply doesn't appear).
- **Dedupe against a pending file** (same bytes, possibly another user): keep
  the existing dedupe path (record the filename, report `existed: true`) and
  add `"pending": true` to the response so the UI can say "already uploaded,
  awaiting moderation" instead of "already in library". Ownership stays with
  the first uploader. No metadata is echoed (it already isn't on the dedupe
  path).
- **`POST /api/files/check`**: the status enum gains a fourth value,
  `pending` (alongside `absent`/`present`/`trashed`). The endpoint is gated
  `file.upload`, so this leaks existence only to would-be uploaders — same
  posture as `trashed`. Client copy: "already uploaded, awaiting review".

### Uploader endpoints (gated `file.upload`, owner-scoped)

| Endpoint | Behavior |
|---|---|
| `GET /api/my/uploads` | Own staging list: hash, filename, tags, state, `review_note`, timestamps. |
| `PATCH /api/my/uploads/{hash}/metadata` | Same body/pointer semantics as the existing metadata patch, but authorized by **ownership + state** (`uploaded_by = me AND review_state IN ('draft','returned')`) instead of `metadata.edit`. Refactor `updateFileMetadata`'s core (tag update + entity re-resolution) into a shared helper both handlers call. |
| `POST /api/my/uploads/submit` | Body `{hashes: [...]}`. Transitions own `draft`/`returned` → `submitted` (clears `review_note`, stamps `submitted_at`). If the caller has `content.moderate` → straight to `approved` (audit as `file.approve` with `detail: self`). |

### Moderation endpoints (gated `content.moderate`, under `/api/admin`)

Moderators already pass the admin route-group's blanket `file.delete` gate
(the built-in moderator role holds `file.delete`), so these live with the rest
of `/api/admin/*`:

| Endpoint | Behavior |
|---|---|
| `GET /api/admin/moderation` | All non-trashed, non-approved files, grouped/groupable by uploader (uploader id + name included). |
| `POST /api/admin/moderation/{hash}/approve` | `submitted`/`returned` → `approved`; clears note; audit `file.approve`. |
| `POST /api/admin/moderation/{hash}/return` | Body `{note: "…"}` (required, short). `submitted` → `returned`; audit `file.return`. |
| *(discard)* | **No new endpoint** — reuse `DELETE /api/admin/files/{hash}` (soft delete → Trash). Batch actions loop the per-file endpoints client-side, matching the established entity-delete / trash-bulk convention (a server-side batch endpoint remains the shared follow-up noted in admin-files-rework). |

The existing `PATCH /api/files/{hash}/metadata` (gated `metadata.edit`) is what
moderators use to edit a submission themselves; verify it operates on
non-approved rows (it is hash-addressed and should not filter by state — add a
test).

### Blob access (`api.fileAccessGuard`)

Today identities with `content.access` (every logged-in role) bypass the
per-file check entirely, so a pending file's blob would be fetchable by anyone
who learns its hash. **Owner decision (2026-06-11): pending blobs are
fetchable by the uploader, moderator, and admin roles — i.e. any identity
holding `file.upload` or `content.moderate` — and 404 for everyone else
(listeners, anonymous).** No per-owner check: any uploader can fetch any
pending blob by its (unguessable, 64-hex) hash, not just his own. This is an
accepted, **documented-as-potentially-dangerous** behavior that may be
tightened later — the note goes in the regular docs
(`docs/architecture/auth.md`, content-access section), not only in this plan.

Implementation: after the `images` pass-through, do one narrow
`FileReviewInfo` lookup; if the file exists and `review_state <> 'approved'`,
require `file.upload` or `content.moderate`, else 404. Approved files keep
today's exact behavior (including the `content.access` pass — the extra
lookup is one indexed point query against local SQLite). The `file.upload`
pass is what lets an uploader preview his own staged tracks.

### Audit actions

`file.submit`, `file.approve` (detail `self` for trusted self-approve),
`file.return` (detail = note), plus the existing `file.trash` on discard.

## Permissions & roles (migration 017, continued)

```sql
-- new capability: moderate submissions (holders also self-approve own submits)
INSERT INTO role_permissions (role_id, permission) VALUES
  (1, 'content.moderate'),   -- admin
  (2, 'content.moderate');   -- moderator

-- moderators are the trusted uploaders: give the built-in moderator role
-- upload capability (it has none today)
INSERT OR IGNORE INTO role_permissions (role_id, permission) VALUES
  (2, 'file.upload');
```

No new role. `auth/auth.go` gains the `PermContentModerate` constant.
`docs/architecture/auth.md` gets the permission and the moderator-role change
added to its tables, **plus the "pending blobs are hash-fetchable by any
file.upload holder — potentially dangerous, may be tightened" note** (see the
blob-access section).

## Web UI

### `/upload` — "My uploads" tab (uploader side)

`upload.html` gets a two-tab header: **Upload** (the current page content) and
**My uploads (N)** — N = staged file count, fetched on init. The tab is part of
the same shell-native page/module (no new route). Tab content:

- Sections by state: **Returned** (each group headed by the moderator's note
  in a highlighted message box — files sharing the same note group under one
  box), **Drafts**, **Awaiting review** (read-only rows, no edit).
- Draft/returned rows: inline metadata edit (title / artist / album /
  album-artist) via a **shared track-edit module** (see "Code reuse"), preview
  playback, per-row and select-all checkboxes.
- A primary **"Send to approval"** button submits the selected (default: all)
  draft/returned files → `POST /api/my/uploads/submit`. For `content.moderate`
  holders the success toast says "published to the library" instead of "sent
  for review".
- After a successful upload batch on the Upload tab, nudge the user to the My
  uploads tab ("N files staged — review their metadata and send to approval").
- Preview playback: rows enqueue into the **shell player** like library rows do
  (the page is shell-native; the blob gate's owner-pass makes the URLs work).

### `/admin/moderation` (moderator side)

New admin page: `webui/html/admin/moderation.html` +
`webui/static/js/admin/moderation.js` (+ a small page sheet only if needed),
registered in `webui.go`'s admin routes/template block, added to the admin
shell nav and as a dashboard card with a pending count. Client-side gate:
visible/usable only with `content.moderate` (server enforces regardless).

- List grouped by **uploader** (collapsible sections, per Decision #6), state
  badges (`submitted` actionable, `returned` actionable, `draft` info-only).
- Per row: **preview play** (the page-local `player.js`, consistent with all
  admin pages staying outside the shell), **Edit** (shared track-edit module →
  existing `PATCH /api/files/{hash}/metadata`), **Approve**, **Return…** (modal
  with a required short note), **Discard** (two-step, → Trash).
- **Selection & bulk (revised after owner feedback 2026-06-11):** one global
  bulk toolbar (approve / return-with-one-note / discard selected, client-side
  loops) acting on a cross-group selection; a group-header checkbox selects an
  uploader's whole batch (works while collapsed), and a global select-all
  covers every uploader. Only `submitted` rows are selectable — **`returned`
  rows carry no checkbox**, so a bulk approve right after a return cannot
  republish the very files just sent back (they keep per-row actions for a
  deliberate change of mind).
- The Trash page rows gain a small "pending review" badge when a trashed row's
  `review_state <> 'approved'`, so a restored discard visibly re-enters the
  queue.

### Code reuse (owner point 4)

Extract the per-track metadata edit form/modal currently embedded in
`webui/static/js/admin/files.js` into a shared module, e.g.
`webui/static/js/track-edit.js` (form fields, pointer-semantics diffing, save
callback taking the endpoint to PATCH). Consumers: admin Files page
(unchanged behavior), the new Moderation page, and the My-uploads tab (which
points it at the owner-scoped endpoint). Toast/format helpers come from the
existing `admin/shared.js` / shared helpers — no re-implementation.

## Interactions reviewed

- **Auto-publish (autoderive guest_playable)** — composable, unchanged: a
  pending file may already carry a derived license/guest flag, but nothing is
  reachable until `approved` because every listing/access path filters on the
  shared predicate. (Exactly the composition the original sketch wanted.)
- **Trash-restore policy** (`reupload_restores` etc.) — **amended after an
  owner bug report (2026-06-11):** an *upload-initiated* restore (re-upload of
  trashed bytes under `reupload_restores`, or the explicit uploader-restore
  endpoint under `uploader_restore`) must not silently republish. When the
  restored file was `approved`, it is re-staged as the **restorer's draft**
  (`StageRestoredFile`: state → `draft`, note/submitted_at cleared,
  `uploaded_by` → restorer) so it lands in their My-uploads tab and passes
  review again — otherwise any `file.upload` holder could push any trashed
  file straight into the library by re-sending its bytes. Files trashed while
  *pending* keep their state/owner (they already re-enter the queue). The
  admin **Trash-page restore is unchanged** (prior-state restore, Decision
  #3) — that one is an explicit moderator action.
- **Prune** — `ListFileRefs` is intentionally state-blind; pending blobs are
  never "dangling". No change.
- **Playlists/favorites** — pending files can't be added (lookup now requires
  approved); a file that *leaves* the approved state (discard) shows as
  `unavailable`, same as trash today.
- **Embedded covers** — still claimed at upload time. A discarded draft can
  leave a cover on an otherwise trackless entity; invisible (no approved
  tracks) and self-healing when the entity gains tracks. Accepted; not worth a
  cleanup pass now.
- **Hash precheck** — `pending` status added; see API section.

## Phasing

1. **Backend** — migration 017 (columns + index + permissions + role), model +
   visibility predicate sweep, new Repository methods, upload-insert state,
   uploader + moderation endpoints, `fileAccessGuard` tightening, `checkFile`
   `pending` status, audit actions. Full test coverage (see below). The webui
   still works after this phase — existing libraries are all `approved`; new
   uploads stage but are manageable via the API.
2. **Uploader UI** — track-edit module extraction (refactor admin Files to use
   it, no behavior change), then the My-uploads tab on `/upload`.
3. **Moderation UI** — `/admin/moderation` page + admin nav + dashboard count +
   Trash badge.
4. **Docs** — update `docs/architecture/auth.md` (permissions/roles tables),
   replace the "Review bucket" sketch in `docs/plans/upload-rework.md` with a
   pointer here, CLAUDE.md route/table notes.

## Testing

- **Known gotchas:** migration 017 breaks the `database_test.go`
  version/table assertions; each new Repository method breaks the api
  package's `fakeRepo`.
- State machine: every legal transition + every illegal one (submit an
  approved file, return a draft, edit a submitted file as owner, approve as
  non-moderator → 403/404).
- Visibility: a `draft`/`submitted`/`returned` file is absent from
  `/api/files`, all drill-down/search endpoints, favorites, and playlist
  add; its blob 404s for a listener with `content.access` only, serves for
  `file.upload` / `content.moderate` holders (uploader, moderator, admin).
- Self-approve: a `content.moderate` holder's submit lands `approved`
  directly; plain uploader lands `submitted`.
- Return flow: note round-trips to `GET /api/my/uploads`; owner can edit and
  resubmit; resubmit clears the note.
- Discard/restore: discard → Trash; Trash restore re-enters the queue with the
  prior state (not the library).
- Dedupe/check: second upload of pending bytes reports `existed+pending`;
  `/api/files/check` returns `pending`.
- Auth-unconfigured (`NewRouter`) uploads land `approved` (existing tests keep
  passing).
- Browser pass: upload → My uploads → edit → send → moderate (approve /
  return-with-note / discard) → library visibility / Trash, as admin,
  moderator, plain uploader, listener. Webui assets are
  compile-time embedded — rebuild/restart between checks.

## Open questions (decide at impl, none blocking)

- Should `GET /api/my/uploads` also show the user's recently-approved files
  briefly (a "published ✓" section), or strictly the pending set? (Plan
  assumes strictly pending.)
- Exact grouping of return-notes in the My-uploads tab when a user has files
  returned at different times with different notes (plan: group by identical
  note text; ordering by return time).
- Whether the moderation queue should expose per-uploader counts on the admin
  dashboard card or just a global pending count.
