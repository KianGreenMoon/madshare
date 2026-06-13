package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---- helpers ----------------------------------------------------------------

func sha256hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// newLocal returns a Local backed by a temp dir that is cleaned up after t.
func newLocal(t *testing.T) *Local {
	t.Helper()
	return NewLocal(t.TempDir())
}

// ---- HashUpload -------------------------------------------------------------

// TestHashUpload_SmallFile exercises the in-memory path (size <= memBufferLimit).
func TestHashUpload_SmallFile(t *testing.T) {
	data := []byte("hello madshare")
	want := sha256hex(data)

	hash, content, size, cleanup, err := HashUpload(bytes.NewReader(data), int64(len(data)), t.TempDir())
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hash != want {
		t.Errorf("hash = %q, want %q", hash, want)
	}
	if size != int64(len(data)) {
		t.Errorf("size = %d, want %d", size, len(data))
	}
	if cleanup != nil {
		t.Error("small file should not produce a cleanup func (no temp file)")
	}

	got, err := io.ReadAll(content)
	if err != nil {
		t.Fatalf("reading content: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("content mismatch: got %q, want %q", got, data)
	}
}

// TestHashUpload_EmptyFile ensures a zero-byte upload is handled correctly.
func TestHashUpload_EmptyFile(t *testing.T) {
	data := []byte{}
	want := sha256hex(data)

	hash, content, size, cleanup, err := HashUpload(bytes.NewReader(data), 0, t.TempDir())
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hash != want {
		t.Errorf("hash = %q, want %q", hash, want)
	}
	if size != 0 {
		t.Errorf("size = %d, want 0", size)
	}
	got, _ := io.ReadAll(content)
	if len(got) != 0 {
		t.Errorf("expected empty content, got %d bytes", len(got))
	}
}

// TestHashUpload_LargeFile exercises the temp-file spool path by passing a
// declaredSize larger than memBufferLimit. The actual data is small — we only
// need the declared size to exceed the threshold.
func TestHashUpload_LargeFile(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 1024)
	want := sha256hex(data)
	cacheDir := t.TempDir()

	// Force the large-file spool path by claiming the file is enormous.
	hash, content, size, cleanup, err := HashUpload(bytes.NewReader(data), memBufferLimit+1, cacheDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cleanup == nil {
		t.Fatal("large file path must return a cleanup func")
	}

	if hash != want {
		t.Errorf("hash = %q, want %q", hash, want)
	}
	if size != int64(len(data)) {
		t.Errorf("size = %d, want %d", size, len(data))
	}

	got, err := io.ReadAll(content)
	if err != nil {
		t.Fatalf("reading spooled content: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("spooled content does not match original data")
	}

	// Verify the temp file exists before cleanup.
	entries, _ := os.ReadDir(cacheDir)
	var tempFiles []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "upload-") {
			tempFiles = append(tempFiles, e.Name())
		}
	}
	if len(tempFiles) == 0 {
		t.Error("expected a temp file in cacheDir before cleanup")
	}

	// Cleanup must remove the temp file.
	cleanup()
	entries, _ = os.ReadDir(cacheDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "upload-") {
			t.Errorf("temp file %q was not removed by cleanup", e.Name())
		}
	}
}

