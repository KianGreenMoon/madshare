// cover-edit.js — the shared "Add cover" picker used by the grouped file view.
// Self-contained: it owns a hidden <input type=file>, validates JPEG/PNG ≤10MB,
// and POSTs to the artist/album cover endpoints. No page DOM dependency, so the
// same picker works on the admin pages and the (shell-native) upload page.
//
// Add-only by design: the affordance is offered only when the artist/album has
// no cover yet (the grouped separator hides the button otherwise). Replacing an
// existing cover stays in the admin By-entity view; the server also enforces
// this — a file.upload-only caller gets 403 trying to overwrite.

const MAX_BYTES = 10 * 1024 * 1024;

/**
 * createCoverPicker returns { pick(target), destroy() }.
 *   apiBase     URL prefix the scope already uses for its fetches ('' = same-origin)
 *   toast       (msg, type) notifier
 *   checkAuth   (res) => bool — handle 401/redirect; return true to abort
 *   onUploaded  (target) => void — called after a successful upload (e.g. reload)
 *
 * target: { kind:'artist'|'album', artist, album? }
 */
export function createCoverPicker({ apiBase = '', toast = () => {}, checkAuth, onUploaded } = {}) {
  const input = document.createElement('input');
  input.type = 'file';
  input.accept = 'image/jpeg,image/png';
  input.style.display = 'none';
  document.body.appendChild(input);

  let target = null;

  input.addEventListener('change', () => {
    const file = input.files[0];
    const t = target;
    target = null;
    input.value = '';            // allow re-picking the same file next time
    if (file && t) upload(t, file);
  });

  function pick(t) {
    target = t;
    input.value = '';
    input.click();
  }

  async function upload(t, file) {
    if (!['image/jpeg', 'image/png'].includes(file.type)) { toast('Cover must be a JPEG or PNG.', 'error'); return; }
    if (file.size > MAX_BYTES) { toast('Cover must be 10 MB or smaller.', 'error'); return; }

    const path = t.kind === 'artist'
      ? `/api/artists/${encodeURIComponent(t.artist)}/image`
      : `/api/albums/${encodeURIComponent(t.album)}/image?artist=${encodeURIComponent(t.artist || '')}`;
    const fd = new FormData();
    fd.append('image', file);

    try {
      const res = await fetch(`${apiBase}${path}`, { method: 'POST', body: fd });
      if (checkAuth && checkAuth(res)) return;
      if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
      toast('Cover added.', 'success');
      onUploaded && onUploaded(t);
    } catch (err) {
      toast(`Couldn’t add cover: ${err.message}`, 'error');
    }
  }

  function destroy() { input.remove(); }

  return { pick, destroy };
}
