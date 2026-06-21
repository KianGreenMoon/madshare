// Tiny shared helpers with no fixture or library coupling, so the read cases
// (browse/search/listen) can grab a utility without importing the audio manifest
// in lib/data.js — they pick from content discovered live in setup(), not fixtures.

// pick returns a random element, or null for an empty/absent array.
export function pick(arr) {
  return arr && arr.length ? arr[Math.floor(Math.random() * arr.length)] : null;
}
