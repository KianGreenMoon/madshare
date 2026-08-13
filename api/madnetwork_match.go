package api

// Federation F8 item 1 — what the madnetwork says about a recording under review
// (docs/architecture/federation.md §Quality upgrades, "The match arm on the
// review card").
//
// The classify endpoint already answers "what would an approve change" in the
// library's own terms. This answers the same question looking outward, and it is
// deliberately the *only* thing it does: the arm is advisory, offers no action
// beyond filling the edit form, and never starts a transfer. Approving is still
// the moderator's act, made with more in front of them.

import (
	"context"
	"sort"
	"strings"
	"time"

	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

// networkTake is the arm's payload: the names other nodes give this audio, and
// the renditions of it they hold. Both are claims — nothing here is verified
// until bytes arrive and the analysis pipeline re-derives it locally.
type networkTake struct {
	// Tagsets are the distinct labellings, most independent voices first. The
	// leader is the dominant label; a mislabel arriving in the queue shows up as a
	// minority entry beside it, which is §Trust graph layer 1 made visible.
	Tagsets []networkTagset `json:"tagsets,omitempty"`
	// Renditions are the remote encodings, ladder-best first, each flagged for
	// whether it would beat what the recording already has.
	Renditions []networkRendition `json:"renditions,omitempty"`
	// Fingerprinted reports whether stage 2 of the join could run at all. False
	// means the local blob has no fingerprint yet, so the answer covers only nodes
	// holding our exact bytes — worth saying, because an empty arm otherwise reads
	// as "the network knows nothing about this".
	Fingerprinted bool `json:"fingerprinted"`
}

type networkTagset struct {
	Title       string `json:"title"`
	Artist      string `json:"artist,omitempty"`
	AlbumArtist string `json:"album_artist,omitempty"`
	Album       string `json:"album,omitempty"`
	Genre       string `json:"genre,omitempty"`
	Year        *int64 `json:"year,omitempty"`
	Track       *int64 `json:"track_number,omitempty"`
	Disc        *int64 `json:"disc_number,omitempty"`

	// Voices is the branch-weighted count this list is ordered by, and Holders
	// the raw node count. Both are always sent here, unlike on /madnetwork: a
	// moderator deciding whether a label is the network's consensus is exactly
	// the reader who needs to see when eight nodes are one voice.
	Voices  int                `json:"voices"`
	Holders []madnetworkHolder `json:"holders"`
	// Match is how these nodes were tied to our recording (hash or fingerprint),
	// and BER the measurement when it was a fingerprint. Evidence travels with
	// the claim, so nothing on the card asks to be taken on trust.
	Match string  `json:"match"`
	BER   float64 `json:"ber,omitempty"`
}

type networkRendition struct {
	Hash       string  `json:"hash"`
	Codec      string  `json:"codec,omitempty"`
	Bitrate    int64   `json:"bitrate,omitempty"`
	SampleRate int64   `json:"sample_rate,omitempty"`
	BitDepth   int64   `json:"bit_depth,omitempty"`
	ByteSize   int64   `json:"byte_size,omitempty"`
	Duration   float64 `json:"duration,omitempty"`

	// Better is true when this claimed rendition outranks the recording's current
	// ladder-best. Claimed: the ladder is fed the origin's own tech facts, which
	// only become facts about bytes once the bytes are here.
	Better bool `json:"better,omitempty"`
	// Held marks a rendition this node already has — the common case for a
	// downloaded track, and the reason a hash match is not automatically news.
	Held    bool               `json:"held,omitempty"`
	Holders []madnetworkHolder `json:"holders"`
}

// networkTakeOn builds the arm for one recording.
//
// nil and empty mean different things, and the card says different words for
// them: nil is "this node has no madnetwork to ask" (federation off, or a cache
// read that failed — a failure must never cost the moderator the classification
// they came for), while an empty take is "we asked, and nobody out there knows
// this audio". Collapsing the two would put a false statement on one of them.
func (h *handler) networkTakeOn(ctx context.Context, recordingID int64, currentBest *database.Rendition) *networkTake {
	if h.madnetwork == nil || recordingID == 0 {
		return nil
	}
	matches, err := h.madnetwork.MatchRecording(ctx, recordingID, time.Now().Unix()-h.reachWindow())
	if err != nil {
		return nil
	}
	opts := h.mergeOpts(ctx)
	take := &networkTake{}
	for _, m := range matches {
		if m.Match == database.MatchFingerprint {
			take.Fingerprinted = true
		}
	}
	take.Tagsets = foldMatchTagsets(matches, opts)
	take.Renditions = foldMatchRenditions(matches, opts, currentBest)
	return take
}

// tagsetIdentity is the folding key for "the same labelling": the text a
// moderator is comparing, case-folded. Deliberately not the library's appearance
// identity key — that one is built from resolved local entity ids, and these are
// other people's strings, which is exactly what is being judged here.
func tagsetIdentity(e federation.CatalogEntry) string {
	return strings.ToLower(strings.Join([]string{
		strings.TrimSpace(e.Title), strings.TrimSpace(e.Artist),
		strings.TrimSpace(e.AlbumArtist), strings.TrimSpace(e.Album),
	}, "\x00"))
}

// foldMatchTagsets groups the matched entries by their text and orders them by
// independent voices — the same rule and the same reason as the version ordering
// on /madnetwork: a farm of keys behind one friendship is one opinion about what
// this audio is called, however many nodes it runs.
func foldMatchTagsets(matches []database.NetworkMatch, opts mergeOpts) []networkTagset {
	type group struct {
		out  networkTagset
		keys []string
	}
	byText := map[string]*group{}
	var order []string
	for _, m := range matches {
		id := tagsetIdentity(m.Entry)
		g, ok := byText[id]
		if !ok {
			g = &group{out: networkTagset{
				Title: m.Entry.Title, Artist: m.Entry.Artist,
				AlbumArtist: m.Entry.AlbumArtist, Album: m.Entry.Album,
				Genre: m.Entry.Genre, Year: m.Entry.Year,
				Track: m.Entry.TrackNumber, Disc: m.Entry.DiscNumber,
				Match: m.Match, BER: m.BER,
			}}
			byText[id] = g
			order = append(order, id)
		}
		// A group carrying both kinds of evidence reports the stronger one: an
		// exact byte match is not weakened by a measured one arriving beside it.
		if m.Match == database.MatchHash {
			g.out.Match, g.out.BER = database.MatchHash, 0
		}
		g.out.Holders = append(g.out.Holders, holderOf(m, opts))
		g.keys = append(g.keys, m.Source.PublicKey)
	}
	out := make([]networkTagset, 0, len(order))
	for _, id := range order {
		g := byText[id]
		g.out.Voices = opts.branches.Voices(g.keys, false)
		out = append(out, g.out)
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Voices != out[b].Voices {
			return out[a].Voices > out[b].Voices
		}
		return len(out[a].Holders) > len(out[b].Holders)
	})
	return out
}

