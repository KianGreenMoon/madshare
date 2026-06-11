# Unified File-Management View

Design + phasing record for collapsing every place a file's metadata can be
viewed or edited into **one component**, parameterised by *scope*. Pre-build
design; a `docs/architecture/` reference follows once it ships (the pattern
`docs/plans/moderation-review-bucket.md` → `docs/architecture/moderation.md`
already established).

Mockups under review live in `webui/static/dev/` (`unified-v2-hybrid.html` is
the chosen direction; `_demo-core.js` is the throwaway driver that prototypes the
component contract below). Those files are review-only and are deleted when the
real component lands.

---

## Problem

The same "list of files with title/artist/album, a preview player, and a
metadata editor" is re-implemented in **five** places, each with its own row
rendering, selection, bulk toolbar, badges, and inline-confirm code:

| Surface | Endpoint | Grouping | Can edit tags? |
|---|---|---|---|
| Admin → Files · flat table | `GET /api/files` | none | yes |
| Admin → Files · entity drill-down | `GET /api/artists` … | artist→album→track | yes (2nd impl) |
| Admin → Moderation | `GET /api/admin/moderation` | by uploader | yes |
| Upload → My uploads | `GET /api/my/uploads` | by review state | yes (owner endpoint) |
| Admin → Trash | `GET /api/admin/trash` | none | **no** ← the gap |

They already share `track-edit.js` (the edit modal), `player.js`, and the
`.files-table` CSS — but nothing else. The duplication is why Trash drifted
(no Edit), why "Files" carries two renderers, and why a fix has to be made four
times. Access controls (guest toggle + license) are rendered inline in the Files
table only, where they read as clutter.

## Goal

One file-list component that every surface renders through, so:

- **Edit works everywhere**, identically — including Trash.
- A bug or feature is fixed once.
- Access is presented as a calm read-only summary and edited where the other
  tags are (per-file and in bulk), not as inline table widgets.
- Finding a file to edit doesn't require search: an artist/album **Browse**
  presentation sits alongside the flat **List**.

No new persistence and no new backend are required — the component composes the
endpoints that already exist (bulk = client-side loop, the established
trash/moderation convention).

---

## Chosen shape — Hybrid

Two homes for the one component, keeping the admin and owner contexts apart:

```
ADMIN ▸ Library                         UPLOAD ▸ My uploads
┌───────────────────────────────┐      ┌───────────────────────────┐
│ scope:  All · Review · Trash  │      │ (same component,          │
│ ───────────────────────────── │      │  owner-scoped endpoints,  │
│ Browse ⇄ List · search · bulk │      │  grouped by review state) │
│ table / drill-down + Edit      │      │ table + Edit              │
└───────────────────────────────┘      └───────────────────────────┘
```

- The admin **Library** page folds the old `Files`, `Moderation`, and `Trash`
  nav items into one page with a **scope switch** (`All · Review · Trash`). The
  admin secondary nav drops three entries for one.
- The uploader's **My uploads** stays on `/upload` (where uploaders already
  look), rendered by the same component in owner mode.

Rejected alternatives (mockups kept for the record): **v1** single page folds
*everything* incl. My uploads onto one admin page — mixes owner and admin
contexts. **v3** leaves all four pages in place and only unifies rendering —
lowest-risk but keeps the nav clutter the rework set out to remove.

---

## Component contract

A scope is the unit of configuration. Everything the component renders is driven
by a scope descriptor; switching scope swaps data + actions, never the chrome.

```
Scope = {
  title, count,
  endpoint:        loader for the row list,
  columns:         ordered column keys,
  presentation:    'list' | 'browse'  (browse only where a hierarchy exists),
  group:           null | 'uploader' | 'state',
  rowActions:      ordered action keys,
  bulk:            ordered bulk-action descriptors,
  selectable:      (row) => bool,      // which rows enter bulk selection
  badge:           null | 'review-state' | 'pending',
  accessEditable:  bool,               // show License+Guest in the editors
}
```

### Presentations

- **List** — the flat `.files-table`. Columns adapt per scope; rows carry a
  `data-row` payload so an Edit click opens the modal with no DOM scraping.
