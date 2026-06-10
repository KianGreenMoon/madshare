// about-menu.js — the header "About" mini-menu and the About (version) modal.
//
// The markup lives in the shared header partial (partials.html), so this wires
// the same behavior everywhere that header appears: the shell pages (called from
// shell.js) and the admin pages (called from admin/shared.js). initAboutMenu()
// no-ops when the header isn't present, and is meant to run once per page
// context (the shell calls it once at boot; admin pages are full loads).

export function initAboutMenu() {
  const about = document.getElementById('about');
  const btn = document.getElementById('aboutBtn');
  const menu = document.getElementById('aboutMenu');
  if (!about || !btn || !menu) return; // page without the shared header

  const openMenu = () => { menu.hidden = false; btn.setAttribute('aria-expanded', 'true'); };
  const closeMenu = () => { menu.hidden = true; btn.setAttribute('aria-expanded', 'false'); };

  btn.addEventListener('click', e => {
    e.stopPropagation();
    menu.hidden ? openMenu() : closeMenu();
  });

  // A click anywhere outside the menu closes it.
  document.addEventListener('click', e => {
    if (!menu.hidden && !about.contains(e.target)) closeMenu();
  });

  // ── Version entry → About modal ────────────────────────────────────────────
  const modal = document.getElementById('aboutModal');
  const versionItem = document.getElementById('aboutVersion');
  const closeBtn = document.getElementById('aboutClose');

  const openModal = () => modal?.classList.remove('hidden');
  const closeModal = () => modal?.classList.add('hidden');

  versionItem?.addEventListener('click', () => { closeMenu(); openModal(); });
  closeBtn?.addEventListener('click', closeModal);
  modal?.addEventListener('click', e => { if (e.target === modal) closeModal(); }); // backdrop click

  document.addEventListener('keydown', e => {
    if (e.key !== 'Escape') return;
    if (modal && !modal.classList.contains('hidden')) { closeModal(); return; }
    if (!menu.hidden) closeMenu();
  });
}
