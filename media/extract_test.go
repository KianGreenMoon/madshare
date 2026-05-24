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
