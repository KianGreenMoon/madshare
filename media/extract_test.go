package media

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// buildID3v23 hand-builds a minimal ID3v2.3 tag with the given text frames.
// frames maps frame ID (4 chars, e.g. "TIT2") to UTF-8 text content. Text is
// stored as ISO-8859-1 (encoding byte 0) so ASCII payloads work as-is.
//
// The result is a valid ID3v2.3 tag block — no audio frames follow. dhowden's
// parser reads the tag from the head of the stream without needing audio.
func buildID3v23(frames map[string]string) []byte {
	var body bytes.Buffer
	for id, text := range frames {
		// Frame header: 4-byte ID, 4-byte big-endian size, 2-byte flags.
		// Frame body: 1-byte text encoding (0x00 = ISO-8859-1) + text.
		frameBody := append([]byte{0x00}, []byte(text)...)
		body.WriteString(id)
		var sz [4]byte
		binary.BigEndian.PutUint32(sz[:], uint32(len(frameBody)))
		body.Write(sz[:])
		body.Write([]byte{0x00, 0x00}) // flags
		body.Write(frameBody)
	}

	// ID3v2 header: "ID3" + v2.3.0 + flags=0 + 4-byte syncsafe size.
	tagSize := body.Len()
	header := []byte{
		'I', 'D', '3',
		0x03, 0x00, // version 2.3.0
		0x00, // flags
		byte((tagSize >> 21) & 0x7f),
		byte((tagSize >> 14) & 0x7f),
		byte((tagSize >> 7) & 0x7f),
		byte(tagSize & 0x7f),
	}

	out := make([]byte, 0, len(header)+body.Len())
	out = append(out, header...)
	out = append(out, body.Bytes()...)
	return out
}

