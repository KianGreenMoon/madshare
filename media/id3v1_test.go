package media

import (
	"bytes"
	"errors"
	"testing"
)

// buildID3v1 assembles a 128-byte ID3v1 trailer. track 0 = plain v1.0 comment.
func buildID3v1(title, artist, album, year, comment string, track, genre byte) []byte {
	buf := make([]byte, 128)
	copy(buf[0:3], "TAG")
	copy(buf[3:33], title)
	copy(buf[33:63], artist)
	copy(buf[63:93], album)
	copy(buf[93:97], year)
	copy(buf[97:127], comment)
	if track > 0 {
		buf[125] = 0
		buf[126] = track
	}
	buf[127] = genre
	return buf
}

func TestReadID3v1_V11(t *testing.T) {
	data := append([]byte("junk audio bytes"),
		buildID3v1("Title A", "Artist B", "Album C", "1999", "hi", 7, 17)...)
	got, err := ReadID3v1(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadID3v1: %v", err)
	}
	if string(got.Title) != "Title A" || string(got.Artist) != "Artist B" ||
		string(got.Album) != "Album C" || string(got.Year) != "1999" {
		t.Errorf("fields = %q %q %q %q", got.Title, got.Artist, got.Album, got.Year)
	}
	if string(got.Comment) != "hi" {
		t.Errorf("comment = %q, want %q", got.Comment, "hi")
	}
	if got.Track != 7 {
		t.Errorf("track = %d, want 7", got.Track)
	}
	if got.Genre != "Rock" {
		t.Errorf("genre = %q, want Rock", got.Genre)
	}
}

func TestReadID3v1_V10NoTrackUnknownGenre(t *testing.T) {
	data := buildID3v1("T", "A", "B", "2001", "a 29-char comment ends here!x", 0, 255)
	got, err := ReadID3v1(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadID3v1: %v", err)
	}
	if got.Track != 0 {
		t.Errorf("track = %d, want 0 (v1.0)", got.Track)
	}
	if got.Genre != "" {
		t.Errorf("genre = %q, want empty for index 255", got.Genre)
	}
}

func TestReadID3v1_PaddingTrimmed(t *testing.T) {
	raw := buildID3v1("Song", "", "", "", "", 0, 12)
	// Space padding plus post-NUL garbage must both be stripped.
	copy(raw[3:], "Spaced   ")
	copy(raw[33:], "Artist\x00garbage-after-nul")
	got, err := ReadID3v1(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("ReadID3v1: %v", err)
	}
	if string(got.Title) != "Spaced" {
		t.Errorf("title = %q, want %q", got.Title, "Spaced")
	}
	if string(got.Artist) != "Artist" {
		t.Errorf("artist = %q, want %q", got.Artist, "Artist")
	}
}

func TestReadID3v1_Absent(t *testing.T) {
	for _, data := range [][]byte{
		[]byte("short"),
		bytes.Repeat([]byte{0x55}, 300), // long enough, no TAG magic
	} {
		if _, err := ReadID3v1(bytes.NewReader(data)); !errors.Is(err, ErrNoID3v1) {
			t.Errorf("len=%d: err = %v, want ErrNoID3v1", len(data), err)
		}
	}
}
