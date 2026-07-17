// Shared bulk charset-fix modal (docs/architecture/tag-suggestions.md, "Bulk
// charset fix") — the album-sized companion to track-edit.js's per-file charset
// override. Where that panel re-reads one file's tag blocks, this modal
// reinterprets the STORED text tags of a whole selection: mojibake from a
// mis-decoded charset round-trips losslessly through Latin-1, so the server can
// re-decode it without touching the files — and values that don't fit Latin-1
// (i.e. already-correct text) are provably left unchanged.
//
// The preview is computed client-side with the same trick (Latin-1 bytes →
// TextDecoder), live per charset pick; the server-side apply is authoritative.
// The component owns the modal; the caller owns persistence via onApply.
//
// Self-contained: builds its own .modal-backdrop on document.body, styled by
// app.css (.modal-backdrop/.modal/.modal-wide/.suggest-charset/.suggest-table).

let nextCharsetId = 1;

// Must stay in sync with media.CharsetNames (the server's ?charset/=charset
// allowlist). Note: WHATWG TextDecoder aliases iso-8859-1 to windows-1252, so
// that one preview can differ from the server in the 0x80–0x9F control range —
// harmless, the apply is server-side.
const CHARSETS = ['utf-8', 'windows-1252', 'iso-8859-1', 'windows-1251', 'koi8-r', 'shift_jis'];

// The row fields shown in the preview table.
const PREVIEW_FIELDS = [
  ['title', 'Title'],
  ['artist', 'Artist'],
  ['album', 'Album'],
];

const PREVIEW_CAP = 25;

// recodePreview reinterprets a Latin-1-decoded string in the given charset —
// the client twin of media.ReencodeLatin1. Returns s unchanged when it doesn't
// fit Latin-1 (never was mojibake) or the decoder is unavailable.
function recodePreview(s, charset) {
  if (!s) return s;
  const bytes = new Uint8Array(s.length);
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c > 0xFF) return s;
    bytes[i] = c;
  }
  try { return new TextDecoder(charset).decode(bytes); } catch { return s; }
}

// detectCharset picks the default dropdown value: the charset whose preview
// changes the most fields without introducing replacement characters. Rows
// whose decode yields U+FFFD count against a candidate. Falls back to the
// first candidate when nothing changes anything (the apply is then a no-op).
function detectCharset(rows) {
  let best = CHARSETS[0], bestScore = 0;
  for (const cs of CHARSETS) {
    let score = 0;
    for (const row of rows) {
      for (const [key] of PREVIEW_FIELDS) {
        const cur = row[key] || '';
        if (!cur) continue;
        const out = recodePreview(cur, cs);
        if (out === cur) continue;
        score += out.includes('�') ? -2 : 1;
      }
    }
    if (score > bestScore) { best = cs; bestScore = score; }
  }
  return best;
}

/**
 * createCharsetEditor builds the modal (hidden) and returns its controls.
 *
 * @param {Object}   opts
 * @param {Function} opts.onApply  async (charset) => void — run the bulk recode
 *                                 (the caller knows its endpoint/selection) and
 *                                 reload. Thrown errors show inline.
 * @returns {{ open(rows, count): void, close(): void, destroy(): void }}
 *   open()'s `rows` are the LOADED selected row objects ({title, artist,
 *   album}) backing the preview; `count` is the full selection size (may exceed
 *   rows.length under "select all N matching").
 */
