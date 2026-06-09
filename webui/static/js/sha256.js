// sha256.js — minimal dependency-free incremental SHA-256 (FIPS 180-4).
//
// Produces the same lowercase hex digest as Go's crypto/sha256 for the same
// bytes, fed any way (one-shot or chunk-by-chunk), so the upload precheck hash
// matches what the server stores. This is the pure-JS implementation that runs
// behind the hash-worker; a WASM hasher can later replace it behind the same
// hashBlob() interface (see docs/plans/upload-rework.md §3b).

const K = new Uint32Array([
  0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5,
  0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174,
  0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da,
  0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967,
  0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85,
  0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070,
  0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3,
  0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2,
]);

// createSha256 returns an incremental hasher: feed bytes with update(), then read
// the lowercase-hex digest with hex(). update() accepts arbitrary chunk sizes.
export function createSha256() {
  const h = new Uint32Array([
    0x6a09e667, 0xbb67ae85, 0x3c6ef372, 0xa54ff53a, 0x510e527f, 0x9b05688c, 0x1f83d9ab, 0x5be0cd19,
  ]);
  const block = new Uint8Array(64); // partial-block buffer
  const w = new Uint32Array(64);
  let blockLen = 0;                 // bytes currently buffered in `block`
  let totalLen = 0;                 // total bytes processed (for length padding)

  function processBlock(buf, off) {
    for (let i = 0; i < 16; i++) {
      w[i] = (buf[off + i * 4] << 24) | (buf[off + i * 4 + 1] << 16) |
             (buf[off + i * 4 + 2] << 8) | buf[off + i * 4 + 3];
    }
    for (let i = 16; i < 64; i++) {
      const x = w[i - 15], y = w[i - 2];
      const s0 = ((x >>> 7) | (x << 25)) ^ ((x >>> 18) | (x << 14)) ^ (x >>> 3);
      const s1 = ((y >>> 17) | (y << 15)) ^ ((y >>> 19) | (y << 13)) ^ (y >>> 10);
      w[i] = (w[i - 16] + s0 + w[i - 7] + s1) | 0;
    }
    let a = h[0], b = h[1], c = h[2], d = h[3], e = h[4], f = h[5], g = h[6], hh = h[7];
    for (let i = 0; i < 64; i++) {
      const S1 = ((e >>> 6) | (e << 26)) ^ ((e >>> 11) | (e << 21)) ^ ((e >>> 25) | (e << 7));
      const ch = (e & f) ^ (~e & g);
      const t1 = (hh + S1 + ch + K[i] + w[i]) | 0;
      const S0 = ((a >>> 2) | (a << 30)) ^ ((a >>> 13) | (a << 19)) ^ ((a >>> 22) | (a << 10));
      const maj = (a & b) ^ (a & c) ^ (b & c);
      const t2 = (S0 + maj) | 0;
      hh = g; g = f; f = e; e = (d + t1) | 0; d = c; c = b; b = a; a = (t1 + t2) | 0;
    }
    h[0] = (h[0] + a) | 0; h[1] = (h[1] + b) | 0; h[2] = (h[2] + c) | 0; h[3] = (h[3] + d) | 0;
    h[4] = (h[4] + e) | 0; h[5] = (h[5] + f) | 0; h[6] = (h[6] + g) | 0; h[7] = (h[7] + hh) | 0;
  }

  return {
    update(bytes) {
      let i = 0;
      totalLen += bytes.length;
      if (blockLen > 0) {                              // top up a partial block
        while (blockLen < 64 && i < bytes.length) block[blockLen++] = bytes[i++];
        if (blockLen === 64) { processBlock(block, 0); blockLen = 0; }
      }
      while (i + 64 <= bytes.length) { processBlock(bytes, i); i += 64; } // full blocks
      while (i < bytes.length) block[blockLen++] = bytes[i++];            // remainder
    },
    hex() {
      const bitLen = totalLen * 8;                     // files ≤ 1 TiB → exact in a JS number
      block[blockLen] = 0x80;
      let p = blockLen + 1;
      if (p > 56) {                                    // no room for the length → flush
        while (p < 64) block[p++] = 0;
        processBlock(block, 0);
        p = 0;
      }
      while (p < 56) block[p++] = 0;
      const hi = Math.floor(bitLen / 0x100000000);
      const lo = bitLen >>> 0;
      block[56] = (hi >>> 24) & 0xff; block[57] = (hi >>> 16) & 0xff;
      block[58] = (hi >>> 8) & 0xff;  block[59] = hi & 0xff;
      block[60] = (lo >>> 24) & 0xff; block[61] = (lo >>> 16) & 0xff;
      block[62] = (lo >>> 8) & 0xff;  block[63] = lo & 0xff;
      processBlock(block, 0);
      let out = '';
      for (let i = 0; i < 8; i++) out += (h[i] >>> 0).toString(16).padStart(8, '0');
      return out;
    },
  };
}

// hashBlob streams a Blob/File through the hasher in chunks — never buffering the
// whole file — and returns its SHA-256 hex.
export async function hashBlob(blob) {
  const h = createSha256();
  const reader = blob.stream().getReader();
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    h.update(value);
  }
  return h.hex();
}
