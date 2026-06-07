// Admin · Users — create / edit roles / reset password / enable-disable /
// delete accounts. Requires user.manage.
import { bootAdmin, API, toast, handleAuthError, el } from './shared.js';

const ROLE_LABELS = { admin: 'Admin', moderator: 'Moderator', uploader: 'Uploader', listener: 'Listener' };
const roleLabel = name => ROLE_LABELS[name] || name;

const usersList      = document.getElementById('usersList');
const userCreateForm = document.getElementById('userCreateForm');
const newUserName    = document.getElementById('newUserName');
const newUserPass    = document.getElementById('newUserPass');
const newUserRole    = document.getElementById('newUserRole');
const newUserForceChange = document.getElementById('newUserForceChange');

let availableRoles = [];  // [{name, built_in}]
let currentUsername = ''; // to suppress self-disable / self-delete

async function loadRoles() {
  try {
    const res = await fetch(`${API}/api/admin/roles`);
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    availableRoles = await res.json();
  } catch (err) {
    console.error('load roles:', err);
    availableRoles = [];
  }
  newUserRole.replaceChildren(...availableRoles.map(r =>
    el('option', { value: r.name, ...(r.name === 'listener' ? { selected: 'selected' } : {}) }, [roleLabel(r.name)])));
}

async function loadUsers() {
  usersList.replaceChildren(el('p', { class: 'cell-muted', text: 'Loading users…' }));
  let users;
  try {
    const res = await fetch(`${API}/api/admin/users`);
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    users = await res.json();
  } catch (err) {
    usersList.replaceChildren(el('p', { class: 'cell-muted', text: `Failed to load users: ${err.message}` }));
    return;
  }
  renderUsers(users || []);
}

function renderUsers(users) {
  if (!users.length) {
    usersList.replaceChildren(el('p', { class: 'cell-muted', text: 'No users yet.' }));
    return;
  }
  usersList.replaceChildren(...users.map(buildUserCard));
}

function buildUserCard(u) {
  const isSelf = u.username === currentUsername;

  const badges = [];
  (u.roles || []).forEach(r => badges.push(el('span', { class: 'role-badge', text: roleLabel(r) })));
  if (!u.roles || !u.roles.length) badges.push(el('span', { class: 'cell-muted', text: 'no roles' }));
  if (u.disabled) badges.push(el('span', { class: 'role-badge is-disabled', text: 'Disabled' }));
  if (isSelf) badges.push(el('span', { class: 'role-badge is-you', text: 'you' }));

  const delBtn = el('button', {
    class: 'btn btn-destructive-outline btn-sm', text: 'Delete',
    onclick: () => deleteUserAccount(u),
  });
  if (isSelf) delBtn.disabled = true;

  const head = el('div', { class: 'group-head' }, [
    el('div', { class: 'user-head-info' }, [el('h3', { class: 'group-name', text: u.username }), ...badges]),
    delBtn,
  ]);

  // Role editor: one checkbox per available role + Save.
  const have = new Set(u.roles || []);
  const checks = availableRoles.map(r => {
    const cb = el('input', { type: 'checkbox', value: r.name, ...(have.has(r.name) ? { checked: 'checked' } : {}) });
    return el('label', { class: 'role-check' }, [cb, el('span', { text: roleLabel(r.name) })]);
  });
  const saveRolesBtn = el('button', {
    class: 'btn btn-neutral btn-sm', text: 'Save roles',
    onclick: () => {
      const roles = checks.map(l => l.querySelector('input')).filter(cb => cb.checked).map(cb => cb.value);
      updateUser(u, { roles });
    },
  });

  const disableBtn = el('button', {
    class: 'btn btn-neutral btn-sm', text: u.disabled ? 'Enable' : 'Disable',
    onclick: () => updateUser(u, { disabled: !u.disabled }),
  });
  if (isSelf) disableBtn.disabled = true;

  const resetBtn = el('button', {
    class: 'btn btn-neutral btn-sm', text: 'Reset password',
    onclick: () => resetPassword(u),
  });

  return el('div', { class: 'group-card' }, [
    head,
    el('h4', { class: 'group-sub', text: 'Roles' }),
    el('div', { class: 'role-checks' }, checks),
    el('div', { class: 'inline-form' }, [saveRolesBtn, resetBtn, disableBtn]),
  ]);
}

userCreateForm.addEventListener('submit', async e => {
  e.preventDefault();
  const username = newUserName.value.trim();
  const password = newUserPass.value;
  if (!username || !password) return;
  try {
    const res = await fetch(`${API}/api/admin/users`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        username, password,
        roles: newUserRole.value ? [newUserRole.value] : [],
        require_password_change: newUserForceChange.checked,
      }),
    });
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    newUserName.value = '';
    newUserPass.value = '';
    newUserForceChange.checked = false;
    toast(`User "${username}" created.`, 'success');
    loadUsers();
  } catch (err) {
    toast(`Couldn't create user: ${err.message}`, 'error');
  }
});

async function updateUser(u, body) {
  try {
    const res = await fetch(`${API}/api/admin/users/${u.id}`, {
      method: 'PATCH', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    toast(`User "${u.username}" updated.`, 'success');
    loadUsers();
  } catch (err) {
    toast(`Couldn't update user: ${err.message}`, 'error');
  }
}

async function resetPassword(u) {
  const pw = prompt(`New password for "${u.username}" (min 8 characters):`);
  if (pw == null) return; // cancelled
  if (pw.length < 8) { toast('Password too short (min 8).', 'error'); return; }
  try {
    const res = await fetch(`${API}/api/admin/users/${u.id}/password`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ new_password: pw, require_password_change: false }),
    });
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    toast(`Password reset for "${u.username}". Active sessions were signed out.`, 'success');
  } catch (err) {
    toast(`Couldn't reset password: ${err.message}`, 'error');
  }
}

async function deleteUserAccount(u) {
  if (!confirm(`Delete user "${u.username}"? This removes their account, sessions and tokens. This cannot be undone.`)) return;
  try {
    const res = await fetch(`${API}/api/admin/users/${u.id}`, { method: 'DELETE' });
    if (handleAuthError(res)) return;
    if (!res.ok && res.status !== 204) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    toast(`User "${u.username}" deleted.`, 'success');
    loadUsers();
  } catch (err) {
    toast(`Couldn't delete user: ${err.message}`, 'error');
  }
}

// ── Boot ────────────────────────────────────────────────────────────────────
(async function boot() {
  const identity = await bootAdmin({ require: 'user.manage' });
  if (!identity) return;
  currentUsername = identity.username || '';
  await loadRoles();
  loadUsers();
})();
