// Shared helpers for the admin sub-pages. Anything that was duplicated inside the
// old monolithic admin.js (formatting, the DOM builder, toasts, auth-error
// handling, theme wiring) lives here once so each page imports only what it needs.
//
// Path note: this module sits in static/js/admin/, so auth.js is one level up.
import { initAuth, openLoginModal, gatePage, PAGE_PERMS } from '../auth.js';

// API base from <meta name="api-url">. Empty => relative, same-origin URLs.
export const API = document.querySelector('meta[name="api-url"]')?.content || '';

// License vocabulary mirrors api.knownLicenses; "" clears the license.
export const LICENSE_OPTIONS = ['', 'CC0-1.0', 'CC-BY-4.0', 'CC-BY-SA-4.0', 'public-domain', 'all-rights-reserved', 'unknown'];
// Free licenses offered as the auto-publish allow-list.
export const FREE_LICENSES = ['CC0-1.0', 'CC-BY-4.0', 'CC-BY-SA-4.0', 'public-domain'];

// ── Formatting ──────────────────────────────────────────────────────────────

export function fmtBytes(n) {
  if (!Number.isFinite(n) || n < 0) return '—';
  if (n < 1024) return n + ' B';
  const kb = n / 1024;
  if (kb < 1024) return kb.toFixed(kb < 10 ? 1 : 0) + ' KB';
  const mb = kb / 1024;
  if (mb < 1024) return mb.toFixed(mb < 10 ? 1 : 0) + ' MB';
  return (mb / 1024).toFixed(1) + ' GB';
}

export function shortHash(h) {
  if (!h) return '';
  return h.length > 12 ? h.slice(0, 12) + '…' : h;
}

export function fmtDate(unix) {
  if (!unix) return '—';
  return new Date(unix * 1000).toLocaleDateString(undefined, { dateStyle: 'medium' });
}

export function fmtTime(s) {
  if (!isFinite(s) || s < 0) return '0:00';
  const m = Math.floor(s / 60);
  const sec = Math.floor(s % 60);
  return m + ':' + String(sec).padStart(2, '0');
}

const AUDIO_EXT = new Set(['mp3', 'ogg', 'oga', 'flac', 'wav', 'mp4', 'm4a', 'aac', 'opus']);
export function isAudioFile(file) {
  if (file.type && file.type.startsWith('audio/')) return true;
  const ext = file.name.split('.').pop().toLowerCase();
  return AUDIO_EXT.has(ext);
}

// ── DOM builder ──────────────────────────────────────────────────────────────
// el('button', {class:'btn', onclick: fn}, ['Label'])
export function el(tag, props = {}, children = []) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (k === 'class') node.className = v;
    else if (k === 'text') node.textContent = v;
    else if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v);
  }
  (Array.isArray(children) ? children : [children]).forEach(c => {
    if (c != null) node.append(c.nodeType ? c : document.createTextNode(c));
  });
  return node;
}

// ── Toasts ───────────────────────────────────────────────────────────────────
// Success/info → polite status region; errors → assertive alert region.
// The stacks (#toastStatus / #toastAlert) come from the auth-modals partial.
export function toast(message, type = 'info') {
  const stack = document.getElementById(type === 'error' ? 'toastAlert' : 'toastStatus');
  if (!stack) return;

  const node = el('div', { class: 'toast' + (type === 'success' ? ' is-success' : type === 'error' ? ' is-error' : '') }, [
    el('span', { class: 'toast-icon', 'aria-hidden': 'true', text: type === 'success' ? '✓' : type === 'error' ? '✕' : 'ℹ' }),
    el('span', { class: 'toast-msg', text: message }),
  ]);
  const close = el('button', { class: 'toast-close', 'aria-label': 'Dismiss', text: '×', onclick: () => node.remove() });
  node.append(close);
  stack.appendChild(node);

  if (type !== 'error') setTimeout(() => node.remove(), 4000);
}

// handleAuthError turns a 401 into a re-login prompt; returns true if handled.
export function handleAuthError(res) {
  if (res && res.status === 401) {
    toast('Your session expired — please sign in again.', 'error');
    openLoginModal();
    return true;
  }
  return false;
}

// ── Theme ────────────────────────────────────────────────────────────────────
const VALID_THEMES = new Set(['dark', 'light', 'ocean', 'sunset']);

export function initTheme() {
  const htmlEl = document.documentElement;
  const themeDots = document.querySelectorAll('.theme-dot');

  const apply = (name) => {
    if (!VALID_THEMES.has(name)) name = 'dark';
    htmlEl.dataset.theme = name;
    localStorage.setItem('madshare-theme', name);
    themeDots.forEach(d => {
      const on = d.dataset.theme === name;
      d.classList.toggle('active', on);
      d.setAttribute('aria-pressed', String(on));
    });
  };

  apply(localStorage.getItem('madshare-theme') || 'dark');
  themeDots.forEach(dot => dot.addEventListener('click', () => apply(dot.dataset.theme)));
}

// ── Page boot ─────────────────────────────────────────────────────────────────
// Shared boot for every admin sub-page: wire the theme switcher, run auth, and
// gate the page on admin access. `require` is an optional finer permission (e.g.
// 'user.manage') the sub-page additionally needs; when the identity lacks it,
// the page content is replaced with a notice and boot returns null.
//
// Returns the identity on success, or null when the caller should stop (the gate
// already rendered the appropriate notice).
export async function bootAdmin({ require } = {}) {
  initTheme();
  const identity = await initAuth();
  if (!gatePage(PAGE_PERMS.admin)) return null;

  if (require) {
    const perms = (identity && identity.permissions) || [];
    if (!perms.includes(require)) {
      renderInsufficient();
      return null;
    }
  }
  return identity;
}

function renderInsufficient() {
  const main = document.querySelector('main');
  if (!main) return;
  main.replaceChildren(
    el('div', { class: 'admin-placeholder' }, [
      el('h2', { text: 'Not available' }),
      el('p', { text: 'Your account does not have permission to use this section.' }),
    ]),
  );
}
