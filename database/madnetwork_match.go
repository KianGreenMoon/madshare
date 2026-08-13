package database

// Federation F8 — the audio-identity join (docs/architecture/federation.md
// §Quality upgrades, "The audio-identity join"). One question, asked by all three
// F8 surfaces: which cached catalog entries are talking about *this* recording?
//
// Two stages, hash then fingerprint head, and never text. Both reuse arithmetic
// this package already performs — the hash join is checkHeldBlobClaims' shape and
// the fingerprint compare is the local resolver's threshold and duration window —
// which is what lets a finding be explained in one sentence: this is the same
// audio by the very standard this node uses to decide that two files are.
//
// Nothing here decides anything, and nothing here calls the network: the join
// reads the cache, so it is cheap enough to sit on a request path.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"daemonlord.ygg/madshare/federation"
	"daemonlord.ygg/madshare/media"
)

// How a match was established. The distinction is evidence, not bookkeeping: a
// hash match is identity (the same bytes), a fingerprint match is a measurement
// with a bit-error rate attached, and every surface that shows one says which.
const (
	MatchHash        = "hash"
	MatchFingerprint = "fingerprint"
)

// NetworkMatch is one cached catalog entry that describes the same audio as a
// local recording, together with the evidence for that claim.
type NetworkMatch struct {
	Source federation.BlobProvider // the node whose catalog carries the entry
	Entry  federation.CatalogEntry // their tags and their renditions
	Match  string                  // MatchHash or MatchFingerprint
	// BER and Words carry the measurement for a fingerprint match (BER 0 and
	// Words 0 for a hash match, where there is nothing to measure).
	BER   float64
	Words int
	// Pinged is the source's freshness CLASS — true when something watches it on
	// the one-minute cadence (F7 item 10). It travels with the match because the
	// window a node is judged by follows whoever observes it, and a caller that
	// greys a stale holder without it would grey every member.
	Pinged bool
	// SharedHash is the blob both sides hold, for a hash match. It is what makes
	// the match self-evident in the UI ("you both have this exact file").
	SharedHash string
}

// srcEntry identifies a cached row: catalogs are per-source, and two sources
// naturally carry the same entry key.
type srcEntry struct {
	sourceID int64
	entryKey string
}

// MatchRecording returns the cached catalog entries that describe recordingID's
// audio, hash matches first. Read-only, no network.
//
// pingedSince is the freshness-hint horizon (MadnetworkView.PingedSince) and is
// used for exactly one thing: classifying each source so a caller can grey a
// stale one. It is NOT a filter, and this function has none — deliberately. A
// match found in a quiet node's catalog is still a true fact about the network,
// and withholding it would repeat the mistake §Availability exists to fix.
// Callers dim a stale source; they do not drop it.
func (db *DB) MatchRecording(ctx context.Context, recordingID, pingedSince int64) ([]NetworkMatch, error) {
	pinged := sourcePinged(MadnetworkView{PingedSince: pingedSince})
	seen := map[srcEntry]bool{}
	matches, err := db.matchByHash(ctx, recordingID, pinged, seen)
	if err != nil {
		return nil, err
	}
	raw, dur, ok, err := db.recordingFingerprint(ctx, recordingID)
	if err != nil {
		return nil, err
	}
	if !ok {
		// No local fingerprint: stage 1 is all we can honestly do. Not an error —
		// an unfingerprinted recording is simply uncheckable beyond its bytes.
		return matches, nil
	}
	wide, err := db.matchByFingerprint(ctx, raw, dur, pinged, seen)
	if err != nil {
		return nil, err
	}
	return append(matches, wide...), nil
}

