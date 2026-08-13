package database

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"testing"

	"daemonlord.ygg/madshare/federation"
)

// signer returns a keypair with the public half in the lowercase-hex form keys
// are stored in.
func signer(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return priv, hex.EncodeToString(pub)
}

// putGraph signs and stores a friend-list record, returning whether the store
// took it.
func putGraph(t *testing.T, db *DB, priv ed25519.PrivateKey, rec federation.GraphRecord, from *int64, expiresAt int64) bool {
	t.Helper()
	raw, err := federation.SignGraphRecord(priv, rec)
	if err != nil {
		t.Fatalf("SignGraphRecord: %v", err)
	}
	parsed, err := federation.ParseGraphRecord(raw)
	if err != nil {
		t.Fatalf("ParseGraphRecord: %v", err)
	}
	stored, err := db.PutGraphRecord(context.Background(), parsed, raw, from, expiresAt, 1000)
	if err != nil {
		t.Fatalf("PutGraphRecord: %v", err)
	}
	return stored
}

func TestPutGraphRecordKeepsHighestSeq(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	priv, origin := signer(t)
	_, friendA := signer(t)
	_, friendB := signer(t)

	if !putGraph(t, db, priv, federation.GraphRecord{
		Origin: origin, Seq: 5, IssuedAt: 100,
		Friends: []federation.GraphEdge{{Key: friendA, Name: "a", Since: 50}},
	}, nil, 9000) {
		t.Fatal("first record was not stored")
	}

	// A record we already hold must be dropped — this is what stops a gossip
	// loop, since the caller relays nothing when the store reports false.
	if putGraph(t, db, priv, federation.GraphRecord{
		Origin: origin, Seq: 5, IssuedAt: 200,
		Friends: []federation.GraphEdge{{Key: friendB}},
	}, nil, 9000) {
		t.Error("an equal sequence was stored (a loop would never terminate)")
	}
	if putGraph(t, db, priv, federation.GraphRecord{
		Origin: origin, Seq: 4, IssuedAt: 300,
		Friends: []federation.GraphEdge{{Key: friendB}},
	}, nil, 9000) {
		t.Error("an older sequence overwrote a newer one")
	}

	edges, err := db.GraphEdges(ctx, 0)
	if err != nil {
		t.Fatalf("GraphEdges: %v", err)
	}
	if len(edges) != 1 || edges[0].Peer != friendA || edges[0].Name != "a" || edges[0].Since != 50 {
		t.Fatalf("edges = %+v, want the seq-5 record's single edge", edges)
	}

	// A newer sequence replaces the record AND its derived edges wholesale — a
	// dropped friendship must not survive as a stale row.
	if !putGraph(t, db, priv, federation.GraphRecord{
		Origin: origin, Seq: 6, IssuedAt: 400,
		Friends: []federation.GraphEdge{{Key: friendB, Name: "b"}},
	}, nil, 9000) {
		t.Fatal("a newer sequence was refused")
	}
	edges, err = db.GraphEdges(ctx, 0)
	if err != nil {
		t.Fatalf("GraphEdges: %v", err)
	}
	if len(edges) != 1 || edges[0].Peer != friendB {
		t.Fatalf("edges = %+v, want only the seq-6 record's edge", edges)
	}
}

