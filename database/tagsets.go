package database

// Tagset-rooted query building blocks (recording-tagsets P1). The library's
// listening surfaces list *tagsets* — a track row is an appearance — and every
// appearance plays its recording's ladder-best surviving rendition, resolved
// server-side. The admin/file surfaces stay files-rooted (tagsetJoin, files.go).

// visibleTagset is the predicate every tagset-rooted listing / access query
// must apply (aliased table m): an appearance is publicly visible only when it
// is approved, not trashed, and its recording has at least one surviving
// (non-removed) rendition to play (docs/architecture/recording-tagsets.md,
// Visibility). A recording whose renditions are all removed keeps its tagsets
// but drops out of the library until one is restored.
const visibleTagset = `m.deleted_at IS NULL AND m.review_state = 'approved'
	AND EXISTS (SELECT 1 FROM files sf WHERE sf.recording_id = m.recording_id AND sf.deleted_at IS NULL)`

// recordingJoin binds the recording of the tagset aliased `m` as `r` — the
// alias accessClause/guestAccessibleExpr expect (access lives on the
// recording).
const recordingJoin = `
	JOIN recordings r ON r.id = m.recording_id`

// renditionLadderOrder returns the ORDER BY fragment ranking the files of one
// recording best-first: the manual preferred override, then the quality ladder
// (lossless > lossy > unknown codec; sample-rate/bit-depth for lossless,
// bitrate for lossy; size as the final tiebreak) — the SQL mirror of
// RankRenditions, shared with migration 024's collapse. fileAlias/mmAlias name
// the files / media_metadata aliases; preferredExpr is the recording's
// preferred_file_id expression.
func renditionLadderOrder(fileAlias, mmAlias, preferredExpr string) string {
	return `(` + fileAlias + `.id = COALESCE(` + preferredExpr + `, -1)) DESC,
		CASE
			WHEN lower(COALESCE(` + mmAlias + `.codec, '')) IN ('flac', 'alac') THEN 0
			WHEN lower(COALESCE(` + mmAlias + `.codec, '')) IN ('mp3', 'aac', 'vorbis', 'opus', 'wmav2', 'ac3', 'mp2') THEN 1
			ELSE 2
		END ASC,
		COALESCE(` + mmAlias + `.sample_rate, 0) DESC,
		COALESCE(` + mmAlias + `.bit_depth, 0) DESC,
		COALESCE(` + mmAlias + `.bitrate, 0) DESC,
		` + fileAlias + `.byte_size DESC,
		` + fileAlias + `.id ASC`
}

// bestRenditionJoin binds the serving rendition of the tagset aliased `m` as
// `f`: the recording's ladder-best surviving file (the play-URL resolution —
// every appearance plays the same best blob, whatever rendition it was offered
// from). Requires the recordings join aliased `r` (the preferred override).
// optional=true yields a LEFT JOIN for surfaces that must still list dormant
// appearances (e.g. a trashed playlist item), where f.* read back NULL.
func bestRenditionJoin(optional bool) string {
	kind := "JOIN"
	if optional {
		kind = "LEFT JOIN"
	}
	return `
	` + kind + ` files f ON f.id = (
		SELECT f2.id FROM files f2
		LEFT JOIN media_metadata mm2 ON mm2.file_id = f2.id
		WHERE f2.recording_id = m.recording_id AND f2.deleted_at IS NULL
		ORDER BY ` + renditionLadderOrder("f2", "mm2", "r.preferred_file_id") + `
		LIMIT 1)`
}
