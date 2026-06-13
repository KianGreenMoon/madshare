// Admin · dashboard — the /admin landing. Shows at-a-glance counts on the
// section cards plus a storage panel. Each endpoint is permission-gated; data
// the current admin can't read is simply left off (no error surfaced).
import { bootAdmin, API, fmtBytes } from './shared.js';

async function fillCount(id, url) {
  try {
    const res = await fetch(`${API}${url}`);
    if (!res.ok) return; // lacking permission, etc. — leave the badge hidden
    const data = await res.json();
    if (!Array.isArray(data)) return;
    const badge = document.getElementById(id);
    if (badge) { badge.textContent = String(data.length); badge.hidden = false; }
  } catch { /* network error — leave the badge hidden */ }
}

function setText(id, text) {
  const node = document.getElementById(id);
  if (node) node.textContent = text;
}

// Storage panel: free/used disk space for the files volume + Madshare's own
// footprint. Gated like the other admin endpoints; on a 403/error the card
// stays hidden. An object-store backend (future S3) returns volume=null, so we
// drop the meter and show only the library figure.
async function fillStorage() {
  try {
    const res = await fetch(`${API}/api/admin/storage`);
    if (!res.ok) return; // lacks file.delete, etc. — leave the card hidden
    const s = await res.json();

    setText('storageBackend', s.backend || '—');
    setText('storageLocation', s.location || '—');
    setText('storageLibrary', fmtBytes(s.library_bytes));

    const meter = document.getElementById('storageMeter');
    const note = document.getElementById('storageNote');
    const usedRow = document.getElementById('storageUsedRow');

    if (s.volume) {
      const total = s.volume.total_bytes;
      const used = s.volume.used_bytes;
      const lib = Math.max(0, Math.min(s.library_bytes, used));
      setText('storageFree', fmtBytes(s.volume.free_bytes));
      setText('storageTotal', fmtBytes(total));
      setText('storageUsed', `${fmtBytes(used)} (${Math.round(s.volume.used_percent)}%)`);

      // Bar = [Madshare library][other disk usage][free]. Widths are % of total.
      const libPct = total ? (lib / total) * 100 : 0;
      const otherPct = total ? Math.max(0, (used - lib) / total * 100) : 0;
      document.getElementById('storageBarLib').style.width = libPct.toFixed(2) + '%';
      document.getElementById('storageBarOther').style.width = otherPct.toFixed(2) + '%';

      meter.hidden = false;
      note.hidden = true;
      usedRow.hidden = false;
    } else {
      meter.hidden = true;
      note.hidden = false;
      usedRow.hidden = true; // no whole-disk figure for object storage
    }

    document.getElementById('storageCard').hidden = false;
  } catch { /* network error — leave the card hidden */ }
}

(async function boot() {
  const identity = await bootAdmin();
  if (!identity) return;
  fillStorage();
  fillCount('countFiles', '/api/files');
  fillCount('countModeration', '/api/admin/moderation');
  fillCount('countTrash', '/api/admin/trash');
  fillCount('countUsers', '/api/admin/users');
})();