// The payload is stored and returned byte-for-byte: a relay that re-encoded it
// would invalidate the author's signature for everyone downstream.
func TestGraphPayloadsAreVerbatim(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	priv, origin := signer(t)
	_, friend := signer(t)

	raw, err := federation.SignGraphRecord(priv, federation.GraphRecord{
		Origin: origin, Seq: 1, IssuedAt: 100,
		Friends: []federation.GraphEdge{{Key: friend, Name: "n"}},
	})
	if err != nil {
		t.Fatalf("SignGraphRecord: %v", err)
	}
	parsed, err := federation.ParseGraphRecord(raw)
	if err != nil {
		t.Fatalf("ParseGraphRecord: %v", err)
	}
	if _, err := db.PutGraphRecord(ctx, parsed, raw, nil, 9000, 1000); err != nil {
		t.Fatalf("PutGraphRecord: %v", err)
	}

	got, err := db.GraphPayloads(ctx, []string{origin}, 0)
	if err != nil {
		t.Fatalf("GraphPayloads: %v", err)
	}
	if string(got[origin]) != string(raw) {
		t.Fatalf("payload round-trip altered the bytes:\n stored %s\n got    %s", raw, got[origin])
	}
	// And what came back still verifies, which is the property that actually
	// matters to the node downstream.
	if _, err := federation.ParseGraphRecord(got[origin]); err != nil {
		t.Fatalf("stored payload no longer verifies: %v", err)
	}
}

func TestGraphDigestOrdersAndSkipsExpired(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	origins := make([]string, 3)
	for i := range origins {
		priv, origin := signer(t)
		origins[i] = origin
		expires := int64(9000)
		if i == 2 {
			expires = 500 // already stale at the query time below
		}
		putGraph(t, db, priv, federation.GraphRecord{Origin: origin, Seq: int64(i + 1), IssuedAt: 100}, nil, expires)
	}

	records, marks, err := db.GraphDigest(ctx, 1000)
	if err != nil {
		t.Fatalf("GraphDigest: %v", err)
	}
	if len(marks) != 0 {
		t.Errorf("marks = %d, want 0", len(marks))
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2 (the expired one must not be offered)", len(records))
	}
	if records[0].Origin > records[1].Origin {
		t.Errorf("digest not origin-ordered: %+v", records)
	}
}

// A record whose author nobody has heard of is junk a friend invented. The
// admission check is what keeps it out, and it is also what makes blocking a
// friend able to clear the whole branch that arrived through it.
func TestGraphKnowsKey(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	priv, origin := signer(t)
	_, named := signer(t)
	_, stranger := signer(t)

	putGraph(t, db, priv, federation.GraphRecord{
		Origin: origin, Seq: 1, IssuedAt: 100,
		Friends: []federation.GraphEdge{{Key: named}},
	}, nil, 9000)

	for _, tc := range []struct {
		key  string
		want bool
		why  string
	}{
		{named, true, "named by a record we hold"},
		{stranger, false, "named by nobody"},
	} {
		got, err := db.GraphKnowsKey(ctx, tc.key)
		if err != nil {
			t.Fatalf("GraphKnowsKey: %v", err)
		}
		if got != tc.want {
			t.Errorf("GraphKnowsKey(%s) = %v, want %v (%s)", tc.key[:8], got, tc.want, tc.why)
		}
	}

	// A direct friend counts even before any record names them — otherwise a
	// brand-new friendship could not bootstrap its first record.
	peerID, err := db.InsertFederationPeer(ctx, &federation.ExternalNode{
		PublicKey: stranger, Label: "new friend", TrustState: federation.PeerFriend, TrustedAt: 1,
	})
	if err != nil {
		t.Fatalf("InsertFederationPeer: %v", err)
	}
	known, err := db.GraphKnowsKey(ctx, stranger)
	if err != nil {
		t.Fatalf("GraphKnowsKey: %v", err)
	}
	if !known {
		t.Error("a direct friend is not admissible")
	}

	// Branch attribution: records delivered by that peer are counted against
	// its quota.
	priv2, origin2 := signer(t)
	putGraph(t, db, priv2, federation.GraphRecord{Origin: origin2, Seq: 1, IssuedAt: 100}, &peerID, 9000)
	n, err := db.GraphIntroducedCount(ctx, peerID)
	if err != nil {
		t.Fatalf("GraphIntroducedCount: %v", err)
	}
	if n != 1 {
		t.Errorf("introduced count = %d, want 1", n)
	}
}

