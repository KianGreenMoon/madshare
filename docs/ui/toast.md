# Toasts — transient messages

One canonical toast module, `webui/static/js/toast.js`, is the single system for
every transient pop-up message in the web UI. It exports:

```js
showToast(message, { type = 'status', actionLabel, onAction, timeout = 5000 } = {})
```

- **`type`** — `status` (informational, default), `success`, or `error`. Drives the
  icon and which stack the toast mounts in.
- **`actionLabel` + `onAction`** — render an inline action button (e.g. the
  queue-replaced *"Undo — restore my queue"*).
- **`timeout`** — auto-dismiss in ms; **errors persist** until dismissed.

## Mount points

Two stacks from the `auth-modals` partial (`webui/html/partials.html`):

| Stack | `aria-live` | Carries |
|-------|-------------|---------|
| `#toastStatus` | polite | `status` + `success` (above the player bar) |
| `#toastAlert`  | assertive | `error` (top of the page) |

`showToast` looks the stack up **lazily inside the call** and **no-ops if it is
absent** — so a page without the stacks simply shows nothing
rather than erroring. The module does no DOM work at import time (it only defines
the function), which satisfies the shell's "no DOM at module-eval" rule so it is
safe on shell-native pages.

## Who uses it

- **`shell.js`** re-exports it (`export { showToast } from './toast.js'`) so older
  `import { showToast } from './shell.js'` callers (e.g. `app.js`) keep working.
- **`upload.js`** imports it directly.
- **`auth.js`** and **`admin/shared.js`** delegate to it — `admin/shared.js` keeps
  its exported `toast(msg, type)` name/signature (≈7 admin modules import it) and
  just forwards to `showToast(msg, { type })`.

## Styling

The canonical `.toast` / `.toast-icon` / `.toast-msg` / `.toast-close` /
`.toast-action` rules live in `app.css` — the single source. A page-local
stylesheet must **never** redefine these global classes: the shell injects a
swapped page's stylesheet on first visit and never removes it, so a page-local
`.toast` override leaks to every page once that page has been visited.

## Not a toast

The upload page's separate `aria-live` screen-reader region (`announce()` /
`#srStatus`) is a different concern — progress narration for assistive tech, not a
visible pop-up — and stays independent of this module.
