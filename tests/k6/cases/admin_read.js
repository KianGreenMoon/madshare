// admin_read (admin): the read-only admin dashboards.
import { get } from '../lib/http.js';

const PANELS = ['storage', 'trash', 'moderation', 'duplicates'];

export function adminRead(data) {
  for (const panel of PANELS) {
    get(`/api/admin/${panel}`, data.tokens.admin, 'admin_read');
  }
}
