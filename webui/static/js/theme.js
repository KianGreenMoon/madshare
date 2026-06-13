// theme.js — the single source of truth for the color theme.
//
// Theme is a per-device preference held in localStorage ('madshare-theme') and
// applied by setting <html data-theme>, which app.css styles against. The user
// changes it on the Settings page (settings.js); the persistent shell and the
// admin pages only need to apply the saved value on boot. A tiny inline <head>
// guard ({{define "theme-guard"}} in partials.html) applies it before first
// paint to avoid a flash — this module is the deferred-module counterpart that
// reuses the same key, so the two never disagree.

export const VALID_THEMES = ['dark', 'light', 'ocean', 'sunset'];
const KEY = 'madshare-theme';

// currentTheme returns the saved theme, defaulting to 'dark'.
export function currentTheme() {
  try {
    const t = localStorage.getItem(KEY);
    if (t && VALID_THEMES.includes(t)) return t;
  } catch { /* localStorage may be unavailable */ }
  return 'dark';
}

// applyTheme sets the live theme and persists it. Unknown names fall back to
// 'dark'. Returns the applied name.
export function applyTheme(name) {
  if (!VALID_THEMES.includes(name)) name = 'dark';
  document.documentElement.dataset.theme = name;
  try { localStorage.setItem(KEY, name); } catch { /* ignore */ }
  return name;
}
