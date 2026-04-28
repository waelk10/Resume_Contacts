package cmd

import (
	"fmt"
	"log"

	"Resume_Contacts_Scraper/internal/output"
	"Resume_Contacts_Scraper/internal/scraper"
)

const appOutputFile = "application_pages.txt"

func appscan(concurrency int) {
	cfg := scraper.DefaultConfig
	cfg.Parallelism = concurrency

	fmt.Println("Starting Application Page Scanner...")
	fmt.Printf("Concurrency: %d  |  Output: %s\n\n", concurrency, appOutputFile)

	writer, err := output.NewURLWriter(appOutputFile)
	if err != nil {
		log.Fatalf("failed to open output file: %v", err)
	}
	defer writer.Close()

	scanner := scraper.NewAppScanner(cfg, func(u string) {
		ok, err := writer.Write(u)
		if err != nil {
			log.Printf("write error: %v", err)
			return
		}
		if ok {
			fmt.Printf("[+] %s\n", u)
		}
	})

	if err := scanner.Run(); err != nil {
		log.Fatalf("scanner error: %v", err)
	}

	fmt.Printf("\nDone. %d application pages written to %s\n", writer.Count(), appOutputFile)
}
