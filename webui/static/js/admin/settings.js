// Admin · Settings — placeholder boot (Phase 1 scaffold). Requires user.manage.
// Real auto-publish policy lands in Phase 6.
import { bootAdmin } from './shared.js';

bootAdmin({ require: 'user.manage' });
