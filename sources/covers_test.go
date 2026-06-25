package sources_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"daemonlord.ygg/madshare/media"
)

// buildID3v23 hand-builds a minimal ID3v2.3 tag with the given text frames and an
// optional embedded cover (APIC). frames maps a 4-char frame ID (e.g. "TALB") to
// ISO-8859-1 text. When cover is non-nil an APIC frame with coverMIME is added.
// dhowden's parser reads the tag from the head of the stream without audio frames.
func buildID3v23(frames map[string]string, coverMIME string, cover []byte) []byte {
	var body bytes.Buffer
	writeFrame := func(id string, frameBody []byte) {
		body.WriteString(id)
		var sz [4]byte
		binary.BigEndian.PutUint32(sz[:], uint32(len(frameBody)))
		body.Write(sz[:])
		body.Write([]byte{0x00, 0x00}) // flags
		body.Write(frameBody)
	}
	for id, text := range frames {
		writeFrame(id, append([]byte{0x00}, []byte(text)...)) // 0x00 = ISO-8859-1
	}
	if cover != nil {
		var apic bytes.Buffer
		apic.WriteByte(0x00)        // text encoding ISO-8859-1
		apic.WriteString(coverMIME) // MIME type...
		apic.WriteByte(0x00)        // ...null-terminated
		apic.WriteByte(0x03)        // picture type = front cover
		apic.WriteByte(0x00)        // empty description + terminator
		apic.Write(cover)           // picture data
		writeFrame("APIC", apic.Bytes())
	}

	tagSize := body.Len()
	header := []byte{
		'I', 'D', '3', 0x03, 0x00, 0x00,
		byte((tagSize >> 21) & 0x7f), byte((tagSize >> 14) & 0x7f),
		byte((tagSize >> 7) & 0x7f), byte(tagSize & 0x7f),
	}
	return append(header, body.Bytes()...)
}

func TestScan_EmbeddedCover(t *testing.T) {
	root := t.TempDir()
	cover := []byte("\xff\xd8\xff-fake-jpeg-bytes-not-decoded-by-scan")
	mp3 := buildID3v23(map[string]string{"TIT2": "Song", "TPE1": "Artist Y", "TALB": "Album X"}, "image/jpeg", cover)
	if err := os.WriteFile(filepath.Join(root, "song.mp3"), mp3, 0o644); err != nil {
		t.Fatal(err)
	}

	store := newFakeStore()
	srcImages := t.TempDir()
	m := newManager(t, store, root).WithCovers(srcImages, nil)

	if _, err := m.Add(context.Background(), "x", root, sql.NullInt64{}); err != nil {
		t.Fatal(err)
	}
	m.Wait()

	if store.coverClaims != 1 || store.imageJobs != 1 {
		t.Fatalf("coverClaims=%d imageJobs=%d, want 1/1", store.coverClaims, store.imageJobs)
	}
	// The decoded source original is written into the owned tree as original.jpg.
	want := filepath.Join(srcImages, media.ImageHash(cover), "original.jpg")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected cover source original at %s: %v", want, err)
	}
}

// A sidecar cover image in the file's directory is preferred over embedded art.
func TestScan_SidecarCoverPreferred(t *testing.T) {
	root := t.TempDir()
	embedded := []byte("embedded-jpeg-bytes")
	mp3 := buildID3v23(map[string]string{"TPE1": "A", "TALB": "B"}, "image/jpeg", embedded)
	if err := os.WriteFile(filepath.Join(root, "t.mp3"), mp3, 0o644); err != nil {
		t.Fatal(err)
	}
	sidecar := []byte("sidecar-png-bytes")
	if err := os.WriteFile(filepath.Join(root, "cover.png"), sidecar, 0o644); err != nil {
		t.Fatal(err)
	}

	store := newFakeStore()
	srcImages := t.TempDir()
	m := newManager(t, store, root).WithCovers(srcImages, nil)
	if _, err := m.Add(context.Background(), "x", root, sql.NullInt64{}); err != nil {
		t.Fatal(err)
	}
	m.Wait()

	if store.coverClaims != 1 {
		t.Fatalf("coverClaims = %d, want 1", store.coverClaims)
	}
	// The SIDECAR png original is written (not the embedded jpeg).
	wantSidecar := filepath.Join(srcImages, media.ImageHash(sidecar), "original.png")
	if _, err := os.Stat(wantSidecar); err != nil {
		t.Errorf("sidecar cover should win; expected %s: %v", wantSidecar, err)
	}
	if _, err := os.Stat(filepath.Join(srcImages, media.ImageHash(embedded), "original.jpg")); err == nil {
		t.Error("embedded cover was written even though a sidecar exists")
	}
}

// With WithCovers not called, cover extraction is disabled (P3 behaviour).
func TestScan_CoversDisabled(t *testing.T) {
	root := t.TempDir()
	mp3 := buildID3v23(map[string]string{"TPE1": "A", "TALB": "B"}, "image/jpeg", []byte("cover"))
	if err := os.WriteFile(filepath.Join(root, "t.mp3"), mp3, 0o644); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	m := newManager(t, store, root) // no WithCovers
	if _, err := m.Add(context.Background(), "x", root, sql.NullInt64{}); err != nil {
		t.Fatal(err)
	}
	m.Wait()
	if store.coverClaims != 0 || store.imageJobs != 0 {
		t.Errorf("covers disabled: claims=%d jobs=%d, want 0/0", store.coverClaims, store.imageJobs)
	}
}
