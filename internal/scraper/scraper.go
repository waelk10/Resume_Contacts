package scraper

import (
	"bufio"
	"context"
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
)

const maxParallelism = 8

// reseedDelay is how long runWeb waits between crawl cycles once the queue drains.
const reseedDelay = 30 * time.Minute

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
	var all []string
	for _, s := range webSeeds {
		if filter == nil || seedMatchesFilter(s.Countries, filter) {
			all = append(all, s.URL)
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
	}
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

// domainBlocker tracks consecutive per-FQDN failures and blocks a domain once
// it reaches the failure threshold, skipping all future visits to that FQDN.
type domainBlocker struct {
	mu        sync.Mutex
	failures  map[string]int
	blocked   map[string]bool
	threshold int
}

func newDomainBlocker(threshold int) *domainBlocker {
	return &domainBlocker{
		failures:  make(map[string]int),
		blocked:   make(map[string]bool),
		threshold: threshold,
	}
}

func (db *domainBlocker) isBlocked(host string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.blocked[host]
}

// recordFailure increments the consecutive-failure counter and blocks the host
// if the threshold is reached. Returns true when the host becomes blocked.
func (db *domainBlocker) recordFailure(host string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.failures[host]++
	if !db.blocked[host] && db.failures[host] >= db.threshold {
		db.blocked[host] = true
		log.Printf("[web] blocking %s after %d consecutive failures", host, db.threshold)
		return true
	}
	return db.blocked[host]
}

// recordSuccess resets the consecutive-failure counter for a host (does not
// un-block a host that has already been blocked).
func (db *domainBlocker) recordSuccess(host string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	delete(db.failures, host)
}

// Engine orchestrates all scraping sources.
type Engine struct {
	cfg Config
	on  func(contact.Contact)
}

func New(cfg Config, onContact func(contact.Contact)) *Engine {
	return &Engine{cfg: cfg, on: onContact}
}

// Run launches the HN source and the general web scraper concurrently.
// It blocks until ctx is cancelled (e.g. SIGINT / SIGTERM).
func (e *Engine) Run(ctx context.Context) error {
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		e.runHN(ctx)
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		e.runWeb(ctx)
	}()

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
		threadID, err := hnLatestHiringID(client)
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
	thread, err := hnFetchItem(client, threadID)
	if err != nil {
		return fmt.Errorf("fetching thread: %w", err)
	}
	log.Printf("[hn] %d top-level comments to scan", len(thread.Kids))

	sem := make(chan struct{}, e.cfg.parallelism())
	var wg sync.WaitGroup
	for _, kid := range thread.Kids {
		select {
		case <-ctx.Done():
			wg.Wait()
			return ctx.Err()
		default:
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(id int) {
			defer wg.Done()
			defer func() { <-sem }()

			item, err := hnFetchItem(client, fmt.Sprint(id))
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
			time.Sleep(e.cfg.Delay / 4)
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
	blocker := newDomainBlocker(3)

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		queue := newURLQueue()
		c := e.newCollector(blocker, queue)

		for _, seed := range e.cfg.buildSeeds() {
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
			batch := queue.drain(nil)
			for _, u := range batch {
				_ = c.Visit(u)
			}
			c.Wait()
		}

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
func (e *Engine) newCollector(blocker *domainBlocker, queue *urlQueue) *colly.Collector {
	c := colly.NewCollector(
		colly.MaxDepth(e.cfg.MaxDepth),
		colly.Async(true),
		colly.MaxBodySize(e.cfg.MaxBodyBytes),
	)
	c.WithTransport(newTransport())
	c.SetRequestTimeout(e.cfg.RequestTimeout)
	extensions.RandomUserAgent(c)
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
	c.OnHTML("body", func(el *colly.HTMLElement) {
		src := el.Request.URL.String()
		for _, em := range extractor.ExtractEmails(el.Text) {
			e.on(contact.Contact{
				Email:  em,
				Org:    extractor.OrgFromEmail(em),
				Source: src,
			})
		}
	})

	// Enqueue links that look like contact/team/careers pages.
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
	})

	c.OnError(func(r *colly.Response, err error) {
		host := r.Request.URL.Hostname()
		if r.StatusCode == http.StatusTooManyRequests {
			log.Printf("[web] %s: rate-limited (429)", host)
		} else {
			log.Printf("[web] %s: %v", r.Request.URL, err)
		}
		blocker.recordFailure(host)
	})

	return c
}

// isRelevantURL returns true for pages likely to contain contact emails.
func isRelevantURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	p := strings.ToLower(u.Path + "?" + u.RawQuery)
	for _, kw := range []string{
		"contact", "about", "team", "career", "job", "hire",
		"recruit", "people", "staff", "join", "opening",
		"position", "vacancy", "listing",
	} {
		if strings.Contains(p, kw) {
			return true
		}
	}
	return false
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

func hnLatestHiringID(client *http.Client) (string, error) {
	resp, err := client.Get(
		"https://hn.algolia.com/api/v1/search?query=Ask+HN+Who+is+Hiring&tags=story&hitsPerPage=1",
	)
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

func hnFetchItem(client *http.Client, id string) (*hnItem, error) {
	resp, err := client.Get(
		"https://hacker-news.firebaseio.com/v0/item/" + id + ".json",
	)
	if err != nil {
		return nil, err
	}
	var item hnItem
	if err := hnDecode(resp, &item); err != nil {
		return nil, err
	}
	return &item, nil
}
