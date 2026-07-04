package database

import "database/sql"

// User is a row in the users table.
type User struct {
	ID                     int64
	Username               string
	PasswordHash           string
	PasswordChangeRequired bool
	Disabled               bool
	CreatedAt              int64
}

// Role is a row in the roles table — a named bundle of permissions. BuiltIn
// marks the seeded roles (admin/moderator/uploader/listener) that ship with the
// schema and cannot be deleted.
type Role struct {
	ID      int64
	Name    string
	BuiltIn bool
}

// APIToken is a row in the api_tokens table. The raw token is never stored —
// only its hash. RawToken is populated only at creation time, to show once.
type APIToken struct {
	ID         int64
	UserID     int64
	Name       string
	CreatedAt  int64
	LastUsedAt sql.NullInt64
	ExpiresAt  sql.NullInt64
	RevokedAt  sql.NullInt64
	RawToken   string `json:"-"`
}

// Review states for tagsets.review_state (migration 017, moved onto the tagset
// by migration 024). Draft and returned are editable by the uploader; submitted
// awaits a moderator; only approved tagsets are visible in the library. The
// state is orthogonal to DeletedAt: a trashed tagset keeps its review state and
// re-enters it on restore.
const (
	ReviewDraft     = "draft"
	ReviewSubmitted = "submitted"
	ReviewReturned  = "returned"
	ReviewApproved  = "approved"
)

// Storage-backend origin hints recorded in files.storage_backend. They are an
// origin hint only — the resolver decides which copy to serve by probing storages
// (see docs/architecture/data-sources.md), so serving never depends on this value.
const (
	StorageBackendLocal = "local" // an owned blob under files_dir/audio (uploads)
	StorageBackendLinks = "links" // a symlink to an external original (symlink import)
)

// File is a row in the files table — one record per unique content hash.
//
// The catalog lifecycle (trash, review) lives on the file's tagset since
// migration 024 (docs/architecture/recording-tagsets.md); DeletedAt,
// ReviewState, ReviewNote, and SubmittedAt are *derived* from the file's
// offered tagset (origin_file_id = this file) so the upload/dedup flows keep
// their one-lookup shape. Writers go through the tagset-targeting methods
// (SoftDeleteFileByHash, UpdateReviewState, …), never these fields.
type File struct {
	ID             int64
	Hash           string
	ByteSize       int64
	MimeType       string
	StorageBackend string
	ObjectKey      string
	// LinkTarget is the absolute path of the external original for a symlink
	// import (storage_backend='links'); invalid (NULL) for owned local blobs.
	// See docs/architecture/data-sources.md.
	LinkTarget sql.NullString
	CreatedAt  int64
	// RecordingID is the audio identity the file is a rendition of. Every file
	// belongs to a recording (NOT NULL, enforced by trigger); InsertFile creates
	// a singleton recording, which the fingerprint resolver may later merge.
	RecordingID int64
	// UploadedBy is the id of the uploading user, or invalid for pre-auth /
	// federated files.
	UploadedBy sql.NullInt64
	// DeletedAt (derived, tagsets.deleted_at): set when the file's tagset has
	// been soft-deleted (moved to trash). NULL means the tagset is live.
	DeletedAt sql.NullInt64
	// ReviewState (derived, tagsets.review_state) is one of the Review*
	// constants. On insert it seeds the offered tagset's state.
	ReviewState string
	// ReviewNote (derived) carries the moderator's message on a returned
	// tagset; cleared on submit and approve.
	ReviewNote sql.NullString
	// SubmittedAt (derived) is the time of the last transition to submitted.
	SubmittedAt sql.NullInt64
}

// FileUpload is a row in the file_uploads table — one record per
// (file, original filename) tuple.
type FileUpload struct {
	ID         int64
	FileID     int64
	Filename   string
	UploadedAt int64
}

// FileRef pairs a content hash with the original filenames recorded for it.
// Used by admin delete/prune flows to report what was (or would be) removed.
// StorageBackend + LinkTarget let the prune scan probe the right storage (a
// local blob vs a links symlink) and detect a retargeted link.
type FileRef struct {
	Hash           string
	Filenames      []string
	StorageBackend string // StorageBackendLocal | StorageBackendLinks
	LinkTarget     string // recorded external target for a links row; "" for local
}

// FileListEntry is a flattened view of a file row joined with its first
// upload filename and media_metadata tags. Used for the library listing and
// the trash listing (DeletedAt is populated only for the latter).
type FileListEntry struct {
	ID              int64
	Hash            string
	Filename        string
	MimeType        string
	ByteSize        int64
	ObjectKey       string
	CreatedAt       int64
	Title           string
	Artist          string
	AlbumArtist     sql.NullString
	Album           string
	TrackNumber     sql.NullInt64
	DiscNumber      sql.NullInt64
	Year            int64
	DurationSeconds sql.NullFloat64
	GuestPlayable   bool
	License         sql.NullString
	// ArtistHasImage / AlbumHasImage: whether the resolved artist/album entity has
	// a cover — feeds the grouped view's "Add cover" affordance (offer only when missing).
	ArtistHasImage bool
	AlbumHasImage  bool
	DeletedAt      sql.NullInt64
	// ReviewState is populated by the trash listing so the UI can badge a
	// discarded submission (it re-enters the review queue on restore).
	ReviewState string
}