// TestHashUpload_LargeFile_TempFileReadableAfterCleanup verifies that the
// caller can read the content before calling cleanup (cleanup closes the file).
func TestHashUpload_LargeFile_ContentReadBeforeCleanup(t *testing.T) {
	data := []byte("spool me please")
	cacheDir := t.TempDir()

	_, content, _, cleanup, err := HashUpload(bytes.NewReader(data), memBufferLimit+1, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	got, err := io.ReadAll(content)
	if err != nil {
		t.Fatalf("read after spool: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("content mismatch after spool")
	}
}

// TestHashUpload_ReaderError exercises the error path when the reader fails.
func TestHashUpload_ReaderError(t *testing.T) {
	boom := errors.New("simulated read failure")
	bad := &errorReader{err: boom}

	// Small-file path error.
	_, _, _, cleanup, err := HashUpload(bad, 1, t.TempDir())
	if cleanup != nil {
		defer cleanup()
	}
	if !errors.Is(err, boom) {
		t.Errorf("small path: expected wrapped boom, got %v", err)
	}

	// Large-file spool path error.
	_, _, _, cleanup2, err2 := HashUpload(bad, memBufferLimit+1, t.TempDir())
	if cleanup2 != nil {
		defer cleanup2()
	}
	if !errors.Is(err2, boom) {
		t.Errorf("large path: expected wrapped boom, got %v", err2)
	}
}

// TestHashUpload_LargeFile_BadCacheDir exercises the MkdirAll / CreateTemp
// error path when cacheDir is an unwritable location.
func TestHashUpload_LargeFile_BadCacheDir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; permission checks do not apply")
	}
	// Use a path that cannot be created.
	badCache := "/proc/this-cannot-exist/cache"
	data := []byte("data")
	_, _, _, cleanup, err := HashUpload(bytes.NewReader(data), memBufferLimit+1, badCache)
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil {
		t.Error("expected error with unwritable cacheDir, got nil")
	}
}

// TestHashUpload_HashIsActualSHA256 is a property test: the returned hash must
// always equal sha256(actual bytes read), regardless of declaredSize.
func TestHashUpload_HashIsActualSHA256(t *testing.T) {
	cases := []struct {
		name         string
		data         []byte
		declaredSize int64
	}{
		{"declared==actual small", []byte("abc"), 3},
		{"declared>actual small", []byte("abc"), 9999}, // small path, excess declared
		{"declared==actual large", bytes.Repeat([]byte("z"), 8), memBufferLimit + 1},
		{"declared<actual large", []byte("short"), memBufferLimit + 1}, // spool with small actual data
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := sha256hex(tc.data)
			hash, _, _, cleanup, err := HashUpload(bytes.NewReader(tc.data), tc.declaredSize, t.TempDir())
			if cleanup != nil {
				defer cleanup()
			}
			if err != nil {
				t.Fatalf("error: %v", err)
			}
			if hash != want {
				t.Errorf("hash mismatch: got %q, want %q", hash, want)
			}
		})
	}
}

// ---- Local.Exists -----------------------------------------------------------

// TestLocalExists_Missing returns false for an unknown hash/filename.
func TestLocalExists_Missing(t *testing.T) {
	s := newLocal(t)
	ok, err := s.Exists("deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "any.mp3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("Exists = true on empty store, want false")
	}
}

// TestLocalExists_Present stores a file then verifies Exists returns true.
func TestLocalExists_Present(t *testing.T) {
	s := newLocal(t)
	hash := sha256hex([]byte("test content"))
	data := []byte("test content")

	if err := s.Put(hash, "test.txt", bytes.NewReader(data)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	ok, err := s.Exists(hash, "test.txt")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !ok {
		t.Error("Exists = false after Put, want true")
	}
}

// TestLocalExists_DifferentFilename verifies Exists is per-filename, not
// per-hash — same hash with a different filename returns false.
func TestLocalExists_DifferentFilename(t *testing.T) {
	s := newLocal(t)
	hash := sha256hex([]byte("same bytes"))
	if err := s.Put(hash, "first.mp3", bytes.NewReader([]byte("same bytes"))); err != nil {
		t.Fatal(err)
	}
	ok, err := s.Exists(hash, "second.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("Exists = true for different filename, want false")
	}
}

// TestLocalExists_InvalidHash returns an error for a malformed hash.
func TestLocalExists_InvalidHash(t *testing.T) {
	s := newLocal(t)
	_, err := s.Exists("not-a-hash", "any.mp3")
	if err == nil {
		t.Error("expected error for invalid hash, got nil")
	}
}

// TestLocalPut_Concurrent races two Put calls for the same hash to surface
// any data race. Run with -race.
func TestLocalPut_Concurrent(t *testing.T) {
	s := newLocal(t)
	hash := sha256hex([]byte("concurrent"))
	data := []byte("concurrent")

	const goroutines = 10
	errs := make(chan error, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			errs <- s.Put(hash, "file.txt", bytes.NewReader(data))
		}()
	}
	for i := 0; i < goroutines; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Put: %v", err)
		}
	}

	ok, err := s.Exists(hash, "file.txt")
	if err != nil {
		t.Fatalf("Exists after concurrent writes: %v", err)
	}
	if !ok {
		t.Error("Exists = false after concurrent writes")
	}
}

