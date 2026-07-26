package federation

// Friend-list gossip (federation F6): the two record types nodes relay, their
// canonical encoding, and signing/verification. Design:
// docs/architecture/federation.md §"Friend-list gossip & the network graph".
//
// Untagged, like federation.go and for the same reason: the database layer
// stores these records and is built in both tag variants, so the types (and the
// verification the store gates admission on) must exist without the yggdrasil
// dependencies. Only the wire and the sync loop need -tags !nofederation.
//
// The shape of the whole feature follows from one property: a node relays
// records it did not write. A friend hands us a record authored by a node we
// have never met, so the sender's mesh address — which authenticates everything
// else in this package (friendship.go, "the source address *is* proof of key
// possession") — proves nothing about the record's contents. The author's own
// signature is what carries the claim across the hop, and it is why a relay can
// withhold a record but never forge one.
//
// That also fixes how the signature is computed. A record is verified against
// the *bytes as received*, never against a re-marshaled struct: a relay running
// an older build must be able to pass along a record carrying fields it cannot
// parse, and re-serializing would silently drop them and break the signature for
// everyone downstream. [signingInput] therefore canonicalizes the raw JSON —
// unknown fields and all — instead of the parsed value.

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Bounds on a single record. These are anti-flood limits, not policy: they cap
// what one document can cost a receiver, and are checked before anything is
// stored (docs/architecture/federation.md §Friend-list gossip, "Anti-flood
// bounds").
const (
	// MaxGraphEdges caps the friendships one record may claim. A longer list is
	// *refused*, not truncated — a truncated friend list is a false statement
	// about a node's edges, and the map would draw it as fact.
	MaxGraphEdges = 512
	// MaxMarksPerRecord caps the distrust marks one record may carry, for the
	// same reason and with the same effect.
	MaxMarksPerRecord = 512
	// MaxMarkReasonRunes caps a mark's free text. Unlike the list lengths this
	// truncates rather than refuses: an over-long reason is still a usable
	// reason, and the peer-name rules (runes, never bytes — see CleanPeerName)
	// apply because it is remote input rendered in a UI.
	MaxMarkReasonRunes = 280
	// MaxOriginsPerBranch caps how many nodes any single friend may introduce
	// into our store. A sybil farm is cheap to mint and arrives through exactly
	// one edge, so the bound is per-branch rather than global: an honest friend
	// with a large network is unaffected, and a farm hits a ceiling that a
	// single block then clears entirely.
	MaxOriginsPerBranch = 5000
)

// sigField is the record field the signature itself lives in — excluded from
// the bytes it covers, since it cannot sign itself.
const sigField = "sig"

// ErrBadSignature marks a record whose signature does not verify against its
// claimed origin: a forgery, a corrupted relay, or a bug on the far side. The
// receiver drops it silently — there is no honest way to produce one.
var ErrBadSignature = errors.New("federation: gossip record signature does not verify")

// GraphEdge is one friendship as gossiped: the friend's key, the label the
// publisher uses for that friend, and when the friendship was made.
//
// The name is *hearsay about a stranger* — the publisher's private label for a
// node the receiver may never have met — so every surface rendering it shows
// the key beside it (docs/architecture/federation.md §Friendship, naming).
// Since is a durability signal: an old edge is worth more than a fresh one when
// trust weighting arrives (F7).
type GraphEdge struct {
	Key   string `json:"key"`
	Name  string `json:"name,omitempty"`
	Since int64  `json:"since,omitempty"`
}

// GraphRecord is one node's signed statement about its own friendships — the
// unit that propagates. Receivers keep the highest Seq per Origin, so a
// duplicate is dropped without re-propagation and gossip loops terminate
// without hop counts or TTLs.
type GraphRecord struct {
	Protocol int    `json:"protocol"`
	Origin   string `json:"origin"`
	Seq      int64  `json:"seq"`
	// IssuedAt is when the origin signed this record (unix seconds). It drives
	// expiry: a receiver drops the record after the TTL and stops serving it, so
	// an abandoned key fades from every store without anyone acting.
	IssuedAt  int64       `json:"issued_at"`
	Friends   []GraphEdge `json:"friends"`
	Signature string      `json:"sig,omitempty"`
}