func TestExpireGraphDropsRecordsAndDerivedRows(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	privA, originA := signer(t)
	privB, originB := signer(t)
	_, friend := signer(t)

	putGraph(t, db, privA, federation.GraphRecord{
		Origin: originA, Seq: 1, IssuedAt: 100,
		Friends: []federation.GraphEdge{{Key: friend}},
	}, nil, 500) // expires before the sweep
	putGraph(t, db, privB, federation.GraphRecord{
		Origin: originB, Seq: 1, IssuedAt: 100,
		Friends: []federation.GraphEdge{{Key: friend}},
	}, nil, 9000)

	// A mark record too, so the sweep is shown to cover both document types.
	privM, originM := signer(t)
	_, target := signer(t)
	rawMark, err := federation.SignMarkRecord(privM, federation.MarkRecord{
		Origin: originM, Seq: 1, IssuedAt: 100,
		Marks: []federation.DistrustMark{{Key: target, At: 90, Reason: "contradicted claim"}},
	})
	if err != nil {
		t.Fatalf("SignMarkRecord: %v", err)
	}
	parsedMark, err := federation.ParseMarkRecord(rawMark)
	if err != nil {
		t.Fatalf("ParseMarkRecord: %v", err)
	}
	if _, err := db.PutMarkRecord(ctx, parsedMark, rawMark, nil, 500, 1000); err != nil {
		t.Fatalf("PutMarkRecord: %v", err)
	}

	marks, err := db.GraphMarks(ctx, 0)
	if err != nil {
		t.Fatalf("GraphMarks: %v", err)
	}
	if len(marks) != 1 || marks[0].Target != target || marks[0].Reason != "contradicted claim" {
		t.Fatalf("marks = %+v, want the one stored mark", marks)
	}

	n, err := db.ExpireGraph(ctx, 1000)
	if err != nil {
		t.Fatalf("ExpireGraph: %v", err)
	}
	if n != 2 {
		t.Errorf("expired %d records, want 2 (one graph, one mark)", n)
	}

	edges, err := db.GraphEdges(ctx, 0)
	if err != nil {
		t.Fatalf("GraphEdges: %v", err)
	}
	if len(edges) != 1 || edges[0].Origin != originB {
		t.Errorf("edges = %+v, want only the unexpired origin's", edges)
	}
	// The derived rows went with their record: an expired record's edges must
	// not linger and keep a dead node on the map.
	marks, err = db.GraphMarks(ctx, 0)
	if err != nil {
		t.Fatalf("GraphMarks: %v", err)
	}
	if len(marks) != 0 {
		t.Errorf("marks = %+v, want none after expiry", marks)
	}
}

// Lifting a block clears the mark network-wide within the window: the author
// republishes a record without it, and the store replaces the whole set.
func TestMarkRecordReplacementDropsLiftedMarks(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	priv, origin := signer(t)
	_, targetA := signer(t)
	_, targetB := signer(t)

	put := func(seq int64, marks []federation.DistrustMark) {
		t.Helper()
		raw, err := federation.SignMarkRecord(priv, federation.MarkRecord{
			Origin: origin, Seq: seq, IssuedAt: 100 * seq, Marks: marks,
		})
		if err != nil {
			t.Fatalf("SignMarkRecord: %v", err)
		}
		parsed, err := federation.ParseMarkRecord(raw)
		if err != nil {
			t.Fatalf("ParseMarkRecord: %v", err)
		}
		if _, err := db.PutMarkRecord(ctx, parsed, raw, nil, 9000, 1000); err != nil {
			t.Fatalf("PutMarkRecord: %v", err)
		}
	}
	put(1, []federation.DistrustMark{{Key: targetA, At: 1}, {Key: targetB, At: 1}})
	put(2, []federation.DistrustMark{{Key: targetB, At: 1}})

	marks, err := db.GraphMarks(ctx, 0)
	if err != nil {
		t.Fatalf("GraphMarks: %v", err)
	}
	if len(marks) != 1 || marks[0].Target != targetB {
		t.Fatalf("marks = %+v, want only the still-published one", marks)
	}
}

