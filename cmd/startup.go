package cmd

import (
	"fmt"
	"log"

	"Resume_Contacts_Scraper/internal/contact"
	"Resume_Contacts_Scraper/internal/output"
	"Resume_Contacts_Scraper/internal/scraper"
)

const outputDir = "contacts"

func startup(concurrency int) {
	cfg := scraper.DefaultConfig
	cfg.Parallelism = concurrency

	fmt.Println("Starting Resume Contacts Scraper...")
	fmt.Printf("Concurrency: %d  |  Output: %s/\n\n", concurrency, outputDir)

	writer, err := output.NewVCFWriter(outputDir)
	if err != nil {
		log.Fatalf("failed to open output directory: %v", err)
	}
	defer writer.Close()

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

	if err := eng.Run(); err != nil {
		log.Fatalf("scraper error: %v", err)
	}

	fmt.Printf("\nDone. %d new contacts written to %s/\n", writer.Count(), outputDir)
}
