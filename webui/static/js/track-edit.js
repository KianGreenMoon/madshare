// Shared per-track metadata editor — one modal editing the four base tags
// (title / artist / album artist / album) of a single file.
//
// Self-contained: builds its own .modal-backdrop on document.body; the styling
// comes from app.css (.modal-backdrop / .modal / .btn-close / .edit-form), so
// any page that links app.css can use it without extra markup or CSS.
//
// Consumers (docs/architecture/moderation.md, "Code reuse"):
//   - the admin Files page  → PATCH /api/files/{hash}/metadata (metadata.edit)
//   - the upload page's "My uploads" tab → PATCH /api/my/uploads/{hash}/metadata
//     (owner-scoped; drafts and returned files only)
// The endpoint is caller-supplied, the component owns the form/modal mechanics.

let nextEditorId = 1;

/**
 * createTrackEditor builds the modal (hidden) and returns its controls.
 *
 * @param {Object}   opts
 * @param {Function} opts.patchURL  (file) => string — the PATCH endpoint for this file.
 * @param {string}   [opts.note]    Explanatory text shown under the fields.
 * @param {Function} [opts.checkAuth] (Response) => bool — return true when the
 *                                  response was an auth failure the caller
 *                                  already handled (saving is then abandoned).
 * @param {Function} opts.onSaved   (file, data, ctx) => void — called with the
 *                                  server's authoritative values on success.
 * @param {Function} [opts.onError] (err, file) => void — save failure.
 * @param {Object}   [opts.access]  Optional access section (License + Guest),
 *                                  shown only when supplied (admin scopes):
 *                                  { licenses: string[],
 *                                    save: async (file, {guest, license}) => void }.
 *                                  The tag PATCH runs first; `save` is awaited
 *                                  after it, so access writes hit their own
 *                                  endpoints (POST …/guest, …/license).
 * @returns {{ open(file, ctx?): void, close(): void, destroy(): void }}
 *   open()'s `file` needs { hash, title, artist, album_artist, album }; with
 *   `access` it also reads { guest_playable, license }. `ctx` is passed through
 *   to onSaved untouched (e.g. "which view opened me").
 */
export function createTrackEditor({ patchURL, note = '', checkAuth = null, onSaved, onError, access = null }) {
  const titleId = `trackEditTitle${nextEditorId++}`;

  const backdrop = document.createElement('div');
  backdrop.className = 'modal-backdrop hidden';
  backdrop.setAttribute('role', 'dialog');
  backdrop.setAttribute('aria-modal', 'true');
  backdrop.setAttribute('aria-labelledby', titleId);

  const modal = document.createElement('div');
  modal.className = 'modal';

  const header = document.createElement('div');
  header.className = 'modal-header';
  const heading = document.createElement('h2');
  heading.id = titleId;
  heading.textContent = 'Edit metadata';
  const closeBtn = document.createElement('button');
  closeBtn.type = 'button';
  closeBtn.className = 'btn-close';
  closeBtn.setAttribute('aria-label', 'Close');
  closeBtn.textContent = '×';
  header.append(heading, closeBtn);

  const form = document.createElement('form');
  form.className = 'edit-form';

  const inputs = {};
  for (const [key, label] of [
    ['title', 'Title'],
    ['artist', 'Artist'],
    ['album_artist', 'Album artist'],
    ['album', 'Album'],
  ]) {
    const wrap = document.createElement('label');
    wrap.append(document.createTextNode(label));
    const input = document.createElement('input');
    input.type = 'text';
    input.autocomplete = 'off';
    wrap.appendChild(input);
    inputs[key] = input;
    form.appendChild(wrap);
  }

  // Optional access section (admin scopes only). Saved via access.save() after
  // the tag PATCH, since guest/license have their own endpoints.
  let licenseSel = null, guestCb = null;
  if (access) {
    const split = document.createElement('div');
    split.className = 'field-split';

    const licWrap = document.createElement('label');
    licWrap.append(document.createTextNode('License'));
    licenseSel = document.createElement('select');
    for (const lic of access.licenses || []) {
      const opt = document.createElement('option');
      opt.value = lic;
      opt.textContent = lic || '— none —';
      licenseSel.appendChild(opt);
    }
    licWrap.appendChild(licenseSel);

    const guestWrap = document.createElement('label');
    guestWrap.className = 'field-check';
    guestCb = document.createElement('input');
    guestCb.type = 'checkbox';
    const guestText = document.createElement('span');
    guestText.textContent = 'Guest-playable';
    guestWrap.append(guestCb, guestText);

    split.append(licWrap, guestWrap);
    form.appendChild(split);
  }

  if (note) {
    const p = document.createElement('p');
    p.className = 'modal-body';
    p.textContent = note;
    form.appendChild(p);
  }

  const actions = document.createElement('div');
  actions.className = 'modal-actions';
  const cancelBtn = document.createElement('button');
  cancelBtn.type = 'button';
  cancelBtn.className = 'btn btn-neutral';
  cancelBtn.textContent = 'Cancel';
  const submitBtn = document.createElement('button');
  submitBtn.type = 'submit';
  submitBtn.className = 'btn btn-neutral';
  submitBtn.textContent = 'Save';
  actions.append(cancelBtn, submitBtn);
  form.appendChild(actions);

  modal.append(header, form);
  backdrop.appendChild(modal);
  document.body.appendChild(backdrop);

  let editing = null;   // { file, ctx } while open
  const isOpen = () => !backdrop.classList.contains('hidden');

  function open(file, ctx = null) {
    editing = { file, ctx };
    inputs.title.value        = file.title || '';
    inputs.artist.value       = file.artist || '';
    inputs.album_artist.value = file.album_artist || '';
    inputs.album.value        = file.album || '';
    if (access) {
      licenseSel.value = file.license || '';
      guestCb.checked  = !!file.guest_playable;
    }
    backdrop.classList.remove('hidden');
    inputs.title.focus();
  }

  function close() {
    backdrop.classList.add('hidden');
    editing = null;
  }

  closeBtn.addEventListener('click', close);
  cancelBtn.addEventListener('click', close);
  backdrop.addEventListener('click', e => { if (e.target === backdrop) close(); });
  const onKey = e => { if (e.key === 'Escape' && isOpen()) close(); };
  document.addEventListener('keydown', onKey);

  form.addEventListener('submit', async e => {
    e.preventDefault();
    if (!editing) return;
    const { file, ctx } = editing;
    const body = {
      title:        inputs.title.value,
      artist:       inputs.artist.value,
      album_artist: inputs.album_artist.value,
      album:        inputs.album.value,
    };
    submitBtn.disabled = true;
    try {
      const res = await fetch(patchURL(file), {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (checkAuth && checkAuth(res)) return;
      const data = await res.json().catch(() => ({}));
      if (!res.ok || !data.ok) throw new Error(data.error || `HTTP ${res.status}`);
      // Access writes hit their own endpoints; run them after the tag PATCH.
      if (access) await access.save(file, { guest: guestCb.checked, license: licenseSel.value });
      close();
      onSaved(file, data, ctx);
    } catch (err) {
      if (onError) onError(err, file);
      else console.error('track-edit save failed:', err);
    } finally {
      submitBtn.disabled = false;
    }
  });

  // destroy removes the modal and its document-level listener — required on
  // shell pages, whose modules are torn down when <main> is swapped.
  function destroy() {
    document.removeEventListener('keydown', onKey);
    backdrop.remove();
    editing = null;
  }

  return { open, close, destroy };
}
