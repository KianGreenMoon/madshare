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
}
