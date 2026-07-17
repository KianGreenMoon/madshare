package media

import (
	"bytes"
	"math"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/japanese"
)

// Charset detection and decoding for the local tag-suggestion sources
// (docs/architecture/tag-suggestions.md). ID3v1 carries no charset, and plenty
// of ID3v2.3 files declare ISO-8859-1 while actually holding a Windows
// codepage — the suggestion panel decodes with a detected charset and offers a
// manual override, so the candidate list here is the single source of truth
// for both the API's ?charset= allowlist and the UI dropdown.

// CharsetUTF8 is the detector's answer for valid-UTF-8 (including pure ASCII)
// input.
const CharsetUTF8 = "utf-8"

// suggestCharsets is the fixed candidate set, in tie-break order (an earlier
// entry wins an equal detection score). utf-8 is handled before scoring.
var suggestCharsets = []struct {
	name string
	enc  encoding.Encoding
}{
	{CharsetUTF8, nil},
	{"windows-1252", charmap.Windows1252},
	{"iso-8859-1", charmap.ISO8859_1},
	{"windows-1251", charmap.Windows1251},
	{"koi8-r", charmap.KOI8R},
	{"shift_jis", japanese.ShiftJIS},
}

// CharsetNames returns the supported charset names in presentation order.
func CharsetNames() []string {
	names := make([]string, len(suggestCharsets))
	for i, c := range suggestCharsets {
		names[i] = c.name
	}
	return names
}

// ValidCharset reports whether name is a supported charset name.
func ValidCharset(name string) bool {
	for _, c := range suggestCharsets {
		if c.name == name {
			return true
		}
	}
	return false
}

// DecodeWith decodes b as the named charset. Malformed input never fails —
// x/text decoders substitute U+FFFD, and the utf-8 path does the same — so a
// wrong override still yields a previewable string. ok is false only for an
// unknown charset name.
func DecodeWith(name string, b []byte) (string, bool) {
	for _, c := range suggestCharsets {
		if c.name != name {
			continue
		}
		if c.enc == nil { // utf-8
			if utf8.Valid(b) {
				return string(b), true
			}
			return string(bytes.ToValidUTF8(b, []byte("�"))), true
		}
		out, err := c.enc.NewDecoder().Bytes(b)
		if err != nil {
			return string(bytes.ToValidUTF8(b, []byte("�"))), true
		}
		return string(out), true
	}
	return "", false
}

// DetectCharset guesses the charset of the given raw text fields (best-effort;
// the UI override is the contract). Valid UTF-8 wins outright; otherwise every
// single-byte/multi-byte candidate is decoded and scored, highest score wins.
func DetectCharset(fields [][]byte) string {
	joined := bytes.Join(fields, []byte{'\n'})
	if len(joined) == 0 || utf8.Valid(joined) {
		return CharsetUTF8
	}
	best, bestScore := CharsetUTF8, math.MinInt
	for _, c := range suggestCharsets[1:] {
		s, _ := DecodeWith(c.name, joined)
		if score := charsetScore(s); score > bestScore {
			best, bestScore = c.name, score
		}
	}
	return best
}

// charsetScore rates a candidate decode: letters and word-like text score up,
// control chars / replacement runes score down. Three shape rules separate the
// codepages that plain letter-counting cannot:
//   - a mid-word lower→upper case flip is penalised — the signature of
//     Cyrillic read with the wrong Cyrillic codepage (cp1251 vs KOI8-R invert
//     case);
//   - adjacent accented-Latin letters are penalised — real Latin-script text
//     is mostly ASCII with isolated accents, whereas Cyrillic bytes read as
//     cp1252 become whole words of them;
//   - CJK/kana letters score their two encoded bytes, so a valid Shift-JIS
//     decode isn't outscored by a codepage that yields twice the runes.
func charsetScore(s string) int {
	score := 0
	var prev rune
	prevExtLatin := false
	for _, r := range s {
		extLatin := false
		switch {
		case r == utf8.RuneError || (unicode.IsControl(r) && r != '\n'):
			score -= 4
		case unicode.IsLetter(r):
			switch {
			case r >= 0xFF61 && r <= 0xFF9F:
				// Half-width katakana: one byte each in Shift-JIS and rare in
				// real tags — random single-byte Cyrillic decodes here, so it
				// must not collect the two-byte CJK bonus.
				score++
			case r >= 0x2E80: // CJK / kana / hangul blocks (two encoded bytes)
				score += 4
			case r >= 0xC0 && r <= 0x24F: // accented Latin
				score += 2
				extLatin = true
				if prevExtLatin {
					score -= 3
				}
			default:
				score += 2
			}
			if unicode.IsUpper(r) && unicode.IsLower(prev) {
				score -= 3
			}
		case unicode.IsDigit(r) || unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r):
			score++
		default:
			score -= 2
		}
		prev, prevExtLatin = r, extLatin
	}
	return score
}

// ReencodeLatin1 reinterprets a string that was decoded as ISO-8859-1 in the
// named charset instead — the override path for ID3v2.3 frames that declare
// Latin-1 but really hold a Windows codepage. The round-trip is lossless
// because every Latin-1-decoded rune is ≤ U+00FF; a string that doesn't fit
// (i.e. was never Latin-1) comes back unchanged with ok=false.
func ReencodeLatin1(s, charset string) (string, bool) {
	b := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xFF {
			return s, false
		}
		b = append(b, byte(r))
	}
	out, ok := DecodeWith(charset, b)
	if !ok {
		return s, false
	}
	return out, true
}
