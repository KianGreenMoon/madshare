// settings.js — the user settings page module (docs/ui/user-settings.md).
//
// A shell-native page at /settings, gated only on being signed in (every account
// manages its own settings — no extra permission). Three sections: Account
// (change password), API tokens (list / create with optional expiry / revoke),
// and Appearance (theme). Wired by shell.js via <body data-module>.
import { getIdentity, openLoginModal } from './auth.js';
import { showToast } from './toast.js';
import { applyTheme, currentTheme } from './theme.js';
import { copyText, selectElementText } from './clipboard.js';

const API = document.querySelector('meta[name="api-url"]')?.content || '';

export async function init() {
  // The only gate is "is signed in". Anonymous visitors get a sign-in prompt
  // instead of the controls (the server-side endpoints enforce auth regardless).
  if (!getIdentity()) {
    renderSignInRequired();
    return;
  }
  wireTabs();
  wirePassword();
  wireTokens();
  wireTheme();
  await loadTokens();
}

// ── Subtabs (Account · API tokens · Appearance) ───────────────────────────────
// Shared .subtabs / .subtab component (app.css); shows one panel at a time and
// follows the ARIA tablist keyboard pattern (arrows / Home / End, roving tabindex).

function wireTabs() {
  const tabs = [
    ['tabBtnAccount', 'panelAccount'],
    ['tabBtnTokens', 'panelTokens'],
    ['tabBtnAppearance', 'panelAppearance'],
  ].map(([tabId, panelId]) => ({
    tab: document.getElementById(tabId),
    panel: document.getElementById(panelId),
  })).filter(({ tab, panel }) => tab && panel);

  const select = (idx) => {
    tabs.forEach(({ tab, panel }, i) => {
      const on = i === idx;
      tab.classList.toggle('is-active', on);
      tab.setAttribute('aria-selected', String(on));
      tab.tabIndex = on ? 0 : -1;
      panel.hidden = !on;
    });
  };

  tabs.forEach(({ tab }, i) => {
    tab.addEventListener('click', () => select(i));
    tab.addEventListener('keydown', (e) => {
      const last = tabs.length - 1;
      let next = null;
      if (e.key === 'ArrowRight' || e.key === 'ArrowDown') next = i === last ? 0 : i + 1;
      else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') next = i === 0 ? last : i - 1;
      else if (e.key === 'Home') next = 0;
      else if (e.key === 'End') next = last;
      if (next === null) return;
      e.preventDefault();
      select(next);
      tabs[next].tab.focus();
    });
  });
}

// ── Gate ─────────────────────────────────────────────────────────────────────

function renderSignInRequired() {
  const main = document.getElementById('settingsMain') || document.querySelector('main');
  if (!main) return;
  const panel = document.createElement('div');
  panel.className = 'access-denied';
  const h = document.createElement('h1');
  h.textContent = 'Sign in required';
  const p = document.createElement('p');
  p.textContent = 'You need to sign in to manage your settings.';
  const btn = document.createElement('button');
  btn.className = 'btn btn-neutral';
  btn.textContent = 'Sign in';
  btn.addEventListener('click', openLoginModal);
  panel.append(h, p, btn);
  main.replaceChildren(panel);
}

// ── Account — change password ────────────────────────────────────────────────

function wirePassword() {
  const form = document.getElementById('passSettingsForm');
  const oldPass = document.getElementById('setOldPass');
  const newPass = document.getElementById('setNewPass');
  const confirmPass = document.getElementById('setConfirmPass');
  const err = document.getElementById('setPassError');
  const fail = (msg) => { err.textContent = msg; err.hidden = false; };

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    err.hidden = true;
    if (newPass.value !== confirmPass.value) return fail('New passwords do not match.');
    if (newPass.value.length < 8) return fail('New password must be at least 8 characters.');
    try {
      const res = await fetch(`${API}/api/auth/password`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ old_password: oldPass.value, new_password: newPass.value }),
      });
      if (!res.ok) {
        const msg = (await res.text()).trim();
        return fail(res.status === 401
          ? 'Current password is incorrect.'
          : `Couldn't change password: ${msg || `HTTP ${res.status}`}`);
      }
      form.reset();
      showToast('Password changed.', { type: 'success' });
    } catch (e2) {
      fail(`Couldn't change password: ${e2.message}`);
    }
  });
}

// ── API tokens ───────────────────────────────────────────────────────────────

