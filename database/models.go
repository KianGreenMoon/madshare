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

// Review states for files.review_state (migration 017). Draft and returned are
// editable by the uploader; submitted awaits a moderator; only approved files
// are visible in the library. The state is orthogonal to DeletedAt: a trashed
// file keeps its review state and re-enters it on restore.
const (
	ReviewDraft     = "draft"
	ReviewSubmitted = "submitted"
	ReviewReturned  = "returned"
	ReviewApproved  = "approved"
)

// File is a row in the files table — one record per unique content hash.
type File struct {
	ID             int64
	Hash           string
	ByteSize       int64
	MimeType       string
	StorageBackend string
	ObjectKey      string
	CreatedAt      int64
	// UploadedBy is the id of the uploading user, or invalid for pre-auth /
	// federated files.
	UploadedBy sql.NullInt64
	// DeletedAt is set when the file has been soft-deleted (moved to trash).
	// NULL means the file is live.
	DeletedAt sql.NullInt64
	// ReviewState is one of the Review* constants.
	ReviewState string
	// ReviewNote carries the moderator's message on a returned file; cleared
	// on submit and approve.
	ReviewNote sql.NullString
	// SubmittedAt is the time of the last transition to submitted.
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
type FileRef struct {
	Hash      string
	Filenames []string
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
	Year            int64
	DurationSeconds sql.NullFloat64
	GuestPlayable   bool
	License         sql.NullString
	DeletedAt       sql.NullInt64
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
	Year            sql.NullInt64
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

// AlbumEntry is a row returned by ListAlbumsByArtist. ID is the stable albums.id
// surrogate. Title="" represents the "Other" bucket (tracks with no album, i.e.
// the unknown-album entity under that artist).
type AlbumEntry struct {
	ID         int64
	ArtistName string
	Title      string
	Year       sql.NullInt64
	TrackCount int
	HasImage   bool
}

// TrackEntry is a row returned by ListTracksByAlbumArtist.
type TrackEntry struct {
	ID              int64
	Title           string
	TrackNumber     sql.NullInt64
	DurationSeconds sql.NullFloat64
	ObjectKey       string
	MimeType        string
}

// SearchResults is returned by Search and SearchGuest.
type SearchResults struct {
	Artists []*ArtistEntry
	Albums  []*AlbumEntry
	Tracks  []*SearchTrackEntry
}

// SearchTrackEntry is like TrackEntry but also carries the artist and album
// so the frontend can navigate to the right drill-down level.
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

// MediaMetadata is a row in the media_metadata table. Tag fields are nullable
// because uploads may carry incomplete (or no) tags — except Title, which is
// required non-empty (migration 016): it defaults to the filename (extension
// stripped) when the file has no title tag.
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
