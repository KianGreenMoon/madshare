package media

import "encoding/base64"

// Chromaprint fingerprint compression (docs/architecture/tag-suggestions.md,
// P1). AcoustID's lookup API takes the compressed textual fingerprint fpcalc
// prints by default, but analysis stores only the raw uint32 stream
// (`fpcalc -raw`, audio_fingerprints.fingerprint). CompressFingerprint
// re-derives the compressed form from the stored stream, so every
// already-analyzed file can be looked up without touching the blob or fpcalc.
//
// The format (chromaprint's FingerprintCompressor): each sub-fingerprint is
// XORed with its predecessor, the result decomposed into 1-based set-bit
// positions, and the deltas between successive positions emitted with a
// 0 terminator per value. The delta stream is packed twice over — every delta
// as min(d, 7) in 3 bits, then each d >= 7 again as d-7 in 5 bits — after a
// 4-byte header (algorithm, 24-bit big-endian value count), and the whole
// thing is URL-safe unpadded base64.

// chromaprintAlgorithm is fpcalc's default fingerprint algorithm
// (CHROMAPRINT_ALGORITHM_TEST2 = 1) — the first header byte.
const chromaprintAlgorithm = 1

// CompressFingerprint encodes a raw chromaprint sub-fingerprint stream into
// the compressed base64 form the AcoustID API accepts.
func CompressFingerprint(raw []uint32) string {
	// Per-value XOR delta → 1-based set-bit position deltas, 0-terminated.
	deltas := make([]uint32, 0, len(raw)*8)
	var prev uint32
	for i, v := range raw {
		x := v
		if i > 0 {
			x ^= prev
		}
		prev = v
		bit, lastBit := uint32(1), uint32(0)
		for ; x != 0; x >>= 1 {
			if x&1 != 0 {
				deltas = append(deltas, bit-lastBit)
				lastBit = bit
			}
			bit++
		}
		deltas = append(deltas, 0)
	}

	w := bitWriter{buf: []byte{
		chromaprintAlgorithm,
		byte(len(raw) >> 16), byte(len(raw) >> 8), byte(len(raw)),
	}}
	for _, d := range deltas {
		w.write(min(d, 7), 3)
	}
	for _, d := range deltas {
		if d >= 7 {
			w.write(d-7, 5) // max delta is 32, so d-7 always fits 5 bits
		}
	}
	w.flush()
	return base64.RawURLEncoding.EncodeToString(w.buf)
}

// bitWriter packs values LSB-first into bytes, matching chromaprint's
// BitStringWriter.
type bitWriter struct {
	buf  []byte
	acc  uint32
	nacc uint // bits currently held in acc
}

func (w *bitWriter) write(v uint32, bits uint) {
	w.acc |= v << w.nacc
	w.nacc += bits
	for w.nacc >= 8 {
		w.buf = append(w.buf, byte(w.acc))
		w.acc >>= 8
		w.nacc -= 8
	}
}

func (w *bitWriter) flush() {
	if w.nacc > 0 {
		w.buf = append(w.buf, byte(w.acc))
		w.acc, w.nacc = 0, 0
	}
}
