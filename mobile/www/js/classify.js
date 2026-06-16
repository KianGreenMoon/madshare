// classify.js — the connection-safety classification for the Android launcher.
//
// This is the security-critical core of the app (docs/architecture/android-app.md
// §4). It decides, purely from a URL, whether sending the session cookie over that
// connection is safe. Kept DOM-free and dependency-free so it is unit-testable
// without a browser (tests/classify.test.mjs, run `node --test`). Nothing here
// touches the network or storage — given a URL string it returns a verdict.
//
// The model (§4): auto-trust what we can positively recognise (TLS, loopback,
// Tor, Yggdrasil), treat the user's own LAN as acceptable-with-a-note, and
// HARD-WARN on everything else over plaintext (public IPs and bare hostnames).
// A per-server "I trust this network" override downgrades UNSAFE_PUBLIC only.

export const CLASS = {
  SAFE_TLS:      'SAFE_TLS',      // https — transport-encrypted
  SAFE_LOCAL:    'SAFE_LOCAL',    // loopback (127/8, ::1, localhost)
  SAFE_OVERLAY:  'SAFE_OVERLAY',  // Tor (.onion) / Yggdrasil (0200::/7) — self-encrypting
  LOCAL_LAN:     'LOCAL_LAN',     // RFC1918 / ULA / link-local — plaintext on your own LAN
  UNSAFE_PUBLIC: 'UNSAFE_PUBLIC', // public IP or hostname over plaintext — leak risk
  INVALID:       'INVALID',       // not a parseable URL
};

// Action the gate takes for a (possibly override-adjusted) class.
export const ACTION = {
  CONNECT: 'connect', // proceed silently
  NOTE:    'note',    // proceed, show a dismissible one-line note (LAN plaintext)
  WARN:    'warn',    // block behind the §4.3 confirm dialog
  INVALID: 'invalid', // unusable URL
};

