# Cross-Shell Upload Page

Make the upload page render inside **whichever shell it was reached from**: the
admin shell (admin nav, no persistent player) when entered from the admin panel,
and the listening shell (persistent header + player-bar) when entered from the
listening pages. Today the admin "Upload" link drops the user completely out of
the admin panel into the listening shell, because the upload page only exists at
`/upload`, a listening-shell page.

## Background — the two shells

The web UI has two distinct chromes, by design:

- **Listening shell** (`webui/static/js/shell.js`) — wraps `/`, `/playlists`,
  `/upload`. A client-side router with a persistent header + player-bar +
  `<audio>` that survive navigation. Pages opt in with
  `<body data-module="/static/js/x.js">`; the shell imports the module and runs
  its `init()`/`teardown()` on entry/exit.
- **Admin shell** (`{{define "admin-shell"}}` in `partials.html` +
  `bootAdmin()` in `static/js/admin/shared.js`) — wraps `/admin` and
  `/admin/{library,users,prune,settings}`. These are **full-page loads**, no
  client router, no persistent player (each page wires its own page-local
  preview player, e.g. `admin/library.js`).

`shell.js` deliberately refuses to intercept `/admin*` links (shell.js:142), and
the admin nav's "Upload" link points at `/upload` (partials.html:232,
dashboard.html:38). So clicking it is a hard browser navigation that boots the
listening shell — the reported behavior. It is not a bug; `/upload` simply only
exists in one shell.

## Decision

Add a **parallel admin route, `/admin/upload`**, that wraps the *same* upload
`<main>` markup and the *same* `upload.js` core in the admin shell. `/upload`
keeps wrapping them in the listening shell. The admin nav's "Upload" link points
to `/admin/upload`; the listening nav stays `/upload`.

The two requirements that make this clean rather than a fork:

1. **One copy of the markup.** Factor the upload `<main>` (drop-zone, tabs,
   queue, "My uploads" pane) into a shared template partial
   `{{define "upload-main"}}`; both `upload.html` and the new
   `admin/upload.html` include it. Single source of truth.
