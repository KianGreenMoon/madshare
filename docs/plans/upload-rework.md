# Upload Rework — Client Hash-Precheck + Page Rewrite

Status: **planned** (design; no code yet)
Branch: aidev
Depends on: `docs/plans/persistent-shell-playback.md` (the new upload page is a
shell-native listening page)

This covers roadmap **Phase 2** (hash-precheck backend) and **Phase 3** (upload
page rewrite). Phase 2 is backend-only and independently testable; Phase 3 needs
both the shell (Phase 1) and the Phase 2 endpoint.

**Staging (decided 2026-06-09):** Phase 3 is split so the riskiest part (client
hashing) is isolated:

- **3a — upload page rewrite:** make `/upload` **shell-native** (continuity:
  music keeps playing across Library⇄Upload) + **one-button file/folder upload**
  + close the BUG-15 `.btn` remnant. **No hashing/precheck.** Verifiable on its
  own. *(This is the part being built now.)*
- **3b — precheck:** the `/api/files/check` endpoint (Phase 2), client-side WASM
  hashing in a worker pool, the trash-restore policy wiring, and the off-switch.

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

1. Rewrite the upload page as a **shell-native** listening page (continuity) with
   **one-button file/folder upload**; drop the `upload.css` `.btn` override
   (BUG-15 remnant) in favor of the shared `app.css` button. *(3a)*
