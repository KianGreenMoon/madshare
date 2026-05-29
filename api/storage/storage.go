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

	// HashDirExists reports whether the <hash> directory exists, regardless of
	// its contents. Used to detect blobs that back a database record.
	HashDirExists(hash string) (bool, error)
}
