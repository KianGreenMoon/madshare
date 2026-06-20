// Static test data loaded once at init: the audio manifest and the audio bytes
// it points at. k6 cannot list a directory at runtime, so the files actually
// uploaded are exactly those named in config/audio-manifest.json (relative to
// TEST_AUDIO_DIR). Missing audio is tolerated — the upload case then no-ops, so
// read-only runs work without any fixtures present.

import crypto from 'k6/crypto';
import { TEST_AUDIO_DIR } from '../config/env.js';

const MIME = {
  mp3: 'audio/mpeg',
  flac: 'audio/flac',
  m4a: 'audio/mp4',
  mp4: 'audio/mp4',
  ogg: 'audio/ogg',
  opus: 'audio/ogg',
  aac: 'audio/aac',
  wav: 'audio/wav',
};

function mimeFor(name) {
  const ext = name.split('.').pop().toLowerCase();
  return MIME[ext] || 'application/octet-stream';
}

function baseName(rel) {
  return rel.split('/').pop();
}

function safeOpen(path, mode) {
  try {
    return open(path, mode);
  } catch (e) {
    return null; // fixture absent — upload case will skip
  }
}

// Manifest is committed config, so this open() must succeed.
const manifest = JSON.parse(open('../config/audio-manifest.json'));

// Audio payloads, ready for http.file(). k6 shares the underlying buffers
// across VUs, so loading them once at init is cheap. The server keys blobs by
// sha256 of the bytes, so we compute the same hash here — the delete case then
// targets exactly the suite's own uploads (see audioHashes) with no discovery.
export const audioFiles = manifest
  .map((rel) => {
    const bin = safeOpen(`${TEST_AUDIO_DIR}/${rel}`, 'b');
    return bin ? { name: baseName(rel), bin, type: mimeFor(rel), hash: crypto.sha256(bin, 'hex') } : null;
  })
  .filter(Boolean);

// Content hashes (sha256 hex) of the fixtures — the delete case's targets.
export const audioHashes = audioFiles.map((a) => a.hash);

// pick returns a random element, or null for an empty/absent array.
export function pick(arr) {
  return arr && arr.length ? arr[Math.floor(Math.random() * arr.length)] : null;
}
