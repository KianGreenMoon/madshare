# Web UI shells

The web UI has **two distinct chromes**, by design. A page's shell decides its
header, navigation, and whether playback persists across navigation.

## Listening shell

`webui/static/js/shell.js` — a client-side router wrapping `/library`,
`/playlists`, `/upload`, `/settings` and the `/madnetwork` section. A persistent header +
player-bar + `<audio>` live outside `<main>` and
**survive navigation**, so playback and the shared queue are continuous across
these pages (see `docs/ui/player-and-queue.md`). A page opts in with
`<body data-module="/static/js/x.js">`; the shell imports the module and runs its
`init()` / `teardown()` on entry / exit. `shell.js` deliberately does **not**
intercept `/admin*` links — those are hard navigations into the other shell.

### `/` is a front door, not a page

The root URL holds no content. `webui.homeHandler` `302`s it to the section this
node opens on: **`/madnetwork`** when `[federation].enabled` is set *and* the
caller holds `madnetwork.access`, otherwise **`/library`**. A node that federates
is one whose interesting surface is the network, and a node that doesn't has no
network to show — so the entry URL states which kind of node this is instead of
making every install the same.

The madnetwork arm repeats that page's own gate (`madnetwork.access`): `/` is
exactly where an anonymous visitor and a listener-only account arrive, and
forwarding them to a page that answers them nothing would make the front door a
dead end. Those principals land on `/library`. Because the target depends on who
asks, the redirect carries `Cache-Control: no-store`.

**The Madshare wordmark is the one link to `/`** (owner decision 2026-08-02) —
it stands for "wherever this server starts", which is exactly what the front door
means, and it is the only header element that stands for no particular page. It
deliberately carries no `.nav-link` class, so `setActiveNav` never marks it
active. Everything that means a *page* addresses that page: the header tab, the
Music subtab and the network page's *Local library* lane all point at `/library`
directly, so a click is one request rather than a redirect hop.

Clicking the wordmark inside the shell therefore goes through the redirect, which
is why `shell.js`'s `navigate()` adopts `res.url` when `res.redirected`: the
address bar ends on `/madnetwork` or `/library`, history holds that entry, and
Back returns to the page you came from instead of bouncing through the door again.

### Library section + subtabs

The header nav lists **sections**, not individual pages. **Library** is one
section spanning a subtab bar (`{{define "library-subnav"}}` in `partials.html`,
class `.subtabs`) rendered at the top of `<main>` on each listening page:
**Music** (`/library`, the artist/album browse) and **Playlists** (`/playlists`) today,
with Most played / Recently added / Podcasts intended later — each is just another
`.subtab` link plus its route. Upload is its own section.

Active state is two-level:

- The **header "Library" tab** stays active across all its subtabs. Pages carry
  `Section` (`pageData.Section`, `"library"` for `/library` and `/playlists`) rendered
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

### Header auth state is server-rendered

The header's auth-dependent parts are rendered **server-side per request**
(`webui.makeHandler` fills `pageData` from the request-context identity), so the
first paint is already correct — no FOUC. Specifically: the **Upload** / **Admin**
nav links and the **Playlists** subtab are emitted only for a principal that holds
the permission, and the user area shows the username + **Log out** (vs. **Sign in**)
straight away rather than swapping in after a `/api/auth/me` round-trip. This is a
UX hint only — the API still enforces every gate — so the per-user HTML is sent
`Cache-Control: no-store`. Login and logout both reload the page, so the server
re-renders on every auth-state change; the client never has to *add* links.

`applyNavPermissions` (auth.js) stays as the **client-side reconciler**: it removes
a link/subtab the server rendered for a session that has since changed (e.g. signed
out in another tab), and re-gates the swapped-in subtab bar after each navigation.
It only ever removes, and is idempotent — for the common case it's a no-op.

### Responsive header (narrow widths)

The header is a single non-wrapping flex row. With every link visible it cannot
fit a phone-width viewport — worst as an admin, who has the most links — so the
buttons used to spill past the edge, widen the document, and force a horizontal
page scroll. The fix collapses the header into a **☰ overflow menu** below a
breakpoint (shared by both shells, since the `{{define "header"}}` partial is):

