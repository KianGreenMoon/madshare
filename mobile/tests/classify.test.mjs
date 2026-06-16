// Unit tests for the connection-safety classifier (www/js/classify.js).
// Run with: node --test mobile/tests/classify.test.mjs
// This is the security gate of the Android app — keep coverage tight.
import test from 'node:test';
import assert from 'node:assert/strict';
import {
  CLASS, ACTION, classify, evaluate, normalizeUrl, parseIPv4, parseIPv6,
} from '../www/js/classify.js';

test('classify: TLS is always safe', () => {
  assert.equal(classify('https://media.example.org'), CLASS.SAFE_TLS);
  assert.equal(classify('https://203.0.113.5'), CLASS.SAFE_TLS);
  assert.equal(classify('https://[2001:db8::1]'), CLASS.SAFE_TLS);
});

test('classify: loopback is SAFE_LOCAL', () => {
  assert.equal(classify('http://localhost:3000'), CLASS.SAFE_LOCAL);
  assert.equal(classify('http://127.0.0.1:3000'), CLASS.SAFE_LOCAL);
  assert.equal(classify('http://127.1.2.3'), CLASS.SAFE_LOCAL); // all of 127/8
  assert.equal(classify('http://[::1]:3000'), CLASS.SAFE_LOCAL);
});

test('classify: Tor and Yggdrasil are SAFE_OVERLAY', () => {
  assert.equal(classify('http://abcdef234567.onion'), CLASS.SAFE_OVERLAY);
  assert.equal(classify('http://[200:abcd:1234::1]:3000'), CLASS.SAFE_OVERLAY); // 0200::/7
  assert.equal(classify('http://[300::1]'), CLASS.SAFE_OVERLAY);                // 0300 still in /7
  assert.equal(classify('http://[3ff:ffff::1]'), CLASS.SAFE_OVERLAY);          // top of 0200::/7
});

test('classify: private IPv4 ranges are LOCAL_LAN', () => {
  assert.equal(classify('http://10.0.0.5:3000'), CLASS.LOCAL_LAN);
  assert.equal(classify('http://172.16.0.1'), CLASS.LOCAL_LAN);
  assert.equal(classify('http://172.31.255.255'), CLASS.LOCAL_LAN);
  assert.equal(classify('http://192.168.1.67:3000'), CLASS.LOCAL_LAN);
  assert.equal(classify('http://169.254.10.10'), CLASS.LOCAL_LAN); // link-local
});

test('classify: 172.32/12 boundary is NOT private', () => {
  assert.equal(classify('http://172.15.0.1'), CLASS.UNSAFE_PUBLIC);
  assert.equal(classify('http://172.32.0.1'), CLASS.UNSAFE_PUBLIC);
});

test('classify: ULA and link-local IPv6 are LOCAL_LAN', () => {
  assert.equal(classify('http://[fc00::1]'), CLASS.LOCAL_LAN);
  assert.equal(classify('http://[fd12:3456::1]:3000'), CLASS.LOCAL_LAN);
  assert.equal(classify('http://[fe80::1]'), CLASS.LOCAL_LAN);
  assert.equal(classify('http://[febf::1]'), CLASS.LOCAL_LAN); // top of fe80::/10
});

test('classify: public IPs and hostnames over http are UNSAFE_PUBLIC', () => {
  assert.equal(classify('http://203.0.113.5:3000'), CLASS.UNSAFE_PUBLIC);
  assert.equal(classify('http://media.example.org'), CLASS.UNSAFE_PUBLIC);
  assert.equal(classify('http://[2001:db8::1]:3000'), CLASS.UNSAFE_PUBLIC);
  assert.equal(classify('http://my-nas'), CLASS.UNSAFE_PUBLIC); // bare hostname
});

test('classify: CGNAT / Tailscale 100.64/10 is NOT auto-trusted', () => {
  assert.equal(classify('http://100.64.0.1'), CLASS.UNSAFE_PUBLIC);
  assert.equal(classify('http://100.127.255.255'), CLASS.UNSAFE_PUBLIC);
});

