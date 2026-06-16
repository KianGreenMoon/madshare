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
// Like the per-file editor, an "+ Extended edit…" button opens a stacked wide
// modal carrying the rarely-touched tags (year, track total, disc number, genre,
// composer, comment) — reusing track-edit.js's EXTENDED_FIELDS so the two stay in
// lock-step. Blank-field = keep applies there too; track number stays excluded
// (unique per track, like Title).
//
// Self-contained: builds its own .modal-backdrop on document.body, styled by
// app.css (.modal-backdrop/.modal/.modal-wide/.edit-form/.edit-grid-2/
// .edit-extended-toggle) + file-view.css (.field-split). The component owns the
// form; the caller owns persistence via onApply.

import { EXTENDED_FIELDS } from './track-edit.js';

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

  // "Extended edit" — opens the stacked wide modal with the rarely-touched tags.
  const EXTENDED_LABEL = '+ Extended edit…';
  const extendedBtn = document.createElement('button');
  extendedBtn.type = 'button';
  extendedBtn.className = 'edit-extended-toggle';
  extendedBtn.textContent = EXTENDED_LABEL;
  extendedBtn.addEventListener('click', () => openExtended());
  form.appendChild(extendedBtn);

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

  // ── Wide secondary modal: the extended tags (same set as track-edit.js) ─────
  const extBackdrop = document.createElement('div');
  extBackdrop.className = 'modal-backdrop hidden is-stacked';
  extBackdrop.setAttribute('role', 'dialog');
  extBackdrop.setAttribute('aria-modal', 'true');

  const extModal = document.createElement('div');
  extModal.className = 'modal modal-wide';
  const extHeader = document.createElement('div');
  extHeader.className = 'modal-header';
  const extHeading = document.createElement('h2');
  extHeading.textContent = 'Extended tags for selection';
  const extClose = document.createElement('button');
  extClose.type = 'button';
  extClose.className = 'btn-close';
  extClose.setAttribute('aria-label', 'Close');
  extClose.textContent = '×';
  extClose.addEventListener('click', () => closeExtended());
  extHeader.append(extHeading, extClose);

  const extForm = document.createElement('form');
  extForm.className = 'edit-form';
  const grid = document.createElement('div');
  grid.className = 'edit-grid-2';
  const extInputs = {};
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
    input.placeholder = 'Keep each file’s value';
    extInputs[key] = input;
    const wrap = document.createElement('label');
    if (span === 'span') wrap.className = 'span-2';
    wrap.append(document.createTextNode(label), input);
    grid.appendChild(wrap);
  }
  extForm.appendChild(grid);

  const extHint = document.createElement('p');
  extHint.className = 'modal-body modal-hint';
  extHint.textContent = 'Same rule here: only the fields you fill are written across the selection.';
  extForm.appendChild(extHint);

  const extActions = document.createElement('div');
  extActions.className = 'modal-actions';
  const extBack = document.createElement('button');
  extBack.type = 'button';
  extBack.className = 'btn btn-neutral';
  extBack.textContent = 'Back';
  extBack.addEventListener('click', () => closeExtended());
  const extSubmitBtn = document.createElement('button');
  extSubmitBtn.type = 'submit';
  extSubmitBtn.className = 'btn btn-neutral';
  extSubmitBtn.textContent = 'Apply to selection';
  extActions.append(extBack, extSubmitBtn);
  extForm.appendChild(extActions);

  extModal.append(extHeader, extForm);
  extBackdrop.appendChild(extModal);
  extBackdrop.addEventListener('click', e => { if (e.target === extBackdrop) closeExtended(); });
  document.body.appendChild(extBackdrop);

  let hashes = [];
  const isOpen = () => !backdrop.classList.contains('hidden');
  const extIsOpen = () => !extBackdrop.classList.contains('hidden');

  function openExtended() { extBackdrop.classList.remove('hidden'); extInputs.year.focus(); }
  function closeExtended() { extBackdrop.classList.add('hidden'); syncExtendedBadge(); }

  // syncExtendedBadge annotates the toggle with how many extended fields are
  // filled, so a closed wide modal still shows the user they staged changes there.
  function syncExtendedBadge() {
    const n = EXTENDED_FIELDS.reduce((c, [key]) => c + (extInputs[key].value.trim() ? 1 : 0), 0);
    extendedBtn.textContent = n ? `${EXTENDED_LABEL} (${n})` : EXTENDED_LABEL;
  }
  extForm.addEventListener('input', syncExtendedBadge);

  function open(selectedHashes) {
    hashes = Array.isArray(selectedHashes) ? selectedHashes : [];
    if (!hashes.length) return;
    const n = hashes.length;
    countLine.textContent = `Applies to ${n} selected file${n === 1 ? '' : 's'}. `
      + 'Leave a field blank to keep each file’s current value.';
    inputs.artist.value = '';
    inputs.album_artist.value = '';
    inputs.album.value = '';
    for (const [key] of EXTENDED_FIELDS) extInputs[key].value = '';
    syncExtendedBadge();
    if (access) { licenseSel.value = LICENSE_KEEP; guestSel.value = GUEST_KEEP; }
    errLine.hidden = true;
    errLine.textContent = '';
    backdrop.classList.remove('hidden');
    extBackdrop.classList.add('hidden');
    inputs.artist.focus();
  }

  function close() {
    backdrop.classList.add('hidden');
    extBackdrop.classList.add('hidden');
    hashes = [];
  }

  closeBtn.addEventListener('click', close);
  cancelBtn.addEventListener('click', close);
  backdrop.addEventListener('click', e => { if (e.target === backdrop) close(); });
  const onKey = e => {
    if (e.key !== 'Escape') return;
    if (extIsOpen()) closeExtended();
    else if (isOpen()) close();
  };
  document.addEventListener('keydown', onKey);

  // buildPatch collects only the fields the user actually set. Extended numeric
  // fields ride along as strings (the server parses them); a blank one is omitted
  // so it stays "keep", never "clear".
  function buildPatch() {
    const patch = {};
    for (const key of ['artist', 'album_artist', 'album']) {
      const v = inputs[key].value.trim();
      if (v) patch[key] = v;
    }
    for (const [key] of EXTENDED_FIELDS) {
      const v = extInputs[key].value.trim();
      if (v) patch[key] = v;
    }
    if (access) {
      if (licenseSel.value !== LICENSE_KEEP) patch.license = licenseSel.value;
      if (guestSel.value !== GUEST_KEEP) patch.guest = guestSel.value === GUEST_ON;
    }
    return patch;
  }

  // Submit is shared by both forms (base + extended). Collapse the extended
  // overlay first so the main modal's error line is the visible surface.
  async function submit(e) {
    if (e) e.preventDefault();
    if (!hashes.length) return;
    closeExtended();
    const patch = buildPatch();
    if (!Object.keys(patch).length) {
      errLine.textContent = 'Fill at least one field to apply.';
      errLine.hidden = false;
      return;
    }
    const targets = hashes.slice();
    submitBtn.disabled = true;
    extSubmitBtn.disabled = true;
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
      extSubmitBtn.disabled = false;
    }
  }
  form.addEventListener('submit', submit);
  extForm.addEventListener('submit', submit);

  function destroy() {
    document.removeEventListener('keydown', onKey);
    backdrop.remove();
    extBackdrop.remove();
    hashes = [];
  }

  return { open, close, destroy };
}