2. `POST /api/files/check` (auth'd): given a content hash, report a single
   `status` (`absent`/`present`/`trashed`). Advisory only. *(3b)*
3. Client computes the **same** hash the server does (SHA-256 hex of raw bytes,
   via a WASM hasher in a worker pool) and uses the endpoint to skip duplicates
   and handle trashed content per the admin policy. *(3b)*
4. A user-visible **toggle to disable the precheck**, defaulting **ON**, placed
   **below the fold / not prominent**, with a short explanation. *(3b)*

## Non-goals

- No change to the actual upload/dedupe/tag-extraction path on the server beyond
  adding the check endpoint and (if chosen) an uploader-facing restore.
- No client-side trust: the server re-hashes, always.
- Not a general "file exists?" public API — the endpoint is auth'd (see below).

---

## Phase 2 — backend: `POST /api/files/check`

**Endpoint.** `POST /api/files/check` with `{ "hash": "<sha256-hex>" }` →

```json
{ "status": "absent" | "present" | "trashed" }
```

A **single enum**, not three booleans (decided 2026-06-09 — booleans could encode
contradictions like `exists:false,trashed:true`, and the old `restorable` was a
*permission* concern wrongly folded into an *existence* check):

- `absent` — no content with this hash on the server → the client should upload.
- `present` — content exists and is live → skip (duplicate).
- `trashed` — content exists but is soft-deleted → see the **trash policy** below.

There is intentionally **no `restorable` field** — whether/how a trashed file
comes back is governed by the admin trash-restore policy, not reported per-check.

**Auth.** Require authentication (a logged-in uploader). Rationale: this is a
by-hash existence oracle; do not expose it anonymously. Gate at least on the same
permission as upload (`file.upload`) so only would-be uploaders can probe.

**No client trust.** The endpoint only *reads*. The upload path
(`POST /files/upload`) is unchanged and remains the sole authority — it re-hashes
every received file (`storage.HashUpload`) and dedupes on the server-computed
value.

#### Trash-restore policy — an admin setting (decided)

When a precheck returns `trashed`, what the uploader can do is **configurable by
the admin** (all three behaviors are legitimate, so it's a policy, not a
hardcoded choice). A setting on **`/admin/settings`** (alongside auto-publish),
e.g. `[upload].trash_restore_policy`, with three modes:

| Mode | On `status: trashed` the uploader… | Notes |
|------|-----------------------------------|-------|
| `inform` **(default)** | is told "already on the server, in Trash — an admin can restore it"; no upload | Safest; restore stays admin-only (today's model). No new endpoint/permission. |
| `reupload_restores` | may upload anyway; the **server un-trashes** the file when it receives the matching content (proves possession by sending the bytes) | Lives in the upload handler, not a new restore endpoint; preserves possession proof. |
| `uploader_restore` | gets a **Restore** action that restores without re-sending bytes | Needs a new uploader-restore permission/endpoint — the largest access-control surface. |

The default is `inform`. The upload UI reads the policy (served with the other
public UI config) and behaves accordingly.

**Testing (Phase 2).** Unit-test the endpoint: unknown hash → `status:"absent"`;
active file → `"present"`; trashed file → `"trashed"`; bad/empty hash → 400;
unauthenticated → 401. Confirm it never mutates state. Per-policy upload behavior
is tested with Phase 3b.

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
feed an incremental hasher block-by-block — never buffer the whole file. An
incremental SHA-256 produces a **byte-identical digest** to a one-shot hash (the
algorithm processes 64-byte blocks internally regardless), and matches the
server, which itself streams the upload through `sha256` (`io.Copy`,
`api/storage/hash.go`). So client hex == server hex for the same bytes.

**Hasher = WASM SHA-256 (decided 2026-06-09)**, with a tiny **pure-JS
incremental SHA-256 kept behind the same interface** as a fallback. WASM is
near-native fast — chosen to future-proof for video / multi-GB uploads, where it
matters; for typical audio either would be imperceptible.

**Run hashing in a Web Worker pool (off the main thread).** Hashing is CPU-bound;
doing it on the main thread freezes the UI (rendering + clicks), especially for a
dropped folder of many files. A worker pool keeps the UI smooth. **Pool size is
adaptive** to the client, capped to stay polite:

```js
const cores = navigator.hardwareConcurrency || 4;   // logical cores; fallback 4
let workers = Math.min(4, Math.max(1, cores >> 1));  // half the cores, clamp 1..4
if ((navigator.deviceMemory || 4) <= 2) workers = 1; // back off on tiny devices
```

Rationale: **half, not all** (leave cores for the OS/main thread/other apps);
**floor 1** (always progress); **ceiling 4** (past ~4 you hit diminishing returns
because **upload bandwidth, not hashing, is the real bottleneck** — and 4 keeps a
32-core box from spinning every fan for no faster uploads). One tunable constant;
could later be surfaced like the existing upload-worker slider
(`webui.toml` `default_parallel_workers`/`max_parallel_workers`) but starts
**auto**. Hash (CPU) and upload (network) don't compete, so they can pipeline.
Show progress (a full read of a large file takes time).

### Upload flow with precheck

```
user selects file(s)  (one file, several files, or a whole folder)
  for each file (hashed in the worker pool, limited concurrency):
    if precheck ON:
      hash = streaming-sha256(file)        (WASM, in a worker, with progress)
      { status } = POST /api/files/check { hash }
      "present"  → skip upload, mark "already in library"
      "trashed"  → per the admin trash-restore policy:
                     inform            → "already on the server (in Trash) — ask an admin"
                     reupload_restores → upload anyway; server un-trashes on match
                     uploader_restore  → offer a Restore action
      "absent"   → upload normally
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

### Page rewrite (3a)

- Make the upload page a **shell-native listening page**: `<body data-page="upload"
  data-module="/static/js/upload.js">`, `{{template "player-bar" .}}` after
  `</main>`, load `shell.js`. So playback started on the library keeps going while
  uploading, and uploading is reachable without losing the queue.
- Convert the upload JS to the `{ init, teardown }` module contract (abort any
  in-flight hashing/upload on `teardown`).
- **One-button file/folder upload (decided 2026-06-09).** A native file dialog
  can't offer "files *or* a folder" in one picker (`<input webkitdirectory>` is
  folder-only). So: **one "Add music" button → a small menu** ("Choose files…" /
  "Choose folder…"), backed by two hidden inputs (one plain, one
  `webkitdirectory`). The **drop zone accepts both** files and a dropped folder
  (folder drops are read via `DataTransferItem.webkitGetAsEntry()` recursion).
  Folder picks/drops flatten to the set of audio files (filter by type), then
  upload like any multi-file batch.
- **BUG-15 remnant:** drop `upload.css`'s `.btn` override and use the shared
  `app.css` `.btn` / `.btn-neutral`; rename upload's own accent button to a
  page-local class so it stops shadowing the shared header buttons.
- Reuse existing helpers (`auth.js`, the toast/`shared.js` helpers) rather than
  re-implementing.

The precheck UX (hashing, status handling, off-switch) is **3b** and lands on top
of this page later.

### Testing — 3a (page rewrite)

- Shell-native: start playback on library → go to `/upload` → playback continues;
  navigate back → library is intact; upload page `teardown` aborts in-flight
  uploads.
- One-button: the menu's "Choose files…" and "Choose folder…" both work; a
  dropped folder recurses and uploads its audio files; non-audio is skipped.
- BUG-15: header auth buttons on the upload page match the shared look (no
  `upload.css` `.btn` shadowing).

### Testing — 3b (precheck)

- Hash correctness: client hash of a file **equals** the server's stored hash for
  the same content (round-trip a known file).
- Large file: hashing a multi-hundred-MB file does not blow memory (streaming +
  worker) and shows progress; the UI stays responsive during a big batch.
- Precheck branches: `absent` uploads; `present` is skipped; `trashed` behaves per
  the active admin policy (`inform` / `reupload_restores` / `uploader_restore`).
- Off-switch: disabled → always uploads; server still dedupes on receipt; choice
  persists across reloads.

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
3. **Response is a single `status` enum** (`absent`/`present`/`trashed`), not
   three booleans; no `restorable` field.
4. **Trash-restore is an admin policy** (`inform` default / `reupload_restores` /
   `uploader_restore`) on `/admin/settings`, not a hardcoded behavior.
5. **Hasher = WASM SHA-256** (pure-JS incremental fallback behind the same
   interface), run in a **Web-Worker pool sized `clamp(cores/2, 1, 4)`** (auto;
   back off to 1 on ≤2 GB devices).
6. **Off-switch defaults ON, lives below the fold** with an explanation.
7. **Upload page becomes shell-native** and sheds the `upload.css` `.btn` override
   (closes the last BUG-15 remnant).
8. **One-button file/folder upload** via a button + menu (two hidden inputs) and a
   drop zone that accepts folders.
9. **Staged: 3a (page rewrite, no hashing) then 3b (precheck).**

## Open questions

- Per-file vs. whole-batch precheck UI when many files/a folder are selected (3b).
- Whether to surface the hash-worker count manually later (starts auto).
- Exact `webui.toml`/settings keys for the trash-restore policy and worker counts
  (decide at impl).
