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

	Year        int
	TrackNumber int
	TrackTotal  int
	DiscNumber  int
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

	return &Tags{
		Title:       m.Title(),
		Artist:      m.Artist(),
		Album:       m.Album(),
		AlbumArtist: m.AlbumArtist(),
		Genre:       m.Genre(),
		Composer:    m.Composer(),
		Comment:     m.Comment(),
		TagFormat:   string(m.Format()),
		Year:        m.Year(),
		TrackNumber: trackNum,
		TrackTotal:  trackTotal,
		DiscNumber:  discNum,
	}, nil
}
