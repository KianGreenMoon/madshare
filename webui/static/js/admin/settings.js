// Admin · Settings — license-based auto-publish policy. Requires user.manage.
import { bootAdmin, API, FREE_LICENSES, toast, handleAuthError, el } from './shared.js';
import { depthSelectValue, depthFromSelect, DEPTH_UNLIMITED } from '../share-depth.js';

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

// ── Tag services (MusicBrainz via AcoustID) ──────────────────────────────────
// The stored API key is never round-tripped: the field starts blank with a
// placeholder describing the stored state; blank on save = keep, the Clear
// button sends an explicit "".
const tagsourceForm     = document.getElementById('tagsourceForm');
const tagsourceEnabled  = document.getElementById('tagsourceEnabled');
const tagsourceKey      = document.getElementById('tagsourceKey');
const tagsourceClearKey = document.getElementById('tagsourceClearKey');

function showTagsource(p) {
  tagsourceEnabled.checked = !!p.musicbrainz_enabled;
  tagsourceKey.value = '';
  tagsourceKey.placeholder = p.api_key_set
    ? `unchanged (••••${p.api_key_last4})` : 'no key set';
  tagsourceClearKey.disabled = !p.api_key_set;
}

async function loadTagsource() {
  try {
    const res = await fetch(`${API}/api/admin/settings/tagsource`);
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    showTagsource(await res.json());
  } catch (err) {
    console.error('load tag services:', err);
    toast(`Couldn't load tag services: ${err.message}`, 'error');
  }
}

