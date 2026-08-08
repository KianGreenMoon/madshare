package media

import (
	"testing"

	"golang.org/x/text/encoding"
)

// The cases below are the ones that decide whether the review page's charset
// prompt is useful or noise. A prompt that fires on ordinary accented Latin
// trains people to dismiss it, at which point it is worse than nothing.

func TestSuggestCharsetFlagsMisdecodedCyrillic(t *testing.T) {
	// Real bytes from an ID3 tag holding CP1251 "Трек  1", decoded as Latin-1.
	// This is the exact case that prompted the feature.
	got := SuggestCharset("Òðåê  1", "JMJ", "Metamorphoses")
	if got != "windows-1251" {
		t.Errorf("SuggestCharset = %q, want windows-1251", got)
	}
}

func TestSuggestCharsetIgnoresOrdinaryAccentedLatin(t *testing.T) {
	// Isolated accents are what real Latin-script text looks like. Flagging
	// these would put a prompt on half of a European library.
	quiet := [][]string{
		{"Björk", "Björk", "Homogenic"},
		{"Édith Piaf", "Édith Piaf", "L'Hymne à l'amour"},
		{"Sigur Rós", "Sigur Rós", "Ágætis byrjun"},
		{"Mötley Crüe", "Mötley Crüe", "Dr. Feelgood"},
		{"Antônio Carlos Jobim", "", "Corcovado"},
	}
	for _, fields := range quiet {
		if got := SuggestCharset(fields...); got != "" {
			t.Errorf("SuggestCharset(%q) = %q, want no suggestion", fields[0], got)
		}
	}
}

func TestSuggestCharsetIgnoresTextThatIsAlreadyRight(t *testing.T) {
	// Runes above U+00FF mean the frame declared its encoding honestly, so
	// there is nothing to reinterpret — including text already repaired once.
	already := [][]string{
		{"Трек  1", "JMJ", "Metamorphoses"},
		{"坂本龍一", "坂本龍一", "戦場のメリークリスマス"},
		{"Plain ASCII Title", "Some Artist", "Some Album"},
		{"", "", ""},
	}
	for _, fields := range already {
		if got := SuggestCharset(fields...); got != "" {
			t.Errorf("SuggestCharset(%q) = %q, want no suggestion", fields[0], got)
		}
	}
}

func TestSuggestCharsetIsQuietOnASingleHighByte(t *testing.T) {
	// One high byte on its own is an accent, not a mis-decode — the run is the
	// whole signal.
	if got := SuggestCharset("Café"); got != "" {
		t.Errorf("SuggestCharset(\"Café\") = %q, want no suggestion", got)
	}
}

func TestSuggestCharsetSeparatesTheTwoCyrillicCodepages(t *testing.T) {
	// KOI8-R and CP1251 both turn the same bytes into Cyrillic; picking the
	// wrong one inverts case mid-word, which charsetScore penalises. Round-trip
	// real text through each encoder rather than hand-writing byte vectors, so
	// the test cannot be wrong about the codepage it claims to be testing.
	for _, cs := range []string{"windows-1251", "koi8-r"} {
		enc, ok := encoderFor(cs)
		if !ok {
			t.Fatalf("no encoder for %s", cs)
		}
		raw, err := enc.Bytes([]byte("Хорошая песня о любви"))
		if err != nil {
			t.Fatalf("%s encode: %v", cs, err)
		}
		misdecoded, _ := DecodeWith("iso-8859-1", raw)
		if got := SuggestCharset(misdecoded); got != cs {
			t.Errorf("text encoded as %s, mis-read as Latin-1 → SuggestCharset = %q, want %s",
				cs, got, cs)
		}
	}
}

// encoderFor exposes the candidate table's encoders to the tests.
func encoderFor(name string) (*encoding.Encoder, bool) {
	for _, c := range suggestCharsets {
		if c.name == name && c.enc != nil {
			return c.enc.NewEncoder(), true
		}
	}
	return nil, false
}
