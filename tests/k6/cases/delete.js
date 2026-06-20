// delete (admin): remove one of the suite's own uploaded files. Soft-delete to
// trash, then — when HARD_DELETE (default) — purge it from trash. Targets only
// manifest-matched files (discover.js), never arbitrary library content. 404 is
// an accepted outcome (another VU may have already removed it).
//
// Exported as deleteCase because `delete` is a reserved word; the runner maps
// the 'delete' case name to it.
import { del } from '../lib/http.js';
import { pick } from '../lib/data.js';
import { HARD_DELETE } from '../config/env.js';

export function deleteCase(data) {
  const target = pick(data.deletable);
  if (!target) return; // nothing of ours to reap
  del(`/api/admin/files/${target.hash}`, data.tokens.admin, 'delete');
  if (HARD_DELETE) {
    del(`/api/admin/trash/${target.hash}`, data.tokens.admin, 'delete');
  }
}