// DistrustMark is one published block: whom, when, and why.
//
// The reason is what makes a mark actionable — a bare key is an anonymous
// downvote that forces the reader to ask out-of-band what happened. It is also
// why marks are the most dangerous thing in this file: they relay network-wide
// and are readable by their target (see the accepted risk recorded in
// docs/architecture/federation.md §Friend-list gossip).
type DistrustMark struct {
	Key    string `json:"key"`
	At     int64  `json:"at"`
	Reason string `json:"reason,omitempty"`
}

// MarkRecord is one node's signed distrust list. Separate from [GraphRecord]
// and independently sequenced, so a mark can expire while the friendship record
// it travelled with stays live.
type MarkRecord struct {
	Protocol  int            `json:"protocol"`
	Origin    string         `json:"origin"`
	Seq       int64          `json:"seq"`
	IssuedAt  int64          `json:"issued_at"`
	Marks     []DistrustMark `json:"marks"`
	Signature string         `json:"sig,omitempty"`
}

// signingInput is the canonical byte sequence a record's signature covers: the
// document with its "sig" field removed, object keys sorted, whitespace
// compacted.
//
// It works from raw JSON rather than a parsed struct on purpose. Values are
// carried as [json.RawMessage] and re-emitted verbatim (encoding/json compacts
// them), so a field this build has never heard of still contributes its exact
// bytes to the signature. That is what lets an old relay carry a new node's
// record without invalidating it — the property the whole relay design rests on.
func signingInput(raw []byte) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("federation: gossip record is not a JSON object: %w", err)
	}
	delete(doc, sigField)
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("federation: canonicalize gossip record: %w", err)
	}
	return out, nil
}

// signDocument signs a record with the node's own ed25519 key and returns the
// wire bytes, signature included. The returned bytes are what gets stored and
// relayed — callers must not re-marshal the parsed form (see the file comment).
func signDocument(priv ed25519.PrivateKey, doc any) ([]byte, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("federation: encode gossip record: %w", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("federation: encode gossip record: %w", err)
	}
	delete(m, sigField)
	input, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("federation: canonicalize gossip record: %w", err)
	}
	sig, err := json.Marshal(hex.EncodeToString(ed25519.Sign(priv, input)))
	if err != nil {
		return nil, fmt.Errorf("federation: encode signature: %w", err)
	}
	m[sigField] = sig
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("federation: encode gossip record: %w", err)
	}
	return out, nil
}

// verifySignature checks a record's signature against the key it claims to come
// from. The origin is self-certifying in the sense that matters here: a key that
// verifies the bytes is the key that wrote them, whoever handed them over.
func verifySignature(raw []byte, origin, signature string) error {
	key, err := NormalizeKey(origin)
	if err != nil {
		return fmt.Errorf("federation: gossip record origin: %w", err)
	}
	pub, err := hex.DecodeString(key)
	if err != nil {
		return fmt.Errorf("federation: gossip record origin: %w", err)
	}
	sig, err := hex.DecodeString(strings.TrimSpace(signature))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return ErrBadSignature
	}
	input, err := signingInput(raw)
	if err != nil {
		return err
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), input, sig) {
		return ErrBadSignature
	}
	return nil
}

// SignGraphRecord fills in the protocol version and signs rec, returning the
// wire bytes to publish. Friends are left in the caller's order — the builder
// sorts them (see the own-record build) so a record only changes when the
// friendships do.
func SignGraphRecord(priv ed25519.PrivateKey, rec GraphRecord) ([]byte, error) {
	rec.Protocol = ProtocolVersion
	rec.Signature = ""
	return signDocument(priv, rec)
}

// SignMarkRecord fills in the protocol version and signs rec.
func SignMarkRecord(priv ed25519.PrivateKey, rec MarkRecord) ([]byte, error) {
	rec.Protocol = ProtocolVersion
	rec.Signature = ""
	return signDocument(priv, rec)
}

