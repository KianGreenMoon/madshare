package media

import (
	"bytes"
	"errors"
	"io"
)

// ID3v1 support for the tag-suggestion sources (docs/architecture/
// tag-suggestions.md). dhowden/tag prefers ID3v2 and cannot expose v1 and v2
// side by side, so the v1 suggestion source reads the trailing 128-byte block
// itself. Fields come back as raw bytes: ID3v1 has no charset, so decoding is
// the charset layer's job (DetectCharset / DecodeWith).

// ErrNoID3v1 reports that the stream carries no ID3v1 trailer (or is too short
// to hold one).
var ErrNoID3v1 = errors.New("no ID3v1 tag")

// RawID3v1 is one parsed-but-undecoded ID3v1 trailer. Text fields are trimmed
// of their NUL/space padding but otherwise untouched.
type RawID3v1 struct {
	Title   []byte
	Artist  []byte
	Album   []byte
	Comment []byte
	Year    []byte
	Track   int    // v1.1 track number; 0 = unset (plain v1.0)
	Genre   string // resolved from the genre index; "" when 255/unknown
}

// ReadID3v1 parses the ID3v1 trailer (the last 128 bytes, magic "TAG") from r.
// Returns ErrNoID3v1 when the trailer is absent.
func ReadID3v1(r io.ReadSeeker) (*RawID3v1, error) {
	if _, err := r.Seek(-128, io.SeekEnd); err != nil {
		return nil, ErrNoID3v1
	}
	var buf [128]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return nil, ErrNoID3v1
	}
	if string(buf[0:3]) != "TAG" {
		return nil, ErrNoID3v1
	}
	t := &RawID3v1{
		Title:  trimID3v1(buf[3:33]),
		Artist: trimID3v1(buf[33:63]),
		Album:  trimID3v1(buf[63:93]),
		Year:   trimID3v1(buf[93:97]),
	}
	// ID3v1.1: a NUL at comment[28] marks byte 29 as the track number.
	comment := buf[97:127]
	if comment[28] == 0 && comment[29] != 0 {
		t.Track = int(comment[29])
		comment = comment[:28]
	}
	t.Comment = trimID3v1(comment)
	if g := int(buf[127]); g < len(id3v1Genres) {
		t.Genre = id3v1Genres[g]
	}
	return t, nil
}

// trimID3v1 strips a field's padding: writers pad with NULs or spaces, and
// garbage may follow a terminating NUL, so cut at the first NUL then trim
// trailing spaces.
func trimID3v1(b []byte) []byte {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	out := make([]byte, len(b))
	copy(out, b)
	return bytes.TrimRight(out, " ")
}

// id3v1Genres is the standard ID3v1 genre table (indexes 0–79). The rarer
// Winamp extensions (80+) resolve to "".
var id3v1Genres = [...]string{
	"Blues", "Classic Rock", "Country", "Dance", "Disco", "Funk", "Grunge",
	"Hip-Hop", "Jazz", "Metal", "New Age", "Oldies", "Other", "Pop", "R&B",
	"Rap", "Reggae", "Rock", "Techno", "Industrial", "Alternative", "Ska",
	"Death Metal", "Pranks", "Soundtrack", "Euro-Techno", "Ambient",
	"Trip-Hop", "Vocal", "Jazz+Funk", "Fusion", "Trance", "Classical",
	"Instrumental", "Acid", "House", "Game", "Sound Clip", "Gospel", "Noise",
	"AlternRock", "Bass", "Soul", "Punk", "Space", "Meditative",
	"Instrumental Pop", "Instrumental Rock", "Ethnic", "Gothic", "Darkwave",
	"Techno-Industrial", "Electronic", "Pop-Folk", "Eurodance", "Dream",
	"Southern Rock", "Comedy", "Cult", "Gangsta", "Top 40", "Christian Rap",
	"Pop/Funk", "Jungle", "Native American", "Cabaret", "New Wave",
	"Psychadelic", "Rave", "Showtunes", "Trailer", "Lo-Fi", "Tribal",
	"Acid Punk", "Acid Jazz", "Polka", "Retro", "Musical", "Rock & Roll",
	"Hard Rock",
}
