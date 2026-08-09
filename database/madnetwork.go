package database

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"daemonlord.ygg/madshare/federation"
)

// Federation F2 — the catalog (docs/architecture/federation.md §Catalog): what
// this node publishes (PublishedCatalog), the per-source cached copies pulled
// from other nodes (ReplaceSourceCatalog), and the merged browse queries behind
// /api/madnetwork/*. *DB satisfies the catalog half of federation.PeerStore here.
//
// Since F7 item 5 a cached copy hangs off a *source* (federation_catalog_sources,
// madnetwork_sources.go) rather than off a friendship, because this node pulls
// from every member of its community and not only from the nodes an admin
// hand-picked. The one trust condition the browse queries still carry is the
// block: a blocked node's rows are kept but hidden, so an unblock restores the
// view without a resync.

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
	//
	// The fingerprint head rides along per rendition (F6): what this node says the
	// audio *is*, in a form a friend holding the same bytes can check. substr on a
	// BLOB counts bytes, so this reads the first ClaimHeadWords packed words and
	// never the whole fingerprint — see federation.ClaimHeadWords for why.
	rrows, err := db.QueryContext(ctx, `
		SELECT f.recording_id, f.hash, f.byte_size,
		       COALESCE(mm.codec, ''), COALESCE(mm.bitrate, 0),
		       COALESCE(mm.sample_rate, 0), COALESCE(mm.bit_depth, 0),
		       COALESCE(mm.duration_seconds, 0),
		       COALESCE(af.algo, ''), COALESCE(af.algo_version, ''),
		       COALESCE(LENGTH(af.fingerprint), 0),
		       SUBSTR(af.fingerprint, 1, ?)
		FROM files f
		JOIN recordings r ON r.id = f.recording_id
		LEFT JOIN media_metadata mm ON mm.file_id = f.id
		LEFT JOIN audio_fingerprints af ON af.file_id = f.id
		WHERE f.deleted_at IS NULL AND (`+scope+`) AND EXISTS (
			SELECT 1 FROM tagsets m WHERE m.recording_id = f.recording_id AND `+visibleTagset+`)
		ORDER BY f.recording_id, f.id`,
		append([]any{federation.ClaimHeadWords * 4}, scopeArgs(defaultDepth, aud)...)...)
	if err != nil {
		return nil, fmt.Errorf("catalog renditions: %w", err)
	}
	defer rrows.Close()
	for rrows.Next() {
		var recordingID int64
		var rd federation.CatalogRendition
		var algo, algoVersion string
		var fpBytes int
		var head []byte
		if err := rrows.Scan(&recordingID, &rd.Hash, &rd.Size, &rd.Codec,
			&rd.Bitrate, &rd.SampleRate, &rd.BitDepth, &rd.Duration,
			&algo, &algoVersion, &fpBytes, &head); err != nil {
			return nil, fmt.Errorf("scan catalog rendition: %w", err)
		}
		if len(head) >= 4 {
			rd.Fingerprint = &federation.FingerprintClaim{
				Algo:    algo,
				Version: algoVersion,
				Words:   fpBytes / 4,
				Head:    base64.StdEncoding.EncodeToString(head),
			}
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

// ReplaceSourceCatalog atomically replaces the cached copy of one source's
// catalog with a fresh snapshot and records the snapshot serial + sync time.
//
// The replace is wholesale, but first_seen is NOT: the dates of the entries this
// source already offered are read before the delete and re-applied to the ones
// that survive (migration 037). Without that, every sync that changed anything
// would re-date the source's whole library, and "New on the network" would be a
// list of whoever synced most recently.
func (db *DB) ReplaceSourceCatalog(ctx context.Context, sourceID int64, serial string, syncedAt int64, entries []federation.CatalogEntry) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace source catalog: %w", err)
	}
	defer tx.Rollback()

	seen, err := catalogFirstSeen(ctx, tx, sourceID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM federation_catalog WHERE source_id = ?`, sourceID); err != nil {
		return fmt.Errorf("clear source catalog: %w", err)
	}
	ins, err := tx.PrepareContext(ctx, `
		INSERT INTO federation_catalog (source_id, entry_key, recording_key, title, artist,
			album_artist, album, genre, year, track_number, disc_number, duration,
			license, guest_playable, renditions, first_seen)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare source catalog insert: %w", err)
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
		firstSeen := syncedAt
		if prior, ok := seen[e.Key]; ok && prior > 0 {
			firstSeen = prior
		}
		if _, err := ins.ExecContext(ctx, sourceID, e.Key, e.RecordingKey, e.Title, e.Artist,
			e.AlbumArtist, e.Album, e.Genre, e.Year, e.TrackNumber, e.DiscNumber,
			nullFloat(e.Duration), e.License, e.GuestPlayable, string(renditions), firstSeen); err != nil {
			return fmt.Errorf("insert source catalog entry: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE federation_catalog_sources
		   SET catalog_serial = ?, catalog_synced_at = ?, last_seen = MAX(last_seen, ?)
		 WHERE id = ?`, serial, syncedAt, syncedAt, sourceID); err != nil {
		return fmt.Errorf("update source sync state: %w", err)
	}
	return tx.Commit()
}

// catalogFirstSeen reads one source's entry_key → first_seen map inside the
// replace transaction. A source's catalog is bounded by what we chose to cache,
// so holding it in memory for the length of the replace is the same order of
// cost as the snapshot being applied.
func catalogFirstSeen(ctx context.Context, tx *sql.Tx, sourceID int64) (map[string]int64, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT entry_key, first_seen FROM federation_catalog WHERE source_id = ?`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("read source catalog dates: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var key string
		var at int64
		if err := rows.Scan(&key, &at); err != nil {
			return nil, fmt.Errorf("scan source catalog date: %w", err)
		}
		out[key] = at
	}
	return out, rows.Err()
}

// MarkSourceCatalogChecked records a sync round that confirmed the cached copy
// is still fresh (the not-modified path). An answer is contact, so it advances
// last_seen exactly as a full snapshot does — for a member we never ping, this
// and the transfer path are the only liveness this node ever observes.
func (db *DB) MarkSourceCatalogChecked(ctx context.Context, sourceID int64, serial string, syncedAt int64) error {
	_, err := db.ExecContext(ctx, `
		UPDATE federation_catalog_sources
		   SET catalog_serial = ?, catalog_synced_at = ?, last_seen = MAX(last_seen, ?)
		 WHERE id = ?`, serial, syncedAt, syncedAt, sourceID)
	if err != nil {
		return fmt.Errorf("mark source catalog checked: %w", err)
	}
	return nil
}

// ReplaceSourceHoldings atomically replaces the cached list of what one source
// holds in its download cache and will seed (federation F4 holdings tracker,
// GET /madnetwork/v0/holdings). Invalid entries are skipped; duplicates collapse
// on the composite primary key.
func (db *DB) ReplaceSourceHoldings(ctx context.Context, sourceID int64, hashes []string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace source holdings: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM federation_holdings WHERE source_id = ?`, sourceID); err != nil {
		return fmt.Errorf("clear source holdings: %w", err)
	}
	ins, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO federation_holdings (source_id, hash) VALUES (?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare holdings insert: %w", err)
	}
	defer ins.Close()
	for _, h := range hashes {
		if !isContentHash(h) {
			continue // remote input — a content hash is 64 lowercase hex
		}
		if _, err := ins.ExecContext(ctx, sourceID, h); err != nil {
			return fmt.Errorf("insert source holding: %w", err)
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

// ── Cached rows and the node they came from ──────────────────────────────────

// sourceJoin attaches a cached catalog or holdings row (alias) to the node it
// was pulled from — its SOURCE — and to the local peer row for that node when
// there is one. The peer join is LEFT because since F7 item 5 most sources are
// members no admin ever touched; it carries the two things a source row
// deliberately does not hold: the admin's label, and whether the admin blocked
// this node.
func sourceJoin(alias string) string {
	return `
	JOIN federation_catalog_sources s ON s.id = ` + alias + `.source_id
	LEFT JOIN federation_peers p ON p.public_key = s.public_key`
}

// notBlocked hides a blocked node's cached rows without deleting them, so an
// unblock restores the view with no resync. Before migration 036 this was the
// `p.state = 'friend'` join every browse query carried; it is the only trust
// condition left, because pulling a catalog no longer implies friendship.
const notBlocked = `COALESCE(p.state, '') <> 'blocked'`

// srcLastSeen is a source's freshness: the newest of what a catalog pull
// observed and what a friendship ping observed. A friend is pinged every minute
// and pulled every fifteen, a member is only ever pulled — so neither clock
// alone is the answer, and the later one always is.
const srcLastSeen = `MAX(s.last_seen, COALESCE(p.last_seen, 0))`

// sourceLabelExpr names a cached row's origin: the admin's label if this node is
// a peer they named, else what the node calls itself, else its short key. The
// SQL twin of federation.BlobProvider.Display.
//
// Two heard names, and both are consulted: a *friend's* claim is refreshed by the
// friendship ping and lands on the peer row, a *member's* by the discovery ping
// and lands on the source row. Reading only one of them made friends — the nodes
// an admin cares most about — show as bare key prefixes while strangers showed
// their names, which is exactly backwards.
const sourceHeardExpr = `COALESCE(NULLIF(p.heard_name, ''), s.heard_name)`
const sourceLabelExpr = `COALESCE(NULLIF(p.name, ''), NULLIF(` + sourceHeardExpr + `, ''), substr(s.public_key, 1, 12))`

// ── Blob lookup (federation F3/F4) ───────────────────────────────────────────

// madnetworkRowsForHash scans cached rows whose renditions advertise hash,
// most-recently-seen source first. The LIKE is a cheap pre-filter (a content
// hash is plain hex — no LIKE metacharacters); the JSON parse confirms.
func (db *DB) madnetworkRowsForHash(ctx context.Context, hash string,
	visit func(provider *federation.BlobProvider, entry *federation.CatalogEntry, rendition *federation.CatalogRendition) bool) error {
	rows, err := db.QueryContext(ctx, `
		SELECT s.id, COALESCE(p.id, 0), s.public_key, COALESCE(p.name, ''),
		       `+sourceHeardExpr+`, `+srcLastSeen+`,
		       c.entry_key, c.recording_key, c.title, c.artist, c.album_artist,
		       c.album, COALESCE(c.genre, ''), c.year, c.track_number, c.disc_number,
		       COALESCE(c.duration, 0), COALESCE(c.license, ''), c.guest_playable, c.renditions
		FROM federation_catalog c`+sourceJoin("c")+`
		WHERE c.renditions LIKE ? AND `+notBlocked+`
		ORDER BY `+srcLastSeen+` DESC, s.id, c.entry_key`, "%"+hash+"%")
	if err != nil {
		return fmt.Errorf("madnetwork rows for hash: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p federation.BlobProvider
		var e federation.CatalogEntry
		var year, track, disc sql.NullInt64
		var renditions string
		if err := rows.Scan(&p.SourceID, &p.PeerID, &p.PublicKey, &p.Name, &p.HeardName, &p.LastSeen,
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

// StaleHolderWindow is how far back a FETCH PLAN will look. A node nothing has
// observed within it is not offered as somewhere to fetch from, however recently
// it advertised the hash.
//
// It exists because a plan without one is expensive in a way that is easy to
// miss. Measured 2026-08-09 against a live server, a device was handed holders
// last seen 21 and 54 hours earlier, and the four-node experiment
// federation.TestStaleHoldersCostAFetch put the price at ~150× the whole clean
// fetch for ONE dead entry — paid while a live holder sits there carrying every
// byte, because dispatch is round-robin and a dial that never connects burns
// Timeouts.PerChunk (2 minutes) rather than ChunkStall.
//
// The window is the PULL window rather than the browse's tighter ping window,
// and one window rather than the browse's two. A fetch plan's job is to exclude
// the definitely-gone, not to be precise about the briefly-quiet: the two errors
// are not symmetric, since dropping a live holder costs a fetch one source while
// keeping a dead one costs it minutes. Three catalog cycles is long enough that
// anything past it is not being observed by us at all, and short enough that the
// 21-hour case never survives it.
//
// Fetching FAILS CLOSED, unlike the browse, which fails open when this node's own
// inbound is sick. An empty plan is a good answer — the holders endpoint already
// documents empty as 200-not-404 because the caller's fallback is the relay — so
// a client pays milliseconds to learn there is nobody, instead of minutes to
// learn it from a list of corpses. An empty browse page, by contrast, would be a
// lie about the library, which is why that one leans the other way.
const StaleHolderWindow = federation.PullFreshnessWindow

// MadnetworkBlobProviders returns the nodes that hold hash — the swarm's
// tracker (federation F4). It unions three sources: nodes whose published
// catalog advertises the hash as a rendition (their library), nodes advertising
// it in their download cache (federation_holdings), and this server's own
// listener devices (federation_listener_holdings, §"The household"). Ordered
// most-recently-seen first (the fetch order); the advertised byte size comes
// from the catalog (a hint; a cache-only holder contributes none and the fetch
// learns the size from the manifest). Satisfies the F4 half of
// federation.PeerStore.
//
// Since F7 item 5 a holder is any node we cache a catalog from — every member of
// our community the frontier has reached, not only a friend. That widening is
// the whole point of the phase: authorization was never what kept other people's
// libraries out of reach, knowing who holds a hash was.
//
// The devices are the same idea one step further in, and they are the reason
// this dedupes by public key rather than by source id: a device has no source
// row, so a source-id map would fold every one of them into a single entry. They
// reach exactly two callers — this node's own EnsureBlob, and the fetch plan a
// device asks for — and deliberately never the mesh, whose holdings endpoint
// answers from its own cache directory and cannot see this table at all.
func (db *DB) MadnetworkBlobProviders(ctx context.Context, hash string) (int64, []*federation.BlobProvider, error) {
	var size int64
	// Keyed by public key, not source id. A node IS its key everywhere else in
	// federation — the frontier rotation recycles ids, and a listener device has
	// no source row at all, so a source-id map would fold every device on this
	// server into one entry.
	holders := map[string]*federation.BlobProvider{}
	// A node nothing has observed within StaleHolderWindow is not offered as
	// somewhere to fetch from. The size is still taken from its catalog row: the
	// advertised byte count is a fact about the blob, and dropping it because its
	// advertiser went quiet would make an unknown size out of a known one.
	cutoff := time.Now().Add(-StaleHolderWindow).Unix()
	err := db.madnetworkRowsForHash(ctx, hash, func(p *federation.BlobProvider, _ *federation.CatalogEntry, rd *federation.CatalogRendition) bool {
		if size == 0 {
			size = rd.Size
		}
		if p.LastSeen < cutoff {
			return true
		}
		if _, ok := holders[p.PublicKey]; !ok {
			cp := *p
			holders[cp.PublicKey] = &cp
		}
		return true
	})
	if err != nil {
		return 0, nil, err
	}

	// Cache holders (federation_holdings) — nodes seeding the blob from their
	// download cache without it being in their library catalog.
	rows, err := db.QueryContext(ctx, `
		SELECT s.id, COALESCE(p.id, 0), s.public_key, COALESCE(p.name, ''),
		       `+sourceHeardExpr+`, `+srcLastSeen+`
		FROM federation_holdings h`+sourceJoin("h")+`
		WHERE h.hash = ? AND `+notBlocked+` AND `+srcLastSeen+` >= ?`, hash, cutoff)
	if err != nil {
		return 0, nil, fmt.Errorf("holdings providers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var p federation.BlobProvider
		if err := rows.Scan(&p.SourceID, &p.PeerID, &p.PublicKey, &p.Name, &p.HeardName, &p.LastSeen); err != nil {
			return 0, nil, fmt.Errorf("scan holdings provider: %w", err)
		}
		if _, ok := holders[p.PublicKey]; !ok {
			cp := p
			holders[cp.PublicKey] = &cp
		}
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}

	// This server's own listener devices (§"The household", migration 045). They
	// are here rather than in a separate call because a device seeding its cache
	// is a holder like any other from the fetching side — the swarm asks it for
	// chunks over the same wire, and it fails over the same way. What is
	// different is who may learn about it, and that is a question about the
	// endpoints, not about this list.
	//
	// Added last, so a node with a catalog row keeps that identity: the same
	// machine could in principle be both.
	devices, err := db.ListenerBlobProviders(ctx, hash)
	if err != nil {
		return 0, nil, err
	}
	for _, p := range devices {
		if _, ok := holders[p.PublicKey]; !ok {
			holders[p.PublicKey] = p
		}
	}

	out := make([]*federation.BlobProvider, 0, len(holders))
	for _, p := range holders {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LastSeen != out[j].LastSeen {
			return out[i].LastSeen > out[j].LastSeen // most recently seen first
		}
		return out[i].PublicKey < out[j].PublicKey
	})
	return size, out, nil
}

// MadnetworkEntryForHash returns the catalog entry (tagset text) behind a
// rendition hash — from the most recently seen node advertising it — for the
// download-to-library staging metadata. Nil when nobody advertises it.
func (db *DB) MadnetworkEntryForHash(ctx context.Context, hash string) (*federation.CatalogEntry, error) {
	var entry *federation.CatalogEntry
	err := db.madnetworkRowsForHash(ctx, hash, func(_ *federation.BlobProvider, e *federation.CatalogEntry, _ *federation.CatalogRendition) bool {
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
	// PullCutoff is the same idea for a source whose only liveness clock is the
	// catalog pull — a member no friend of ours vouches for (F7 item 10,
	// §Availability, "Two clocks, two windows"). Three catalog cycles rather than
	// three ping rounds, because the window measures how recently we would have
	// NOTICED, and for such a node that is the rotation and not the ping. Zero
	// falls back to Cutoff, which reproduces the single-window behaviour.
	PullCutoff int64
	// PingedSince decides WHICH of the two a row is judged by: a source is on the
	// ping window if it is a direct friend, or if a freshness hint about it
	// arrived since this moment. It is *now − the ping window*, and it is set
	// even when the two cutoffs above are not — the class of a node is a fact
	// about who watches it, not about whether this request is filtering, and the
	// ⓘ panel greys holders by it while the browse shows everything (fail open).
	//
	// Asking "is a fast observer reporting NOW" rather than "did one ever" is
	// what keeps a healthy member visible when the friend that vouched for it
	// dies: the hints stop, the row falls back to the pull clock our own rotation
	// still refreshes, instead of being held to a window nothing feeds. Zero
	// classes every source as pinged, which is the pre-F7-item-10 behaviour.
	PingedSince int64
	// DefaultShareDepth is the node-level sharing scope the self-merged rows
	// inherit when a recording carries none (F5). A recording this node does not
	// publish is not on the network, so it must not appear on the network page
	// either — it stays in the local library at /, which is exactly the
	// distinction the admin made. Only the private/not-private boundary matters
	// here, so the zero value (DepthFriends) is the safe default for an unset
	// view: recordings without an explicit depth stay visible, explicitly
	// private ones do not.
	DefaultShareDepth int

	// SourceID restricts the browse to ONE cached catalog — a node's shelf
	// (docs/ui/madnetwork-page.md §Browsing a single node). Zero is the merged
	// view, and NoSourceID is the shelf of a node we hold nothing from. A single
	// node's shelf never folds the own set in: browsing a node means seeing what
	// that node offers, and we are a different node.
	SourceID int64
	// SelfOnly is the same restriction pointed at ourselves — our own published
	// library as the network sees it, which is the one shelf on the list whose
	// contents an admin can actually change.
	SelfOnly bool
	// AllOwn drops the sharing-scope filter from the OWN rows: the whole local
	// library, not only the part this node publishes. It exists for the Local
	// library lane, which is a doorway to `/` rather than a view of the network
	// — the reader is looking at their own server, and a lane that quietly left
	// out the recordings scoped Local would be answering a question nobody
	// asked. It never widens what is served: publishing runs off
	// PublishedCatalog, and every remote-facing query leaves this false.
	AllOwn bool
}

// includeRemote / includeOwn split a view into the two row sources the merged
// queries union. Keeping the rule in one place is what stops a source filter
// from being applied to one half of a UNION and forgotten on the other.
//
// Both can be false, and that case is load-bearing rather than an oversight:
// asking for OUR shelf on a node that publishes nothing to the network must
// answer with nothing. Answering with the merged catalog instead — the shape
// this had first — is the one answer that is certainly wrong.
func (v MadnetworkView) includeRemote() bool { return !v.SelfOnly && v.SourceID != NoSourceID }
func (v MadnetworkView) includeOwn() bool {
	return v.IncludeSelf && (v.SelfOnly || v.SourceID == 0)
}

// NoSourceID is the shelf of a node this server holds no catalog from: a view
// with neither half, so every browse over it answers empty.
//
// It exists because the two ways of naming a node fail differently. A stale
// catalog-source id may safely widen back to the merged view — a row number is
// this server's own bookkeeping. A node KEY may not: it is an explicit request
// for one node, and answering it with the whole community's catalog would put
// other nodes' content under that node's name.
const NoSourceID int64 = -1

// reachClause gates a source join by reachability. Both cutoffs are
// server-computed unix times (now − window), never user input, so inlining the
// integers is safe and avoids threading bound parameters through every shared
// fragment. An empty clause (Cutoff <= 0) leaves the join unfiltered.
//
// There is one window per CLASS OF OBSERVER, not one per query, because a single
// browse mixes them (F7 item 10, docs/architecture/federation.md §Availability,
// "Two clocks, two windows"):
//
//	friend            pinged every minute        → the tight ping window
//	hinted member     a friend pings it for us   → the tight ping window
//	pull-only member  reached by the rotation    → three catalog cycles
//
// A hint counts only while it is still ARRIVING (PingedSince, one ping window
// back), never for as long as one once did. Both failure modes turn on that:
//
//	the member died      hints keep coming, carrying a frozen observation, so the
//	                     row stays on the tight window and is hidden in 3 minutes
//	the VOUCHER died     hints stop, the row drops back to the pull clock our own
//	                     rotation still refreshes, and a healthy member stays up
//
// Reading the class off a hint that arrived 40 minutes ago would get the second
// case backwards — hiding a node we can reach, because somebody else stopped
// talking about it.
func reachClause(view MadnetworkView) string {
	if view.Cutoff <= 0 {
		return ""
	}
	pull := view.PullCutoff
	if pull <= 0 || pull >= view.Cutoff || view.PingedSince <= 0 {
		return fmt.Sprintf(" AND "+srcLastSeen+" >= %d", view.Cutoff)
	}
	return fmt.Sprintf(" AND "+srcLastSeen+" >= (CASE WHEN "+sourcePingedExpr+
		" THEN %d ELSE %d END)", view.PingedSince, view.Cutoff, pull)
}

// sourcePingedExpr is true for a source something watches on the one-minute ping
// cadence: our own friendship ping, or a friend's, relayed as a freshness hint.
// It takes the hint horizon as its one argument (MadnetworkView.PingedSince) and
// is also selected as a column, so the ⓘ panel greys a holder by the same window
// the browse filtered it by.
const sourcePingedExpr = `(COALESCE(p.state,'') = 'friend' OR s.hinted_at >= %d)`

// sourcePinged renders sourcePingedExpr as a selectable column. Without a
// horizon there is nothing to divide the sources by, so the column reads
// constant-true and every row is judged on the one window.
func sourcePinged(view MadnetworkView) string {
	if view.PingedSince <= 0 {
		return "1"
	}
	return fmt.Sprintf(sourcePingedExpr, view.PingedSince)
}

// reachable applies the view's own windows to a row judged after the query — the
// summary strip, which lists every source and greys the stale ones rather than
// filtering them out.
func (v MadnetworkView) reachable(lastSeen int64, pinged bool) bool {
	return ReachableAt(lastSeen, pinged, v.Cutoff, v.PullCutoff)
}

// ReachableAt is the Go twin of reachClause, for the callers that judge a row
// after it has been selected. The ⓘ panel's holder greying passes its own
// cutoffs rather than the view's, deliberately: when the browse fails open and
// shows everything, a stale holder must still read as stale instead of every
// holder suddenly looking reachable.
func ReachableAt(lastSeen int64, pinged bool, cutoff, pullCutoff int64) bool {
	if cutoff <= 0 {
		return true
	}
	if !pinged && pullCutoff > 0 && pullCutoff < cutoff {
		return lastSeen >= pullCutoff
	}
	return lastSeen >= cutoff
}

// sourceClause narrows a cached-row query to one source (the "By node" shelf).
// The id is an int64 the handler parsed out of the query string — a value that
// cannot carry SQL whatever it holds — so it is inlined like reachClause's
// cutoff rather than threaded as a bind through every shared fragment.
func sourceClause(view MadnetworkView) string {
	switch {
	case view.SourceID == NoSourceID:
		// A node we hold nothing from: constant-false rather than unfiltered, so
		// the empty shelf holds even in the queries that read this clause
		// directly instead of going through includeRemote.
		return " AND 0"
	case view.SourceID <= 0:
		return ""
	}
	return fmt.Sprintf(" AND s.id = %d", view.SourceID)
}

// The browse queries group by DISPLAY identity — the grouping artist is the
// album artist, falling back to the performer, falling back to the unknown
// bucket, mirroring the local library's album-artist-only artist list; albums
// fall back to the shared "Other" bucket. Only reachable, unblocked sources'
// catalogs are visible (reachClause; cutoff <= 0 = every source).
//
// pkey is the row's other artist credit — its PERFORMER, empty when the row
// carries none. It is what lets an artist be browsed for the tracks they play on
// under somebody else's album artist (fedcatCreditBase).
func fedcatBase(view MadnetworkView) string {
	return `
	FROM (SELECT COALESCE(NULLIF(c.album_artist, ''), NULLIF(c.artist, ''), '` + DefaultArtistName + `') AS akey,
	             COALESCE(c.artist, '') AS pkey,
	             COALESCE(NULLIF(c.album, ''), '` + DefaultAlbumTitle + `') AS alb,
	             ` + sourceLabelExpr + ` AS source_label,
	             ` + srcLastSeen + ` AS source_last_seen,
	             ` + sourcePinged(view) + ` AS source_pinged,
	             s.public_key AS source_key,
	             c.*
	      FROM federation_catalog c` + sourceJoin("c") + `
	      WHERE ` + notBlocked + reachClause(view) + sourceClause(view) + `)`
}

// fedcatRemoteRows / fedcatSelfRows are the two sources of the merged counting
// queries (artists / albums / summary / search), reduced to the columns those
// queries group and count by. The self source is this node's own published set
// — the same visibleTagset predicate and display-name resolution that
// PublishedCatalog advertises to friends, with fedcatBase's bucket fallbacks
// applied on top, so a track we publish folds with the same track cached from
// a friend (docs/ui/madnetwork-page.md §Own tracks).
func fedcatRemoteRows(view MadnetworkView) string {
	return `
	SELECT COALESCE(NULLIF(c.album_artist, ''), NULLIF(c.artist, ''), '` + DefaultArtistName + `') AS akey,
	       COALESCE(c.artist, '') AS pkey,
	       COALESCE(NULLIF(c.album, ''), '` + DefaultAlbumTitle + `') AS alb,
	       c.title AS title, c.track_number AS track_number,
	       c.disc_number AS disc_number, c.year AS year
	FROM federation_catalog c` + sourceJoin("c") + `
	WHERE ` + notBlocked + reachClause(view) + sourceClause(view)
}

// selfPublishedClause keeps the self-merged rows to what this node actually
// publishes: a recording is on the network iff its effective depth reaches at
// least a direct friend (F5). The depth is a server-resolved integer, never user
// input, so it is inlined like reachClause's cutoff rather than threaded as a
// bind parameter through every shared fragment.
//
// Empty for a view that asked for the whole local library (AllOwn) — the one
// place on this page that is about our own shelf rather than about the network.
func selfPublishedClause(view MadnetworkView) string {
	if view.AllOwn {
		return ""
	}
	return fmt.Sprintf(" AND COALESCE(r.share_depth, %d) >= %d", view.DefaultShareDepth, federation.DepthFriends)
}

func fedcatSelfRows(view MadnetworkView) string {
	return `
	SELECT COALESCE(NULLIF(COALESCE(aar.name, m.album_artist, ''), ''),
	                NULLIF(COALESCE(par.name, m.artist, ''), ''), '` + DefaultArtistName + `') AS akey,
	       COALESCE(par.name, m.artist, '') AS pkey,
	       COALESCE(NULLIF(COALESCE(al.title, m.album, ''), ''), '` + DefaultAlbumTitle + `') AS alb,
	       m.title AS title, m.track_number AS track_number,
	       m.disc_number AS disc_number, m.year AS year
	FROM tagsets m` + recordingJoin + `
	LEFT JOIN artists par ON par.id = m.artist_id
	LEFT JOIN artists aar ON aar.id = m.album_artist_id
	LEFT JOIN albums al   ON al.id  = m.album_id
	WHERE ` + visibleTagset + selfPublishedClause(view)
}

// fedcatCountBase is the FROM clause of the counting queries: reachable friends'
// catalogs (cutoff), optionally unioned with the own published set (always
// available — self is never gated). includeSelf is off when federation is
// disabled — the page then stays what the friends provide (nothing), matching
// the "list fully clears" rule. A source-filtered view (the "By node" shelf)
// keeps exactly one of the two halves.
func fedcatCountBase(view MadnetworkView) string {
	return ` FROM ` + fedcatRowSource(view)
}

// fedcatRowSource is that FROM clause's parenthesized row set on its own, for
// the queries that wrap it in something else (fedcatCreditBase).
func fedcatRowSource(view MadnetworkView) string {
	switch {
	case view.includeRemote() && view.includeOwn():
		return `(` + fedcatRemoteRows(view) + ` UNION ALL ` + fedcatSelfRows(view) + `)`
	case view.includeRemote():
		return `(` + fedcatRemoteRows(view) + `)`
	case view.includeOwn():
		return `(` + fedcatSelfRows(view) + `)`
	default:
		return `(` + fedcatNoRows + `)`
	}
}

// fedcatNoRows is a row source shaped like the other two and guaranteed empty —
// the view that includes neither half (§includeOwn). A well-typed nothing keeps
// every query above it unchanged; a special case at each call site would not.
const fedcatNoRows = `
	SELECT '' AS akey, '' AS pkey, '' AS alb, '' AS title, NULL AS track_number,
	       NULL AS disc_number, NULL AS year
	WHERE 0`

// fedcatCreditBase is the row source of the ARTIST-scoped queries: every row
// once per artist credit it carries — its album-artist bucket, and, when the
// performer differs, that performer too (album_artist_credit tells the two
// apart). It mirrors the local library, where an artist entity is browsable in
// EITHER role (database/library.go, listAlbumsByArtistID's `al.artist_id = ? OR
// m.artist_id = ?`): a compilation is filed under its album artist AND under
// each performer on it, counting only that performer's tracks.
//
// The counting queries (summary, lanes, album search) keep fedcatCountBase: a
// track has ONE identity there, and counting it once per credit would inflate
// every total on the page.
//
// Which of those buckets the A-Z list SHOWS is the artist list's own rule, not
// this one's — see MadnetworkArtists.
func fedcatCreditBase(view MadnetworkView) string {
	return ` FROM (WITH fedcat_rows AS (SELECT * FROM ` + fedcatRowSource(view) + `)
	      SELECT akey, alb, title, track_number, disc_number, year, 1 AS album_artist_credit
	      FROM fedcat_rows
	      UNION ALL
	      SELECT pkey AS akey, alb, title, track_number, disc_number, year, 0 AS album_artist_credit
	      FROM fedcat_rows
	      WHERE pkey <> '' AND lower(pkey) <> lower(akey))`
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

// trackFullIdent is that identity across the whole merged view rather than
// inside one artist bucket — the key the discovery lanes rank and the counting
// queries count distinct.
const trackFullIdent = `lower(akey) || char(31) || ` + trackIdent

// MadnetworkArtist is one row of the merged artist list.
type MadnetworkArtist struct {
	Name   string `json:"name"`
	Albums int64  `json:"albums"`
	Tracks int64  `json:"tracks"`
}

// MadnetworkArtists lists the merged catalog's artists — the A-Z browse of the
// network and of a single node's shelf — optionally filtered by a substring. The
// unknown bucket sorts last; includeSelf merges the own published set in.
//
// It lists the ALBUM ARTISTS, exactly as the local library's list does
// (database/library.go, listArtists): an artist who only ever plays on somebody
// else's release is not a row here (HAVING MAX(album_artist_credit) = 1), while
// an artist who has a release of their own is — and their guest appearances are
// counted and browsable under their name too, because the rows come from
// fedcatCreditBase. A performer with no release of their own stays reachable
// through search (MadnetworkSearchArtists), which is where the library leaves
// them as well.
//
// The list is keyset-paged: limit <= 0 returns everything, otherwise one page
// plus the cursor for the next. Browse all is now the community's whole output
// rather than a few friends' libraries, which is what took this off the "adopt
// when catalogs grow" list.
func (db *DB) MadnetworkArtists(ctx context.Context, q string, view MadnetworkView, limit int, cursor string) ([]*MadnetworkArtist, string, error) {
	return db.madnetworkArtists(ctx, q, view, limit, cursor, true)
}

// MadnetworkSearchArtists is the search counterpart: the same buckets WITHOUT
// the album-artist rule, so a performer who only appears on other artists'
// releases is a hit and their appearances are one click away. The local library
// splits the two the same way (its Search matches either role, its browse list
// only the album-artist one), so a name that is missing from the A-Z grid on one
// page is missing from it on both — and found by searching on both.
func (db *DB) MadnetworkSearchArtists(ctx context.Context, q string, limit int, view MadnetworkView) ([]*MadnetworkArtist, error) {
	if strings.TrimSpace(q) == "" || limit <= 0 {
		return []*MadnetworkArtist{}, nil
	}
	out, _, err := db.madnetworkArtists(ctx, q, view, limit, "", false)
	if out == nil {
		out = []*MadnetworkArtist{}
	}
	return out, err
}

func (db *DB) madnetworkArtists(ctx context.Context, q string, view MadnetworkView, limit int, cursor string, albumArtistsOnly bool) ([]*MadnetworkArtist, string, error) {
	conds, args := []string{}, []any{}
	if s := strings.TrimSpace(q); s != "" {
		escaped := strings.NewReplacer(`%`, `\%`, `_`, `\_`).Replace(s)
		conds = append(conds, `lower(akey) LIKE lower(?) ESCAPE '\'`)
		args = append(args, "%"+escaped+"%")
	}
	if c, ok := decodeArtistCursor(cursor); ok {
		// Row-value comparison over the ORDER BY key: the bucket flag first (the
		// unknown bucket sorts last), then the folded name. Applied row-level
		// because the grouping key is lower(akey), which is constant per group.
		// The cursor type is the library's — same sort key, minus the id a merged
		// row has no equivalent of (the folded name IS the group).
		conds = append(conds, `((`+artistBucketExpr+`), lower(akey)) > (?, ?)`)
		args = append(args, c.Unknown, c.Name)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}
	page := ""
	if limit > 0 {
		page = " LIMIT ?"
		args = append(args, limit+1) // one extra row = "there is a next page"
	}
	// The album-artist rule is a HAVING, not a WHERE: a bucket qualifies on one
	// credit and is then counted over ALL of them, so a guest appearance adds to
	// the artist's totals without ever putting a pure performer in the list.
	having := ""
	if albumArtistsOnly {
		having = " HAVING MAX(album_artist_credit) = 1"
	}
	rows, err := db.QueryContext(ctx, `
		SELECT MIN(akey), COUNT(DISTINCT lower(alb)), COUNT(DISTINCT `+trackIdent+`)
		`+fedcatCreditBase(view)+where+`
		GROUP BY lower(akey)`+having+`
		ORDER BY `+artistBucketLast+`, lower(akey)`+page, args...)
	if err != nil {
		return nil, "", fmt.Errorf("madnetwork artists: %w", err)
	}
	defer rows.Close()
	var out []*MadnetworkArtist
	for rows.Next() {
		var a MadnetworkArtist
		if err := rows.Scan(&a.Name, &a.Albums, &a.Tracks); err != nil {
			return nil, "", fmt.Errorf("scan madnetwork artist: %w", err)
		}
		out = append(out, &a)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if limit > 0 && len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1].Name
		c := artistCursor{Name: strings.ToLower(last)}
		if strings.EqualFold(last, DefaultArtistName) {
			c.Unknown = 1
		}
		next = encodeArtistCursor(c)
	}
	return out, next, nil
}

// artistBucketExpr is artistBucketLast without its sort direction — the same
// flag as a value, so the keyset cursor compares exactly what the ORDER BY
// orders by.
const artistBucketExpr = `lower(akey) = lower('` + DefaultArtistName + `')`

// MadnetworkAlbum is one row of an artist's merged album list.
type MadnetworkAlbum struct {
	Title  string `json:"title"`
	Tracks int64  `json:"tracks"`
	Year   *int64 `json:"year,omitempty"`
}

// MadnetworkAlbums lists one artist's albums in the merged catalog; the
// "Other" bucket sorts last.
//
// "Their" albums means either credit (fedcatCreditBase): the releases filed
// under this artist, plus the ones they only play on — those counting just their
// own tracks, which is the same hybrid count the library's drill-down shows.
func (db *DB) MadnetworkAlbums(ctx context.Context, artist string, view MadnetworkView) ([]*MadnetworkAlbum, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT MIN(alb), COUNT(DISTINCT `+trackIdent+`), MAX(year)
		`+fedcatCreditBase(view)+`
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
// (source, appearance), a source being another node's cached catalog or, for
// Self rows, this node's own published set. The handler merges rows into
// logical tracks and their "N versions" (distinct claimed recordings) — set
// logic that reads better in Go than SQL at album scale.
type MadnetworkTrackRow struct {
	// SourceID / SourceName / SourceLastSeen describe the node the row came
	// from. Since F7 item 5 that node is not necessarily a friend — it is any
	// member of our community whose catalog the frontier has cached — so the
	// display name falls back through the admin's label, the node's own claim
	// and its short key.
	SourceID       int64
	SourceName     string
	SourceLastSeen int64
	// SourcePinged reports which freshness window this node is judged by — true
	// for one something pings every minute (our friend, or a friend's friend
	// vouching for it), false for one reached only by the catalog rotation (F7
	// item 10). The ⓘ panel needs it to grey a holder by the same window the
	// browse filtered it by; without it a perfectly fresh member reads as stale.
	SourcePinged bool
	// SourceKey is the node's public key — the map address of a holder, so the
	// library's ⓘ list can link one (F7 item 7). Empty on Self rows.
	SourceKey string
	Entry     federation.CatalogEntry

	// GroupArtist/GroupAlbum are the display-identity buckets (akey/alb) the
	// row belongs to — the search handler groups cross-album results by them.
	GroupArtist string
	GroupAlbum  string

	// Self rows come from the local library: SourceID is 0, Entry.Key is the
	// local tagset id, and ObjectKeys maps each rendition hash to its local
	// files object key (for direct /files/ play URLs).
	Self       bool
	ObjectKeys map[string]string
}

// remoteTrackRows runs the raw cached-row query with a caller-supplied match
// clause over the bucketed columns (akey/alb/title available). cutoff gates the
// rows to reachable sources (cutoff <= 0 = all).
func (db *DB) remoteTrackRows(ctx context.Context, view MadnetworkView, match string, args ...any) ([]*MadnetworkTrackRow, error) {
	if !view.includeRemote() {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT source_id, source_label, source_last_seen, source_pinged, source_key, akey, alb,
		       entry_key, recording_key, title, artist, album_artist,
		       COALESCE(genre, ''), year, track_number, disc_number,
		       COALESCE(duration, 0), COALESCE(license, ''), guest_playable, renditions
		`+fedcatBase(view)+`
		WHERE `+match+`
		ORDER BY (disc_number IS NULL) ASC, disc_number ASC, track_number ASC, lower(title) ASC, source_id ASC`,
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
		if err := rows.Scan(&r.SourceID, &r.SourceName, &r.SourceLastSeen, &r.SourcePinged, &r.SourceKey, &r.GroupArtist, &r.GroupAlbum,
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

// MadnetworkTracks returns reachable sources' cached rows for one artist+album,
// in display order (the view gates reachability and, for a single node's shelf,
// which source may answer at all).
//
// The artist is matched in either credit, so an album reached through a
// performer's list (MadnetworkAlbums) opens on the tracks that put it there
// rather than on nothing.
func (db *DB) MadnetworkTracks(ctx context.Context, artist, album string, view MadnetworkView) ([]*MadnetworkTrackRow, error) {
	return db.remoteTrackRows(ctx, view,
		`(lower(akey) = lower(?) OR lower(pkey) = lower(?)) AND lower(alb) = lower(?)`, artist, artist, album)
}

// Self-row display-identity expressions over the tagsets join (aliases par /
// aar / al as in fedcatSelfRows) — the WHERE side of the bucket fallbacks.
const selfAkeyExpr = `COALESCE(NULLIF(COALESCE(aar.name, m.album_artist, ''), ''),
	NULLIF(COALESCE(par.name, m.artist, ''), ''), '` + DefaultArtistName + `')`
const selfPkeyExpr = `COALESCE(par.name, m.artist, '')`
const selfAlbExpr = `COALESCE(NULLIF(COALESCE(al.title, m.album, ''), ''), '` + DefaultAlbumTitle + `')`

// ownTrackRows returns this node's own appearances matching a caller-supplied
// clause (akey/alb/title-level), shaped like cached catalog rows: Self = true,
// SourceID 0, Entry.Key = tagset id, renditions attached from the recording's
// live files with their local object keys. It applies the same self-published
// filter as the counting queries, so a recording kept off the network cannot be
// listed by a view whose counts already exclude it — unless the view asked for
// the whole local library (AllOwn), where the counts include it too.
func (db *DB) ownTrackRows(ctx context.Context, view MadnetworkView, match string, args ...any) ([]*MadnetworkTrackRow, error) {
	if !view.includeOwn() {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.recording_id, `+selfAkeyExpr+`, `+selfAlbExpr+`, m.title,
		       COALESCE(par.name, m.artist, ''), COALESCE(aar.name, m.album_artist, ''),
		       COALESCE(m.genre, ''), m.year, m.track_number, m.disc_number,
		       COALESCE(r.license, ''), r.guest_playable
		FROM tagsets m`+recordingJoin+`
		LEFT JOIN artists par ON par.id = m.artist_id
		LEFT JOIN artists aar ON aar.id = m.album_artist_id
		LEFT JOIN albums al   ON al.id  = m.album_id
		WHERE `+visibleTagset+selfPublishedClause(view)+` AND `+match+`
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
// the Self side of the merged track view, matched on the same two credits as
// the remote side.
func (db *DB) MadnetworkOwnTracks(ctx context.Context, artist, album string, view MadnetworkView) ([]*MadnetworkTrackRow, error) {
	return db.ownTrackRows(ctx, view,
		`(lower(`+selfAkeyExpr+`) = lower(?) OR lower(`+selfPkeyExpr+`) = lower(?)) AND lower(`+selfAlbExpr+`) = lower(?)`,
		artist, artist, album)
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
	rows, err := db.remoteTrackRows(ctx, view, `lower(title) LIKE lower(?) ESCAPE '\'`, escaped)
	if err != nil {
		return nil, err
	}
	own, err := db.ownTrackRows(ctx, view, `lower(m.title) LIKE lower(?) ESCAPE '\'`, escaped)
	if err != nil {
		return nil, err
	}
	rows = append(rows, own...)
	if len(rows) > searchRowCap {
		rows = rows[:searchRowCap]
	}
	return rows, nil
}

// MadnetworkNode is one node as the /madnetwork surfaces list it: the Nodes
// lane, the directory at /madnetwork/nodes, and the card on a node's own page
// (docs/ui/madnetwork-nodes.md). Every node whose catalog this node caches is
// listed, reachable or not; Reachable drives the greying of one seen longer ago
// than the view's freshness window, and Friend distinguishes the nodes an admin
// hand-picked from the members the frontier reached on its own.
//
// It is not called "friend" any more because it stopped being one at F7 item 5,
// when the sweep learned to pull from members nobody here chose.
type MadnetworkNode struct {
	// ID is the catalog-source row. Useful within one server's session; it is
	// NOT this node's address — a source evicted past discovery_cap comes back
	// with a different id, so the URL of a node page is its Key.
	ID int64 `json:"id"`
	// Key is the node's public key: its identity everywhere, and what the node
	// page is addressed by.
	Key       string `json:"key"`
	Name      string `json:"name"`
	LastSeen  int64  `json:"last_seen"`
	SyncedAt  int64  `json:"synced_at"`
	Entries   int64  `json:"entries"`
	Reachable bool   `json:"reachable"`
	Friend    bool   `json:"friend"`
	// Pinged reports which freshness window judged this node — see
	// MadnetworkTrackRow.SourcePinged. Not serialized: the client is told the
	// verdict (Reachable), not the arithmetic behind it.
	Pinged bool `json:"-"`
}

// MadnetworkSummary reports the merged catalog's shape: every source with sync
// state and reachability, plus the merged distinct track count over the visible
// (reachable + own) set. The source list is not filtered by reachability — the
// surfaces show them all and grey the unreachable — but the track count uses the
// view's cutoff.
//
// The ORDER BY here is a deterministic base, not the order a reader sees: nodes
// are listed by hops first, and SQL cannot know hops (the graph is the
// federation node's, not a table this joins). The handler sorts.
func (db *DB) MadnetworkSummary(ctx context.Context, view MadnetworkView) ([]*MadnetworkNode, int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.id, s.public_key, `+sourceLabelExpr+`, `+srcLastSeen+`, s.catalog_synced_at,
		       (SELECT COUNT(*) FROM federation_catalog c WHERE c.source_id = s.id),
		       COALESCE(p.state, '') = 'friend', `+sourcePinged(view)+`
		FROM federation_catalog_sources s
		LEFT JOIN federation_peers p ON p.public_key = s.public_key
		WHERE `+notBlocked+`
		  AND (COALESCE(p.state, '') = 'friend'
		       OR EXISTS (SELECT 1 FROM federation_catalog c2 WHERE c2.source_id = s.id))
		ORDER BY (COALESCE(p.state, '') = 'friend') DESC, lower(`+sourceLabelExpr+`), s.id`)
	if err != nil {
		return nil, 0, fmt.Errorf("madnetwork summary: %w", err)
	}
	defer rows.Close()
	var nodes []*MadnetworkNode
	for rows.Next() {
		var n MadnetworkNode
		if err := rows.Scan(&n.ID, &n.Key, &n.Name, &n.LastSeen, &n.SyncedAt, &n.Entries, &n.Friend, &n.Pinged); err != nil {
			return nil, 0, fmt.Errorf("scan madnetwork source: %w", err)
		}
		n.Reachable = view.reachable(n.LastSeen, n.Pinged)
		nodes = append(nodes, &n)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}

	var tracks int64
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT `+trackFullIdent+`)
		`+fedcatCountBase(view)).Scan(&tracks); err != nil {
		return nil, 0, fmt.Errorf("madnetwork track count: %w", err)
	}
	return nodes, tracks, nil
}

// MadnetworkOwnEntries counts the distinct tracks this node publishes to the
// network — the "entries" figure beside our own name in the node list, counted
// the same way a friend's is (distinct display identities, not blobs) so the two
// numbers on one screen mean the same thing.
//
// Scoped by the caller's view: it is what the NETWORK can see of us, so a
// recording held back to Local is not in it (selfPublishedClause, F5).
func (db *DB) MadnetworkOwnEntries(ctx context.Context, view MadnetworkView) (int64, error) {
	own := MadnetworkView{IncludeSelf: true, SelfOnly: true, DefaultShareDepth: view.DefaultShareDepth}
	var n int64
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT `+trackFullIdent+`)
		`+fedcatCountBase(own)).Scan(&n); err != nil {
		return 0, fmt.Errorf("madnetwork own entry count: %w", err)
	}
	return n, nil
}

// MadnetworkSourceByKey resolves a node's public key to the catalog source we
// cache for it. Found is false when we hold no catalog from that node — which is
// an ordinary state of the frontier rotation (we can place a node on the graph
// long before its turn to be pulled from comes up), not an error.
//
// Blocked sources are excluded on the same terms as every browse query: blocking
// is decided by the query, because it must be instant.
func (db *DB) MadnetworkSourceByKey(ctx context.Context, key string, view MadnetworkView) (*MadnetworkNode, bool, error) {
	var n MadnetworkNode
	err := db.QueryRowContext(ctx, `
		SELECT s.id, s.public_key, `+sourceLabelExpr+`, `+srcLastSeen+`, s.catalog_synced_at,
		       (SELECT COUNT(*) FROM federation_catalog c WHERE c.source_id = s.id),
		       COALESCE(p.state, '') = 'friend', `+sourcePinged(view)+`
		FROM federation_catalog_sources s
		LEFT JOIN federation_peers p ON p.public_key = s.public_key
		WHERE `+notBlocked+` AND s.public_key = ?`, key).
		Scan(&n.ID, &n.Key, &n.Name, &n.LastSeen, &n.SyncedAt, &n.Entries, &n.Friend, &n.Pinged)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("madnetwork source by key: %w", err)
	}
	n.Reachable = view.reachable(n.LastSeen, n.Pinged)
	return &n, true, nil
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
