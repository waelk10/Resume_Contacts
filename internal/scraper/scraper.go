package scraper

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gocolly/colly/v2"
	"github.com/gocolly/colly/v2/extensions"

	"Resume_Contacts_Scraper/internal/contact"
	"Resume_Contacts_Scraper/internal/extractor"
	"Resume_Contacts_Scraper/internal/memguard"
)

const (
	// maxParallelism caps the contact scraper, which crawls arbitrary company
	// websites and must stay polite.
	maxParallelism = 8
	// maxAppScanParallelism caps the app-page scanner, which hits large ATS
	// platforms that can sustain much higher concurrency.
	maxAppScanParallelism = 512
)

// reseedDelay is how long runWeb waits between crawl cycles once the queue drains.
const reseedDelay = 5 * time.Minute

// hnRecheckInterval is how long runHN waits between polls for new HN threads.
const hnRecheckInterval = 1 * time.Hour

// Config controls scraper behaviour.
type Config struct {
	MaxDepth       int
	Parallelism    int           // concurrent requests per domain, clamped to [1, maxParallelism]
	Delay          time.Duration // base random delay between requests to the same domain
	RequestTimeout time.Duration // per-request wall-clock timeout (connect + headers + body)
	MaxBodyBytes   int           // maximum response body bytes read; excess is discarded
	ExtraSeeds     []string      // additional seed URLs merged with the built-in list
	Countries      []string      // ISO 3166-1 alpha-2 codes / region aliases to filter built-in seeds; nil = all
	IgnoreCountries []string     // codes / region aliases whose seeds are always excluded; nil = nothing excluded
	// Roles, when non-nil, restricts AppScanner to job-application links whose
	// anchor text contains at least one of the listed keywords (case-insensitive
	// substring).  Links with absent or generic anchor text are always followed.
	// nil (default) disables role filtering.
	Roles []string
	// BlockedDomains is a list of domains (e.g. "example.com") whose URLs —
	// and any subdomain thereof — are never emitted by AppScanner.
	// Entries are matched case-insensitively against the URL host.
	BlockedDomains []string
}

// BuiltInSeeds returns the URLs of all built-in seeds regardless of country filter.
// Used by the source-discovery command to deduplicate against known hosts.
func BuiltInSeeds() []string {
	out := make([]string, len(webSeeds))
	for i, s := range webSeeds {
		out[i] = s.URL
	}
	return out
}

// buildSeeds returns built-in seeds (filtered by cfg.Countries when set) merged
// with cfg.ExtraSeeds, shuffled so every run hits targets in a different sequence.
func (cfg Config) buildSeeds() []string {
	filter := expandCountries(cfg.Countries)
	ignoreFilter := expandCountries(cfg.IgnoreCountries)
	var all []string
	for _, s := range webSeeds {
		if filter == nil || seedMatchesFilter(s.Countries, filter) {
			if ignoreFilter == nil || !seedMatchesFilter(s.Countries, ignoreFilter) {
				all = append(all, s.URL)
			}
		}
	}
	all = append(all, cfg.ExtraSeeds...)
	rand.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	return all
}

var DefaultConfig = Config{
	MaxDepth:       2,
	Parallelism:    4,
	Delay:          2 * time.Second,
	RequestTimeout: 30 * time.Second,
	MaxBodyBytes:   2 * 1024 * 1024, // 2 MB
}

// newTransport returns an http.Transport with sensible dial and header
// timeouts so individual stages of a slow connection are bounded independently
// of the overall request timeout.
func newTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout:   15 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		// Restrict to classical curves only. Go 1.23+ advertises post-quantum
		// key shares (Kyber/MLKEM) by default; many self-hosted servers running
		// older TLS stacks reject the oversized ClientHello with a handshake
		// failure alert (e.g. fsf.org). Listing only classical curves keeps the
		// ClientHello small and compatible without weakening cipher security.
		TLSClientConfig: &tls.Config{
			CurvePreferences: []tls.CurveID{
				tls.X25519,
				tls.CurveP256,
				tls.CurveP384,
				tls.CurveP521,
			},
		},
	}
}

