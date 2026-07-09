// icons.js — the row-action glyph set, shared by the file-management component
// (file-list.js) and the bespoke admin lists, so one action always wears one
// icon wherever it appears.
//
// Material-style 24×24 paths drawn at 16px in `currentColor`, so the button
// class (.icon-btn, .icon-btn--danger, .btn-edit) decides the colour. They are
// injected as trusted markup — keep them literal, never interpolate into them.
// The button itself carries the label as `title` + `aria-label`; the SVG is
// aria-hidden so a screen reader reads the label once.

export const PLAY_ICON = '<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M8 5v14l11-7z"/></svg>';

export const EDIT_ICON = '<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M3 17.25V21h3.75L17.81 9.94l-3.75-3.75L3 17.25zm17.71-9.96a1 1 0 000-1.41l-2.34-2.34a1 1 0 00-1.41 0l-1.83 1.83 3.75 3.75 1.83-1.83z"/></svg>';

// A counter-clockwise arrow: "put this back where it came from".
export const RESTORE_ICON = '<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M12 5V1L7 6l5 5V7c3.31 0 6 2.69 6 6s-2.69 6-6 6-6-2.69-6-6H4c0 4.42 3.58 8 8 8s8-3.58 8-8-3.58-8-8-8z"/></svg>';

export const TRASH_ICON = '<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M6 19c0 1.1.9 2 2 2h8c1.1 0 2-.9 2-2V7H6v12zM19 4h-3.5l-1-1h-5l-1 1H5v2h14V4z"/></svg>';
