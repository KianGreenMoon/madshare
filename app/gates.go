package app

import (
	"fmt"
	"log"
	"runtime"

	"daemonlord.ygg/madshare/config"
	"daemonlord.ygg/madshare/federation"
	"daemonlord.ygg/madshare/media"
	"daemonlord.ygg/madshare/webui"
)

// checkGates refuses a config this binary or this host cannot honour. It runs
// before anything is opened or created, so a rejected start has changed nothing.
//
// Two kinds of refusal live here, and the distinction is worth keeping: a FEATURE
// gate says the binary was built without the thing the config asks for, and an
// ENVIRONMENT gate says the host is missing a tool the config's promises depend
// on. Both are fatal to startup; neither is a config-file syntax error, which is
// why config.Load cannot make these checks itself.
func checkGates(cfg config.Config, lg *log.Logger, tools media.Tools) error {
	// A listener may only serve the web UI if it is compiled in.
	for idx, l := range cfg.Listen {
		if l.Serves(config.GroupWebUI) && !webui.Available {
			return fmt.Errorf("listen[%d] serves %q but this binary was built with -tags nowebui; "+
				"rebuild without that tag or drop %q", idx, config.GroupWebUI, config.GroupWebUI)
		}
	}
	for _, m := range cfg.MeshListeners() {
		if m.Serves(config.GroupWebUI) && !webui.Available {
			return fmt.Errorf("listen_mesh[%d] serves %q but this binary was built with -tags nowebui; "+
				"rebuild without that tag or drop %q", m.Index, config.GroupWebUI, config.GroupWebUI)
		}
	}
	// The mesh may only be enabled if it is compiled in. Separating the transport
	// from madnetwork in the config does not separate them in the binary —
	// -tags nofederation strips the yggdrasil and gVisor dependencies outright, so
	// a mesh listener has nothing to bind either.
	if cfg.MeshEnabled() && !federation.Available {
		// Name whichever key actually asked for it, so the fix is the line the
		// operator wrote rather than the one they inherited.
		key := "yggdrasil.enabled"
		if cfg.Federation.Enabled {
			key = "federation.enabled"
		}
		return fmt.Errorf("%s is set but this binary was built with -tags nofederation; "+
			"rebuild without that tag, or remove [yggdrasil], [[listen_mesh]] and [federation]", key)
	}
	// Environment gate, same class as the one above: federation is enabled but
	// this host cannot do it correctly.
	if cfg.Federation.Enabled {
		if err := requireFingerprinting(cfg, lg, tools); err != nil {
			return err
		}
	}
	return nil
}

// requireFingerprinting refuses to bring up a federated node that cannot
// fingerprint, unless [federation].allow_missing_fingerprinting says otherwise.
// It is the one analysis pass federation cannot do without — the reasoning,
// including why ffprobe is deliberately not gated the same way, is on
// config.FederationConfig.AllowMissingFingerprinting.
//
// Asking the tools is the whole check, on purpose: an implementation that is
// present but broken shows up per analysis job, where the failure is
// recoverable and visible, and probing it here would only add a startup
// dependency on running somebody else's code.
//
// An embedder that fingerprints in-process (app.WithMediaTools) satisfies this
// gate the same way a server with fpcalc on PATH does. The requirement is that
// downloaded audio gets re-fingerprinted locally, not that a particular binary
// exists — which is why the refusal below names the binary only in its install
// hint, the case where PATH is in fact the answer.
func requireFingerprinting(cfg config.Config, lg *log.Logger, tools media.Tools) error {
	if _, haveFpcalc := tools.Available(); haveFpcalc {
		return nil
	}
	if cfg.Federation.AllowMissingFingerprinting {
		lg.Printf("warning: federation is enabled without fpcalc (allow_missing_fingerprinting is set) — " +
			"downloaded audio cannot be checked against what it claims to be, and never groups with " +
			"recordings this node already holds")
		return nil
	}
	return fmt.Errorf("federation.enabled is set but fpcalc (Chromaprint) is not on PATH.\n"+
		"  A federated node re-fingerprints downloaded audio locally before it joins a recording,\n"+
		"  because a peer's claims about it are hints and never facts. Without fpcalc that check\n"+
		"  cannot happen, so this node would import and re-publish content it is unable to verify.\n"+
		"  Install it:\n%s\n"+
		"  and restart — the startup backfill re-analyses every file that still lacks a\n"+
		"  fingerprint, so an existing library repairs itself.\n"+
		"  To federate without it anyway, set [federation] allow_missing_fingerprinting = true.",
		fpcalcInstallHint())
}

// fpcalcInstallHint names the package carrying fpcalc on this platform, so the
// refusal above ends in a command rather than in a search. Indented to sit
// inside that message.
func fpcalcInstallHint() string {
	switch runtime.GOOS {
	case "linux":
		return "    Debian/Ubuntu: apt install libchromaprint-tools\n" +
			"    Fedora/RHEL:   dnf install chromaprint-tools\n" +
			"    Alpine:        apk add chromaprint"
	case "freebsd":
		return "    pkg install chromaprint"
	case "darwin":
		return "    brew install chromaprint"
	default:
		return "    it ships with Chromaprint, https://acoustid.org/chromaprint"
	}
}
