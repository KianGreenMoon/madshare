package federation

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// newSigner returns an ed25519 keypair with the public half in the lowercase
// hex form the protocol stores keys in.
func newSigner(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return priv, hex.EncodeToString(pub)
}

func TestGraphRecordRoundTrip(t *testing.T) {
	priv, origin := newSigner(t)
	_, friendKey := newSigner(t)

	raw, err := SignGraphRecord(priv, GraphRecord{
		Origin:   origin,
		Seq:      7,
		IssuedAt: 1753400000,
		Friends:  []GraphEdge{{Key: strings.ToUpper(friendKey), Name: "  studio  ", Since: 1750000000}},
	})
	if err != nil {
		t.Fatalf("SignGraphRecord: %v", err)
	}
	rec, err := ParseGraphRecord(raw)
	if err != nil {
		t.Fatalf("ParseGraphRecord: %v", err)
	}
	if rec.Origin != origin || rec.Seq != 7 || rec.IssuedAt != 1753400000 {
		t.Errorf("header round-trip: %+v", rec)
	}
	if len(rec.Friends) != 1 {
		t.Fatalf("friends = %d, want 1", len(rec.Friends))
	}
	if rec.Friends[0].Key != friendKey {
		t.Errorf("edge key = %q, want lowercased %q", rec.Friends[0].Key, friendKey)
	}
	if rec.Friends[0].Name != "studio" {
		t.Errorf("edge name = %q, want trimmed", rec.Friends[0].Name)
	}
	if rec.Friends[0].Since != 1750000000 {
		t.Errorf("edge since = %d", rec.Friends[0].Since)
	}
}

// A relay must not be able to rewrite what it carries. Every mutation below
// leaves the signature covering different bytes than the ones presented.
func TestGraphRecordRejectsTampering(t *testing.T) {
	priv, origin := newSigner(t)
	_, friendA := newSigner(t)
	_, friendB := newSigner(t)

	raw, err := SignGraphRecord(priv, GraphRecord{
		Origin: origin, Seq: 3, IssuedAt: 1753400000,
		Friends: []GraphEdge{{Key: friendA, Name: "a"}},
	})
	if err != nil {
		t.Fatalf("SignGraphRecord: %v", err)
	}

	mutate := func(t *testing.T, f func(map[string]any)) []byte {
		t.Helper()
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		f(doc)
		out, err := json.Marshal(doc)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		return out
	}

	cases := map[string]func(map[string]any){
		"added edge": func(d map[string]any) {
			d["friends"] = append(d["friends"].([]any), map[string]any{"key": friendB, "name": "b"})
		},
		"removed edge":  func(d map[string]any) { d["friends"] = []any{} },
		"renamed edge":  func(d map[string]any) { d["friends"].([]any)[0].(map[string]any)["name"] = "impostor" },
		"bumped seq":    func(d map[string]any) { d["seq"] = 99.0 },
		"moved issued":  func(d map[string]any) { d["issued_at"] = 1753499999.0 },
		"reassigned to": func(d map[string]any) { d["origin"] = friendB },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseGraphRecord(mutate(t, mut)); err == nil {
				t.Fatal("ParseGraphRecord accepted a tampered record")
			}
		})
	}
}

// A record signed by one key but claiming another as its origin is the forgery
// the relay design has to refuse: the carrier is not the author.
func TestGraphRecordRejectsForeignSignature(t *testing.T) {
	priv, _ := newSigner(t)
	_, otherOrigin := newSigner(t)

	raw, err := SignGraphRecord(priv, GraphRecord{Origin: otherOrigin, Seq: 1, IssuedAt: 1})
	if err != nil {
		t.Fatalf("SignGraphRecord: %v", err)
	}
	if _, err := ParseGraphRecord(raw); err != ErrBadSignature {
		t.Fatalf("err = %v, want ErrBadSignature", err)
	}
}

// The signature covers the raw bytes, so a field this build cannot parse still
// contributes to it. Without that, an old relay would break every record a newer
// node publishes the moment the format grows a field.
func TestGraphRecordToleratesUnknownFields(t *testing.T) {
	priv, origin := newSigner(t)
	_, friend := newSigner(t)

	// Sign a document carrying a field no GraphRecord field maps to.
	future := map[string]any{
		"protocol":  ProtocolVersion,
		"origin":    origin,
		"seq":       2,
		"issued_at": 1753400000,
		"friends":   []any{map[string]any{"key": friend, "name": "n"}},
		"vouches":   []any{map[string]any{"key": friend, "weight": 3}},
	}
	raw, err := signDocument(priv, future)
	if err != nil {
		t.Fatalf("signDocument: %v", err)
	}
	rec, err := ParseGraphRecord(raw)
	if err != nil {
		t.Fatalf("ParseGraphRecord: %v", err)
	}
	if rec.Seq != 2 || len(rec.Friends) != 1 {
		t.Errorf("parsed = %+v", rec)
	}

	// And the unknown field survives in the bytes a relay would pass on.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := doc["vouches"]; !ok {
		t.Error("unknown field dropped from the relayed bytes")
	}
}

