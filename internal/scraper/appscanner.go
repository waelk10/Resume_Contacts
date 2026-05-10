package scraper

import (
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/gocolly/colly/v2"
	"github.com/gocolly/colly/v2/extensions"
)

// appPageRe matches the host+path of confirmed single-job application pages on
// the most widely used ATS platforms.
var appPageRe = regexp.MustCompile(`(?i)` +
	`boards\.greenhouse\.io/[^/?#]+/jobs/\d+` +
	// Lever: job detail page is jobs.lever.co/co/uuid; the apply form is at .../uuid/apply.
	`|jobs\.lever\.co/[^/?#]+/[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}/apply(?:[/?#]|$)` +
	`|[^./\s]+\.myworkdayjobs\.com/.+/job/.+` +
	`|[^./\s]+\.icims\.com/jobs/\d+/[^/?#]+/job\b` +
	`|[^./\s]+\.bamboohr\.com/careers/\d+` +
	`|[^./\s]+\.taleo\.net/careersection/.+/jobdetail` +
	`|jobs\.ashbyhq\.com/[^/?#]+/[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}` +
	`|apply\.workable\.com/[^/?#]+/j/[A-Z0-9]+` +
	`|[^./\s]+\.workable\.com/j/[A-Z0-9]+` +
	`|careers\.smartrecruiters\.com/[^/?#]+/[^/?#]+/\d+` +
	`|[^./\s]+\.breezy\.hr/p/[0-9a-f-]{30,}` +
	`|[^./\s]+\.jobs\.personio\.(?:de|com)/job/\d+` +
	`|[^./\s]+\.recruitee\.com/o/[^/?#]+` +
	`|[^./\s]+\.jazz\.co/apply/[^/?#]+/[^/?#]+` +
	`|[^./\s]+\.jobvite\.com/[^/?#]+/job/[^/?#]+` +
	`|[^./\s]+\.pinpointhq\.com/jobs/[^/?#]+` +
	`|app\.dover\.com/apply/[^/?#]+/[^/?#]+`,
)

// atsListingRe matches ATS company-level job-list pages that are worth following
// to discover individual application-page links underneath them.
var atsListingRe = regexp.MustCompile(`(?i)` +
	`boards\.greenhouse\.io/[^/?#]+(?:[/?#]|$)` +
	`|jobs\.lever\.co/[^/?#]+(?:[/?#]|$)` +
	`|jobs\.ashbyhq\.com/[^/?#]+(?:[/?#]|$)` +
	`|[^./\s]+\.myworkdayjobs\.com/[^/?#]+(?:[/?#]|$)` +
	`|[^./\s]+\.recruitee\.com/?(?:[/?#]|$)` +
	`|careers\.smartrecruiters\.com/[^/?#]+(?:[/?#]|$)` +
	`|apply\.workable\.com/[^/?#]+(?:[/?#]|$)` +
	`|[^./\s]+\.bamboohr\.com/careers/?(?:[/?#]|$)` +
	`|[^./\s]+\.breezy\.hr/?(?:[/?#]|$)` +
	`|[^./\s]+\.pinpointhq\.com/jobs/?(?:[/?#]|$)`,
)

// applyTextRe matches the visible text of typical "Apply" call-to-action buttons
// in English and German.
var applyTextRe = regexp.MustCompile(`(?i)^\s*(?:` +
	`apply(?:\s+(?:now|here|for\s+this\s+(?:job|role|position)))?` +
	`|jetzt\s+bewerben` +
	`|bewerben(?:\s+(?:sie\s+sich|jetzt))?` +
	`|zur\s+bewerbung` +
	`|bewerbung\s+starten` +
	`)\s*$`)

// atsDomainRe matches hostnames of the ATS platforms we track.
var atsDomainRe = regexp.MustCompile(`(?i)greenhouse\.io|lever\.co|myworkdayjobs\.com|icims\.com|bamboohr\.com|taleo\.net|ashbyhq\.com|workable\.com|smartrecruiters\.com|breezy\.hr|personio\.|recruitee\.com|jazz\.co|jobvite\.com|pinpointhq\.com|dover\.com`)

func isATSDomain(host string) bool {
	return atsDomainRe.MatchString(strings.ToLower(host))
}

// isAppPageURL reports whether raw points to a single-job application page.
func isAppPageURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	hostPath := strings.ToLower(u.Host + u.Path)
	if appPageRe.MatchString(hostPath) {
		return true
	}
	// Generic fallback: path ends at a clear apply action.
	p := strings.ToLower(u.Path)
	return strings.HasSuffix(p, "/apply") ||
		strings.HasSuffix(p, "/apply-now") ||
		strings.HasSuffix(p, "/application") ||
		strings.Contains(p, "/apply/")
}

