// Shared auth module — header sign-in/out and change-password.
// Import initAuth() in each page's boot; it wires all header auth elements
// and returns the signed-in identity (or null).  openLoginModal() is exported
// for pages that need to surface it on a 401 response.

const API = document.querySelector('meta[name="api-url"]')?.content || '';

let _identity = null;

export function getIdentity() { return _identity; }

export function openLoginModal() {
  document.getElementById('loginForm').reset();
  document.getElementById('loginError').hidden = true;
  document.getElementById('loginModal').classList.remove('hidden');
  document.getElementById('loginUser').focus();
}

function closeLoginModal() {
  document.getElementById('loginModal').classList.add('hidden');
}

async function fetchIdentity() {
  try {
    const res = await fetch(`${API}/api/auth/me`);
    if (!res.ok) return null;
    return await res.json();
  } catch { return null; }
}

function authToast(message, type) {
  const stackId = type === 'error' ? 'toastAlert' : 'toastStatus';
  const stack = document.getElementById(stackId);
  if (!stack) return;

  const el = document.createElement('div');
  el.className = 'toast' + (type === 'success' ? ' is-success' : type === 'error' ? ' is-error' : '');

  const icon = document.createElement('span');
  icon.className = 'toast-icon';
  icon.setAttribute('aria-hidden', 'true');
  icon.textContent = type === 'success' ? '✓' : type === 'error' ? '✕' : 'ℹ';

  const msg = document.createElement('span');
  msg.className = 'toast-msg';
  msg.textContent = message;

  const close = document.createElement('button');
  close.className = 'toast-close';
  close.setAttribute('aria-label', 'Dismiss');
  close.textContent = '×';
  close.addEventListener('click', () => el.remove());

  el.append(icon, msg, close);
  stack.appendChild(el);
  if (type !== 'error') setTimeout(() => el.remove(), 4000);
}

export async function initAuth() {
  _identity = await fetchIdentity();

  const userArea      = document.getElementById('userArea');
  const userName      = document.getElementById('userName');
  const signInBtn     = document.getElementById('signInBtn');
  const logoutBtn     = document.getElementById('logoutBtn');
  const changePassBtn = document.getElementById('changePassBtn');
  const loginModal    = document.getElementById('loginModal');
  const loginForm     = document.getElementById('loginForm');
  const loginPass     = document.getElementById('loginPass');
  const loginError    = document.getElementById('loginError');
  const loginCancel   = document.getElementById('loginCancel');
  const loginClose    = document.getElementById('loginClose');

  if (_identity) {
    userArea.hidden  = false;
    userName.textContent = _identity.username;
    signInBtn.hidden = true;
  }

  signInBtn.addEventListener('click', openLoginModal);
  loginClose.addEventListener('click', closeLoginModal);
  loginCancel.addEventListener('click', closeLoginModal);
  loginModal.addEventListener('click', e => { if (e.target === loginModal) closeLoginModal(); });
  loginModal.addEventListener('keydown', e => { if (e.key === 'Escape') closeLoginModal(); });

  loginForm.addEventListener('submit', async e => {
    e.preventDefault();
    loginError.hidden = true;
    try {
      const res = await fetch(`${API}/api/auth/login`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          username: document.getElementById('loginUser').value,
          password: loginPass.value,
        }),
      });
      if (!res.ok) {
        loginError.textContent = res.status === 401
          ? 'Invalid username or password.'
          : `Sign-in failed (HTTP ${res.status}).`;
        loginError.hidden = false;
        return;
      }
      loginPass.value = '';
      location.reload();
    } catch (err) {
      loginError.textContent = `Sign-in failed: ${err.message}`;
      loginError.hidden = false;
    }
  });

  logoutBtn.addEventListener('click', async () => {
    try { await fetch(`${API}/api/auth/logout`, { method: 'POST' }); } catch {}
    location.reload();
  });

  // ── Change password ──────────────────────────────────────────────────────

  const passModal   = document.getElementById('passModal');
  const passForm    = document.getElementById('passForm');
  const oldPass     = document.getElementById('oldPass');
  const newPass     = document.getElementById('newPass');
  const confirmPass = document.getElementById('confirmPass');
  const passError   = document.getElementById('passError');
  const passForced  = document.getElementById('passForced');
  const passCancel  = document.getElementById('passCancel');
  const passClose   = document.getElementById('passClose');

  let passIsForced = false;

  function openPassModal(forced) {
    passIsForced = forced;
    passForm.reset();
    passError.hidden = true;
    passForced.hidden = !forced;
    passCancel.hidden = forced;
    passClose.hidden  = forced;
    passModal.classList.remove('hidden');
    oldPass.focus();
  }

  function closePassModal() {
    if (passIsForced) return;
    passModal.classList.add('hidden');
  }

  changePassBtn.addEventListener('click', () => openPassModal(false));
  passCancel.addEventListener('click', closePassModal);
  passClose.addEventListener('click', closePassModal);
  passModal.addEventListener('click', e => { if (e.target === passModal) closePassModal(); });
  passModal.addEventListener('keydown', e => { if (e.key === 'Escape') closePassModal(); });

  passForm.addEventListener('submit', async e => {
    e.preventDefault();
    passError.hidden = true;
    if (newPass.value !== confirmPass.value) {
      passError.textContent = 'New passwords do not match.';
      passError.hidden = false;
      return;
    }
    if (newPass.value.length < 8) {
      passError.textContent = 'New password must be at least 8 characters.';
      passError.hidden = false;
      return;
    }
    try {
      const res = await fetch(`${API}/api/auth/password`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ old_password: oldPass.value, new_password: newPass.value }),
      });
      if (!res.ok) {
        const msg = (await res.text()).trim();
        if (res.status === 401 && /authentication required/i.test(msg)) {
          passIsForced = false;
          passModal.classList.add('hidden');
          location.reload();
          return;
        }
        passError.textContent = res.status === 401
          ? 'Current password is incorrect.'
          : `Couldn't change password: ${msg || `HTTP ${res.status}`}`;
        passError.hidden = false;
        return;
      }
      passIsForced = false;
      passModal.classList.add('hidden');
      authToast('Password changed.', 'success');
      _identity = await fetchIdentity();
    } catch (err) {
      passError.textContent = `Couldn't change password: ${err.message}`;
      passError.hidden = false;
    }
  });

  if (_identity?.password_change_required) openPassModal(true);

  return _identity;
}
