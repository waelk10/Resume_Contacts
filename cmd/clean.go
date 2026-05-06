package cmd

import (
	"flag"
	"fmt"
	"log"
	"os"
	"regexp"

	"Resume_Contacts_Scraper/internal/output"
)

func cleanContacts() {
	fs := flag.NewFlagSet("clean", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: Resume_Contacts_Scraper clean [flags]\n\nCleans up contacts/*.vcf files in-place.\n\nFlags:\n")
		fs.PrintDefaults()
	}

	var (
		dir         string
		filterRegex string
		dedup       bool
	)
	fs.StringVar(&dir, "dir", "contacts", "contacts directory to clean")
	fs.StringVar(&dir, "d", "contacts", "contacts directory to clean (shorthand)")
	fs.StringVar(&filterRegex, "filter-regex", "", "remove contacts whose email matches this regex")
	fs.StringVar(&filterRegex, "f", "", "remove contacts whose email matches this regex (shorthand)")
	fs.BoolVar(&dedup, "dedup", false, "deduplicate by email, keeping the card with the most information")

	_ = fs.Parse(os.Args[2:])

	if filterRegex == "" && !dedup {
		fmt.Fprintln(os.Stderr, "clean: nothing to do — specify --filter-regex and/or --dedup")
		fs.Usage()
		os.Exit(1)
	}

	contacts, err := output.ReadAllContacts(dir)
	if err != nil {
		log.Fatalf("clean: read contacts: %v", err)
	}
	before := len(contacts)
	fmt.Printf("loaded:  %d contacts from %s/\n", before, dir)

	if filterRegex != "" {
		pat, compileErr := regexp.Compile(filterRegex)
		if compileErr != nil {
			log.Fatalf("clean: invalid regex %q: %v", filterRegex, compileErr)
		}
		contacts = output.FilterByEmailRegex(contacts, pat)
		fmt.Printf("filter:  removed %d  (%d → %d)\n", before-len(contacts), before, len(contacts))
		before = len(contacts)
	}

	if dedup {
		contacts = output.DeduplicateByEmail(contacts)
		fmt.Printf("dedup:   removed %d duplicates  (%d → %d)\n", before-len(contacts), before, len(contacts))
	}

	if err := output.RewriteContacts(dir, contacts); err != nil {
		log.Fatalf("clean: write contacts: %v", err)
	}
	fmt.Printf("done:    %d contacts written to %s/\n", len(contacts), dir)
}
