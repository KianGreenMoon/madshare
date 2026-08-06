# Copying — one module, because the deployment is plain HTTP

One canonical module, `webui/static/js/clipboard.js`, is how the web UI puts text
on the clipboard. **Nothing calls `navigator.clipboard` directly.**

```js
copyText(text)             // → Promise<boolean>: true when the text is on the clipboard
selectElementText(el)      // → boolean: leaves the shown text selected for Ctrl/Cmd+C
```

## Why it exists

`navigator.clipboard` is a **secure-context** API. On `http://` to anything but
`localhost` the property is `undefined` — there is no call to make and no error to
catch. That is not an edge case here, it is the ordinary madshare deployment: a
node reached on its yggdrasil address, or through a plain-HTTP reverse proxy.

So every copy control in the UI was dead on the origins the UI is actually used
from, and the things they copy — a node card, a mesh address, a public key, an API
token — are precisely the strings nobody should be retyping by hand. On the node
card it was worse than dead: the button was drawn only when `navigator.clipboard`
existed, so the control that hands you the address of the page you are looking at
was simply absent.

`copyText` therefore treats the modern API as the *fallback-eligible* path, not the
only one:

1. **`navigator.clipboard.writeText`** — but only when `window.isSecureContext` is
   not `false`. Some browsers expose the object on an insecure origin and then
   reject; asking first keeps the failure out of the path that still has a working
   alternative.
2. **`document.execCommand('copy')`** over an off-screen `<textarea>`. Deprecated,
   and the only thing that copies on these origins, so it stays. It needs a user
   gesture — which is why `copyText` **must be called from inside a click handler**
   — and it restores both the focused element and the reader's own selection.
3. **Neither** — the caller falls back to `selectElementText` on the element that
   shows the same value, so the copy is one keystroke away rather than a careful
   drag across 64 hex characters. That manual step is what this module exists to
   remove, so it is the failure message, never the design.

## Who uses it

| Surface | Copies |
|---|---|
| `admin/network.js` | mesh address, public key, node card JSON (`copyToClipboard` adds the toast + the select-on-failure) |
| `mn-nodes.js` (`buildNodeCard`) | the node key on `/madnetwork/node/{key}`; reports back through `onCopy(ok, selected)` so the page names the keystroke |
| `settings.js` | the one-time reveal of a new API token |

## Testing

The module does no DOM work at import time, so it imports directly:
`tests/js/clipboard.test.mjs` runs it against a stub document and pins the cases
that matter — no clipboard object at all, an insecure context that has one anyway,
a rejected `writeText`, a refused `execCommand`, and the cleanup (textarea removed,
focus and selection restored).
