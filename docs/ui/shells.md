# Web UI shells

The web UI has **two distinct chromes**, by design. A page's shell decides its
header, navigation, and whether playback persists across navigation.

## Listening shell

`webui/static/js/shell.js` — a client-side router wrapping `/`, `/playlists`, and
`/upload`. A persistent header + player-bar + `<audio>` live outside `<main>` and
**survive navigation**, so playback and the shared queue are continuous across
these pages (see `docs/ui/player-and-queue.md`). A page opts in with
`<body data-module="/static/js/x.js">`; the shell imports the module and runs its
`init()` / `teardown()` on entry / exit. `shell.js` deliberately does **not**
intercept `/admin*` links — those are hard navigations into the other shell.

## Admin shell

`{{define "admin-shell"}}` (`webui/html/partials.html`) + `bootAdmin()`
(`webui/static/js/admin/shared.js`) — wraps `/admin` and
`/admin/{library,users,prune,settings,upload,moderation}`. These are **full-page
loads**: no client router, no persistent player. Each page wires its own
**page-local preview player** from `createPlayer` (`player.js`) — e.g.
`admin/library.js`.

Admin **pages** are served *ungated* (`RegisterAdminPage`, no
`RequirePermission`) so a page can render its own login prompt; the page content
is gated **client-side** by `bootAdmin()` → `gatePage(['file.delete',
'user.manage'])`. The actual protection is server-side on the destructive
`/api/admin/*` API (`RegisterAdmin`, `file.delete`-gated) — the page/route split
is topology, not access control (see `docs/architecture/auth.md`).

## Cross-shell upload page

The upload UI exists in **both** shells so the admin "Upload" link keeps the admin
chrome instead of dropping the user into the listening shell:

- `/upload` — the upload `<main>` wrapped in the listening shell.
- `/admin/upload` — the **same** markup and the **same** `upload.js` core wrapped
  in the admin shell.

Two seams keep this one implementation rather than a fork:

1. **One copy of the markup.** The upload `<main>` (drop-zone, tabs, queue, *My
   uploads* pane) is the shared `{{define "upload-main"}}` partial; both
   `upload.html` and `admin/upload.html` include it.
2. **One preview seam.** `upload.js`'s `init(opts)` takes an optional
   `opts.preview(tracks, idx)` sink (default = the shell controller's `setQueue`).
   The admin boot module (`admin/upload.js`) passes a page-local `createPlayer` as
   the sink, since the persistent controller does not exist on admin pages. The
   `file-list.js` "My uploads" list is already host-neutral (`onPlay` callback).

`/admin/upload` is client-gated like every admin page; all writes hit the same
`file.upload`-gated upload endpoints `/upload` uses, so no new permission surface
is introduced.

## cmus

`/cmus` (`webui/html/cmus.html`) is a **paused, standalone** view: its own header,
its own player design, and outside both shells. Bringing it onto the shared
player/shell is deferred (`docs/plans/roadmap.md`).
