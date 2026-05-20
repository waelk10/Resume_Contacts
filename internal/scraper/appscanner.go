package scraper

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

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

// pendingItem is a URL that was deferred because its domain is rate-limited.
type pendingItem struct {
	rawURL string
	domain string
}

// domainRateLimit tracks per-domain 429 backoffs.  Each consecutive 429 on the
// same domain doubles the cooldown (starting at 30 s, capped at 10 min).
type domainRateLimit struct {
	mu        sync.Mutex
	releaseAt map[string]time.Time
	backoff   map[string]time.Duration
}

func newDomainRateLimit() *domainRateLimit {
	return &domainRateLimit{
		releaseAt: make(map[string]time.Time),
		backoff:   make(map[string]time.Duration),
	}
}

const (
	rlInitialBackoff = 30 * time.Second
	rlMaxBackoff     = 10 * time.Minute
)

// record marks domain as rate-limited and returns the earliest retry time.
func (r *domainRateLimit) record(domain string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.backoff[domain]
	if b == 0 {
		b = rlInitialBackoff
	} else {
		b *= 2
		if b > rlMaxBackoff {
			b = rlMaxBackoff
		}
	}
	r.backoff[domain] = b
	t := time.Now().Add(b)
	r.releaseAt[domain] = t
	return t
}

// isReady reports whether domain has no active rate-limit cooldown.
func (r *domainRateLimit) isReady(domain string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.releaseAt[domain]
	return !ok || time.Now().After(t)
}

// readyAt returns the time when domain becomes eligible for retry,
// or the zero time if no cooldown is active.
func (r *domainRateLimit) readyAt(domain string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.releaseAt[domain]
}

// AppScanner crawls job boards and ATS platforms to collect application-page URLs.
type AppScanner struct {
	cfg Config
	on  func(string)
}

func NewAppScanner(cfg Config, onURL func(string)) *AppScanner {
	return &AppScanner{cfg: cfg, on: onURL}
}

