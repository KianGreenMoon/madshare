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

	// ListFiles returns all files ordered by upload time descending,
	// joined with their first filename and media metadata.
	ListFiles(ctx context.Context) ([]*FileListEntry, error)

	// ListArtists returns one entry per effective artist name, ordered
	// alphabetically. album_artist is preferred over artist for grouping.
	ListArtists(ctx context.Context) ([]*ArtistEntry, error)

	// ListAlbumsByArtist returns albums for the given artist. Pass an
	// empty string to return albums across all artists. Tracks with no
	// album are grouped under Title="".
	ListAlbumsByArtist(ctx context.Context, artist string) ([]*AlbumEntry, error)

	// ListTracksByAlbumArtist returns tracks for the given artist+album
	// combination. album="" selects the Other bucket (no album tag).
	ListTracksByAlbumArtist(ctx context.Context, artist, album string) ([]*TrackEntry, error)

	// UpsertArtistImage stores (or replaces) the image reference for an artist.
	UpsertArtistImage(ctx context.Context, artist, objectKey, mimeType string, updatedAt int64) error

	// UpsertAlbumImage stores (or replaces) the image reference for an album.
	UpsertAlbumImage(ctx context.Context, artist, album, objectKey, mimeType string, updatedAt int64) error

	// GetArtistImage returns the image object key and MIME type for an artist.
	// Returns found=false (no error) when no image is stored.
	GetArtistImage(ctx context.Context, artist string) (objectKey, mimeType string, found bool, err error)

	// GetAlbumImage returns the image object key and MIME type for an album.
	// Returns found=false (no error) when no image is stored.
	GetAlbumImage(ctx context.Context, artist, album string) (objectKey, mimeType string, found bool, err error)
}