func TestGraphRecordBounds(t *testing.T) {
	priv, origin := newSigner(t)
	_, friend := newSigner(t)

	t.Run("too many edges refused, not truncated", func(t *testing.T) {
		edges := make([]GraphEdge, MaxGraphEdges+1)
		for i := range edges {
			_, k := newSigner(t)
			edges[i] = GraphEdge{Key: k}
		}
		raw, err := SignGraphRecord(priv, GraphRecord{Origin: origin, Seq: 1, Friends: edges})
		if err != nil {
			t.Fatalf("SignGraphRecord: %v", err)
		}
		if _, err := ParseGraphRecord(raw); err == nil {
			t.Fatal("accepted a record over the edge cap")
		}
	})

	t.Run("exactly the cap is fine", func(t *testing.T) {
		edges := make([]GraphEdge, MaxGraphEdges)
		for i := range edges {
			_, k := newSigner(t)
			edges[i] = GraphEdge{Key: k}
		}
		raw, err := SignGraphRecord(priv, GraphRecord{Origin: origin, Seq: 1, Friends: edges})
		if err != nil {
			t.Fatalf("SignGraphRecord: %v", err)
		}
		if _, err := ParseGraphRecord(raw); err != nil {
			t.Fatalf("rejected a record at the cap: %v", err)
		}
	})

	t.Run("self-loop", func(t *testing.T) {
		raw, err := SignGraphRecord(priv, GraphRecord{Origin: origin, Seq: 1, Friends: []GraphEdge{{Key: origin}}})
		if err != nil {
			t.Fatalf("SignGraphRecord: %v", err)
		}
		if _, err := ParseGraphRecord(raw); err == nil {
			t.Fatal("accepted a record claiming friendship with itself")
		}
	})

	t.Run("duplicate edge", func(t *testing.T) {
		raw, err := SignGraphRecord(priv, GraphRecord{
			Origin: origin, Seq: 1,
			Friends: []GraphEdge{{Key: friend}, {Key: strings.ToUpper(friend)}},
		})
		if err != nil {
			t.Fatalf("SignGraphRecord: %v", err)
		}
		if _, err := ParseGraphRecord(raw); err == nil {
			t.Fatal("accepted a record naming one key twice")
		}
	})

	t.Run("malformed edge key", func(t *testing.T) {
		raw, err := SignGraphRecord(priv, GraphRecord{Origin: origin, Seq: 1, Friends: []GraphEdge{{Key: "nope"}}})
		if err != nil {
			t.Fatalf("SignGraphRecord: %v", err)
		}
		if _, err := ParseGraphRecord(raw); err == nil {
			t.Fatal("accepted a record with a non-key edge")
		}
	})
}

func TestMarkRecordRoundTrip(t *testing.T) {
	priv, origin := newSigner(t)
	_, target := newSigner(t)

	raw, err := SignMarkRecord(priv, MarkRecord{
		Origin: origin, Seq: 4, IssuedAt: 1753400000,
		Marks: []DistrustMark{{
			Key:    strings.ToUpper(target),
			At:     1753300000,
			Reason: "  advertised hash 3a9f… with a contradicting fingerprint  ",
		}},
	})
	if err != nil {
		t.Fatalf("SignMarkRecord: %v", err)
	}
	rec, err := ParseMarkRecord(raw)
	if err != nil {
		t.Fatalf("ParseMarkRecord: %v", err)
	}
	if len(rec.Marks) != 1 {
		t.Fatalf("marks = %d, want 1", len(rec.Marks))
	}
	if rec.Marks[0].Key != target {
		t.Errorf("mark key = %q, want lowercased", rec.Marks[0].Key)
	}
	if !strings.HasPrefix(rec.Marks[0].Reason, "advertised") || strings.HasSuffix(rec.Marks[0].Reason, " ") {
		t.Errorf("reason not trimmed: %q", rec.Marks[0].Reason)
	}
}

func TestMarkRecordRejectsTampering(t *testing.T) {
	priv, origin := newSigner(t)
	_, target := newSigner(t)

	raw, err := SignMarkRecord(priv, MarkRecord{
		Origin: origin, Seq: 1, IssuedAt: 1,
		Marks: []DistrustMark{{Key: target, At: 1, Reason: "contradicted claim"}},
	})
	if err != nil {
		t.Fatalf("SignMarkRecord: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Rewriting someone else's accusation is the attack that matters here: a
	// relay could otherwise put words in an honest node's mouth.
	doc["marks"].([]any)[0].(map[string]any)["reason"] = "ships malware"
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := ParseMarkRecord(out); err == nil {
		t.Fatal("ParseMarkRecord accepted a rewritten reason")
	}
}

func TestCleanMarkReason(t *testing.T) {
	if got := CleanMarkReason("  why  "); got != "why" {
		t.Errorf("trim: %q", got)
	}
	// Runes, not bytes — the same rule as peer names, so a multi-byte reason is
	// never cut mid-character.
	for _, s := range []string{
		strings.Repeat("a", MaxMarkReasonRunes+50),
		strings.Repeat("я", MaxMarkReasonRunes+50),
		strings.Repeat("🎵", MaxMarkReasonRunes+50),
	} {
		got := CleanMarkReason(s)
		if n := utf8.RuneCountInString(got); n != MaxMarkReasonRunes {
			t.Errorf("%.1s cap: %d runes, want %d", s, n, MaxMarkReasonRunes)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%.1s cap produced invalid UTF-8", s)
		}
	}
}

// Signing must not depend on how the caller happened to order or space the
// JSON: signer and verifier both canonicalize, so a record re-encoded with
// different key order still verifies.
func TestSigningInputIsCanonical(t *testing.T) {
	priv, origin := newSigner(t)
	raw, err := SignGraphRecord(priv, GraphRecord{Origin: origin, Seq: 1, IssuedAt: 5})
	if err != nil {
		t.Fatalf("SignGraphRecord: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	reordered, err := json.Marshal(doc) // Go sorts map keys; whitespace differs from the original
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := ParseGraphRecord(reordered); err != nil {
		t.Fatalf("re-encoded record failed to verify: %v", err)
	}
}
