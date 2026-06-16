// Shared per-track metadata editor — one modal editing a single file's tags.
//
// The regular (compact) modal edits the four base tags (title / artist / album
// artist / album) plus the track number. An "Extended edit" button opens a
// second, wider modal stacked above it that collects the rarely-touched tags
// (year, track total, disc number, genre, composer, comment) in a two-column
// layout. Both modals' Save persist the whole tag set in one PATCH.
//
// Self-contained: builds its own .modal-backdrop elements on document.body; the
// styling comes from app.css (.modal-backdrop / .modal / .modal-wide /
// .btn-close / .edit-form / .edit-grid-2), so any page that links app.css can use
// it without extra markup.
//
// Consumers (docs/architecture/moderation.md, "Code reuse"):
//   - the admin Files page  → PATCH /api/files/{hash}/metadata (metadata.edit)
//   - the upload page's "My uploads" tab → PATCH /api/my/uploads/{hash}/metadata
//     (owner-scoped; drafts and returned files only)
// The endpoints are caller-supplied, the component owns the form/modal mechanics.
//
// The track number + extended fields are shown only when `detailURL` is supplied:
// the modal GETs the file's current full tag set on open so it can edit those
// fields without clobbering the ones the user never sees. Without `detailURL` the
// modal is base-fields-only, exactly as before.

let nextEditorId = 1;

// The extended fields (shown in the wide modal) and their input flavour. A
// 'span' field spans both grid columns. Exported so bulk-edit.js builds the same
// extended set from one source of truth.
export const EXTENDED_FIELDS = [
  ['year', 'Year', 'number'],
  ['track_total', 'Track total', 'number'],
  ['disc_number', 'Disc number', 'number'],
  ['genre', 'Genre', 'text'],
  ['composer', 'Composer', 'text', 'span'],
  ['comment', 'Comment', 'textarea', 'span'],
];

