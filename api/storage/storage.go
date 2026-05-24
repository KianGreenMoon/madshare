package storage

import "io"

// Storage abstracts where uploaded files are persisted.
// Implement this interface for local disk, S3, or any other backend.
type Storage interface {
	// Stat returns the stored byte count for hash, or -1 if the hash is unknown.
	Stat(hash string) (int64, error)
	// Put stores the content of r under <hash>/<filename>.
	Put(hash, filename string, r io.Reader, size int64) error
}
