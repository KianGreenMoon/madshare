// Admin · Access — placeholder boot (Phase 1 scaffold). Requires user.manage.
// Real groups/members/grants land in Phase 4.
import { bootAdmin } from './shared.js';

bootAdmin({ require: 'user.manage' });
