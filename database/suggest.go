package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Tag-suggestion subject lookup (docs/architecture/tag-suggestions.md). The
// suggestions endpoint is tagset-addressed; the local sources re-read the
// appearance's ORIGIN blob (the file whose bytes carried the tags —
// origin_file_id is exactly that provenance), and the P1 service source will
// use the origin file's acoustic fingerprint.

// SuggestSubject is the origin-blob identity + analysis facts behind one
// appearance — what the tag-suggestion sources need. Title/Artist are the
// appearance's CURRENT tags — the text-search seed (tag-suggestions P2);
// TechDuration is ffprobe's duration, the search window's fallback when the
// file was never fingerprinted.
type SuggestSubject struct {
	Hash         string
	MIMEType     string
	Fingerprint  []byte          // packed chromaprint sub-fingerprints; nil when not analyzed
	Duration     sql.NullFloat64 // fingerprinted duration in seconds
	Title        string
	Artist       sql.NullString
	TechDuration sql.NullFloat64 // media_metadata.duration_seconds (ffprobe)
}

// RecodeTagsetsText re-decodes the stored text tags (title / artist /
// album_artist / album / genre / composer / comment) of each appearance with
// recode — the bulk charset fix. recode returns (fixed, true) when the value
// could be reinterpreted (media.ReencodeLatin1 with a charset closed over);
// fields it can't reinterpret, and fields it leaves identical, are untouched,
// so already-correct rows are safe to include. Changed fields go through
// applyMetadataPatchTagsetTx, so identity changes re-resolve the artist/album
// entity FKs. Chunked like BulkUpdateTagsetMetadata; ids outside the scope are
// reported in notFound, not fatal. affected counts appearances with at least
// one changed field.
//
// A valid owner narrows the scope to that user's editable staging (their own
// non-trashed draft/returned appearances) — the My-uploads path, which trusts
// its explicit id list no further than ownership (mirroring
// BulkDiscardOwnUploads). An invalid owner is the unscoped metadata.edit path.
func (db *DB) RecodeTagsetsText(ctx context.Context, tagsetIDs []int64, owner sql.NullInt64, recode func(string) (string, bool)) (affected int, notFound []int64, err error) {
	scope := ""
	if owner.Valid {
		scope = ` AND created_by = ? AND deleted_at IS NULL
			AND review_state IN ('` + ReviewDraft + `','` + ReviewReturned + `')`
	}
	const chunk = 500
	for i := 0; i < len(tagsetIDs); i += chunk {
		batch := tagsetIDs[i:min(i+chunk, len(tagsetIDs))]
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return affected, notFound, fmt.Errorf("recode appearances: begin: %w", err)
		}
		ph := make([]string, len(batch))
		args := make([]any, len(batch))
		for j, id := range batch {
			ph[j] = "?"
			args[j] = id
		}
		if owner.Valid {
			args = append(args, owner.Int64)
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT id, title, artist, album_artist, album, genre, composer, comment
			FROM tagsets WHERE id IN (`+strings.Join(ph, ",")+`)`+scope, args...)
		if err != nil {
			tx.Rollback()
			return affected, notFound, fmt.Errorf("recode appearances: lookup: %w", err)
		}
		type patchRow struct {
			id    int64
			patch MetadataPatch
		}
		var patches []patchRow
		found := make(map[int64]struct{}, len(batch))
		for rows.Next() {
			var id int64
			var title string
			var artist, albumArtist, album, genre, composer, comment sql.NullString
			if err := rows.Scan(&id, &title, &artist, &albumArtist, &album, &genre, &composer, &comment); err != nil {
				rows.Close()
				tx.Rollback()
				return affected, notFound, fmt.Errorf("recode appearances: scan: %w", err)
			}
			found[id] = struct{}{}
			var p MetadataPatch
			set := func(dst **string, cur sql.NullString) {
				if !cur.Valid {
					return
				}
				if out, ok := recode(cur.String); ok && out != cur.String {
					*dst = &out
				}
			}
			if out, ok := recode(title); ok && out != title {
				p.Title = &out
			}
			set(&p.Artist, artist)
			set(&p.AlbumArtist, albumArtist)
			set(&p.Album, album)
			set(&p.Genre, genre)
			set(&p.Composer, composer)
			set(&p.Comment, comment)
			if !p.IsEmpty() {
				patches = append(patches, patchRow{id: id, patch: p})
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			tx.Rollback()
			return affected, notFound, fmt.Errorf("recode appearances: rows: %w", err)
		}
		rows.Close()
		for _, id := range batch {
			if _, ok := found[id]; !ok {
				notFound = append(notFound, id)
			}
		}
		for _, pr := range patches {
			if e := applyMetadataPatchTagsetTx(ctx, tx, pr.id, pr.patch); e != nil {
				tx.Rollback()
				return affected, notFound, e
			}
			affected++
		}
		if err := tx.Commit(); err != nil {
			return affected, notFound, fmt.Errorf("recode appearances: commit: %w", err)
		}
	}
	return affected, notFound, nil
}

// TagsetSuggestSubject loads the suggestion subject for one appearance. found
// is false (no error) on an unknown tagset id.
func (db *DB) TagsetSuggestSubject(ctx context.Context, tagsetID int64) (*SuggestSubject, bool, error) {
	var s SuggestSubject
	err := db.QueryRowContext(ctx, `
		SELECT f.hash, f.mime_type, af.fingerprint, af.duration,
		       t.title, t.artist, mm.duration_seconds
		FROM tagsets t
		JOIN files f ON f.id = t.origin_file_id
		LEFT JOIN audio_fingerprints af ON af.file_id = f.id
		LEFT JOIN media_metadata mm ON mm.file_id = f.id
		WHERE t.id = ?`, tagsetID).
		Scan(&s.Hash, &s.MIMEType, &s.Fingerprint, &s.Duration,
			&s.Title, &s.Artist, &s.TechDuration)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("tagset suggest subject: %w", err)
	}
	return &s, true, nil
}
