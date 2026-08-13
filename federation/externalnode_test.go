package federation

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
)

// TestExternalNodeWireIsUnchangedByTheFold pins the admin peer list's JSON.
//
// The struct fold merged three view types into one, and the admin page marshals
// this struct directly — so the one way the refactor could have changed
// behaviour is by widening (or renaming) what /api/admin/federation/peers
// sends. The sync and household groups must stay off the wire: that endpoint is
// the TRUST view of a node, and a reader of it never asked what our pull
// rotation is doing.
func TestExternalNodeWireIsUnchangedByTheFold(t *testing.T) {
	// Every field set, so an accidentally-serialized one shows up as a key.
	b, err := json.Marshal(&ExternalNode{
		ID: 1, PublicKey: "aa", HeardName: "heard", FirstSeen: 2, LastSeen: 3,
		HintedAt: 4, UnreachableAt: 5,
		TrustState: PeerFriend, PrevState: PeerPendingOutgoing, Label: "label",
		GuestOnly: true, TrustedAt: 6, BlockReason: "why", BlockedAt: 7,
		SyncAddedAt: 8, CatalogSerial: "serial", CatalogSyncedAt: 9, AttemptedAt: 10,
		HomeAddedAt: 11, HomeBaseURL: "http://home.example:3000",
		Address: "200::1",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	want := []string{
		"address", "block_reason", "blocked_at", "created_at", "guest_only",
		"heard_name", "id", "last_seen", "name", "public_key", "state",
	}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("peer wire keys =\n  %v\nwant\n  %v\n(the admin page reads these names; "+
			"the fold must not rename or add any)", keys, want)
	}
	// The two names are what the wire calls them, not what the columns do.
	if got["name"] != "label" || got["heard_name"] != "heard" {
		t.Errorf("name/heard_name = %v/%v, want the admin's label and the node's own claim",
			got["name"], got["heard_name"])
	}
}

// TestGroupPredicatesReadTheirOwnColumn: which groups a row carries is asked
// through the predicates and never inferred from another group's data — the
// whole point of the merged table is that a node can be trusted without being
// pulled from, pulled from without being trusted, and a home without either.
func TestGroupPredicatesReadTheirOwnColumn(t *testing.T) {
	var bare ExternalNode
	if bare.IsTrusted() || bare.InRotation() || bare.IsHome() {
		t.Error("a bare row claimed a group; the zero value is a node we have merely heard of")
	}
	trusted := ExternalNode{TrustState: PeerPendingIncoming}
	if !trusted.IsTrusted() {
		t.Error("a pending peer is an admin decision in progress and belongs to the trust group")
	}
	if trusted.InRotation() {
		t.Error("a pending peer joined the pull rotation — that is exactly what sync_added_at " +
			"exists to prevent, since row existence used to imply it")
	}
	if home := (ExternalNode{HomeAddedAt: 1}); !home.IsHome() || home.IsTrusted() {
		t.Error("a home node read as trusted; signing in to a server never friends it")
	}
}

// TestNameResolvesTheTwoOwners: an admin's label always wins over hearsay, and
// Display never yields a blank — the fallback CatalogSource.Display used to
// carry on its own, now that one rule serves both kinds of row.
func TestNameResolvesTheTwoOwners(t *testing.T) {
	key := strings.Repeat("ab", 32)
	labelled := ExternalNode{PublicKey: key, Label: "studio", HeardName: "their claim"}
	if got := labelled.Name(); got != "studio" {
		t.Errorf("Name() = %q, want the admin's label to win", got)
	}
	heardOnly := ExternalNode{PublicKey: key, HeardName: "their claim"}
	if got := heardOnly.Name(); got != "their claim" {
		t.Errorf("Name() = %q, want what the node calls itself", got)
	}
	nameless := ExternalNode{PublicKey: key}
	if got := nameless.Name(); got != "" {
		t.Errorf("Name() = %q, want empty — the key fallback is Display's, not Name's", got)
	}
	if got := nameless.Display(); got != key[:shortKeyRunes] {
		t.Errorf("Display() = %q, want the short key; a node is never named by a blank", got)
	}
}
