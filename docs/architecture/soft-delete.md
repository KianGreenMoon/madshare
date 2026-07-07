# Soft Delete & Trash — three perspectives

## Model

The system never destroys content on an admin delete. It sets one of **two
soft-delete marks**, and whether something is *in the library* or *in the trash*
is **derived** from them — there is no third "trashed" flag or state to keep in
sync.

| Mark | Grain | Set/cleared by | Meaning |
|---|---|---|---|
| `tagsets.deleted_at` | appearance (catalog unit) | Trash/Restore of an appearance; `TrashRecording` trashes all of a recording's appearances | this **appearance** is trashed |
| `files.deleted_at` | file / rendition (blob) | `RemoveRendition` / `RestoreRendition` (bytes kept on disk) | this **blob** is soft-removed |

Everything else — "is this track in the library?", "is this recording trashed?",
"is this blob dormant?" — is a **query** over those two marks plus the recording
overlay (`recording-tagsets.md`). Neither mark ever cascades on set.

### The one visibility invariant

Every user-facing (tagset-rooted) listing gates an appearance on `visibleTagset`
(`database/tagsets.go`):

```
m.deleted_at IS NULL
  AND m.review_state = 'approved'
  AND EXISTS (SELECT 1 FROM files sf
              WHERE sf.recording_id = m.recording_id AND sf.deleted_at IS NULL)
```

An appearance is in the library **iff** it is approved, not trashed, **and its
recording still has at least one surviving rendition to play**. The moment the
**last** surviving file of a recording is soft-removed, that `EXISTS` goes false
and **every appearance of the recording drops out of the library** — no separate
action needed. Such a recording is called **dormant**; "dormant" is a *label for
a derived condition* (`recording has zero surviving files`), not a stored state.
Restoring any rendition makes `EXISTS` true again and the appearances return.

**Trash is the complement of the library at three grains.** The same underlying
removal is visible through three lenses; an admin restores from whichever one
they happen to be looking at, and it always lands the item back in a playable
library state. The three perspectives are **never merged into one list** — each
is its own view over the same facts.

---

## The three Trash perspectives

The Trash page carries a sub-mode switch: **Appearances · Recordings · Files.**

### 1. Appearances (default)

**Membership** — approved appearances that are *not currently in the library*:

```
m.review_state = 'approved' AND NOT (visibleTagset)
  ≡  m.review_state = 'approved'
     AND ( m.deleted_at IS NOT NULL                        -- trashed appearance
           OR NOT EXISTS surviving file for its recording ) -- dormant
```

(Drafts / submitted appearances are excluded — those live in the moderation
queue, not Trash.) This is the tagset-grain "trash bin of tracks", and it now
includes **dormant** appearances (recording lost its last file) as well as
individually trashed ones.

- **Restore** — clear `deleted_at`; **and if the recording is dormant, also
  restore its ladder-best removed rendition** so the appearance actually plays.
  ("Restore the appearance restores the file too.")
- **Delete forever** — tagset-addressed hard delete through the shared
  `hardDeleteTagsetsTx` cascade (last appearance GCs the recording + its files).
  The guard refuses only a **visible** appearance (never one shown here).
- **Edit** — tags only (unchanged; access is meaningless on a non-served file).

### 2. Recordings

**Membership** — recordings entirely out of the library: at least one appearance
was once approved, but **none is visible now** (all trashed and/or dormant).

```
EXISTS (approved appearance of r)  AND  NOT EXISTS (visible appearance of r)
```

(Recordings that only ever had drafts are excluded — moderation, not trash.)
This is the whole-recording bin; it overlaps the other two views by design (its
appearances also show under Appearances, its removed files under Files).

- **Restore** — un-trash every trashed appearance **and** ensure ≥1 rendition is
  surviving (restore the best removed one if none) → fully back in the library.
- **Delete forever** — `HardDeleteRecording` (recording + all appearances + all
  files, blobs reclaimed after commit), count-aware confirm.

### 3. Files

**Membership** — soft-removed blobs (`files.deleted_at IS NOT NULL`): removed
renditions and absorbed/dormant blobs. The file grain.

- **Restore** — `RestoreRendition` (clear `files.deleted_at`); any dormant
  appearances of its recording re-enter the library automatically.
- **Delete forever** —
  - **Not the last file** of its recording → reclaim this blob + `files` row
    only; repoint any *live* tagset whose `origin_file_id` is this file onto a
    surviving rendition so no provenance dangles.
  - **Last file** of its recording → **cascade-prune the whole recording**
    (recording row + every appearance + this file, blobs reclaimed). No metadata
    is preserved — the recording has nothing left to play. (Owner decision:
    "if it's the last file, just prune everything.")

