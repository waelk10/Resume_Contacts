package cmd

import (
	"fmt"
	"log"
	"os"

	"Resume_Contacts_Scraper/internal/scraper"
	"Resume_Contacts_Scraper/internal/sources"
)

const discoveredSeedsFile = "discovered_seeds.txt"

func discoverSeeds(f runFlags) {
	cfg := sources.DefaultConfig
	cfg.Concurrency = f.concurrency

	fmt.Println("Discovering new seed sources...")
	fmt.Printf("Concurrency: %d  |  Meta-sources: %d\n\n",
		cfg.Concurrency, len(cfg.MetaSources))

	d := sources.New(cfg)
	results, err := d.Run(scraper.BuiltInSeeds())
	if err != nil {
		log.Fatalf("discovery error: %v", err)
	}

	if len(results) == 0 {
		fmt.Println("No new sources discovered.")
		return
	}

	file, err := os.Create(discoveredSeedsFile)
	if err != nil {
		log.Fatalf("failed to create output file: %v", err)
	}
	defer file.Close()

	for _, r := range results {
		fmt.Fprintf(file, "# %s\n%s\n", r.Source, r.URL)
		fmt.Printf("[+] %-60s  (from %s)\n", r.URL, shortSource(r.Source))
	}

	fmt.Printf("\nDone. %d new sources written to %s\n", len(results), discoveredSeedsFile)
	fmt.Printf("Tip: feed them into the scraper with:  -s %s\n", discoveredSeedsFile)
}

// shortSource trims a long URL to just its hostname for display.
func shortSource(raw string) string {
	for _, prefix := range []string{"https://raw.githubusercontent.com/", "https://github.com/"} {
		if trimmed := trimPrefix(raw, prefix); trimmed != raw {
			parts := splitPath(trimmed, 2)
			return parts
		}
	}
	if i := len("https://"); len(raw) > i {
		raw = raw[i:]
	}
	if i := indexOf(raw, '/'); i > 0 {
		return raw[:i]
	}
	return raw
}

func trimPrefix(s, prefix string) string {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}

func splitPath(s string, n int) string {
	count := 0
	for i, c := range s {
		if c == '/' {
			count++
			if count == n {
				return s[:i]
			}
		}
	}
	return s
}

func indexOf(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
