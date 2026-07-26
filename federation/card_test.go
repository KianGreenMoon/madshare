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
		strings.Repeat("я", 300),  // 2 bytes per rune
		strings.Repeat("音", 300),  // 3
		strings.Repeat("🎵", 300), // 4
		strings.Repeat("ä", 300),  // 2, and what a German host name hits first
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