// writeFixture writes data to a temp file and returns its path.
// We synthesize the ID3 tag bytes in-test rather than committing a binary
// fixture so the test is fully self-contained.
func writeFixture(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractTags_ReadsID3v23Fixture(t *testing.T) {
	data := buildID3v23(map[string]string{
		"TIT2": "Test Title",
		"TPE1": "Test Artist",
		"TALB": "Test Album",
		"TYER": "2024",
	})
	path := writeFixture(t, "id3v23.mp3", data)

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()

	tags, err := ExtractTags(f, "audio/mpeg")
	if err != nil {
		t.Fatalf("ExtractTags: %v", err)
	}
	if tags == nil {
		t.Fatal("nil tags returned")
	}
	if tags.Title != "Test Title" {
		t.Errorf("Title = %q, want %q", tags.Title, "Test Title")
	}
	if tags.Artist != "Test Artist" {
		t.Errorf("Artist = %q, want %q", tags.Artist, "Test Artist")
	}
	if tags.Album != "Test Album" {
		t.Errorf("Album = %q, want %q", tags.Album, "Test Album")
	}
	if tags.Year != 2024 {
		t.Errorf("Year = %d, want 2024", tags.Year)
	}
	if tags.TagFormat != "ID3v2.3" {
		t.Errorf("TagFormat = %q, want %q", tags.TagFormat, "ID3v2.3")
	}
}

func TestExtractTags_UntaggedReturnsEmpty(t *testing.T) {
	// Bytes that match no known tag format. The input must be at least
	// 128 bytes so the ID3v1 trailing-128 probe doesn't error on a short
	// read — when probing fails cleanly we expect ErrNoTagsFound, which
	// ExtractTags maps to (&Tags{}, nil).
	data := bytes.Repeat([]byte{0x00}, 256)
	copy(data, "PADX") // four leading bytes that don't match any magic
	r := bytes.NewReader(data)

	tags, err := ExtractTags(r, "audio/mpeg")
	if err != nil {
		t.Fatalf("untagged: unexpected error %v", err)
	}
	if tags == nil {
		t.Fatal("nil tags for untagged input")
	}
	if (*tags != Tags{}) {
		t.Errorf("expected zero Tags, got %+v", tags)
	}
}

func TestExtractTags_MIMETypeIsInformational(t *testing.T) {
	// Build an ID3v2 fixture but tell ExtractTags it's audio/flac — the
	// content is what matters, not the declared MIME type.
	data := buildID3v23(map[string]string{"TIT2": "Misdeclared"})
	r := bytes.NewReader(data)

	tags, err := ExtractTags(r, "audio/flac")
	if err != nil {
		t.Fatalf("ExtractTags: %v", err)
	}
	if tags.Title != "Misdeclared" {
		t.Errorf("Title = %q, want %q", tags.Title, "Misdeclared")
	}
}

// buildFLAC hand-builds a FLAC stream whose only metadata block is the Vorbis
// comment. It is not playable audio and does not need to be: the tag reader
// reads the block chain from the head of the file and stops at the last block,
// which is the whole of what is under test.
func buildFLAC(comments map[string]string) []byte {
	var payload bytes.Buffer
	vendor := "madshare-test"
	_ = binary.Write(&payload, binary.LittleEndian, uint32(len(vendor)))
	payload.WriteString(vendor)
	_ = binary.Write(&payload, binary.LittleEndian, uint32(len(comments)))
	for k, v := range comments {
		c := k + "=" + v
		_ = binary.Write(&payload, binary.LittleEndian, uint32(len(c)))
		payload.WriteString(c)
	}

	out := []byte("fLaC")
	// Block header: last-block flag (0x80) | block type 4 (VORBIS_COMMENT),
	// then a 3-byte big-endian length.
	out = append(out, 0x80|0x04,
		byte(payload.Len()>>16), byte(payload.Len()>>8), byte(payload.Len()))
	out = append(out, payload.Bytes()...)
	return append(out, bytes.Repeat([]byte{0}, 256)...)
}

// The album-artist field, in every spelling a tagger writes it.
//
// Vorbis comments have no standard for it, so two spellings are both common in
// the wild — Picard writes ALBUMARTIST, MP3Tag and foobar2000 write ALBUM ARTIST
// — and a reader that knows only one of them reports a well-tagged album as
// having no artist at all. Downstream that is not a missing field: the entity
// resolver files the whole record under "Unknown artist", which is what a person
// sees and reports as their album disappearing.
func TestExtractTags_AlbumArtistSpellings(t *testing.T) {
	for _, key := range []string{"ALBUMARTIST", "ALBUM ARTIST", "albumartist", "Album Artist", "ALBUM_ARTIST"} {
		t.Run(key, func(t *testing.T) {
			data := buildFLAC(map[string]string{
				"TITLE": "Sweet Unrest",
				"ALBUM": "The Devil's Walk",
				key:     "Apparat",
			})
			tags, err := ExtractTags(bytes.NewReader(data), "audio/flac")
			if err != nil {
				t.Fatalf("ExtractTags: %v", err)
			}
			if tags.AlbumArtist != "Apparat" {
				t.Errorf("AlbumArtist = %q for a %q comment, want Apparat", tags.AlbumArtist, key)
			}
			if tags.Album != "The Devil's Walk" || tags.Title != "Sweet Unrest" {
				t.Errorf("the rest of the tags did not survive: %+v", tags)
			}
		})
	}
}

// The same for ID3v2: TPE2 is the frame, and it is read whether or not the file
// carries a TPE1 beside it.
func TestExtractTags_AlbumArtistWithoutAnArtistFrame(t *testing.T) {
	data := buildID3v23(map[string]string{
		"TIT2": "Sweet Unrest",
		"TPE2": "Apparat",
		"TALB": "The Devil's Walk",
	})
	tags, err := ExtractTags(bytes.NewReader(data), "audio/mpeg")
	if err != nil {
		t.Fatalf("ExtractTags: %v", err)
	}
	if tags.AlbumArtist != "Apparat" {
		t.Errorf("AlbumArtist = %q, want Apparat", tags.AlbumArtist)
	}
	if tags.Artist != "" {
		t.Errorf("Artist = %q, want empty — the file carries no TPE1", tags.Artist)
	}
}