/**
 * createTrackEditor builds the modal(s) (hidden) and returns its controls.
 *
 * @param {Object}   opts
 * @param {Function} opts.patchURL  (file) => string — the PATCH endpoint for this file.
 * @param {Function} [opts.detailURL] (file) => string — GET endpoint returning the
 *                                  full tag set. Enables the track-number field +
 *                                  the "Extended edit" wide modal; omit for
 *                                  base-fields-only editing.
 * @param {string}   [opts.note]    Explanatory text shown under the base fields.
 * @param {Function} [opts.checkAuth] (Response) => bool — return true when the
 *                                  response was an auth failure the caller
 *                                  already handled (the operation is abandoned).
 * @param {Function} opts.onSaved   (file, data, ctx) => void — called with the
 *                                  server's authoritative values on success.
 * @param {Function} [opts.onError] (err, file) => void — load/save failure.
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
export function createTrackEditor({ patchURL, detailURL = null, note = '', checkAuth = null, onSaved, onError, access = null }) {
  const titleId = `trackEditTitle${nextEditorId++}`;

  // labelled builds a <label>text + control</label> and returns the <label>.
  function labelled(parent, text, control, cls = '') {
    const wrap = document.createElement('label');
    if (cls) wrap.className = cls;
    wrap.append(document.createTextNode(text), control);
    parent.appendChild(wrap);
    return wrap;
  }

  function makeBackdrop(extraClass = '') {
    const b = document.createElement('div');
    b.className = `modal-backdrop hidden${extraClass ? ' ' + extraClass : ''}`;
    b.setAttribute('role', 'dialog');
    b.setAttribute('aria-modal', 'true');
    return b;
  }
  function makeHeader(text, onClose) {
    const header = document.createElement('div');
    header.className = 'modal-header';
    const h = document.createElement('h2');
    h.textContent = text;
    const x = document.createElement('button');
    x.type = 'button';
    x.className = 'btn-close';
    x.setAttribute('aria-label', 'Close');
    x.textContent = '×';
    x.addEventListener('click', onClose);
    header.append(h, x);
    return { header, heading: h };
  }
  function makeActions(buttons) {
    const actions = document.createElement('div');
    actions.className = 'modal-actions';
    actions.append(...buttons);
    return actions;
  }

  // ── Regular (compact) modal: base tags + track number + access ──────────────
  const backdrop = makeBackdrop();
  const modal = document.createElement('div');
  modal.className = 'modal';
  const { header: mainHeader, heading } = makeHeader('Edit metadata', () => closeAll());
  heading.id = titleId;
  backdrop.setAttribute('aria-labelledby', titleId);

  const form = document.createElement('form');
  form.className = 'edit-form';

  const inputs = {};
  for (const [key, label] of [
    ['title', 'Title'],
    ['artist', 'Artist'],
    ['album_artist', 'Album artist'],
    ['album', 'Album'],
  ]) {
    const input = document.createElement('input');
    input.type = 'text';
    input.autocomplete = 'off';
    inputs[key] = input;
    labelled(form, label, input);
  }

  // Track number — main form, but only meaningful (and shown) in detail mode.
  const trackNumberInput = document.createElement('input');
  trackNumberInput.type = 'number';
  trackNumberInput.min = '0';
  trackNumberInput.step = '1';
  trackNumberInput.inputMode = 'numeric';
  const trackNumberWrap = labelled(form, 'Track number', trackNumberInput);

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

  // "Extended edit" — opens the wide modal (only in detail mode).
  let extendedBtn = null;
  if (detailURL) {
    extendedBtn = document.createElement('button');
    extendedBtn.type = 'button';
    extendedBtn.className = 'edit-extended-toggle';
    extendedBtn.textContent = '+ Extended edit…';
    extendedBtn.addEventListener('click', () => openExtended());
    form.appendChild(extendedBtn);
  }

  const submitBtn = document.createElement('button');
  submitBtn.type = 'submit';
  submitBtn.className = 'btn btn-neutral';
  submitBtn.textContent = 'Save';
  const cancelBtn = document.createElement('button');
  cancelBtn.type = 'button';
  cancelBtn.className = 'btn btn-neutral';
  cancelBtn.textContent = 'Cancel';
  cancelBtn.addEventListener('click', () => closeAll());
  form.appendChild(makeActions([cancelBtn, submitBtn]));

  modal.append(mainHeader, form);
  backdrop.appendChild(modal);
  document.body.appendChild(backdrop);

  // ── Wide secondary modal: the extended tags (only built in detail mode) ─────
  let extBackdrop = null, extForm = null, extSubmitBtn = null;
  const extInputs = {};
  if (detailURL) {
    extBackdrop = makeBackdrop('is-stacked');
    const extModal = document.createElement('div');
    extModal.className = 'modal modal-wide';
    const { header: extHeader } = makeHeader('Extended tags', () => closeExtended());
    extModal.appendChild(extHeader);

    extForm = document.createElement('form');
    extForm.className = 'edit-form';
    const grid = document.createElement('div');
    grid.className = 'edit-grid-2';
    for (const [key, label, kind, span] of EXTENDED_FIELDS) {
      let input;
      if (kind === 'textarea') {
        input = document.createElement('textarea');
        input.rows = 2;
      } else {
        input = document.createElement('input');
        input.type = kind;
        if (kind === 'number') { input.min = '0'; input.step = '1'; input.inputMode = 'numeric'; }
        else input.autocomplete = 'off';
      }
      extInputs[key] = input;
      labelled(grid, label, input, span === 'span' ? 'span-2' : '');
    }
    extForm.appendChild(grid);

    extSubmitBtn = document.createElement('button');
    extSubmitBtn.type = 'submit';
    extSubmitBtn.className = 'btn btn-neutral';
    extSubmitBtn.textContent = 'Save';
    const extBack = document.createElement('button');
    extBack.type = 'button';
    extBack.className = 'btn btn-neutral';
    extBack.textContent = 'Back';
    extBack.addEventListener('click', () => closeExtended());
    extForm.appendChild(makeActions([extBack, extSubmitBtn]));

    extModal.appendChild(extForm);
    extBackdrop.appendChild(extModal);
    extBackdrop.addEventListener('click', e => { if (e.target === extBackdrop) closeExtended(); });
    document.body.appendChild(extBackdrop);
  }

  let editing = null;       // { file, ctx } while open
  let detailLoaded = false; // true once a GET populated the extended/track fields
  const mainOpen = () => !backdrop.classList.contains('hidden');
  const extOpen = () => extBackdrop && !extBackdrop.classList.contains('hidden');

  const numStr = v => (v === null || v === undefined ? '' : String(v));

  // setExtendedFrom populates the track-number + extended inputs from a server
  // metadata payload (numbers may be null) and reveals the controls.
  function setExtendedFrom(data) {
    trackNumberInput.value = numStr(data.track_number);
    trackNumberWrap.classList.remove('hidden');
    for (const [key, , kind] of EXTENDED_FIELDS) {
      extInputs[key].value = kind === 'number' ? numStr(data[key]) : (data[key] || '');
    }
    if (extendedBtn) extendedBtn.classList.remove('hidden');
    detailLoaded = true;
  }

  // hideExtended keeps the modal base-fields-only (no detailURL, or a failed load).
  function hideExtended() {
    detailLoaded = false;
    trackNumberWrap.classList.add('hidden');
    if (extendedBtn) extendedBtn.classList.add('hidden');
    if (extBackdrop) extBackdrop.classList.add('hidden');
  }

  function openExtended() {
    if (extBackdrop) { extBackdrop.classList.remove('hidden'); extInputs.year.focus(); }
  }
  function closeExtended() {
    if (extBackdrop) extBackdrop.classList.add('hidden');
  }

  async function open(file, ctx = null) {
    editing = { file, ctx };
    inputs.title.value        = file.title || '';
    inputs.artist.value       = file.artist || '';
    inputs.album_artist.value = file.album_artist || '';
    inputs.album.value        = file.album || '';
    if (access) {
      licenseSel.value = file.license || '';
      guestCb.checked  = !!file.guest_playable;
    }
    hideExtended();
    backdrop.classList.remove('hidden');
    inputs.title.focus();

    if (!detailURL) return;
    // Fetch the full tag set so the track-number + extended fields reflect the
    // stored values (and can be saved without clobbering unseen fields).
    try {
      const res = await fetch(detailURL(file));
      if (checkAuth && checkAuth(res)) { closeAll(); return; }
      if (editing && editing.file === file) {
        if (res.ok) {
          const data = await res.json().catch(() => ({}));
          setExtendedFrom(data);
        } else if (onError) {
          onError(new Error(`HTTP ${res.status}`), file);
        }
      }
    } catch (err) {
      if (onError) onError(err, file);
    }
  }

  function closeAll() {
    backdrop.classList.add('hidden');
    if (extBackdrop) extBackdrop.classList.add('hidden');
    editing = null;
  }

  backdrop.addEventListener('click', e => { if (e.target === backdrop) closeAll(); });
  const onKey = e => {
    if (e.key !== 'Escape') return;
    if (extOpen()) closeExtended();
    else if (mainOpen()) closeAll();
  };
  document.addEventListener('keydown', onKey);

  async function submit(e) {
    if (e) e.preventDefault();
    if (!editing) return;
    const { file, ctx } = editing;
    const body = {
      title:        inputs.title.value,
      artist:       inputs.artist.value,
      album_artist: inputs.album_artist.value,
      album:        inputs.album.value,
    };
    // Only send the track-number + extended fields once they were loaded, so a
    // base-only edit (or a failed detail fetch) never blanks them.
    if (detailLoaded) {
      body.track_number = trackNumberInput.value;
      for (const [key] of EXTENDED_FIELDS) body[key] = extInputs[key].value;
    }
    submitBtn.disabled = true;
    if (extSubmitBtn) extSubmitBtn.disabled = true;
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
      closeAll();
      onSaved(file, data, ctx);
    } catch (err) {
      if (onError) onError(err, file);
      else console.error('track-edit save failed:', err);
    } finally {
      submitBtn.disabled = false;
      if (extSubmitBtn) extSubmitBtn.disabled = false;
    }
  }

  form.addEventListener('submit', submit);
  if (extForm) extForm.addEventListener('submit', submit);

  // destroy removes the modals and the document-level listener — required on
  // shell pages, whose modules are torn down when <main> is swapped.
  function destroy() {
    document.removeEventListener('keydown', onKey);
    backdrop.remove();
    if (extBackdrop) extBackdrop.remove();
    editing = null;
  }

  return { open, close: closeAll, destroy };
}
