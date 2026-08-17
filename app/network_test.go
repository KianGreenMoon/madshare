package app_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/app"
	"daemonlord.ygg/madshare/config"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/federation"
)

// The mesh half of the embedder surface (docs/architecture/embedding.md
// §"Network", docs/architecture/federation-access.md §"The household"). It exists for
// the listener node, so these tests are the listener node's own path: no
// listeners, federation on, and every substitute for the things a device cannot
// earn by itself.

// meshConfig is embeddedConfig with the mesh switched on. fpcalc is not required
// of a test the way it is of a deployment — the gate is about verifying content
// this node redistributes, and nothing here fetches any.
func meshConfig(t *testing.T, dataDir string) config.Config {
	t.Helper()
	cfg, _ := embeddedConfig(t, dataDir)
	cfg.Federation.Enabled = true
	cfg.Federation.AllowMissingFingerprinting = true
	cfg, err := cfg.Prepare()
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return cfg
}

// TestNetworkIsAbsentWithoutAMesh: an embedder should learn once, at startup,
// that this configuration has no mesh — rather than from a call that fails.
func TestNetworkIsAbsentWithoutAMesh(t *testing.T) {
	cfg, _ := embeddedConfig(t, t.TempDir())
	lg, out := testLogger()
	inst, err := app.Start(context.Background(), cfg, app.WithLogger(lg))
	if err != nil {
		t.Fatalf("Start: %v\nlog:\n%s", err, out)
	}
	defer inst.Stop(context.Background())

	if _, ok := inst.Network(); ok {
		t.Error("Network() is available with federation disabled; want not ok")
	}
}

// TestNetworkIdentityAndHomes covers what a device does the moment it signs in:
// it names itself, records the server it signed in to, and installs the vouch
// that server handed back. All three are the substitutes for things an ordinary
// node gets from a graph it is not in.
func TestNetworkIdentityAndHomes(t *testing.T) {
	if !federation.Available {
		t.Skip("built with -tags nofederation")
	}
	lg, out := testLogger()
	inst, err := app.Start(context.Background(), meshConfig(t, t.TempDir()), app.WithLogger(lg))
	if err != nil {
		t.Fatalf("Start: %v\nlog:\n%s", err, out)
	}
	defer inst.Stop(context.Background())

	net, ok := inst.Network()
	if !ok {
		t.Fatal("Network() unavailable with federation enabled")
	}
	if len(net.Key()) != 64 {
		t.Errorf("Key() = %q, want a 64-character hex node key", net.Key())
	}
	if !strings.HasPrefix(net.Address(), "2") {
		t.Errorf("Address() = %q, want a yggdrasil 200::/7 address", net.Address())
	}

	ctx := context.Background()
	home := strings.Repeat("ab", 32)
	if err := net.AddHome(ctx, home, "http://home.example:3000", "home"); err != nil {
		t.Fatalf("AddHome: %v", err)
	}
	homes, err := net.Homes(ctx)
	if err != nil {
		t.Fatalf("Homes: %v", err)
	}
	if len(homes) != 1 || homes[0].PublicKey != home || homes[0].HomeBaseURL != "http://home.example:3000" {
		t.Fatalf("Homes() = %+v, want the one just recorded", homes)
	}

	// Signing in again is the ordinary case, not a duplicate: a client that
	// re-derives its home list on every launch must not accumulate rows.
	if err := net.AddHome(ctx, home, "http://home.example:3000", "home renamed"); err != nil {
		t.Fatalf("AddHome again: %v", err)
	}
	if homes, _ := net.Homes(ctx); len(homes) != 1 || homes[0].HeardName != "home renamed" {
		t.Errorf("Homes() after a second sign-in = %+v, want one row, renamed", homes)
	}

	if err := net.RemoveHome(ctx, home); err != nil {
		t.Fatalf("RemoveHome: %v", err)
	}
	if homes, _ := net.Homes(ctx); len(homes) != 0 {
		t.Errorf("Homes() after signing out = %+v, want none", homes)
	}
}

