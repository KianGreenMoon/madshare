# Toast Messaging — Extraction & Reuse Plan

**Status:** Implemented — Phases 1–4 done (build + Go/JS tests green). Phase 5
manual browser checklist still to run (esp. the cross-page regression).
**Branch target:** `aidev` → merge to `develop`.
**Module path:** `daemonlord.ygg/madshare`.
**Scope driver:** the upload page's pop-up messages "work not right"; the library's
work well. Both run *separate* toast implementations. Extract one shared toast
module and reuse it everywhere a transient message is shown.

---

## Problem (why the upload toasts misbehave)

There are **four** near-identical toast implementations in the web UI, plus one
divergent one — and a CSS class collision between two of them:

| # | Where | Function | DOM stack | Capabilities |
|---|---|---|---|---|
| 1 | `webui/static/js/shell.js:156` | `showToast(msg, {type, actionLabel, onAction, timeout})` | `#toastStatus` / `#toastAlert` | type (status/success/error), icon, **close button**, **inline action button**, configurable timeout, **errors persist** |
| 2 | `webui/static/js/auth.js:95` | `authToast(msg, type)` *(private)* | `#toastStatus` / `#toastAlert` | type, icon, close button; no action |
| 3 | `webui/static/js/admin/shared.js:72` | `toast(msg, type)` *(exported)* | `#toastStatus` / `#toastAlert` | type, icon, close button; no action |
| 4 | `webui/static/js/upload.js:858` | `showToast(msg)` *(divergent)* | **`#toastStack`** (its own) | **no** type/icon/color, **no** close button, **no** action; centered bubble, opacity fade |

Implementations #1–#3 are byte-for-byte the same markup (`.toast` + `.toast-icon`
+ `.toast-msg` + `.toast-close`, optional `.toast-action`) styled by **`app.css`**,
and they target the shared `#toastStatus` / `#toastAlert` stacks defined in the
`auth-modals` partial (`webui/html/partials.html:116-117`).

Implementation #4 (upload) is the odd one out and the source of the bug:

1. **CSS class collision.** `upload.css:247-273` redefines the *global* `.toast`
   and `.toast-stack` classes — and sets `.toast { opacity: 0 }`, only made
   visible by `.toast.is-visible`. Only upload's own `showToast` adds
   `.is-visible`.