// isFollowableJobURL returns true for pages likely to contain links to
// application pages (job boards, ATS company listings, careers sections).
func isFollowableJobURL(raw string) bool {
	if isRelevantURL(raw) {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return atsListingRe.MatchString(strings.ToLower(u.Host + u.Path))
}

// AppScanner crawls job boards and ATS platforms to collect application-page URLs.
type AppScanner struct {
	cfg Config
	on  func(string)
}

func NewAppScanner(cfg Config, onURL func(string)) *AppScanner {
	return &AppScanner{cfg: cfg, on: onURL}
}

// Run starts the crawl and blocks until all seeds are exhausted.
func (s *AppScanner) Run() error {
	blocker := newDomainBlocker(3)

	c := colly.NewCollector(
		// +2 so the path board(0)→listing(1)→job-detail(2)→apply-form(3) fits within the limit,
		// with one extra hop for boards that interpose an intermediate redirect page.
		colly.MaxDepth(s.cfg.MaxDepth+2),
		colly.Async(true),
		colly.MaxBodySize(s.cfg.MaxBodyBytes),
	)
	c.WithTransport(newTransport())
	c.SetRequestTimeout(s.cfg.RequestTimeout)
	extensions.RandomUserAgent(c)
	if err := c.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Parallelism: s.cfg.parallelism(),
		RandomDelay: s.cfg.Delay,
	}); err != nil {
		log.Printf("[app] rate limit setup: %v", err)
	}

	// Emit application-page URLs only on a clean 2xx response.
	// URLs that return 404 or any other error are silently discarded.
	c.OnResponse(func(r *colly.Response) {
		if r.StatusCode >= 200 && r.StatusCode < 300 {
			blocker.recordSuccess(r.Request.URL.Hostname())
			if isAppPageURL(r.Request.URL.String()) {
				s.on(r.Request.URL.String())
			}
		}
	})

	c.OnHTML("a[href]", func(el *colly.HTMLElement) {
		abs := el.Request.AbsoluteURL(el.Attr("href"))
		if abs == "" {
			return
		}
		u, err := url.Parse(abs)
		if err != nil || blocker.isBlocked(u.Hostname()) {
			return
		}
		// Application pages: visit first so that 404s are never passed to the
		// output. OnResponse above handles the actual emit on a 2xx response.
		if isAppPageURL(abs) {
			_ = c.Visit(abs)
			return
		}
		// Job-board and ATS listing pages are worth crawling deeper.
		if isFollowableJobURL(abs) {
			_ = c.Visit(abs)
			return
		}
		// Fallback: follow explicit "Apply" / "Apply Now" button links on known ATS
		// domains even when the href doesn't yet match a known apply-form pattern.
		// This catches platforms where the apply URL differs from the job-detail URL.
		if applyTextRe.MatchString(strings.TrimSpace(el.Text)) && isATSDomain(u.Hostname()) {
			_ = c.Visit(abs)
		}
	})

	c.OnError(func(r *colly.Response, err error) {
		host := r.Request.URL.Hostname()
		switch r.StatusCode {
		case http.StatusNotFound:
			// A 404 means the listing was removed — don't penalise the domain.
			log.Printf("[app] %s: 404 not found (skipped)", r.Request.URL)
		case http.StatusTooManyRequests:
			log.Printf("[app] %s: rate-limited (429)", host)
			blocker.recordFailure(host)
		default:
			if err != nil && strings.Contains(err.Error(), "tls:") {
				log.Printf("[app] %s: TLS error — skipping domain: %v", host, err)
				blocker.blockNow(host)
			} else {
				log.Printf("[app] %s: %v", r.Request.URL, err)
				blocker.recordFailure(host)
			}
		}
	})

	allSeeds := s.cfg.buildSeeds()
	// Append app-scanner-specific seeds (not used by the contact scraper).
	appFilter := expandCountries(s.cfg.Countries)
	for _, as := range appScanSeeds {
		if appFilter == nil || seedMatchesFilter(as.Countries, appFilter) {
			allSeeds = append(allSeeds, as.URL)
		}
	}
	for _, seed := range allSeeds {
		u, err := url.Parse(seed)
		if err != nil || blocker.isBlocked(u.Hostname()) {
			continue
		}
		_ = c.Visit(seed)
	}
	c.Wait()
	return nil
}

// appScanSeeds are built-in seeds specific to the application-page scanner.
// They are not included in the general contact scraper's seed list.
var appScanSeeds = []taggedSeed{
	// German Federal Employment Agency — Nuremberg/Mittelfranken, 25 km radius,
	// IT and industrial sectors (branche=3;11).
	{
		URL:       "https://www.arbeitsagentur.de/jobsuche/suche?angebotsart=1&wo=N%C3%BCrnberg,%20Mittelfranken&umkreis=25&branche=3;11",
		Countries: []string{"de"},
	},
}
