package database

import (
	"context"
	"database/sql"
)

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

	// ListFilesFiltered, ListArtistsFiltered, ListAlbumsByArtistFiltered, and
	// ListTracksByAlbumArtistFiltered are the access-filtered counterparts of the
	// listings above: they return only what the user (invalid userID =
	// anonymous) may reach per the §5.3 predicate. Callers holding content.all
	// use the unfiltered variants.
	ListFilesFiltered(ctx context.Context, userID sql.NullInt64) ([]*FileListEntry, error)
	ListArtistsFiltered(ctx context.Context, userID sql.NullInt64) ([]*ArtistEntry, error)
	ListAlbumsByArtistFiltered(ctx context.Context, artist string, userID sql.NullInt64) ([]*AlbumEntry, error)
	ListTracksByAlbumArtistFiltered(ctx context.Context, artist, album string, userID sql.NullInt64) ([]*TrackEntry, error)

	// Search returns artists, albums, and tracks whose names/titles contain q
	// (case-insensitive LIKE). q="" returns empty results immediately.
	Search(ctx context.Context, q string) (*SearchResults, error)

	// SearchFiltered is Search restricted to content the user (invalid userID =
	// anonymous) can reach per the §5.3 access predicate.
	SearchFiltered(ctx context.Context, q string, userID sql.NullInt64) (*SearchResults, error)

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

	// SoftDeleteFileByHash marks the file as trashed (sets deleted_at). The
	// blob and DB row are preserved. Returns the recorded filenames for audit.
	// found is false (no error) when no live file matches the hash.
	SoftDeleteFileByHash(ctx context.Context, hash string) (filenames []string, found bool, err error)

	// HardDeleteFileByHash permanently removes the files row for hash (cascading
	// to its file_uploads and media_metadata rows). Works on both live and
	// trashed files. Used by PruneDangling. found is false (no error) when no
	// row matches.
	HardDeleteFileByHash(ctx context.Context, hash string) (filenames []string, found bool, err error)

	// HardDeleteTrashedFileByHash permanently removes a trashed files row.
	// Live (non-trashed) files return found=false so the caller cannot bypass
	// the soft-delete step. The check and delete are atomic within one
	// transaction, preventing a concurrent restore from racing the delete.
	HardDeleteTrashedFileByHash(ctx context.Context, hash string) (filenames []string, found bool, err error)

	// RestoreFileByHash clears deleted_at on a trashed file, returning it to
	// the live library. found is false (no error) when no trashed row matches.
	RestoreFileByHash(ctx context.Context, hash string) (found bool, err error)

	// ListTrashedFiles returns all soft-deleted files ordered by deletion time
	// descending, joined with the first filename and media_metadata tags.
	ListTrashedFiles(ctx context.Context) ([]*FileListEntry, error)

	// ListFileRefs returns one FileRef per files row, each carrying the
	// content hash and the filenames recorded for it, ordered by file id.
	ListFileRefs(ctx context.Context) ([]FileRef, error)

	// RecordAudit appends a row to the audit log. actorUserID is invalid for
	// system/anonymous actions.
	RecordAudit(ctx context.Context, actorUserID sql.NullInt64, action, target, detail string) error

	// FileAccessibleByHash reports whether the user (invalid userID = anonymous)
	// may play/download the file with the given hash. Callers holding the
	// content.all permission bypass this. Unknown hashes return false.
	FileAccessibleByHash(ctx context.Context, hash string, userID sql.NullInt64) (bool, error)

	// --- Cover image variants & async job queue (Phase 1: upload & covers) ---

	// EnqueueImageJob inserts a pending image-variant job. Idempotent per
	// base_key (at most one active job); a duplicate enqueue is a no-op.
	EnqueueImageJob(ctx context.Context, coverType, subjectKey, baseKey string, now int64) error

	// ClaimImageJob atomically claims the oldest pending job (flipping it to
	// running) and returns it, or (nil, nil) when the queue is empty.
	ClaimImageJob(ctx context.Context) (*ImageJob, error)

	// FinishImageJob records a claimed job's outcome, owning the
	// done/retry/failed decision and flagging variants_ready on success.
	FinishImageJob(ctx context.Context, id int64, jobErr error) error

	// ResetStaleJobs returns all running jobs to pending (startup recovery).
	ResetStaleJobs(ctx context.Context) error

	// SetAlbumCover inserts/replaces an album cover row with variant-tracking
	// fields (variants_ready reset to 0).
	SetAlbumCover(ctx context.Context, artist, album, baseKey, sourceExt, objectKey, mimeType string, now int64) error

	// SetAlbumCoverIfAbsent inserts an album cover row only when none exists,
	// reporting inserted=true exactly when this call created it. Race-free
	// fill-if-missing: it never overwrites an existing cover.
	SetAlbumCoverIfAbsent(ctx context.Context, artist, album, baseKey, sourceExt, objectKey, mimeType string, now int64) (bool, error)

	// GetAlbumCoverStatus returns the variant-tracking state for an album cover;
	// found is false when no row exists.
	GetAlbumCoverStatus(ctx context.Context, artist, album string) (baseKey, sourceExt string, variantsReady, found bool, err error)

	// HasAlbumCover reports whether any album_images row exists for the album.
	HasAlbumCover(ctx context.Context, artist, album string) (bool, error)
}
