package cmd

import (
	"context"
	"fmt"
	"log"
	"os/signal"
	"strings"
	"syscall"

	"Resume_Contacts_Scraper/internal/contact"
	"Resume_Contacts_Scraper/internal/output"
	"Resume_Contacts_Scraper/internal/scraper"
)

const outputDir = "contacts"

func startup(f runFlags) {
	cfg := scraper.DefaultConfig
	cfg.Parallelism = f.concurrency
	cfg.ExtraSeeds = mustLoadSeeds(f.seedsFile)
	cfg.Countries = f.countries

	fmt.Println("Starting Resume Contacts Scraper...")
	countriesLabel := "all"
	if len(f.countries) > 0 {
		countriesLabel = strings.Join(f.countries, ",")
	}
	fmt.Printf("Concurrency: %d  |  Extra seeds: %d  |  Countries: %s  |  Output: %s/\n\n",
		f.concurrency, len(cfg.ExtraSeeds), countriesLabel, outputDir)

	writer, err := output.NewVCFWriter(outputDir)
	if err != nil {
		log.Fatalf("failed to open output directory: %v", err)
	}
	defer writer.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	eng := scraper.New(cfg, func(c contact.Contact) {
		ok, err := writer.Write(c)
		if err != nil {
			log.Printf("write error: %v", err)
			return
		}
		if ok {
			label := c.Email
			if c.Org != "" {
				label = fmt.Sprintf("%-40s  [%s]", c.Email, c.Org)
			}
			fmt.Printf("[+] %s\n", label)
		}
	})

	if err := eng.Run(ctx); err != nil {
		log.Fatalf("scraper error: %v", err)
	}

	fmt.Printf("\nStopped. %d new contacts written to %s/\n", writer.Count(), outputDir)
}
