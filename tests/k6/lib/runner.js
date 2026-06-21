// The case registry and the single dispatch function used by the arrival-rate
// scenarios. Each generated scenario is named after its case and runs `runCase`,
// which looks up the case by the active scenario's name (k6/execution). This
// keeps one exec function for every case and sidesteps the `delete` reserved
// word. CASES is also imported directly by smoke.js.

import exec from 'k6/execution';

import { browse } from '../cases/browse.js';
import { search } from '../cases/search.js';
import { listen } from '../cases/listen.js';
import { playlists } from '../cases/playlists.js';
import { adminRead } from '../cases/admin_read.js';
import { upload } from '../cases/upload.js';
import { deleteCase } from '../cases/delete.js';

export const CASES = {
  browse,
  search,
  listen,
  playlists,
  admin_read: adminRead,
  upload,
  delete: deleteCase,
};

// Ordered so smoke runs reads first and the destructive delete last.
export const CASE_ORDER = ['browse', 'search', 'listen', 'playlists', 'admin_read', 'upload', 'delete'];

export function runCase(data) {
  const fn = CASES[exec.scenario.name];
  if (fn) fn(data);
}
