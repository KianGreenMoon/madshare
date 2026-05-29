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
// upload filename and media_metadata tags. Used for the library listing.
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
	Year            int64
	DurationSeconds sql.NullFloat64
	GuestPlayable   bool
	License         sql.NullString
}

// ArtistEntry is a row returned by ListArtists.
type ArtistEntry struct {
	Name       string
	TrackCount int
	HasImage   bool
}

// AlbumEntry is a row returned by ListAlbumsByArtist.
// Title="" represents the "Other" bucket (tracks with no album).
type AlbumEntry struct {
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

// MediaMetadata is a row in the media_metadata table. All tag fields are
// nullable because uploads may carry incomplete (or no) tags.
type MediaMetadata struct {
	FileID          int64
	Title           sql.NullString
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