// normalizeUrl turns user input into a canonical URL string, defaulting a
// missing scheme to http:// (the common ygg/LAN case). Returns null if the
// result still isn't a valid http(s) URL with a host.
export function normalizeUrl(input) {
  if (typeof input !== 'string') return null;
  let s = input.trim();
  if (!s) return null;
  if (!/^[a-zA-Z][a-zA-Z0-9+.-]*:\/\//.test(s)) s = 'http://' + s;
  let u;
  try { u = new URL(s); } catch { return null; }
  if (u.protocol !== 'http:' && u.protocol !== 'https:') return null;
  if (!u.hostname) return null;
  return u.toString();
}

// parseIPv4 returns [a,b,c,d] for a strict dotted-decimal IPv4, else null.
export function parseIPv4(host) {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(host);
  if (!m) return null;
  const parts = m.slice(1);
  // Reject leading zeros (e.g. "010") — avoids octal ambiguity / sneaky inputs.
  if (parts.some((o) => o.length > 1 && o[0] === '0')) return null;
  const octets = parts.map(Number);
  if (octets.some((o) => o > 255)) return null;
  return octets;
}

// parseIPv6 returns an array of 8 16-bit groups for a valid IPv6 literal
// (with `::` compression and an optional trailing embedded IPv4), else null.
// A zone id (`%eth0`) is stripped before parsing.
export function parseIPv6(host) {
  let s = host;
  const zone = s.indexOf('%');
  if (zone !== -1) s = s.slice(0, zone);
  if (s === '') return null;

  // At most one '::'.
  if (s.indexOf('::') !== s.lastIndexOf('::')) return null;

  const expand = (groups) => {
    const out = [];
    for (let i = 0; i < groups.length; i++) {
      const g = groups[i];
      if (g === '') return null; // empty group only legal via '::', handled below
      if (g.includes('.')) {
        if (i !== groups.length - 1) return null; // IPv4 only allowed as the last group
        const v4 = parseIPv4(g);
        if (!v4) return null;
        out.push((v4[0] << 8) | v4[1], (v4[2] << 8) | v4[3]);
      } else {
        if (!/^[0-9a-fA-F]{1,4}$/.test(g)) return null;
        out.push(parseInt(g, 16));
      }
    }
    return out;
  };

  let groups;
  if (s.includes('::')) {
    const [headStr, tailStr] = s.split('::');
    const head = headStr === '' ? [] : expand(headStr.split(':'));
    const tail = tailStr === '' ? [] : expand(tailStr.split(':'));
    if (head === null || tail === null) return null;
    const fill = 8 - head.length - tail.length;
    if (fill < 1) return null; // '::' must stand for at least one zero group
    groups = [...head, ...new Array(fill).fill(0), ...tail];
  } else {
    groups = expand(s.split(':'));
    if (groups === null || groups.length !== 8) return null;
  }
  return groups;
}

function isLoopback(host, v4, v6) {
  if (host === 'localhost') return true;
  if (v4) return v4[0] === 127;            // 127.0.0.0/8
  if (v6) return v6.every((g, i) => (i === 7 ? g === 1 : g === 0)); // ::1
  return false;
}

// classify returns one of CLASS.* for a URL string or URL object.
export function classify(input) {
  let u;
  try { u = typeof input === 'string' ? new URL(input) : input; } catch { return CLASS.INVALID; }
  if (!u || (u.protocol !== 'http:' && u.protocol !== 'https:')) return CLASS.INVALID;

  if (u.protocol === 'https:') return CLASS.SAFE_TLS;

  // http: from here on. URL.hostname keeps IPv6 in brackets ("[::1]") — strip
  // them so the IP parsers see the bare address.
  let host = u.hostname.toLowerCase();
  if (host.startsWith('[') && host.endsWith(']')) host = host.slice(1, -1);
  const v4 = parseIPv4(host);
  const v6 = v4 ? null : parseIPv6(host);

  if (isLoopback(host, v4, v6)) return CLASS.SAFE_LOCAL;
  if (host.endsWith('.onion')) return CLASS.SAFE_OVERLAY; // Tor-encrypted

  if (v4) {
    const [a, b] = v4;
    if (a === 10) return CLASS.LOCAL_LAN;                       // 10/8
    if (a === 172 && b >= 16 && b <= 31) return CLASS.LOCAL_LAN; // 172.16/12
    if (a === 192 && b === 168) return CLASS.LOCAL_LAN;          // 192.168/16
    if (a === 169 && b === 254) return CLASS.LOCAL_LAN;          // 169.254/16 link-local
    return CLASS.UNSAFE_PUBLIC;                                  // public IPv4 (incl. 100.64/10 CGNAT)
  }

  if (v6) {
    const b0 = v6[0] >> 8;
    if ((b0 & 0xfe) === 0x02) return CLASS.SAFE_OVERLAY;        // 0200::/7 Yggdrasil
    if ((b0 & 0xfe) === 0xfc) return CLASS.LOCAL_LAN;           // fc00::/7 ULA
    if ((v6[0] & 0xffc0) === 0xfe80) return CLASS.LOCAL_LAN;    // fe80::/10 link-local
    return CLASS.UNSAFE_PUBLIC;                                 // global / IPv4-mapped → conservative
  }

  // A hostname, not an IP literal: DNS + Host header in clear → unsafe.
  return CLASS.UNSAFE_PUBLIC;
}

// evaluate is what the launcher calls: it classifies, applies the per-server
// trusted-network override (downgrades UNSAFE_PUBLIC → SAFE_OVERLAY only), and
// returns the gate verdict plus display bits.
export function evaluate(input, { trusted = false } = {}) {
  const url = typeof input === 'string' ? normalizeUrl(input) : input?.toString();
  if (!url) return { ok: false, cls: CLASS.INVALID, action: ACTION.INVALID, url: null, host: null, scheme: null };

  const u = new URL(url);
  let cls = classify(u);
  let overridden = false;
  if (trusted && cls === CLASS.UNSAFE_PUBLIC) { cls = CLASS.SAFE_OVERLAY; overridden = true; }

  let action;
  switch (cls) {
    case CLASS.SAFE_TLS:
    case CLASS.SAFE_LOCAL:
    case CLASS.SAFE_OVERLAY: action = ACTION.CONNECT; break;
    case CLASS.LOCAL_LAN:    action = ACTION.NOTE;    break;
    case CLASS.UNSAFE_PUBLIC: action = ACTION.WARN;   break;
    default:                 action = ACTION.INVALID; break;
  }

  return { ok: action !== ACTION.INVALID, cls, action, overridden, url, host: u.host, scheme: u.protocol.replace(':', '') };
}