export function createCharsetEditor({ onApply }) {
  const titleId = `charsetEditTitle${nextCharsetId++}`;

  const backdrop = document.createElement('div');
  backdrop.className = 'modal-backdrop hidden';
  backdrop.setAttribute('role', 'dialog');
  backdrop.setAttribute('aria-modal', 'true');
  backdrop.setAttribute('aria-labelledby', titleId);
  const modal = document.createElement('div');
  modal.className = 'modal modal-wide';

  const header = document.createElement('div');
  header.className = 'modal-header';
  const heading = document.createElement('h2');
  heading.id = titleId;
  heading.textContent = 'Fix charset';
  const closeX = document.createElement('button');
  closeX.type = 'button';
  closeX.className = 'btn-close';
  closeX.setAttribute('aria-label', 'Close');
  closeX.textContent = '×';
  closeX.addEventListener('click', () => close());
  header.append(heading, closeX);

  const note = document.createElement('p');
  note.className = 'modal-body';

  const charsetWrap = document.createElement('label');
  charsetWrap.className = 'suggest-charset';
  charsetWrap.append(document.createTextNode('Charset'));
  const charsetSel = document.createElement('select');
  for (const cs of CHARSETS) {
    const opt = document.createElement('option');
    opt.value = cs;
    opt.textContent = cs;
    charsetSel.appendChild(opt);
  }
  charsetWrap.appendChild(charsetSel);
  charsetSel.addEventListener('change', () => renderPreview());

  const body = document.createElement('div');
  body.className = 'suggest-body';

  const errorEl = document.createElement('p');
  errorEl.className = 'edit-error hidden';
  errorEl.setAttribute('role', 'alert');

  const cancelBtn = document.createElement('button');
  cancelBtn.type = 'button';
  cancelBtn.className = 'btn btn-neutral';
  cancelBtn.textContent = 'Cancel';
  cancelBtn.addEventListener('click', () => close());
  const applyBtn = document.createElement('button');
  applyBtn.type = 'button';
  applyBtn.className = 'btn btn-neutral';
  applyBtn.addEventListener('click', () => apply());
  const actions = document.createElement('div');
  actions.className = 'modal-actions';
  actions.append(cancelBtn, applyBtn);

  modal.append(header, note, charsetWrap, body, errorEl, actions);
  backdrop.appendChild(modal);
  backdrop.addEventListener('click', e => { if (e.target === backdrop) close(); });
  document.body.appendChild(backdrop);

  let previewRows = []; // capped subset backing the table

  const isOpen = () => !backdrop.classList.contains('hidden');
  const onKey = e => { if (e.key === 'Escape' && isOpen()) close(); };
  document.addEventListener('keydown', onKey);

  function renderPreview() {
    const cs = charsetSel.value;
    const table = document.createElement('table');
    table.className = 'suggest-table';
    const thead = table.createTHead().insertRow();
    for (const [, label] of PREVIEW_FIELDS) {
      const th = document.createElement('th');
      th.textContent = label;
      thead.appendChild(th);
    }
    const tbody = table.createTBody();
    let changed = 0;
    for (const row of previewRows) {
      const tr = tbody.insertRow();
      for (const [key] of PREVIEW_FIELDS) {
        const cur = row[key] || '';
        const out = recodePreview(cur, cs);
        const td = tr.insertCell();
        td.textContent = out;
        if (out !== cur) { td.className = 'is-diff'; changed++; }
      }
    }
    body.replaceChildren(table);
    if (!changed) {
      const p = document.createElement('p');
      p.className = 'suggest-empty';
      p.textContent = 'Nothing in the preview changes with this charset.';
      body.appendChild(p);
    }
  }

  function open(rows, count) {
    previewRows = (rows || []).slice(0, PREVIEW_CAP);
    const n = count || previewRows.length;
    note.textContent = `Re-decodes the text tags of ${n} selected file${n === 1 ? '' : 's'} in the chosen charset. `
      + `Values that already read correctly are left unchanged.`
      + (previewRows.length < n ? ` Preview shows the first ${previewRows.length} loaded — highlighted values will change.` : ' Highlighted values will change.');
    applyBtn.textContent = `Apply to ${n}`;
    applyBtn.disabled = false;
    errorEl.classList.add('hidden');
    charsetSel.value = detectCharset(previewRows);
    renderPreview();
    backdrop.classList.remove('hidden');
    charsetSel.focus();
  }

  async function apply() {
    applyBtn.disabled = true;
    errorEl.classList.add('hidden');
    try {
      await onApply(charsetSel.value);
      close();
    } catch (err) {
      errorEl.textContent = `Couldn’t fix the charset: ${err.message}`;
      errorEl.classList.remove('hidden');
      applyBtn.disabled = false;
    }
  }

  function close() { backdrop.classList.add('hidden'); }

  function destroy() {
    document.removeEventListener('keydown', onKey);
    backdrop.remove();
  }

  return { open, close, destroy };
}
