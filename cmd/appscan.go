package cmd

import (
	"fmt"
	"log"
	"strings"

	"Resume_Contacts_Scraper/internal/output"
	"Resume_Contacts_Scraper/internal/scraper"
)

const appOutputFile = "application_pages.txt"

func appscan(f runFlags) {
	cfg := scraper.DefaultConfig
	cfg.Parallelism = f.concurrency
	cfg.ExtraSeeds = mustLoadSeeds(f.seedsFile)
	cfg.Countries = f.countries

	fmt.Println("Starting Application Page Scanner...")
	countriesLabel := "all"
	if len(f.countries) > 0 {
		countriesLabel = strings.Join(f.countries, ",")
	}
	fmt.Printf("Concurrency: %d  |  Extra seeds: %d  |  Countries: %s  |  Output: %s\n\n",
		f.concurrency, len(cfg.ExtraSeeds), countriesLabel, appOutputFile)

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