async function saveTagsource(apiKey) {
  const body = { musicbrainz_enabled: tagsourceEnabled.checked };
  if (apiKey !== undefined) body.api_key = apiKey;
  try {
    const res = await fetch(`${API}/api/admin/settings/tagsource`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    showTagsource(await res.json());
    toast('Tag services saved.', 'success');
  } catch (err) {
    toast(`Couldn't save tag services: ${err.message}`, 'error');
  }
}

tagsourceForm.addEventListener('submit', e => {
  e.preventDefault();
  const typed = tagsourceKey.value.trim();
  saveTagsource(typed === '' ? undefined : typed); // blank field = keep stored key
});
tagsourceClearKey.addEventListener('click', () => {
  tagsourceEnabled.checked = false; // a keyless enable is refused server-side
  saveTagsource('');
});

// ── Madnetwork (federation F3 downloads + F4 seeding) ────────────────────────
const madnetworkForm        = document.getElementById('madnetworkForm');
const madnetworkAutoapprove = document.getElementById('madnetworkAutoapprove');
const madnetworkSeedEnabled = document.getElementById('madnetworkSeedEnabled');
const madnetworkSeedCache   = document.getElementById('madnetworkSeedCache');
const madnetworkHideUnavail = document.getElementById('madnetworkHideUnavail');
const madnetworkDepth       = document.getElementById('madnetworkDefaultDepth');
const madnetworkServeGuests = document.getElementById('madnetworkServeGuests');
const madnetworkCacheMax     = document.getElementById('madnetworkCacheMax');
const madnetworkCacheDefault = document.getElementById('madnetworkCacheDefault');
const madnetworkCacheMaxAge  = document.getElementById('madnetworkCacheMaxAge');

// The ceiling is stored in BYTES and typed in MiB. Converting at the edge keeps
// the stored number the one the API and the sweep agree on, rather than a unit
// each surface has to remember.
const MIB = 1024 * 1024;

async function loadMadnetwork() {
  try {
    const res = await fetch(`${API}/api/admin/settings/madnetwork`);
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    const p = await res.json();
    madnetworkAutoapprove.checked = !!p.autoapprove_downloads;
    madnetworkSeedEnabled.checked = p.seed_enabled !== false;
    madnetworkSeedCache.checked   = p.seed_cache !== false;
    madnetworkHideUnavail.checked = p.hide_unavailable !== false;
    // The one madnetwork switch that defaults OFF: serving nodes outside the
    // community is the deliberate exception, so a missing field is not consent.
    if (madnetworkServeGuests) madnetworkServeGuests.checked = p.serve_guests === true;
    // The node default is never "inherit" — there is nothing above it to inherit
    // from — so a missing field falls back to ∞, the documented default.
    if (madnetworkDepth) {
      madnetworkDepth.value = depthSelectValue(
        typeof p.default_share_depth === 'number' ? p.default_share_depth : DEPTH_UNLIMITED);
    }
    if (madnetworkCacheMax) {
      // Empty = no override, so the configured default applies. A stored 0 is a
      // different thing — "no limit", chosen — and must show as 0, not as empty.
      madnetworkCacheMax.value = typeof p.cache_max_bytes === 'number'
        ? String(Math.round(p.cache_max_bytes / MIB))
        : '';
    }
    if (madnetworkCacheMaxAge) {
      // Two-valued, unlike the limit above: there is no configured default under
      // it, so there is no empty state to preserve — 0 is simply "keep
      // everything", and that is what a missing field means too.
      madnetworkCacheMaxAge.value = typeof p.cache_max_age_days === 'number'
        ? String(p.cache_max_age_days)
        : '0';
    }
    if (madnetworkCacheDefault) {
      const def = typeof p.cache_default_bytes === 'number' ? p.cache_default_bytes : 0;
      // Naming what "Default" resolves to is the whole point of showing it: on a
      // server it is usually no limit, and a person should not have to read the
      // config file to find that out.
      madnetworkCacheDefault.textContent =
        def > 0 ? `${Math.round(def / MIB)} MiB` : 'no limit';
    }
  } catch (err) {
    console.error('load madnetwork settings:', err);
    toast(`Couldn't load madnetwork settings: ${err.message}`, 'error');
  }
}

async function saveMadnetwork() {
  // Three-valued on the wire, and the three states are genuinely different: an
  // EMPTY field is an explicit null — "go back to the configured default" — a
  // number pins it, and 0 pins "no limit". Omitting the field entirely means
  // unchanged, which is what a value we cannot parse falls back to rather than
  // guessing.
  let cacheField = {};
  if (madnetworkCacheMax) {
    const raw = madnetworkCacheMax.value.trim();
    if (raw === '') {
      cacheField = { cache_max_bytes: null };
    } else {
      const mib = Number(raw);
      if (!Number.isFinite(mib) || mib < 0) {
        toast('The download cache limit must be a whole number of MiB, 0 for no limit, ' +
              'or empty to use the configured default.', 'error');
        return;
      }
      cacheField = { cache_max_bytes: Math.round(mib) * MIB };
    }
  }
  // Two-valued: a number sets it (0 = off), and an unreadable field is omitted
  // so a typo leaves the policy alone instead of switching it off.
  let ageField = {};
  if (madnetworkCacheMaxAge) {
    const raw = madnetworkCacheMaxAge.value.trim();
    const days = raw === '' ? 0 : Number(raw);
    if (!Number.isFinite(days) || days < 0) {
      toast('The cache age limit must be a whole number of days, 0 to keep everything.', 'error');
      return;
    }
    ageField = { cache_max_age_days: Math.round(days) };
  }
  try {
    const res = await fetch(`${API}/api/admin/settings/madnetwork`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        autoapprove_downloads: madnetworkAutoapprove.checked,
        seed_enabled:          madnetworkSeedEnabled.checked,
        seed_cache:            madnetworkSeedCache.checked,
        hide_unavailable:      madnetworkHideUnavail.checked,
        ...(madnetworkServeGuests ? { serve_guests: madnetworkServeGuests.checked } : {}),
        ...(madnetworkDepth ? { default_share_depth: depthFromSelect(madnetworkDepth.value) } : {}),
        ...cacheField,
        ...ageField,
      }),
    });
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    const saved = await res.json().catch(() => ({}));
    // Say what the save actually did to the disk. A ceiling that quietly deleted
    // forty tracks should not report "saved" and nothing else.
    if (saved.evicted > 0) {
      toast(`Madnetwork settings saved — removed ${saved.evicted} cached ` +
            `${saved.evicted === 1 ? 'track' : 'tracks'} to fit the limit.`, 'success');
    } else {
      toast('Madnetwork settings saved.', 'success');
    }
    // Re-read rather than trusting the form: the field should end up showing
    // what is STORED — blank for "use the default", a number for an override —
    // so a save that was interpreted differently from how it was typed says so
    // instead of leaving the form asserting something else.
    await loadMadnetwork();
  } catch (err) {
    toast(`Couldn't save madnetwork settings: ${err.message}`, 'error');
  }
}

madnetworkForm.addEventListener('submit', e => { e.preventDefault(); saveMadnetwork(); });

// ── Boot ────────────────────────────────────────────────────────────────────
(async function boot() {
  const identity = await bootAdmin({ require: 'user.manage' });
  if (!identity) return;
  loadAutoDerive();
  loadTrashPolicy();
  loadTagsource();
  loadMadnetwork();
})();