// TestNetworkSetTokenIsSafeWhileRunning: a token is renewed at its half-life,
// which is to say while transfers are in flight, so installing one must not
// require stopping anything. The value itself is federation's to verify — what
// is under test here is that the facade will take it at any time.
func TestNetworkSetTokenIsSafeWhileRunning(t *testing.T) {
	if !federation.Available {
		t.Skip("built with -tags nofederation")
	}
	lg, out := testLogger()
	inst, err := app.Start(context.Background(), meshConfig(t, t.TempDir()), app.WithLogger(lg))
	if err != nil {
		t.Fatalf("Start: %v\nlog:\n%s", err, out)
	}
	defer inst.Stop(context.Background())

	net, _ := inst.Network()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			net.SetToken("token-" + strings.Repeat("x", i%8))
		}
	}()
	for i := 0; i < 100; i++ {
		_ = net.Holdings() // reads the cache directory while the token churns
	}
	<-done
	net.SetToken("") // and clearing it is how a device signs out
}

// TestNetworkFetchNeedsSomebodyToAsk: a listener node's own tables are empty
// forever, so a fetch with no holders is not "we asked and nobody had it" — it
// is a caller that named nobody. Both come back as ErrNoHolder because both mean
// the same thing to the caller: there is nowhere to get this.
func TestNetworkFetchNeedsSomebodyToAsk(t *testing.T) {
	if !federation.Available {
		t.Skip("built with -tags nofederation")
	}
	lg, out := testLogger()
	inst, err := app.Start(context.Background(), meshConfig(t, t.TempDir()), app.WithLogger(lg))
	if err != nil {
		t.Fatalf("Start: %v\nlog:\n%s", err, out)
	}
	defer inst.Stop(context.Background())

	net, _ := inst.Network()
	hash := strings.Repeat("11", 32)
	for _, tc := range []struct {
		name    string
		holders []string
	}{
		{"none named", nil},
		// A holder list arrives from another machine, so one unusable entry must
		// cost that holder and not the download — with nothing left, the answer
		// is the same as naming nobody.
		{"all unusable", []string{"nonsense", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := net.Fetch(context.Background(), hash, 0, tc.holders); !errors.Is(err, federation.ErrNoHolder) {
				t.Errorf("Fetch = %v, want ErrNoHolder", err)
			}
		})
	}
}

// TestNetworkHoldingsReadTheCacheDirectory: the list a device pushes to its home
// server comes from the disk, not from an index. A device advertising from an
// index could advertise a blob it has already swept, and the swarm reads a
// holder that refuses as a holder that is broken.
func TestNetworkHoldingsReadTheCacheDirectory(t *testing.T) {
	if !federation.Available {
		t.Skip("built with -tags nofederation")
	}
	dir := t.TempDir()
	cfg := meshConfig(t, dir)
	lg, out := testLogger()
	inst, err := app.Start(context.Background(), cfg, app.WithLogger(lg))
	if err != nil {
		t.Fatalf("Start: %v\nlog:\n%s", err, out)
	}
	defer inst.Stop(context.Background())

	net, _ := inst.Network()
	if got := net.Holdings(); len(got) != 0 {
		t.Fatalf("Holdings() on a fresh node = %v, want none", got)
	}

	hash := strings.Repeat("cd", 32)
	writeCacheBlob(t, cfg.MadnetworkCacheDir(), hash)
	got := net.Holdings()
	if len(got) != 1 || got[0] != hash {
		t.Errorf("Holdings() = %v, want the blob just cached", got)
	}
}

