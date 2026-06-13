// favorites.js — the shared liked-tracks cache (Phase 5 step 4 of
// docs/api/playlists.md). One module-level Set of liked hashes backs every
// heart in the UI — library rows, search rows, and the player-bar Like button
// (Decision §8) — so they can never disagree. Favorites are just a system
// playlist server-side; this is only the client-side view of its membership.
import { openLoginModal } from './auth.js';

const API = document.querySelector('meta[name="api-url"]')?.content || '';

let liked = null;      // Set<hash> | null (not loaded yet)
let loading = null;    // in-flight ensureLiked promise
const subs = new Set();

// trackHash returns the content hash for a queue track: either the hash the
// page attached when building the queue, or parsed from the /files/<hash>/…
// URL (every locally served track URL has that shape).
export function trackHash(track) {
  if (track.hash) return track.hash;
  const m = new URL(track.url, location.origin).pathname.match(/^\/files\/([^/]+)\//);
  return m ? m[1] : null;
}

// onLikedChange subscribes to any change of the liked set (load or toggle).
export function onLikedChange(cb) {
  subs.add(cb);
  return () => subs.delete(cb);
}
function notify() {
  subs.forEach(cb => { try { cb(); } catch (e) { console.error('liked listener:', e); } });
}

// ensureLiked loads the liked set once (anonymous / unauthorized requests
// resolve to an empty set — no login prompt for merely rendering hearts).
export function ensureLiked() {
  if (liked) return Promise.resolve(liked);
  if (!loading) {
    loading = (async () => {
      try {
        const res = await fetch(`${API}/api/favorites`);
        liked = new Set(res.ok ? (await res.json()).hashes : []);
      } catch {
        liked = new Set();
      }
      notify();
      return liked;
    })();
  }
  return loading;
}

export function isLiked(hash) {
  return !!hash && !!liked?.has(hash);
}

// toggleLike flips membership server-side and mirrors the result locally.
// 401/403 → login modal (an anonymous user clicked a heart). Returns the new
// state, or null when the toggle didn't happen.
export async function toggleLike(hash) {
  if (!hash) return null;
  try {
    const res = await fetch(`${API}/api/favorites/${encodeURIComponent(hash)}`, { method: 'POST' });
    if (res.status === 401 || res.status === 403) { openLoginModal(); return null; }
    if (!res.ok) return null;
    const { liked: on } = await res.json();
    if (!liked) liked = new Set();
    if (on) liked.add(hash); else liked.delete(hash);
    notify();
    return on;
  } catch {
    return null;
  }
}
