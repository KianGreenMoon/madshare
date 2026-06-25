package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

// validHash matches a lowercase SHA-256 hex digest (64 chars).
var validHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// AudioSubdir is the subdirectory of files_dir under which audio blobs live, so
// the served blob tree (/files) is a SIBLING of — not a parent of — the cover
// images tree. Nesting images under the served tree would expose them at
// /files/images/<key> too; keeping them out means /files can only ever reach
// audio. The store's baseDir is files_dir/<AudioSubdir>.
const AudioSubdir = "audio"

// ImagesSubdir is the subdirectory, under the variants dir, that holds owned
// cover-image variants (<variants_dir>/images/<base_key>/…, served at /images).
// It historically lived under files_dir; see RelocateImageVariants and
// docs/architecture/variants.md.
const ImagesSubdir = "images"

// RelocateImageVariants migrates the owned cover-image tree from its historical
// home under filesDir (filesDir/images) into the dedicated variants dir
// (variantsDir/images), so files_dir holds only source blobs and all derived
// media lives under variants_dir. It is the one-time upgrade for installs created
// before the split; mirrors RelocateLegacyBlobs.
//
// Idempotent and non-destructive: a missing source dir (fresh install or already
// migrated) is a no-op; an entry already present at the destination (a
// half-finished prior run) is left in place rather than clobbered. When the two
// paths resolve to the same directory (variants_dir == files_dir) it does
// nothing. Returns the number of top-level image entries moved.
func RelocateImageVariants(filesDir, variantsDir string) (int, error) {
	oldImages := filepath.Join(filesDir, ImagesSubdir)
	newImages := filepath.Join(variantsDir, ImagesSubdir)
	if filepath.Clean(oldImages) == filepath.Clean(newImages) {
		return 0, nil // variants_dir resolves to files_dir: nothing to relocate
	}
	entries, err := os.ReadDir(oldImages)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil // fresh install or already migrated
		}
		return 0, fmt.Errorf("storage: read images dir: %w", err)
	}
	moved := 0
	for _, e := range entries {
		if err := os.MkdirAll(newImages, 0755); err != nil {
			return moved, err
		}
		dst := filepath.Join(newImages, e.Name())
		if _, err := os.Stat(dst); err == nil {
			// Already at the destination (a half-finished prior migration);
			// leave the source copy rather than clobbering it.
			continue
		}
		if err := os.Rename(filepath.Join(oldImages, e.Name()), dst); err != nil {
			return moved, fmt.Errorf("storage: relocate image %s: %w", e.Name(), err)
		}
		moved++
	}
	// Best-effort tidy: drop the now-(hopefully-)empty old images dir. A
	// non-empty dir (entries left behind above) makes Remove fail, which we
	// deliberately ignore — the relocation itself already succeeded.
	_ = os.Remove(oldImages)
	return moved, nil
}

// RelocateLegacyBlobs migrates pre-AudioSubdir blobs — hash-named directories
// sitting directly under filesDir — into filesDir/<AudioSubdir>. It is the
// one-time upgrade for databases created before audio and images were split
// into sibling subtrees. Idempotent: once moved, no hash dirs remain at the top
// level, so a re-run is a no-op. Returns the number of blob directories moved.
// Cover images (filesDir/images) and any other non-hash entry are left in place.
func RelocateLegacyBlobs(filesDir string) (int, error) {
	entries, err := os.ReadDir(filesDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil // fresh install: nothing to migrate
		}
		return 0, fmt.Errorf("storage: read files dir: %w", err)
	}
	audioDir := filepath.Join(filesDir, AudioSubdir)
	moved := 0
	for _, e := range entries {
		if !e.IsDir() || !validHash.MatchString(e.Name()) {
			continue
		}
		if err := os.MkdirAll(audioDir, 0755); err != nil {
			return moved, err
		}
		dst := filepath.Join(audioDir, e.Name())
		if _, err := os.Stat(dst); err == nil {
			// Already at the destination (a half-finished prior migration);
			// leave the stray top-level copy rather than clobbering it.
			continue
		}
		if err := os.Rename(filepath.Join(filesDir, e.Name()), dst); err != nil {
			return moved, fmt.Errorf("storage: relocate blob %s: %w", e.Name(), err)
		}
		moved++
	}
	return moved, nil
}

