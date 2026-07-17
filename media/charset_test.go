package media

import (
	"testing"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
)

// enc encodes a UTF-8 string into the given legacy charset — fixtures are
// generated with the same tables the decoder uses, so they can't drift.
func enc(t *testing.T, e encoding.Encoding, s string) []byte {
	t.Helper()
	b, err := e.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatalf("encode fixture %q: %v", s, err)
	}
	return b
}

func TestDetectCharset(t *testing.T) {
	cases := []struct {
		name   string
		fields [][]byte
		want   string
	}{
		{"empty", nil, "utf-8"},
		{"ascii", [][]byte{[]byte("Plain Title"), []byte("Some Artist")}, "utf-8"},
		{"utf8-umlauts", [][]byte{[]byte("Grüße aus Köln")}, "utf-8"},
		{"cp1252-german", [][]byte{
			enc(t, charmap.Windows1252, "Grüße aus Köln"),
			enc(t, charmap.Windows1252, "Die Ärzte"),
		}, "windows-1252"},
		{"cp1251-russian", [][]byte{
			enc(t, charmap.Windows1251, "Виктор Цой"),
			enc(t, charmap.Windows1251, "Группа крови"),
			enc(t, charmap.Windows1251, "Кино"),
		}, "windows-1251"},
		{"koi8r-russian", [][]byte{
			enc(t, charmap.KOI8R, "Виктор Цой"),
			enc(t, charmap.KOI8R, "Группа крови"),
			enc(t, charmap.KOI8R, "Кино"),
		}, "koi8-r"},
		{"shiftjis-japanese", [][]byte{
			enc(t, japanese.ShiftJIS, "上を向いて歩こう"),
			enc(t, japanese.ShiftJIS, "坂本九"),
		}, "shift_jis"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DetectCharset(c.fields); got != c.want {
				t.Errorf("DetectCharset = %q, want %q", got, c.want)
			}
		})
	}
}

func TestDecodeWith(t *testing.T) {
	if got, ok := DecodeWith("windows-1251", enc(t, charmap.Windows1251, "Кино")); !ok || got != "Кино" {
		t.Errorf("cp1251 = %q ok=%v", got, ok)
	}
	if _, ok := DecodeWith("ebcdic", []byte("x")); ok {
		t.Error("unknown charset must return ok=false")
	}
	// Malformed input still yields a previewable string (U+FFFD substitution).
	if got, ok := DecodeWith("utf-8", []byte{0xFF, 0xFE, 'a'}); !ok || got == "" {
		t.Errorf("malformed utf-8 = %q ok=%v, want non-empty preview", got, ok)
	}
}

func TestReencodeLatin1(t *testing.T) {
	// The mis-declared ID3v2.3 case: cp1251 bytes decoded as Latin-1 produce
	// mojibake; reinterpreting them as cp1251 restores the original.
	raw := enc(t, charmap.Windows1251, "Кино")
	mojibake, _ := DecodeWith("iso-8859-1", raw)
	got, ok := ReencodeLatin1(mojibake, "windows-1251")
	if !ok || got != "Кино" {
		t.Errorf("ReencodeLatin1 = %q ok=%v, want Кино", got, ok)
	}
	// A string that never was Latin-1 (runes > U+00FF) is left untouched.
	if got, ok := ReencodeLatin1("Кино", "windows-1251"); ok || got != "Кино" {
		t.Errorf("non-latin1 input = %q ok=%v, want unchanged/false", got, ok)
	}
}
