package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"daemonlord.ygg/madshare/federation"
)

// Federation F2 — the catalog (docs/architecture/federation.md §Catalog):
// what this node publishes to friends (PublishedCatalog), the per-peer cached
// copies pulled from them (ReplacePeerCatalog), and the merged browse queries
// behind /api/madnetwork/* (friends only — a blocked peer's cache is kept but
// hidden). *DB satisfies the catalog half of federation.PeerStore here.

// PublishedCatalog builds this node's own catalog *for one audience*: every
// approved, live appearance (the visibleTagset predicate — exactly what the
// local library shows) that the audience's scope admits (F5: share depth, and
// the guest-playable policy for a guest-only audience), with resolved display
// names and its recording's live renditions. Ordered by tagset id so the
// snapshot serial is deterministic.
//
// The audience is a parameter rather than a post-filter because the serial must
// hash the snapshot the peer actually receives: two audiences legitimately have
// two serials, and the node memoizes one snapshot per audience class.
func (db *DB) PublishedCatalog(ctx context.Context, aud federation.Audience) ([]federation.CatalogEntry, error) {
	defaultDepth, err := db.nodeDefaultDepth(ctx)
	if err != nil {
		return nil, err
	}
	scope := audienceClause(aud)
	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.recording_id, m.title,
		       COALESCE(par.name, m.artist, ''),
		       COALESCE(aar.name, m.album_artist, ''),
		       COALESCE(al.title, m.album, ''),
		       COALESCE(m.genre, ''), m.year, m.track_number, m.disc_number,
		       COALESCE(r.license, ''), r.guest_playable
		FROM tagsets m`+recordingJoin+`
		LEFT JOIN artists par ON par.id = m.artist_id
		LEFT JOIN artists aar ON aar.id = m.album_artist_id
		LEFT JOIN albums al   ON al.id  = m.album_id
		WHERE `+visibleTagset+`
		  AND (`+scope+`)
		ORDER BY m.id`, scopeArgs(defaultDepth, aud)...)
	if err != nil {
		return nil, fmt.Errorf("published catalog: %w", err)
	}
	defer rows.Close()

	var entries []federation.CatalogEntry
	recordings := map[string][]int{} // recording key -> entry indexes awaiting renditions
	for rows.Next() {
		var e federation.CatalogEntry
		var tagsetID, recordingID int64
		var year, track, disc sql.NullInt64
		if err := rows.Scan(&tagsetID, &recordingID, &e.Title, &e.Artist, &e.AlbumArtist,
			&e.Album, &e.Genre, &year, &track, &disc, &e.License, &e.GuestPlayable); err != nil {
			return nil, fmt.Errorf("scan catalog entry: %w", err)
		}
		e.Key = strconv.FormatInt(tagsetID, 10)
		e.RecordingKey = strconv.FormatInt(recordingID, 10)
		e.Year, e.TrackNumber, e.DiscNumber = nullInt(year), nullInt(track), nullInt(disc)
		e.Renditions = []federation.CatalogRendition{}
		recordings[e.RecordingKey] = append(recordings[e.RecordingKey], len(entries))
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("published catalog rows: %w", err)
	}
	if len(entries) == 0 {
		return []federation.CatalogEntry{}, nil
	}

	// Attach each recording's live renditions (hash = the swarm id, plus the
	// quality facts the ladder ranks by). Scoped by the same audience clause as
	// the entries above: only entries in `recordings` receive renditions anyway,
	// so this is not what makes the result correct — it is what keeps the two
	// queries obviously the same rule, and what stops an out-of-scope recording's
	// hashes from being read at all.
	rrows, err := db.QueryContext(ctx, `
		SELECT f.recording_id, f.hash, f.byte_size,
		       COALESCE(mm.codec, ''), COALESCE(mm.bitrate, 0),
		       COALESCE(mm.sample_rate, 0), COALESCE(mm.bit_depth, 0),
		       COALESCE(mm.duration_seconds, 0)
		FROM files f
		JOIN recordings r ON r.id = f.recording_id
		LEFT JOIN media_metadata mm ON mm.file_id = f.id
		WHERE f.deleted_at IS NULL AND (`+scope+`) AND EXISTS (
			SELECT 1 FROM tagsets m WHERE m.recording_id = f.recording_id AND `+visibleTagset+`)
		ORDER BY f.recording_id, f.id`, scopeArgs(defaultDepth, aud)...)
	if err != nil {
		return nil, fmt.Errorf("catalog renditions: %w", err)
	}
	defer rrows.Close()
	for rrows.Next() {
		var recordingID int64
		var rd federation.CatalogRendition
		if err := rrows.Scan(&recordingID, &rd.Hash, &rd.Size, &rd.Codec,
			&rd.Bitrate, &rd.SampleRate, &rd.BitDepth, &rd.Duration); err != nil {
			return nil, fmt.Errorf("scan catalog rendition: %w", err)
		}
		for _, i := range recordings[strconv.FormatInt(recordingID, 10)] {
			entries[i].Renditions = append(entries[i].Renditions, rd)
			if entries[i].Duration == 0 && rd.Duration > 0 {
				entries[i].Duration = rd.Duration
			}
		}
	}
	return entries, rrows.Err()
}

// ReplacePeerCatalog atomically replaces the cached copy of one friend's
// catalog with a fresh snapshot and records the snapshot serial + sync time.
func (db *DB) ReplacePeerCatalog(ctx context.Context, peerID int64, serial string, syncedAt int64, entries []federation.CatalogEntry) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace peer catalog: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM federation_catalog WHERE peer_id = ?`, peerID); err != nil {
		return fmt.Errorf("clear peer catalog: %w", err)
	}
	ins, err := tx.PrepareContext(ctx, `
		INSERT INTO federation_catalog (peer_id, entry_key, recording_key, title, artist,
			album_artist, album, genre, year, track_number, disc_number, duration,
			license, guest_playable, renditions)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare peer catalog insert: %w", err)
	}
	defer ins.Close()
	for _, e := range entries {
		if e.Key == "" || e.Title == "" {
			continue // remote input — skip rows that cannot be displayed or re-keyed
		}
		renditions, err := json.Marshal(e.Renditions)
		if err != nil {
			return fmt.Errorf("marshal renditions: %w", err)
		}
		if _, err := ins.ExecContext(ctx, peerID, e.Key, e.RecordingKey, e.Title, e.Artist,
			e.AlbumArtist, e.Album, e.Genre, e.Year, e.TrackNumber, e.DiscNumber,
			nullFloat(e.Duration), e.License, e.GuestPlayable, string(renditions)); err != nil {
			return fmt.Errorf("insert peer catalog entry: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE federation_peers SET catalog_serial = ?, catalog_synced_at = ? WHERE id = ?`,
		serial, syncedAt, peerID); err != nil {
		return fmt.Errorf("update peer sync state: %w", err)
	}
	return tx.Commit()
}

