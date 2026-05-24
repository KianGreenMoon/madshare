package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"daemonlord.ygg/madshare/api"
	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/webui"
)

const (
	defaultDBPath    = "./data/madshare.db"
	filesDir         = "./data/files"
	dbPathEnvVar     = "MADSHARE_DB_PATH"
)

func main() {
	log.Println("Start the program")

	dbPath := os.Getenv(dbPathEnvVar)
	if dbPath == "" {
		dbPath = defaultDBPath
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		log.Fatalf("mkdir %s: %v", filepath.Dir(dbPath), err)
	}

	db, err := database.Open(dbPath)
	if err != nil {
		log.Fatalf("open database %s: %v", dbPath, err)
	}
	defer db.Close()

	if err := database.ReconcileOrphans(context.Background(), db, filesDir); err != nil {
		log.Printf("reconcile orphans: %v", err)
	}

	store := storage.NewLocal(filesDir)

	var wg sync.WaitGroup
	log.Println("Starting api...")
	wg.Go(func() {
		log.Fatal(http.ListenAndServe(":3000", api.NewRouter(store, db, os.TempDir())))
	})
	log.Println("Starting web-ui...")
	wg.Go(func() {
		webui.Route()
	})
	wg.Wait()
	log.Println("End!")
}
