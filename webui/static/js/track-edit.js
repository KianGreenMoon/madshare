// Shared per-track metadata editor — one modal editing a single file's tags.
//
// The main form edits the four base tags (title / artist / album artist / album)
// plus the track number. An "Extended edit" toggle reveals the rarely-touched
// tags (year, track total, disc number, genre, composer, comment).
//
// Self-contained: builds its own .modal-backdrop on document.body; the styling
// comes from app.css (.modal-backdrop / .modal / .btn-close / .edit-form /
// .edit-extended), so any page that links app.css can use it without extra markup.
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

// The extended (collapsed) fields and their input flavour.
const EXTENDED_FIELDS = [
  ['year', 'Year', 'number'],
  ['track_total', 'Track total', 'number'],
  ['disc_number', 'Disc number', 'number'],
  ['genre', 'Genre', 'text'],
  ['composer', 'Composer', 'text'],
  ['comment', 'Comment', 'textarea'],
];

/**
 * createTrackEditor builds the modal (hidden) and returns its controls.
 *
 * @param {Object}   opts
 * @param {Function} opts.patchURL  (file) => string — the PATCH endpoint for this file.
 * @param {Function} [opts.detailURL] (file) => string — GET endpoint returning the
 *                                  full tag set. Enables the track-number + extended
 *                                  fields; omit for base-fields-only editing.
 * @param {string}   [opts.note]    Explanatory text shown under the fields.
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

  // labelled builds a <label>text + control</label> and returns the control.
  function labelled(parent, text, control) {
    const wrap = document.createElement('label');
    wrap.append(document.createTextNode(text), control);
    parent.appendChild(wrap);
    return wrap;
  }

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

  // Extended (collapsed) section — only built/shown in detail mode.
  let extToggle = null, extSection = null;
  const extInputs = {};
  if (detailURL) {
    extToggle = document.createElement('button');
    extToggle.type = 'button';
    extToggle.className = 'edit-extended-toggle';
    extToggle.setAttribute('aria-expanded', 'false');

    extSection = document.createElement('div');
    extSection.className = 'edit-extended hidden';

    for (const [key, label, kind] of EXTENDED_FIELDS) {
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
      labelled(extSection, label, input);
    }
    form.append(extToggle, extSection);

    const setToggleLabel = open => {
      extToggle.textContent = open ? '− Less' : '+ Extended edit';
      extToggle.setAttribute('aria-expanded', open ? 'true' : 'false');
    };
    setToggleLabel(false);
    extToggle.addEventListener('click', () => {
      const open = extSection.classList.toggle('hidden') === false;
      setToggleLabel(open);
    });
    extSection._setToggleLabel = setToggleLabel; // used to reset on open()
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

  let editing = null;     // { file, ctx } while open
  let detailLoaded = false; // true once a GET populated the extended/track fields
  const isOpen = () => !backdrop.classList.contains('hidden');

  const numStr = v => (v === null || v === undefined ? '' : String(v));

  // setExtendedFrom populates the track-number + extended inputs from a server
  // metadata payload (numbers may be null) and reveals the controls.
  function setExtendedFrom(data) {
    trackNumberInput.value = numStr(data.track_number);
    trackNumberWrap.classList.remove('hidden');
    if (extSection) {
      for (const [key, , kind] of EXTENDED_FIELDS) {
        extInputs[key].value = kind === 'number' ? numStr(data[key]) : (data[key] || '');
      }
      extSection.classList.add('hidden');     // start collapsed every open
      extSection._setToggleLabel(false);
      extToggle.classList.remove('hidden');
    }
    detailLoaded = true;
  }

  // hideExtended keeps the modal base-fields-only (no detailURL, or a failed load).
  function hideExtended() {
    detailLoaded = false;
    trackNumberWrap.classList.add('hidden');
    if (extSection) { extSection.classList.add('hidden'); extToggle.classList.add('hidden'); }
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
      if (checkAuth && checkAuth(res)) { close(); return; }
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
    // Only send the track-number + extended fields once they were loaded, so a
    // base-only edit (or a failed detail fetch) never blanks them.
    if (detailLoaded) {
      body.track_number = trackNumberInput.value;
      for (const [key] of EXTENDED_FIELDS) body[key] = extInputs[key].value;
    }
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
