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

// An arrow down to a baseline: save these bytes to the device. Distinct from
// MATERIALIZE_ICON below on purpose — the two verbs are routinely confused, so
// they must not look alike (docs/ui/madnetwork-page.md §Wording).
export const DOWNLOAD_ICON = '<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M5 20h14v-2H5v2zM19 9h-4V3H9v6H5l7 7 7-7z"/></svg>';

// An arrow into a tray: bring these bytes into THIS library, through review.
export const MATERIALIZE_ICON = '<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M19 13h-2v3h-2v-3h-2l3-4 3 4zM5 4h14a2 2 0 012 2v3h-2V6H5v12h14v-3h2v3a2 2 0 01-2 2H5a2 2 0 01-2-2V6a2 2 0 012-2z"/></svg>';

export const INFO_ICON = '<svg width="16" height="16" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M11 17h2v-6h-2v6zm1-15C6.48 2 2 6.48 2 12s4.48 10 10 10 10-4.48 10-10S17.52 2 12 2zm0 18c-4.41 0-8-3.59-8-8s3.59-8 8-8 8 3.59 8 8-3.59 8-8 8zM11 9h2V7h-2v2z"/></svg>';
