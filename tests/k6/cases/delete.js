// delete (admin): remove one of the suite's own uploaded files, addressed by its
// content hash (data.js computes the same sha256 the server uses). Soft-delete to
// trash, then — when HARD_DELETE (default) — purge it from trash. Targets only
// the manifest files, so it never touches the read working set; 404 is accepted
// (already removed by the upload/delete churn or another VU). No-ops when there
// are no fixtures.
//
// Exported as deleteCase because `delete` is a reserved word; the runner maps
// the 'delete' case name to it.
import { del } from '../lib/http.js';
import { pick, audioHashes } from '../lib/data.js';
import { HARD_DELETE } from '../config/env.js';

export function deleteCase(data) {
  const hash = pick(audioHashes);
  if (!hash) return; // no fixtures — nothing of ours to reap
  del(`/api/admin/files/${hash}`, data.tokens.admin, 'delete');
  if (HARD_DELETE) {
    del(`/api/admin/trash/${hash}`, data.tokens.admin, 'delete');
  }
}
