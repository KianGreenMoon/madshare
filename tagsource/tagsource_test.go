package tagsource

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

// blobOpener writes data to a temp file and returns a Subject-compatible opener.
func blobOpener(t *testing.T, data []byte) func() (io.ReadSeekCloser, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blob.mp3")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return func() (io.ReadSeekCloser, error) { return os.Open(path) }
}

// id3v1Block assembles a 128-byte ID3v1.1 trailer with pre-encoded field bytes.
func id3v1Block(title, artist, album []byte, track byte) []byte {
	buf := make([]byte, 128)
	copy(buf[0:3], "TAG")
	copy(buf[3:33], title)
	copy(buf[33:63], artist)
	copy(buf[63:93], album)
	copy(buf[93:97], "1988")
	buf[126] = track
	buf[127] = 255
	return buf
}

// id3v2Tag assembles a minimal ID3v2.3 header with ISO-8859-1 text frames —
// the mis-declared-charset case the v2 chip's override exists for.
func id3v2Tag(frames map[string][]byte) []byte {
	var body bytes.Buffer
	for id, text := range frames {
		payload := append([]byte{0x00}, text...) // encoding 0x00 = ISO-8859-1
		body.WriteString(id)
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(payload)))
		body.Write(size[:])
		body.Write([]byte{0, 0}) // flags
		body.Write(payload)
	}
	n := body.Len()
	head := []byte{'I', 'D', '3', 0x03, 0x00, 0x00,
		byte(n >> 21 & 0x7F), byte(n >> 14 & 0x7F), byte(n >> 7 & 0x7F), byte(n & 0x7F)}
	return append(head, body.Bytes()...)
}

func cp1251(t *testing.T, s string) []byte {
	t.Helper()
	b, err := charmap.Windows1251.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestID3v1Source_DetectsAndOverrides(t *testing.T) {
	data := append([]byte("not really audio"),
		id3v1Block(cp1251(t, "Группа крови"), cp1251(t, "Виктор Цой"), cp1251(t, "Кино"), 7)...)
	open := blobOpener(t, data)

	got := Local([]string{SourceID3v1}, Subject{OpenBlob: open})
	if len(got) != 1 || got[0].Err != "" {
		t.Fatalf("suggestions = %+v, want one clean id3v1 entry", got)
	}
	s := got[0]
	if s.Charset != "windows-1251" {
		t.Errorf("detected charset = %q, want windows-1251", s.Charset)
	}
	if s.Tags["title"] != "Группа крови" || s.Tags["artist"] != "Виктор Цой" || s.Tags["album"] != "Кино" {
		t.Errorf("tags = %v", s.Tags)
	}
	if s.Tags["year"] != 1988 || s.Tags["track_number"] != 7 {
		t.Errorf("year/track = %v/%v", s.Tags["year"], s.Tags["track_number"])
	}
	if s.Label != "ID3v1.1" || len(s.Charsets) == 0 {
		t.Errorf("label = %q charsets = %v", s.Label, s.Charsets)
	}

	// A manual override decodes with the requested charset instead.
	got = Local([]string{SourceID3v1}, Subject{OpenBlob: open, Charset: "koi8-r"})
	if got[0].Charset != "koi8-r" || got[0].Tags["album"] == "Кино" {
		t.Errorf("override: charset=%q album=%q, want koi8-r mojibake", got[0].Charset, got[0].Tags["album"])
	}
}

func TestID3v1Source_AbsentMeansNoChip(t *testing.T) {
	got := Local([]string{SourceID3v1}, Subject{OpenBlob: blobOpener(t, bytes.Repeat([]byte{0x11}, 400))})
	if len(got) != 0 {
		t.Fatalf("suggestions = %+v, want none without a v1 trailer", got)
	}
}

func TestID3v2Source_MisdeclaredLatin1Override(t *testing.T) {
	tag := id3v2Tag(map[string][]byte{
		"TIT2": cp1251(t, "Группа крови"),
		"TPE1": cp1251(t, "Виктор Цой"),
	})
	open := blobOpener(t, append(tag, []byte("audio-ish payload")...))

	// Default decode: dhowden/tag reads the declared ISO-8859-1 → mojibake.
	got := Local([]string{SourceID3v2}, Subject{OpenBlob: open})
	if len(got) != 1 || got[0].Err != "" {
		t.Fatalf("suggestions = %+v, want one clean id3v2 entry", got)
	}
	if got[0].Label != "ID3v2.3" || got[0].Tags["title"] == "Группа крови" {
		t.Errorf("default: label=%q title=%q, want ID3v2.3 mojibake", got[0].Label, got[0].Tags["title"])
	}
	if len(got[0].Charsets) == 0 {
		t.Error("ID3v2 chip must offer charset overrides")
	}

	// Charset override reinterprets the Latin-1-decoded frames as cp1251.
	got = Local([]string{SourceID3v2}, Subject{OpenBlob: open, Charset: "windows-1251"})
	if got[0].Tags["title"] != "Группа крови" || got[0].Tags["artist"] != "Виктор Цой" {
		t.Errorf("override tags = %v", got[0].Tags)
	}
}

func TestID3v2Source_SilentWithoutTags(t *testing.T) {
	got := Local([]string{SourceID3v2}, Subject{OpenBlob: blobOpener(t, bytes.Repeat([]byte{0x22}, 400))})
	if len(got) != 0 {
		t.Fatalf("suggestions = %+v, want none without v2/native tags", got)
	}
}

func TestLocal_BlobUnavailableIsErrorChip(t *testing.T) {
	open := func() (io.ReadSeekCloser, error) { return nil, os.ErrNotExist }
	got := Local(LocalSources(), Subject{OpenBlob: open})
	if len(got) != 2 || got[0].Err == "" || got[1].Err == "" {
		t.Fatalf("suggestions = %+v, want two error entries", got)
	}
}
