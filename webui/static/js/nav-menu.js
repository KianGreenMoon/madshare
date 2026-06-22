// nav-menu.js — the responsive header overflow (☰) menu.
//
// On narrow screens the header collapses everything except the logo and the
// pinned Library link into a dropdown panel (.nav-collapse), opened by the ☰
// toggle (#navToggle). The markup lives in the shared header partial
// (partials.html) and the wide↔narrow layout switch is pure CSS (app.css media
// query); this only wires the open/close behavior, mirroring about-menu.js.
//
// Runs once per page context (shell.js wires it at boot — the header persists
// across shell swaps; admin pages wire it on each full load). No-ops when the
// header isn't present.

export function initNavMenu() {
  const toggle = document.getElementById('navToggle');
  const panel = document.getElementById('navCollapse');
  if (!toggle || !panel) return; // page without the shared header

  const isOpen = () => panel.classList.contains('is-open');
  const open = () => { panel.classList.add('is-open'); toggle.setAttribute('aria-expanded', 'true'); };
  const close = () => { panel.classList.remove('is-open'); toggle.setAttribute('aria-expanded', 'false'); };

  toggle.addEventListener('click', e => {
    e.stopPropagation();
    isOpen() ? close() : open();
  });

  // A click outside the panel (and not on the toggle) closes it.
  document.addEventListener('click', e => {
    if (isOpen() && !panel.contains(e.target) && !toggle.contains(e.target)) close();
  });

  // Choosing any link/button inside the menu dismisses it — so navigating in the
  // shell, signing in/out, or opening the About modal all close the dropdown
  // behind them. (The About flyout's own toggle is hidden at this width.)
  panel.addEventListener('click', e => {
    if (e.target.closest('a, button')) close();
  });

  // Escape closes the menu. about-menu.js / auth.js handle Escape for any open
  // modal first; this only acts while the menu itself is open, so they don't fight.
  document.addEventListener('keydown', e => {
    if (e.key === 'Escape' && isOpen()) close();
  });
}