// ctxTransport wraps a base RoundTripper and attaches a context to every
// outgoing request.  When ctx is cancelled the transport tears down the
// connection immediately instead of waiting for the full request timeout.
type ctxTransport struct {
	base http.RoundTripper
	ctx  context.Context
}

func (t *ctxTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.ctx.Err() != nil {
		return nil, t.ctx.Err()
	}
	return t.base.RoundTrip(req.WithContext(t.ctx))
}

func (c Config) parallelism() int {
	if c.Parallelism < 1 {
		return 1
	}
	if c.Parallelism > maxParallelism {
		return maxParallelism
	}
	return c.Parallelism
}

func (c Config) appScanParallelism() int {
	if c.Parallelism < 1 {
		return 1
	}
	if c.Parallelism > maxAppScanParallelism {
		return maxAppScanParallelism
	}
	return c.Parallelism
}

// newAppScanTransport returns an http.Transport tuned for high-concurrency
// bulk ATS crawling: larger idle-connection pool, faster fail-fast timeouts
// so slow/dead hosts don't hold goroutines for 15–30 s.
func newAppScanTransport(parallelism int) *http.Transport {
	perHost := 8
	if parallelism > 64 {
		perHost = 16
	}
	dialer := &net.Dialer{
		Timeout:   8 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		MaxIdleConns:          parallelism * 4,
		MaxIdleConnsPerHost:   perHost,
		IdleConnTimeout:       90 * time.Second,
		TLSClientConfig: &tls.Config{
			CurvePreferences: []tls.CurveID{
				tls.X25519,
				tls.CurveP256,
				tls.CurveP384,
				tls.CurveP521,
			},
		},
	}
}

const (
	// softBlockWindow is the sliding time window used to count failures for
	// the soft-block mechanism.
	softBlockWindow = 2 * time.Minute
	// softBlockDuration is how long a domain is soft-blocked after hitting the
	// threshold within the window.
	softBlockDuration = 10 * time.Minute
	// softBlockThreshold is the number of failures within softBlockWindow that
	// triggers a soft-block.
	softBlockThreshold = 3
)

// domainBlocker tracks per-FQDN failures and enforces two tiers of blocking:
//   - Soft-block: ≥softBlockThreshold failures within softBlockWindow → blocked
//     for softBlockDuration, then automatically eligible for retry.
//   - Permanent block: ≥threshold consecutive failures, or an explicitly
//     unrecoverable error (e.g. TLS) → domain skipped for the rest of the run.
type domainBlocker struct {
	mu          sync.Mutex
	failures    map[string]int       // consecutive-failure counter (for permanent block)
	blocked     map[string]bool      // permanently blocked domains
	threshold   int                  // consecutive failures before permanent block
	prefix      string               // log prefix, e.g. "[web]" or "[app]"
	failTimes   map[string][]time.Time // recent failure timestamps (for soft-block)
	softBlocked map[string]time.Time   // domain → soft-block expiry
}

func newDomainBlocker(threshold int, prefix string) *domainBlocker {
	return &domainBlocker{
		failures:    make(map[string]int),
		blocked:     make(map[string]bool),
		threshold:   threshold,
		prefix:      prefix,
		failTimes:   make(map[string][]time.Time),
		softBlocked: make(map[string]time.Time),
	}
}

func (db *domainBlocker) isBlocked(host string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.blocked[host] {
		return true
	}
	if exp, ok := db.softBlocked[host]; ok {
		if time.Now().Before(exp) {
			return true
		}
		delete(db.softBlocked, host) // expired — evict to prevent unbounded map growth
	}
	return false
}

