// toast.js — the one transient pop-up message system, shared by every web-UI
// surface (library/shell, upload, auth, admin).
//
// Two mount points come from the `auth-modals` partial (partials.html):
//   #toastStatus — polite live region, bottom-right above the player bar; holds
//                  status (info) and success toasts; auto-dismisses.
//   #toastAlert  — assertive live region, top-right; holds errors, which PERSIST
//                  until the user dismisses them.
// showToast no-ops if the target stack is absent (a page without the stacks
// simply shows nothing), so callers never have to guard.
//
// Pure DOM, zero imports, and no work at module-eval time — only a function
// definition; the stack is looked up lazily inside showToast. That keeps it safe
// to import from a shell-swapped page module (see the shell's no-DOM-at-eval
// rule).
export function showToast(message, { type = 'status', actionLabel, onAction, timeout = 5000 } = {}) {
  const stack = document.getElementById(type === 'error' ? 'toastAlert' : 'toastStatus');
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

  el.append(icon, msg);

  if (actionLabel && onAction) {
    const action = document.createElement('button');
    action.className = 'toast-action';
    action.textContent = actionLabel;
    action.addEventListener('click', () => { el.remove(); onAction(); });
    el.appendChild(action);
  }

  const close = document.createElement('button');
  close.className = 'toast-close';
  close.setAttribute('aria-label', 'Dismiss');
  close.textContent = '×';
  close.addEventListener('click', () => el.remove());
  el.appendChild(close);

  stack.appendChild(el);
  if (type !== 'error') setTimeout(() => el.remove(), timeout);
}
