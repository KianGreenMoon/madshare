# Moderation — Staged Uploads & Review Queue

Every upload stages privately before it can reach the library. An upload is a
**file plus an offered appearance** (a `tagsets` row — title/artist/album/… read
from its tags); the uploader fixes that appearance in **"My uploads"** (on
`/upload`) and clicks *Send to approval*, and a moderator **approves**
(→ library), **returns** (→ back to the uploader with a note), or **discards**
(→ Trash) each submission. Trust is expressed through roles, not a global
switch — `content.moderate` holders' own submissions publish immediately
(self-approve).

Since the recording-tagsets rework the reviewable unit is the **appearance
(tagset)**, not the file: one blob can carry several appearances (a byte-dup
upload attaches a new draft appearance to a held blob), and approving one
resolves duplication at the gate. The full model is
`docs/architecture/recording-tagsets.md`; this is the reference for the review
flow built on it.

---

## State model

The lifecycle columns live on **`tagsets`** (migration **024**), orthogonal to
the tagset's `deleted_at`:

```sql
review_state TEXT NOT NULL DEFAULT 'draft'
  CHECK (review_state IN ('draft','submitted','returned','approved')),
review_note  TEXT,      -- moderator's return message
submitted_at INTEGER,   -- last transition to 'submitted'
created_by   INTEGER REFERENCES users(id),  -- who offered this appearance
-- idx_tagsets_review ON tagsets(review_state) WHERE review_state <> 'approved'
```

(An upload's own file is created together with its primary tagset in one
transaction, so a submission always has exactly one appearance to act on until a
byte-dup adds more.)

```
                    upload (offers a draft appearance)
                                  │
                                  ▼
        ┌─── uploader edits ──► draft ◄────────────────────┐
        │                         │ Send to approval        │ moderator returns
        │                         ▼                         │ (sets review_note)
        │  has content.moderate? ─┼── yes ──► approved      │
        │                         no                        │
        │                         ▼                         │
        └──────────────────── submitted ────────────────────┘
                                  │ moderator approves    (uploader edits again;
                                  ▼                         note shown above rows)
                              approved  ──► in the library
```

- **`draft`** — editable by the owner; visible to moderators for awareness
  only (no queue actions — the uploader hasn't asked for review yet).
- **`submitted`** — locked for the owner; actionable in the queue.
- **`returned`** — behaves like `draft` for the uploader (editable,
  resubmittable) and keeps the moderator's `review_note` until the next
  submit clears it. Moderators may still act on it per row (change of mind),
  but returned appearances are **excluded from bulk selection** on the
  moderation page — a bulk approve right after a return must not republish the
  very appearances just sent back.
- **Approve clears `review_note`.** Submit stamps `submitted_at`.
- **No withdraw**: once submitted, only a moderator action moves it on.
- **Discard** = the tagset soft delete (`tagsets.deleted_at`;
  `docs/architecture/gc-model.md`); `review_state` is untouched, which is
  what makes a Trash restore return the appearance to where it was (queue, not
  library). Trashing never cascades — the blob and recording stay; a
  recording whose every appearance is trashed simply leaves the library
  (Trash › Recordings).
- **Auth-unconfigured mode** (`Deps.Auth == nil`, pure-API/tests): no staging —
  the offered appearance is created `approved`, preserving pre-moderation
  behavior.

The simple transitions run through one guarded UPDATE
(`database.UpdateReviewState` with a `ReviewTransition`: allowed `From` set,
target state, optional note/owner guard/`submitted_at` stamp), so concurrent
requests cannot double-apply. Approve is richer — see below.

## Classification & per-piece approve

Because a submission can be net-new audio, a new encoding of audio already held,
or a byte-identical re-upload, the queue **classifies** each row server-side so
the moderator sees what an approve would actually change. The resolver has
already grouped same-audio files onto one recording at upload time, so the case
is read off the submission's recording (`database.ClassifySubmission`, exposed at
`GET /api/admin/moderation/{tagsetID}/classify`) — no fingerprint pass here:

| Case | `class` | What arrived | Approve publishes… |
|---|---|---|---|
| **A** | `new_recording` | net-new audio | a fresh recording + its appearance |
| **B** | `new_appearance` | same audio, a distinct new blob | the appearance on the existing recording; the new blob is a candidate rendition |
| **C** | `no_new_bytes` | the blob is already a published rendition (byte-dup) | only the offered appearance |