// matchByHash is stage 1: entries advertising a hash we hold on this recording.
// The join considers only hashes present on both sides, so its cost is the
// overlap between the two libraries rather than the size of either.
func (db *DB) matchByHash(ctx context.Context, recordingID int64, pinged string, seen map[srcEntry]bool) ([]NetworkMatch, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.id, `+peerIDExpr+`, s.public_key, s.label,
		       `+sourceHeardExpr+`, `+srcLastSeen+`, `+pinged+`, f.hash,
		       c.entry_key, c.recording_key, c.title, c.artist, c.album_artist,
		       c.album, COALESCE(c.genre, ''), c.year, c.track_number, c.disc_number,
		       COALESCE(c.duration, 0), COALESCE(c.license, ''), c.guest_playable, c.renditions
		FROM federation_catalog c`+sourceJoin("c")+`
		JOIN json_each(c.renditions) r
		JOIN files f ON f.hash = r.value->>'hash' AND f.deleted_at IS NULL
		WHERE f.recording_id = ? AND `+notBlocked+`
		GROUP BY s.id, c.entry_key
		ORDER BY `+srcLastSeen+` DESC, s.id, c.entry_key`, recordingID)
	if err != nil {
		return nil, fmt.Errorf("match recording by hash: %w", err)
	}
	defer rows.Close()

	var out []NetworkMatch
	for rows.Next() {
		m := NetworkMatch{Match: MatchHash}
		if err := scanMatchRow(rows, &m, &m.SharedHash); err != nil {
			return nil, err
		}
		if m.Entry.Key == "" {
			continue // a damaged cache row; scanMatchRow already skipped it
		}
		seen[srcEntry{m.Source.SourceID, m.Entry.Key}] = true
		out = append(out, m)
	}
	return out, rows.Err()
}

// matchByFingerprint is stage 2: entries no hash matched, whose advertised
// fingerprint head measures as the same audio as ours. This is the stage that
// finds a *re-encode*, and a re-encode is where a better rendition lives — a node
// holding our exact bytes by definition holds nothing better than them.
func (db *DB) matchByFingerprint(ctx context.Context, raw []uint32, dur float64, pinged string, seen map[srcEntry]bool) ([]NetworkMatch, error) {
	// The duration shortlist is the same one the local resolver uses, and it is
	// the only thing bounding this scan. Entries whose origin never ran ffprobe
	// advertise no duration and cannot be shortlisted — they stay in, because a
	// missed upgrade costs more than a fingerprint compare, which is 2048 bits.
	rows, err := db.QueryContext(ctx, `
		SELECT s.id, `+peerIDExpr+`, s.public_key, s.label,
		       `+sourceHeardExpr+`, `+srcLastSeen+`, `+pinged+`, '',
		       c.entry_key, c.recording_key, c.title, c.artist, c.album_artist,
		       c.album, COALESCE(c.genre, ''), c.year, c.track_number, c.disc_number,
		       COALESCE(c.duration, 0), COALESCE(c.license, ''), c.guest_playable, c.renditions
		FROM federation_catalog c`+sourceJoin("c")+`
		WHERE `+notBlocked+`
		  AND (c.duration IS NULL OR c.duration <= 0 OR ABS(c.duration - ?) <= ?)
		  AND c.renditions LIKE '%"fingerprint"%'
		ORDER BY `+srcLastSeen+` DESC, s.id, c.entry_key`,
		dur, recordingDurationTolerance)
	if err != nil {
		return nil, fmt.Errorf("match recording by fingerprint: %w", err)
	}
	defer rows.Close()

	var out []NetworkMatch
	for rows.Next() {
		var m NetworkMatch
		var unused string
		if err := scanMatchRow(rows, &m, &unused); err != nil {
			return nil, err
		}
		if m.Entry.Key == "" || seen[srcEntry{m.Source.SourceID, m.Entry.Key}] {
			continue
		}
		// An entry can carry several renditions; the best measurement among them
		// is the entry's, because they are all claimed to be the same recording.
		best, bestWords, found := 0.0, 0, false
		for i := range m.Entry.Renditions {
			ber, words, ok := compareClaim(raw, m.Entry.Renditions[i].Fingerprint)
			if !ok || ber > maxBitErrorRate {
				continue
			}
			if !found || ber < best {
				best, bestWords, found = ber, words, true
			}
		}
		if !found {
			continue
		}
		m.Match, m.BER, m.Words = MatchFingerprint, best, bestWords
		seen[srcEntry{m.Source.SourceID, m.Entry.Key}] = true
		out = append(out, m)
	}
	return out, rows.Err()
}

// compareClaim measures one advertised fingerprint head against our own words.
// ok is false when the claim is absent or malformed — uncheckable, which is not
// the same as contradicted (the F6 rule, and it holds here too).
func compareClaim(ours []uint32, claim *federation.FingerprintClaim) (float64, int, bool) {
	if claim == nil || claim.Head == "" {
		return 0, 0, false
	}
	theirs, err := base64.StdEncoding.DecodeString(claim.Head)
	if err != nil {
		return 0, 0, false
	}
	return compareHeads(ours, media.DecodeFingerprint(theirs))
}

// scanMatchRow reads one cached catalog row into a match. A row whose renditions
// JSON will not parse leaves Entry.Key empty rather than failing the whole query:
// one damaged cache row must not cost a moderator the rest of the answer.
func scanMatchRow(rows *sql.Rows, m *NetworkMatch, hash *string) error {
	var year, track, disc sql.NullInt64
	var renditions string
	if err := rows.Scan(&m.Source.SourceID, &m.Source.PeerID, &m.Source.PublicKey,
		&m.Source.Name, &m.Source.HeardName, &m.Source.LastSeen, &m.Pinged, hash,
		&m.Entry.Key, &m.Entry.RecordingKey, &m.Entry.Title, &m.Entry.Artist,
		&m.Entry.AlbumArtist, &m.Entry.Album, &m.Entry.Genre, &year, &track, &disc,
		&m.Entry.Duration, &m.Entry.License, &m.Entry.GuestPlayable, &renditions); err != nil {
		return fmt.Errorf("scan match row: %w", err)
	}
	m.Entry.Year, m.Entry.TrackNumber, m.Entry.DiscNumber = nullInt(year), nullInt(track), nullInt(disc)
	if err := json.Unmarshal([]byte(renditions), &m.Entry.Renditions); err != nil {
		m.Entry.Key = ""
	}
	return nil
}

// recordingFingerprint returns the fingerprint of one of the recording's live
// renditions. Any of them will do — they are grouped precisely because they
// measure as the same audio — so the oldest file id is chosen for stability
// across calls rather than for quality.
func (db *DB) recordingFingerprint(ctx context.Context, recordingID int64) (raw []uint32, dur float64, found bool, err error) {
	var fileID int64
	err = db.QueryRowContext(ctx, `
		SELECT f.id FROM files f
		JOIN audio_fingerprints af ON af.file_id = f.id
		WHERE f.recording_id = ? AND f.deleted_at IS NULL
		ORDER BY f.id LIMIT 1`, recordingID).Scan(&fileID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, fmt.Errorf("recording fingerprint: %w", err)
	}
	return db.fileFingerprint(ctx, fileID)
}