2. **Stylesheets are inject-and-keep.** The shell adds a swapped page's
   stylesheets the first time it's visited and **never removes them**
   (`shell.js:82` `ensureStylesheets` — "inject-and-keep: add … if absent, never
   remove"). So once `/upload` has been visited, `upload.css`'s
   `.toast { opacity: 0 }` rule poisons the canonical `.toast` everywhere — and
   the shell/auth/admin toasts (which never set `.is-visible`) can render
   **invisible**.
3. **Two parallel systems on one page.** The upload page already includes the
   shared stacks (`{{template "auth-modals" .}}` at `upload.html:125`), yet it
   *also* renders its own `#toastStack` (`upload.html:29`) and routes every
   message through the lesser implementation #4. Any shell-originated toast on
   the upload page (queue replaced, "added to queue", queue-panel save errors)
   uses stacks #1's code path but is broken by upload.css's override.

Net effect: on the upload page the messages are a different shape (centered, no
icon/colors, no close, no error-persistence) **and** the global override risks
breaking the good toasts on other pages after `/upload` is visited.

---

## Goal

One canonical toast module in its own file, imported by every surface that shows
a transient message. Delete the divergent upload implementation, its private
`#toastStack`, and the colliding CSS. No behavior regressions on the library.

---

## Locked Decisions (proposed — confirm before implementing)

| Topic | Decision |
|---|---|
| Canonical implementation | Implementation **#1** (the `shell.js` `showToast`) — it is the superset (type + icon + close + inline action + per-call timeout + error persistence). |
| New file | `webui/static/js/toast.js` — exports `showToast(message, opts)`. Pure DOM, **zero imports**, no page/shell coupling. |
| Shared stacks | Keep `#toastStatus` (polite/status+success) and `#toastAlert` (assertive/errors) from the `auth-modals` partial as the single pair of mount points. The module no-ops if the target stack is absent (same as today). |
| CSS home | Keep canonical `.toast*` rules in `app.css`. **Remove** the colliding `.toast` / `.toast-stack` / `.toast.is-visible` rules from `upload.css`. |
| Back-compat for importers | `shell.js` **re-exports** `showToast` from `toast.js` (`export { showToast } from './toast.js'`) so existing `import { showToast } from './shell.js'` (app.js) keeps working with no churn. New importers import from `toast.js` directly. |
| Reuse breadth | "Reuse it everywhere we message something": route **upload, auth, and admin** through the shared module too — auth's `authToast` and admin's `toast` are exact duplicates, so folding them in is zero-behavior-change deduplication. (See Phase 4; can be deferred without blocking the upload fix.) |
| `/cmus` exclusion | **Out of scope by design.** `cmus.html` has neither `app.css` nor the `auth-modals` toast stacks; its auth toasts already no-op silently (pre-existing gap) and the shared module keeps no-opping when the stack is absent. Phase 4 neither fixes nor breaks cmus. "Everywhere" does **not** mean cmus — it keeps its standalone old header on purpose. |
| Folded-in timeout | Accept a **uniform 5000ms** auto-dismiss. auth and admin currently use 4000ms; we do **not** preserve that (would require passing `{ timeout: 4000 }`). Cosmetic; call it out in the PR. |
| Admin API stability | Keep `admin/shared.js`'s **exported `toast(msg, type)` name + signature**; only its body changes to delegate to the shared `showToast`. ~7 admin modules import it — none of them change. |
| Upload visual change | The upload page's toasts move from a centered fading bubble to the standard corner stack (status → above the player bar, errors → top). This is the intended unification, **not** a regression. |
| API shape | Unchanged from #1: `showToast(message, { type = 'status', actionLabel, onAction, timeout = 5000 } = {})`. The single-arg upload calls (`showToast('…')`) keep working since all options are optional. |
| Accessibility | Keep upload's separate `aria-live` SR region (`announce()` / `srStatus`) as-is — it is a *different* concern (screen-reader progress narration), not a visible toast. |

---

## Phase Execution Order

```
Phase 1 — Extract toast.js (canonical module)                 [no behavior change]
Phase 2 — Re-point shell.js at toast.js (re-export)           [no behavior change]
Phase 3 — Migrate the upload page (the actual bug fix)        [removes system #4]
Phase 4 — Fold in auth + admin duplicates (optional dedup)    [no behavior change]
Phase 5 — Verify
```

Phases 1–3 deliver the fix. Phase 4 is pure deduplication and can ship separately.

---

### Phase 1 — Extract `toast.js`

- **New** `webui/static/js/toast.js`: move the body of `shell.js`'s `showToast`
  verbatim into an exported `showToast`. No other code moves. Add a top-of-file
  comment describing the two stacks and the `type`/`actionLabel`/`timeout`
  contract.
- No callers change yet. `go build ./...` (asset embed) + load the library —
  identical behavior.

### Phase 2 — Re-point `shell.js`

- In `shell.js`, delete the local `showToast` definition and add
  `export { showToast } from './toast.js';` (keep the existing `import`/usage of
  `showToast` inside `shell.js` working by importing it at the top).
- `app.js` (`import { showToast } from './shell.js'`) is untouched and keeps
  working via the re-export.
- Library still works; queue-replaced toast + queue-panel toasts unchanged.

### Phase 3 — Migrate the upload page (the fix)

Files: `webui/static/js/upload.js`, `webui/html/upload.html`,
`webui/static/css/upload.css`.

1. `upload.js`: add `import { showToast } from './toast.js';` at the top.
2. `upload.js`: **delete** the local `showToast` (lines ~858-870) and the
   `toastStack` variable + its lookup (`let … toastStack …` ~line 35;
   `toastStack = document.getElementById('toastStack')` ~line 53).
3. `upload.js`: review the call sites and pass an explicit `type` where it adds
   value (all currently single-arg):
   - `:468` "N files staged…" → `{ type: 'success' }`
   - `:598` "Server busy — workers reduced…" → `{ type: 'status' }` (info)
   - `:697` "Cover uploaded…" → `{ type: 'success' }`
   - `:700` "Cover upload failed…" → `{ type: 'error' }`
   - `:752` "Removed …" → default status (or `success`)
   - `:762` `toast: msg => showToast(msg)` (injected into `file-list.js`
     `scope.toast`) → keep the wrapper; optionally widen later. file-list keeps
     receiving a `toast(msg)` function, so no change needed there.
   - `:791/:792` "Removed N; M failed" / "Removed N files" → `error` when
     `fail`, else `success`.
   - `:803` approval result → `success`.
4. `upload.html`: **remove** the `#toastStack` block + its comment (lines ~26-29).
   The page already includes `{{template "auth-modals" .}}` (`:125`), which
   provides `#toastStatus` / `#toastAlert`. Nothing else to add.
5. `upload.css`: **remove** the `.toast` / `.toast-stack` / `.toast.is-visible`
   rules (lines ~247-273) and the section comment. This kills the global
   override that poisoned the canonical `.toast` after `/upload` was visited.

### Phase 4 — Fold in the auth + admin duplicates (optional, zero-behavior dedup)

Severable from the upload fix — **ship as its own commit** (it touches auth +
admin, which are unrelated to the reported upload bug; isolating it keeps
`git bisect` clean if anything regresses).

**Pre-verified safe:** every web-UI entry point is `type="module"`, and `auth.js`
/ `admin/shared.js` are *only* ever pulled in through those module graphs
(`shell.js`→auth, `admin/*.js`→`admin/shared.js`, `cmus.js`→auth). Adding an
`import` to them cannot break a classic-script load — there isn't one. Admin
pages already load `app.css` and the `auth-modals` stacks and already emit the
identical `.toast` markup, so this adds **no** new coupling.

- `auth.js`: replace the private `authToast` with the shared `showToast`
  (import from `toast.js`). Remap the call sites from positional
  `authToast(msg, type)` → `showToast(msg, { type })`. **Careful:** the shared
  API takes an *options object*, not a positional `type` — passing a bare string
  silently drops the type (→ defaults to `status`). Only a handful of call
  sites; review each.
- `admin/shared.js`: keep the **exported `toast(msg, type)` name + signature**
  (every `admin/*.js` importer stays untouched); change only its body to call
  the shared `showToast(msg, { type })`. Accept the 4000ms → 5000ms timeout
  drift (uniform; see Locked Decisions).
- `'info'` (admin default) and `'status'` (canonical default) are functionally
  identical — both route to `#toastStatus` with the ℹ icon — so the default-type
  change is a no-op.
- Result: a single toast code path across shell, library, upload, auth, admin.
  `/cmus` stays excluded by design (no stacks → silent no-op, unchanged).

### Phase 5 — Verify

- `go build ./...` and `go test ./...` (asset embed compiles; no Go logic
  touched).
- **Restart the server** — webui assets are compile-time embedded, so edits are
  invisible until restart (known gotcha).
- Manual browser checklist:
  1. Library: add-to-queue / replace-queue / save-playlist toasts — appear in the
     corner, correct colors/icons, close button, error persists, action button
     (queue-replaced "click here") works.
  2. Upload: stage files → success toast in the standard stack; trigger a 429
     (or simulate) → "workers reduced" status toast; cover upload success +
     failure; remove/send-to-approval results.
  3. **Cross-page regression** (the original bug): visit `/upload` first, then
     navigate to the library and trigger a toast — it must still be **visible**
     (proves the upload.css override is gone).
  4. Admin + auth (if Phase 4 done): a login error and an admin action still
     toast correctly.

---

## Files Touched

| File | Phase | Change |
|---|---|---|
| `webui/static/js/toast.js` | 1 | **new** — canonical `showToast` |
| `webui/static/js/shell.js` | 2 | drop local def; `export { showToast } from './toast.js'` |
| `webui/static/js/upload.js` | 3 | import shared; delete local `showToast` + `toastStack`; add `type` to calls |
| `webui/html/upload.html` | 3 | remove `#toastStack` markup |
| `webui/static/css/upload.css` | 3 | remove colliding `.toast` / `.toast-stack` rules |
| `webui/static/js/auth.js` | 4 | replace `authToast` with shared |
| `webui/static/js/admin/shared.js` | 4 | delegate `toast()` to shared |

No Go, DB, or API changes. No new dependencies.

---

## Risks & Notes

- **Inject-and-keep CSS is the root cause.** Even with the upload toast migrated,
  leaving the `.toast` override in `upload.css` would keep the cross-page bug
  alive — removing those rules (Phase 3.5) is the load-bearing step, not the JS.
- **Visual change on the upload page is intended.** Centered bubble → corner
  stack. Flag in the PR so it isn't mistaken for a regression.
- **Module-eval safety (shell pages).** `toast.js` must do no page DOM work at
  import time — it only defines a function and looks up the stack lazily *inside*
  `showToast`. This matches the shell's "no DOM at module-eval" rule; the upload
  page is shell-native so this matters.
- **No JS unit test harness for DOM.** `tests/js` runs under `node --test` with
  no jsdom, so toasts stay manual-verified (the existing `queue-ops` test is
  pure arithmetic). Optional: a tiny stub test asserting `showToast` no-ops when
  the stack is missing; low value, can skip.
- **Phase 4 is severable.** If we want the smallest possible fix PR, ship Phases
  1–3 (and 5) and leave auth/admin dedup for a follow-up — they aren't broken,
  just duplicated.