// Run starts the crawl and blocks until all seeds — including any URLs that
// were deferred due to per-domain rate-limit (429) cooldowns — are exhausted.
func (s *AppScanner) Run() error {
	blocker := newDomainBlocker(3)
	rl := newDomainRateLimit()

	// deferred accumulates URLs whose domains hit a 429 during the crawl.
	// Protected by deferMu; drained in retry rounds after c.Wait().
	var deferMu sync.Mutex
	var deferred []pendingItem

	addDeferred := func(rawURL, domain string) {
		deferMu.Lock()
		deferred = append(deferred, pendingItem{rawURL: rawURL, domain: domain})
		deferMu.Unlock()
	}

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
		if err != nil {
			return
		}
		host := u.Hostname()
		if blocker.isBlocked(host) {
			return
		}

		// Classify the link before touching cooldown state so we only defer
		// URLs we would actually visit.
		isApp := isAppPageURL(abs)
		isFollowable := !isApp && isFollowableJobURL(abs)
		isApplyBtn := !isApp && !isFollowable &&
			applyTextRe.MatchString(strings.TrimSpace(el.Text)) && isATSDomain(host)

		if !isApp && !isFollowable && !isApplyBtn {
			return
		}

		// Domain is currently rate-limited — defer instead of visiting now.
		// The URL will be retried once the cooldown expires.
		if !rl.isReady(host) {
			addDeferred(abs, host)
			return
		}

		_ = c.Visit(abs)
	})

	c.OnError(func(r *colly.Response, err error) {
		host := r.Request.URL.Hostname()
		switch r.StatusCode {
		case http.StatusNotFound:
			// A 404 means the listing was removed — don't penalise the domain.
			log.Printf("[app] %s: 404 not found (skipped)", r.Request.URL)
		case http.StatusTooManyRequests:
			// Record the cooldown and defer the URL for retry; do NOT count
			// this as a blocker failure — the domain is healthy, just busy.
			retryAt := rl.record(host)
			log.Printf("[app] %s: rate-limited (429) — retry after %s",
				host, retryAt.Format("15:04:05"))
			addDeferred(r.Request.URL.String(), host)
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

	// Pull application-page URLs from Reddit hiring posts and Lobste.rs "hiring"
	// threads before starting the main colly crawl so they are included in the
	// same pass.  These calls are synchronous; any URLs they find are already in
	// colly's queue when c.Wait() is called below.
	s.seedFromReddit(c, blocker)
	s.seedFromLobsters(c, blocker)

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

	// Retry loop: drain deferred URLs in rounds.  Each round waits for each
	// domain's cooldown to expire before re-submitting, mirroring the
	// applicator's per-platform cooldown queue.  New 429s during a retry pass
	// re-populate deferred, so we loop until it stabilises at empty.
	for {
		deferMu.Lock()
		batch := deferred
		deferred = nil
		deferMu.Unlock()

		if len(batch) == 0 {
			break
		}

		// Deduplicate: the same URL can be linked from many pages.
		seen := make(map[string]struct{}, len(batch))
		unique := batch[:0]
		for _, item := range batch {
			if _, dup := seen[item.rawURL]; !dup {
				seen[item.rawURL] = struct{}{}
				unique = append(unique, item)
			}
		}
		batch = unique

		log.Printf("[app] retrying %d rate-limited URL(s)", len(batch))

		// Sort by cooldown expiry: shortest-wait domains first.  This lets
		// colly start crawling whichever domain unblocks earliest while we
		// wait for longer cooldowns — maximising throughput.
		sort.Slice(batch, func(i, j int) bool {
			return rl.readyAt(batch[i].domain).Before(rl.readyAt(batch[j].domain))
		})

		for _, item := range batch {
			if blocker.isBlocked(item.domain) {
				continue
			}
			if readyAt := rl.readyAt(item.domain); time.Now().Before(readyAt) {
				wait := time.Until(readyAt)
				log.Printf("[app] [%s] cooldown — waiting %.0fs before retry",
					item.domain, wait.Seconds())
				time.Sleep(wait)
			}
			_ = c.Visit(item.rawURL)
		}
		c.Wait()
	}

	return nil
}

// rawURLRe extracts bare https?:// URLs from plain text, Markdown, and HTML.
// Excludes whitespace and common surrounding punctuation so trailing commas
// or closing parentheses are not swallowed into the URL.
var rawURLRe = regexp.MustCompile(`https?://[^\s"'<>()\[\]{}]+`)

// extractAppURLsFromText returns all application-page URLs found in text.
// Trailing sentence punctuation is trimmed before the isAppPageURL check.
func extractAppURLsFromText(text string) []string {
	var out []string
	for _, u := range rawURLRe.FindAllString(text, -1) {
		u = strings.TrimRight(u, ".,;:!?)/")
		if isAppPageURL(u) {
			out = append(out, u)
		}
	}
	return out
}

// seedFromReddit fetches the newest posts from each subreddit in
// redditSubreddits, extracts ATS application-page URLs from their titles and
// self-text, and queues them for visiting via c.  Called once at the start of
// Run() so the URLs are included in the same colly crawl as the built-in seeds.
func (s *AppScanner) seedFromReddit(c *colly.Collector, blocker *domainBlocker) {
	client := &http.Client{Timeout: s.cfg.RequestTimeout, Transport: newTransport()}
	ctx := context.Background()

	for _, sub := range redditSubreddits {
		listing, err := redditFetchListing(ctx, client, sub)
		if err != nil {
			log.Printf("[app/reddit] r/%s: %v", sub, err)
			continue
		}
		count := 0
		for _, child := range listing.Data.Children {
			p := child.Data
			for _, u := range extractAppURLsFromText(p.Title + "\n" + p.Selftext) {
				parsed, err := url.Parse(u)
				if err != nil || blocker.isBlocked(parsed.Hostname()) {
					continue
				}
				_ = c.Visit(u)
				count++
			}
		}
		if count > 0 {
			log.Printf("[app/reddit] r/%s: queued %d application-page URL(s)", sub, count)
		}
		// Polite inter-subreddit pause.
		time.Sleep(2 * time.Second)
	}
}

// seedFromLobsters fetches stories tagged "hiring" on lobste.rs, extracts ATS
// application-page URLs from story descriptions and all comment bodies, and
// queues them via c.  Mirrors the Lobste.rs integration in the contact scraper.
func (s *AppScanner) seedFromLobsters(c *colly.Collector, blocker *domainBlocker) {
	client := &http.Client{Timeout: s.cfg.RequestTimeout, Transport: newTransport()}
	ctx := context.Background()

	stubs, err := lobstersFetchTag(ctx, client, "hiring")
	if err != nil {
		log.Printf("[app/lobsters] %v", err)
		return
	}

	total := 0
	for _, stub := range stubs {
		story, err := lobstersFetchStory(ctx, client, stub.ShortID)
		if err != nil {
			log.Printf("[app/lobsters] %s: %v", stub.ShortID, err)
			continue
		}
		total += queueAppURLsFromText(c, blocker, story.Description)
		total += queueAppURLsFromComments(c, blocker, story.Comments)
		time.Sleep(s.cfg.Delay)
	}
	if total > 0 {
		log.Printf("[app/lobsters] queued %d application-page URL(s)", total)
	}
}

func queueAppURLsFromText(c *colly.Collector, blocker *domainBlocker, text string) int {
	n := 0
	for _, u := range extractAppURLsFromText(text) {
		parsed, err := url.Parse(u)
		if err != nil || blocker.isBlocked(parsed.Hostname()) {
			continue
		}
		_ = c.Visit(u)
		n++
	}
	return n
}

func queueAppURLsFromComments(c *colly.Collector, blocker *domainBlocker, comments []lobstersComment) int {
	n := 0
	for _, cmt := range comments {
		n += queueAppURLsFromText(c, blocker, cmt.Comment)
		if len(cmt.Comments) > 0 {
			n += queueAppURLsFromComments(c, blocker, cmt.Comments)
		}
	}
	return n
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
