package cmd

import (
	"flag"
	"fmt"
	"os"
)

const version = "0.1.0"

const (
	defaultConcurrency = 4
	maxConcurrency     = 8
)

func Execute() {
	if len(os.Args) < 2 {
		printHelp()
		os.Exit(0)
	}

	switch os.Args[1] {
	case "help", "--help", "-h":
		printHelp()
	case "version", "--version", "-v":
		printVersion()
	case "start":
		startup(parseConcurrency("start"))
	case "pages":
		appscan(parseConcurrency("pages"))
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n\n", os.Args[1])
		printHelp()
		os.Exit(1)
	}
}

// parseConcurrency builds a FlagSet for the named sub-command, parses
// os.Args[2:], and returns a clamped concurrency value.
func parseConcurrency(cmd string) int {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: Resume_Contacts_Scraper %s [flags]\n\nFlags:\n", cmd)
		fs.PrintDefaults()
	}
	var c int
	fs.IntVar(&c, "concurrency", defaultConcurrency, "concurrent lookups per domain (1–8)")
	fs.IntVar(&c, "c", defaultConcurrency, "concurrent lookups per domain (1–8) (shorthand)")
	_ = fs.Parse(os.Args[2:])
	if c < 1 {
		c = 1
	}
	if c > maxConcurrency {
		c = maxConcurrency
	}
	return c
}

func printHelp() {
	fmt.Printf(`Resume Contacts Scraper v%s

Usage:
  Resume_Contacts_Scraper <command> [flags]

Commands:
  start      Scrape job boards and extract contact emails  →  contacts/
  pages      Scrape job boards and collect application-page URLs  →  application_pages.txt
  version    Print the version
  help       Show this help message

Flags (start, pages):
  -c, --concurrency int   concurrent lookups per domain, 1–%d (default %d)

Examples:
  Resume_Contacts_Scraper start
  Resume_Contacts_Scraper start -c 8
  Resume_Contacts_Scraper pages
  Resume_Contacts_Scraper pages --concurrency 6
`, version, maxConcurrency, defaultConcurrency)
}

func printVersion() {
	fmt.Printf("Resume Contacts Scraper v%s\n", version)
}
