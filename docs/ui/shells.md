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

### Library section + subtabs

The header nav lists **sections**, not individual pages. **Library** is one
section spanning a subtab bar (`{{define "library-subnav"}}` in `partials.html`,
class `.section-tabs`) rendered at the top of `<main>` on each listening page:
**Music** (`/`, the artist/album browse) and **Playlists** (`/playlists`) today,
with Most played / Recently added / Podcasts intended later — each is just another
`.section-tab` link plus its route. Upload is its own section.

Active state is two-level:

- The **header "Library" tab** stays active across all its subtabs. Pages carry
  `Section` (`pageData.Section`, `"library"` for `/` and `/playlists`) rendered
  into `<body data-section>` and onto the header link's `data-section`;
  `shell.js`'s `setActiveNav` lights a header link whose `data-section` matches the
  body's, falling back to an exact path match for section-less links (Upload).
- The **subtab bar** marks the open subtab from `pageData.Page` (`"library"` =
  Music, `"playlists"`). It lives inside `<main>`, so the shell swaps it with the
  page and the server-rendered active state stays correct after a client swap.
  It's styled as an underline tab strip matching the header `.nav-link`, so the
  app's tabs look the same throughout.

The subtab is each section's **sole label and back-to-top affordance**, so the
breadcrumb never repeats the section name (no "Music / Library", "Playlists /
Playlists"): it holds only the drill path *below* the section root (artist /
album, or the open playlist's name) and is hidden entirely at a section's top
level. Clicking the active subtab returns to that top.

The subtab bar is permission-gated like the header: `applyNavPermissions` (auth.js)
removes the Playlists tab for a principal without `content.access`, and the shell
re-runs it after every swap (the swapped-in bar always ships all tabs).

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

### Uploads survive in-shell navigation

Leaving `/upload` for another **listening-shell** page (e.g. Library) must not lose
the intake queue or abort in-progress uploads. The shell never reloads the
document — it only swaps `<main>` — so the JS heap (the picked `File` objects and
live `XMLHttpRequest`s) survives; the only reason work was lost before was that the
page module's `teardown()` deliberately aborted everything and cleared the queue.

The fix mirrors the player: the upload engine is a document-lifetime **singleton**,
`upload-controller.js` (`getUploadController()`), and `upload.js` is a thin view.

- **Controller owns** the queue/groups/folders state, the worker pool, the in-flight
  XHR set, the pump loop, and the per-row rendering — including the **persistent
  `<ul class="queue-list">` node**, which it creates once and keeps alive across
  swaps (a detached-but-referenced node stays valid and keeps updating). It exposes
  `addEntries / start / clear / setWorkerCap / ensureConfig` and emits `change`,
  `announce`, `workercap`, `staged`.
- **View (`upload.js`)** owns the swappable chrome (drop-zone, slider, buttons,
  tabs, *My uploads* list). On `init()` it re-attaches the controller's `<ul>` into
  the freshly-swapped `<main>`, wires chrome → controller, subscribes to the events
  to sync chrome, and reflects current state. Its `teardown()` only detaches the
  view's listeners — it **does not** abort uploads, terminate workers, or clear the
  queue. So you can navigate away mid-upload and find it still running on return.

Two boundaries:

- **Survives in-shell navigation, not a hard refresh / tab close.** Browser security
  forbids re-reading a user-picked file after a real reload without re-selection, so
  the queue can't be persisted to storage. A document-level `beforeunload` guard
  (registered by the controller, active only while work is in flight) shows the
  native "Leave site?" prompt to prevent *accidental* loss on refresh/close.
- **Admin is full-page navigation.** Admin pages are not a client router (see *Admin
  shell*), so admin→admin navigation reloads the document and the heap is gone —
  uploads cannot survive it without first making admin a single-document shell. The
  same `beforeunload` guard *does* fire on admin navigation (it's a real navigation),
  so an in-progress admin upload at least prompts before it's lost. The controller is
  reusable as-is if the admin section ever becomes a client shell.

## cmus

`/cmus` (`webui/html/cmus.html`) is a **paused, standalone** view: its own header,
its own player design, and outside both shells. Bringing it onto the shared
player/shell is deferred (`docs/plans/roadmap.md`).