// writeCacheBlob puts a file where a completed fetch would have left one.
func writeCacheBlob(t *testing.T, cacheDir, hash string) {
	t.Helper()
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, hash), []byte("fetched"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestNetworkPublishNothing is the rule the household made load-bearing. A home
// server is a member from the device's side, so the shipped default of
// "Madnetwork" would let it pull the device's own catalog and its blobs —
// exactly the one-way publication rule, broken by the mechanism built to let the
// device seed. Seeding what you fetched and publishing what you own are
// different claims, and a person's phone should never make the second.
func TestNetworkPublishNothing(t *testing.T) {
	if !federation.Available {
		t.Skip("built with -tags nofederation")
	}
	dir := t.TempDir()
	cfg := meshConfig(t, dir)
	lg, out := testLogger()
	inst, err := app.Start(context.Background(), cfg, app.WithLogger(lg))
	if err != nil {
		t.Fatalf("Start: %v\nlog:\n%s", err, out)
	}
	defer inst.Stop(context.Background())

	// A second handle on the same file, so the assertion is about what the mesh
	// would actually answer rather than about the setting that decides it.
	db, err := database.Open(cfg.Database.Path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	before, err := db.GetMadnetworkPolicy(context.Background())
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if before.DefaultShareDepth == federation.DepthPrivate {
		t.Fatal("a fresh node already publishes nothing; this test would prove nothing")
	}

	net, _ := inst.Network()
	ctx := context.Background()
	if err := net.PublishNothing(ctx); err != nil {
		t.Fatalf("PublishNothing: %v", err)
	}
	after, err := db.GetMadnetworkPolicy(ctx)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}
	if after.DefaultShareDepth != federation.DepthPrivate {
		t.Errorf("default share depth = %d, want %d (Local)", after.DefaultShareDepth, federation.DepthPrivate)
	}
	// The seeding switches are untouched: the cache is served by a different arm
	// of seedableBlob, and turning publication off must not turn seeding off.
	if after.SeedEnabled != before.SeedEnabled || after.SeedCache != before.SeedCache {
		t.Errorf("seeding changed from %v/%v to %v/%v — publishing and seeding are different claims",
			before.SeedEnabled, before.SeedCache, after.SeedEnabled, after.SeedCache)
	}

	// And nothing is offered to the widest audience anything here ever gets.
	entries, err := db.PublishedCatalog(ctx, federation.MemberAudience)
	if err != nil {
		t.Fatalf("PublishedCatalog: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("published %d entries to a member; want none", len(entries))
	}

	// Idempotent: a client calls this on every launch.
	if err := net.PublishNothing(ctx); err != nil {
		t.Errorf("PublishNothing again: %v", err)
	}
}

// TestNetworkAddPeer: a device learns where the mesh is by signing in, which
// happens long after startup — so a peer added now has to be dialled now, not at
// the next launch. Re-adding is not a second link, which is what lets a periodic
// refresh re-add everything it knows.
func TestNetworkAddPeer(t *testing.T) {
	if !federation.Available {
		t.Skip("built with -tags nofederation")
	}
	lg, out := testLogger()
	inst, err := app.Start(context.Background(), meshConfig(t, t.TempDir()), app.WithLogger(lg))
	if err != nil {
		t.Fatalf("Start: %v\nlog:\n%s", err, out)
	}
	defer inst.Stop(context.Background())

	net, _ := inst.Network()
	// A peer that will never answer: the claim under test is that the URI is
	// accepted and dialling starts, not that anything is on the other end.
	const uri = "tcp://127.0.0.1:1"
	if err := net.AddPeer(uri); err != nil {
		t.Fatalf("AddPeer: %v", err)
	}
	if err := net.AddPeer(uri); err != nil {
		t.Errorf("AddPeer again: %v — re-adding a known peer must be a no-op, not an error", err)
	}
	if err := net.AddPeer("://nonsense"); err == nil {
		t.Error("AddPeer accepted a malformed URI")
	}
}

// TestNetworkUnderlayPeersSeesWhatAddPeerCannotSay: the two halves of the
// underlay surface, and the gap between them is the reason the second one
// exists. AddPeer returns nil as soon as the link is CONFIGURED — the dial runs
// on the core's own goroutine with backoff — so its nil says nothing whatever
// about whether anything is on the other end. An embedder offering somebody a
// box to type a peer into has to be able to answer that afterwards.
//
// The peer here is one that cannot connect (127.0.0.1:1), which is the case
// that matters: it must be VISIBLE and it must be visible as down, rather than
// absent or indistinguishable from a working one.
func TestNetworkUnderlayPeersSeesWhatAddPeerCannotSay(t *testing.T) {
	if !federation.Available {
		t.Skip("built with -tags nofederation")
	}
	lg, out := testLogger()
	inst, err := app.Start(context.Background(), meshConfig(t, t.TempDir()), app.WithLogger(lg))
	if err != nil {
		t.Fatalf("Start: %v\nlog:\n%s", err, out)
	}
	defer inst.Stop(context.Background())

	net, _ := inst.Network()
	const uri = "tcp://127.0.0.1:1"
	if err := net.AddPeer(uri); err != nil {
		t.Fatalf("AddPeer: %v", err)
	}

	var found *federation.UnderlayPeer
	for _, p := range net.UnderlayPeers() {
		if strings.Contains(p.URI, "127.0.0.1:1") {
			found = &p
			break
		}
	}
	if found == nil {
		t.Fatalf("a peer that AddPeer accepted is in no peering state at all; got %+v", net.UnderlayPeers())
	}
	if found.Up {
		t.Errorf("%s reports as up — nothing is listening on port 1", found.URI)
	}
}
