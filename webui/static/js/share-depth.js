// Madnetwork share depth — the shared vocabulary between the surfaces that edit
// it (the Recordings lens's access modal, the All Appearances bulk bar) and the
// chips that display it. Federation F5,
// docs/architecture/federation.md §Sharing scope.
//
// The wire value is an integer: -1 private (nobody), 0 direct friends, n hops,
// DEPTH_UNLIMITED = ∞. `null` means "no override — inherit the node default",
// which the listing reports as node_share_depth so a chip can name what
// "inherit" currently resolves to. Absent (never sent) means "leave unchanged" —
// a distinction a select can't make, so the callers decide when to send at all.

export const DEPTH_UNLIMITED = 1 << 20;

// depthName renders a depth as a short human label. nodeDefault, when given,
// resolves the inherit case to the value it actually takes.
export function depthName(depth, nodeDefault) {
  if (depth === null || depth === undefined) {
    if (nodeDefault === null || nodeDefault === undefined) return 'node default';
    return `node default · ${depthName(nodeDefault)}`;
  }
  if (depth <= -1) return 'private';
  if (depth >= DEPTH_UNLIMITED) return 'madnetwork';
  if (depth === 0) return 'friends';
  return `${depth} hops`;
}

// depthSelectValue maps a recording's stored depth onto the <select> option
// values used by the access modal and the bulk bar.
export function depthSelectValue(depth) {
  if (depth === null || depth === undefined) return 'inherit';
  if (depth >= DEPTH_UNLIMITED) return 'unlimited';
  return String(depth);
}

// depthFromSelect maps a <select> value back onto what the API expects. Returns
// `undefined` for the sentinel '' (leave unchanged — the bulk bar's "don't
// touch" option), `null` for inherit, otherwise a number.
export function depthFromSelect(value) {
  if (value === '' || value === undefined || value === null) return undefined;
  if (value === 'inherit') return null;
  if (value === 'unlimited') return DEPTH_UNLIMITED;
  const n = Number(value);
  return Number.isFinite(n) ? n : undefined;
}

// depthIsPrivate reports whether a resolved depth keeps content off the network
// entirely — what the chip highlights, since it is the one state that changes
// what friends can see.
export function depthIsPrivate(depth, nodeDefault) {
  const effective = (depth === null || depth === undefined) ? nodeDefault : depth;
  return typeof effective === 'number' && effective <= -1;
}