`ClassifySubmission` also returns `CollidesAppearance` (the offered identity key
— album/album-artist/disc/track, NULL-safe — already exists as an approved
appearance on the recording: nothing new; deny/return is the natural action) and
a **ladder compare** (`CurrentBest` among the recording's *other* live renditions
vs `Submitted`, `SubmittedIsNewBest`) for case B.

The classification is a *suggestion*; the moderator validates and approves
per-piece via `database.ApproveSubmission(tagsetID, dropBytes, forceNew)`, one
transaction:

- **plain approve** — publish the appearance (`review_state → approved`).
- **`drop_bytes`** (case B) — after publishing, soft-remove the submitted
  rendition (`files.deleted_at`): *keep the appearance, drop the blob* —
  absorb-at-the-gate. The recording keeps serving its other renditions; dropping
  its only rendition makes it dormant.
- **`force_new`** — split the submitted blob into a new `recording_pinned`
  recording (its appearance moves with it, becomes primary) before publishing:
  the "this is actually new audio, the fingerprint match was wrong" override.
  **Ignored** when the blob already carries another approved appearance (a
  byte-dup shared blob — splitting would strand it; a byte-identical upload can't
  be "actually new").

The state machine (approve / return-with-note / discard as the terminal actions)
is unchanged — the rework adds the recording context and per-piece control on
approve.

## Visibility

Two shared SQL predicates, both in `database/`:

- **Tagset-rooted listings** (the library, search, playlists/favorites —
  `database/library.go`, `playlists.go`) filter on `visibleTagset`
  (`database/tagsets.go`): an appearance is public only when it is `approved`,
  not trashed, **and** its recording has ≥ 1 surviving (non-removed) rendition
  to play. A recording whose renditions are all removed keeps its tagsets but
  drops out of the library until one is restored.
- **Files-rooted surfaces** (`/api/files`, storage stats) filter on
  `visibleFile` (`database/files.go`): the file is not removed and its
  **representative** appearance (`reprTagset` — primary, else oldest; keeps a
  files surface 1:1 with the blob even after byte-dup drafts) is neither trashed
  nor pending. Entities (artists/albums) holding only pending appearances simply
  don't appear — their `track_count` counts approved appearances.

Deliberately **state-blind**: `ListFileRefs` (prune must treat pending blobs as
referenced), `GetFileByHash` (upload dedupe needs every row), and the trash
queries.

**Blob access** (`api.fileAccessGuard` → `Repo.BlobPubliclyVisible`): the gate is
now **recording-level** — a blob serves publicly iff its recording has ≥ 1
approved, non-trashed appearance. A blob with no such appearance (pending, or an
all-pending recording) serves only to identities holding `file.upload` or
`content.moderate` and 404s for everyone else — including `content.access`-only
listeners. The check is *not* owner-scoped; see the warning in
`docs/architecture/auth.md` §5 (owner-accepted, may be tightened later). This is
what keeps the moderator's preview able to play the *submitted* blob.

## Permissions & roles

One permission: **`content.moderate`** — act on submissions, and self-approve
one's own *Send to approval*. Granted to the built-in `admin` and `moderator`
roles; `moderator` also holds `file.upload`: **moderators are the trusted
uploaders** (their uploads still stage in "My uploads", but submitting publishes
immediately). Plain uploaders always go through the queue. Discard additionally
requires `file.delete`. Tables: `docs/architecture/auth.md` §4.

**Duplicate exception:** a submission whose audio is already in the library is
**never** self-approved — even for a `content.moderate` holder — and is flagged
in the queue. The flag is derived at submit time from the classification
(`ClassifySubmission` → `MatchedExisting`, i.e. case B or C), so a duplicate is
always routed to the queue for a human look.

## Endpoints

Rows are addressed by **`tagset_id`** (the appearance), not `hash` — a byte-dup
makes two rows share one blob hash, so the appearance is the identity. Each
listing item still carries `hash` too (the origin blob, for the preview URL and
admin ops).

### Uploader side (gated `file.upload`, owner-scoped, registered only with auth configured)

