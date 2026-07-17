package media

import (
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
)

// Golden raw/compressed pairs captured from a real fpcalc run (chromaprint
// 1.5.1, default algorithm TEST2): the same file fingerprinted with and
// without -raw. Two shapes: a constant sine-mix stream (long runs of zero XOR
// deltas) and a pink-noise stream (dense, varied deltas exercising the 5-bit
// exception path).
var compressGolden = []struct {
	name string
	raw  string // fpcalc -raw -plain output (comma-separated uint32s)
	want string // fpcalc -plain output (compressed base64)
}{
	{
		name: "constant sine mix",
		raw: "558758263,558758263,558758263,558758263,558758263,558758263,558758263,558758263," +
			"558758263,558758263,558758263,558758263,558758263,558758263,558758263,558758263," +
			"558758263,558758263,558758263,558758263,558758263,558758263,558758263,558758263," +
			"558758263,558758263,558758263,558758263,558758263,558758263,558758263,558758263," +
			"558758263,558758263,558758263,558758263,558758263,558758263,558758263,558758263," +
			"558758263,558758263,558758263",
		want: "AQAAK0mUaEkSZSoAAAAAAAAAAAAAAAAAAAAA",
	},
	{
		name: "pink noise",
		raw: "2515916061,2516440381,2516442428,2499673404,2491284829,2491282781,2505962879," +
			"2505953791,2505953774,2505396726,2505528822,2505536982,2505532886,2547477974," +
			"3621221846,3623343574,3589657943,3589666167,3589599607,3589599607,3589666173," +
			"3078083965,3078084477,3078084445,3061306717,3061183837,3077961599",
		want: "AQAAG1FCSYmUJNLgI8ePNMchfviGEMyJHz6OX3iOy8h_-LgB6cZpMTiMHydEHoccokoKhIBgSinrgBECAWcMdBA",
	},
}

func parseRawFingerprint(t *testing.T, s string) []uint32 {
	t.Helper()
	parts := strings.Split(s, ",")
	out := make([]uint32, len(parts))
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			t.Fatalf("bad raw value %q: %v", p, err)
		}
		out[i] = uint32(v)
	}
	return out
}

func TestCompressFingerprint_GoldenPairs(t *testing.T) {
	for _, tc := range compressGolden {
		t.Run(tc.name, func(t *testing.T) {
			got := CompressFingerprint(parseRawFingerprint(t, tc.raw))
			if got != tc.want {
				t.Errorf("CompressFingerprint = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCompressFingerprint_Empty(t *testing.T) {
	got := CompressFingerprint(nil)
	b, err := base64.RawURLEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("output is not URL-safe base64: %v", err)
	}
	// Header only: algorithm byte + zero 24-bit count.
	want := []byte{chromaprintAlgorithm, 0, 0, 0}
	if len(b) != len(want) || b[0] != want[0] || b[1] != 0 || b[2] != 0 || b[3] != 0 {
		t.Errorf("empty fingerprint = % x, want % x", b, want)
	}
}

// A single sub-fingerprint with the top bit set produces the maximum bit-
// position delta (32), exercising the largest 5-bit exception value (25).
func TestCompressFingerprint_MaxDelta(t *testing.T) {
	got := CompressFingerprint([]uint32{1 << 31})
	b, err := base64.RawURLEncoding.DecodeString(got)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if b[0] != chromaprintAlgorithm || b[3] != 1 {
		t.Fatalf("bad header: % x", b[:4])
	}
	// Body: normal bits 7,0 (3 bits each) then exception 25 (5 bits) →
	// 0b111 | 0b000<<3 | 0b11001<<6 = two bytes 0x47, 0x06.
	if len(b) != 6 || b[4] != 0x47 || b[5] != 0x06 {
		t.Errorf("body = % x, want 47 06", b[4:])
	}
}
