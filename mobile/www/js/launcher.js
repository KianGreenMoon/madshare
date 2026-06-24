// launcher.js — the bundled "home" of the Android app (android-app.md §2).
//
// Responsibilities (P1): keep a saved-server list, run the §4 connection-safety
// gate before handing the WebView to a server, best-effort health-probe a server
// so we can show "Madshare vX", and hand off with window.location once the gate
// passes. All connection-safety logic lives in classify.js (pure + tested); this
// file is the DOM/storage/network glue around it.

import { evaluate, normalizeUrl, ACTION, CLASS } from './classify.js';

const STORE_KEY = 'madshare.servers';

// --- tiny DOM helper -------------------------------------------------------
function el(tag, props = {}, ...children) {
  const node = Object.assign(document.createElement(tag), props);
  for (const c of children) node.append(c?.nodeType ? c : document.createTextNode(c ?? ''));
  return node;
}

// --- server store (localStorage) -------------------------------------------
function loadServers() {
  try {
    const v = JSON.parse(localStorage.getItem(STORE_KEY));
    return Array.isArray(v) ? v : [];
  } catch { return []; }
}
function saveServers(list) {
  localStorage.setItem(STORE_KEY, JSON.stringify(list));
}
function upsertServer(entry) {
  const list = loadServers();
  const i = list.findIndex((s) => s.id === entry.id);
  if (i === -1) list.push(entry); else list[i] = entry;
  saveServers(list);
  return list;
}
function removeServer(id) {
  saveServers(loadServers().filter((s) => s.id !== id));
}
function setTrusted(id, trusted) {
  const list = loadServers();
  const s = list.find((x) => x.id === id);
  if (s) { s.trusted = trusted; saveServers(list); }
}

// --- network: a CORS-free GET via the native HTTP layer when available -----
// In the app the launcher origin (https://localhost) is cross-origin to the
// server, and the server's default CORS policy is closed — so a WebView fetch
// can't read the response. CapacitorHttp makes the request natively, bypassing
// CORS. In a plain browser (dev) we fall back to fetch (best-effort).
function nativeHttp() {
  return globalThis.Capacitor?.Plugins?.CapacitorHttp || null;
}
async function httpGetJSON(url, timeoutMs = 4000) {
  const http = nativeHttp();
  if (http) {
    const res = await http.get({ url, connectTimeout: timeoutMs, readTimeout: timeoutMs });
    const data = typeof res.data === 'string' ? safeJSON(res.data) : res.data;
    return { status: res.status, data };
  }
  const ctrl = new AbortController();
  const t = setTimeout(() => ctrl.abort(), timeoutMs);
  try {
    const res = await fetch(url, { signal: ctrl.signal });
    const data = await res.json().catch(() => null);
    return { status: res.status, data };
  } finally { clearTimeout(t); }
}
function safeJSON(s) { try { return JSON.parse(s); } catch { return null; } }

// probe checks a server is reachable and (if CORS/native allows) that it is a
// Madshare instance. Never blocks connecting — it only enriches the row.
async function probe(server) {
  const base = server.url.replace(/\/+$/, '');
  try {
    const health = await httpGetJSON(base + '/healthz');
    if (health.status < 200 || health.status >= 400) return { reachable: false };
    const cfg = await httpGetJSON(base + '/api/ui/config').catch(() => ({ data: null }));
    return { reachable: true, madshare: !!cfg.data, config: cfg.data };
  } catch {
    return { reachable: false };
  }
}

// --- the safety gate -------------------------------------------------------
function classMeta(cls) {
  switch (cls) {
    case CLASS.SAFE_TLS:     return { label: 'TLS', kind: 'safe' };
    case CLASS.SAFE_LOCAL:   return { label: 'local', kind: 'safe' };
    case CLASS.SAFE_OVERLAY: return { label: 'encrypted overlay', kind: 'safe' };
    case CLASS.LOCAL_LAN:    return { label: 'plaintext · your LAN', kind: 'note' };
    case CLASS.UNSAFE_PUBLIC:return { label: 'plaintext · unencrypted', kind: 'warn' };
    default:                 return { label: 'invalid URL', kind: 'bad' };
  }
}

// connect runs the gate for a server and either hands off or warns.
function connect(server) {
  const verdict = evaluate(server.url, { trusted: !!server.trusted });
  switch (verdict.action) {
    case ACTION.CONNECT:
    case ACTION.NOTE:               // LAN plaintext: allowed, no blocking dialog
      handoff(server);
      break;
    case ACTION.WARN:
      openWarnDialog(server, verdict);
      break;
    default:
      flash(`“${server.url}” isn’t a valid http(s) address.`);
  }
}

