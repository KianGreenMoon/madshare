package storage

import "io"

// Storage abstracts where uploaded files are persisted.
// Implement this interface for local disk, S3, or any other backend.
type Storage interface {
	// Put stores the content of r under <hash>/<filename>.
	Put(hash, filename string, r io.Reader) error

	// Exists reports whether <hash>/<filename> is already stored.
	Exists(hash, filename string) (bool, error)
}
