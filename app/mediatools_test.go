package app_test

import (
	"context"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/app"
	"daemonlord.ygg/madshare/federation"
	"daemonlord.ygg/madshare/media"
)

// The analysis an embedder brings itself (docs/architecture/embedding.md
// §"Analysis an embedder brings itself"). A server runs ffprobe and fpcalc as
// child processes; a phone can run neither, and without a way to say so it
// loses its tech columns, its fingerprints, and with them the mesh.

// ownTools is what such an embedder supplies: analysis compiled into itself.
// Neither method is called here — these tests are about the STARTUP gate, which
// asks only whether fingerprinting can happen at all.
type ownTools struct{ fingerprints bool }

func (o ownTools) Available() (bool, bool) { return true, o.fingerprints }

func (ownTools) ProbeTech(context.Context, string) (*media.TechInfo, error) {
	return &media.TechInfo{Codec: "flac"}, nil
}

func (ownTools) ComputeFingerprint(context.Context, string) (*media.Fingerprint, error) {
	return &media.Fingerprint{Algo: "chromaprint", Raw: []uint32{1, 2, 3}}, nil
}

// The gate is about VERIFICATION, not about a binary: a node that fingerprints
// in its own process satisfies it exactly as a server with fpcalc on PATH does.
// Note what this config does NOT set — allow_missing_fingerprinting. Reaching
// the mesh by declaring the check unnecessary is the thing this must not become.
func TestFingerprintingGateAcceptsAnEmbeddersOwnTools(t *testing.T) {
	if !federation.Available {
		t.Skip("built with -tags nofederation")
	}
	cfg, _ := embeddedConfig(t, t.TempDir())
	cfg.Federation.Enabled = true
	cfg, err := cfg.Prepare()
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	lg, out := testLogger()

	inst, err := app.Start(context.Background(), cfg,
		app.WithLogger(lg), app.WithMediaTools(ownTools{fingerprints: true}))
	if err != nil {
		t.Fatalf("Start refused a node that can fingerprint: %v\nlog:\n%s", err, out)
	}
	defer inst.Stop(context.Background())

	if strings.Contains(out.String(), "fpcalc not found") {
		t.Errorf("warned about fpcalc while fingerprinting in-process\nlog:\n%s", out)
	}
}

// The other direction, which is the part that must not weaken: an embedder that
// cannot fingerprint is refused, with the same message a server gets.
func TestFingerprintingGateRefusesToolsThatCannotFingerprint(t *testing.T) {
	if !federation.Available {
		t.Skip("built with -tags nofederation")
	}
	cfg, _ := embeddedConfig(t, t.TempDir())
	cfg.Federation.Enabled = true
	cfg, err := cfg.Prepare()
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	lg, _ := testLogger()

	inst, err := app.Start(context.Background(), cfg,
		app.WithLogger(lg), app.WithMediaTools(ownTools{fingerprints: false}))
	if err == nil {
		inst.Stop(context.Background())
		t.Fatal("Start accepted a federated node that cannot fingerprint")
	}
	if !strings.Contains(err.Error(), "fpcalc") {
		t.Errorf("refusal does not name the missing capability: %v", err)
	}
}

// A nil Tools is the unbuilt dependency an embedder passes by accident. It must
// fall back to the binaries rather than silently analysing nothing — which,
// with federation on, would also mean refusing to start.
func TestNilMediaToolsKeepsTheDefault(t *testing.T) {
	cfg, _ := embeddedConfig(t, t.TempDir())
	lg, out := testLogger()

	inst, err := app.Start(context.Background(), cfg,
		app.WithLogger(lg), app.WithMediaTools(nil))
	if err != nil {
		t.Fatalf("Start: %v\nlog:\n%s", err, out)
	}
	defer inst.Stop(context.Background())
}