// Local stores files at baseDir/<sha256>/<filename>.
type Local struct {
	baseDir string
}

// NewLocal creates a local filesystem storage backend.
func NewLocal(baseDir string) *Local {
	return &Local{baseDir: baseDir}
}

// Exists reports whether the blob at <hash>/<filename> already exists on disk.
func (s *Local) Exists(hash, filename string) (bool, error) {
	if !validHash.MatchString(hash) {
		return false, fmt.Errorf("storage: invalid hash %q", hash)
	}
	path := filepath.Join(s.baseDir, hash, filepath.Base(filename))
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// DeleteAll removes the entire <baseDir>/<hash> directory and everything under
// it. It is idempotent: a missing directory yields (false, nil).
func (s *Local) DeleteAll(hash string) (bool, error) {
	if !validHash.MatchString(hash) {
		return false, fmt.Errorf("storage: invalid hash %q", hash)
	}
	dir := filepath.Join(s.baseDir, hash)
	if _, err := os.Stat(dir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("storage: stat %s: %w", dir, err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, fmt.Errorf("storage: remove %s: %w", dir, err)
	}
	return true, nil
}

// BlobPresent reports whether <baseDir>/<hash> exists and contains at least one
// regular file. An emptied hash directory (the blob deleted but the directory
// left behind) reads as not present.
func (s *Local) BlobPresent(hash string) (bool, error) {
	if !validHash.MatchString(hash) {
		return false, fmt.Errorf("storage: invalid hash %q", hash)
	}
	entries, err := os.ReadDir(filepath.Join(s.baseDir, hash))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("storage: read hash dir: %w", err)
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			return true, nil
		}
	}
	return false, nil
}

// VerifyBlob re-reads the blob(s) under <baseDir>/<hash> and reports whether one
// hashes to hash. Returns false (no error) when the directory is missing/empty
// or the content has been corrupted (no file matches the expected digest).
func (s *Local) VerifyBlob(hash string) (bool, error) {
	if !validHash.MatchString(hash) {
		return false, fmt.Errorf("storage: invalid hash %q", hash)
	}
	dir := filepath.Join(s.baseDir, hash)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("storage: read hash dir: %w", err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		sum, err := sha256File(filepath.Join(dir, e.Name()))
		if err != nil {
			return false, err
		}
		if sum == hash {
			return true, nil
		}
	}
	return false, nil
}

// sha256File streams a file through SHA-256 and returns the hex digest.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("storage: open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("storage: hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// errNoStatfs is returned by diskUsage on platforms without a statfs syscall.
// Stats treats it as "capacity unknown" (HasVolume=false), not a failure.
var errNoStatfs = errors.New("storage: filesystem usage unsupported on this platform")

// Stats reports the capacity of the filesystem holding baseDir. The byte
// figures come from a statfs of baseDir (see diskUsage, which is build-tagged
// per OS); on a platform without statfs support it reports HasVolume=false
// rather than failing.
func (s *Local) Stats() (Stats, error) {
	st := Stats{Backend: "local", Location: s.baseDir}
	total, free, used, err := diskUsage(s.baseDir)
	if errors.Is(err, errNoStatfs) {
		return st, nil // HasVolume stays false
	}
	if err != nil {
		return st, fmt.Errorf("storage: disk usage for %s: %w", s.baseDir, err)
	}
	st.HasVolume = true
	st.TotalBytes, st.FreeBytes, st.UsedBytes = total, free, used
	return st, nil
}

// Put writes r to <baseDir>/<hash>/<filename>, creating the hash directory if
// needed. Path traversal in filename is neutralised by filepath.Base.
func (s *Local) Put(hash, filename string, r io.Reader) error {
	if !validHash.MatchString(hash) {
		return fmt.Errorf("storage: invalid hash %q", hash)
	}
	dir := filepath.Join(s.baseDir, hash)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	dst := filepath.Join(dir, filepath.Base(filename))
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}
