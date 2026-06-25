// Package storages resolves a content-addressed hash to an on-disk path across
// the configured physical storages, in a fixed precedence (local before links).
//
// It is the read/serve-side counterpart to api/storage (which writes blobs for a
// single backend). The library is content-addressed: one logical files row per
// hash, but the bytes for that hash may physically exist in more than one
// storage. The resolver decides which copy to serve by probing storages in
// order and returning the first that actually has the hash.
//
// v0 ships exactly two storages, one each, so precedence is the constant
// [local, links] — there is no reorder or default control. The configurable,
// priority-ordered probe only earns its keep once a second interchangeable
// storage (S3) exists; see docs/architecture/{data-sources,s3-storage}.md.
package storages

import (
	"os"
	"path/filepath"
	"regexp"

	"daemonlord.ygg/madshare/api/storage"
)

// Storage backend identifiers. They double as files.storage_backend origin
// hints, but serving never depends on that column — the resolver probes.
const (
	Local = "local"
	Links = "links"
)

// validHash matches a lowercase SHA-256 hex digest (64 chars). Locate is a
// no-op for anything else so a malformed hash can never escape the hash dir.
var validHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Storage is one physical backend that may hold a content-addressed blob.
type Storage interface {
	// ID is the backend identifier (Local or Links).
	ID() string
	// Locate returns the on-disk path of the blob for hash and whether this
	// storage actually has it. A local storage hits on a regular file in the
	// hash dir; a links storage hits only when its symlink resolves (stats
	// through) to a regular file — a DANGLING link reports ok=false, so
	// resolution falls through to whatever storage does have the hash.
	Locate(hash string) (path string, ok bool)
}

// diskStorage backs both Local and Links: a hash dir holds the single blob,
// either as a regular file (local) or as a symlink to the external original
// (links). os.Stat follows symlinks, so the same probe serves both — a local
// regular file resolves to itself; a links symlink resolves to its target, and
// a dangling link errors out and is skipped.
type diskStorage struct {
	id   string
	root string // the storage root; the audio tree lives under <root>/audio
}

func (s *diskStorage) ID() string { return s.id }

func (s *diskStorage) Locate(hash string) (string, bool) {
	if !validHash.MatchString(hash) {
		return "", false
	}
	dir := filepath.Join(s.root, storage.AudioSubdir, hash)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		// os.Stat follows a symlink to its target; a dangling links entry
		// errors here and is skipped. Only a regular file (or a link to one)
		// counts as present.
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return p, true
		}
	}
	return "", false
}

// Registry is the fixed-precedence set of storages built at startup. In v0 it
// holds [local, links] in that order and is immutable after construction.
type Registry struct {
	ordered []Storage
	byID    map[string]Storage
}

// New builds the registry from the resolved data paths: local rooted at
// filesDir (its audio tree is filesDir/audio) and links rooted at linksDir
// (linksDir/audio). Both storages always exist; links is simply empty until a
// symlink source populates it. Precedence is fixed: local before links.
func New(filesDir, linksDir string) *Registry {
	local := &diskStorage{id: Local, root: filesDir}
	links := &diskStorage{id: Links, root: linksDir}
	return &Registry{
		ordered: []Storage{local, links},
		byID:    map[string]Storage{Local: local, Links: links},
	}
}

// Resolve returns the on-disk path to serve for hash and the id of the storage
// that holds it, trying storages in fixed precedence and returning the first
// hit. ok is false when no storage has the hash (including the case where only
// a dangling link exists). The returned path may be a symlink; the caller
// (http.ServeFile) follows it to the external original.
func (r *Registry) Resolve(hash string) (path, storageID string, ok bool) {
	for _, s := range r.ordered {
		if p, hit := s.Locate(hash); hit {
			return p, s.ID(), true
		}
	}
	return "", "", false
}

// Get returns the storage with the given id, or nil if there is none.
func (r *Registry) Get(id string) Storage { return r.byID[id] }

// Storages returns the storages in precedence order. The slice is the
// registry's own backing array; callers must not mutate it.
func (r *Registry) Storages() []Storage { return r.ordered }
