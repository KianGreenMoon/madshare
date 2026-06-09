# Upload Rework — Client Hash-Precheck + Page Rewrite

Status: **planned** (design; no code yet)
Branch: aidev
Depends on: `docs/plans/persistent-shell-playback.md` (the new upload page is a
shell-native listening page)

This covers roadmap **Phase 2** (hash-precheck backend) and **Phase 3** (upload
page rewrite). Phase 2 is backend-only and independently testable; Phase 3 needs
both the shell (Phase 1) and the Phase 2 endpoint.

## Why

Two things:

1. **Save pointless uploads.** Before sending a (possibly huge) file, the client
   computes its content hash and asks the server whether that content already
   exists. If it does, skip the upload entirely. If it exists but is
   **soft-deleted (trashed)**, offer to restore it instead of re-uploading. This
   is a bandwidth/UX win, especially over the Yggdrasil mesh deployment.
2. **The upload page needs a rewrite anyway** — to live in the persistent shell,
   to host the precheck UX, and to clean up the lingering `upload.css` `.btn`
   override (the last remnant of BUG-15).

## Design owner's note on the security question

A client-supplied hash is normally a red flag (trusting client input). It is
**safe here by construction** because of one rule:

> **The server always recomputes the hash itself and is the only authority.** The
> client hash is purely an advisory pre-check to avoid an upload. The server never
> stores, trusts, or dedupes on the client-provided value.

Worst a lying/buggy client achieves:

- claims "exists" when it doesn't → it just declines to upload, harming only
  itself;
- claims "doesn't exist" when it does → the normal upload path runs and the server
  hashes + dedupes anyway (current behavior).

So there is no trust placed in the client value and nothing to exploit. This is an
explicit, owner-approved design. (The server's existing hash is **SHA-256 hex over
the raw file bytes** — `api/storage/hash.go`; the client must match exactly, see
below.)

## Goals