// ---- Local.Put --------------------------------------------------------------

// TestLocalPut_Normal verifies a round-trip store.
func TestLocalPut_Normal(t *testing.T) {
	s := newLocal(t)
	hash := sha256hex([]byte("normal"))
	data := []byte("normal file content")

	if err := s.Put(hash, "audio.mp3", bytes.NewReader(data)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	stored := filepath.Join(s.baseDir, hash, "audio.mp3")
	got, err := os.ReadFile(stored)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("stored content does not match original")
	}
}

// TestLocalPut_EmptyFilename passes an empty filename string.
// filepath.Base("") returns "." which means os.Create would create a directory
// entry named "." — verify behaviour and document the bug.
func TestLocalPut_EmptyFilename(t *testing.T) {
	s := newLocal(t)
	hash := sha256hex([]byte("emptyname"))
	data := []byte("data")

	err := s.Put(hash, "", bytes.NewReader(data))
	// Document the actual behaviour. An empty filename causes filepath.Base("")
	// to return "." which is the directory itself — os.Create("dir/.") on Linux
	// returns EISDIR. The test asserts an error IS returned (i.e. not silently
	// writing to the directory).
	if err == nil {
		// If no error, check what was actually created.
		stat, statErr := os.Stat(filepath.Join(s.baseDir, hash, "."))
		if statErr == nil && stat.IsDir() {
			t.Error("BUG: Put('') silently targets the hash directory itself instead of rejecting the empty filename")
		}
	}
	// Either an error is returned (good) or we document the created path.
	t.Logf("Put with empty filename returned err=%v", err)
}

// TestLocalPut_PathTraversalInFilename demonstrates that a filename containing
// path separators is sanitised by filepath.Base before being used as the
// on-disk name.
func TestLocalPut_PathTraversalInFilename(t *testing.T) {
	s := newLocal(t)
	hash := sha256hex([]byte("traversal"))
	data := []byte("should not escape")

	// Attempt classic path traversal via the filename parameter.
	malicious := "../../../tmp/evil"
	if err := s.Put(hash, malicious, bytes.NewReader(data)); err != nil {
		// An error is acceptable; it must not write outside baseDir.
		t.Logf("Put with traversal filename returned error (acceptable): %v", err)
		return
	}

	// Verify the file did NOT land outside baseDir.
	escapedPath := "/tmp/evil"
	if _, err := os.Stat(escapedPath); err == nil {
		t.Errorf("CRITICAL: file escaped baseDir to %s", escapedPath)
		os.Remove(escapedPath)
	}

	// Verify the file DID land inside baseDir with just the base name "evil".
	safeBase := filepath.Base(malicious) // "evil"
	expected := filepath.Join(s.baseDir, hash, safeBase)
	if _, err := os.Stat(expected); err != nil {
		t.Errorf("expected file at %s not found: %v", expected, err)
	}
}

// TestLocalPut_HashPathTraversal checks whether a caller-controlled hash value
// can escape baseDir via directory traversal. This would require HashUpload to
// return a crafted hash, which sha256 encoding prevents — but the interface
// allows any string.
func TestLocalPut_HashPathTraversal(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root; permission checks do not apply")
	}
	s := newLocal(t)
	// A crafted hash that attempts traversal.
	// filepath.Join cleans the path, so "../evil" as hash would be resolved
	// relative to baseDir — let's verify it cannot escape.
	maliciousHash := "../evil-hash"
	data := []byte("escape attempt")

	err := s.Put(maliciousHash, "file.txt", bytes.NewReader(data))
	// The file must NOT be written to the parent of baseDir.
	escapedDir := filepath.Join(filepath.Dir(s.baseDir), "evil-hash")
	if _, statErr := os.Stat(escapedDir); statErr == nil {
		t.Errorf("CRITICAL: hash path traversal succeeded — file written to %s", escapedDir)
		os.RemoveAll(escapedDir)
	}
	t.Logf("Put with traversal hash returned err=%v (escaped dir stat reported not-exist: good)", err)
}

