package scraper

import (
	"context"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gocolly/colly/v2"
	"github.com/gocolly/colly/v2/extensions"

	"Resume_Contacts_Scraper/internal/memguard"
)

// appPageRe matches the host+path of confirmed single-job application pages on
// the most widely used ATS platforms.
var appPageRe = regexp.MustCompile(`(?i)` +
	`boards\.greenhouse\.io/[^/?#]+/jobs/\d+` +
	// New Greenhouse format (job-boards.greenhouse.io) and EU instance.
	`|job-boards\.greenhouse\.io/[^/?#]+/jobs/\d+` +
	`|boards\.eu\.greenhouse\.io/[^/?#]+/jobs/\d+` +
	// Lever: job detail page is jobs.lever.co/co/uuid; the apply form is at .../uuid/apply.
	// Also match apply.lever.co (alternative apply domain used by some companies).
	`|jobs\.lever\.co/[^/?#]+/[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}/apply(?:[/?#]|$)` +
	`|apply\.lever\.co/[^/?#]+/[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}(?:[/?#]|$)` +
	`|[^./\s]+\.myworkdayjobs\.com/.+/job/.+` +
	`|[^./\s]+\.icims\.com/jobs/\d+/[^/?#]+/job\b` +
	`|[^./\s]+\.bamboohr\.com/careers/\d+` +
	`|[^./\s]+\.taleo\.net/careersection/.+/jobdetail` +
	`|jobs\.ashbyhq\.com/[^/?#]+/[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}` +
	// Ashby direct application form URL (app.ashbyhq.com/company/posting/uuid).
	`|app\.ashbyhq\.com/[^/?#]+/posting/[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}` +
	`|apply\.workable\.com/[^/?#]+/j/[A-Z0-9]+` +
	`|[^./\s]+\.workable\.com/j/[A-Z0-9]+` +
	// SmartRecruiters: individual job pages have exactly 2 path segments
	// (company/job-slug).  The previous pattern required a numeric third segment
	// which never matches real SmartRecruiters URLs.
	`|careers\.smartrecruiters\.com/[^/?#]+/[^/?#]+` +
	`|jobs\.smartrecruiters\.com/[^/?#]+/[^/?#]+` +
	`|[^./\s]+\.breezy\.hr/p/[0-9a-f-]{30,}` +
	`|[^./\s]+\.jobs\.personio\.(?:de|com)/job/\d+` +
	`|[^./\s]+\.recruitee\.com/o/[^/?#]+` +
	`|[^./\s]+\.jazz\.co/apply/[^/?#]+/[^/?#]+` +
	`|[^./\s]+\.jobvite\.com/[^/?#]+/job/[^/?#]+` +
	`|[^./\s]+\.pinpointhq\.com/jobs/[^/?#]+` +
	`|app\.dover\.com/apply/[^/?#]+/[^/?#]+` +
	// Teamtailor: job URLs are company.teamtailor.com/jobs/NNN-slug.
	`|[^./\s]+\.teamtailor\.com/jobs/\d+-[^/?#]+` +
	// Comeet: www.comeet.com/jobs/company/hash.
	`|www\.comeet\.com/jobs/[^/?#]+/[A-Za-z0-9.]+` +
	// Freshteam (Zoho): company.freshteam.com/jobs/id/apply.
	`|[^./\s]+\.freshteam\.com/jobs/[^/?#]+/apply` +
	// Rippling embedded job pages.
	`|app\.rippling\.com/job-listings/[^/?#]+` +
	// Softgarden (common in DACH region): company.softgarden.io/job/id or /jobs/id.
	`|[^./\s]+\.softgarden\.io/(?:[a-z]{2}/)?jobs?/\d+` +
	// Zoho Recruit: recruit.zoho.{com,eu}/jobs/locale/id/title.
	`|recruit\.zoho\.(?:com|eu)/jobs/[^/?#]+/\d+` +
	// Nortech (Northern Israel tech hub): individual job listing pages.
	`|nortech-platform\.com/listing/[^/?#]+`,
)