// DropUnreachableGraph is the retention half of the reachability walk: whatever
// the caller's walk did not reach goes, with its derived rows, exactly as
// expiry drops what aged out (docs/architecture/federation-trust.md §Forgetting).
func TestDropUnreachableGraphCollectsCutBranches(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	privKeep, originKeep := signer(t)
	privDrop, originDrop := signer(t)
	_, friend := signer(t)

	putGraph(t, db, privKeep, federation.GraphRecord{
		Origin: originKeep, Seq: 1, IssuedAt: 100,
		Friends: []federation.GraphEdge{{Key: friend}},
	}, nil, 9000)
	putGraph(t, db, privDrop, federation.GraphRecord{
		Origin: originDrop, Seq: 1, IssuedAt: 100,
		Friends: []federation.GraphEdge{{Key: friend}},
	}, nil, 9000)

	// A mark from the cut branch, so both document types are shown to go.
	rawMark, err := federation.SignMarkRecord(privDrop, federation.MarkRecord{
		Origin: originDrop, Seq: 1, IssuedAt: 100,
		Marks: []federation.DistrustMark{{Key: friend, At: 90, Reason: "hearsay"}},
	})
	if err != nil {
		t.Fatalf("SignMarkRecord: %v", err)
	}
	parsedMark, err := federation.ParseMarkRecord(rawMark)
	if err != nil {
		t.Fatalf("ParseMarkRecord: %v", err)
	}
	if _, err := db.PutMarkRecord(ctx, parsedMark, rawMark, nil, 9000, 1000); err != nil {
		t.Fatalf("PutMarkRecord: %v", err)
	}

	n, err := db.DropUnreachableGraph(ctx, map[string]struct{}{originKeep: {}})
	if err != nil {
		t.Fatalf("DropUnreachableGraph: %v", err)
	}
	if n != 2 {
		t.Errorf("dropped %d records, want 2 (one graph, one mark)", n)
	}

	digest, marks, err := db.GraphDigest(ctx, 0)
	if err != nil {
		t.Fatalf("GraphDigest: %v", err)
	}
	if len(digest) != 1 || digest[0].Origin != originKeep {
		t.Errorf("digest = %+v, want only the reachable origin", digest)
	}
	if len(marks) != 0 {
		t.Errorf("marks = %+v, want none: the branch that published them is cut", marks)
	}

	// The derived rows go with their records — a dropped branch must also stop
	// satisfying GraphKnowsKey, which is what keeps it from being re-admitted.
	edges, err := db.GraphEdges(ctx, 0)
	if err != nil {
		t.Fatalf("GraphEdges: %v", err)
	}
	for _, e := range edges {
		if e.Origin == originDrop {
			t.Errorf("edge %+v survived its record", e)
		}
	}
	stored, err := db.GraphMarks(ctx, 0)
	if err != nil {
		t.Fatalf("GraphMarks: %v", err)
	}
	if len(stored) != 0 {
		t.Errorf("derived marks = %+v, want none", stored)
	}
}

// An empty keep set means the caller's walk produced nothing, which cannot
// happen (it always contains our own key) and would erase the store if obeyed.
func TestDropUnreachableGraphRefusesToKeepNothing(t *testing.T) {
	db := openMem(t)
	priv, origin := signer(t)
	_, friend := signer(t)
	putGraph(t, db, priv, federation.GraphRecord{
		Origin: origin, Seq: 1, IssuedAt: 100,
		Friends: []federation.GraphEdge{{Key: friend}},
	}, nil, 9000)

	if _, err := db.DropUnreachableGraph(context.Background(), nil); err == nil {
		t.Fatal("an empty keep set should be refused, not obeyed")
	}
	digest, _, err := db.GraphDigest(context.Background(), 0)
	if err != nil {
		t.Fatalf("GraphDigest: %v", err)
	}
	if len(digest) != 1 {
		t.Errorf("digest = %+v, want the store untouched after a refusal", digest)
	}
}
