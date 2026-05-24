package main

import (
	"log"
	"net/http"
	"os"
	"sync"

	"daemonlord.ygg/madshare/api"
	"daemonlord.ygg/madshare/api/storage"
	"daemonlord.ygg/madshare/webui"
)

func main() {
	log.Println("Start the program")
	store := storage.NewLocal("./data/files")

	var wg sync.WaitGroup
	log.Println("Starting api...")
	wg.Go(func() {
		log.Fatal(http.ListenAndServe(":3000", api.NewRouter(store, os.TempDir())))
	})
	log.Println("Starting web-ui...")
	wg.Go(func() {
		webui.Route()
	})
	wg.Wait()
	log.Println("End!")
}