// atsListingRe matches ATS company-level job-list pages that are worth following
// to discover individual application-page links underneath them.
var atsListingRe = regexp.MustCompile(`(?i)` +
	`boards\.greenhouse\.io/[^/?#]+(?:[/?#]|$)` +
	`|job-boards\.greenhouse\.io/[^/?#]+(?:[/?#]|$)` +
	`|boards\.eu\.greenhouse\.io/[^/?#]+(?:[/?#]|$)` +
	`|jobs\.lever\.co/[^/?#]+(?:[/?#]|$)` +
	// Lever job-detail page (not yet /apply) — follow to reach the apply form.
	`|jobs\.lever\.co/[^/?#]+/[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}(?:[/?#]|$)` +
	`|jobs\.ashbyhq\.com/[^/?#]+(?:[/?#]|$)` +
	`|[^./\s]+\.myworkdayjobs\.com/[^/?#]+(?:[/?#]|$)` +
	`|[^./\s]+\.recruitee\.com/?(?:[/?#]|$)` +
	`|careers\.smartrecruiters\.com/[^/?#]+(?:[/?#]|$)` +
	`|jobs\.smartrecruiters\.com/[^/?#]+(?:[/?#]|$)` +
	`|apply\.workable\.com/[^/?#]+(?:[/?#]|$)` +
	`|[^./\s]+\.workable\.com/?(?:[/?#]|$)` +
	`|[^./\s]+\.bamboohr\.com/careers/?(?:[/?#]|$)` +
	`|[^./\s]+\.breezy\.hr/?(?:[/?#]|$)` +
	`|[^./\s]+\.pinpointhq\.com/jobs/?(?:[/?#]|$)` +
	`|[^./\s]+\.teamtailor\.com/jobs/?(?:[/?#]|$)` +
	`|www\.comeet\.com/jobs/[^/?#]+(?:[/?#]|$)` +
	`|[^./\s]+\.freshteam\.com/jobs/?(?:[/?#]|$)` +
	`|app\.rippling\.com/job-listings/?(?:[/?#]|$)` +
	`|app\.dover\.com/jobs/[^/?#]+(?:[/?#]|$)` +
	`|[^./\s]+\.jazz\.co/[^/?#]+(?:[/?#]|$)` +
	`|[^./\s]+\.jobvite\.com/[^/?#]+/jobs(?:[/?#]|$)` +
	`|[^./\s]+\.jobs\.personio\.(?:de|com)/?(?:[/?#]|$)` +
	`|[^./\s]+\.softgarden\.io/(?:[a-z]{2}/)?(?:vacancies?|jobs?)(?:[/?#]|$)`,
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
var atsDomainRe = regexp.MustCompile(`(?i)greenhouse\.io|lever\.co|myworkdayjobs\.com|icims\.com|bamboohr\.com|taleo\.net|ashbyhq\.com|workable\.com|smartrecruiters\.com|breezy\.hr|personio\.|recruitee\.com|jazz\.co|jobvite\.com|pinpointhq\.com|dover\.com|teamtailor\.com|comeet\.com|freshteam\.com|rippling\.com|softgarden\.io|zoho\.com|zoho\.eu`)

func isATSDomain(host string) bool {
	return atsDomainRe.MatchString(strings.ToLower(host))
}

// isAppPageURL reports whether raw points to a single-job application page.
func isAppPageURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return isAppPageURLParsed(u, strings.ToLower(u.Host+u.Path))
}

// isAppPageURLParsed is the hot path used by OnHTML, which has already parsed
// the URL.  hostPath must be strings.ToLower(u.Host + u.Path).
func isAppPageURLParsed(u *url.URL, hostPath string) bool {
	if appPageRe.MatchString(hostPath) {
		return true
	}
	p := strings.ToLower(u.Path)
	return strings.HasSuffix(p, "/apply") ||
		strings.HasSuffix(p, "/apply-now") ||
		strings.HasSuffix(p, "/application") ||
		strings.Contains(p, "/apply/")
}

// isFollowableJobURL returns true for pages likely to contain links to
// application pages (job boards, ATS company listings, careers sections).
func isFollowableJobURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return isFollowableJobURLParsed(u, strings.ToLower(u.Host+u.Path))
}

// isFollowableJobURLParsed is the hot path used by OnHTML.
func isFollowableJobURLParsed(u *url.URL, hostPath string) bool {
	if isRelevantURLParsed(u) {
		return true
	}
	return atsListingRe.MatchString(hostPath)
}

// pendingItem is a URL that was deferred because its domain is rate-limited.
type pendingItem struct {
	rawURL string
	domain string
}

// visitedSet is a mutex-backed URL set that replaces sync.Map for visited-URL
// deduplication.  sync.Map's internal dirty-map promotions become expensive when
// the set is large and Deletes are interspersed with Stores (our 429 retry path),
// causing periodic GC pauses that manifest as high CPU.  A plain map + Mutex
// avoids that entirely.
type visitedSet struct {
	mu sync.Mutex
	m  map[string]struct{}
}

func newVisitedSet() *visitedSet {
	return &visitedSet{m: make(map[string]struct{})}
}

// loadOrStore returns true if key was already present (abort needed); false if
// it was newly stored (request may proceed).
func (s *visitedSet) loadOrStore(key string) (loaded bool) {
	s.mu.Lock()
	_, loaded = s.m[key]
	if !loaded {
		s.m[key] = struct{}{}
	}
	s.mu.Unlock()
	return
}

func (s *visitedSet) delete(key string) {
	s.mu.Lock()
	delete(s.m, key)
	s.mu.Unlock()
}

// maxDeferredURLs is the maximum number of unique URLs that may accumulate in
// the deferred set at one time.  A large job board with deep pagination can
// generate tens of thousands of rate-limited URLs; without a cap the retry
// rounds become very slow and memory usage balloons.
const maxDeferredURLs = 50_000

// retryMaxRounds caps how many retry rounds the scanner will attempt before
// giving up on remaining rate-limited URLs.  Each round can wait up to
// rlMaxBackoff (2 h), so 12 rounds gives the exponential backoff enough room
// to climb from the 30 s initial value all the way to the 2 h ceiling.
const retryMaxRounds = 12

// app404Threshold is the number of app-page 404 responses from a single domain
// before the scanner stops queuing further app-page URLs from that domain for
// the rest of the run.  Followable listing pages are unaffected — only
// isAppPageURL links are suppressed once the threshold is crossed.
const app404Threshold = 10

