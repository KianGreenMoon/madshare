// Madnetwork sharing scope — the shared vocabulary between the surfaces that
// edit it (the Recordings lens's access modal, the All Appearances bulk bar, the
// /admin/settings node default) and the chips that display it. Federation F5,
// collapsed to three values in F7: docs/architecture/federation-access.md §Sharing
// scope.
//
// Three scopes, and no ladder between them:
//
//   -1                local — published to nobody
//    0                direct friends — the nodes this admin hand-picked
//    DEPTH_UNLIMITED  madnetwork — our whole community
//
// `null` means "no override — inherit the node default", which the listing
// reports as node_share_depth so a chip can name what "inherit" currently
// resolves to. Absent (never sent) means "leave unchanged" — a distinction a
// select can't make, so the callers decide when to send at all.
//
// The wire value is still an integer distance, because that is how the server
// compares a scope against a requester's reach. Only the vocabulary changed.

export const DEPTH_UNLIMITED = 1 << 20;

// The three scopes, in the order every control offers them: widest first, since
// the node default is the widest and sharing is the point of the network.
// Migration 035 snapped the old in-between values away, so a stored one can only
// come from an older client; everything here treats it as "direct friends", the
// same way the migration did.
export const SCOPE_OPTIONS = [
  ['unlimited', 'Madnetwork — everyone in our community'],
  ['0', 'Direct friends only'],
  ['-1', 'Local — not shared at all'],
];

// depthName renders a scope as a short human label. nodeDefault, when given,
// resolves the inherit case to the value it actually takes.
//
// "Direct friends" rather than "friends": in this project's vocabulary friends
// means the whole community, and using the short word here would understate what
// the value restricts.
export function depthName(depth, nodeDefault) {
  if (depth === null || depth === undefined) {
    if (nodeDefault === null || nodeDefault === undefined) return 'node default';
    return `node default · ${depthName(nodeDefault)}`;
  }
  if (depth <= -1) return 'local';
  if (depth >= DEPTH_UNLIMITED) return 'madnetwork';
  return 'direct friends';
}

// depthSelectValue maps a recording's stored scope onto the <select> option
// values used by the access modal, the bulk bar and the settings card.
export function depthSelectValue(depth) {
  if (depth === null || depth === undefined) return 'inherit';
  if (depth >= DEPTH_UNLIMITED) return 'unlimited';
  if (depth <= -1) return '-1';
  return '0';
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

// depthIsPrivate reports whether a resolved scope keeps content off the network
// entirely — what the chip highlights, since it is the one state that changes
// what the community can see.
export function depthIsPrivate(depth, nodeDefault) {
  const effective = (depth === null || depth === undefined) ? nodeDefault : depth;
  return typeof effective === 'number' && effective <= -1;
}
