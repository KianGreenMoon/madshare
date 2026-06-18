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

// Show an "in progress" badge on the Verify & Prune card when the single prune
// job is running, so the running state is visible without opening the page.
async function fillPruneStatus() {
  try {
    const res = await fetch(`${API}/api/admin/prune/status`);
    if (!res.ok) return; // lacks file.delete — leave the badge hidden
    const snap = await res.json();
    const badge = document.getElementById('pruneRunning');
    if (badge) badge.hidden = snap.state !== 'running';
  } catch { /* network error — leave the badge hidden */ }
}

// Colors for the per-category bar segments + swatches. Known categories get a
// stable color; anything unknown (a future category the server adds before the
// UI knows about it) cycles through the fallbacks. Values are CSS vars so they
// follow the theme (defined in admin-dashboard.css).
const CATEGORY_COLORS = {
  audio: 'var(--accent)',
  images: 'var(--storage-cat-images)',
  video: 'var(--storage-cat-video)',
};
const CATEGORY_FALLBACKS = ['var(--storage-cat-alt1)', 'var(--storage-cat-alt2)', 'var(--storage-cat-alt3)'];

function categoryColor(name, i) {
  return CATEGORY_COLORS[name] || CATEGORY_FALLBACKS[i % CATEGORY_FALLBACKS.length];
}

// Title-case a category name for display ("audio" -> "Audio").
function categoryLabel(name) {
  return name ? name.charAt(0).toUpperCase() + name.slice(1) : '—';
}

// Render the per-category bar segments + detail rows from s.categories. Each
// category becomes a colored bar segment (before the rest-of-disk "other"
// segment) and a swatch+bytes detail row (before the "Madshare total" row).
// Idempotent: prior dynamic nodes are cleared first.
function renderCategories(categories, totalBytes) {
  const bar = document.getElementById('storageBar');
  const other = document.getElementById('storageBarOther');
  const detail = document.getElementById('storageDetail');
  const totalRow = document.getElementById('storageTotalRow');

  bar.querySelectorAll('.storage-bar-cat').forEach((n) => n.remove());
  detail.querySelectorAll('.storage-cat-row').forEach((n) => n.remove());

  categories.forEach((c, i) => {
    const color = categoryColor(c.name, i);

    const seg = document.createElement('div');
    seg.className = 'storage-bar-seg storage-bar-cat';
    seg.style.background = color;
    seg.style.width = (totalBytes ? (c.bytes / totalBytes) * 100 : 0).toFixed(2) + '%';
    bar.insertBefore(seg, other);

    const row = document.createElement('div');
    row.className = 'storage-detail-row storage-cat-row';
    const dt = document.createElement('dt');
    const swatch = document.createElement('span');
    swatch.className = 'storage-swatch';
    swatch.style.background = color;
    swatch.setAttribute('aria-hidden', 'true');
    dt.append(swatch, document.createTextNode(categoryLabel(c.name)));
    const dd = document.createElement('dd');
    dd.textContent = fmtBytes(c.bytes);
    row.append(dt, dd);
    detail.insertBefore(row, totalRow);
  });
}

// Storage panel: free/used disk space for the files volume + Madshare's own
// footprint, broken down by category (audio, images, …). Gated like the other
// admin endpoints; on a 403/error the card stays hidden. An object-store backend
// (future S3) returns volume=null, so we drop the meter and show only the
// per-category breakdown.
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
    const categories = Array.isArray(s.categories) ? s.categories : [];

    if (s.volume) {
      const total = s.volume.total_bytes;
      const used = s.volume.used_bytes;
      const lib = Math.max(0, Math.min(s.library_bytes, used));
      setText('storageFree', fmtBytes(s.volume.free_bytes));
      setText('storageTotal', fmtBytes(total));
      setText('storageUsed', `${fmtBytes(used)} (${Math.round(s.volume.used_percent)}%)`);

      // Bar = [audio][images][…][other disk usage][free]. The category segments
      // sum to the library footprint; "other" is the rest of the disk's used
      // space. Widths are % of total.
      renderCategories(categories, total);
      const otherPct = total ? Math.max(0, (used - lib) / total * 100) : 0;
      document.getElementById('storageBarOther').style.width = otherPct.toFixed(2) + '%';

      meter.hidden = false;
      note.hidden = true;
      usedRow.hidden = false;
    } else {
      // No whole-disk figure for object storage: still show the per-category
      // footprint (the meaningful number), just no meter/disk-used row.
      renderCategories(categories, 0);
      meter.hidden = true;
      note.hidden = false;
      usedRow.hidden = true;
    }

    document.getElementById('storageCard').hidden = false;
  } catch { /* network error — leave the card hidden */ }
}

(async function boot() {
  const identity = await bootAdmin();
  if (!identity) return;
  fillStorage();
  fillPruneStatus();
  fillCount('countFiles', '/api/files');
  fillCount('countModeration', '/api/admin/moderation');
  fillCount('countTrash', '/api/admin/trash');
  fillCount('countDuplicates', '/api/admin/duplicates');
  fillCount('countUsers', '/api/admin/users');
})();
