package database

// Federation F8 item 3 — the quality-upgrade scan (docs/architecture/federation.md
// §Quality upgrades, "The upgrade scan").
//
// The synced catalogs already say which renditions other nodes hold. This asks
// the only question that makes that useful to the local library: is any of them
// BETTER than what we have of the same recording? It runs on every catalog sync,
// beside checkClaims and for the same reason — their catalog stands still while
// our library moves — and it is bounded so that cadence stays affordable:
//
//   - stage 1 (shared hash) is a join, so its cost is the OVERLAP between two
//     libraries rather than the size of either. It runs whole, every time.
//   - stage 2 (fingerprint) is the expensive half, and runs only over material
//     newer than the source's watermark on either side. Local fingerprint heads
//     are bucketed by duration first, so a remote entry is compared against the
//     handful of local recordings that could possibly be it, not against all of
//     them.
//
// Everything a finding says about remote bytes is the origin's CLAIM. It becomes
// a fact when the bytes arrive and the analysis pipeline re-derives it here.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"daemonlord.ygg/madshare/federation"
	"daemonlord.ygg/madshare/media"
)

// Upgrade dispositions. Detection writes measurements and never a disposition:
// an admin who dismissed a finding is not asked again on the next sync.
const (
	UpgradeNew          = "new"
	UpgradeDismissed    = "dismissed"
	UpgradeMaterialized = "materialized"
)

// upgradeCandidate is one remote rendition proposed as an upgrade to one local
// recording, before the quality ladder has been consulted.
type upgradeCandidate struct {
	recordingID int64
	entryKey    string
	match       string
	ber         float64
	rd          federation.CatalogRendition
}

type candKey struct {
	recordingID int64
	hash        string
}

// localHead is one local rendition's fingerprint head, kept in memory for the
// duration of a scan. A head is 64 words — 256 bytes — so a ten-thousand-track
// library costs a couple of megabytes to hold, which is what makes comparing
// every remote entry against the whole library affordable at all.
type localHead struct {
	recordingID int64
	words       []uint32
	duration    float64
}