test('classify: fe80::/10 lower boundary fe7f is not link-local', () => {
  // fe7f is below fe80::/10 → global → unsafe (defensive boundary check).
  assert.equal(classify('http://[fe7f::1]'), CLASS.UNSAFE_PUBLIC);
});

test('classify: garbage is INVALID', () => {
  assert.equal(classify('not a url'), CLASS.INVALID);
  assert.equal(classify('ftp://example.org'), CLASS.INVALID);
});

test('parseIPv4: strictness', () => {
  assert.deepEqual(parseIPv4('192.168.1.1'), [192, 168, 1, 1]);
  assert.equal(parseIPv4('256.0.0.1'), null);
  assert.equal(parseIPv4('010.0.0.1'), null);   // leading zero rejected
  assert.equal(parseIPv4('1.2.3'), null);
  assert.equal(parseIPv4('1.2.3.4.5'), null);
});

test('parseIPv6: compression and embedded IPv4', () => {
  assert.deepEqual(parseIPv6('::1'), [0, 0, 0, 0, 0, 0, 0, 1]);
  assert.deepEqual(parseIPv6('2001:db8::'), [0x2001, 0xdb8, 0, 0, 0, 0, 0, 0]);
  assert.deepEqual(parseIPv6('::ffff:192.168.1.1'),
    [0, 0, 0, 0, 0, 0xffff, (192 << 8) | 168, (1 << 8) | 1]);
  assert.equal(parseIPv6('2001::db8::1'), null);    // two '::'
  assert.equal(parseIPv6('1:2:3:4:5:6:7:8:9'), null); // too many groups
  assert.equal(parseIPv6('1:2:3:4:5:6:7'), null);     // too few, no '::'
  assert.equal(parseIPv6('gggg::1'), null);
});

test('parseIPv6: strips zone id', () => {
  assert.deepEqual(parseIPv6('fe80::1%eth0'), [0xfe80, 0, 0, 0, 0, 0, 0, 1]);
});

test('normalizeUrl: defaults scheme to http and validates', () => {
  assert.equal(normalizeUrl('192.168.1.5:3000'), 'http://192.168.1.5:3000/');
  assert.equal(normalizeUrl('  media.example.org '), 'http://media.example.org/');
  assert.equal(normalizeUrl('https://x.example'), 'https://x.example/');
  assert.equal(normalizeUrl('ftp://x'), null);
  assert.equal(normalizeUrl(''), null);
  assert.equal(normalizeUrl(null), null);
});

test('evaluate: actions map from class', () => {
  assert.equal(evaluate('https://x.example').action, ACTION.CONNECT);
  assert.equal(evaluate('http://localhost').action, ACTION.CONNECT);
  assert.equal(evaluate('http://[200::1]').action, ACTION.CONNECT);
  assert.equal(evaluate('http://192.168.1.5:3000').action, ACTION.NOTE);
  assert.equal(evaluate('http://203.0.113.5').action, ACTION.WARN);
  assert.equal(evaluate('nonsense://').action, ACTION.INVALID);
});

test('evaluate: trusted override downgrades only UNSAFE_PUBLIC', () => {
  const warned = evaluate('http://203.0.113.5:3000', { trusted: false });
  assert.equal(warned.action, ACTION.WARN);

  const trusted = evaluate('http://203.0.113.5:3000', { trusted: true });
  assert.equal(trusted.action, ACTION.CONNECT);
  assert.equal(trusted.cls, CLASS.SAFE_OVERLAY);
  assert.equal(trusted.overridden, true);

  // Override does not change a LAN note into a silent connect (only UNSAFE_PUBLIC).
  const lan = evaluate('http://192.168.1.5', { trusted: true });
  assert.equal(lan.action, ACTION.NOTE);
  assert.equal(lan.overridden, false);
});

test('evaluate: surfaces host and scheme for the UI', () => {
  const r = evaluate('http://203.0.113.5:3000');
  assert.equal(r.host, '203.0.113.5:3000');
  assert.equal(r.scheme, 'http');
  assert.equal(r.url, 'http://203.0.113.5:3000/');
});
