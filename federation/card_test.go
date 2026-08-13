package federation

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const testKey = "0000000000000000000000000000000000000000000000000000000000000001"

func TestParseCard(t *testing.T) {
	card, err := ParseCard([]byte(`{"madshare_node_card": 0, "name": "  My Node  ", "public_key": "` + strings.ToUpper(testKey) + `"}`))
	if err != nil {
		t.Fatalf("ParseCard: %v", err)
	}
	if card.PublicKey != testKey {
		t.Errorf("key = %q, want lowercased %q", card.PublicKey, testKey)
	}
	if card.Name != "My Node" {
		t.Errorf("name = %q, want trimmed", card.Name)
	}

	bad := []string{
		`not json`,
		`{}`, // no marker, no key
		`{"madshare_node_card": 1, "public_key": "` + testKey + `"}`,       // future version
		`{"madshare_node_card": 0, "public_key": "abc"}`,                   // short key
		`{"madshare_node_card": 0, "public_key": "zz` + testKey[2:] + `"}`, // non-hex
	}
	for _, raw := range bad {
		if _, err := ParseCard([]byte(raw)); err == nil {
			t.Errorf("ParseCard accepted %q", raw)
		}
	}
}

func TestCleanPeerName(t *testing.T) {
	if got := CleanPeerName("  x  "); got != "x" {
		t.Errorf("trim: got %q", got)
	}
	if got := CleanPeerName(strings.Repeat("a", 300)); utf8.RuneCountInString(got) != MaxPeerNameRunes {
		t.Errorf("ascii cap: %d runes, want %d", utf8.RuneCountInString(got), MaxPeerNameRunes)
	}
	if got := CleanPeerName("Kians Musikserver"); got != "Kians Musikserver" {
		t.Errorf("short name altered: %q", got)
	}

	// The cap counts runes, so a multi-byte name is cut on a character
	// boundary. Capping bytes instead would slice the last character in half
	// and leave invalid UTF-8 behind — the whole point of the rune count.
	for _, name := range []string{
		strings.Repeat("я", 300), // 2 bytes per rune
		strings.Repeat("音", 300), // 3
		strings.Repeat("🎵", 300), // 4
		strings.Repeat("ä", 300), // 2, and what a German host name hits first
	} {
		got := CleanPeerName(name)
		if n := utf8.RuneCountInString(got); n != MaxPeerNameRunes {
			t.Errorf("%.1s cap: %d runes, want %d", name, n, MaxPeerNameRunes)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%.1s cap produced invalid UTF-8: %q", name, got)
		}
	}
}

// TestSanitizePeerName is the golden table for the display-integrity rules
// (docs/architecture/federation-trust.md §Name sanitization). Every case is a way two
// nodes could otherwise render identically, or a way one name could render as
// something it is not — plus the two accepted losses, asserted so that removing
// them is a deliberate act rather than an accident.
func TestSanitizePeerName(t *testing.T) {
	for _, tc := range []struct {
		what string
		in   string
		want string
	}{
		{"plain name untouched", "Kians Musikserver", "Kians Musikserver"},
		{"trims and collapses runs", "  My   Node  Two  ", "My Node Two"},

		// U+202E RIGHT-TO-LEFT OVERRIDE renders the tail reversed, so "studio"
		// can present itself as "oiduts" — or as another node's name entirely.
		{"bidi override stripped", "stu\u202edio", "studio"},
		// Zero-width padding is the invisible-difference vector: this must land on
		// exactly the string the real node produces, so the collision is visible
		// and an admin checks the address.
		{"zero-width space stripped", "stu\u200bdio", "studio"},
		{"zero-width non-joiner and BOM stripped", "st\u200cud\ufeffio", "studio"},
		// Controls: an embedded newline breaks a table row in two.
		{"newline and tab stripped", "stu\ndi\to", "studio"},
		{"private use stripped", "stu\uf8ffdio", "studio"},

		// NFC: "é" precomposed against "e" + U+0301. Two byte-different names
		// that render the same become one string.
		{"decomposed form composes", "Cafe\u0301", "Café"},
		// Zalgo: marks beyond the bound go, the base and a sane accent stay. NFC
		// runs first, so the first accent composed into the base — the bound is
		// two marks *following* a base character, which caps the rendered stack
		// without having to reason about which scripts precompose.
		{"combining marks bounded", "a\u0301\u0302\u0303\u0304\u0305b", "\u00e1\u0302\u0303b"},
		{"leading mark has no base", "\u0301studio", "studio"},

		{"nothing survives", "\u200b\u202e ", ""},
		{"only marks survive nothing", "\u0301\u0302", ""},

		// The two accepted costs, per the design. If these ever change, the doc's
		// §Name sanitization must change with them.
		{"emoji family loses its joiners", "\U0001f468\u200d\U0001f469\u200d\U0001f467", "\U0001f468\U0001f469\U0001f467"},
		{"Persian ZWNJ is lost", "\u0645\u06cc\u200c\u062e\u0648\u0627\u0646\u0645", "\u0645\u06cc\u062e\u0648\u0627\u0646\u0645"},
	} {
		if got := CleanPeerName(tc.in); got != tc.want {
			t.Errorf("%s: CleanPeerName(%q) = %q, want %q", tc.what, tc.in, got, tc.want)
		}
	}
}

// TestSanitizeCapsLast pins rule 6: the cap runs after stripping, so junk cannot
// consume the budget and truncate the real name. Capping first would leave this
// name a fraction of its length.
func TestSanitizeCapsLast(t *testing.T) {
	name := strings.Repeat("a", MaxPeerNameRunes)
	padded := strings.Repeat("\u200b", 200) + name
	if got := CleanPeerName(padded); got != name {
		t.Errorf("padded name = %q (%d runes), want the full %d-rune name",
			got, utf8.RuneCountInString(got), MaxPeerNameRunes)
	}
}

// TestSanitizeMarkReason checks the mark reason shares the rules at its own cap —
// one sanitizer, two limits.
func TestSanitizeMarkReason(t *testing.T) {
	if got := CleanMarkReason("advertised\u202e hash\u200b 3a9f…"); got != "advertised hash 3a9f…" {
		t.Errorf("reason = %q", got)
	}
	if got := CleanMarkReason(strings.Repeat("é", 400)); utf8.RuneCountInString(got) != MaxMarkReasonRunes {
		t.Errorf("reason cap: %d runes, want %d", utf8.RuneCountInString(got), MaxMarkReasonRunes)
	}
}

// TestPeerDisplayNeverBlank pins the rule the naming split rests on: a peer is
// never named by an empty string. Migration 033 emptied every local label, so
// after it Label() is blank for any peer whose owner has not renamed it and whose
// name we have not yet heard — and every log line and stats row named that peer
// "". Display() is what those callers use instead.
func TestPeerDisplayNeverBlank(t *testing.T) {
	key := strings.Repeat("ab", 32)
	for _, tc := range []struct {
		name, label, heard, want string
	}{
		{"local label wins", "my studio node", "node-b", "my studio node"},
		{"heard name when unlabelled", "", "node-b", "node-b"},
		{"short key when neither", "", "", key[:shortKeyRunes]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &ExternalNode{PublicKey: key, Label: tc.label, HeardName: tc.heard}
			if got := p.Display(); got != tc.want {
				t.Errorf("Display() = %q, want %q", got, tc.want)
			}
		})
	}
	// Label keeps returning empty: the network map needs that case to fall
	// through to what the *graph* calls a node before reaching for the key.
	if got := (&ExternalNode{PublicKey: key}).Name(); got != "" {
		t.Errorf("Label() with no names = %q, want empty", got)
	}
}
