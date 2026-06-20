// upload (uploader): POST a fixture audio file. 200 = content already existed
// (dedup), 201 = newly stored. With auth configured the file lands as the
// uploader's draft. No-ops when no audio fixtures are present (read-only runs).
import http from 'k6/http';
import { postFile } from '../lib/http.js';
import { audioFiles, pick } from '../lib/data.js';

export function upload(data) {
  const audio = pick(audioFiles);
  if (!audio) return; // no fixtures — see config/audio-manifest.json + TEST_AUDIO_DIR
  const file = http.file(audio.bin, audio.name, audio.type);
  postFile('/files/upload', data.tokens.uploader, 'upload', file);
}
