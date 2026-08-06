// Package media extracts audio tag metadata from uploaded files.
//
// In v0 it only wraps github.com/dhowden/tag, which covers ID3v1/v2,
// MP4 (M4A), FLAC, and OGG Vorbis. Bitrate, sample rate, channel count,
// codec, and duration are intentionally left out — dhowden/tag does not
// reliably report them. Adding ffprobe-based extraction later is a
// non-breaking change.
package media

import (
	"errors"
	"fmt"
	"io"

	"github.com/dhowden/tag"
)

// Tags is the subset of tag.Metadata Madshare cares about for v0.
// Strings are empty when missing; ints are 0.
type Tags struct {
	Title       string
	Artist      string
	Album       string
	AlbumArtist string
	Genre       string
	Composer    string
	Comment     string
	TagFormat   string
	// FileType is the CONTAINER the tag reader recognised from the bytes
	// ("MP3", "FLAC", "OGG", "M4A", …; empty when unknown). Distinct from
	// TagFormat, which names the tag dialect inside it — an MP3 carries ID3v2.
	// It is the only thing that can say what kind of file a blob is when its
	// name was never recorded, which is what lets a cached blob with no
	// filename still be materialized (docs/architecture/madnetwork-cache.md).
	FileType string

	Year        int
	TrackNumber int
	TrackTotal  int
	DiscNumber  int

	// CoverImage holds the embedded cover art extracted from the audio tags.
	// Nil when the file carries no embedded art (or it could not be read).
	CoverImage *CoverData
}

// CoverData is a single piece of embedded cover art: its declared MIME type and
// raw bytes, as reported by the tag reader.
type CoverData struct {
	MIMEType string // e.g. "image/jpeg"
	Data     []byte
}

// ExtractTags reads ID3/MP4/FLAC/OGG tags from r. The mimeType argument is
// informational only — tag.ReadFrom sniffs the content itself.
//
// A file with no recognisable tags returns (&Tags{}, nil), not an error:
// untagged uploads are valid, they just carry no metadata.
func ExtractTags(r io.ReadSeeker, mimeType string) (*Tags, error) {
	_ = mimeType // reserved for future format-specific extraction

	m, err := tag.ReadFrom(r)
	if err != nil {
		if errors.Is(err, tag.ErrNoTagsFound) {
			return &Tags{}, nil
		}
		return nil, fmt.Errorf("read tags: %w", err)
	}

	trackNum, trackTotal := m.Track()
	discNum, _ := m.Disc()

	t := &Tags{
		Title:       m.Title(),
		Artist:      m.Artist(),
		Album:       m.Album(),
		AlbumArtist: m.AlbumArtist(),
		Genre:       m.Genre(),
		Composer:    m.Composer(),
		Comment:     m.Comment(),
		TagFormat:   string(m.Format()),
		FileType:    string(m.FileType()),
		Year:        m.Year(),
		TrackNumber: trackNum,
		TrackTotal:  trackTotal,
		DiscNumber:  discNum,
	}

	// Embedded cover art is optional; only record it when present and non-empty.
	// Note: for MP4/M4A the tag library only reports a cover when it can infer
	// the MIME type (PNG, or an explicit-flagged atom), so some M4A files with
	// embedded JPEG art may extract nothing here. See .issues/open-issues.md.
	if pic := m.Picture(); pic != nil && len(pic.Data) > 0 {
		t.CoverImage = &CoverData{MIMEType: pic.MIMEType, Data: pic.Data}
	}

	return t, nil
}