// ReviewEntry is a staged (non-approved, non-trashed) file as listed by the
// review flows: the uploader's "My uploads" staging list and the moderation
// queue. UploaderName is populated only by ListPendingReview.
type ReviewEntry struct {
	Hash            string
	Filename        string
	MimeType        string
	ByteSize        int64
	ObjectKey       string
	CreatedAt       int64
	Title           string
	Artist          sql.NullString
	Album           sql.NullString
	AlbumArtist     sql.NullString
	TrackNumber     sql.NullInt64
	DiscNumber      sql.NullInt64
	Year            sql.NullInt64
	ArtistHasImage  bool
	AlbumHasImage   bool
	DurationSeconds sql.NullFloat64
	ReviewState     string
	ReviewNote      sql.NullString
	SubmittedAt     sql.NullInt64
	UploaderID      sql.NullInt64
	UploaderName    sql.NullString
}

// ArtistEntry is a row returned by ListArtists. ID is the stable artists.id
// surrogate (added with the entity overlay); Name is its canonical display name.
type ArtistEntry struct {
	ID         int64
	Name       string
	TrackCount int
	HasImage   bool
}

// AlbumEntry is a row returned by ListAlbumsByArtistID. ID is the stable albums.id
// surrogate. Title="" represents the "Other" bucket (tracks with no album, i.e.
// the unknown-album entity under that artist).
type AlbumEntry struct {
	ID         int64
	ArtistID   int64
	ArtistName string
	Title      string
	Year       sql.NullInt64
	TrackCount int
	HasImage   bool
}

// TrackEntry is a row returned by ListTracksByAlbumID. ArtistName is the track's
// performer (its media_metadata.artist_id entity), which may differ from the
// album's album-artist on a compilation; "" when unresolved.
type TrackEntry struct {
	ID              int64
	Hash            string
	Title           string
	ArtistName      string
	TrackNumber     sql.NullInt64
	DiscNumber      sql.NullInt64
	DurationSeconds sql.NullFloat64
	ObjectKey       string
	MimeType        string
}

// MergePreview is the read-only "what would this merge do" report returned by
// MergeArtistsPreview / MergeAlbumsPreview. It computes the same moves the
// corresponding destructive merge performs, without mutating anything. Fields
// that don't apply to a given merge kind are left zero (see the per-field notes).
type MergePreview struct {
	FromID    int64  // source entity (merged away)
	IntoID    int64  // target entity (absorbs the source)
	FromLabel string // source display name/title
	IntoLabel string // target display name/title

	TracksMoved int // media_metadata rows repointed off the source

	// Artist merge only:
	AlbumsMoved     int      // source albums with no title collision, moved as-is
	AlbumsCollapsed int      // source albums folding into an existing target album
	CollapsedTitles []string // titles of the collapsing source albums

	SourceHasCover bool // the source entity has a cover (moves only if target lacks one)
	TargetHasCover bool // the target entity already has a cover (kept; source's ignored)

	// Album merge only:
	SourceArtistOrphaned bool // the source album's artist would be left with nothing
}

// SearchResults is returned by Search and SearchGuest.
type SearchResults struct {
	Artists []*ArtistEntry
	Albums  []*AlbumEntry
	Tracks  []*SearchTrackEntry
}

// SearchTrackEntry is like TrackEntry but also carries the album title for
// context. ArtistName is the track's performer (its artist_id entity), matching
// the track list; AlbumTitle lets the frontend navigate to the right drill level.
type SearchTrackEntry struct {
	ID              int64
	Title           string
	TrackNumber     sql.NullInt64
	DurationSeconds sql.NullFloat64
	ObjectKey       string
	MimeType        string
	ArtistName      string
	AlbumTitle      string
}

// MediaMetadata is the combined per-file metadata view: the descriptive tag
// fields live on the file's offered tagset and the tech fields (duration,
// bitrate, …) on the media_metadata table (blob-owned, ffprobe-filled) since
// migration 024 — InsertFile splits a value into the two rows and the readers
// join them back. Tag fields are nullable because uploads may carry incomplete
// (or no) tags — except Title, which is required non-empty (migration 016): it
// defaults to the filename (extension stripped) when the file has no title tag.
type MediaMetadata struct {
	FileID          int64
	Title           string
	Artist          sql.NullString
	Album           sql.NullString
	AlbumArtist     sql.NullString
	Genre           sql.NullString
	Year            sql.NullInt64
	TrackNumber     sql.NullInt64
	TrackTotal      sql.NullInt64
	DiscNumber      sql.NullInt64
	Composer        sql.NullString
	Comment         sql.NullString
	DurationSeconds sql.NullFloat64
	Bitrate         sql.NullInt64
	SampleRate      sql.NullInt64
	Channels        sql.NullInt64
	Codec           sql.NullString
	TagFormat       sql.NullString
	ExtractedAt     int64
}