// foldMatchRenditions collects the distinct remote encodings, ranks them on the
// local quality ladder and marks the ones that would beat what we hold.
func foldMatchRenditions(matches []database.NetworkMatch, opts mergeOpts, currentBest *database.Rendition) []networkRendition {
	byHash := map[string]*networkRendition{}
	var ranked []database.Rendition
	for _, m := range matches {
		for _, rd := range m.Entry.Renditions {
			if rd.Hash == "" {
				continue
			}
			r, ok := byHash[rd.Hash]
			if !ok {
				r = &networkRendition{
					Hash: rd.Hash, Codec: rd.Codec, Bitrate: rd.Bitrate,
					SampleRate: rd.SampleRate, BitDepth: rd.BitDepth,
					ByteSize: rd.Size, Duration: rd.Duration,
					// A hash match names the blob we and they share; every other
					// rendition of that entry is one we do not hold.
					Held: m.Match == database.MatchHash && m.SharedHash == rd.Hash,
				}
				byHash[rd.Hash] = r
				ranked = append(ranked, database.Rendition{
					Hash: rd.Hash, Codec: rd.Codec, Bitrate: int(rd.Bitrate),
					SampleRate: int(rd.SampleRate), BitDepth: int(rd.BitDepth),
					ByteSize: rd.Size,
				})
			}
			r.Holders = append(r.Holders, holderOf(m, opts))
		}
	}
	if currentBest != nil {
		// "Better" is decided by ranking the claim against ours and seeing which
		// one the ladder puts first — the same function, so the card can never
		// disagree with the recording page about what better means.
		for hash, r := range byHash {
			pair := database.RankRenditions([]database.Rendition{*currentBest, {
				Hash: hash, Codec: r.Codec, Bitrate: int(r.Bitrate),
				SampleRate: int(r.SampleRate), BitDepth: int(r.BitDepth),
				ByteSize: r.ByteSize,
			}})
			r.Better = !r.Held && pair[0].Hash == hash
		}
	}
	out := make([]networkRendition, 0, len(byHash))
	for _, rr := range database.RankRenditions(ranked) {
		out = append(out, *byHash[rr.Hash])
	}
	return out
}

// holderOf renders one matched source as a holder, greyed by the same freshness
// rule the /madnetwork ⓘ panel uses. A stale holder is shown and dimmed, never
// dropped: the match is a true fact about the network whether or not that node
// answered a ping in the last three minutes.
func holderOf(m database.NetworkMatch, opts mergeOpts) madnetworkHolder {
	return madnetworkHolder{
		Name:     m.Source.Display(),
		LastSeen: m.Source.LastSeen,
		Key:      m.Source.PublicKey,
		Reachable: opts.reach.ok(database.SourceReach{
			LastSeen: m.Source.LastSeen, UnreachableAt: m.Down, Pinged: m.Pinged,
		}),
	}
}
