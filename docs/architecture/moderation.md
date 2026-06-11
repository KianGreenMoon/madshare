# Moderation — Staged Uploads & Review Queue

Every upload stages privately before it can reach the library: the uploader
fixes metadata in **"My uploads"** (on `/upload`) and clicks *Send to
approval*; a moderator **approves** (→ library), **returns** (→ back to the
uploader with a note), or **discards** (→ Trash) each submission. Trust is
expressed through roles, not a global switch — `content.moderate` holders'
own submissions publish immediately (self-approve).

This is the reference for the shipped system. Design history and the original
decision record: `docs/plans/moderation-review-bucket.md`.

---

## State model

`files.review_state` (migration **017**), orthogonal to `deleted_at`:

```sql
ALTER TABLE files ADD COLUMN review_state TEXT NOT NULL DEFAULT 'approved'
  CHECK (review_state IN ('draft','submitted','returned','approved'));
ALTER TABLE files ADD COLUMN review_note TEXT;      -- moderator's return message
ALTER TABLE files ADD COLUMN submitted_at INTEGER;  -- last transition to 'submitted'
CREATE INDEX idx_files_review ON files(review_state) WHERE review_state <> 'approved';
```

Pre-017 rows backfilled to `approved` via the column default — a migrated
library is unchanged.

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