// MarkPeerCatalogChecked records a sync round that confirmed the cached copy
// is still fresh (the not-modified path).
func (db *DB) MarkPeerCatalogChecked(ctx context.Context, peerID int64, serial string, syncedAt int64) error {
	_, err := db.ExecContext(ctx, `
		UPDATE federation_peers SET catalog_serial = ?, catalog_synced_at = ? WHERE id = ?`,
		serial, syncedAt, peerID)
	if err != nil {
		return fmt.Errorf("mark peer catalog checked: %w", err)
	}
	return nil
}

// ReplacePeerHoldings atomically replaces the cached list of what one friend
// holds in its download cache and will seed (federation F4 holdings tracker,
// GET /madnetwork/v0/holdings). Invalid entries are skipped; duplicates collapse
// on the composite primary key.
func (db *DB) ReplacePeerHoldings(ctx context.Context, peerID int64, hashes []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace peer holdings: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM federation_holdings WHERE peer_id = ?`, peerID); err != nil {
		return fmt.Errorf("clear peer holdings: %w", err)
	}
	ins, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO federation_holdings (peer_id, hash) VALUES (?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare holdings insert: %w", err)
	}
	defer ins.Close()
	for _, h := range hashes {
		if !isContentHash(h) {
			continue // remote input — a content hash is 64 lowercase hex
		}
		if _, err := ins.ExecContext(ctx, peerID, h); err != nil {
			return fmt.Errorf("insert peer holding: %w", err)
		}
	}
	return tx.Commit()
}

// isContentHash reports whether s is a well-formed content hash (64 lowercase
// hex) — the same shape federation.isBlobHash enforces, kept local so the
// database package carries no build-tagged dependency.
func isContentHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// ── Blob lookup (federation F3/F4) ───────────────────────────────────────────

// madnetworkRowsForHash scans friends' cached rows whose renditions advertise
// hash, most-recently-seen friend first. The LIKE is a cheap pre-filter (a
// content hash is plain hex — no LIKE metacharacters); the JSON parse confirms.
func (db *DB) madnetworkRowsForHash(ctx context.Context, hash string,
	visit func(peer *federation.Peer, entry *federation.CatalogEntry, rendition *federation.CatalogRendition) bool) error {
	rows, err := db.QueryContext(ctx, `
		SELECT p.id, p.public_key, p.name, p.heard_name, p.last_seen,
		       c.entry_key, c.recording_key, c.title, c.artist, c.album_artist,
		       c.album, COALESCE(c.genre, ''), c.year, c.track_number, c.disc_number,
		       COALESCE(c.duration, 0), COALESCE(c.license, ''), c.guest_playable, c.renditions
		FROM federation_catalog c
		JOIN federation_peers p ON p.id = c.peer_id AND p.state = 'friend'
		WHERE c.renditions LIKE ?
		ORDER BY p.last_seen DESC, p.id, c.entry_key`, "%"+hash+"%")
	if err != nil {
		return fmt.Errorf("madnetwork rows for hash: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p federation.Peer
		var e federation.CatalogEntry
		var year, track, disc sql.NullInt64
		var renditions string
		if err := rows.Scan(&p.ID, &p.PublicKey, &p.Name, &p.HeardName, &p.LastSeen,
			&e.Key, &e.RecordingKey, &e.Title, &e.Artist, &e.AlbumArtist, &e.Album,
			&e.Genre, &year, &track, &disc, &e.Duration, &e.License, &e.GuestPlayable,
			&renditions); err != nil {
			return fmt.Errorf("scan madnetwork hash row: %w", err)
		}
		e.Year, e.TrackNumber, e.DiscNumber = nullInt(year), nullInt(track), nullInt(disc)
		if err := json.Unmarshal([]byte(renditions), &e.Renditions); err != nil {
			continue // tolerate a damaged cache row
		}
		for i := range e.Renditions {
			if e.Renditions[i].Hash == hash {
				if !visit(&p, &e, &e.Renditions[i]) {
					return nil
				}
				break
			}
		}
	}
	return rows.Err()
}

// MadnetworkBlobProviders returns the friends who hold hash — the swarm's
// tracker (federation F4). It unions two sources: friends whose published
// catalog advertises the hash as a rendition (their library) and friends
// advertising it in their download cache (federation_holdings). Ordered
// most-recently-seen first (the fetch order); the advertised byte size comes
// from the catalog (a hint; a cache-only holder contributes none and the fetch
// learns the size from the manifest). Satisfies the F4 half of
// federation.PeerStore.
func (db *DB) MadnetworkBlobProviders(ctx context.Context, hash string) (int64, []*federation.Peer, error) {
	var size int64
	holders := map[int64]*federation.Peer{}
	err := db.madnetworkRowsForHash(ctx, hash, func(p *federation.Peer, _ *federation.CatalogEntry, rd *federation.CatalogRendition) bool {
		if size == 0 {
			size = rd.Size
		}
		if _, ok := holders[p.ID]; !ok {
			cp := *p
			holders[cp.ID] = &cp
		}
		return true
	})
	if err != nil {
		return 0, nil, err
	}

	// Cache holders (federation_holdings) — friends seeding the blob from their
	// download cache without it being in their library catalog.
	rows, err := db.QueryContext(ctx, `
		SELECT p.id, p.public_key, p.name, p.heard_name, p.last_seen
		FROM federation_holdings h
		JOIN federation_peers p ON p.id = h.peer_id AND p.state = 'friend'
		WHERE h.hash = ?`, hash)
	if err != nil {
		return 0, nil, fmt.Errorf("holdings providers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p federation.Peer
		if err := rows.Scan(&p.ID, &p.PublicKey, &p.Name, &p.HeardName, &p.LastSeen); err != nil {
			return 0, nil, fmt.Errorf("scan holdings provider: %w", err)
		}
		if _, ok := holders[p.ID]; !ok {
			cp := p
			holders[cp.ID] = &cp
		}
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}

	out := make([]*federation.Peer, 0, len(holders))
	for _, p := range holders {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastSeen != out[j].LastSeen {
			return out[i].LastSeen > out[j].LastSeen // most recently seen first
		}
		return out[i].ID < out[j].ID
	})
	return size, out, nil
}

// MadnetworkEntryForHash returns the catalog entry (tagset text) behind a
// rendition hash — from the most recently seen friend advertising it — for the
// download-to-library staging metadata. Nil when no friend advertises it.
func (db *DB) MadnetworkEntryForHash(ctx context.Context, hash string) (*federation.CatalogEntry, error) {
	var entry *federation.CatalogEntry
	err := db.madnetworkRowsForHash(ctx, hash, func(_ *federation.Peer, e *federation.CatalogEntry, _ *federation.CatalogRendition) bool {
		entry = e
		return false
	})
	return entry, err
}

// ── Merged browse (the /madnetwork drill-down) ───────────────────────────────

// MadnetworkView carries the merged-browse policy: whether to fold in this
// node's own published set, and the reachability Cutoff — a friend's rendition is
// shown only when its holder's last_seen >= Cutoff (docs/architecture/federation.md
// §Availability & node health). Cutoff <= 0 disables availability filtering (fail
// open, or the admin turned hiding off), which reproduces the pre-availability
// behaviour of showing every friend's cached catalog.
type MadnetworkView struct {
	IncludeSelf bool
	Cutoff      int64
	// DefaultShareDepth is the node-level sharing scope the self-merged rows
	// inherit when a recording carries none (F5). A recording this node does not
	// publish is not on the network, so it must not appear on the network page
	// either — it stays in the local library at /, which is exactly the
	// distinction the admin made. Only the private/not-private boundary matters
	// here, so the zero value (DepthFriends) is the safe default for an unset
	// view: recordings without an explicit depth stay visible, explicitly
	// private ones do not.
	DefaultShareDepth int
}

// reachClause gates a friend join by reachability. cutoff is a server-computed
// unix time (now − window), never user input, so inlining the integer is safe
// and avoids threading a bound parameter through every shared fragment. An
// empty clause (cutoff <= 0) leaves the join unfiltered.
func reachClause(cutoff int64) string {
	if cutoff <= 0 {
		return ""
	}
	return fmt.Sprintf(" AND p.last_seen >= %d", cutoff)
}

// The browse queries group by DISPLAY identity — the grouping artist is the
// album artist, falling back to the performer, falling back to the unknown
// bucket, mirroring the local library's album-artist-only artist list; albums
// fall back to the shared "Other" bucket. Only reachable friends' catalogs are
// visible (reachClause; cutoff <= 0 = all friends).
func fedcatBase(cutoff int64) string {
	return `
	FROM (SELECT COALESCE(NULLIF(c.album_artist, ''), NULLIF(c.artist, ''), '` + DefaultArtistName + `') AS akey,
	             COALESCE(NULLIF(c.album, ''), '` + DefaultAlbumTitle + `') AS alb,
	             c.*
	      FROM federation_catalog c
	      JOIN federation_peers p ON p.id = c.peer_id AND p.state = 'friend'` + reachClause(cutoff) + `)`
}

// fedcatRemoteRows / fedcatSelfRows are the two sources of the merged counting
// queries (artists / albums / summary / search), reduced to the columns those
// queries group and count by. The self source is this node's own published set
// — the same visibleTagset predicate and display-name resolution that
// PublishedCatalog advertises to friends, with fedcatBase's bucket fallbacks
// applied on top, so a track we publish folds with the same track cached from
// a friend (docs/ui/madnetwork-page.md §Own tracks).
func fedcatRemoteRows(cutoff int64) string {
	return `
	SELECT COALESCE(NULLIF(c.album_artist, ''), NULLIF(c.artist, ''), '` + DefaultArtistName + `') AS akey,
	       COALESCE(NULLIF(c.album, ''), '` + DefaultAlbumTitle + `') AS alb,
	       c.title AS title, c.track_number AS track_number,
	       c.disc_number AS disc_number, c.year AS year
	FROM federation_catalog c
	JOIN federation_peers p ON p.id = c.peer_id AND p.state = 'friend'` + reachClause(cutoff)
}

// selfPublishedClause keeps the self-merged rows to what this node actually
// publishes: a recording is on the network iff its effective depth reaches at
// least a direct friend (F5). defaultDepth is a server-resolved integer, never
// user input, so it is inlined like reachClause's cutoff rather than threaded as
// a bind parameter through every shared fragment.
func selfPublishedClause(defaultDepth int) string {
	return fmt.Sprintf(" AND COALESCE(r.share_depth, %d) >= %d", defaultDepth, federation.DepthFriends)
}

func fedcatSelfRows(defaultDepth int) string {
	return `
	SELECT COALESCE(NULLIF(COALESCE(aar.name, m.album_artist, ''), ''),
	                NULLIF(COALESCE(par.name, m.artist, ''), ''), '` + DefaultArtistName + `') AS akey,
	       COALESCE(NULLIF(COALESCE(al.title, m.album, ''), ''), '` + DefaultAlbumTitle + `') AS alb,
	       m.title AS title, m.track_number AS track_number,
	       m.disc_number AS disc_number, m.year AS year
	FROM tagsets m` + recordingJoin + `
	LEFT JOIN artists par ON par.id = m.artist_id
	LEFT JOIN artists aar ON aar.id = m.album_artist_id
	LEFT JOIN albums al   ON al.id  = m.album_id
	WHERE ` + visibleTagset + selfPublishedClause(defaultDepth)
}

// fedcatCountBase is the FROM clause of the counting queries: reachable friends'
// catalogs (cutoff), optionally unioned with the own published set (always
// available — self is never gated). includeSelf is off when federation is
// disabled — the page then stays what the friends provide (nothing), matching
// the "list fully clears" rule.
func fedcatCountBase(view MadnetworkView) string {
	if view.IncludeSelf {
		return ` FROM (` + fedcatRemoteRows(view.Cutoff) + ` UNION ALL ` + fedcatSelfRows(view.DefaultShareDepth) + `)`
	}
	return ` FROM (` + fedcatRemoteRows(view.Cutoff) + `)`
}

// Leading ORDER BY keys forcing the unknown buckets to the bottom of the
// alphabetical lists (the library's norm_name trick, matched on the canonical
// default strings — remote catalogs carry no normalized ids).
const artistBucketLast = `(lower(akey) = lower('` + DefaultArtistName + `')) ASC`
const albumBucketLast = `(lower(alb) = lower('` + DefaultAlbumTitle + `')) ASC`

// trackIdent is the merged logical-track identity inside one artist bucket:
// album + disc + track + title (case-insensitive) — the same text offered by
// several friends is ONE row.
const trackIdent = `lower(alb) || char(31) || COALESCE(disc_number, -1) || char(31) ||
	COALESCE(track_number, -1) || char(31) || lower(title)`

// MadnetworkArtist is one row of the merged artist list.
type MadnetworkArtist struct {
	Name   string `json:"name"`
	Albums int64  `json:"albums"`
	Tracks int64  `json:"tracks"`
}

// MadnetworkArtists lists the merged catalog's artists (display-identity
// grouping, case-insensitive), optionally filtered by a substring. The unknown
// bucket sorts last; includeSelf merges the own published set in.
func (db *DB) MadnetworkArtists(ctx context.Context, q string, view MadnetworkView) ([]*MadnetworkArtist, error) {
	where, args := "", []any{}
	if s := strings.TrimSpace(q); s != "" {
		escaped := strings.NewReplacer(`%`, `\%`, `_`, `\_`).Replace(s)
		where = ` WHERE lower(akey) LIKE lower(?) ESCAPE '\'`
		args = append(args, "%"+escaped+"%")
	}
	rows, err := db.QueryContext(ctx, `
		SELECT MIN(akey), COUNT(DISTINCT lower(alb)), COUNT(DISTINCT `+trackIdent+`)
		`+fedcatCountBase(view)+where+`
		GROUP BY lower(akey)
		ORDER BY `+artistBucketLast+`, lower(akey)`, args...)
	if err != nil {
		return nil, fmt.Errorf("madnetwork artists: %w", err)
	}
	defer rows.Close()
	var out []*MadnetworkArtist
	for rows.Next() {
		var a MadnetworkArtist
		if err := rows.Scan(&a.Name, &a.Albums, &a.Tracks); err != nil {
			return nil, fmt.Errorf("scan madnetwork artist: %w", err)
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// MadnetworkAlbum is one row of an artist's merged album list.
type MadnetworkAlbum struct {
	Title  string `json:"title"`
	Tracks int64  `json:"tracks"`
	Year   *int64 `json:"year,omitempty"`
}

// MadnetworkAlbums lists one artist's albums in the merged catalog; the
// "Other" bucket sorts last.
func (db *DB) MadnetworkAlbums(ctx context.Context, artist string, view MadnetworkView) ([]*MadnetworkAlbum, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT MIN(alb), COUNT(DISTINCT `+trackIdent+`), MAX(year)
		`+fedcatCountBase(view)+`
		WHERE lower(akey) = lower(?)
		GROUP BY lower(alb)
		ORDER BY `+albumBucketLast+`, year IS NULL, year, lower(alb)`, artist)
	if err != nil {
		return nil, fmt.Errorf("madnetwork albums: %w", err)
	}
	defer rows.Close()
	var out []*MadnetworkAlbum
	for rows.Next() {
		var a MadnetworkAlbum
		var year sql.NullInt64
		if err := rows.Scan(&a.Title, &a.Tracks, &year); err != nil {
			return nil, fmt.Errorf("scan madnetwork album: %w", err)
		}
		a.Year = nullInt(year)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// MadnetworkTrackRow is one RAW row of the merged track view — one per
// (source, appearance), a source being a friend's cached catalog or, for
// Self rows, this node's own published set. The handler merges rows into
// logical tracks and their "N versions" (distinct claimed recordings) — set
// logic that reads better in Go than SQL at album scale.
type MadnetworkTrackRow struct {
	PeerID       int64
	PeerName     string
	PeerLastSeen int64
	Entry        federation.CatalogEntry

	// GroupArtist/GroupAlbum are the display-identity buckets (akey/alb) the
	// row belongs to — the search handler groups cross-album results by them.
	GroupArtist string
	GroupAlbum  string

	// Self rows come from the local library: PeerID is 0, Entry.Key is the
	// local tagset id, and ObjectKeys maps each rendition hash to its local
	// files object key (for direct /files/ play URLs).
	Self       bool
	ObjectKeys map[string]string
}

// remoteTrackRows runs the raw cached-row query with a caller-supplied match
// clause over the bucketed columns (akey/alb/title available). cutoff gates the
// rows to reachable friends (cutoff <= 0 = all).
func (db *DB) remoteTrackRows(ctx context.Context, cutoff int64, match string, args ...any) ([]*MadnetworkTrackRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT peer_id, `+peerLabelExpr("p2")+`, p2.last_seen, akey, alb,
		       entry_key, recording_key, title, artist, album_artist,
		       COALESCE(genre, ''), year, track_number, disc_number,
		       COALESCE(duration, 0), COALESCE(license, ''), guest_playable, renditions
		`+fedcatBase(cutoff)+`
		JOIN federation_peers p2 ON p2.id = peer_id
		WHERE `+match+`
		ORDER BY (disc_number IS NULL) ASC, disc_number ASC, track_number ASC, lower(title) ASC, peer_id ASC`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("madnetwork tracks: %w", err)
	}
	defer rows.Close()
	var out []*MadnetworkTrackRow
	for rows.Next() {
		var r MadnetworkTrackRow
		var year, track, disc sql.NullInt64
		var renditions string
		if err := rows.Scan(&r.PeerID, &r.PeerName, &r.PeerLastSeen, &r.GroupArtist, &r.GroupAlbum,
			&r.Entry.Key, &r.Entry.RecordingKey, &r.Entry.Title, &r.Entry.Artist,
			&r.Entry.AlbumArtist, &r.Entry.Genre, &year, &track, &disc,
			&r.Entry.Duration, &r.Entry.License, &r.Entry.GuestPlayable, &renditions); err != nil {
			return nil, fmt.Errorf("scan madnetwork track: %w", err)
		}
		r.Entry.Album = r.GroupAlbum
		r.Entry.Year, r.Entry.TrackNumber, r.Entry.DiscNumber = nullInt(year), nullInt(track), nullInt(disc)
		if err := json.Unmarshal([]byte(renditions), &r.Entry.Renditions); err != nil {
			r.Entry.Renditions = nil // tolerate a damaged cache row rather than failing the album
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// MadnetworkTracks returns reachable friends' cached rows for one artist+album,
// in display order (cutoff gates reachability; <= 0 = all friends).
func (db *DB) MadnetworkTracks(ctx context.Context, artist, album string, cutoff int64) ([]*MadnetworkTrackRow, error) {
	return db.remoteTrackRows(ctx, cutoff, `lower(akey) = lower(?) AND lower(alb) = lower(?)`, artist, album)
}

// Self-row display-identity expressions over the tagsets join (aliases par /
// aar / al as in fedcatSelfRows) — the WHERE side of the bucket fallbacks.
const selfAkeyExpr = `COALESCE(NULLIF(COALESCE(aar.name, m.album_artist, ''), ''),
	NULLIF(COALESCE(par.name, m.artist, ''), ''), '` + DefaultArtistName + `')`
const selfAlbExpr = `COALESCE(NULLIF(COALESCE(al.title, m.album, ''), ''), '` + DefaultAlbumTitle + `')`

// ownTrackRows returns this node's own published appearances matching a
// caller-supplied clause (akey/alb/title-level), shaped like cached catalog
// rows: Self = true, PeerID 0, Entry.Key = tagset id, renditions attached from
// the recording's live files with their local object keys. defaultDepth applies
// the same self-published filter as the counting queries, so a recording kept
// off the network cannot be listed by a view whose counts already exclude it.
func (db *DB) ownTrackRows(ctx context.Context, defaultDepth int, match string, args ...any) ([]*MadnetworkTrackRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.recording_id, `+selfAkeyExpr+`, `+selfAlbExpr+`, m.title,
		       COALESCE(par.name, m.artist, ''), COALESCE(aar.name, m.album_artist, ''),
		       COALESCE(m.genre, ''), m.year, m.track_number, m.disc_number,
		       COALESCE(r.license, ''), r.guest_playable
		FROM tagsets m`+recordingJoin+`
		LEFT JOIN artists par ON par.id = m.artist_id
		LEFT JOIN artists aar ON aar.id = m.album_artist_id
		LEFT JOIN albums al   ON al.id  = m.album_id
		WHERE `+visibleTagset+selfPublishedClause(defaultDepth)+` AND `+match+`
		ORDER BY (m.disc_number IS NULL) ASC, m.disc_number ASC, m.track_number ASC, lower(m.title) ASC, m.id ASC`,
		args...)
	if err != nil {
		return nil, fmt.Errorf("madnetwork own tracks: %w", err)
	}
	defer rows.Close()

	var out []*MadnetworkTrackRow
	recordings := map[string][]int{} // recording key -> row indexes awaiting renditions
	for rows.Next() {
		r := MadnetworkTrackRow{Self: true, ObjectKeys: map[string]string{}}
		var tagsetID, recordingID int64
		var year, track, disc sql.NullInt64
		if err := rows.Scan(&tagsetID, &recordingID, &r.GroupArtist, &r.GroupAlbum, &r.Entry.Title,
			&r.Entry.Artist, &r.Entry.AlbumArtist, &r.Entry.Genre, &year, &track, &disc,
			&r.Entry.License, &r.Entry.GuestPlayable); err != nil {
			return nil, fmt.Errorf("scan madnetwork own track: %w", err)
		}
		r.Entry.Key = strconv.FormatInt(tagsetID, 10)
		r.Entry.RecordingKey = strconv.FormatInt(recordingID, 10)
		r.Entry.Album = r.GroupAlbum
		r.Entry.Year, r.Entry.TrackNumber, r.Entry.DiscNumber = nullInt(year), nullInt(track), nullInt(disc)
		r.Entry.Renditions = []federation.CatalogRendition{}
		recordings[r.Entry.RecordingKey] = append(recordings[r.Entry.RecordingKey], len(out))
		out = append(out, &r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	// Attach each recording's live renditions plus their object keys (the
	// local /files/ addresses the browse serves for self-held tracks).
	ids := make([]any, 0, len(recordings))
	ph := make([]string, 0, len(recordings))
	for key := range recordings {
		ids = append(ids, key)
		ph = append(ph, "?")
	}
	rrows, err := db.QueryContext(ctx, `
		SELECT f.recording_id, f.hash, f.byte_size, f.object_key,
		       COALESCE(mm.codec, ''), COALESCE(mm.bitrate, 0),
		       COALESCE(mm.sample_rate, 0), COALESCE(mm.bit_depth, 0),
		       COALESCE(mm.duration_seconds, 0)
		FROM files f
		LEFT JOIN media_metadata mm ON mm.file_id = f.id
		WHERE f.deleted_at IS NULL AND f.recording_id IN (`+strings.Join(ph, ",")+`)
		ORDER BY f.recording_id, f.id`, ids...)
	if err != nil {
		return nil, fmt.Errorf("madnetwork own renditions: %w", err)
	}
	defer rrows.Close()
	for rrows.Next() {
		var recordingID int64
		var objectKey string
		var rd federation.CatalogRendition
		if err := rrows.Scan(&recordingID, &rd.Hash, &rd.Size, &objectKey, &rd.Codec,
			&rd.Bitrate, &rd.SampleRate, &rd.BitDepth, &rd.Duration); err != nil {
			return nil, fmt.Errorf("scan madnetwork own rendition: %w", err)
		}
		for _, i := range recordings[strconv.FormatInt(recordingID, 10)] {
			out[i].Entry.Renditions = append(out[i].Entry.Renditions, rd)
			out[i].ObjectKeys[rd.Hash] = objectKey
			if out[i].Entry.Duration == 0 && rd.Duration > 0 {
				out[i].Entry.Duration = rd.Duration
			}
		}
	}
	return out, rrows.Err()
}

// MadnetworkOwnTracks returns the own published rows for one artist+album —
// the Self side of the merged track view.
func (db *DB) MadnetworkOwnTracks(ctx context.Context, artist, album string, view MadnetworkView) ([]*MadnetworkTrackRow, error) {
	return db.ownTrackRows(ctx, view.DefaultShareDepth,
		`lower(`+selfAkeyExpr+`) = lower(?) AND lower(`+selfAlbExpr+`) = lower(?)`, artist, album)
}

// ── Merged search (docs/ui/madnetwork-page.md §Search) ───────────────────────

// MadnetworkSearchAlbum is one album hit of the merged search.
type MadnetworkSearchAlbum struct {
	Artist string `json:"artist_name"`
	Title  string `json:"title"`
	Tracks int64  `json:"track_count"`
	Year   *int64 `json:"year,omitempty"`
}

// MadnetworkSearchAlbums lists merged albums whose title matches a substring.
func (db *DB) MadnetworkSearchAlbums(ctx context.Context, q string, limit int, view MadnetworkView) ([]*MadnetworkSearchAlbum, error) {
	s := strings.TrimSpace(q)
	if s == "" || limit <= 0 {
		return []*MadnetworkSearchAlbum{}, nil
	}
	escaped := strings.NewReplacer(`%`, `\%`, `_`, `\_`).Replace(s)
	rows, err := db.QueryContext(ctx, `
		SELECT MIN(akey), MIN(alb), COUNT(DISTINCT `+trackIdent+`), MAX(year)
		`+fedcatCountBase(view)+`
		WHERE lower(alb) LIKE lower(?) ESCAPE '\'
		GROUP BY lower(akey), lower(alb)
		ORDER BY `+albumBucketLast+`, lower(alb), lower(akey)
		LIMIT ?`, "%"+escaped+"%", limit)
	if err != nil {
		return nil, fmt.Errorf("madnetwork search albums: %w", err)
	}
	defer rows.Close()
	out := []*MadnetworkSearchAlbum{}
	for rows.Next() {
		var a MadnetworkSearchAlbum
		var year sql.NullInt64
		if err := rows.Scan(&a.Artist, &a.Title, &a.Tracks, &year); err != nil {
			return nil, fmt.Errorf("scan madnetwork search album: %w", err)
		}
		a.Year = nullInt(year)
		out = append(out, &a)
	}
	return out, rows.Err()
}

// searchRowCap bounds the raw rows fed into the search handler's track merge —
// a LIKE over titles can fan out; the handler caps the merged result anyway.
const searchRowCap = 400

// MadnetworkSearchTrackRows returns the raw rows (remote and, when includeSelf,
// own) whose title matches a substring. Rows arrive grouped by source; the
// handler groups by (GroupArtist, GroupAlbum) and merges per group, so having
// every source's rows for a matching title keeps version folding correct.
func (db *DB) MadnetworkSearchTrackRows(ctx context.Context, q string, view MadnetworkView) ([]*MadnetworkTrackRow, error) {
	s := strings.TrimSpace(q)
	if s == "" {
		return []*MadnetworkTrackRow{}, nil
	}
	escaped := "%" + strings.NewReplacer(`%`, `\%`, `_`, `\_`).Replace(s) + "%"
	rows, err := db.remoteTrackRows(ctx, view.Cutoff, `lower(title) LIKE lower(?) ESCAPE '\'`, escaped)
	if err != nil {
		return nil, err
	}
	if view.IncludeSelf {
		own, err := db.ownTrackRows(ctx, view.DefaultShareDepth, `lower(m.title) LIKE lower(?) ESCAPE '\'`, escaped)
		if err != nil {
			return nil, err
		}
		rows = append(rows, own...)
	}
	if len(rows) > searchRowCap {
		rows = rows[:searchRowCap]
	}
	return rows, nil
}

// MadnetworkFriend is one friend's sync status on the /madnetwork page strip.
// The strip lists every friend (reachable or not); Reachable drives the greying
// of a friend seen longer ago than the view's freshness window.
type MadnetworkFriend struct {
	Name      string `json:"name"`
	LastSeen  int64  `json:"last_seen"`
	SyncedAt  int64  `json:"synced_at"`
	Entries   int64  `json:"entries"`
	Reachable bool   `json:"reachable"`
}

// MadnetworkSummary reports the merged catalog's shape: every friend with sync
// state and reachability, plus the merged distinct track count over the visible
// (reachable + own) set. The friend list is not filtered — the strip shows all
// friends and greys the unreachable — but the track count uses the view's cutoff.
func (db *DB) MadnetworkSummary(ctx context.Context, view MadnetworkView) ([]*MadnetworkFriend, int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT `+peerLabelExpr("p")+`, p.last_seen, p.catalog_synced_at,
		       (SELECT COUNT(*) FROM federation_catalog c WHERE c.peer_id = p.id)
		FROM federation_peers p
		WHERE p.state = 'friend'
		ORDER BY lower(`+peerLabelExpr("p")+`), p.id`)
	if err != nil {
		return nil, 0, fmt.Errorf("madnetwork summary: %w", err)
	}
	defer rows.Close()
	var friends []*MadnetworkFriend
	for rows.Next() {
		var f MadnetworkFriend
		if err := rows.Scan(&f.Name, &f.LastSeen, &f.SyncedAt, &f.Entries); err != nil {
			return nil, 0, fmt.Errorf("scan madnetwork friend: %w", err)
		}
		f.Reachable = view.Cutoff <= 0 || f.LastSeen >= view.Cutoff
		friends = append(friends, &f)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	var tracks int64
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT lower(akey) || char(31) || `+trackIdent+`)
		`+fedcatCountBase(view)).Scan(&tracks); err != nil {
		return nil, 0, fmt.Errorf("madnetwork track count: %w", err)
	}
	return friends, tracks, nil
}

func nullInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.Int64
}

func nullFloat(v float64) any {
	if v == 0 {
		return nil
	}
	return v
}
