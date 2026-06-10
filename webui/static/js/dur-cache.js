// dur-cache.js — the shared client-side duration cache (url → formatted
// duration, e.g. "3:45"). Durations are often NULL server-side in v0 (ffprobe
// extraction is deferred), so the library fetches them via audio metadata and
// caches here; the queue panel and the playlists page read the same cache so
// known durations show everywhere — not just after a track has played.
const KEY = 'madshare-durations';

export function loadDurCache() {
  try { return JSON.parse(localStorage.getItem(KEY) || '{}'); }
  catch { return {}; }
}

export function saveDurCache(cache) {
  try { localStorage.setItem(KEY, JSON.stringify(cache)); }
  catch { /* quota exceeded — not fatal */ }
}
