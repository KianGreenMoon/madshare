package main

import (
	"log"
	"sync"
)

func main() {
	log.Println("Start the program")
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		route()
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		mainui()
	}()
	wg.Wait()
	log.Println("End!")
}
