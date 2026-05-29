package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"daemonlord.ygg/madshare/api"
	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/config"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/webui"
)

func main() {
	configPath := flag.String("config", "madshare.toml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config %s: %v", *configPath, err)
	}

	log.Println("Start the program")

	// Config values are validated by config.Load.

	if err := os.MkdirAll(filepath.Dir(cfg.Database.Path), 0755); err != nil {
		log.Fatalf("mkdir %s: %v", filepath.Dir(cfg.Database.Path), err)
	}

	db, err := database.Open(cfg.Database.Path)
	if err != nil {
		log.Fatalf("open database %s: %v", cfg.Database.Path, err)
	}
	defer db.Close()

	filesDir := cfg.Storage.FilesDir
	if err := database.ReconcileOrphans(context.Background(), db, filesDir); err != nil {
		log.Printf("reconcile orphans: %v", err)
	}

	store := storage.NewLocal(filesDir)
	maxUploadSize := cfg.Storage.MaxUploadBytes()

	var wg sync.WaitGroup
	log.Println("Starting api...")
	wg.Go(func() {
		log.Fatal(http.ListenAndServe(cfg.API.Addr, api.NewRouter(store, db, os.TempDir(), filesDir, maxUploadSize)))
	})
	log.Println("Starting web-ui...")
	wg.Go(func() {
		webui.Route(cfg.WebUI.Addr, cfg.API.PublicURL)
	})
	wg.Wait()
	log.Println("End!")
}