// recordFailure records a failure for host, applying both the soft-block
// (time-windowed) and the permanent-block (consecutive-count) logic.
// Returns true when host becomes permanently blocked.
func (db *domainBlocker) recordFailure(host string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()

	now := time.Now()

	// Soft-block: maintain a sliding window of failure timestamps.
	cutoff := now.Add(-softBlockWindow)
	times := db.failTimes[host]
	// Drop timestamps outside the window.
	i := 0
	for i < len(times) && times[i].Before(cutoff) {
		i++
	}
	times = append(times[i:], now)
	db.failTimes[host] = times
	if len(times) >= softBlockThreshold {
		if exp, ok := db.softBlocked[host]; !ok || now.After(exp) {
			db.softBlocked[host] = now.Add(softBlockDuration)
			log.Printf("%s soft-blocking %s for %v (%d failures in %v)",
				db.prefix, host, softBlockDuration, softBlockThreshold, softBlockWindow)
		}
	}

	// Permanent block: N consecutive failures.
	db.failures[host]++
	if !db.blocked[host] && db.failures[host] >= db.threshold {
		db.blocked[host] = true
		log.Printf("%s permanently blocking %s after %d consecutive failures",
			db.prefix, host, db.threshold)
		return true
	}
	return db.blocked[host]
}

// recordSuccess resets the consecutive-failure counter and the soft-block
// failure window for host. An active soft-block is left to expire on its own
// so flaky domains cannot escape the cooldown on a single lucky response.
func (db *domainBlocker) recordSuccess(host string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	delete(db.failures, host)
	delete(db.failTimes, host)
}

// blockNow immediately permanently-blocks a host, bypassing the failure
// threshold. Use for errors that are guaranteed to be domain-wide and permanent
// (e.g. TLS handshake failures), so we avoid repeated log spam.
func (db *domainBlocker) blockNow(host string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.blocked[host] = true
}

// Engine orchestrates all scraping sources.
type Engine struct {
	cfg       Config
	on        func(contact.Contact)
	guard     *memguard.Guard
	metaSeeds *metaSeedStore
}

func New(cfg Config, onContact func(contact.Contact)) *Engine {
	return &Engine{cfg: cfg, on: onContact, guard: memguard.New(), metaSeeds: newMetaSeedStore()}
}

// searchQueryParams are query-string parameter names that identify a search
// or filter interface on a job board (as opposed to a static listing page).
var searchQueryParams = []string{"q", "query", "keywords", "keyword", "k", "search", "s", "term"}

// isSearchURL reports whether u looks like a job-board search-results URL:
// it must pass isRelevantURLParsed (job context) AND carry a non-empty search
// query parameter.
func isSearchURL(u *url.URL) bool {
	if !isRelevantURLParsed(u) {
		return false
	}
	q := u.Query()
	for _, p := range searchQueryParams {
		if q.Get(p) != "" {
			return true
		}
	}
	return false
}

// Run launches all scraping sources concurrently and blocks until ctx is
// cancelled (e.g. SIGINT / SIGTERM).
func (e *Engine) Run(ctx context.Context) error {
	var wg sync.WaitGroup

	for _, fn := range []func(context.Context){
		e.runWeb,
		e.runHN,
	} {
		fn := fn
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(ctx)
		}()
	}

	wg.Wait()
	return nil
}

// runHN periodically fetches "Ask HN: Who is Hiring?" threads and emits
// contacts. It loops forever, sleeping hnRecheckInterval between polls, and
// exits when ctx is cancelled.
func (e *Engine) runHN(ctx context.Context) {
	client := &http.Client{
		Timeout:   e.cfg.RequestTimeout,
		Transport: newTransport(),
	}
	seen := make(map[string]bool)

	for {
		threadID, err := hnLatestHiringID(ctx, client)
		if err != nil {
			log.Printf("[hn] finding thread: %v", err)
		} else if seen[threadID] {
			log.Printf("[hn] thread %s already processed — waiting", threadID)
		} else {
			seen[threadID] = true
			log.Printf("[hn] thread id=%s", threadID)
			if err := e.processHNThread(ctx, client, threadID); err != nil {
				log.Printf("[hn] processing thread: %v", err)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(hnRecheckInterval):
		}
	}
}

// processHNThread fetches all top-level comments for threadID and emits emails.
// Returns early when ctx is cancelled.
func (e *Engine) processHNThread(ctx context.Context, client *http.Client, threadID string) error {
	thread, err := hnFetchItem(ctx, client, threadID)
	if err != nil {
		return fmt.Errorf("fetching thread: %w", err)
	}
	log.Printf("[hn] %d top-level comments to scan", len(thread.Kids))

	sem := make(chan struct{}, e.cfg.parallelism())
	var wg sync.WaitGroup
	for _, kid := range thread.Kids {
		if err := e.guard.Throttle(ctx); err != nil {
			wg.Wait()
			return err
		}
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			defer func() { <-sem }()

			if ctx.Err() != nil {
				return
			}
			item, err := hnFetchItem(ctx, client, fmt.Sprint(id))
			if err != nil || item.Text == "" {
				return
			}
			src := fmt.Sprintf("https://news.ycombinator.com/item?id=%d", id)
			for _, em := range extractor.ExtractEmails(item.Text) {
				e.on(contact.Contact{
					Email:  em,
					Org:    extractor.OrgFromEmail(em),
					Source: src,
				})
			}
			select {
			case <-ctx.Done():
			case <-time.After(e.cfg.Delay / 4):
			}
		}(kid)
	}
	wg.Wait()
	return nil
}