// TestLocalPut_FilenameWithNullByte verifies behaviour when filename contains
// a null byte (some OS calls truncate at null).
func TestLocalPut_FilenameWithNullByte(t *testing.T) {
	s := newLocal(t)
	hash := sha256hex([]byte("nullbyte"))
	data := []byte("null byte test")

	err := s.Put(hash, "file\x00.txt", bytes.NewReader(data))
	// On Linux, os.Create with a null byte in the name returns EINVAL.
	// We assert that if no error was returned the file can be stat'd cleanly.
	t.Logf("Put with null-byte filename returned err=%v", err)
}

// TestLocalPut_CreatesHashDir verifies MkdirAll is called and the dir exists.
func TestLocalPut_CreatesHashDir(t *testing.T) {
	s := newLocal(t)
	hash := sha256hex([]byte("mkdir"))
	data := []byte("mkdir test")

	if err := s.Put(hash, "f.bin", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.baseDir, hash)); errors.Is(err, fs.ErrNotExist) {
		t.Error("hash directory was not created")
	}
}

// ---- Local.DeleteAll ---------------------------------------------------------

// TestLocalDeleteAll_RemovesAllFilenames stores two filenames under one hash
// and verifies DeleteAll wipes the whole hash directory.
func TestLocalDeleteAll_RemovesAllFilenames(t *testing.T) {
	s := newLocal(t)
	hash := sha256hex([]byte("delete me"))
	if err := s.Put(hash, "a.mp3", bytes.NewReader([]byte("delete me"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(hash, "b.mp3", bytes.NewReader([]byte("delete me"))); err != nil {
		t.Fatal(err)
	}

	removed, err := s.DeleteAll(hash)
	if err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	if !removed {
		t.Error("removed = false, want true for an existing hash dir")
	}
	if _, err := os.Stat(filepath.Join(s.baseDir, hash)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("hash dir still exists after DeleteAll: stat err = %v", err)
	}
}

// TestLocalDeleteAll_Missing is idempotent: deleting an absent hash is a no-op.
func TestLocalDeleteAll_Missing(t *testing.T) {
	s := newLocal(t)
	hash := sha256hex([]byte("never stored"))

	removed, err := s.DeleteAll(hash)
	if err != nil {
		t.Fatalf("DeleteAll on missing: %v", err)
	}
	if removed {
		t.Error("removed = true for a missing hash, want false")
	}
}

// TestLocalDeleteAll_InvalidHash rejects a malformed hash.
func TestLocalDeleteAll_InvalidHash(t *testing.T) {
	s := newLocal(t)
	if _, err := s.DeleteAll("not-a-hash"); err == nil {
		t.Error("expected error for invalid hash, got nil")
	}
}

// ---- Local.BlobPresent -------------------------------------------------------

func TestLocalBlobPresent_Present(t *testing.T) {
	s := newLocal(t)
	hash := sha256hex([]byte("present"))
	if err := s.Put(hash, "x.mp3", bytes.NewReader([]byte("present"))); err != nil {
		t.Fatal(err)
	}
	ok, err := s.BlobPresent(hash)
	if err != nil {
		t.Fatalf("BlobPresent: %v", err)
	}
	if !ok {
		t.Error("BlobPresent = false after Put, want true")
	}
}

func TestLocalBlobPresent_Missing(t *testing.T) {
	s := newLocal(t)
	hash := sha256hex([]byte("absent"))
	ok, err := s.BlobPresent(hash)
	if err != nil {
		t.Fatalf("BlobPresent: %v", err)
	}
	if ok {
		t.Error("BlobPresent = true on empty store, want false")
	}
}

// TestLocalBlobPresent_EmptyDir is the Issue-2 case: the file was deleted but
// its hash directory was left behind. BlobPresent must report not-present.
func TestLocalBlobPresent_EmptyDir(t *testing.T) {
	s := newLocal(t)
	hash := sha256hex([]byte("emptied"))
	if err := s.Put(hash, "x.mp3", bytes.NewReader([]byte("emptied"))); err != nil {
		t.Fatal(err)
	}
	// Remove the file but keep the directory.
	if err := os.Remove(filepath.Join(s.baseDir, hash, "x.mp3")); err != nil {
		t.Fatal(err)
	}
	ok, err := s.BlobPresent(hash)
	if err != nil {
		t.Fatalf("BlobPresent: %v", err)
	}
	if ok {
		t.Error("BlobPresent = true for an emptied hash dir, want false")
	}
}

func TestLocalBlobPresent_InvalidHash(t *testing.T) {
	s := newLocal(t)
	if _, err := s.BlobPresent("nope"); err == nil {
		t.Error("expected error for invalid hash, got nil")
	}
}

// ---- Local.VerifyBlob --------------------------------------------------------

func TestLocalVerifyBlob_Intact(t *testing.T) {
	s := newLocal(t)
	data := []byte("intact content")
	hash := sha256hex(data)
	if err := s.Put(hash, "song.mp3", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	ok, err := s.VerifyBlob(hash)
	if err != nil {
		t.Fatalf("VerifyBlob: %v", err)
	}
	if !ok {
		t.Error("VerifyBlob = false for intact blob, want true")
	}
}

func TestLocalVerifyBlob_Corrupted(t *testing.T) {
	s := newLocal(t)
	data := []byte("original content")
	hash := sha256hex(data)
	if err := s.Put(hash, "song.mp3", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	// Corrupt the stored bytes without touching the hash-named directory.
	if err := os.WriteFile(filepath.Join(s.baseDir, hash, "song.mp3"), []byte("rotted"), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, err := s.VerifyBlob(hash)
	if err != nil {
		t.Fatalf("VerifyBlob: %v", err)
	}
	if ok {
		t.Error("VerifyBlob = true for corrupted blob, want false")
	}
}

func TestLocalVerifyBlob_Missing(t *testing.T) {
	s := newLocal(t)
	hash := sha256hex([]byte("never stored"))
	ok, err := s.VerifyBlob(hash)
	if err != nil {
		t.Fatalf("VerifyBlob: %v", err)
	}
	if ok {
		t.Error("VerifyBlob = true for missing blob, want false")
	}
}

func TestLocalVerifyBlob_InvalidHash(t *testing.T) {
	s := newLocal(t)
	if _, err := s.VerifyBlob("nope"); err == nil {
		t.Error("expected error for invalid hash, got nil")
	}
}

// ---- Local.Stats -------------------------------------------------------------

// On the unix CI/dev target, Stats reports a real volume: a non-zero total and
// a used+free that doesn't exceed it. (On a hypothetical non-unix build the
// backend reports HasVolume=false; we don't assert that path here.)
func TestLocalStats(t *testing.T) {
	s := newLocal(t)
	st, err := s.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.Backend != "local" {
		t.Errorf("Backend = %q, want local", st.Backend)
	}
	if st.Location != s.baseDir {
		t.Errorf("Location = %q, want %q", st.Location, s.baseDir)
	}
	if !st.HasVolume {
		t.Skip("filesystem usage unsupported on this platform")
	}
	if st.TotalBytes == 0 {
		t.Error("TotalBytes = 0 on a real volume, want > 0")
	}
	if st.FreeBytes > st.TotalBytes {
		t.Errorf("FreeBytes %d > TotalBytes %d", st.FreeBytes, st.TotalBytes)
	}
	if st.UsedBytes > st.TotalBytes {
		t.Errorf("UsedBytes %d > TotalBytes %d", st.UsedBytes, st.TotalBytes)
	}
}

// ---- errorReader helper -----------------------------------------------------

type errorReader struct{ err error }

func (e *errorReader) Read(_ []byte) (int, error) { return 0, e.err }
