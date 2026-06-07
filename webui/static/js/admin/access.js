// Admin · Access groups — create groups, manage members and content grants.
// Requires user.manage.
import { bootAdmin, API, toast, handleAuthError, el } from './shared.js';

const groupsList      = document.getElementById('groupsList');
const groupCreateForm = document.getElementById('groupCreateForm');
const groupName       = document.getElementById('groupName');

let allUsers = []; // for the add-member picker

async function loadUsers() {
  try {
    const res = await fetch(`${API}/api/admin/users`);
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    allUsers = await res.json();
  } catch (err) {
    console.error('load users:', err);
    allUsers = [];
  }
}

async function loadGroups() {
  groupsList.replaceChildren(el('p', { class: 'cell-muted', text: 'Loading groups…' }));
  let groups;
  try {
    const res = await fetch(`${API}/api/admin/access/groups`);
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error(`HTTP ${res.status}`);
    groups = await res.json();
  } catch (err) {
    groupsList.replaceChildren(el('p', { class: 'cell-muted', text: `Failed to load groups: ${err.message}` }));
    return;
  }
  renderGroups(groups || []);
}

function renderGroups(groups) {
  if (!groups.length) {
    groupsList.replaceChildren(el('p', { class: 'cell-muted', text: 'No groups yet.' }));
    return;
  }
  groupsList.replaceChildren(...groups.map(buildGroupCard));
}

function buildGroupCard(g) {
  const head = el('div', { class: 'group-head' }, [
    el('h3', { class: 'group-name', text: g.name }),
    el('button', {
      class: 'btn btn-destructive-outline btn-sm', text: 'Delete group',
      onclick: () => deleteGroup(g),
    }),
  ]);

  // Members
  const memberItems = (g.members || []).map(m =>
    el('li', {}, [
      el('span', { text: m.username }),
      el('button', { class: 'btn btn-neutral btn-sm', text: 'Remove', onclick: () => removeMember(g, m.user_id) }),
    ]));
  const memberList = el('ul', { class: 'member-list' },
    memberItems.length ? memberItems : [el('li', { class: 'cell-muted', text: 'No members.' })]);

  // Add-member control: users not already in the group.
  const memberIds = new Set((g.members || []).map(m => m.user_id));
  const candidates = allUsers.filter(u => !memberIds.has(u.id));
  const memberSelect = el('select', { class: 'license-select', 'aria-label': 'Add user' },
    candidates.length
      ? candidates.map(u => el('option', { value: String(u.id), text: u.username }))
      : [el('option', { value: '', text: 'All users are members' })]);
  const addMemberBtn = el('button', {
    class: 'btn btn-neutral btn-sm', text: 'Add member',
    onclick: () => { if (memberSelect.value) addMember(g, Number(memberSelect.value)); },
  });
  if (!candidates.length) addMemberBtn.disabled = true;

  // Grants
  const grantItems = (g.grants || []).map(gr =>
    el('li', {}, [
      el('span', { text: describeGrant(gr) }),
      el('button', { class: 'btn btn-neutral btn-sm', text: 'Remove', onclick: () => deleteGrant(g, gr.id) }),
    ]));
  const grantList = el('ul', { class: 'grant-list' },
    grantItems.length ? grantItems : [el('li', { class: 'cell-muted', text: 'No grants — group can reach nothing.' })]);

  return el('div', { class: 'group-card' }, [
    head,
    el('h4', { class: 'group-sub', text: 'Members' }),
    memberList,
    el('div', { class: 'inline-form' }, [memberSelect, addMemberBtn]),
    el('h4', { class: 'group-sub', text: 'Grants' }),
    grantList,
    buildGrantForm(g),
  ]);
}

function describeGrant(gr) {
  switch (gr.scope_type) {
    case 'all':    return 'Whole library';
    case 'artist': return `Artist: ${gr.artist}`;
    case 'album':  return `Album: ${gr.artist} — ${gr.album}`;
    case 'file':   return `File #${gr.file_id}`;
    default:       return gr.scope_type;
  }
}

function buildGrantForm(g) {
  const scope = el('select', { class: 'license-select', 'aria-label': 'Grant scope' }, [
    el('option', { value: 'all', text: 'Whole library' }),
    el('option', { value: 'artist', text: 'Artist' }),
    el('option', { value: 'album', text: 'Album' }),
    el('option', { value: 'file', text: 'File (hash)' }),
  ]);
  const artist   = el('input', { type: 'text', placeholder: 'Artist', class: 'grant-input' });
  const album    = el('input', { type: 'text', placeholder: 'Album', class: 'grant-input' });
  const fileHash = el('input', { type: 'text', placeholder: 'File hash', class: 'grant-input' });

  function sync() {
    artist.hidden   = !(scope.value === 'artist' || scope.value === 'album');
    album.hidden    = scope.value !== 'album';
    fileHash.hidden = scope.value !== 'file';
  }
  scope.addEventListener('change', sync);
  sync();

  const addBtn = el('button', {
    class: 'btn btn-neutral btn-sm', text: 'Add grant',
    onclick: () => addGrant(g, {
      scope_type: scope.value,
      artist: artist.value.trim(),
      album: album.value.trim(),
      file_hash: fileHash.value.trim(),
    }),
  });

  return el('div', { class: 'inline-form grant-form' }, [scope, artist, album, fileHash, addBtn]);
}

groupCreateForm.addEventListener('submit', async e => {
  e.preventDefault();
  const name = groupName.value.trim();
  if (!name) return;
  try {
    const res = await fetch(`${API}/api/admin/access/groups`, {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ name }),
    });
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    groupName.value = '';
    toast(`Group "${name}" created.`, 'success');
    loadGroups();
  } catch (err) {
    toast(`Couldn't create group: ${err.message}`, 'error');
  }
});

async function deleteGroup(g) {
  if (!confirm(`Delete group "${g.name}"? Its members and grants are removed.`)) return;
  await mutateGroup(`/api/admin/access/groups/${g.id}`, 'DELETE', null, `Group "${g.name}" deleted.`);
}
async function addMember(g, userID) {
  await mutateGroup(`/api/admin/access/groups/${g.id}/members`, 'POST', { user_id: userID }, 'Member added.');
}
async function removeMember(g, userID) {
  await mutateGroup(`/api/admin/access/groups/${g.id}/members/${userID}`, 'DELETE', null, 'Member removed.');
}
async function addGrant(g, body) {
  await mutateGroup(`/api/admin/access/groups/${g.id}/grants`, 'POST', body, 'Grant added.');
}
async function deleteGrant(g, grantID) {
  await mutateGroup(`/api/admin/access/grants/${grantID}`, 'DELETE', null, 'Grant removed.');
}

// mutateGroup performs a group/grant mutation, then refreshes the list.
async function mutateGroup(path, method, body, okMsg) {
  try {
    const opts = { method };
    if (body != null) {
      opts.headers = { 'Content-Type': 'application/json' };
      opts.body = JSON.stringify(body);
    }
    const res = await fetch(`${API}${path}`, opts);
    if (handleAuthError(res)) return;
    if (!res.ok) throw new Error((await res.text()).trim() || `HTTP ${res.status}`);
    toast(okMsg, 'success');
    loadGroups();
  } catch (err) {
    toast(`Action failed: ${err.message}`, 'error');
  }
}

// ── Boot ────────────────────────────────────────────────────────────────────
(async function boot() {
  const identity = await bootAdmin({ require: 'user.manage' });
  if (!identity) return;
  await loadUsers(); // populate the member picker before rendering groups
  loadGroups();
})();