// runWeb crawls job boards and company pages for exposed emails, running
// forever until ctx is cancelled. Each cycle creates a fresh colly Collector
// (resetting its visited-URL cache) and re-seeds from the full seed list.
// When the queue drains, it sleeps reseedDelay before the next cycle.
func (e *Engine) runWeb(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		e.metaSeeds.load()

		blocker := newDomainBlocker(3, "[web]")
		queue := newURLQueue()
		c := e.newCollector(ctx, blocker, queue)

		// Merge static seeds with discovered meta-seeds, deduplicating so a URL
		// that appears in both lists is only queued once.
		staticSeeds := e.cfg.buildSeeds()
		inStatic := make(map[string]bool, len(staticSeeds))
		for _, s := range staticSeeds {
			inStatic[s] = true
		}
		allSeeds := staticSeeds
		for _, s := range e.metaSeeds.all() {
			if !inStatic[s] {
				allSeeds = append(allSeeds, s)
			}
		}
		for _, seed := range allSeeds {
			u, err := url.Parse(seed)
			if err != nil || blocker.isBlocked(u.Hostname()) {
				continue
			}
			queue.Push(seed)
		}

		for queue.Len() > 0 {
			select {
			case <-ctx.Done():
				c.Wait()
				return
			default:
			}
			if err := e.guard.Throttle(ctx); err != nil {
				c.Wait()
				return
			}
			batch := queue.drain(nil)
			for _, u := range batch {
				_ = c.Visit(u)
			}
			c.Wait()
		}

		e.metaSeeds.flush()
		log.Printf("[web] queue exhausted — re-seeding in %s", reseedDelay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(reseedDelay):
		}
	}
}

