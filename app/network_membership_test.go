package app

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/config"
	"daemonlord.ygg/madshare/federation"
)

// In-package on purpose: the assertion is about the RUNNING NODE's next
// verification, and the node is the instance's own — the facade deliberately
// does not export it. federation.Node.Vouches is the standing check token
// verification makes per request, so it is the exact question a home server's
// token would be answered with.

// membershipMeshConfig is the app_test helpers' meshConfig, rebuilt here
// because an in-package test cannot reach the external test package.
func membershipMeshConfig(t *testing.T) config.Config {
	t.Helper()
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatalf("GenerateSecret: %v", err)
	}
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Listen = nil
	cfg.Sources.AllowAny = true
	cfg.Auth.InitialAdminUser = "owner"
	cfg.Auth.InitialAdminPassword = secret
	cfg.Federation.Enabled = true
	cfg.Federation.AllowMissingFingerprinting = true
	cfg, err = cfg.Prepare()
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	return cfg
}

// TestFacadeAddHomeIsHonouredByTheRunningNode is the 2026-08-23 defect at the
// layer it was wired: Network.AddHome writes the household straight to the DB,
// and the node's membership memo — an input of which the household is — was
// never told, so the node refused the home server's perfectly valid capability
// tokens for up to a MembershipTTL (a minute of skipped tracks right after
// signing in, from the listener's side). The memo here is primed milliseconds
// before AddHome, so without the invalidation the next verification still
// answers from it.
func TestFacadeAddHomeIsHonouredByTheRunningNode(t *testing.T) {
	if !federation.Available {
		t.Skip("built with -tags nofederation")
	}
	ctx := context.Background()
	inst, err := Start(ctx, membershipMeshConfig(t), WithLogger(log.New(io.Discard, "", 0)))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer inst.Stop(context.Background())

	net, ok := inst.Network()
	if !ok {
		t.Fatal("Network() unavailable with federation enabled")
	}
	home := strings.Repeat("ab", 32)

	// Prime the memo the way any mesh request would; before the sign-in the
	// answer must be no, and the memo it computed is now fresh for a full TTL.
	if vouches, err := inst.node.Vouches(ctx, home); err != nil || vouches {
		t.Fatalf("Vouches before sign-in = %v, %v; want false, nil", vouches, err)
	}

	if err := net.AddHome(ctx, home, "http://home.example:3000", "home"); err != nil {
		t.Fatalf("AddHome: %v", err)
	}
	if vouches, err := inst.node.Vouches(ctx, home); err != nil {
		t.Fatalf("Vouches after sign-in: %v", err)
	} else if !vouches {
		t.Error("the node does not honour the home server on the verification right " +
			"after AddHome — the facade wrote the store and left the membership memo standing")
	}

	// Signing out is the same promise the other way: served devices stop on the
	// next request, not on a timer (Network.RemoveHome's contract).
	if err := net.RemoveHome(ctx, home); err != nil {
		t.Fatalf("RemoveHome: %v", err)
	}
	if vouches, err := inst.node.Vouches(ctx, home); err != nil {
		t.Fatalf("Vouches after sign-out: %v", err)
	} else if vouches {
		t.Error("the node still honours a home server the facade just removed")
	}
}