- **`draft`** — editable by the owner; visible to moderators for awareness
  only (no queue actions — the uploader hasn't asked for review yet).
- **`submitted`** — locked for the owner; actionable in the queue.
- **`returned`** — behaves like `draft` for the uploader (editable,
  resubmittable) and keeps the moderator's `review_note` until the next
  submit clears it. Moderators may still act on it per row (change of mind),
  but returned files are **excluded from bulk selection** on the moderation
  page — a bulk approve right after a return must not republish the very
  files just sent back.
- **Approve clears `review_note`.** Submit stamps `submitted_at`.
- **No withdraw**: once submitted, only a moderator action moves the file on.
- **Discard** = the existing soft delete (`docs/architecture/soft-delete.md`);
  `review_state` is untouched, which is what makes a Trash restore return the
  file to where it was (queue, not library).
- **Auth-unconfigured mode** (`Deps.Auth == nil`, pure-API/tests): no staging —
  inserts go straight to `approved`, preserving pre-moderation behavior.

All transitions run through one guarded UPDATE
(`database.UpdateReviewState` with a `ReviewTransition`: allowed `From` set,
target state, optional note/owner guard/`submitted_at` stamp), so concurrent
requests cannot double-apply.

## Visibility

One shared SQL fragment (`database/files.go`):

```go
const visibleFile = "f.deleted_at IS NULL AND f.review_state = 'approved'"
```

Every public-facing listing and access path filters on it: `/api/files`, all
drill-down/search endpoints (`database/library.go`), the guest predicates,
favorites and playlist-add lookups (a track that leaves `approved` shows as
`unavailable`, same as trash). Entities (artists/albums) holding only pending
tracks simply don't appear — their `track_count` counts approved tracks.

Deliberately **state-blind**: `ListFileRefs` (prune must treat pending blobs
as referenced), `GetFileByHash` (upload dedupe needs every row), and the trash
queries.

**Blob access** (`api.fileAccessGuard`): a pending file's blob serves only to
identities holding `file.upload` or `content.moderate` and 404s for everyone
else — including `content.access`-only listeners. The check is *not*
owner-scoped; see the warning in `docs/architecture/auth.md` §5
(owner-accepted, may be tightened later).

## Permissions & roles

One permission: **`content.moderate`** — act on submissions, and self-approve
one's own *Send to approval*. Granted to the built-in `admin` and `moderator`
roles; migration 017 also gave `moderator` the `file.upload` permission:
**moderators are the trusted uploaders** (their uploads still stage in "My
uploads", but submitting publishes immediately). Plain uploaders always go
through the queue. Tables: `docs/architecture/auth.md` §4.

## Endpoints

### Uploader side (gated `file.upload`, owner-scoped, registered only with auth configured)

| Endpoint | Behavior |
|---|---|
| `GET /api/my/uploads` | The caller's staged files (any state but `approved`, non-trashed), newest first: hash, filename, tags, `state`, `note`, timestamps. |
| `PATCH /api/my/uploads/{hash}/metadata` | Tag edit with the same body/pointer semantics as `PATCH /api/files/{hash}/metadata`, but authorized by **ownership + editable state** (`draft`/`returned`) instead of `metadata.edit`. 404 on anything the caller may not edit (reveals nothing about other users' staged files). |
| `POST /api/my/uploads/submit` | Body `{"hashes": [...]}`. Each owned `draft`/`returned` file → `submitted` (clears the note, stamps `submitted_at`); for `content.moderate` holders → straight to `approved`. Per-hash `results`; a non-eligible hash reports `ok: false` without failing the batch. Response `approved: true` signals self-approve. |
| `DELETE /api/my/uploads/{hash}` | The owner removes one of his own **editable** staged files (`draft`/`returned` → Trash, the regular soft delete; an admin can restore it). `submitted` files cannot be removed — no withdraw once sent to approval. 404 on anything the caller may not remove. Audit: `file.trash` / `owner-discard`. |

### Moderator side (gated `content.moderate`, under `/api/admin`)

| Endpoint | Behavior |
|---|---|
| `GET /api/admin/moderation` | Every staged (non-trashed, non-approved) file with uploader id + name, ordered for by-uploader grouping. |
| `POST /api/admin/moderation/{hash}/approve` | `submitted`/`returned` → `approved`; clears the note. |
| `POST /api/admin/moderation/{hash}/return` | Body `{"note": "…"}` (required, ≤ 1000 bytes). `submitted`/`returned` → `returned` with the note. |
| *(discard)* | No distinct endpoint — `DELETE /api/admin/files/{hash}` (soft delete → Trash). Bulk actions loop the per-file endpoints client-side. |

Moderators edit a submission's tags via the regular
`PATCH /api/files/{hash}/metadata` (`metadata.edit`) — it is hash-addressed
and does not filter by review state.

Upload-flow integration (`POST /files/upload`, `POST /api/files/check`,
`pending`/`restored` response fields): `docs/api/upload.md`.

## Restores must not bypass review

Restoring a trashed file brings back whatever `review_state` it had — which
for a previously approved file means *live*. Who initiates the restore
therefore matters:

- **Library → Trash scope** (`POST /api/admin/trash/{hash}/restore`) —
  prior-state restore, unchanged. An explicit moderator action; a discarded
  submission visibly re-enters the queue (Trash badges such rows "pending
  review").
- **Upload-initiated restores** — a re-upload of trashed bytes under the
  `reupload_restores` policy, or `POST /api/files/{hash}/restore` under
  `uploader_restore` — would otherwise let **any `file.upload` holder publish
  any trashed file** by re-sending its bytes. With auth configured, such a
  restore of an `approved` file is demoted to the **restorer's draft**
  (`database.StageRestoredFile`: state → `draft`, note/`submitted_at`
  cleared, `uploaded_by` → restorer), so it lands in *their* "My uploads" and
  passes review again. Files trashed while *pending* keep their state and
  owner — they re-enter the queue where they were.

## Audit actions

| Action | When | Detail |
|---|---|---|
| `file.submit` | uploader sends to approval | — |
| `file.approve` | moderator approve, or trusted self-approve | `self` for self-approve |
| `file.return` | moderator returns | the note |
| `file.trash` | discard (the regular soft delete) | filenames |
| `file.restore` | any restore | `restore-via-reupload (re-staged as draft): …` / `uploader-restore (re-staged as draft)` mark demoted restores |

## Web UI

- **`/upload` → "My uploads" tab** (shell-native, the shared file-management
  component in owner mode — `docs/architecture/file-management-view.md`):
  state sections **Returned** (each row carries the moderator's note),
  **Drafts**, **Awaiting review** (read-only). Draft/returned rows edit via the
  shared `track-edit.js` modal (pointed at the owner-scoped PATCH), preview
  through the shell player, and can be **removed** (per-row inline confirm, or
  **Remove selected** in the bulk toolbar) → Trash via the owner-scoped DELETE. A *Send to approval* button submits the selection (toast says
  "published" for self-approvers). The upload pane itself has no album
  verify/edit cards — tag fixing happens here; folder cover images are still
  co-located and posted server-side for `metadata.edit` holders (headless).
- **`/admin/library` → "Review" scope** (the unified file-management view —
  `docs/architecture/file-management-view.md`; client-gated `content.moderate`,
  page-local shared player): the queue grouped by uploader in collapsible
  sections, state badges, per-row preview / Edit (shared `track-edit.js` →
  `metadata.edit` PATCH) / Approve / Return… / Discard. **Selection model:**
  one bulk toolbar (approve / return-with-one-note / discard selected) over a
  cross-group selection; a group-header checkbox selects an uploader's whole
  batch (works while collapsed). Only `submitted` rows are selectable. (The
  dashboard "Review" card deep-links here via `#review`.)
- **Dashboard** card with the pending count; **Library → Trash scope** rows whose
  `review_state <> 'approved'` carry a "pending review" badge (restore
  re-enters the queue).

## Repository surface (`database/`)

`ListUploadsByUser`, `ListPendingReview`, `UpdateReviewState` (+
`ReviewTransition`), `FileReviewInfo` (narrow state/owner/trash lookup for
the blob gate), `StageRestoredFile` (restore demotion), `DiscardOwnUpload`
(owner remove). Adding to `database.Repository` breaks the api package's
`fakeRepo` — the usual gotcha.