function wireTokens() {
  const form = document.getElementById('tokenForm');
  const nameInput = document.getElementById('tokenName');
  const expiryInput = document.getElementById('tokenExpiry');
  const err = document.getElementById('tokenError');
  const reveal = document.getElementById('tokenReveal');
  const revealValue = document.getElementById('tokenRevealValue');
  const copyBtn = document.getElementById('tokenCopy');
  const doneBtn = document.getElementById('tokenRevealDone');
  const fail = (msg) => { err.textContent = msg; err.hidden = false; };

  // No point offering a past date; the server also rejects one.
  expiryInput.min = new Date().toISOString().slice(0, 10);

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    err.hidden = true;
    const name = nameInput.value.trim();
    if (!name) return fail('Token name required.');
    const body = { name };
    if (expiryInput.value) {
      const exp = endOfDayUnix(expiryInput.value);
      if (exp <= nowUnix()) return fail('Expiry date must be in the future.');
      body.expires_at = exp;
    }
    try {
      const res = await fetch(`${API}/api/auth/tokens`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      if (!res.ok) {
        const msg = (await res.text()).trim();
        return fail(`Couldn't create token: ${msg || `HTTP ${res.status}`}`);
      }
      const created = await res.json();
      form.reset();
      // Show the raw token exactly once — it is never returned again.
      revealValue.textContent = created.token;
      reveal.hidden = false;
      await loadTokens();
    } catch (e2) {
      fail(`Couldn't create token: ${e2.message}`);
    }
  });

  copyBtn.addEventListener('click', async () => {
    if (await copyText(revealValue.textContent)) {
      showToast('Token copied.', { type: 'success' });
      return;
    }
    // Neither path was allowed — select the text so it is one keystroke away.
    selectElementText(revealValue);
    showToast('Press Ctrl/Cmd+C to copy the selected token.');
  });

  doneBtn.addEventListener('click', () => {
    reveal.hidden = true;
    revealValue.textContent = '';
  });
}

async function loadTokens() {
  const list = document.getElementById('tokenList');
  if (!list) return;
  list.setAttribute('aria-busy', 'true');
  try {
    const res = await fetch(`${API}/api/auth/tokens`);
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    renderTokens(list, await res.json());
  } catch (e) {
    list.replaceChildren(note(`Couldn't load tokens: ${e.message}`));
  }
  list.setAttribute('aria-busy', 'false');
}

function renderTokens(list, tokens) {
  if (!tokens.length) {
    list.replaceChildren(note('No API tokens yet.'));
    return;
  }
  const table = document.createElement('table');
  table.className = 'token-table';
  const thead = document.createElement('thead');
  thead.innerHTML =
    '<tr><th>Name</th><th>Created</th><th>Last used</th><th>Expires</th><th></th></tr>';
  const tbody = document.createElement('tbody');
  for (const t of tokens) {
    const tr = document.createElement('tr');
    if (t.revoked) tr.classList.add('token-row--revoked');
    tr.append(
      cell(t.name),
      cell(fmtDate(t.created_at) || '—'),
      cell(fmtDate(t.last_used) || 'Never'),
      cell(t.revoked ? 'Revoked' : (fmtDate(t.expires_at) || 'Never')),
    );
    const action = document.createElement('td');
    action.className = 'token-table__action';
    if (!t.revoked) {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'btn btn-neutral token-revoke';
      btn.textContent = 'Revoke';
      btn.addEventListener('click', () => revokeToken(t.id, t.name));
      action.append(btn);
    }
    tr.append(action);
    tbody.append(tr);
  }
  table.append(thead, tbody);
  list.replaceChildren(table);
}

async function revokeToken(id, name) {
  if (!confirm(`Revoke token "${name}"? Clients using it will stop working. This cannot be undone.`)) return;
  try {
    const res = await fetch(`${API}/api/auth/tokens/${id}`, { method: 'DELETE' });
    if (!res.ok && res.status !== 404) throw new Error(`HTTP ${res.status}`);
    showToast('Token revoked.', { type: 'success' });
    await loadTokens();
  } catch (e) {
    showToast(`Couldn't revoke token: ${e.message}`, { type: 'error' });
  }
}

// ── Appearance — theme ───────────────────────────────────────────────────────

function wireTheme() {
  const fs = document.getElementById('themeChoices');
  if (!fs) return;
  const cur = currentTheme();
  fs.querySelectorAll('input[name="theme"]').forEach((radio) => {
    radio.checked = radio.value === cur;
    radio.addEventListener('change', () => { if (radio.checked) applyTheme(radio.value); });
  });
}

// ── Helpers ──────────────────────────────────────────────────────────────────

const nowUnix = () => Math.floor(Date.now() / 1000);

// endOfDayUnix turns a "YYYY-MM-DD" picker value into the unix second at the end
// of that day in LOCAL time, so "expires 2026-12-31" is valid through that whole
// day. Built from parts to avoid the date-only string being parsed as UTC.
function endOfDayUnix(yyyyMmDd) {
  const [y, m, d] = yyyyMmDd.split('-').map(Number);
  return Math.floor(new Date(y, m - 1, d, 23, 59, 59).getTime() / 1000);
}

function fmtDate(sec) {
  if (!sec) return null;
  return new Date(sec * 1000).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
}

function cell(text) {
  const td = document.createElement('td');
  td.textContent = text;
  return td;
}

function note(text) {
  const p = document.createElement('p');
  p.className = 'token-empty';
  p.textContent = text;
  return p;
}