- **Logo + Library + Madnetwork + About stay pinned** inline on the left at every
  width (owner decisions: Library is the primary tab and always one tap away;
  Madnetwork joined it 2026-08-02 — the two libraries a person moves between are
  the header's primary pair, so the network tab must not be a page down under the
  ☰ on a phone; About is kept a visible button, never folded into the ☰ — judged
  inconvenient under the menu). Library and Madnetwork are `.nav-link`s carrying
  `data-section` (so `setActiveNav` still lights them) plus `.nav-link--pinned`
  (`flex-shrink: 0`); they and the `.about` flyout render *outside* the
  collapsible group.

  Because Madnetwork is gated on `madnetwork.access` yet sits outside `.main-nav`,
  `auth.js`'s `applyNavPermissions` scopes its removal to the whole `<header>`.
  Scoping it to `.main-nav` — as it did while every gated tab lived there — would
  leave the pinned tab on screen for a principal who lost the permission.
- Everything else — **Upload, Admin, the user area / Sign in** — lives in a single
  `#navCollapse` wrapper. On wide screens that wrapper is `display: contents`, so
  its children (`.main-nav`, `.header-actions`) are direct header flex items laid
  out **exactly as before** (`.main-nav`'s `margin-right: auto` still pushes the user
  area to the far right); the **`#navToggle` ☰ button is hidden**.
- Below **720px** (`app.css` media query) the toggle appears (pushed right with
  `margin-left: auto`) and `#navCollapse` becomes an absolutely-positioned dropdown
  panel anchored under the sticky header, hidden until `.is-open`. Inside it the
  links stack vertically; a hairline divider sets off the user area. The header row
  is then `logo · Library · Madnetwork · About ▾ · ☰`. Adding a fourth pinned item
  is one more than the row was measured for, so the same media query tightens the
  row's own spacing — smaller header padding, a 16px wordmark, 5px side padding on
  the pinned tabs, all via direct-child selectors so the ☰ panel keeps its own
  metrics. Verified to fit without horizontal overflow down to a 320px viewport.

  The About flyout (`.about__menu`) is shown/hidden via an `.is-open` **class**, not
  the `hidden` attribute — the UA stylesheet's `[hidden] { display: none !important }`
  outranks all author CSS (UA `!important` beats even author `!important`), so an
  element keeping `[hidden]` can never be re-shown by CSS.

`nav-menu.js` (`initNavMenu`) wires the open/close, mirroring `about-menu.js`:
toggle, close on outside-click / Escape, and close when any item inside the panel
is chosen. It runs once per page context — `shell.js` at boot (the persistent
header survives shell swaps) and `bootAdmin()` on each admin full load — next to
`initAboutMenu()`. Because the inner nav keeps the `.main-nav` class and the
Upload/Admin links, `applyNavPermissions` still finds and gates them unchanged.

## Admin shell

`{{define "admin-shell"}}` (`webui/html/partials.html`) + `bootAdmin()`
(`webui/static/js/admin/shared.js`) — wraps `/admin` and every page in
`webui.adminSubPages` (the nav strip in the partial and the route table are the
same list: library, upload, users, prune, duplicates, sources, network,
upgrades, cache, swarm, settings). These are **full-page loads**: no client
router, no persistent player. Each page wires its own **page-local preview
player** from `createPlayer` (`player.js`) — e.g. `admin/library.js`.

Shared chrome lives in `admin-shell.css`, and two classes there are worth
knowing before styling a new list: **`.admin-search` / `.admin-select`** are the
filter row's box and dropdown. Native form controls are the one thing on these
pages that ignores the theme — an unstyled `<select>` stays light on a dark
page — so a list that grows a filter row uses them rather than minting another
private copy (`file-view.css`'s `.files-search input` and `admin-recordings.css`'s
`.rec-search` predate them and stay). Prose links inside `.admin-main` are styled
there too, for unclassed anchors only, so every component keeps its own look.

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