// ScanSourceUpgrades compares one source's cached catalog against the local
// library and records the renditions that would beat ours. Returns the number of
// findings awaiting a decision afterwards — what a badge counts.
func (db *DB) ScanSourceUpgrades(ctx context.Context, sourceID, now int64) (int, error) {
	var watermark int64
	if err := db.QueryRowContext(ctx,
		`SELECT upgrade_scanned_at FROM federation_catalog_sources WHERE id = ?`, sourceID,
	).Scan(&watermark); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil // the source went away mid-round; nothing to scan
		}
		return 0, fmt.Errorf("upgrade scan: watermark: %w", err)
	}

	cands := map[candKey]upgradeCandidate{}
	if err := db.scanUpgradesByHash(ctx, sourceID, cands); err != nil {
		return 0, err
	}
	if err := db.scanUpgradesByFingerprint(ctx, sourceID, watermark, cands); err != nil {
		return 0, err
	}
	if err := db.recordUpgrades(ctx, sourceID, now, cands); err != nil {
		return 0, err
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE federation_catalog_sources SET upgrade_scanned_at = ? WHERE id = ?`, now, sourceID,
	); err != nil {
		return 0, fmt.Errorf("upgrade scan: watermark update: %w", err)
	}
	return db.CountOpenUpgrades(ctx)
}

// scanUpgradesByHash is stage 1: for every cached entry advertising a blob we
// hold, that entry's OTHER renditions are renditions of the same recording — by
// the origin's own grouping, which is the same claim its catalog already makes.
func (db *DB) scanUpgradesByHash(ctx context.Context, sourceID int64, cands map[candKey]upgradeCandidate) error {
	rows, err := db.QueryContext(ctx, `
		SELECT c.entry_key, c.renditions, f.recording_id
		FROM federation_catalog c
		JOIN json_each(c.renditions) r
		JOIN files f ON f.hash = r.value->>'hash' AND f.deleted_at IS NULL
		WHERE c.source_id = ?
		GROUP BY c.entry_key, f.recording_id`, sourceID)
	if err != nil {
		return fmt.Errorf("upgrade scan: hash stage: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var entryKey, renditions string
		var recordingID int64
		if err := rows.Scan(&entryKey, &renditions, &recordingID); err != nil {
			return fmt.Errorf("upgrade scan: scan hash row: %w", err)
		}
		var rds []federation.CatalogRendition
		if json.Unmarshal([]byte(renditions), &rds) != nil {
			continue // a damaged cache row is skipped, not fatal
		}
		for _, rd := range rds {
			addCandidate(cands, upgradeCandidate{
				recordingID: recordingID, entryKey: entryKey, match: MatchHash, rd: rd,
			})
		}
	}
	return rows.Err()
}

// scanUpgradesByFingerprint is stage 2, and the incremental bound lives here.
// On a first scan (watermark 0) it is one full pass. Afterwards it is two
// narrow ones — new remote rows against the whole library, and the whole
// catalog against locally-new fingerprints — because every other pairing was
// already compared on an earlier round and its answer has not changed.
func (db *DB) scanUpgradesByFingerprint(ctx context.Context, sourceID, watermark int64, cands map[candKey]upgradeCandidate) error {
	if watermark == 0 {
		heads, err := db.localFingerprintHeads(ctx, 0)
		if err != nil || len(heads) == 0 {
			return err
		}
		return db.compareCatalogAgainst(ctx, sourceID, 0, heads, cands)
	}
	// (a) rows this source published since we last looked, against everything.
	heads, err := db.localFingerprintHeads(ctx, 0)
	if err != nil {
		return err
	}
	if len(heads) > 0 {
		if err := db.compareCatalogAgainst(ctx, sourceID, watermark, heads, cands); err != nil {
			return err
		}
	}
	// (b) everything they hold, against recordings fingerprinted since.
	fresh, err := db.localFingerprintHeads(ctx, watermark)
	if err != nil || len(fresh) == 0 {
		return err
	}
	return db.compareCatalogAgainst(ctx, sourceID, 0, fresh, cands)
}

// localFingerprintHeads loads the head of every live local fingerprint, bucketed
// by whole seconds of duration. since > 0 narrows it to fingerprints written
// after that moment.
func (db *DB) localFingerprintHeads(ctx context.Context, since int64) (map[int64][]localHead, error) {
	query := `
		SELECT f.recording_id, SUBSTR(af.fingerprint, 1, ?), COALESCE(af.duration, 0)
		FROM files f
		JOIN audio_fingerprints af ON af.file_id = f.id
		WHERE f.deleted_at IS NULL`
	args := []any{federation.ClaimHeadWords * 4}
	if since > 0 {
		query += ` AND af.created_at >= ?`
		args = append(args, since)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("upgrade scan: local heads: %w", err)
	}
	defer rows.Close()
	out := map[int64][]localHead{}
	for rows.Next() {
		var h localHead
		var blob []byte
		if err := rows.Scan(&h.recordingID, &blob, &h.duration); err != nil {
			return nil, fmt.Errorf("upgrade scan: scan local head: %w", err)
		}
		h.words = media.DecodeFingerprint(blob)
		if len(h.words) == 0 {
			continue
		}
		out[durationBucket(h.duration)] = append(out[durationBucket(h.duration)], h)
	}
	return out, rows.Err()
}

// durationBucket keys the shortlist. Whole seconds, so a lookup walks the
// buckets within the resolver's own tolerance rather than the whole library.
func durationBucket(d float64) int64 { return int64(math.Round(d)) }

// compareCatalogAgainst streams a source's cached entries and measures each
// advertised fingerprint against the local heads that could plausibly be the
// same audio. since > 0 restricts it to entries new to us since then.
func (db *DB) compareCatalogAgainst(ctx context.Context, sourceID, since int64,
	heads map[int64][]localHead, cands map[candKey]upgradeCandidate) error {
	query := `
		SELECT c.entry_key, COALESCE(c.duration, 0), c.renditions
		FROM federation_catalog c
		WHERE c.source_id = ? AND c.renditions LIKE '%"fingerprint"%'`
	args := []any{sourceID}
	if since > 0 {
		query += ` AND c.first_seen >= ?`
		args = append(args, since)
	}
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("upgrade scan: fingerprint stage: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var entryKey, renditions string
		var duration float64
		if err := rows.Scan(&entryKey, &duration, &renditions); err != nil {
			return fmt.Errorf("upgrade scan: scan catalog row: %w", err)
		}
		var rds []federation.CatalogRendition
		if json.Unmarshal([]byte(renditions), &rds) != nil {
			continue
		}
		for _, rd := range rds {
			d := duration
			if d <= 0 {
				d = rd.Duration
			}
			rec, ber, ok := matchHeadToLibrary(heads, rd.Fingerprint, d)
			if !ok {
				continue
			}
			addCandidate(cands, upgradeCandidate{
				recordingID: rec, entryKey: entryKey, match: MatchFingerprint, ber: ber, rd: rd,
			})
		}
	}
	return rows.Err()
}

// matchHeadToLibrary finds the local recording an advertised fingerprint head
// belongs to: the best measurement within the grouping threshold, over the
// duration buckets inside the resolver's tolerance. An entry that advertises no
// duration is compared against every bucket — uncheckable duration is not a
// reason to skip checkable audio.
func matchHeadToLibrary(heads map[int64][]localHead, claim *federation.FingerprintClaim, duration float64) (int64, float64, bool) {
	words, ok := decodeClaimHead(claim)
	if !ok {
		return 0, 0, false
	}
	best, bestRec, found := math.MaxFloat64, int64(0), false
	consider := func(hs []localHead) {
		for _, h := range hs {
			ber, _, ok := compareHeads(h.words, words)
			if !ok || ber > maxBitErrorRate || ber >= best {
				continue
			}
			best, bestRec, found = ber, h.recordingID, true
		}
	}
	if duration <= 0 {
		for _, hs := range heads {
			consider(hs)
		}
		return bestRec, best, found
	}
	centre := durationBucket(duration)
	span := int64(math.Ceil(recordingDurationTolerance))
	for b := centre - span; b <= centre+span; b++ {
		consider(heads[b])
	}
	return bestRec, best, found
}

// addCandidate keeps the strongest evidence per (recording, remote blob): a hash
// match is identity and outranks any measurement, and among measurements the
// lowest bit-error rate wins.
func addCandidate(cands map[candKey]upgradeCandidate, c upgradeCandidate) {
	if c.rd.Hash == "" || c.recordingID == 0 {
		return
	}
	k := candKey{c.recordingID, c.rd.Hash}
	prev, ok := cands[k]
	if !ok || (prev.match == MatchFingerprint && (c.match == MatchHash || c.ber < prev.ber)) {
		cands[k] = c
	}
}

// recordUpgrades consults the quality ladder and writes what survives it. The
// comparison is RankRenditions itself rather than a reimplementation of "better",
// so this page can never disagree with the review card or the recordings lens
// about which of two renditions wins.
func (db *DB) recordUpgrades(ctx context.Context, sourceID, now int64, cands map[candKey]upgradeCandidate) error {
	if len(cands) == 0 {
		return nil
	}
	recIDs := map[int64]bool{}
	for k := range cands {
		recIDs[k.recordingID] = true
	}
	ours := map[int64][]Rendition{}
	for id := range recIDs {
		rs, err := db.recordingRenditions(ctx, id)
		if err != nil {
			return err
		}
		ours[id] = RankRenditions(rs)
	}

	for k, c := range cands {
		mine := ours[k.recordingID]
		if len(mine) == 0 {
			continue // a recording with no live rendition is dormant, not upgradable
		}
		if holdsHash(mine, k.hash) {
			continue // we already have these exact bytes
		}
		best := mine[0]
		claimed := Rendition{
			Hash: c.rd.Hash, Codec: c.rd.Codec, Bitrate: int(c.rd.Bitrate),
			SampleRate: int(c.rd.SampleRate), BitDepth: int(c.rd.BitDepth), ByteSize: c.rd.Size,
		}
		if RankRenditions([]Rendition{best, claimed})[0].Hash != claimed.Hash {
			continue // not an upgrade by our own ladder
		}
		if err := db.upsertUpgrade(ctx, sourceID, now, best, c, claimed); err != nil {
			return err
		}
	}
	return nil
}

func holdsHash(rs []Rendition, hash string) bool {
	for _, r := range rs {
		if r.Hash == hash {
			return true
		}
	}
	return false
}

// upsertUpgrade writes a finding. An existing row keeps its DISPOSITION and only
// its measurement, its offering source and last_seen move — the same rule
// recordClaimReports follows, and the reason a dismissal survives a rescan.
func (db *DB) upsertUpgrade(ctx context.Context, sourceID, now int64, best Rendition, c upgradeCandidate, claimed Rendition) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO library_upgrades
		    (recording_id, remote_hash, source_id, entry_key, match, ber, our_file_id,
		     codec, bitrate, sample_rate, bit_depth, byte_size, disposition, first_seen, last_seen)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (recording_id, remote_hash) DO UPDATE SET
		    source_id   = excluded.source_id,
		    entry_key   = excluded.entry_key,
		    match       = excluded.match,
		    ber         = excluded.ber,
		    our_file_id = excluded.our_file_id,
		    codec       = excluded.codec,
		    bitrate     = excluded.bitrate,
		    sample_rate = excluded.sample_rate,
		    bit_depth   = excluded.bit_depth,
		    byte_size   = excluded.byte_size,
		    last_seen   = excluded.last_seen`,
		c.recordingID, c.rd.Hash, sourceID, c.entryKey, c.match, c.ber, best.FileID,
		claimed.Codec, claimed.Bitrate, claimed.SampleRate, claimed.BitDepth, claimed.ByteSize,
		UpgradeNew, now, now)
	if err != nil {
		return fmt.Errorf("upgrade scan: record finding: %w", err)
	}
	return nil
}

