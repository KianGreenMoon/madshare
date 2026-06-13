package storage

import "io"

// Storage abstracts where uploaded files are persisted.
// Implement this interface for local disk, S3, or any other backend.
type Storage interface {
	// Put stores the content of r under <hash>/<filename>.
	Put(hash, filename string, r io.Reader) error

	// Exists reports whether <hash>/<filename> is already stored.
	Exists(hash, filename string) (bool, error)

	// DeleteAll removes every stored copy of the content addressed by hash
	// (the whole <hash> directory and all filenames under it). It is
	// idempotent: removed is false with a nil error when nothing was stored.
	DeleteAll(hash string) (removed bool, err error)

	// BlobPresent reports whether a backing blob exists for hash: the <hash>
	// directory exists AND holds at least one regular file. An emptied hash
	// directory (file deleted, directory left behind) reads as not present, so
	// the prune scan catches it. This is the cheap existence check.
	BlobPresent(hash string) (bool, error)

	// VerifyBlob is the deep integrity check: it reads the blob(s) under <hash>
	// and reports whether one hashes to hash. It returns false (no error) when
	// the blob is missing or its content has been corrupted (digest mismatch).
	// Expensive — it reads file content — so callers gate it behind an opt-in.
	VerifyBlob(hash string) (bool, error)

	// Stats reports the backend's capacity and usage for the admin panel. A
	// local-disk backend fills the byte fields from the filesystem holding its
	// base directory; an object store (future S3) has no fixed capacity and
	// reports HasVolume=false (the app's own footprint is then the meaningful
	// figure, computed by the caller from the database).
	Stats() (Stats, error)
}

// Stats describes a storage backend's capacity. It is deliberately backend-
// neutral so the future S3 implementation reports the same shape: a disk has a
// bounded volume; an object store sets HasVolume=false and leaves the byte
// fields zero.
type Stats struct {
	// Backend identifies the implementation: "local" today, "s3" later.
	Backend string
	// Location is the base directory (local) or bucket (object store).
	Location string
	// HasVolume is true when TotalBytes/FreeBytes/UsedBytes reflect a real,
	// fixed-capacity filesystem. Object stores set it false.
	HasVolume bool
	// TotalBytes is the filesystem capacity. Valid only when HasVolume.
	TotalBytes uint64
	// FreeBytes is the space available to an unprivileged user (df "Avail").
	// Valid only when HasVolume.
	FreeBytes uint64
	// UsedBytes is df-style usage: capacity minus all free blocks (including
	// the root-reserved ones), so it matches what `df` prints. Valid only when
	// HasVolume.
	UsedBytes uint64
}
