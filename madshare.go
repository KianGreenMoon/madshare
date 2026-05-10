package main

import (
	"log"
	"sync"
)

func main() {
	log.Println("Start the program")
	var wg sync.WaitGroup
	log.Println("Starting api...")
	wg.Add(1)
	go func() {
		defer wg.Done()
		route()
	}()
	log.Println("Started!")
	log.Println("Starting web-ui...")
	wg.Add(1)
	go func() {
		defer wg.Done()
		mainui()
	}()
	log.Println("Started!")
	wg.Wait()
	log.Println("End!")
}
