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
	maxConcurrency     = 128
)

// runFlags holds the parsed flags common to the start, pages, and discover commands.
type runFlags struct {
	concurrency int
	seedsFile   string
	countries   []string // ISO 3166-1 alpha-2 codes or region aliases; nil = all seeds
	hops        int      // BFS depth for discover (ignored by start/pages)
	smtpVerify  bool     // probe mail server to confirm address exists (start only)
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
	case "apply":
		applyJobs()
	case "clean":
		cleanContacts()
	case "purge":
		purgeAll()
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
	var countriesRaw string
	var hops int
	fs.IntVar(&c, "concurrency", defaultConcurrency, "concurrent lookups per domain (1–8)")
	fs.IntVar(&c, "c", defaultConcurrency, "concurrent lookups per domain (1–8) (shorthand)")
	fs.StringVar(&s, "seeds", "", "path to a line-separated file of extra seed URLs")
	fs.StringVar(&s, "s", "", "path to a line-separated file of extra seed URLs (shorthand)")
	fs.StringVar(&countriesRaw, "countries", "", "comma-separated country/region codes to restrict seeds")
	fs.IntVar(&hops, "hops", 2, "BFS depth for discover: how many meta-source hops beyond the initial list (discover only)")
	var smtpVerify bool
	fs.BoolVar(&smtpVerify, "smtp-verify", false, "probe the mail server to confirm each address exists before saving (start only; slower)")
	_ = fs.Parse(os.Args[2:])
	if c < 1 {
		c = 1
	}
	if c > maxConcurrency {
		c = maxConcurrency
	}
	if hops < 0 {
		hops = 0
	}
	var countries []string
	for _, tok := range strings.Split(countriesRaw, ",") {
		tok = strings.TrimSpace(strings.ToLower(tok))
		if tok != "" {
			countries = append(countries, tok)
		}
	}
	return runFlags{concurrency: c, seedsFile: s, countries: countries, hops: hops, smtpVerify: smtpVerify}
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
  start      Continuously scrape job boards for contact emails  →  contacts/  (Ctrl+C to stop)
  pages      Scrape job boards and collect application-page URLs  →  application_pages.txt
  discover   Auto-discover new seed sources and write them to discovered_seeds.txt
  apply      Auto-apply to jobs from a URL list using a headless browser
  clean      Clean contacts/*.vcf files in-place (filter by regex, deduplicate)
  purge      Delete all persistent output (contacts, application pages, seeds)
  version    Print the version
  help       Show this help message

Flags (start, pages, discover):
  -c, --concurrency int   concurrent lookups per domain, 1–%d (default %d)
  -s, --seeds FILE        line-separated file of extra seed URLs to add to the built-in list
      --countries CODES   comma-separated country/region codes to restrict seeds
      --hops int          BFS depth for discover: extra meta-source hops beyond the initial
                          list (default 2, 0 = single-pass; ignored by start/pages)
      --smtp-verify       probe each email's mail server to confirm the address exists before
                          saving it (start only; adds latency, use on slow/targeted runs)

Country codes (ISO 3166-1 alpha-2):
  Individual:  de at ch nl be lu fr es pt it gr mt gb ie
               dk se no fi is pl cz hu ro bg hr si sk
  Special:     global   (boards with no geographic focus)
               eu       (pan-European boards)
  Region aliases (expand to their constituent country codes):
               dach      → de, at, ch
               benelux   → nl, be, lu
               nordics   → dk, se, no, fi, is
               cee       → pl, cz, hu, ro, bg, hr, si, sk
               southern  → es, pt, it, gr, mt
               eu        → all EU-adjacent countries + eu tag

  For start / pages: filters which built-in seeds are visited.
  For discover: appends country-specific meta-sources and filters discovered
  URLs by ccTLD (.de → de, .co.uk → gb, etc.).  Non-ccTLD domains (.com/.io)
  are treated as "global" and kept only when "global" is in the filter.
  Extra seeds added via --seeds are always included regardless of the filter.

Examples:
  # Runs indefinitely: web spider re-seeds every 30 min, HN re-checked hourly.
  # Press Ctrl+C to stop cleanly — all discovered contacts are flushed to disk.
  Resume_Contacts_Scraper start
  Resume_Contacts_Scraper start -c 8
  Resume_Contacts_Scraper start --countries de,dach,eu,global
  Resume_Contacts_Scraper start --countries gb,ie,global -s my_seeds.txt
  Resume_Contacts_Scraper start --countries nordics,global
  Resume_Contacts_Scraper pages --concurrency 6 --seeds extra.txt
  Resume_Contacts_Scraper pages --countries dach
  Resume_Contacts_Scraper discover
  Resume_Contacts_Scraper discover --hops 3
  Resume_Contacts_Scraper discover --countries dach,global --hops 3
  Resume_Contacts_Scraper discover --countries gb,ie -c 8
  Resume_Contacts_Scraper start -s discovered_seeds.txt

  # Remove noreply/no-reply addresses missed by the scraper's built-in filter
  Resume_Contacts_Scraper clean --filter-regex '^(noreply|no-reply|donotreply)@'

  # Deduplicate contacts, keeping the most-complete card for each email
  Resume_Contacts_Scraper clean --dedup

  # Both at once, custom directory
  Resume_Contacts_Scraper clean --filter-regex '@example\.com$' --dedup --dir my_contacts

Flags (clean):
  -d, --dir DIR           contacts directory to clean (default: contacts)
  -f, --filter-regex RE   remove contacts whose email matches RE (Go regexp)
      --dedup             deduplicate by email, keeping the card with the most information

  # Wipe all output (contacts, application pages, discovered seeds) — asks for confirmation
  Resume_Contacts_Scraper purge

  # Skip the confirmation prompt (useful in scripts)
  Resume_Contacts_Scraper purge --yes

Flags (purge):
  -y, --yes               skip confirmation prompt and delete immediately

  # Dry-run: fill forms but do not submit (use --headful to watch the browser)
  Resume_Contacts_Scraper apply --urls application_pages.txt --resume cv.pdf \
    --name "Jane Doe" --email jane@example.com --dry-run

  # Live apply with optional details
  Resume_Contacts_Scraper apply --urls application_pages.txt --resume cv.pdf \
    --name "Jane Doe" --email jane@example.com \
    --phone "+1 555 0100" --linkedin "https://linkedin.com/in/janedoe"

Flags (apply):
  -u, --urls FILE         line-separated file of job-application URLs [required]
  -r, --resume FILE       path to your CV/resume PDF [required]
      --name NAME         your full name [required]
      --email EMAIL       your email address [required]
      --phone PHONE       your phone number
      --linkedin URL      your LinkedIn profile URL
  -c, --concurrency int   parallel browser pages, keep ≤ 2 (default 1)
      --dry-run           fill forms but do not click Submit
      --hold              keep each window open until you close it, then move to the next URL (implies --headful)
      --headful           show the browser window (useful for debugging)
      --screenshots       save a PNG screenshot after filling each form
      --tailor            use the claude CLI to tailor the resume to each job before uploading
      --output-dir DIR    directory for tailored resume PDFs (default: tailored_resumes/)
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