| Endpoint | Behavior |
|---|---|
| `GET /api/my/uploads` | The caller's staged appearances (any state but `approved`, non-trashed). **Paged** like the library (`?limit&offset&q&field&sort`, default `sort=state`); returns `{total, selectable_total, items}` — `selectable_total` is the editable (`draft`+`returned`) subset count. Each item: `tagset_id`, `hash`, filename, tags, `state`, `note`, timestamps. |
| `GET`/`PATCH /api/my/uploads/{tagsetID}/metadata` | Read / tag-edit of one owned appearance, authorized by **ownership + editable state** (`draft`/`returned`) instead of `metadata.edit`. Same patch body/pointer semantics as the file metadata PATCH, targeting the tagset. 404 on anything the caller may not edit (reveals nothing about other users' staged appearances). |
| `POST /api/my/uploads/submit` | Body `{"tagset_ids": [...]}`. Each owned `draft`/`returned` appearance → `submitted` (clears the note, stamps `submitted_at`); for `content.moderate` holders → straight to `approved` (a duplicate-flagged one always queues). A non-eligible id is skipped. Response `{submitted, approved, flagged, warning?}`; `approved: true` signals self-approve. Internally partitioned by the dup check into buckets, each a single batched transition + one summary audit row. |
| `POST /api/my/uploads/bulk` | Batch `{"action": "submit"\|"remove"\|"edit"\|"recode", "tagset_ids": [...] \| "filter": {q,field}, "all": bool}` over the owner's `draft`+`returned` set (`all:true` required when the filter term is blank). `submit` shares the `/submit` semantics; `remove` discards to Trash; `edit` writes one tag `patch` across the set (tags only — an access field is refused, it belongs to the recording); `recode` is the bulk charset fix. Every action is owner+state scoped in the DB call, so an id outside the caller's editable staging matches nothing. Backs "Select all N matching". |
| `DELETE /api/my/uploads/{tagsetID}` | The owner removes one of his own **editable** staged appearances (`draft`/`returned` → Trash; an admin can restore it). `submitted` cannot be removed — no withdraw. 404 on anything the caller may not remove. |

### Moderator side (gated `content.moderate`, under `/api/admin`)

| Endpoint | Behavior |
|---|---|
| `GET /api/admin/moderation` | Staged (non-trashed, non-approved) appearances with uploader id + name and the **classification** (`class`, `recording_id`, `collides`). **Paged** (`?limit&offset&q&field&sort`, default `sort=uploader`); returns `{total, selectable_total, items}` — `selectable_total` is the actionable (`submitted`) subset count. |
| `GET /api/admin/moderation/{tagsetID}/classify` | The full classification for one row: case + `recording_id` + `collides` + the ladder compare (`current_best` vs `submitted`, `submitted_is_new_best`). Fetched when the moderator expands a card. |
| `POST /api/admin/moderation/{tagsetID}/approve` | Body `{"drop_bytes": bool, "force_new": bool}` (empty = plain approve) → `ApproveSubmission`. Publishes the appearance and applies the per-piece decisions atomically. |
| `POST /api/admin/moderation/{tagsetID}/return` | Body `{"note": "…"}` (required, ≤ 1000 bytes). `submitted`/`returned` → `returned` with the note. |
| `POST /api/admin/moderation/{tagsetID}/discard` | Trash the appearance (tagset soft delete — keeps the blob and recording; the appearance re-enters the queue on restore). Additionally requires `file.delete`. |
| `GET`/`PATCH /api/admin/moderation/{tagsetID}/metadata` | Moderator edit of one appearance's tags (`metadata.edit`), tagset-addressed and not state-filtered. |
| `POST /api/admin/moderation/bulk` | Batch `{"action": "approve"\|"return"\|"discard", "tagset_ids": [...] \| "filter": {q,field}, "all": bool, "note": "…"}`. Filter resolves to **submitted** rows only (`all:true` required when the term is blank); applies the per-row transitions in one batched transaction per action (`BulkUpdateReviewState` for approve/return, `BulkTrashTagsets` for discard) + one summary audit row. `discard` additionally requires `file.delete`. Backs "Select all N matching". |

Bulk approve is the plain publish (no per-piece overrides — those are single-row,
expanded-card decisions). Upload-flow integration (`POST /files/upload`,
`POST /api/files/check`, `pending`/`restored` response fields; byte-dup →
`AttachDraftTagset` offering a draft appearance): `docs/api/upload.md`.

## Restores must not bypass review

Restoring a trashed file brings back whatever review state its appearance had —
which for a previously approved one means *live*. Who initiates matters:

- **Library → Trash scope** (`POST /api/admin/tagsets/{id}/restore`) —
  prior-state restore, unchanged. An explicit moderator action; a discarded
  submission visibly re-enters the queue (Trash badges such rows "pending
  review").
- **Upload-initiated restores** — a re-upload of trashed bytes under the
  `reupload_restores` policy, or `POST /api/files/{hash}/restore` under
  `uploader_restore` — would otherwise let **any `file.upload` holder publish
  any trashed file** by re-sending its bytes. With auth configured, such a
  restore of an `approved` file is demoted to the **restorer's draft**
  (`database.StageRestoredFile`: state → `draft`, note/`submitted_at` cleared,
  ownership → restorer), so it lands in *their* "My uploads" and passes review
  again. Files trashed while *pending* keep their state and owner — they
  re-enter the queue where they were.

## Audit actions

| Action | When | Detail |
|---|---|---|
| `file.bulk_submit` / `file.bulk_approve` | uploader sends to approval (queued / self-approved) | one summary row per bucket |
| `file.approve` | moderator approve | `tagset:N` |
| `file.return` | moderator returns | `tagset:N`, the note |
| `file.trash` | discard (tagset soft delete) | `tagset:N`, `owner-discard` / `moderator-discard` |
| `file.bulk_approve` / `file.bulk_return` / `file.bulk_trash` | bulk moderation | `tagsets`, count |
| `metadata.edit` | tag edit of an appearance | `tagset:N` |
| `file.restore` | any restore | `restore-via-reupload (re-staged as draft): …` / `uploader-restore (re-staged as draft)` mark demoted restores |

## Web UI

The Review queue and My-uploads are **bespoke standalone modules**, not the
shared `file-management-view.md` component — the shared file-list couldn't host
the uploader's state/notes view or the moderator's per-piece decision card, so
both were rebuilt (recording-tagsets P4; `file-list.js` reverted to serving only
Files/Trash/Library/Duplicates).

- **`/upload` → "My uploads" tab** (`webui/static/js/mine-list.js`, shell-native,
  owner mode): state sections **Returned** (each row carries the moderator's
  note), **Drafts**, **Awaiting review** (read-only), a sticky select-all +
  *Send to approval* / *Remove* bulk bar. Draft/returned rows edit via the
  shared `track-edit.js` modal (owner-scoped PATCH), preview through the shell
  player, and can be removed (per-row confirm or bulk) → Trash. Toast says
  "published" for self-approvers. Folder cover images are still co-located and
  posted server-side for `metadata.edit` holders (headless).
- **`/admin/library` → "Review" scope** (`webui/static/js/admin/moderation.js`,
  client-gated `content.moderate`, page-local shared player): per-uploader
  **collapsible groups → collapsed submission cards → an expandable 3-piece
  decision card**. The card shows the classification chip beside the state
  badge; case B renders the ladder compare (current best vs submitted, from the
  classify endpoint) with a keep/drop-bytes choice (recommended off the ladder)
  + a force-new toggle, wired to approve `{drop_bytes, force_new}`; A/C get
  trimmed cards; a collision is a no-op (Return/Discard only). Compact per-row
  approve/return/discard icons mirror the bulk-bar colours; return/discard reuse
  the `library.html` modals; edit via `track-edit.js` (the tagset). One bulk
  toolbar (approve / return-with-one-note / discard) over a cross-group
  selection of **`submitted`** rows only; a group-header checkbox selects an
  uploader's whole batch (works while collapsed). The dashboard "Review" card
  deep-links here via `#review`.
- **Dashboard** card with the pending count; **Library → Trash scope** rows whose
  appearance `review_state <> 'approved'` carry a "pending review" badge (restore
  re-enters the queue).

## Repository surface (`database/`)

Tagset-addressed: `ListUploadsByUser` / `ListUploadsByUserPage` /
`UploadTagsetIDsByUserFilter`, `ListPendingReview` / `ListPendingReviewPage` /
`PendingReviewTagsetIDsByFilter`, `UpdateReviewState` (+ `ReviewTransition`) and
`BulkUpdateReviewState([]int64)`, `ApproveSubmission(tagsetID, dropBytes,
forceNew)`, `ClassifySubmission(tagsetID)`, `TagsetReviewInfo` (narrow
state/owner/trash lookup for the blob gate), `AttachDraftTagset` (byte-dup
draft), `UpdateTagsetMetadata` / `TagsetMetadataByID`, `BulkTrashTagsets`
(moderator discard), `DiscardOwnUpload` / `BulkDiscardOwnUploads` (owner remove),
`StageRestoredFile` (restore demotion). Adding to `database.Repository` breaks
the api package's `fakeRepo` — the usual gotcha.
