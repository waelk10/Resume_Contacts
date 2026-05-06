package cmd

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// purgeTargets lists every persistent output file / directory the tool creates.
var purgeTargets = []struct {
	desc    string
	pattern string // glob pattern relative to CWD; empty means exact path
}{
	{"contacts VCF files", filepath.Join("contacts", "contacts_*.vcf")},
	{"application pages list", appOutputFile},
	{"discovered seeds list", discoveredSeedsFile},
}

func purgeAll() {
	fs := flag.NewFlagSet("purge", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: Resume_Contacts_Scraper purge [flags]\n\n"+
			"Deletes all persistent output produced by previous runs:\n"+
			"  • contacts/contacts_*.vcf   (scraped email contacts)\n"+
			"  • application_pages.txt     (collected job-application URLs)\n"+
			"  • discovered_seeds.txt      (auto-discovered seed sources)\n\n"+
			"Flags:\n")
		fs.PrintDefaults()
	}

	var yes bool
	fs.BoolVar(&yes, "yes", false, "skip the confirmation prompt and purge immediately")
	fs.BoolVar(&yes, "y", false, "skip the confirmation prompt and purge immediately (shorthand)")
	_ = fs.Parse(os.Args[2:])

	// Resolve which files actually exist so we can show the user what will go.
	type victim struct {
		desc string
		path string
	}
	var victims []victim
	for _, t := range purgeTargets {
		if t.pattern == "" {
			continue
		}
		matches, err := filepath.Glob(t.pattern)
		if err != nil || len(matches) == 0 {
			// Also try treating pattern as a literal path (non-glob targets).
			if _, statErr := os.Stat(t.pattern); statErr == nil {
				victims = append(victims, victim{t.desc, t.pattern})
			}
			continue
		}
		for _, m := range matches {
			victims = append(victims, victim{t.desc, m})
		}
	}

	if len(victims) == 0 {
		fmt.Println("purge: nothing to remove — no output files found.")
		return
	}

	fmt.Printf("The following %d file(s) will be permanently deleted:\n", len(victims))
	for _, v := range victims {
		fmt.Printf("  %-30s  %s\n", v.desc, v.path)
	}

	if !yes {
		fmt.Print("\nProceed? [y/N] ")
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
		answer := strings.TrimSpace(strings.ToLower(scanner.Text()))
		if answer != "y" && answer != "yes" {
			fmt.Println("Aborted.")
			return
		}
	}

	removed := 0
	for _, v := range victims {
		if err := os.Remove(v.path); err != nil {
			fmt.Fprintf(os.Stderr, "purge: remove %s: %v\n", v.path, err)
			continue
		}
		removed++
	}
	fmt.Printf("purge: removed %d/%d file(s).\n", removed, len(victims))
}
