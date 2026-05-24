package database

import "database/sql"

// File is a row in the files table — one record per unique content hash.
type File struct {
	ID             int64
	Hash           string
	ByteSize       int64
	MimeType       string
	StorageBackend string
	ObjectKey      string
	CreatedAt      int64
}

// FileUpload is a row in the file_uploads table — one record per
// (file, original filename) tuple.
type FileUpload struct {
	ID         int64
	FileID     int64
	Filename   string
	UploadedAt int64
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
