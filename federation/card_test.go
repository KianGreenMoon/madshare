package federation

import (
	"strings"
	"testing"
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
	if got := CleanPeerName(strings.Repeat("a", 300)); len(got) != 100 {
		t.Errorf("cap: len = %d, want 100", len(got))
	}
}
