// Admin · dashboard — the /admin landing. Shows at-a-glance counts on the
// section cards plus a storage panel. Each endpoint is permission-gated; data
// the current admin can't read is simply left off (no error surfaced).
import { bootAdmin, API, fmtBytes } from './shared.js';

async function fillCount(id, url) {
  try {
    const res = await fetch(`${API}${url}`);
    if (!res.ok) return; // lacking permission, etc. — leave the badge hidden
    const data = await res.json();
    // Bare-array listings report length; the paginated /api/files envelope
    // reports its total (fetched with limit=0, so no page rows come back).
    let count;
    if (Array.isArray(data)) count = data.length;
    else if (data && typeof data.total === 'number') count = data.total;
    else return;
    const badge = document.getElementById(id);
    if (badge) { badge.textContent = String(count); badge.hidden = false; }
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

// Show the number of madnetwork pairing requests awaiting an accept on the
// Network card (federation F1). Hidden when federation is off or none pend.
async function fillPeerRequests() {
  try {
    const res = await fetch(`${API}/api/admin/federation/peers`);
    if (!res.ok) return; // disabled / lacks federation.manage — leave it hidden
    const data = await res.json();
    const pending = (data.peers || []).filter((p) => p.state === 'pending_incoming').length;
    const badge = document.getElementById('countPeerRequests');
    if (badge && pending > 0) { badge.textContent = String(pending); badge.hidden = false; }
  } catch { /* network error — leave the badge hidden */ }
}

// Show how many contradicted identity claims are waiting for a decision on the
// Network card (federation F6). Evidence, not an alarm: the badge links to the
// page where each finding sits beside the Block action.
async function fillClaimReports() {
  try {
    const res = await fetch(`${API}/api/admin/federation/reports`);
    if (!res.ok) return; // disabled / lacks federation.manage — leave it hidden
    const data = await res.json();
    const open = (data.reports || []).length;
    const badge = document.getElementById('countClaimReports');
    if (badge && open > 0) {
      badge.textContent = `${open} flagged`;
      badge.title = `${open} contradicted identity claim(s) awaiting review`;
      badge.hidden = false;
    }
  } catch { /* network error — leave the badge hidden */ }
}

// Show how many quality upgrades are waiting for a decision (federation F8
// item 3). Same design as the claim-report badge beside it: the count IS the
// notification, and it links to the page where each finding sits next to the
// two things you can do about it.
async function fillUpgrades() {
  try {
    const res = await fetch(`${API}/api/admin/upgrades?disposition=new&limit=1`);
    if (!res.ok) return; // federation off / lacks content.moderate — stay hidden
    const data = await res.json();
    const badge = document.getElementById('countUpgrades');
    if (badge && data.total > 0) {
      badge.textContent = String(data.total);
      badge.title = `${data.total} better rendition(s) available on the madnetwork`;
      badge.hidden = false;
    }
  } catch { /* network error — leave the badge hidden */ }
}

// Show a "scanning" badge on the Data sources card while any symlink source is
// mid-scan (the same shared state the /admin/sources page polls).
async function fillSourcesStatus() {
  try {
    const res = await fetch(`${API}/api/admin/sources`);
    if (!res.ok) return; // lacks content.moderate / no manager — leave it hidden
    const data = await res.json();
    const badge = document.getElementById('sourcesScanning');
    const scanning = Array.isArray(data.sources) && data.sources.some((s) => s.status === 'scanning');
    if (badge) badge.hidden = !scanning;
  } catch { /* network error — leave the badge hidden */ }
}

// Colors for the per-category bar segments + swatches. Known categories get a
// stable color; anything unknown (a future category the server adds before the
// UI knows about it) cycles through the fallbacks. Values are CSS vars so they
// follow the theme (defined in admin-dashboard.css).
const CATEGORY_COLORS = {
  audio: 'var(--accent)',
  images: 'var(--storage-cat-images)',
  review: 'var(--storage-cat-review)',
  trash: 'var(--storage-cat-trash)',
  cache: 'var(--storage-cat-cache)',
  video: 'var(--storage-cat-video)',
};
const CATEGORY_FALLBACKS = ['var(--storage-cat-alt1)', 'var(--storage-cat-alt2)', 'var(--storage-cat-alt3)'];

function categoryColor(name, i) {
  return CATEGORY_COLORS[name] || CATEGORY_FALLBACKS[i % CATEGORY_FALLBACKS.length];
}

// Display labels for categories whose name doesn't title-case nicely. "cache"
// is named in full because it is the one category that is not our own content —
// it is what the swarm fetched from other nodes (docs/architecture/madnetwork-cache.md).
const CATEGORY_LABELS = { review: 'On review', trash: 'In trash', cache: 'Madnetwork cache' };

// Label a category for display: a friendly override, else title-case the name
// ("audio" -> "Audio").
function categoryLabel(name) {
  if (CATEGORY_LABELS[name]) return CATEGORY_LABELS[name];
  return name ? name.charAt(0).toUpperCase() + name.slice(1) : '—';
}

// Render the per-category bar segments + detail rows from s.categories. Each
// category becomes a colored bar segment (before the rest-of-disk "other"
// segment) and a swatch+bytes detail row (before the "Madshare total" row).
// Idempotent: prior dynamic nodes are cleared first.
//
// Segment widths are % of totalBytes (the disk), but collectively capped at
// budgetBytes — the clamped library footprint that the sibling "other" segment
// also respects. They normally match the raw category bytes (budget == footprint),
// but when the logical footprint exceeds the disk's df-style used (logical vs.
// filesystem-allocated bytes — FS compression / sparse / different mount), the
// segments are scaled down proportionally so the bar can't overrun the track.
// The detail rows always show the true (unscaled) byte figures.
function renderCategories(categories, totalBytes, budgetBytes) {
  const bar = document.getElementById('storageBar');
  const other = document.getElementById('storageBarOther');
  const detail = document.getElementById('storageDetail');
  const totalRow = document.getElementById('storageTotalRow');

  bar.querySelectorAll('.storage-bar-cat').forEach((n) => n.remove());
  detail.querySelectorAll('.storage-cat-row').forEach((n) => n.remove());

  const footprint = categories.reduce((sum, c) => sum + (c.bytes || 0), 0);

  categories.forEach((c, i) => {
    const color = categoryColor(c.name, i);

    // Share of the (clamped) library budget, expressed as % of the whole disk.
    const widthPct = (totalBytes > 0 && footprint > 0)
      ? (c.bytes / footprint) * (budgetBytes / totalBytes) * 100
      : 0;
    const seg = document.createElement('div');
    seg.className = 'storage-bar-seg storage-bar-cat';
    seg.style.background = color;
    seg.style.width = widthPct.toFixed(2) + '%';
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

    // External (symlink-imported) bytes live outside data_dir and are not part of
    // the on-disk total, so they get their own row — shown only when non-zero.
    const extRow = document.getElementById('storageExternalRow');
    if (extRow) {
      if (s.external_bytes > 0) {
        setText('storageExternal', fmtBytes(s.external_bytes));
        extRow.hidden = false;
      } else {
        extRow.hidden = true;
      }
    }

    const meter = document.getElementById('storageMeter');
    const note = document.getElementById('storageNote');
    const usedRow = document.getElementById('storageUsedRow');
    const categories = Array.isArray(s.categories) ? s.categories : [];

    // The Cache card's badge is its footprint, taken from the breakdown we just
    // fetched rather than a second request. Hidden at zero: an empty cache is
    // not news, and a badge reading "0 B" is noise on every fresh install.
    const cacheBadge = document.getElementById('countCacheBytes');
    if (cacheBadge) {
      const cache = categories.find(c => c.name === 'cache');
      const bytes = cache ? cache.bytes || 0 : 0;
      cacheBadge.textContent = fmtBytes(bytes);
      cacheBadge.hidden = bytes <= 0;
    }

    if (s.volume) {
      const total = s.volume.total_bytes;
      const used = s.volume.used_bytes;
      const lib = Math.max(0, Math.min(s.library_bytes, used));
      setText('storageFree', fmtBytes(s.volume.free_bytes));
      setText('storageTotal', fmtBytes(total));
      setText('storageUsed', `${fmtBytes(used)} (${Math.round(s.volume.used_percent)}%)`);

      // Bar = [audio][images][…][other disk usage][free]. The category segments
      // collectively fill the (clamped) library footprint `lib`; "other" is the
      // rest of the disk's used space. Passing `lib` as the budget keeps the
      // segments and "other" consistent even when the logical footprint exceeds
      // disk-used (then both shrink to fit). Widths are % of total.
      renderCategories(categories, total, lib);
      const otherPct = total ? Math.max(0, (used - lib) / total * 100) : 0;
      document.getElementById('storageBarOther').style.width = otherPct.toFixed(2) + '%';

      meter.hidden = false;
      note.hidden = true;
      usedRow.hidden = false;
    } else {
      // No whole-disk figure for object storage: still show the per-category
      // footprint (the meaningful number), just no meter/disk-used row.
      renderCategories(categories, 0, 0);
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
  fillSourcesStatus();
  fillPeerRequests();
  fillClaimReports();
  fillUpgrades();
  fillCount('countFiles', '/api/files?limit=0');
  fillCount('countModeration', '/api/admin/moderation?limit=0');
  fillCount('countTrash', '/api/admin/trash?limit=0');
  fillCount('countDuplicates', '/api/admin/duplicates');
  fillCount('countRecordings', '/api/admin/recordings?limit=0');
  fillCount('countUsers', '/api/admin/users');
})();
