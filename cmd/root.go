package cmd

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
)

const version = "0.1.0"

const (
	defaultConcurrency = 4
	maxConcurrency     = 8
)

// runFlags holds the parsed flags common to both the start and pages commands.
type runFlags struct {
	concurrency int
	seedsFile   string
}

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
		startup(parseFlags("start"))
	case "pages":
		appscan(parseFlags("pages"))
	case "discover":
		discoverSeeds(parseFlags("discover"))
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %q\n\n", os.Args[1])
		printHelp()
		os.Exit(1)
	}
}

// parseFlags builds a FlagSet for the named sub-command, parses os.Args[2:],
// and returns a runFlags with clamped concurrency.
func parseFlags(cmd string) runFlags {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: Resume_Contacts_Scraper %s [flags]\n\nFlags:\n", cmd)
		fs.PrintDefaults()
	}
	var c int
	var s string
	fs.IntVar(&c, "concurrency", defaultConcurrency, "concurrent lookups per domain (1–8)")
	fs.IntVar(&c, "c", defaultConcurrency, "concurrent lookups per domain (1–8) (shorthand)")
	fs.StringVar(&s, "seeds", "", "path to a line-separated file of extra seed URLs")
	fs.StringVar(&s, "s", "", "path to a line-separated file of extra seed URLs (shorthand)")
	_ = fs.Parse(os.Args[2:])
	if c < 1 {
		c = 1
	}
	if c > maxConcurrency {
		c = maxConcurrency
	}
	return runFlags{concurrency: c, seedsFile: s}
}

// loadSeedsFile reads a line-separated file of seed URLs.
// Blank lines and lines beginning with '#' are skipped.
// Returns nil, nil when path is empty.
func loadSeedsFile(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("seeds file: %w", err)
	}
	defer f.Close()
	var seeds []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		seeds = append(seeds, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("seeds file: %w", err)
	}
	return seeds, nil
}

func printHelp() {
	fmt.Printf(`Resume Contacts Scraper v%s

Usage:
  Resume_Contacts_Scraper <command> [flags]

Commands:
  start      Scrape job boards and extract contact emails  →  contacts/
  pages      Scrape job boards and collect application-page URLs  →  application_pages.txt
  discover   Auto-discover new seed sources and write them to discovered_seeds.txt
  version    Print the version
  help       Show this help message

Flags (start, pages, discover):
  -c, --concurrency int   concurrent lookups per domain, 1–%d (default %d)
  -s, --seeds FILE        line-separated file of extra seed URLs to add to the built-in list

Examples:
  Resume_Contacts_Scraper start
  Resume_Contacts_Scraper start -c 8
  Resume_Contacts_Scraper start -s my_seeds.txt
  Resume_Contacts_Scraper pages --concurrency 6 --seeds extra.txt
  Resume_Contacts_Scraper discover
  Resume_Contacts_Scraper discover -c 8
  Resume_Contacts_Scraper start -s discovered_seeds.txt
`, version, maxConcurrency, defaultConcurrency)
}

func printVersion() {
	fmt.Printf("Resume Contacts Scraper v%s\n", version)
}

// mustLoadSeeds loads seeds from path and fatals on error.
func mustLoadSeeds(path string) []string {
	seeds, err := loadSeedsFile(path)
	if err != nil {
		log.Fatalf("%v", err)
	}
	return seeds
}