// domain404Counter tracks per-domain 404 hit counts for application-page URLs.
type domain404Counter struct {
	mu     sync.Mutex
	counts map[string]int
}

func newDomain404Counter() *domain404Counter {
	return &domain404Counter{counts: make(map[string]int)}
}

func (c *domain404Counter) record(host string) int {
	c.mu.Lock()
	c.counts[host]++
	n := c.counts[host]
	c.mu.Unlock()
	return n
}

func (c *domain404Counter) get(host string) int {
	c.mu.Lock()
	n := c.counts[host]
	c.mu.Unlock()
	return n
}

// deferredSet accumulates pending URLs with deduplication on insert.
// It replaces the previous []pendingItem + sync.Mutex design which required an
// O(n) dedup pass at the start of every retry round when the slice grew large.
type deferredSet struct {
	mu   sync.Mutex
	seen map[string]struct{}
	list []pendingItem
}

func newDeferredSet() *deferredSet {
	return &deferredSet{seen: make(map[string]struct{})}
}

// add inserts rawURL/domain if not already present and the cap has not been hit.
func (d *deferredSet) add(rawURL, domain string) {
	d.mu.Lock()
	if len(d.seen) < maxDeferredURLs {
		if _, ok := d.seen[rawURL]; !ok {
			d.seen[rawURL] = struct{}{}
			d.list = append(d.list, pendingItem{rawURL: rawURL, domain: domain})
		}
	}
	d.mu.Unlock()
}

// drain atomically takes all pending items and resets the set, ready for the
// next retry round.
func (d *deferredSet) drain() []pendingItem {
	d.mu.Lock()
	out := d.list
	d.list = nil
	d.seen = make(map[string]struct{})
	d.mu.Unlock()
	return out
}

func (d *deferredSet) size() int {
	d.mu.Lock()
	n := len(d.list)
	d.mu.Unlock()
	return n
}

// domainRateLimit tracks per-domain 429 backoffs.  Each consecutive 429 on the
// same domain doubles the cooldown (starting at 30 s, capped at 10 min).
// After rlMaxStrikes rounds at max backoff the domain is considered exhausted
// and should be permanently blocked by the caller.
type domainRateLimit struct {
	mu        sync.Mutex
	releaseAt map[string]time.Time
	backoff   map[string]time.Duration
	strikes   map[string]int // consecutive rounds that hit rlMaxBackoff
}

func newDomainRateLimit() *domainRateLimit {
	return &domainRateLimit{
		releaseAt: make(map[string]time.Time),
		backoff:   make(map[string]time.Duration),
		strikes:   make(map[string]int),
	}
}

const (
	rlInitialBackoff = 30 * time.Second
	rlMaxBackoff     = 2 * time.Hour
	rlMaxStrikes     = 3 // block permanently after this many max-backoff rounds
)

// tryRecord marks domain as rate-limited and returns (retryAt, true) if the
// domain was not already in a cooldown window.  When concurrent in-flight
// requests all return a 429 at the same time, only the first call records and
// doubles the backoff; subsequent calls return (zero, false) so callers skip
// the log line and leave the already-set cooldown intact.  This prevents the
// backoff from compounding (30s→60s→120s→…) just because colly had several
// parallel requests in-flight when the block was detected.
// Backoff doubles on each hit: 30s→1m→2m→4m→8m→16m→32m→64m→2h (capped).
// When the doubled backoff would exceed rlMaxBackoff the strike counter is
// incremented; callers should call isExhausted() and permanently block the
// domain once the counter reaches rlMaxStrikes.
func (r *domainRateLimit) tryRecord(domain string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if t, ok := r.releaseAt[domain]; ok && time.Now().Before(t) {
		return time.Time{}, false
	}
	b := r.backoff[domain]
	if b == 0 {
		b = rlInitialBackoff
	} else {
		b *= 2
		if b > rlMaxBackoff {
			b = rlMaxBackoff
			r.strikes[domain]++
		}
	}
	r.backoff[domain] = b
	t := time.Now().Add(b)
	r.releaseAt[domain] = t
	return t, true
}

// isExhausted reports whether domain has hit the max backoff too many times
// in a row and should be permanently blocked rather than retried.
func (r *domainRateLimit) isExhausted(domain string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.strikes[domain] >= rlMaxStrikes
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
	cfg   Config
	on    func(string)
	guard *memguard.Guard
}

func NewAppScanner(cfg Config, onURL func(string)) *AppScanner {
	return &AppScanner{cfg: cfg, on: onURL, guard: memguard.New()}
}