function handoff(server) {
  server.lastUsed = Date.now();
  upsertServer(server);
  // Hand the WebView to the server, same-origin from here on (android-app.md §1).
  // replace() (not href=) so the launcher is NOT left in the WebView back-stack:
  // the server's library becomes the true root, so the hardware back button can
  // never walk back into the launcher (android-app.md §10 Q1). Returning to the
  // launcher is done explicitly via the native MadshareMedia.openLauncher() bridge,
  // surfaced as the web UI's "Switch server" header item.
  window.location.replace(server.url);
}

// --- rendering -------------------------------------------------------------
const dom = {};

function render() {
  const list = loadServers().sort((a, b) => (b.lastUsed || 0) - (a.lastUsed || 0));
  dom.list.replaceChildren();
  dom.empty.hidden = list.length > 0;

  for (const server of list) {
    const verdict = evaluate(server.url, { trusted: !!server.trusted });
    const meta = classMeta(verdict.cls);

    const badge = el('span', { className: `badge badge-${meta.kind}` }, meta.label);
    if (server.trusted && verdict.overridden) badge.append(el('span', { className: 'badge-override', title: 'You marked this network trusted' }, ' ✓'));

    const status = el('span', { className: 'srv-status', textContent: '' });
    const connectBtn = el('button', { className: 'btn btn-primary', onclick: () => connect(server) }, 'Connect');

    const row = el('div', { className: 'srv' },
      el('div', { className: 'srv-main' },
        el('div', { className: 'srv-name' }, server.name || server.host || server.url),
        el('div', { className: 'srv-url' }, server.url),
        el('div', { className: 'srv-meta' }, badge, status),
      ),
      el('div', { className: 'srv-actions' },
        connectBtn,
        el('button', { className: 'btn btn-ghost', title: 'Forget this server',
          onclick: () => { removeServer(server.id); render(); } }, '✕'),
      ),
    );
    dom.list.append(row);

    // Best-effort probe fills in the status line asynchronously.
    probe(server).then((p) => {
      if (!p.reachable) { status.textContent = 'unreachable'; status.className = 'srv-status is-down'; return; }
      const name = p.config?.server_name;
      status.textContent = p.madshare ? (name ? `${name} · online` : 'Madshare · online') : 'online';
      status.className = 'srv-status is-up';
    });
  }
}

// --- warn dialog (§4.3) ----------------------------------------------------
function openWarnDialog(server, verdict) {
  dom.warnHost.textContent = verdict.host;
  dom.warnRemember.checked = false;
  dom.warn.returnValue = '';
  dom.warn.showModal();

  dom.warn.onclose = () => {
    if (dom.warn.returnValue !== 'continue') return;     // Cancel / dismissed
    if (dom.warnRemember.checked) { setTrusted(server.id, true); server.trusted = true; }
    handoff(server);
  };
}

// --- add-server form -------------------------------------------------------
function onAdd(e) {
  e.preventDefault();
  const raw = dom.urlInput.value;
  const url = normalizeUrl(raw);
  if (!url) { flash('Enter a valid address, e.g. http://192.168.1.10:3000'); return; }

  const u = new URL(url);
  const entry = {
    id: crypto.randomUUID(),
    name: dom.nameInput.value.trim(),
    url,
    host: u.host,
    trusted: dom.trustedInput.checked,
    addedAt: Date.now(),
    lastUsed: 0,
  };
  upsertServer(entry);
  dom.form.reset();
  render();
}

// --- toast-ish inline message ----------------------------------------------
let flashTimer;
function flash(msg) {
  dom.flash.textContent = msg;
  dom.flash.hidden = false;
  clearTimeout(flashTimer);
  flashTimer = setTimeout(() => { dom.flash.hidden = true; }, 4000);
}

// --- boot ------------------------------------------------------------------
function boot() {
  dom.list = document.getElementById('server-list');
  dom.empty = document.getElementById('empty-state');
  dom.form = document.getElementById('add-form');
  dom.urlInput = document.getElementById('url-input');
  dom.nameInput = document.getElementById('name-input');
  dom.trustedInput = document.getElementById('trusted-input');
  dom.flash = document.getElementById('flash');
  dom.warn = document.getElementById('warn-dialog');
  dom.warnHost = document.getElementById('warn-host');
  dom.warnRemember = document.getElementById('warn-remember');

  dom.form.addEventListener('submit', onAdd);
  render();
}

if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
else boot();
