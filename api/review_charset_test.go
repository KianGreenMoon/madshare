package api

import (
	"database/sql"
	"testing"

	"daemonlord.ygg/madshare/database"
)

func ns(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// The charset hint is what puts the mis-decode in front of the uploader while
// the file is still theirs to fix. It is carried by the SHARED review row, so
// the moderation queue gets it too — the second person to look at the file is
// the other one who can still catch it cheaply.
func TestReviewItemCarriesCharsetHint(t *testing.T) {
	item := toReviewItem(&database.ReviewEntry{
		Title:       "Òðåê  1", // CP1251 bytes read as Latin-1
		Artist:      ns("JMJ"),
		AlbumArtist: ns("JMJ"),
		Album:       ns("Metamorphoses"),
	})
	if item.CharsetHint != "windows-1251" {
		t.Errorf("charset_hint = %q, want windows-1251", item.CharsetHint)
	}
}

func TestReviewItemHasNoHintForTextThatReadsFine(t *testing.T) {
	// Ordinary accented Latin must not be flagged: a prompt that fires on half a
	// European library is a prompt people learn to click past.
	clean := []database.ReviewEntry{
		{Title: "Jóga", Artist: ns("Björk"), AlbumArtist: ns("Björk"), Album: ns("Homogenic")},
		{Title: "Non, je ne regrette rien", Artist: ns("Édith Piaf"), Album: ns("L'Hymne à l'amour")},
		{Title: "Трек  1", Artist: ns("JMJ"), Album: ns("Metamorphoses")}, // already correct
		{Title: "Plain Title", Artist: ns("Some Artist"), Album: ns("Some Album")},
	}
	for _, e := range clean {
		if got := toReviewItem(&e).CharsetHint; got != "" {
			t.Errorf("%q: charset_hint = %q, want none", e.Title, got)
		}
	}
}
