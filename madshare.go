package main

import (
	"log"
	"sync"

	"daemonlord.ygg/madshare/api"
	"daemonlord.ygg/madshare/webui"
)

func main() {
	log.Println("Start the program")
	var wg sync.WaitGroup
	log.Println("Starting api...")
	wg.Go(func() {
		api.Route()
	})
	log.Println("Starting web-ui...")
	wg.Go(func() {
		webui.Route()
	})
	wg.Wait()
	log.Println("End!")
}
