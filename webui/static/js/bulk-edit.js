// Shared bulk metadata editor — one modal that sets tags (and, on admin scopes,
// access) across a SELECTION of files at once. The companion to track-edit.js:
// where that edits one file's every field, this writes the same chosen field(s)
// to many files, leaving the rest untouched.
//
// Semantics (see docs/architecture/file-management-view.md):
//   - A blank text field is LEFT UNCHANGED on each file (no clearing in bulk).
//   - License "— keep —" / Guest "— keep —" leave that file's value alone.
//   - Title is intentionally NOT bulk-editable (it is unique per track).
// Setting the album/artist tag on a selection *reassigns* those files (overlay
// re-tag) — it is not an entity rename.
//
// Self-contained: builds its own .modal-backdrop on document.body, styled by
// app.css (.modal-backdrop/.modal/.edit-form) + file-view.css (.field-split).
// The component owns the form; the caller owns persistence via onApply.

let nextBulkId = 1;

const GUEST_KEEP = '';      // sentinel: leave guest as-is
const GUEST_ON   = 'on';
const GUEST_OFF  = 'off';
const LICENSE_KEEP = '\x00keep'; // sentinel distinct from "" (= clear), unused here

/**
 * createBulkEditor builds the modal (hidden) and returns its controls.
 *
 * @param {Object}   opts
 * @param {Object}   [opts.access]   When supplied, adds License + Guest controls:
 *                                   { licenses: string[] }.
 * @param {Function} opts.onApply    async (hashes, patch) => void. `patch` carries
 *                                   only the fields the user filled:
 *                                   { artist?, album_artist?, album?,
 *                                     license?, guest? (boolean) }.
 * @param {Function} [opts.onError]  (err) => void — apply failure.
 * @returns {{ open(hashes): void, close(): void, destroy(): void }}
 */
