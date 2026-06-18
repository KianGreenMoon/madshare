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

	// LibraryByteSize returns the total logical byte size of stored blobs
	// (SUM of byte_size over every files row, trashed-but-unpruned included).
	// Backs the "audio" disk-usage category (see adminStorageStats).
	LibraryByteSize(ctx context.Context) (int64, error)

	// ListArtists returns one entry per effective artist name, ordered
	// alphabetically. album_artist is preferred over artist for grouping.
	ListArtists(ctx context.Context) ([]*ArtistEntry, error)

	// ListAlbumsByArtistID returns the albums of one artist, addressed by its
	// stable surrogate id. Tracks with no album are grouped under Title="" (the
	// unknown-album entity).
	ListAlbumsByArtistID(ctx context.Context, artistID int64) ([]*AlbumEntry, error)

	// ListTracksByAlbumID returns the tracks of one album, addressed by its
	// stable surrogate id (which already pins the artist). The "Other" bucket is
	// reached via the unknown-album entity's id.
	ListTracksByAlbumID(ctx context.Context, albumID int64) ([]*TrackEntry, error)

	// ListFilesGuest, ListArtistsGuest, ListAlbumsByArtistIDGuest, and
	// ListTracksByAlbumIDGuest are the guest counterparts of the listings
	// above: they return only what an anonymous / capability-less request may
	// reach (the guest-playable / license policy). Callers holding content.access
	// use the unfiltered variants.
	ListFilesGuest(ctx context.Context) ([]*FileListEntry, error)
	ListArtistsGuest(ctx context.Context) ([]*ArtistEntry, error)
	ListAlbumsByArtistIDGuest(ctx context.Context, artistID int64) ([]*AlbumEntry, error)
	ListTracksByAlbumIDGuest(ctx context.Context, albumID int64) ([]*TrackEntry, error)

	// Search returns artists, albums, and tracks whose names/titles contain q
	// (case-insensitive LIKE). q="" returns empty results immediately.
	Search(ctx context.Context, q string) (*SearchResults, error)

	// SearchGuest is Search restricted to content an anonymous / capability-less
	// request can reach (the guest-playable / license policy).
	SearchGuest(ctx context.Context, q string) (*SearchResults, error)

	// UpsertArtistImage stores (or replaces) the image reference for an artist
	// entity, keyed by artists.id.
	UpsertArtistImage(ctx context.Context, artistID int64, objectKey, mimeType string, updatedAt int64) error

	// UpsertAlbumImage stores (or replaces) the image reference for an album
	// entity, keyed by albums.id.
	UpsertAlbumImage(ctx context.Context, albumID int64, objectKey, mimeType string, updatedAt int64) error

	// GetArtistImage returns the image object key and MIME type for an artist
	// entity. Returns found=false (no error) when no image is stored.
	GetArtistImage(ctx context.Context, artistID int64) (objectKey, mimeType string, found bool, err error)

	// GetAlbumImage returns the image object key and MIME type for an album
	// entity. Returns found=false (no error) when no image is stored.
	GetAlbumImage(ctx context.Context, albumID int64) (objectKey, mimeType string, found bool, err error)

	// LookupArtistID returns the artists.id for a display name (matched by its
	// normalized key), or found=false. Lookup-only (never creates a row) — for
	// read paths that must not materialize entities for unknown names.
	LookupArtistID(ctx context.Context, name string) (id int64, found bool, err error)

	// LookupAlbumID returns the albums.id for (artist, album) matched by their
	// normalized keys, or found=false. Lookup-only.
	LookupAlbumID(ctx context.Context, artist, album string) (id int64, found bool, err error)

	// ResolveArtistID get-or-creates the artist entity for a display name and
	// returns its id. For cover-write paths that may target an entity with no
	// other attachment. Idempotent.
	ResolveArtistID(ctx context.Context, name string) (int64, error)

	// ResolveAlbumID get-or-creates the (artist, album) entity and returns the
	// album id. For cover-write paths.
	ResolveAlbumID(ctx context.Context, artist, album string) (int64, error)

	// RenameArtist changes an artist entity's display name (and dedup key) in
	// place; tracks and cover follow via FKs. Returns ErrEntityNotFound or
	// ErrNameConflict (the target name is already taken — that is a merge).
	RenameArtist(ctx context.Context, artistID int64, newName string) error

	// RenameAlbum changes an album entity's title (and dedup key) in place.
	// Returns ErrEntityNotFound or ErrNameConflict (a different album under the
	// same artist already has that title).
	RenameAlbum(ctx context.Context, albumID int64, newTitle string) error

	// MergeArtists merges artist fromID into intoID (tracks/albums repointed,
	// colliding albums collapsed, covers moved if the target lacks one, source
	// deleted). Returns ErrMergeSelf / ErrEntityNotFound.
	MergeArtists(ctx context.Context, fromID, intoID int64) error

	// MergeAlbums merges album fromID into intoID (tracks repointed onto the
	// target and its artist, cover moved if absent, source deleted). Returns
	// ErrMergeSelf / ErrEntityNotFound.
	MergeAlbums(ctx context.Context, fromID, intoID int64) error

	// MergeArtistsPreview / MergeAlbumsPreview report what the corresponding
	// merge would do (tracks moved, albums collapsed, cover/orphan effects)
	// without mutating anything. Same ErrMergeSelf / ErrEntityNotFound contract.
	MergeArtistsPreview(ctx context.Context, fromID, intoID int64) (*MergePreview, error)
	MergeAlbumsPreview(ctx context.Context, fromID, intoID int64) (*MergePreview, error)

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

	// GetTrashRestorePolicy reads the trash-restore policy (reupload_restores /
	// inform / uploader_restore); defaults to reupload_restores when unset.
	GetTrashRestorePolicy(ctx context.Context) (string, error)

	// ListTrashedFiles returns all soft-deleted files ordered by deletion time
	// descending, joined with the first filename and media_metadata tags.
	ListTrashedFiles(ctx context.Context) ([]*FileListEntry, error)

	// ListFileRefs returns one FileRef per files row, each carrying the
	// content hash and the filenames recorded for it, ordered by file id.
	ListFileRefs(ctx context.Context) ([]FileRef, error)

	// RecordAudit appends a row to the audit log. actorUserID is invalid for
	// system/anonymous actions.
	RecordAudit(ctx context.Context, actorUserID sql.NullInt64, action, target, detail string) error

	// FileAccessibleByHash reports whether an anonymous / capability-less request
	// may play/download the file with the given hash (the guest-playable /
	// license policy). Callers holding the content.access permission bypass this.
	// Unknown hashes return false.
	FileAccessibleByHash(ctx context.Context, hash string) (bool, error)

	// --- Moderation review bucket (docs/architecture/moderation.md) ---

	// ListUploadsByUser returns the user's staged files (non-trashed, review
	// state other than approved), newest first. Backs the "My uploads" tab.
	ListUploadsByUser(ctx context.Context, userID int64) ([]*ReviewEntry, error)

	// ListPendingReview returns every staged file with its uploader's name,
	// ordered for by-uploader grouping in the moderation queue.
	ListPendingReview(ctx context.Context) ([]*ReviewEntry, error)

	// UpdateReviewState applies a guarded review-state transition (single
	// UPDATE: state must be in From, file non-trashed, owner matching when
	// OwnerID is set). found is false when no row satisfies the guard.
	UpdateReviewState(ctx context.Context, hash string, t ReviewTransition) (found bool, err error)

	// FileReviewInfo is the narrow (state, uploader, trashed) lookup used by
	// the blob-access gate and ownership checks. found is false on unknown hash.
	FileReviewInfo(ctx context.Context, hash string) (state string, uploadedBy sql.NullInt64, deleted bool, found bool, err error)

	// StageRestoredFile demotes a just-restored approved file to the
	// restorer's draft so an upload-initiated restore re-enters the staging
	// pipeline instead of silently republishing. No-op (false) for files that
	// were trashed while pending.
	StageRestoredFile(ctx context.Context, hash string, ownerID sql.NullInt64) (bool, error)

	// DiscardOwnUpload soft-deletes the owner's editable (draft/returned)
	// staged file; found=false for submitted, foreign, or unknown files.
	DiscardOwnUpload(ctx context.Context, hash string, ownerID int64) (bool, error)

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

	// EnqueueAnalysisJob inserts a pending media-analysis job (ffprobe + fpcalc)
	// for a file. Idempotent per file_id (at most one active job); a duplicate
	// enqueue is a no-op. See docs/architecture/recordings.md (P0). The worker
	// methods (ClaimAnalysisJob/FinishAnalysisJob/UpsertTechColumns/
	// InsertAudioFingerprint) live on *database.DB only, behind mediaproc's own
	// interface, so they do not widen this Repository.
	EnqueueAnalysisJob(ctx context.Context, fileID, now int64) error

	// ListDuplicateRecordings returns every recording with >1 non-trashed
	// rendition (the duplicates admin page; recordings P2), each with its
	// renditions' tech info + display fields.
	ListDuplicateRecordings(ctx context.Context) ([]DuplicateRecording, error)

	// SplitRendition detaches a file into a new pinned recording (the "save as
	// another composition" action). found is false when no live file matches.
	SplitRendition(ctx context.Context, fileID int64) (newRecordingID int64, found bool, err error)

	// IsDuplicateSubmission reports whether the file duplicates already-approved
	// content (recordings P3): by fingerprint/recording when one exists, else a
	// non-default tag collision. Suppresses self-approve and flags the queue.
	IsDuplicateSubmission(ctx context.Context, hash string) (bool, error)

	// RecordingRenditionsByHash returns the approved renditions of the file's
	// recording (recordings P4, the player's quality control). A file with no
	// recording yields just itself; an unknown/non-approved hash yields nil.
	RecordingRenditionsByHash(ctx context.Context, hash string) ([]DuplicateRendition, error)

	// SetAlbumCover inserts/replaces an album cover row (keyed by albums.id) with
	// variant-tracking fields (variants_ready reset to 0).
	SetAlbumCover(ctx context.Context, albumID int64, baseKey, sourceExt, objectKey, mimeType string, now int64) error

	// SetAlbumCoverIfAbsent inserts an album cover row (keyed by albums.id) only
	// when none exists, reporting inserted=true exactly when this call created it.
	// Race-free fill-if-missing: it never overwrites an existing cover.
	SetAlbumCoverIfAbsent(ctx context.Context, albumID int64, baseKey, sourceExt, objectKey, mimeType string, now int64) (bool, error)

	// GetAlbumCoverStatus returns the variant-tracking state for an album cover;
	// found is false when no row exists.
	GetAlbumCoverStatus(ctx context.Context, albumID int64) (baseKey, sourceExt string, variantsReady, found bool, err error)

	// HasAlbumCover reports whether an album_images row exists for the album entity.
	HasAlbumCover(ctx context.Context, albumID int64) (bool, error)

	// --- Base metadata editing (Phase 5: upload & covers) ---

	// UpdateFileMetadata writes the provided fields (nil = leave unchanged) onto
	// the media_metadata row of the file with the given content hash and returns
	// the updated row. Returns ErrFileNotFound when no file matches, or an error
	// wrapping ErrInvalidMetadata when a numeric field carries a bad value.
	UpdateFileMetadata(ctx context.Context, hash string, p MetadataPatch) (*MediaMetadata, error)

	// FileMetadataByHash loads the editable media_metadata row for the file with
	// the given content hash. Returns ErrFileNotFound when no file matches.
	FileMetadataByHash(ctx context.Context, hash string) (*MediaMetadata, error)

	// --- Playlists & favorites (docs/api/playlists.md) ---
	// All playlist methods are scoped to userID; a playlist id belonging to a
	// different user yields ErrPlaylistNotFound (mapped to 404, never 403).

	// ListPlaylists returns the user's playlists with item counts (favorites
	// first). Does not create the favorites row.
	ListPlaylists(ctx context.Context, userID int64) ([]*Playlist, error)

	// EnsureFavoritesPlaylist returns the user's favorites playlist id,
	// creating it if absent. Idempotent.
	EnsureFavoritesPlaylist(ctx context.Context, userID int64) (int64, error)

	// CreatePlaylist creates a regular playlist, optionally seeded with items
	// (content hashes, in order). Any unknown/trashed hash fails the whole
	// create with ErrFileNotFound.
	CreatePlaylist(ctx context.Context, userID int64, name string, hashes []string) (*Playlist, error)

	// GetPlaylist returns the playlist and its items in order. Trashed files
	// stay listed (Trashed=true); hard-deleted files vanish via FK cascade.
	GetPlaylist(ctx context.Context, userID, playlistID int64) (*Playlist, []*PlaylistItemEntry, error)

	// RenamePlaylist / DeletePlaylist operate on regular playlists only;
	// favorites returns ErrPlaylistSystem.
	RenamePlaylist(ctx context.Context, userID, playlistID int64, name string) error
	DeletePlaylist(ctx context.Context, userID, playlistID int64) error

	// AddPlaylistItemsByHash atomically appends tracks by content hash; any
	// unknown/trashed hash fails the batch with ErrFileNotFound. On the
	// favorites playlist, already-present files are skipped.
	AddPlaylistItemsByHash(ctx context.Context, userID, playlistID int64, hashes []string) (added int, err error)

	// RemovePlaylistItem removes one item by its id; found is false (no error)
	// when the item is not in that playlist.
	RemovePlaylistItem(ctx context.Context, userID, playlistID, itemID int64) (found bool, err error)

	// ReorderPlaylist rewrites the item order; itemIDs must be a permutation of
	// the playlist's current item ids (ErrBadReorder otherwise).
	ReorderPlaylist(ctx context.Context, userID, playlistID int64, itemIDs []int64) error

	// ToggleFavorite flips the file's membership in the user's favorites
	// playlist (created on first use) and returns the resulting state.
	// Unknown or trashed hashes return ErrFileNotFound.
	ToggleFavorite(ctx context.Context, userID int64, hash string) (liked bool, err error)

	// ListFavoriteHashes returns the user's live favorite hashes in order.
	ListFavoriteHashes(ctx context.Context, userID int64) ([]string, error)
}