- **Browse** — the artist → album → track drill-down (the existing entity view),
  driven by `GET /api/artists`, `GET /api/albums?artist=`,
  `GET /api/tracks?artist=&album=`. Cover tiles + breadcrumb. Selection and
  per-group **Edit tags…** work at the artist and album level (select / edit a
  whole album's worth of files at once). Browse is offered on the **All** scope;
  the small scopes (Review/Trash/Mine) stay List-only.
- The **search** box filters the current presentation in both modes.

### Selection + bulk

One selection set of file hashes, shared across List and Browse. `selectable`
decides which rows can be checked (e.g. Moderation: `submitted` only — a returned
file must not be re-approved by a bulk action right after it was sent back). The
bulk toolbar's action set is the scope's `bulk`. Bulk actions **loop the
per-file endpoints client-side** and tally results, exactly as Trash and
Moderation already do; a server-side batch endpoint is a possible later
optimisation, not a prerequisite.

### The two editors (shared)

- **Per-file edit** — today's `track-edit.js`, extended with an **access**
  section (a **License** picker + **Guest-playable** toggle) shown only when
  `accessEditable`. Tags write via `PATCH …/metadata`; access writes via the
  guest/license endpoints (see below).
- **Bulk edit** — a new modal that sets **Artist / Album artist / Album** and
  (when `accessEditable`) **License / Guest** across the selection.
  **Blank field = keep each file's current value.** **Title is intentionally
  excluded** — it is unique per track. Setting the album/artist tag on a
  selection *reassigns* those files (overlay re-tag); it is **not** an entity
  rename.

### Access column — read-only

Access becomes a display-only summary cell (`Guest · CC BY` / `Private`). The
inline checkbox+select is removed. All access editing happens in the two editors
above, gated by `accessEditable`.

---

## Scope catalog

| Scope | `accessEditable` | Presentation | group | `selectable` | Row actions | Bulk actions |
|---|---|---|---|---|---|---|
| **All** (admin) | ✓ | list **+ browse** | – | all | Play · Edit · Move to Trash | Edit tags… · Move to Trash |
| **Review** (admin) | ✓ | list | uploader | `submitted` | Play · Edit · Approve · Return… · Discard | Edit tags… · Approve · Return… · Discard |
| **Trash** (admin) | ✓ | list | – | all | Play · Edit · Restore · Delete forever | Edit tags… · Restore · Delete forever |
| **My uploads** (owner) | ✗ | list | state | draft/returned | Play · Edit · Send · Remove | Edit tags… · Send to approval · Remove |

The **review-state badge** renders for Review/Mine; the **pending-review badge**
renders for Trash rows whose `review_state <> 'approved'` (a restore returns
them to the queue, not the library) — both already exist in `admin-shell.css`.

---

## Endpoints (all already exist)

| Concern | Route |
|---|---|
| List · all | `GET /api/files` |
| List · review / trash / mine | `GET /api/admin/moderation` · `GET /api/admin/trash` · `GET /api/my/uploads` |
| Browse | `GET /api/artists` · `GET /api/albums?artist=` · `GET /api/tracks?artist=&album=` |
| Edit tags (admin) | `PATCH /api/files/{hash}/metadata` |
| Edit tags (owner) | `PATCH /api/my/uploads/{hash}/metadata` |
| Edit access | `POST /api/admin/files/{hash}/guest` · `POST /api/admin/files/{hash}/license` |
| Move to Trash / Restore / Delete | `DELETE /api/admin/files/{hash}` · `POST /api/admin/trash/{hash}/restore` · `DELETE /api/admin/trash/{hash}` |
| Approve / Return | `POST /api/admin/moderation/{hash}/approve` · `…/return` |
| Submit / Remove (owner) | `POST /api/my/uploads/submit` · `DELETE /api/my/uploads/{hash}` |

Bulk tag/access edits loop the per-file routes above. The access routes are
admin-only, which is exactly why **My uploads is `accessEditable: ✗`** — an
uploader sets tags on their drafts, not guest/license.

**DTO note:** the Browse track DTO (`/api/tracks`) carries no `hash`,
`guest_playable`, or `license`. The component already resolves a browse track to
its file record via the `url → /api/files` map (the existing Files-page trick)
for hash + access; if we want the access summary inline on browse track rows
without that lookup, add the three fields to the track DTO. Non-blocking.

## Permissions

Unchanged from today — the component reads `identity.permissions` and the scope
descriptor; it adds no new permission:

- `metadata.edit` → Edit (tags), Edit tags… (bulk), and the access section.
- `file.delete` → Move to Trash / Restore / Delete forever.
- `content.moderate` → the Review scope and Approve/Return/Discard.
- `file.upload` → the owner My-uploads scope.

A scope/action the caller can't use is simply not rendered; the API enforces the
same gates server-side regardless.

---

## Out of scope (stays where it is)

Artist/album **rename**, **merge**, and **cover upload** are entity-level
operations on a *different axis* than per-file editing — they keep living in the
Browse presentation's existing entity affordances, unchanged. Bulk "set the
album tag on these files" (a re-tag) is **not** a rename and does not touch
`album_images`; an embedded-art-only cover can orphan on re-tag, same caveat as
`docs/api/metadata.md` already documents. Folding a "rename this artist/album"
shortcut into Browse is a possible follow-up, not part of this rework.

---

## Phasing

0. **Mockups + this doc** — done. (`webui/static/dev/unified-v2-hybrid.html`.)
1. **Extract the component** → `webui/static/js/file-list.js` + `file-view.css`:
   rendering (list + browse), selection, bulk toolbar, grouping, badges, inline
   confirms, wiring to the extended `track-edit.js` + the new bulk-edit modal +
   `player.js`. Drive it from the scope catalog above.
2. **Extend `track-edit.js`** with the optional access section; add the
   **bulk-edit** modal component.
3. **Migrate call sites onto it, one at a time, deleting the old renderer:**
   Trash → My uploads → Moderation → Admin Files. Each migration is behaviour-
   preserving except Trash, which *gains* Edit.
4. **Re-home navigation (Hybrid):** Admin `Library` page with the scope switch;
   retire the `Files` / `Moderation` / `Trash` nav entries; `My uploads` stays
   on `/upload`.
5. **Cleanup:** delete superseded CSS/JS (the per-page renderers, the inline
   access controls), reconcile `docs/architecture/moderation.md` and the upload
   docs, and supersede `docs/plans/admin-files-rework.md`. Add the
   `docs/architecture/` reference for the shipped component.

## Rollout

After Phase 5 the view is dogfooded, then opened to other users for a feedback
round; change requests are expected. The **scope catalog is the extension
point** — new columns, actions, or a new scope are descriptor edits, not
re-renders — so the contract is deliberately the thing kept stable while the
particulars flex.
