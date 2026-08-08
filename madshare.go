// Command madshare runs a federated audio/media sharing server.
//
// This file is deliberately thin. Everything it used to do between reading the
// config and waiting for a signal now lives in daemonlord.ygg/madshare/app, so a
// program embedding madshare runs the same startup this one does instead of a
// second copy of it (docs/architecture/embedding.md). What is left here is what
// belongs to the *program* rather than to the node: flags, the compiled-in
// licence and source archive, the working directory, and the signal handler.
package main

import (
	"context"
	_ "embed"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"daemonlord.ygg/madshare/app"
	"daemonlord.ygg/madshare/config"
)

// licenseText is the AGPL LICENSE, embedded unconditionally so GET /license
// works in every build with no working tree.
//
//go:embed LICENSE.md
var licenseText []byte

// embeddedSourceTGZ is the AGPL source archive served at GET /source. It is nil
// here and overridden only in release builds (-tags embedsource, see source.go),
// where it holds the embedded source.tar.gz; dev builds leave it nil and fall
// back to building the archive from git ls-files in the CWD.
var embeddedSourceTGZ []byte

func main() {
	configPath := flag.String("config", "madshare.toml", "path to config file")
	webuiConfigPath := flag.String("webui-config", "webui.toml", "path to web UI config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config %s: %v", *configPath, err)
	}
	uiCfg, err := config.LoadWebUI(*webuiConfigPath)
	if err != nil {
		log.Fatalf("load webui config %s: %v", *webuiConfigPath, err)
	}
	for _, w := range cfg.Warnings() {
		log.Printf("config warning: %s", w)
	}

	// The working tree GET /source builds its archive from when no pre-built one
	// is compiled in. Non-fatal: without it the endpoint simply 404s.
	sourceRoot, err := os.Getwd()
	if err != nil {
		log.Printf("warning: cannot determine working directory for source archive: %v", err)
	}

	inst, err := app.Start(context.Background(), cfg,
		app.WithUIConfig(uiCfg),
		app.WithLicenseText(licenseText),
		app.WithSourceArchive(embeddedSourceTGZ),
		app.WithSourceRoot(sourceRoot),
	)
	if err != nil {
		log.Fatalf("start madshare: %v", err)
	}
	if err := inst.Serve(); err != nil {
		inst.Stop(context.Background())
		log.Fatalf("start listeners: %v", err)
	}

	// Block until a termination signal or a listener that died under us, then shut
	// every server down gracefully. A dead listener was fatal before the facade
	// existed and stays fatal here — but now the node gets to close down in order
	// on the way out instead of the process vanishing mid-write.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	failed := false
	select {
	case <-stop:
	case err := <-inst.Err():
		log.Printf("listener failed: %v", err)
		failed = true
	}
	log.Println("Shutting down...")
	inst.Stop(context.Background())
	log.Println("End!")
	if failed {
		os.Exit(1)
	}
}