// Run starts the crawl and blocks until all seeds — including any URLs that
// were deferred due to per-domain rate-limit (429) cooldowns — are exhausted,
// or until ctx is cancelled (e.g. Ctrl+C).
func (s *AppScanner) Run(ctx context.Context) error {
	blocker := newDomainBlocker(3, "[app]")
	rl := newDomainRateLimit()
	deferred := newDeferredSet()
	visited := newVisitedSet()
	app404s := newDomain404Counter()

	// Shared HTTP client for direct ATS API calls (Greenhouse, Lever, etc.).
	// Uses a modest concurrency transport — the API calls are lightweight JSON
	// fetches, not the high-volume HTML crawl.
	apiClient := &http.Client{Timeout: 15 * time.Second, Transport: newAppScanTransport(8)}

	// Concurrent set tracking which Greenhouse/Lever identifiers have already
	// had an API fetch triggered, so we don't repeat the same company twice.
	seenGHTokens := newVisitedSet()
	seenLeverCos := newVisitedSet()

	// WaitGroup covering all goroutines spawned for direct API fetches so Run()
	// does not return until every in-flight API call has finished.
	var apiWg sync.WaitGroup

	par := s.cfg.appScanParallelism()

	c := colly.NewCollector(
		// +2 so the path board(0)→listing(1)→job-detail(2)→apply-form(3) fits within the limit,
		// with one extra hop for boards that interpose an intermediate redirect page.
		colly.MaxDepth(s.cfg.MaxDepth+2),
		colly.Async(true),
		colly.MaxBodySize(s.cfg.MaxBodyBytes),
		colly.AllowURLRevisit(), // deduplication handled by visitedURLs
	)
	c.WithTransport(&ctxTransport{base: newAppScanTransport(par), ctx: ctx})
	// Shorter timeout: slow ATS hosts shouldn't park a goroutine for 30 s.
	c.SetRequestTimeout(15 * time.Second)
	extensions.RandomUserAgent(c)
	// ATS platforms are large, well-resourced services; limit to 4 concurrent
	// requests per ATS domain to avoid triggering their bot-detection while
	// still being fast overall.
	// RandomDelay is intentionally omitted from both LimitRules: colly's
	// time.Sleep-based delay does not respect context, causing c.Wait() to
	// hang for (queued/parallelism)×delay seconds after Ctrl+C.  A ctx-aware
	// jitter is applied in OnRequest instead.
	if err := c.Limit(&colly.LimitRule{
		DomainRegexp: atsDomainRe.String(),
		Parallelism:  4,
	}); err != nil {
		log.Printf("[app] ATS rate limit setup: %v", err)
	}
	if err := c.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Parallelism: par,
	}); err != nil {
		log.Printf("[app] rate limit setup: %v", err)
	}

	// OnRequest fires immediately before each HTTP dispatch — the last point at
	// which we can cheaply abort a request without paying network RTT.
	c.OnRequest(func(r *colly.Request) {
		// Fast exit on cancel: drains colly's internal queue without blocking.
		if ctx.Err() != nil {
			r.Abort()
			return
		}
		host := r.URL.Hostname()
		rawURL := r.URL.String()

		if blocker.isBlocked(host) || s.isBlockedDomain(host) {
			r.Abort()
			return
		}
		if !rl.isReady(host) {
			visited.delete(rawURL)
			deferred.add(rawURL, host)
			r.Abort()
			return
		}
		if visited.loadOrStore(rawURL) {
			r.Abort()
			return
		}
		// ctx-aware polite jitter: replaces colly's non-ctx-aware RandomDelay so
		// Ctrl+C drains the queue in nanoseconds instead of (queue/par)×600ms.
		if !isATSDomain(host) {
			jitter := time.Duration(rand.Int63n(int64(100 * time.Millisecond)))
			select {
			case <-time.After(jitter):
			case <-ctx.Done():
				r.Abort()
			}
		}
	})

	// Emit application-page URLs only on a clean 2xx response.
	// URLs that return 404 or any other error are silently discarded.
	c.OnResponse(func(r *colly.Response) {
		if r.StatusCode >= 200 && r.StatusCode < 300 {
			blocker.recordSuccess(r.Request.URL.Hostname())
			if isAppPageURL(r.Request.URL.String()) && !s.isBlockedDomain(r.Request.URL.Hostname()) {
				s.on(r.Request.URL.String())
			}
		}
	})

	c.OnHTML("a[href]", func(el *colly.HTMLElement) {
		abs := el.Request.AbsoluteURL(el.Attr("href"))
		if abs == "" {
			return
		}
		// Parse once; pass components to classifiers to avoid redundant parses.
		u, err := url.Parse(abs)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return
		}
		host := u.Hostname()
		if blocker.isBlocked(host) || s.isBlockedDomain(host) {
			return
		}

		hostPath := strings.ToLower(u.Host + u.Path)
		isApp := isAppPageURLParsed(u, hostPath)
		isFollowable := !isApp && isFollowableJobURLParsed(u, hostPath)
		isApplyBtn := !isApp && !isFollowable &&
			applyTextRe.MatchString(strings.TrimSpace(el.Text)) && isATSDomain(host)

		if !isApp && !isFollowable && !isApplyBtn {
			return
		}
		if isApp && !s.passesRoleFilter(strings.TrimSpace(el.Text)) {
			return
		}
		if isApp && app404s.get(host) >= app404Threshold {
			return // domain has too many dead app-page URLs this run — skip
		}
		// Emit app-page URLs immediately on discovery so Ctrl+C never loses them.
		// OnResponse will call s.on again after the fetch, but Write deduplicates.
		if isApp {
			s.on(abs)
		}
		if !rl.isReady(host) {
			deferred.add(abs, host)
			return
		}
		// Use native colly link-following: depth increments from the parent
		// request so the crawl stays pipelined — links are dispatched the
		// moment their parent page lands instead of waiting for an entire BFS
		// level to drain first.
		_ = el.Request.Visit(abs)

		// ── Direct ATS API discovery ─────────────────────────────────────────
		// When a Greenhouse or Lever listing URL is encountered in any page's
		// HTML, immediately query their public JSON APIs to harvest all current
		// job URLs for that company.  This bypasses the HTML-crawl entirely for
		// platforms that use JavaScript-heavy listing pages where colly gets an
		// empty shell, and delivers all job URLs in a single API round-trip
		// instead of one HTTP request per job link.
		//
		// Deduplication via seenGHTokens / seenLeverCos ensures each company's
		// API is queried at most once per Run(), even when the same listing URL
		// appears as a link on many different pages.
		if ghToken := extractGreenhouseToken(u); ghToken != "" && !seenGHTokens.loadOrStore(ghToken) {
			apiWg.Add(1)
			go func(tok string) {
				defer apiWg.Done()
				urls := fetchGreenhouseJobs(ctx, apiClient, tok)
				for _, jobURL := range urls {
					s.on(jobURL)
				}
				if len(urls) > 0 {
					log.Printf("[app/gh-api] %s: emitted %d job(s)", tok, len(urls))
				}
			}(ghToken)
		}
		if leverCo := extractLeverCompany(u); leverCo != "" && !seenLeverCos.loadOrStore(leverCo) {
			apiWg.Add(1)
			go func(co string) {
				defer apiWg.Done()
				urls := fetchLeverJobs(ctx, apiClient, co)
				for _, jobURL := range urls {
					s.on(jobURL)
				}
				if len(urls) > 0 {
					log.Printf("[app/lever-api] %s: emitted %d job(s)", co, len(urls))
				}
			}(leverCo)
		}
	})

	c.OnError(func(r *colly.Response, err error) {
		// Requests cancelled via ctxTransport (Ctrl+C or deadline) are not
		// domain faults — skip all error handling to avoid penalising domains.
		if ctx.Err() != nil {
			return
		}
		host := r.Request.URL.Hostname()
		rawURL := r.Request.URL.String()
		switch r.StatusCode {
		case http.StatusNotFound:
			// Only track 404s on app-page URLs (individual job postings).
			// Non-app-page 404s are expired listing links — silently discard them.
			if isAppPageURL(rawURL) {
				if n := app404s.record(host); n == app404Threshold {
					log.Printf("[app] %s: %d app-page 404s this run — suppressing further app-page URLs from this domain",
						host, app404Threshold)
				}
			}
		case http.StatusTooManyRequests,
			http.StatusForbidden,
			http.StatusUnauthorized,
			http.StatusServiceUnavailable,
			http.StatusBadGateway,
			http.StatusGatewayTimeout:
			if retryAt, fresh := rl.tryRecord(host); fresh {
				log.Printf("[app] %s: blocked (%d) — retry after %s",
					host, r.StatusCode, retryAt.Format("15:04:05"))
				if rl.isExhausted(host) {
					log.Printf("[app] %s: rate-limit exhausted after %d rounds — dropping domain",
						host, rlMaxStrikes)
					blocker.blockNow(host)
					return
				}
			}
			visited.delete(rawURL)
			deferred.add(rawURL, host)
		default:
			switch {
			case err != nil && strings.Contains(err.Error(), "tls:"):
				log.Printf("[app] %s: TLS error — skipping domain: %v", host, err)
				blocker.blockNow(host)
			case isNetworkTimeout(err):
				if retryAt, fresh := rl.tryRecord(host); fresh {
					log.Printf("[app] %s: timeout — retry after %s",
						host, retryAt.Format("15:04:05"))
				}
				visited.delete(rawURL)
				deferred.add(rawURL, host)
			default:
				log.Printf("[app] %s: %v", r.Request.URL, err)
				blocker.recordFailure(host)
			}
		}
	})

	// Kick off public job-feed API seeders concurrently with the BFS crawl.
	// These query JSON endpoints that return ATS apply URLs directly, bypassing
	// the HTML crawl for platforms whose listing pages are JS-rendered SPAs.
	apiWg.Add(2)
	go func() {
		defer apiWg.Done()
		s.seedFromRemotiveAPI(ctx, s.on)
	}()
	go func() {
		defer apiWg.Done()
		s.seedFromRemoteOKAPI(ctx, s.on)
	}()

	allSeeds := s.cfg.buildSeeds()
	// Append app-scanner-specific seeds (not used by the contact scraper).
	appFilter := expandCountries(s.cfg.Countries)
	for _, as := range appScanSeeds {
		if appFilter == nil || seedMatchesFilter(as.Countries, appFilter) {
			allSeeds = append(allSeeds, as.URL)
		}
	}

	// Dispatch all seeds; colly follows discovered links natively via
	// el.Request.Visit() in OnHTML, keeping the crawl fully pipelined.
	// colly.MaxDepth(s.cfg.MaxDepth+2) enforces the depth limit per chain.
	for _, seed := range allSeeds {
		if ctx.Err() != nil {
			break
		}
		if s.guard.Throttle(ctx) != nil {
			break
		}
		u, err := url.Parse(seed)
		if err != nil || blocker.isBlocked(u.Hostname()) {
			continue
		}
		_ = c.Visit(seed)
	}
	c.Wait()
	visited = newVisitedSet() // free visited URL strings accumulated during the initial crawl

	// Retry loop: drain deferred (rate-limited) URLs in rounds.  Deduplication
	// happens on insert (deferredSet), so no O(n) dedup pass is needed here.
	// At most retryMaxRounds are attempted to prevent indefinite running when
	// rate-limited domains keep generating new deferred URLs each round.
	// After each retry c.Wait(), links discovered by OnHTML are followed to
	// one more BFS level so we don't miss app-page links from retried listings.
	for round := 1; ; round++ {
		batch := deferred.drain()
		if len(batch) == 0 || ctx.Err() != nil {
			break
		}
		if round > retryMaxRounds {
			log.Printf("[app] retry limit (%d rounds) reached; dropping %d remaining deferred URL(s)",
				retryMaxRounds, len(batch))
			break
		}

		// Group non-blocked URLs by domain for parallel dispatch.
		domainURLs := make(map[string][]string)
		for _, item := range batch {
			if !blocker.isBlocked(item.domain) {
				domainURLs[item.domain] = append(domainURLs[item.domain], item.rawURL)
			}
		}
		if len(domainURLs) == 0 {
			break // all remaining domains are permanently blocked
		}
		log.Printf("[app] retry round %d/%d: %d URL(s) across %d domain(s)",
			round, retryMaxRounds, len(batch), len(domainURLs))

		var dispatchWg sync.WaitGroup
		for domain, urls := range domainURLs {
			if s.guard.Throttle(ctx) != nil {
				break
			}
			dispatchWg.Add(1)
			go func(domain string, urls []string) {
				defer dispatchWg.Done()
				if readyAt := rl.readyAt(domain); time.Now().Before(readyAt) {
					log.Printf("[app] [%s] cooldown %.0fs — dispatching %d URL(s) when ready",
						domain, time.Until(readyAt).Seconds(), len(urls))
					cooldown := time.NewTimer(time.Until(readyAt))
					select {
					case <-cooldown.C:
					case <-ctx.Done():
						cooldown.Stop()
						return
					}
				}
				if ctx.Err() != nil {
					return
				}
				// A blockNow may have fired while we were sleeping the cooldown.
				if blocker.isBlocked(domain) {
					return
				}
				for _, u := range urls {
					_ = c.Visit(u)
				}
			}(domain, urls)
		}
		dispatchWg.Wait()
		c.Wait()
		visited = newVisitedSet() // free visited URL strings accumulated during this retry round
		// Links discovered from retried pages are followed natively by colly
		// via el.Request.Visit() in OnHTML; no secondary BFS loop needed.
	}

	// Wait for all in-flight API goroutines (Greenhouse/Lever per-company fetches,
	// Remotive, RemoteOK) to finish before returning, so callers see a complete
	// result set.  Each goroutine respects ctx, so this completes promptly on cancel.
	apiWg.Wait()

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
func (s *AppScanner) seedFromReddit(ctx context.Context, c *colly.Collector, blocker *domainBlocker) {
	client := &http.Client{Timeout: s.cfg.RequestTimeout, Transport: newTransport()}

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
func (s *AppScanner) seedFromLobsters(ctx context.Context, c *colly.Collector, blocker *domainBlocker) {
	client := &http.Client{Timeout: s.cfg.RequestTimeout, Transport: newTransport()}

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

// passesRoleFilter returns true when the job link is allowed through the role
// filter configured on the scanner.  It uses anchorText (the visible link label
// on the listing page — typically the job title) as the primary signal.
//
// Filtering is skipped and the link is allowed through when:
//   - no Roles are configured (nil / empty)
//   - the text is blank or matches the generic Apply-CTA pattern
//   - the text is longer than 120 characters (looks like a nav block, not a title)
//
// In those cases we have no reliable role signal, so we pass through rather
// than risk silently dropping legitimate tech listings.
func (s *AppScanner) passesRoleFilter(anchorText string) bool {
	if len(s.cfg.Roles) == 0 {
		return true
	}
	if anchorText == "" || len(anchorText) > 120 || applyTextRe.MatchString(anchorText) {
		return true
	}
	low := strings.ToLower(anchorText)
	for _, role := range s.cfg.Roles {
		if strings.Contains(low, role) {
			return true
		}
	}
	return false
}

// isBlockedDomain reports whether host matches any entry in cfg.BlockedDomains
// exactly or as a subdomain (e.g. "example.com" blocks "jobs.example.com").
func (s *AppScanner) isBlockedDomain(host string) bool {
	host = strings.ToLower(host)
	for _, blocked := range s.cfg.BlockedDomains {
		blocked = strings.ToLower(blocked)
		if host == blocked || strings.HasSuffix(host, "."+blocked) {
			return true
		}
	}
	return false
}

// isNetworkTimeout reports whether err is a transient network timeout with no
// HTTP status code: response-header timeout, dial timeout, or context deadline.
// These share the same recovery path as a 429 — back off and retry.
// TLS errors must be checked before this function is called; a TLS handshake
// failure that also happens to time out will contain "tls:" and be caught first.
func isNetworkTimeout(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "timeout") ||
		strings.Contains(s, "deadline exceeded") ||
		strings.Contains(s, "timed out")
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
	// ── Germany ──────────────────────────────────────────────────────────────
	// German Federal Employment Agency — Nuremberg/Mittelfranken, 25 km radius,
	// IT and industrial sectors (branche=3;11).
	{
		URL:       "https://www.arbeitsagentur.de/jobsuche/suche?angebotsart=1&wo=N%C3%BCrnberg,%20Mittelfranken&umkreis=25&branche=3;11",
		Countries: []string{"de"},
	},

	// ── Global / Remote ───────────────────────────────────────────────────────
	{URL: "https://www.ycombinator.com/jobs", Countries: []string{"global"}},
	{URL: "https://arc.dev/remote-jobs", Countries: []string{"global"}},
	{URL: "https://himalayas.app/jobs", Countries: []string{"global"}},
	{URL: "https://4dayweek.io/jobs", Countries: []string{"global"}},
	{URL: "https://remote.co/remote-jobs/developer/", Countries: []string{"global"}},
	{URL: "https://wfh.io/jobs", Countries: []string{"global"}},
	{URL: "https://justremote.co/remote-developer-jobs", Countries: []string{"global"}},
	{URL: "https://authenticjobs.com/", Countries: []string{"global"}},
	{URL: "https://remoteok.com/remote-dev-jobs", Countries: []string{"global"}},
	{URL: "https://jobspresso.co/remote-work/", Countries: []string{"global"}},
	{URL: "https://nodesk.co/remote-jobs/engineering/", Countries: []string{"global"}},
	{URL: "https://remoteleaf.com/", Countries: []string{"global"}},
	{URL: "https://europeremotely.com/", Countries: []string{"global"}},
	{URL: "https://remotefrontendjobs.com/", Countries: []string{"global"}},
	{URL: "https://whoishiring.io/", Countries: []string{"global"}},
	{URL: "https://www.workatastartup.com/jobs", Countries: []string{"global"}},
	{URL: "https://angel.co/jobs", Countries: []string{"global"}},
	{URL: "https://wellfound.com/jobs", Countries: []string{"global"}},
	{URL: "https://startup.jobs/", Countries: []string{"global"}},
	{URL: "https://otta.com/jobs", Countries: []string{"global"}},

	// ── United States ─────────────────────────────────────────────────────────
	{URL: "https://builtin.com/jobs", Countries: []string{"us"}},
	{URL: "https://builtinnyc.com/jobs", Countries: []string{"us"}},
	{URL: "https://builtinsf.com/jobs", Countries: []string{"us"}},
	{URL: "https://builtinboston.com/jobs", Countries: []string{"us"}},
	{URL: "https://builtinchicago.com/jobs", Countries: []string{"us"}},
	{URL: "https://builtinaustin.com/jobs", Countries: []string{"us"}},
	{URL: "https://builtinseattle.com/jobs", Countries: []string{"us"}},
	{URL: "https://builtinla.com/jobs", Countries: []string{"us"}},
	{URL: "https://www.dice.com/jobs?q=software+engineer", Countries: []string{"us"}},
	{URL: "https://levels.fyi/jobs/", Countries: []string{"us"}},
	{URL: "https://devit.org/jobs", Countries: []string{"us"}},
	{URL: "https://www.idealist.org/en/jobs?q=software", Countries: []string{"us"}},
	{URL: "https://www.simplyhired.com/search?q=software+engineer", Countries: []string{"us"}},
	{URL: "https://www.glassdoor.com/Job/software-engineer-jobs-SRCH_KO0,17.htm", Countries: []string{"us"}},
	{URL: "https://www.indeed.com/jobs?q=software+engineer&sc.keyword=software+engineer", Countries: []string{"us"}},
	{URL: "https://jobs.github.com/positions?description=software+engineer", Countries: []string{"us"}},
	{URL: "https://stackoverflow.com/jobs?q=software+engineer", Countries: []string{"us"}},
	{URL: "https://www.hired.com/jobs/software-engineer", Countries: []string{"us"}},
	{URL: "https://triplebyte.com/jobs", Countries: []string{"us"}},

	// ── United Kingdom ────────────────────────────────────────────────────────
	{URL: "https://cord.co/jobs", Countries: []string{"gb"}},
	{URL: "https://www.technojobs.co.uk/", Countries: []string{"gb"}},
	{URL: "https://www.cwjobs.co.uk/jobs/software-developer", Countries: []string{"gb"}},
	{URL: "https://www.jobserve.com/gb/en/IT-Jobs", Countries: []string{"gb"}},
	{URL: "https://www.efinancialcareers.co.uk/jobs/information-technology", Countries: []string{"gb"}},

	// ── European Union (pan-EU) ───────────────────────────────────────────────
	{URL: "https://arbeitnow.com/", Countries: []string{"eu"}},
	{URL: "https://euremotejobs.com/", Countries: []string{"eu"}},
	{URL: "https://jobs.techeu.com/jobs", Countries: []string{"eu"}},
	{URL: "https://techjobs.eu/", Countries: []string{"eu"}},
	{URL: "https://remoteurope.eu/jobs/", Countries: []string{"eu"}},
	{URL: "https://talent.io/p/en-gb/jobs", Countries: []string{"eu"}},
	{URL: "https://join.com/jobs", Countries: []string{"eu"}},
	{URL: "https://techloop.io/jobs", Countries: []string{"eu"}},
	{URL: "https://sifted.eu/jobs", Countries: []string{"eu"}},
	{URL: "https://www.jobgether.com/en/jobs", Countries: []string{"eu"}},
	{URL: "https://www.jobteaser.com/en/job-offers?contract_type=permanent", Countries: []string{"eu"}},
	{URL: "https://relocate.me/jobs", Countries: []string{"eu"}},
	{URL: "https://otta.com/jobs", Countries: []string{"eu"}},

	// ── Germany (EU) ─────────────────────────────────────────────────────────
	{URL: "https://www.xing.com/jobs/search?keywords=software+developer", Countries: []string{"de"}},
	{URL: "https://www.it-talents.de/stellenangebote", Countries: []string{"de"}},
	{URL: "https://www.entwickler.de/jobs", Countries: []string{"de"}},
	{URL: "https://www.stepstone.de/jobs/en", Countries: []string{"de"}},

	// ── Poland (EU) ──────────────────────────────────────────────────────────
	{URL: "https://bulldogjob.pl/companies/jobs", Countries: []string{"pl"}},
	{URL: "https://solid.jobs/offers/it", Countries: []string{"pl"}},
	{URL: "https://nofluffjobs.com/pl", Countries: []string{"pl"}},

	// ── Romania (EU) ─────────────────────────────────────────────────────────
	{URL: "https://www.hipo.ro/locuri-de-munca/it", Countries: []string{"ro"}},
	{URL: "https://www.ejobs.ro/locuri-de-munca/it", Countries: []string{"ro"}},

	// ── Czech Republic (EU) ──────────────────────────────────────────────────
	{URL: "https://techloop.io/jobs?country=cz", Countries: []string{"cz"}},
	{URL: "https://www.startupjobs.cz", Countries: []string{"cz"}},

	// ── France (EU) ──────────────────────────────────────────────────────────
	{URL: "https://www.welcometothejungle.com/en/jobs", Countries: []string{"fr"}},
	{URL: "https://www.talent.io/p/fr-fr/jobs", Countries: []string{"fr"}},

	// ── Netherlands (EU) ─────────────────────────────────────────────────────
	{URL: "https://www.intermediair.nl/vacatures/ict", Countries: []string{"nl"}},
	{URL: "https://amsterdamtechjobs.com", Countries: []string{"nl"}},

	// ── Spain (EU) ───────────────────────────────────────────────────────────
	{URL: "https://www.infojobs.net/ofertas-trabajo/informatica", Countries: []string{"es"}},
	{URL: "https://www.tecnoempleo.com", Countries: []string{"es"}},

	// ── Israel ────────────────────────────────────────────────────────────────
	{URL: "https://nortech-platform.com/explore/?type=job&sort=latest", Countries: []string{"il"}},
	{URL: "https://www.alljobs.co.il/", Countries: []string{"il"}},
	{URL: "https://www.drushim.co.il/", Countries: []string{"il"}},
	{URL: "https://www.jobmaster.co.il/", Countries: []string{"il"}},
	{URL: "https://www.gotfriends.co.il/jobs/", Countries: []string{"il"}},
	{URL: "https://www.comeet.com/jobs/search?country=Israel", Countries: []string{"il"}},
	{URL: "https://www.linkedin.com/jobs/search/?location=Israel&f_T=software-engineer", Countries: []string{"il"}},
	{URL: "https://www.startupnation.com/jobs/", Countries: []string{"il"}},
	{URL: "https://www.jobnet.co.il/", Countries: []string{"il"}},
	{URL: "https://www.smartr.me/", Countries: []string{"il"}},

	// ── Australia / New Zealand ───────────────────────────────────────────────
	{URL: "https://www.seek.com.au/software-engineer-jobs", Countries: []string{"au"}},
	{URL: "https://www.careerone.com.au/jobs?q=software+engineer", Countries: []string{"au"}},
	{URL: "https://au.indeed.com/jobs?q=software+engineer", Countries: []string{"au"}},
	{URL: "https://www.trademe.co.nz/a/jobs/computing", Countries: []string{"nz"}},

	// ── Singapore / APAC ─────────────────────────────────────────────────────
	{URL: "https://www.mycareersfuture.gov.sg/search?search=software+engineer", Countries: []string{"sg"}},
	{URL: "https://sg.indeed.com/jobs?q=software+engineer", Countries: []string{"sg"}},
	{URL: "https://www.techinasia.com/jobs", Countries: []string{"sg"}},
	{URL: "https://glints.com/sg/opportunities/jobs/explore", Countries: []string{"sg"}},

	// ── Canada ────────────────────────────────────────────────────────────────
	{URL: "https://ca.indeed.com/jobs?q=software+engineer", Countries: []string{"ca"}},
	{URL: "https://www.eluta.ca/jobs-for-software-engineer", Countries: []string{"ca"}},
	{URL: "https://jobs.jobillico.com/en/search-jobs?q=software+engineer", Countries: []string{"ca"}},
}