// SweepUpgrades drops findings that are no longer true: the remote blob has left
// every cached catalog, or we hold those bytes now (the upgrade happened). Both
// are silent — an upgrade that resolved itself is not news, and a finding for a
// blob nobody advertises any more is a dead link.
func (db *DB) SweepUpgrades(ctx context.Context) error {
	_, err := db.ExecContext(ctx, `
		DELETE FROM library_upgrades
		WHERE remote_hash IN (SELECT hash FROM files WHERE deleted_at IS NULL)
		   OR NOT EXISTS (
		        SELECT 1 FROM federation_catalog c, json_each(c.renditions) r
		         WHERE r.value->>'hash' = library_upgrades.remote_hash)`)
	if err != nil {
		return fmt.Errorf("sweep upgrades: %w", err)
	}
	return nil
}

// CountOpenUpgrades is the badge count: findings nobody has decided about.
func (db *DB) CountOpenUpgrades(ctx context.Context) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM library_upgrades WHERE disposition = ?`, UpgradeNew).Scan(&n); err != nil {
		return 0, fmt.Errorf("count open upgrades: %w", err)
	}
	return n, nil
}

// decodeClaimHead unpacks an advertised fingerprint head. Absent or malformed is
// uncheckable, which is not the same as wrong.
func decodeClaimHead(claim *federation.FingerprintClaim) ([]uint32, bool) {
	if claim == nil || claim.Head == "" {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(claim.Head)
	if err != nil {
		return nil, false
	}
	words := media.DecodeFingerprint(raw)
	return words, len(words) > 0
}

// ── Reading findings (the /admin/upgrades page) ──────────────────────────────

// UpgradeRow is one finding, joined to everything the page shows: what we hold,
// what is on offer, and which node is offering it.
type UpgradeRow struct {
	ID          int64   `json:"id"`
	RecordingID int64   `json:"recording_id"`
	Title       string  `json:"title"`
	Artist      string  `json:"artist"`
	RemoteHash  string  `json:"remote_hash"`
	Match       string  `json:"match"`
	BER         float64 `json:"ber,omitempty"`
	Disposition string  `json:"disposition"`
	FirstSeen   int64   `json:"first_seen"`
	LastSeen    int64   `json:"last_seen"`

	// Ours is the rendition on offer would replace as ladder-best; Offered is
	// the claim. Both carry the same tech fields so the page can put them in one
	// table, and Offered is claimed until the bytes arrive.
	Ours    Rendition `json:"ours"`
	Offered Rendition `json:"offered"`

	// Source names a node currently advertising it, greyed when stale by the
	// caller. Empty when the source row is gone — which costs nothing, since the
	// swarm finds holders by hash.
	Source       string `json:"source,omitempty"`
	SourceKey    string `json:"source_key,omitempty"`
	SourceSeen   int64  `json:"source_last_seen,omitempty"`
	SourcePinged bool   `json:"source_pinged,omitempty"`
}

// ListUpgrades returns findings newest-first. disposition "" means open ones
// (the default view); "all" means every row.
func (db *DB) ListUpgrades(ctx context.Context, disposition string, pingedSince int64, limit, offset int) ([]*UpgradeRow, int, error) {
	where, args := "", []any{}
	switch disposition {
	case "", UpgradeNew:
		where, args = " WHERE u.disposition = ?", []any{UpgradeNew}
	case "all":
	default:
		where, args = " WHERE u.disposition = ?", []any{disposition}
	}

	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM library_upgrades u`+where, args...).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("list upgrades: count: %w", err)
	}

	query := `
		SELECT u.id, u.recording_id, COALESCE(pt.title, ''),
		       COALESCE(NULLIF(pt.album_artist, ''), NULLIF(pt.artist, ''), ''),
		       u.remote_hash, u.match, u.ber, u.disposition, u.first_seen, u.last_seen,
		       u.codec, u.bitrate, u.sample_rate, u.bit_depth, u.byte_size,
		       COALESCE(u.our_file_id, 0), COALESCE(of.hash, ''), COALESCE(omm.codec, ''),
		       COALESCE(omm.bitrate, 0), COALESCE(omm.sample_rate, 0), COALESCE(omm.bit_depth, 0),
		       COALESCE(of.byte_size, 0),
		       COALESCE(` + sourceLabelExpr + `, ''), COALESCE(s.public_key, ''),
		       COALESCE(` + srcLastSeen + `, 0),
		       -- COALESCE around the ping class, unlike the browse queries: those
		       -- INNER JOIN the source, this one LEFTs it (a finding outlives the
		       -- source row that reported it), and ` + "`NULL >= n`" + ` makes the whole
		       -- OR null rather than false.
		       COALESCE(` + sourcePinged(MadnetworkView{PingedSince: pingedSince}) + `, 0)
		  FROM library_upgrades u
		  LEFT JOIN tagsets pt ON pt.id = (
		       SELECT t2.id FROM tagsets t2 WHERE t2.recording_id = u.recording_id
		        ORDER BY (t2.deleted_at IS NULL) DESC, t2.is_primary DESC, t2.id ASC LIMIT 1)
		  LEFT JOIN files of ON of.id = u.our_file_id
		  LEFT JOIN media_metadata omm ON omm.file_id = of.id
		  LEFT JOIN federation_catalog_sources s ON s.id = u.source_id
		  LEFT JOIN federation_peers p ON p.public_key = s.public_key` + where + `
		 ORDER BY u.last_seen DESC, u.id DESC
		 LIMIT ? OFFSET ?`
	rows, err := db.QueryContext(ctx, query, append(append([]any{}, args...), limit, max(offset, 0))...)
	if err != nil {
		return nil, 0, fmt.Errorf("list upgrades: %w", err)
	}
	defer rows.Close()

	var out []*UpgradeRow
	for rows.Next() {
		u := &UpgradeRow{}
		if err := rows.Scan(&u.ID, &u.RecordingID, &u.Title, &u.Artist,
			&u.RemoteHash, &u.Match, &u.BER, &u.Disposition, &u.FirstSeen, &u.LastSeen,
			&u.Offered.Codec, &u.Offered.Bitrate, &u.Offered.SampleRate, &u.Offered.BitDepth, &u.Offered.ByteSize,
			&u.Ours.FileID, &u.Ours.Hash, &u.Ours.Codec, &u.Ours.Bitrate, &u.Ours.SampleRate,
			&u.Ours.BitDepth, &u.Ours.ByteSize,
			&u.Source, &u.SourceKey, &u.SourceSeen, &u.SourcePinged); err != nil {
			return nil, 0, fmt.Errorf("list upgrades: scan: %w", err)
		}
		u.Offered.Hash = u.RemoteHash
		out = append(out, u)
	}
	return out, total, rows.Err()
}

// SetUpgradeDisposition records an admin's decision. found is false for an
// unknown id — the caller answers 404 rather than inventing a row.
func (db *DB) SetUpgradeDisposition(ctx context.Context, id int64, disposition string) (bool, error) {
	switch disposition {
	case UpgradeNew, UpgradeDismissed, UpgradeMaterialized:
	default:
		return false, fmt.Errorf("unknown upgrade disposition %q", disposition)
	}
	res, err := db.ExecContext(ctx,
		`UPDATE library_upgrades SET disposition = ? WHERE id = ?`, disposition, id)
	if err != nil {
		return false, fmt.Errorf("set upgrade disposition: %w", err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// UpgradeHash reads a finding's remote hash — what the page's Materialize action
// hands to the ordinary download path. found is false for an unknown id.
func (db *DB) UpgradeHash(ctx context.Context, id int64) (string, bool, error) {
	var hash string
	err := db.QueryRowContext(ctx, `SELECT remote_hash FROM library_upgrades WHERE id = ?`, id).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("upgrade hash: %w", err)
	}
	return hash, true, nil
}