// newCollector creates a fresh colly Collector wired to the given blocker and
// queue. A new Collector resets colly's visited-URL cache so that seeds are
// re-crawled each cycle without duplicating work within a single cycle.
//
// ctx is attached to every outgoing HTTP request so that cancelling ctx
// (e.g. Ctrl+C) causes the transport to abort in-flight connections
// immediately, allowing c.Wait() to return within milliseconds.
func (e *Engine) newCollector(ctx context.Context, blocker *domainBlocker, queue *urlQueue) *colly.Collector {
	c := colly.NewCollector(
		colly.MaxDepth(e.cfg.MaxDepth),
		colly.Async(true),
		colly.MaxBodySize(e.cfg.MaxBodyBytes),
	)
	// ctxTransport wraps every HTTP request with ctx so the transport aborts
	// in-flight connections the moment ctx is cancelled (e.g. Ctrl+C).
	c.WithTransport(&ctxTransport{base: newTransport(), ctx: ctx})
	c.SetRequestTimeout(e.cfg.RequestTimeout)
	extensions.RandomUserAgent(c)

	// Also abort colly's queued-but-not-yet-dispatched requests on cancel.
	c.OnRequest(func(r *colly.Request) {
		if ctx.Err() != nil {
			r.Abort()
		}
	})
	if err := c.Limit(&colly.LimitRule{
		DomainGlob:  "*",
		Parallelism: e.cfg.parallelism(),
		RandomDelay: e.cfg.Delay,
	}); err != nil {
		log.Printf("[web] rate limit setup: %v", err)
	}

	// Reset the failure counter only on clean 2xx responses.
	c.OnResponse(func(r *colly.Response) {
		if r.StatusCode >= 200 && r.StatusCode < 300 {
			blocker.recordSuccess(r.Request.URL.Hostname())
		}
	})

	// mailto links are the most reliable signal — often carry a real name too.
	c.OnHTML("a[href^='mailto:']", func(el *colly.HTMLElement) {
		raw := strings.TrimPrefix(el.Attr("href"), "mailto:")
		if i := strings.Index(raw, "?"); i >= 0 {
			raw = raw[:i]
		}
		name := strings.TrimSpace(el.Text)
		for _, em := range extractor.ExtractEmails(raw) {
			if strings.EqualFold(name, em) {
				name = ""
			}
			e.on(contact.Contact{
				Name:   name,
				Email:  em,
				Org:    extractor.OrgFromEmail(em),
				Source: el.Request.URL.String(),
			})
		}
	})

	// Scan full page text for inline emails not wrapped in mailto links.
	// Uses context-aware extraction: only keeps emails found near hiring
	// keywords (recruiter, hr, careers, job, apply, …) to filter out the
	// large volume of personal/unrelated emails that appear in team bios,
	// investor pages, footers, and support sections.
	c.OnHTML("body", func(el *colly.HTMLElement) {
		src := el.Request.URL.String()
		for _, em := range extractor.ExtractEmailsFromBodyText(el.Text) {
			e.on(contact.Contact{
				Email:  em,
				Org:    extractor.OrgFromEmail(em),
				Source: src,
			})
		}
	})

	// Enqueue links that look like contact/team/careers pages.
	// Search URLs (those with a job-context path and a search query param) are
	// also saved to the meta-seed store so they seed future crawl cycles.
	c.OnHTML("a[href]", func(el *colly.HTMLElement) {
		abs := el.Request.AbsoluteURL(el.Attr("href"))
		if !isRelevantURL(abs) {
			return
		}
		u, err := url.Parse(abs)
		if err != nil || blocker.isBlocked(u.Hostname()) {
			return
		}
		queue.Push(abs)
		if isSearchURL(u) {
			e.metaSeeds.add(abs)
		}
	})

	c.OnError(func(r *colly.Response, err error) {
		host := r.Request.URL.Hostname()
		switch {
		case r.StatusCode == http.StatusNotFound:
			// 404s are normal on the live web; don't penalise the domain.
			log.Printf("[web] %s: 404 (skipped)", r.Request.URL)
		case r.StatusCode == http.StatusTooManyRequests:
			log.Printf("[web] %s: rate-limited (429)", host)
			blocker.recordFailure(host)
		case err != nil && strings.Contains(err.Error(), "tls:"):
			// TLS failures are domain-wide and permanent; block immediately so we
			// don't retry and spam the log for every subsequent URL on that host.
			log.Printf("[web] %s: TLS error — skipping domain: %v", host, err)
			blocker.blockNow(host)
		default:
			log.Printf("[web] %s: %v", r.Request.URL, err)
			blocker.recordFailure(host)
		}
	})

	return c
}

// relevantSegKws is the set of path-segment keywords that indicate a page may
// contain recruiter/hiring contact info.  Matching is done at segment word
// boundaries (see segMatchesKw) to avoid false positives from unrelated slugs
// like "/gifts-for-people-who-have-everything" matching the keyword "people".
var relevantSegKws = []string{
	// English
	"contact", "contacts",
	"about",
	"team", "teams",
	"career", "careers",
	"job", "jobs",
	"hire", "hiring",
	"recruit", "recruiting", "recruitment",
	"people",
	"staff",
	"join",
	"opening", "openings",
	"position", "positions",
	"vacancy", "vacancies",
	"listing", "listings",
	// German
	"kontakt",
	"karriere",
	"stelle", "stellen",
	"stellenangebot", "stellenangebote",
	"ausschreibung", "ausschreibungen",
	"bewerbung", "bewerben",
	"einstellung",
	"jobsuche", "jobdetail",
}

