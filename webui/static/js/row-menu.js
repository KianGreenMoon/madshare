// row-menu.js — a small shared popup menu for row-level quick actions (the
// library's "⋯" menus, Phase 5 step 4). One menu exists at a time; it closes
// on outside click, Escape, scroll, or resize. Items are plain actions or an
// inline text input (used by "New playlist…").
//
//   openRowMenu(anchor, [
//     { label: 'Play next', onClick: fn },          // closes after onClick
//     { label: 'Add to…',  onClick: fn, keepOpen: true }, // e.g. opens a submenu
//     { input: 'Playlist name', onSubmit: fn },     // inline input + OK
//   ]);

let menuEl = null;
let cleanup = null;

export function closeRowMenu() {
  menuEl?.remove();
  menuEl = null;
  cleanup?.();
  cleanup = null;
}

export function openRowMenu(anchor, items) {
  closeRowMenu();

  menuEl = document.createElement('div');
  menuEl.className = 'row-menu';
  menuEl.setAttribute('role', 'menu');

  items.forEach(item => {
    if (item.input !== undefined) {
      const form = document.createElement('form');
      form.className = 'row-menu-input';
      const input = document.createElement('input');
      input.type = 'text';
      input.placeholder = item.input;
      input.maxLength = 200;
      input.required = true;
      input.setAttribute('aria-label', item.input);
      const ok = document.createElement('button');
      ok.type = 'submit';
      ok.textContent = 'OK';
      form.append(input, ok);
      form.addEventListener('submit', e => {
        e.preventDefault();
        const v = input.value.trim();
        if (!v) return;
        closeRowMenu();
        item.onSubmit(v);
      });
      menuEl.appendChild(form);
      return;
    }
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.setAttribute('role', 'menuitem');
    btn.textContent = item.label;
    btn.disabled = !!item.disabled;
    btn.addEventListener('click', () => {
      if (!item.keepOpen) closeRowMenu();
      item.onClick?.();
    });
    menuEl.appendChild(btn);
  });

  document.body.appendChild(menuEl);

  // Position below the anchor, clamped to the viewport (flip up when there's
  // more room above).
  const r = anchor.getBoundingClientRect();
  const mw = menuEl.offsetWidth;
  const mh = menuEl.offsetHeight;
  let left = Math.min(r.left, window.innerWidth - mw - 8);
  let top = r.bottom + 4;
  if (top + mh > window.innerHeight - 8 && r.top - mh - 4 > 8) top = r.top - mh - 4;
  menuEl.style.left = `${Math.max(8, left)}px`;
  menuEl.style.top = `${Math.max(8, top)}px`;

  const onDocClick = e => { if (!menuEl?.contains(e.target)) closeRowMenu(); };
  const onKey = e => {
    if (e.key === 'Escape') { closeRowMenu(); anchor.focus(); }
  };
  // Don't close on scroll/resize while focus is inside the menu. On mobile,
  // tapping the inline "New playlist…" input opens the virtual keyboard, which
  // fires a viewport resize (and often a scroll to reveal the field); closing
  // here would destroy the focused input and drop the keyboard (BUG-18). With
  // no focus inside the menu (the desktop case) a deliberate scroll/resize still
  // closes it as before.
  const onScroll = () => {
    if (menuEl?.contains(document.activeElement)) return;
    closeRowMenu();
  };
  // Defer the click listener so the opening click doesn't immediately close it.
  setTimeout(() => document.addEventListener('click', onDocClick), 0);
  document.addEventListener('keydown', onKey);
  window.addEventListener('scroll', onScroll, true);
  window.addEventListener('resize', onScroll);
  cleanup = () => {
    document.removeEventListener('click', onDocClick);
    document.removeEventListener('keydown', onKey);
    window.removeEventListener('scroll', onScroll, true);
    window.removeEventListener('resize', onScroll);
  };

  menuEl.querySelector('button:not(:disabled), input')?.focus();
}