1. `POST /api/files/check` (auth'd): given a content hash, report whether the
   content exists and whether it is trashed/restorable. Advisory only.
2. Client computes the **same** hash the server does (SHA-256 hex of raw bytes)
   and uses the endpoint to (a) skip a duplicate upload, (b) offer restore for a
   trashed match.
3. A user-visible **toggle to disable the precheck**, defaulting **ON**, placed
   **below the fold / not prominent**, with a short explanation of what turning it
   off means (you'll re-upload duplicates; the server still dedupes on receipt).
4. Rewrite the upload page as a shell-native listening page; drop the `upload.css`
   `.btn` override (BUG-15 remnant) in favor of the shared `app.css` button.

## Non-goals

- No change to the actual upload/dedupe/tag-extraction path on the server beyond
  adding the check endpoint and (if chosen) an uploader-facing restore.
- No client-side trust: the server re-hashes, always.
- Not a general "file exists?" public API — the endpoint is auth'd (see below).

---

## Phase 2 — backend: `POST /api/files/check`

**Endpoint.** `POST /api/files/check` with `{ "hash": "<sha256-hex>" }` →

```json
{ "exists": true, "trashed": false, "restorable": false }
```

- `exists` — content with this hash is present (active) in `files`.
- `trashed` — the content exists but is soft-deleted.
- `restorable` — whether *this identity* may restore it (see permission decision).

**Auth.** Require authentication (a logged-in uploader). Rationale: this is a
by-hash existence oracle; do not expose it anonymously. Gate at least on the same
permission as upload (`file.upload`) so only would-be uploaders can probe.

**No client trust.** The endpoint only *reads*. The upload path
(`POST /files/upload`) is unchanged and remains the sole authority — it re-hashes
every received file (`storage.HashUpload`) and dedupes on the server-computed
value.

**Permission decision — uploader restore (OPEN, needs sign-off).** Today
trash-restore is admin-only (`/api/admin/trash/{id}/restore`). For the precheck
flow a regular uploader who re-presents the exact content of a trashed file may
want to restore it. Options:

- **(a) Restore allowed for an uploader who could upload that content.** Re-adding
  content you're allowed to upload is equivalent to uploading it; restore just
  avoids re-sending bytes. Lean: allow, via a *new* uploader-facing restore path
  scoped to "I can prove I have this content" — but proving possession without
  uploading is the crux (a hash is not proof of possession). Safer variant: the
  client uploads normally and the **server**, on receiving content whose hash
  matches a trashed file, restores-instead-of-creating. That keeps possession
  proof intact (you sent the bytes) and needs no new uploader restore endpoint.
- **(b) Restore stays admin-only.** The precheck reports `trashed: true,
  restorable: false`; the UI tells the uploader the content exists but is trashed
  and to ask an admin. Simplest, most conservative.

**Recommendation:** ship **(b)** for Phase 2 (report trashed, restore stays
admin), and treat the "server restores trashed file when matching content is
actually uploaded" half of **(a)** as a follow-up — it's the only variant that
preserves possession proof, and it lives in the upload handler, not a new
uploader restore endpoint. Decide before building.

**Testing (Phase 2).** Unit-test the endpoint: unknown hash → `exists:false`;
active file → `exists:true,trashed:false`; trashed file → `exists:true,
trashed:true`; bad/empty hash → 400; unauthenticated → 401. Confirm it never
mutates state.

---

## Phase 3 — client: hashing + precheck UX + page rewrite

### Client-side hashing — the one real gotcha

The client must produce **SHA-256 hex of the raw file bytes** to match
`api/storage/hash.go`. The catch: the Web Crypto API (`crypto.subtle.digest`) has
**no streaming/incremental mode** — it needs the whole file buffered in memory.
Uploads here can be very large (`max_upload_mb`, cap 1 TiB), so buffering the
whole file to hash it is a non-starter.

**Therefore: incremental SHA-256 over `File.stream()`.** Read the file in chunks
(`Blob.stream()` → `ReadableStream` reader, or sliced `arrayBuffer()` windows) and
feed an incremental hasher. `crypto.subtle` can't do this, so use a small
streaming SHA-256 implementation:

- a tiny pure-JS incremental SHA-256 (no deps, slowest), or
- a WASM SHA-256 (fast, one small asset).

Decide during impl; either way it must hash **block-by-block off the stream**, not
buffer the file. Show progress (it's a full read of a large file). This is the
only non-trivial engineering in the upload rework.

### Upload flow with precheck

```
user selects file(s)
  for each file:
    if precheck ON:
      hash = streaming-sha256(file)        (with progress)
      { exists, trashed, restorable } = POST /api/files/check { hash }
      exists && !trashed   → skip upload, mark "already in library"
      exists && trashed    → prompt: restore?  (per permission decision)
                               (b): inform "exists but trashed — ask an admin"
                               (a)-follow-up: upload anyway; server restores
      !exists              → upload normally
    else (precheck OFF):
      upload normally  (server hashes + dedupes on receipt as today)
```

The server side of every "upload normally" is unchanged and authoritative.

### The off-switch

- Default **ON**.
- Placed **below the fold / in an advanced/settings disclosure**, not in the
  primary upload affordance.
- Short copy explaining the consequence: *"Skip files already on the server.
  Turning this off re-uploads duplicates over the network; the server still
  de-duplicates them on arrival, so nothing is stored twice — you just spend the
  bandwidth."*
- Persist the choice client-side (localStorage) so it sticks per browser.

### Page rewrite

- Make the upload page a **shell-native listening page**: `<body data-page="upload"
  data-module="/static/js/upload.js">`, `{{template "player-bar" .}}` after
  `</main>`, load `shell.js`. So playback started on the library keeps going while
  uploading, and uploading is reachable without losing the queue.
- Convert the upload JS to the `{ init, teardown }` module contract (abort any
  in-flight hashing/upload on `teardown`).
- **BUG-15 remnant:** drop `upload.css`'s `.btn` override and use the shared
  `app.css` `.btn` / `.btn-neutral`; rename upload's own accent button to a
  page-local class so it stops shadowing the shared header buttons.
- Reuse existing helpers (`auth.js`, the toast/`shared.js` helpers) rather than
  re-implementing.

### Testing (Phase 3)

- Hash correctness: client hash of a file **equals** the server's stored hash for
  the same content (round-trip a known file).
- Large file: hashing a multi-hundred-MB file does not blow memory (streaming
  works) and shows progress.
- Precheck branches: brand-new file uploads; exact duplicate is skipped; trashed
  match shows the right prompt/info per the permission decision.
- Off-switch: disabled → always uploads; server still dedupes on receipt; choice
  persists across reloads.
- Shell-native: start playback on library → go to upload → playback continues;
  upload page `teardown` aborts in-flight hashing/upload.
- BUG-15: header auth buttons on the upload page match the shared look (no
  `upload.css` `.btn` shadowing).

---

---

## Review bucket (moderation queue) for non-admin uploads — policy, planned

A separate, **policy-configurable** idea (owner's thought, 2026-06-09): what
happens to content a **non-admin** uploads?

Two stances, selectable by config:

- **Trust uploaders absolutely (default, = today's behavior).** A non-admin
  upload goes live in the library immediately. No review. Simplest; fine when all
  uploaders are trusted.
- **Review bucket.** When the operator does *not* fully trust uploaders, a
  non-admin upload lands in a **pending-review** state — stored, but **not visible
  or playable** in the public library — until an admin **approves** (publish) or
  **rejects** (→ trash/delete). **Admin uploads bypass review.**

This is a content-**moderation** layer, distinct from the hash-precheck above.
It fits naturally on top of the existing model:

- It reuses the **content-access / default-deny** model (auth Phase 3): a
  pending-review file is simply not granted visibility yet — the gate already
  exists, the upload just enters in an ungranted/quarantined state.
- The admin **review queue UI** is a sibling of the existing **Trash** page
  (list → approve / reject, two-step on reject). Could live as a new
  `/admin/review` page or a tab on the files/trash pages.
- It is adjacent to the existing **auto-publish / autoderive** setting (the
  free-license allow-list on `/admin/settings`): auto-publish decides *licensing*
  of uploads; the review bucket decides *visibility pending approval*. Keep them
  as separate, composable policies (e.g. an upload could auto-derive a license yet
  still await review).

**Config sketch:** a policy flag such as `[upload].review_non_admin_uploads`
(default `false` = trust). When `true`, non-admin uploads enter pending-review.

**Interactions to think through:**

- **Hash-precheck vs. pending files.** Should `POST /api/files/check` report a
  pending-review file as `exists`? Probably yes (the content *is* stored, so a
  second upload of it is still redundant), but it must not leak it as *playable*.
  Decide the exact reporting.
- **Per-uploader trust.** A coarser/finer knob later: trust by role or per-user
  (a "trusted uploader" flag) rather than a single global switch. Out of scope for
  the first cut; note it.
- **Storage/dedupe.** A pending file still dedupes by hash; approval flips
  visibility, it doesn't re-store bytes.

**Status:** planned/optional — not part of Phases 2–3's critical path. Build the
trust-by-default path first; add the review bucket when the moderation need is
real. Flagged here so it isn't forgotten.

---

## Decisions

1. **Client hash is advisory only; server re-hashes and is authoritative.** This
   is what makes the client-supplied hash safe. Owner-approved.
2. **`/api/files/check` is auth'd** (≥ `file.upload`), not public — avoid an
   anonymous existence oracle.
3. **Phase 2 ships restore-reporting only; uploader restore is deferred** to the
   "server restores on actual matching upload" variant (preserves possession
   proof). Confirm before building.
4. **Off-switch defaults ON, lives below the fold** with an explanation.
5. **Upload page becomes shell-native** and sheds the `upload.css` `.btn` override
   (closes the last BUG-15 remnant).

## Open questions

- Streaming SHA-256: pure-JS vs. WASM (size vs. speed). Decide at impl.
- Uploader-restore permission: confirm Decision §3 (report-only now) vs. enabling
  the server-restores-on-upload variant in this pass.
- Per-file vs. whole-batch precheck UI when multiple files are selected.
