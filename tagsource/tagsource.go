// Package tagsource produces candidate tagsets ("suggestions") for one
// appearance from multiple sources: the file's own ID3v2/native tags, its
// ID3v1 trailer with charset re-decoding, and (P1, future) external services
// like MusicBrainz. Suggestions are read-only — applying one is the edit
// modal's job, through the existing metadata PATCH paths. Design:
// docs/architecture/tag-suggestions.md.
package tagsource

import (
	"fmt"
	"io"
	"strconv"
	"strings"

	"daemonlord.ygg/madshare/media"
)

// Source names — the API's ?sources= vocabulary and the UI's chip identity.
const (
	SourceID3v2 = "id3v2" // the file's preferred/native tags (ID3v2, VORBIS, MP4…)
	SourceID3v1 = "id3v1" // the trailing 128-byte ID3v1 block
)

// LocalSources returns the local source names in default (chip) order.
func LocalSources() []string { return []string{SourceID3v2, SourceID3v1} }

// ValidLocalSource reports whether name is a known local source.
func ValidLocalSource(name string) bool {
	return name == SourceID3v2 || name == SourceID3v1
}

// Subject is what the sources may look at for one appearance.
type Subject struct {
	// OpenBlob opens the appearance's origin blob for reading. The caller has
	// already resolved it on disk; each source opens its own handle.
	OpenBlob func() (io.ReadSeekCloser, error)
	// Charset overrides the detected charset for the local sources ("" = auto).
	// Must be pre-validated (media.ValidCharset).
	Charset string
}

// Suggestion is one candidate tagset with provenance, shaped for the
// suggestions endpoint's JSON. Tags holds only the fields the source has an
// opinion on, keyed like the metadata PATCH body (title, artist, album,
// album_artist, genre, composer, comment, year, track_number, track_total,
// disc_number).
type Suggestion struct {
	Source     string         `json:"source"`
	Label      string         `json:"label"`
	Charset    string         `json:"charset,omitempty"`  // charset this decode used
	Charsets   []string       `json:"charsets,omitempty"` // offered overrides
	Tags       map[string]any `json:"tags,omitempty"`
	Confidence float64        `json:"confidence"` // local sources: 1
	Err        string         `json:"error,omitempty"`
}

// Local runs the requested local sources over the subject, in the given order.
// A source with nothing to say (no such tag block) is silently absent; a
// source that failed reading contributes an error entry so the UI can show a
// disabled chip instead of hiding the failure.
func Local(sources []string, sub Subject) []Suggestion {
	out := make([]Suggestion, 0, len(sources))
	for _, name := range sources {
		var s *Suggestion
		switch name {
		case SourceID3v2:
			s = suggestID3v2(sub)
		case SourceID3v1:
			s = suggestID3v1(sub)
		}
		if s != nil {
			out = append(out, *s)
		}
	}
	return out
}

// errSuggestion is the "chip renders an error" shape for a source that exists
// but could not be read.
func errSuggestion(source, msg string) *Suggestion {
	return &Suggestion{Source: source, Label: source, Err: msg, Confidence: 1}
}

// suggestID3v2 re-reads the file's preferred tags (what ingest extracted):
// ID3v2 for MP3, the native container tags elsewhere. When the file's only
// tags are ID3v1, this source stays silent — the dedicated id3v1 source owns
// that block (with charset control).
func suggestID3v2(sub Subject) *Suggestion {
	f, err := sub.OpenBlob()
	if err != nil {
		return errSuggestion(SourceID3v2, "file unavailable")
	}
	defer f.Close()
	t, err := media.ExtractTags(f, "")
	if err != nil {
		return errSuggestion(SourceID3v2, "unreadable tags")
	}
	if t.TagFormat == "" || t.TagFormat == "ID3v1" {
		return nil
	}
	s := &Suggestion{Source: SourceID3v2, Label: t.TagFormat, Confidence: 1}
	// The charset override reinterprets ID3v2.3 frames that declared ISO-8859-1
	// but really hold a Windows codepage; other formats are UTF-8 by spec.
	isID3v2 := strings.HasPrefix(t.TagFormat, "ID3v2")
	if isID3v2 {
		s.Charsets = media.CharsetNames()
		s.Charset = media.CharsetUTF8
	}
	redecode := func(v string) string {
		if !isID3v2 || sub.Charset == "" || sub.Charset == media.CharsetUTF8 {
			return v
		}
		out, _ := media.ReencodeLatin1(v, sub.Charset)
		return out
	}
	if isID3v2 && sub.Charset != "" {
		s.Charset = sub.Charset
	}
	s.Tags = tagsMap(map[string]any{
		"title":        redecode(t.Title),
		"artist":       redecode(t.Artist),
		"album":        redecode(t.Album),
		"album_artist": redecode(t.AlbumArtist),
		"genre":        redecode(t.Genre),
		"composer":     redecode(t.Composer),
		"comment":      redecode(t.Comment),
		"year":         t.Year,
		"track_number": t.TrackNumber,
		"track_total":  t.TrackTotal,
		"disc_number":  t.DiscNumber,
	})
	return s
}

// suggestID3v1 reads the trailing ID3v1 block and decodes it with the detected
// (or overridden) charset.
func suggestID3v1(sub Subject) *Suggestion {
	f, err := sub.OpenBlob()
	if err != nil {
		return errSuggestion(SourceID3v1, "file unavailable")
	}
	defer f.Close()
	raw, err := media.ReadID3v1(f)
	if err != nil {
		return nil // no v1 trailer — no chip
	}
	cs := sub.Charset
	if cs == "" {
		cs = media.DetectCharset([][]byte{raw.Title, raw.Artist, raw.Album, raw.Comment})
	}
	dec := func(b []byte) string {
		out, _ := media.DecodeWith(cs, b)
		return out
	}
	label := "ID3v1"
	if raw.Track > 0 {
		label = "ID3v1.1"
	}
	year, _ := strconv.Atoi(strings.TrimSpace(string(raw.Year)))
	return &Suggestion{
		Source:   SourceID3v1,
		Label:    label,
		Charset:  cs,
		Charsets: media.CharsetNames(),
		Tags: tagsMap(map[string]any{
			"title":        dec(raw.Title),
			"artist":       dec(raw.Artist),
			"album":        dec(raw.Album),
			"comment":      dec(raw.Comment),
			"genre":        raw.Genre,
			"year":         year,
			"track_number": raw.Track,
		}),
		Confidence: 1,
	}
}

// tagsMap drops the no-opinion fields (empty strings, zero numbers) so the
// panel only offers values the source actually carries.
func tagsMap(fields map[string]any) map[string]any {
	out := make(map[string]any, len(fields))
	for k, v := range fields {
		switch x := v.(type) {
		case string:
			if x != "" {
				out[k] = x
			}
		case int:
			if x != 0 {
				out[k] = x
			}
		default:
			panic(fmt.Sprintf("tagsource: unsupported field type %T", v))
		}
	}
	return out
}