2. **One preview seam.** `upload.js` plays the "My uploads" preview through the
   shell-owned player singleton (`playMine` → `getController().setQueue`,
   upload.js:825-836), which does not exist on admin pages. `file-list.js` is
   already host-neutral (`onPlay(file, files)` callback, "page owns the
   player"), and `admin/library.js` already runs a page-local preview player. So
   the only behavioral adaptation is: route the preview through a host-supplied
   player. Everything else (`init()`/`teardown()`, the queue, hashing, staging)
   is identical.

### Rejected alternatives

- **Turn the admin pages into a routed shell** so `/upload` can swap in — large
  refactor that undoes the deliberate "admin = plain full-load, no persistent
  player" decision. Overkill for "show the admin nav around upload."
- **Iframe `/upload` inside an admin page** — cheap, but iframes bring
  height/focus/file-picker/toast friction for a first-party page, and the
  preview wouldn't integrate. No.
- **One route, sniff the referrer to pick chrome** — fragile. No.

## Design

### Routing & gating

`/admin/upload` is registered in `RegisterAdminPage` (webui.go). Crucially, the
admin **pages** are *ungated server-side* — `RegisterAdminPage` is mounted
without `RequirePermission`, by design, so an admin page can render its own login
prompt (only the destructive `/api/admin/*` API from `RegisterAdmin` is
`file.delete`-gated). So `/admin/upload` returns its HTML to anyone, exactly like
`/admin/library` etc. That is safe because the page is a static upload UI with no
sensitive data, and **every mutating action is enforced server-side**: the
upload POST hits `/files/upload` / `/api/my/uploads` (gated `file.upload`), and
metadata/access edits hit `/api/admin/*` (gated `file.delete`). Same `upload.js`,
same gated API as `/upload`.

The visible content is gated **client-side**, in two layers, and no extra
`file.upload` guard is needed:

1. `bootAdmin()` runs `gatePage(['file.delete','user.manage'])` — admin-access.
   A principal without it (anonymous, or a plain uploader) gets the access-denied
   / login notice and `init()` never runs.
2. `upload.js`'s `init()` then runs `gatePage(['file.upload'])`.

The only admin-access roles are **admin** (holds every permission incl.
`file.upload` — `003_auth.sql`) and **moderator** (granted `file.upload` in
`017_review_bucket.sql`), so both clear layer 2 too. A plain uploader who hand-
navigates to `/admin/upload` is stopped at layer 1 and uses `/upload` instead.

### Markup partial

Move the contents of `<main class="upload-main">…</main>` from `upload.html`
into `{{define "upload-main"}}` in `partials.html` (next to the other shared
defines). `upload.html` becomes:

```
<main class="upload-main">{{template "upload-main" .}}</main>
```

and `admin/upload.html` renders `{{template "admin-shell" .}}` then the same
`<main>`. The visually-hidden `#srStatus` region and the comment block move with
it. No behavioral change to `upload.html`.

### Preview seam in upload.js

`init()` currently takes no args (shell.js calls `mod.init?.()`). Widen it to
accept an optional options object and default to the shell controller, so the
listening shell is unchanged:

```js
let previewPlay = (tracks, idx) => getController().setQueue(tracks, idx);

export async function init(opts = {}) {
  if (opts.preview) previewPlay = opts.preview;
  ...
}
```

`playMine` calls `previewPlay(tracks, idx)` instead of
`getController().setQueue` directly. The admin host passes a page-local player
via `opts.preview`.

The page-local player is wired **inline in the admin upload boot module** by
reusing `createPlayer` from `static/js/player.js` and the `player-bar` partial —
exactly the page-local player `admin/library.js` already uses. The boot keeps a
small play context (`items`/`index`) so prev/next/ended walk the previewed list,
and exposes `previewPlay(tracks, idx)` matching the shell controller's
`setQueue` signature. No new player module and no shell controller; just a few
lines of context wiring around the existing `createPlayer`.

### Admin boot module

New `webui/static/js/admin/upload.js` (distinct from the core
`static/js/upload.js`), a thin boot mirroring the other admin pages:

```js
import { bootAdmin } from './shared.js';
import { createPlayer } from '../player.js';
import { init } from '../upload.js';
// previewPlay drives createPlayer over a small play context (see library.js)

const identity = await bootAdmin();
if (identity) await init({ preview: previewPlay });
```

`admin/upload.html` loads it via `<script type="module"
src="/static/js/admin/upload.js">` (admin pages load their module directly,
there is no shell.js here). Teardown isn't needed — admin pages are full reloads.

### CSS

`admin/upload.html` links the union of what both shells need: `app.css`,
`admin-shell.css`, `upload.css`, `file-view.css`, `player.css` (for the
page-local preview). `file-view.css` is self-sufficient (per the
file-management-view design), so the "My uploads" list renders correctly without
the listening CSS.

### Nav wiring

- `partials.html` admin-shell (line 232): `/upload` → `/admin/upload`, with the
  standard `{{if eq .SubPage "upload"}}…{{end}}` active-state treatment.
- `dashboard.html` Upload dash-card (line 38): `/upload` → `/admin/upload`.
- Listening header keeps `/upload` (partials.html:7) untouched.

## Plan

**Phase 1 — Markup partial (no behavior change).**
- Extract `{{define "upload-main"}}` into `partials.html`; reduce `upload.html`
  to include it. Verify `/upload` renders and uploads exactly as before.

**Phase 2 — upload.js preview seam.**
- Add the `previewPlay` indirection + optional `init(opts)`; route `playMine`
  through it. Listening shell behavior unchanged (default = shell controller).

**Phase 3 — Admin upload page.**
- Add `webui/html/admin/upload.html` (admin-shell + the partial + CSS links).
- Add `webui/static/js/admin/upload.js` (plain `bootAdmin()`, a minimal inline
  preview player, `init({ preview })`).
- Register `"upload"` in `adminSubPages` (webui.go) → `/admin/upload`, `.SubPage
  = "upload"`. `webui_test.go`'s `TestAdminSubPagesRender` picks it up
  automatically.

**Phase 4 — Nav wiring.**
- Point the admin-shell nav link and the dashboard dash-card at `/admin/upload`;
  add the `.SubPage == "upload"` active state to the admin nav link.

**Phase 5 — Verify (manual browser checklist).**
- From `/admin` → Upload: stays in the admin shell (admin banner + nav), no
  player-bar; drop/queue/upload a file; "My uploads" tab lists it; preview plays
  via the page-local player.
- From the listening header → Upload: unchanged (player-bar present, preview via
  the persistent queue).
- An admin without `file.upload` sees the "Not available" notice at
  `/admin/upload`.
- `go build ./...`, `go test ./...`, `node --test tests/js/queue-ops.test.mjs`.

## Files touched

- `webui/html/partials.html` — new `{{define "upload-main"}}`; admin-nav link.
- `webui/html/upload.html` — include the partial.
- `webui/html/admin/upload.html` — new.
- `webui/html/admin/dashboard.html` — dash-card href.
- `webui/static/js/upload.js` — `previewPlay` seam + optional `init(opts)`.
- `webui/static/js/admin/upload.js` — new boot module (incl. inline preview).
- `webui/webui.go` — `adminSubPages["upload"]`.
- CSS: no new files; `admin/upload.html` links existing stylesheets.

## Security summary

No new permission surface. `/admin/upload` is served ungated like every other
admin page (it renders the login prompt for anonymous users), but it is a static
upload UI with no sensitive data, the visible content is client-gated by
`bootAdmin()` + `init()`'s `gatePage`, and **all writes are enforced
server-side** through the same `file.upload` / `file.delete`-gated endpoints
`/upload` already uses. The change is chrome + a preview sink, not auth.