export function createBulkEditor({ access = null, onApply, onError }) {
  const titleId = `bulkEditTitle${nextBulkId++}`;

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
  heading.textContent = 'Edit tags for selection';
  const closeBtn = document.createElement('button');
  closeBtn.type = 'button';
  closeBtn.className = 'btn-close';
  closeBtn.setAttribute('aria-label', 'Close');
  closeBtn.textContent = '×';
  header.append(heading, closeBtn);

  const form = document.createElement('form');
  form.className = 'edit-form';

  const countLine = document.createElement('p');
  countLine.className = 'modal-body';
  form.appendChild(countLine);

  const inputs = {};
  for (const [key, label] of [
    ['artist', 'Artist'],
    ['album_artist', 'Album artist'],
    ['album', 'Album'],
  ]) {
    const wrap = document.createElement('label');
    wrap.append(document.createTextNode(label));
    const input = document.createElement('input');
    input.type = 'text';
    input.autocomplete = 'off';
    input.placeholder = 'Leave blank to keep each file’s value';
    wrap.appendChild(input);
    inputs[key] = input;
    form.appendChild(wrap);
  }

  let licenseSel = null, guestSel = null;
  if (access) {
    const split = document.createElement('div');
    split.className = 'field-split';

    const licWrap = document.createElement('label');
    licWrap.append(document.createTextNode('License'));
    licenseSel = document.createElement('select');
    const keepOpt = document.createElement('option');
    keepOpt.value = LICENSE_KEEP;
    keepOpt.textContent = '— keep —';
    licenseSel.appendChild(keepOpt);
    for (const lic of access.licenses || []) {
      const opt = document.createElement('option');
      opt.value = lic;
      opt.textContent = lic || '— none —';
      licenseSel.appendChild(opt);
    }
    licWrap.appendChild(licenseSel);

    const guestWrap = document.createElement('label');
    guestWrap.append(document.createTextNode('Guest access'));
    guestSel = document.createElement('select');
    for (const [val, text] of [[GUEST_KEEP, '— keep —'], [GUEST_ON, 'Guest-playable'], [GUEST_OFF, 'Private']]) {
      const opt = document.createElement('option');
      opt.value = val;
      opt.textContent = text;
      guestSel.appendChild(opt);
    }
    guestWrap.appendChild(guestSel);

    split.append(licWrap, guestWrap);
    form.appendChild(split);
  }

  const hint = document.createElement('p');
  hint.className = 'modal-body modal-hint';
  hint.textContent = 'Only the fields you fill are written; the rest stay as they are on each file. '
    + 'Title isn’t bulk-edited (it is unique per track).';
  form.appendChild(hint);

  const errLine = document.createElement('p');
  errLine.className = 'login-error';
  errLine.setAttribute('role', 'alert');
  errLine.hidden = true;
  form.appendChild(errLine);

  const actions = document.createElement('div');
  actions.className = 'modal-actions';
  const cancelBtn = document.createElement('button');
  cancelBtn.type = 'button';
  cancelBtn.className = 'btn btn-neutral';
  cancelBtn.textContent = 'Cancel';
  const submitBtn = document.createElement('button');
  submitBtn.type = 'submit';
  submitBtn.className = 'btn btn-neutral';
  submitBtn.textContent = 'Apply to selection';
  actions.append(cancelBtn, submitBtn);
  form.appendChild(actions);

  modal.append(header, form);
  backdrop.appendChild(modal);
  document.body.appendChild(backdrop);

  let hashes = [];
  const isOpen = () => !backdrop.classList.contains('hidden');

  function open(selectedHashes) {
    hashes = Array.isArray(selectedHashes) ? selectedHashes : [];
    if (!hashes.length) return;
    const n = hashes.length;
    countLine.textContent = `Applies to ${n} selected file${n === 1 ? '' : 's'}. `
      + 'Leave a field blank to keep each file’s current value.';
    inputs.artist.value = '';
    inputs.album_artist.value = '';
    inputs.album.value = '';
    if (access) { licenseSel.value = LICENSE_KEEP; guestSel.value = GUEST_KEEP; }
    errLine.hidden = true;
    errLine.textContent = '';
    backdrop.classList.remove('hidden');
    inputs.artist.focus();
  }

  function close() {
    backdrop.classList.add('hidden');
    hashes = [];
  }

  closeBtn.addEventListener('click', close);
  cancelBtn.addEventListener('click', close);
  backdrop.addEventListener('click', e => { if (e.target === backdrop) close(); });
  const onKey = e => { if (e.key === 'Escape' && isOpen()) close(); };
  document.addEventListener('keydown', onKey);

  // buildPatch collects only the fields the user actually set.
  function buildPatch() {
    const patch = {};
    for (const key of ['artist', 'album_artist', 'album']) {
      const v = inputs[key].value.trim();
      if (v) patch[key] = v;
    }
    if (access) {
      if (licenseSel.value !== LICENSE_KEEP) patch.license = licenseSel.value;
      if (guestSel.value !== GUEST_KEEP) patch.guest = guestSel.value === GUEST_ON;
    }
    return patch;
  }

  form.addEventListener('submit', async e => {
    e.preventDefault();
    if (!hashes.length) return;
    const patch = buildPatch();
    if (!Object.keys(patch).length) {
      errLine.textContent = 'Fill at least one field to apply.';
      errLine.hidden = false;
      return;
    }
    const targets = hashes.slice();
    submitBtn.disabled = true;
    errLine.hidden = true;
    try {
      await onApply(targets, patch);
      close();
    } catch (err) {
      if (onError) onError(err);
      errLine.textContent = `Couldn’t apply: ${err.message}`;
      errLine.hidden = false;
    } finally {
      submitBtn.disabled = false;
    }
  });

  function destroy() {
    document.removeEventListener('keydown', onKey);
    backdrop.remove();
    hashes = [];
  }

  return { open, close, destroy };
}
