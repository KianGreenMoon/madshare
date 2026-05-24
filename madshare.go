package main

import (
	"log"
	"net/http"
	"os"
	"sync"

	"daemonlord.ygg/madshare/api"
	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/database"
	"daemonlord.ygg/madshare/webui"
)

func main() {
	log.Println("Start the program")
	store := storage.NewLocal("./data/files")

	// Task 7 will introduce MADSHARE_DB_PATH + reconciliation.
	if err := os.MkdirAll("./data", 0755); err != nil {
		log.Fatalf("mkdir data: %v", err)
	}
	db, err := database.Open("./data/madshare.db")
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer db.Close()

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
