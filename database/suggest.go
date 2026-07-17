package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Tag-suggestion subject lookup (docs/architecture/tag-suggestions.md). The
// suggestions endpoint is tagset-addressed; the local sources re-read the
// appearance's ORIGIN blob (the file whose bytes carried the tags —
// origin_file_id is exactly that provenance), and the P1 service source will
// use the origin file's acoustic fingerprint.

// SuggestSubject is the origin-blob identity + analysis facts behind one
// appearance — what the tag-suggestion sources need.
type SuggestSubject struct {
	Hash        string
	MIMEType    string
	Fingerprint []byte          // packed chromaprint sub-fingerprints; nil when not analyzed
	Duration    sql.NullFloat64 // fingerprinted duration in seconds
}

// TagsetSuggestSubject loads the suggestion subject for one appearance. found
// is false (no error) on an unknown tagset id.
func (db *DB) TagsetSuggestSubject(ctx context.Context, tagsetID int64) (*SuggestSubject, bool, error) {
	var s SuggestSubject
	err := db.QueryRowContext(ctx, `
		SELECT f.hash, f.mime_type, af.fingerprint, af.duration
		FROM tagsets t
		JOIN files f ON f.id = t.origin_file_id
		LEFT JOIN audio_fingerprints af ON af.file_id = f.id
		WHERE t.id = ?`, tagsetID).
		Scan(&s.Hash, &s.MIMEType, &s.Fingerprint, &s.Duration)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("tagset suggest subject: %w", err)
	}
	return &s, true, nil
}