// ParseGraphRecord validates raw as a signed friend-list record: well-formed
// JSON, a protocol this node speaks, a verifying signature, and edges within
// the anti-flood bounds. Names are sanitized in the returned value for display;
// raw is unchanged and stays the thing to store and relay.
func ParseGraphRecord(raw []byte) (*GraphRecord, error) {
	var rec GraphRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("federation: not a gossip record: %w", err)
	}
	if rec.Protocol != ProtocolVersion {
		return nil, fmt.Errorf("federation: unsupported gossip record version %d (this node speaks %d)", rec.Protocol, ProtocolVersion)
	}
	if err := verifySignature(raw, rec.Origin, rec.Signature); err != nil {
		return nil, err
	}
	origin, err := NormalizeKey(rec.Origin)
	if err != nil {
		return nil, fmt.Errorf("federation: gossip record origin: %w", err)
	}
	rec.Origin = origin
	if rec.Seq < 0 || rec.IssuedAt < 0 {
		return nil, errors.New("federation: gossip record has a negative sequence or timestamp")
	}
	if len(rec.Friends) > MaxGraphEdges {
		return nil, fmt.Errorf("federation: gossip record claims %d friendships (limit %d)", len(rec.Friends), MaxGraphEdges)
	}
	seen := make(map[string]struct{}, len(rec.Friends))
	for i := range rec.Friends {
		key, err := NormalizeKey(rec.Friends[i].Key)
		if err != nil {
			return nil, fmt.Errorf("federation: gossip record edge %d: %w", i, err)
		}
		// A self-loop and a repeated key are both malformed rather than merely
		// odd: they inflate an edge count that later weighs a branch, and no
		// honest builder emits either.
		if key == origin {
			return nil, errors.New("federation: gossip record claims a friendship with itself")
		}
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("federation: gossip record names %s twice", key)
		}
		seen[key] = struct{}{}
		rec.Friends[i].Key = key
		rec.Friends[i].Name = CleanPeerName(rec.Friends[i].Name)
		if rec.Friends[i].Since < 0 {
			rec.Friends[i].Since = 0
		}
	}
	return &rec, nil
}

// ParseMarkRecord validates raw as a signed distrust list, on the same terms as
// [ParseGraphRecord]. Reasons are capped rather than refused — an over-long one
// is still usable evidence.
func ParseMarkRecord(raw []byte) (*MarkRecord, error) {
	var rec MarkRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("federation: not a distrust record: %w", err)
	}
	if rec.Protocol != ProtocolVersion {
		return nil, fmt.Errorf("federation: unsupported distrust record version %d (this node speaks %d)", rec.Protocol, ProtocolVersion)
	}
	if err := verifySignature(raw, rec.Origin, rec.Signature); err != nil {
		return nil, err
	}
	origin, err := NormalizeKey(rec.Origin)
	if err != nil {
		return nil, fmt.Errorf("federation: distrust record origin: %w", err)
	}
	rec.Origin = origin
	if rec.Seq < 0 || rec.IssuedAt < 0 {
		return nil, errors.New("federation: distrust record has a negative sequence or timestamp")
	}
	if len(rec.Marks) > MaxMarksPerRecord {
		return nil, fmt.Errorf("federation: distrust record carries %d marks (limit %d)", len(rec.Marks), MaxMarksPerRecord)
	}
	seen := make(map[string]struct{}, len(rec.Marks))
	for i := range rec.Marks {
		key, err := NormalizeKey(rec.Marks[i].Key)
		if err != nil {
			return nil, fmt.Errorf("federation: distrust record mark %d: %w", i, err)
		}
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("federation: distrust record marks %s twice", key)
		}
		seen[key] = struct{}{}
		rec.Marks[i].Key = key
		rec.Marks[i].Reason = CleanMarkReason(rec.Marks[i].Reason)
		if rec.Marks[i].At < 0 {
			rec.Marks[i].At = 0
		}
	}
	return &rec, nil
}

// GraphDigestEntry is one line of the digest two nodes exchange before moving
// any payload: an origin and the sequence we hold for it. A sync round compares
// digests and fetches only what is missing, which is what keeps an unchanged
// graph costing one small round-trip instead of a transfer.
type GraphDigestEntry struct {
	Origin string `json:"origin"`
	Seq    int64  `json:"seq"`
}

// GraphEdgeClaim is one stored friendship claim as the network map reads it:
// who claims it, whom with, and under what label. A claim, not a fact — only
// the origin's own signature stands behind it.
type GraphEdgeClaim struct {
	Origin string
	Peer   string
	Name   string
	Since  int64
}

// StoredMark is one published block as the map reads it.
type StoredMark struct {
	Origin string
	Target string
	At     int64
	Reason string
}

// CleanMarkReason trims and rune-caps a distrust mark's free text. Same rules as
// [CleanPeerName] at a larger cap: remote input, rendered in a UI, counted in
// runes so a multi-byte character is never cut in half.
func CleanMarkReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if utf8.RuneCountInString(reason) > MaxMarkReasonRunes {
		reason = string([]rune(reason)[:MaxMarkReasonRunes])
	}
	return reason
}
