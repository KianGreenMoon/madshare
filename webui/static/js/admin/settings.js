// Admin · Settings — license-based auto-publish policy. Requires user.manage.
import { bootAdmin, API, FREE_LICENSES, toast, handleAuthError, el } from './shared.js';

const autoderiveForm     = document.getElementById('autoderiveForm');
const autoderiveEnabled  = document.getElementById('autoderiveEnabled');
const autoderiveLicenses = document.getElementById('autoderiveLicenses');

// Build the allow-list checkboxes once (the <legend> is already in the markup).
FREE_LICENSES.forEach(lic => {
  autoderiveLicenses.appendChild(el('label', { class: 'check-row' }, [
    el('input', { type: 'checkbox', value: lic, 'data-license': lic }),
    el('span', { text: lic }),
  ]));
});

async function loadAutoDerive() {
  try {
    const res = await fetch(`${API}/api/admin/settings/autoderive`);
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const p = await res.json();
    autoderiveEnabled.checked = !!p.enabled;
    const on = new Set(p.licenses || []);
    autoderiveLicenses.querySelectorAll('input[type=checkbox]').forEach(cb => {
      cb.checked = on.has(cb.value);
    });
  } catch (err) {
    console.error('load auto-derive:', err);
    toast(`Couldn't load policy: ${err.message}`, 'error');
  }
}

async function saveAutoDerive() {
  const licenses = Array.from(autoderiveLicenses.querySelectorAll('input:checked')).map(cb => cb.value);
  try {
    const res = await fetch(`${API}/api/admin/settings/autoderive`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ enabled: autoderiveEnabled.checked, licenses }),
    });
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    toast('Auto-publish policy saved.', 'success');
  } catch (err) {
    toast(`Couldn't save policy: ${err.message}`, 'error');
  }
}

autoderiveForm.addEventListener('submit', e => { e.preventDefault(); saveAutoDerive(); });

// ── Trash-restore policy ──────────────────────────────────────────────────────
const trashPolicyForm   = document.getElementById('trashPolicyForm');
const trashPolicySelect = document.getElementById('trashPolicySelect');

async function loadTrashPolicy() {
  try {
    const res = await fetch(`${API}/api/admin/settings/trash-policy`);
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const { policy } = await res.json();
    if (policy) trashPolicySelect.value = policy;
  } catch (err) {
    console.error('load trash policy:', err);
    toast(`Couldn't load trash policy: ${err.message}`, 'error');
  }
}

async function saveTrashPolicy() {
  try {
    const res = await fetch(`${API}/api/admin/settings/trash-policy`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ policy: trashPolicySelect.value }),
    });
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    toast('Trash-restore policy saved.', 'success');
  } catch (err) {
    toast(`Couldn't save trash policy: ${err.message}`, 'error');
  }
}

trashPolicyForm.addEventListener('submit', e => { e.preventDefault(); saveTrashPolicy(); });

// ── Boot ────────────────────────────────────────────────────────────────────
(async function boot() {
  const identity = await bootAdmin({ require: 'user.manage' });
  if (!identity) return;
  loadAutoDerive();
  loadTrashPolicy();
})();