// isRelevantURL returns true for pages likely to contain contact emails.
// It splits the path on "/" and checks each segment at word boundaries
// (hyphens and underscores) rather than searching the full path string.
// This prevents false positives like "/gifts-for-people-who-have-everything"
// matching the keyword "people".
func isRelevantURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	for _, seg := range strings.Split(strings.ToLower(u.Path), "/") {
		if seg == "" {
			continue
		}
		for _, kw := range relevantSegKws {
			if segMatchesKw(seg, kw) {
				return true
			}
		}
	}
	// Also check query-string parameters for search/filter contexts.
	q := strings.ToLower(u.RawQuery)
	for _, kw := range []string{"job", "career", "vacancy", "stelle", "jobsuche", "bewerb"} {
		if strings.Contains(q, kw) {
			return true
		}
	}
	return false
}

// isRelevantURLParsed is the hot-path variant of isRelevantURL for callers that
// have already parsed the URL (avoids a redundant url.Parse call per link in
// OnHTML).
func isRelevantURLParsed(u *url.URL) bool {
	for _, seg := range strings.Split(strings.ToLower(u.Path), "/") {
		if seg == "" {
			continue
		}
		for _, kw := range relevantSegKws {
			if segMatchesKw(seg, kw) {
				return true
			}
		}
	}
	q := strings.ToLower(u.RawQuery)
	for _, kw := range []string{"job", "career", "vacancy", "stelle", "jobsuche", "bewerb"} {
		if strings.Contains(q, kw) {
			return true
		}
	}
	return false
}

// segMatchesKw reports whether a URL path segment contains kw at a slug word
// boundary: the segment equals kw, or kw appears at the start/end separated
// by a hyphen or underscore.  This accepts "/people", "/our-people",
// "/people-ops" but rejects "/gifts-for-people-who-have-everything".
func segMatchesKw(seg, kw string) bool {
	return seg == kw ||
		strings.HasPrefix(seg, kw+"-") || strings.HasPrefix(seg, kw+"_") ||
		strings.HasSuffix(seg, "-"+kw) || strings.HasSuffix(seg, "_"+kw)
}

// ── HN Firebase API ──────────────────────────────────────────────────────────

type hnItem struct {
	Text string `json:"text"`
	Kids []int  `json:"kids"`
}

type hnSearchResult struct {
	Hits []struct {
		ObjectID string `json:"objectID"`
	} `json:"hits"`
}

// hnBodyLimit caps how many bytes we read from the HN / Algolia APIs.
// Their JSON responses are always small; this prevents a runaway body
// from blocking the goroutine indefinitely.
const hnBodyLimit = 512 * 1024 // 512 KB

// hnDecode checks the response status, then reads at most hnBodyLimit bytes
// into dst via a buffered reader. Always drains and closes the body.
func hnDecode(resp *http.Response, dst any) error {
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("rate-limited (429)")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	lr := io.LimitReader(resp.Body, hnBodyLimit)
	return json.NewDecoder(bufio.NewReaderSize(lr, 32*1024)).Decode(dst)
}

func hnLatestHiringID(ctx context.Context, client *http.Client) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://hn.algolia.com/api/v1/search?query=Ask+HN+Who+is+Hiring&tags=story&hitsPerPage=1", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	var r hnSearchResult
	if err := hnDecode(resp, &r); err != nil {
		return "", err
	}
	if len(r.Hits) == 0 {
		return "", fmt.Errorf("no results from Algolia")
	}
	return r.Hits[0].ObjectID, nil
}

func hnFetchItem(ctx context.Context, client *http.Client, id string) (*hnItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://hacker-news.firebaseio.com/v0/item/"+id+".json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	var item hnItem
	if err := hnDecode(resp, &item); err != nil {
		return nil, err
	}
	return &item, nil
}
