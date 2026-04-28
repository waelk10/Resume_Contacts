package scraper

import (
	"log"
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
	`|jobs\.lever\.co/[^/?#]+/[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}` +
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
		// Extra depth so we can reach: board → company listing → job → apply page.
		colly.MaxDepth(s.cfg.MaxDepth+1),
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

	// Safety net: if a redirect lands us on an application page, emit it.
	c.OnResponse(func(r *colly.Response) {
		blocker.recordSuccess(r.Request.URL.Hostname())
		if isAppPageURL(r.Request.URL.String()) {
			s.on(r.Request.URL.String())
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
		// Application pages are leaf nodes — emit and do not follow.
		if isAppPageURL(abs) {
			s.on(abs)
			return
		}
		// Job-board and ATS listing pages are worth crawling deeper.
		if isFollowableJobURL(abs) {
			_ = c.Visit(abs)
		}
	})

	c.OnError(func(r *colly.Response, err error) {
		log.Printf("[app] %s: %v", r.Request.URL, err)
		blocker.recordFailure(r.Request.URL.Hostname())
	})

	for _, seed := range webSeeds {
		u, err := url.Parse(seed)
		if err != nil || blocker.isBlocked(u.Hostname()) {
			continue
		}
		_ = c.Visit(seed)
	}
	c.Wait()
	return nil
}
