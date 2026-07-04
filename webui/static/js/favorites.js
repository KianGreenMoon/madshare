// favorites.js — the shared liked-tracks cache (Phase 5 step 4 of
// docs/api/playlists.md). One module-level Set of liked tagset ids backs every
// heart in the UI — library rows, search rows, and the player-bar Like button
// (Decision §8) — so they can never disagree. A library track is addressed by
// its tagset id (the appearance — recording-tagsets P1); favorites are just a
// system playlist server-side; this is only the client-side view of its
// membership.
import { openLoginModal } from './auth.js';

const API = document.querySelector('meta[name="api-url"]')?.content || '';

let liked = null;      // Set<tagsetId> | null (not loaded yet)
let loading = null;    // in-flight ensureLiked promise
const subs = new Set();

// trackKey returns the listening identity for a queue track: the tagset id the
// page attached when building the queue. Tracks from a stale persisted queue
// (or a local preview blob) have none — their hearts stay inert.
export function trackKey(track) {
  return track && track.tagsetId ? track.tagsetId : null;
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
        liked = new Set(res.ok ? (await res.json()).tagset_ids : []);
      } catch {
        liked = new Set();
      }
      notify();
      return liked;
    })();
  }
  return loading;
}

export function isLiked(tagsetId) {
  return !!tagsetId && !!liked?.has(Number(tagsetId));
}

// toggleLike flips membership server-side and mirrors the result locally.
// 401/403 → login modal (an anonymous user clicked a heart). Returns the new
// state, or null when the toggle didn't happen.
export async function toggleLike(tagsetId) {
  if (!tagsetId) return null;
  try {
    const res = await fetch(`${API}/api/favorites/${encodeURIComponent(tagsetId)}`, { method: 'POST' });
    if (res.status === 401 || res.status === 403) { openLoginModal(); return null; }
    if (!res.ok) return null;
    const { liked: on } = await res.json();
    if (!liked) liked = new Set();
    if (on) liked.add(Number(tagsetId)); else liked.delete(Number(tagsetId));
    notify();
    return on;
  } catch {
    return null;
  }
}
