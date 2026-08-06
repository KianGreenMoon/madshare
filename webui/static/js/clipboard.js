// clipboard.js — copying text on the origins madshare is actually deployed on.
//
// `navigator.clipboard` is a SECURE-CONTEXT api: over plain http:// to anything
// but localhost the property is undefined and there is nothing to catch. That is
// the ordinary madshare deployment — a node reached on its yggdrasil address, or
// behind a plain-http reverse proxy — so the modern API is the fallback case
// here, not the main path. A copy button that only works under TLS is a copy
// button that does not work, and the things it copies (a node card, a mesh
// address, a public key, an API token) are exactly the strings nobody should be
// retyping by hand.
//
// `document.execCommand('copy')` has no such gate: it needs a selection and the
// user gesture a click handler already has. It is deprecated and it is the only
// thing that copies on these origins, so it stays. When even that fails, the
// caller selects the text on screen (selectElementText) so Ctrl/Cmd+C works —
// which is the manual step this whole module exists to remove.
//
// No page DOM at module scope: this file is imported by both shells and tested
// directly (tests/js/clipboard.test.mjs).

// copyText puts `text` on the clipboard and resolves true when it got there.
// MUST be called from within a user gesture — the legacy path is only permitted
// during one, and the async path is subject to the same transient activation on
// some browsers.
export async function copyText(text) {
  const s = String(text ?? '');
  if (!s) return false;

  // isSecureContext is the honest gate. Some browsers expose the object on an
  // insecure origin and reject (or silently do nothing) on use; asking first
  // keeps the failure out of the path that still has a working alternative.
  if (typeof window !== 'undefined' && window.isSecureContext !== false
      && navigator?.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(s);
      return true;
    } catch {
      // Denied permission, an unfocused document, a browser that lied about the
      // context — all of them still leave the legacy path worth trying.
    }
  }
  return legacyCopy(s);
}

// selectElementText selects an element's text so the reader can copy it by hand.
// The last resort, and also what a caller should do when copyText returns false.
export function selectElementText(el) {
  if (!el || typeof document === 'undefined') return false;
  try {
    const sel = window.getSelection?.();
    if (!sel) return false;
    const range = document.createRange();
    range.selectNodeContents(el);
    sel.removeAllRanges();
    sel.addRange(range);
    return true;
  } catch {
    return false;
  }
}

// legacyCopy copies via a throwaway textarea. Everything it touches — the
// focused element, the user's own selection — is put back, because copying a
// key should not move the page under someone mid-read.
function legacyCopy(text) {
  if (typeof document === 'undefined' || !document.body) return false;
  if (typeof document.execCommand !== 'function') return false;

  const ta = document.createElement('textarea');
  ta.value = text;
  ta.setAttribute('readonly', '');    // keeps the soft keyboard down on mobile
  ta.setAttribute('aria-hidden', 'true');
  ta.tabIndex = -1;
  // Present in the layout but invisible: display:none and visibility:hidden are
  // not selectable, and a textarea placed in flow would scroll the page.
  Object.assign(ta.style, {
    position: 'fixed', top: '0', left: '0', width: '1px', height: '1px',
    padding: '0', border: 'none', outline: 'none', boxShadow: 'none',
    background: 'transparent', opacity: '0',
  });

  const active = document.activeElement;
  const saved = saveSelection();
  document.body.appendChild(ta);
  let ok = false;
  try {
    ta.focus({ preventScroll: true });
    ta.select();
    ta.setSelectionRange?.(0, text.length);   // iOS ignores select() on readonly
    ok = document.execCommand('copy') === true;
  } catch {
    ok = false;
  }
  ta.remove();
  restoreSelection(saved);
  if (active && active !== document.body && typeof active.focus === 'function') {
    try { active.focus({ preventScroll: true }); } catch { /* gone from the DOM */ }
  }
  return ok;
}

function saveSelection() {
  const sel = window.getSelection?.();
  if (!sel || !sel.rangeCount) return null;
  const ranges = [];
  for (let i = 0; i < sel.rangeCount; i++) ranges.push(sel.getRangeAt(i));
  return ranges;
}

function restoreSelection(ranges) {
  const sel = window.getSelection?.();
  if (!sel) return;
  try {
    sel.removeAllRanges();                    // drops the textarea's selection
    if (ranges) for (const r of ranges) sel.addRange(r);
  } catch { /* the saved ranges' nodes may be gone; an empty selection is fine */ }
}