---

## Permanent delete lives *only* in Trash

Hard/permanent deletion is available **only on the Trash page**, across the three
perspectives above. Every other surface does **soft** operations only:

- **`/admin/recordings`** loses *all* permanent-delete affordances — both the
  whole-recording "Delete permanently" and the per-trashed-appearance "Delete
  permanently". It keeps Trash / Remove-rendition / Restore / Merge / Move / edit
  (curation + soft ops). Restore stays everywhere (harmless).
- The underlying hard-delete endpoints are **not removed** — they are simply
  *invoked from Trash* instead: Recordings-mode calls `HardDeleteRecording`,
  Appearances-mode calls the tagset hard delete, Files-mode calls the new file
  hard delete.

This gives one rule that is easy to reason about: **you remove things anywhere;
you destroy them in one place.** Which lens an admin used to remove or restore is
irrelevant — the derived model keeps every surface consistent.

---

## Cascades (unchanged core)

Permanent delete runs one shared, symmetric cascade in a single transaction:

- **Tagset-first** (`hardDeleteTagsetsTx`): a non-last appearance drops only its
  tagset (recording + files survive for the other appearances); the last
  appearance takes the recording and all its files with it.
- **File-first** (`hardDeleteFilesTx`): removing the last *file* of a recording
  GCs the recording and all its appearances.

The Files-mode "last file → prune everything" is exactly the file-first cascade;
`HardDeleteRecording` is the tagset-first cascade over every appearance.

---

## Endpoints

`file.delete`-gated under `/api/admin/` unless noted. **New** = added for this
work; the rest already exist.

| Perspective | List | Restore | Delete forever |
|---|---|---|---|
| Appearances | `GET /api/admin/trash` — **broaden** to the membership predicate above (tagset-rooted; today it is file-rooted via `reprTagset` and misses dormant + absorbed-blob appearances) | `POST /api/admin/tagsets/{id}/restore` — **extend** `RestoreTagset` to also restore a rendition when the recording is dormant | `DELETE /api/admin/tagsets/{id}` — **relax** guard to refuse only a *visible* appearance |
| Recordings | `GET /api/admin/trash/recordings` — **new** (trashed-recording membership) | `POST /api/admin/recordings/{id}/restore` — **new** `RestoreRecording` (un-trash appearances + ensure a rendition) | `DELETE /api/admin/recordings/{id}` — existing `HardDeleteRecording` |
| Files | `GET /api/admin/trash/files` — **new** (removed-blob listing) | `POST /api/admin/renditions/{id}/restore` — existing `RestoreRendition` | `DELETE /api/admin/renditions/{id}` — **new** (reclaim blob; last file → recording cascade; repoint live origin refs) |

Bulk variants mirror the existing `POST /api/admin/trash/bulk` shape per mode
(select-all-N-matching uses `all:true`, per `file-list-scaling.md`).

---

## UI

- **Trash panel** (`webui/static/js/admin/trash.js`, hosted by `library.js`)
  gains a three-way sub-mode switch. Appearances reuses the existing
  `file-list.js` scope (membership just broadens). Recordings and Files are
  lighter bespoke lists (recording card / removed-blob row) sharing the page's
  one preview player — not `file-list.js` scopes.
- **`/admin/recordings`** (`webui/static/js/admin/recordings.js`) drops the
  whole-recording and per-appearance "Delete permanently" buttons; keeps
  everything else.

---

## Audit

| Action | Trigger |
|---|---|
| `file.trash` | soft delete an appearance (move to Trash) |
| `rendition.remove` / `rendition.restore` | soft-remove / restore a blob |
| `recording.trash` / `recording.restore` | whole-recording soft trash / restore |
| `file.restore` | restore an appearance from Trash |
| `file.delete` | permanent delete (any perspective) |

---

## Related upload behaviour (unchanged)

A re-upload of a trashed file restores it instead of duplicating (subject to the
trash-restore policy, `docs/api/upload.md`). With moderation configured, an
upload-initiated restore of a previously *approved* file is demoted to the
restorer's draft (`StageRestoredFile`) rather than republishing — restores must
not bypass review (`docs/architecture/moderation.md`).

## Deferred

- **Auto-clear trash**: `[storage] trash_ttl_days = 0` (0 = disabled); a sweep
  hard-deletes appearances/blobs older than the TTL. Design when implementing.

## See also

- `docs/architecture/recording-tagsets.md` — the recording ↔ tagset overlay and
  `visibleTagset`.
- `docs/architecture/file-management-view.md` — the Trash scope + `file-list.js`.
- `docs/architecture/moderation.md` — review states (why drafts aren't Trash).
