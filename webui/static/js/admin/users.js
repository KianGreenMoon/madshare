// Admin · Users — placeholder boot (Phase 1 scaffold). Requires user.manage.
// Real user CRUD lands in Phase 4.
import { bootAdmin } from './shared.js';

bootAdmin({ require: 'user.manage' });
