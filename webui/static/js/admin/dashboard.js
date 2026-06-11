// Admin · dashboard — the /admin landing. Shows at-a-glance counts on the
// section cards. Each count endpoint is permission-gated; a count the current
// admin can't read is simply left off (no error surfaced).
import { bootAdmin, API } from './shared.js';

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

(async function boot() {
  const identity = await bootAdmin();
  if (!identity) return;
  fillCount('countFiles', '/api/files');
  fillCount('countModeration', '/api/admin/moderation');
  fillCount('countTrash', '/api/admin/trash');
  fillCount('countUsers', '/api/admin/users');
})();
