package database

import "context"

// Repository is the persistence boundary the upload handler depends on.
// Implementations must be safe for concurrent use.
type Repository interface {
	// GetFileByHash returns the file row for hash, or (nil, nil) on miss.
	GetFileByHash(ctx context.Context, hash string) (*File, error)

	// InsertFile creates a files row plus its initial file_uploads and
	// media_metadata rows in a single transaction. On success, f.ID is
	// populated with the new row id.
	InsertFile(ctx context.Context, f *File, upload *FileUpload, meta *MediaMetadata) error

	// RecordUpload inserts a new file_uploads row for an existing file.
	// A duplicate (file_id, filename) tuple is silently ignored.
	RecordUpload(ctx context.Context, fileID int64, filename string) error
}
