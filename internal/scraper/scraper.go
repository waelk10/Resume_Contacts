package scraper

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
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

// Config controls scraper behaviour.
type Config struct {
	MaxDepth       int
	Parallelism    int           // concurrent requests per domain, clamped to [1, maxParallelism]
	Delay          time.Duration // base random delay between requests to the same domain
	RequestTimeout time.Duration // per-request wall-clock timeout (connect + headers + body)
	MaxBodyBytes   int           // maximum response body bytes read; excess is discarded
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

// webSeeds are autonomous entry points for the general web scraper.
// Focus on job boards and directories that expose contact emails.
var webSeeds = []string{
	// ── Global ───────────────────────────────────────────────────────────────
	"https://remoteok.com",
	"https://weworkremotely.com/listings",
	"https://startup.jobs",
	"https://wellfound.com/jobs",
	"https://www.workatastartup.com/jobs",
	"https://remotive.com/remote-jobs",

	// ── EU / Pan-European ─────────────────────────────────────────────────────
	"https://eu-startups.com/jobs",            // EU startup job board
	"https://otta.com/jobs",                   // UK/Europe growth-stage startups
	"https://www.honeypot.io/pages/jobs",      // developer-focused, DE/NL/AT/SE
	"https://landing.jobs/jobs",               // Portugal + broader EU tech
	"https://relocate.me/jobs",               // relocation-friendly EU roles
	"https://www.jobgether.com/en/jobs",       // hybrid/remote EU
	"https://nofluffjobs.com/jobs",            // CEE (PL/CZ/SK/RO) tech jobs
	"https://eurojobs.com/jobs",               // pan-European listings
	"https://tech.eu/jobs",                    // European tech news + jobs
	"https://berlinstartupjobs.com",           // Berlin startup ecosystem
	"https://amsterdamtechjobs.com",           // Netherlands tech scene
	"https://jobs.techcorridor.eu",            // Central/Eastern Europe tech

	// ── Country-specific ─────────────────────────────────────────────────────
	"https://www.totaljobs.com/jobs/it-jobs",  // UK (large volume)
	"https://www.reed.co.uk/jobs/it-jobs",     // UK
	"https://www.cwjobs.co.uk/jobs",           // UK tech specialist board
	"https://www.welcometothejungle.com/en/jobs", // France (English listings)
	"https://www.tecnoempleo.com",             // Spain tech jobs
	"https://www.jobsinbarcelona.es",          // Spain/Barcelona hub
	"https://www.stepstone.de/jobs/en",        // Germany (English filter)
	"https://www.karriere.at/jobs",            // Austria
	"https://www.jobat.be/en/jobs",            // Belgium
	"https://www.thelocal.se/jobs",            // Sweden expat/English jobs
	"https://www.finn.no/job/fulltime/search.html", // Norway
	"https://www.jobindex.dk/jobsoegning",     // Denmark
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
func (e *Engine) Run() error {
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := e.runHN(); err != nil {
			log.Printf("[hn] %v", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		e.runWeb()
	}()

	wg.Wait()
	return nil
}

// runHN scrapes the latest "Ask HN: Who is Hiring?" thread.
// HN is the single richest source of recruiter/hiring-manager emails.
func (e *Engine) runHN() error {
	client := &http.Client{
		Timeout:   e.cfg.RequestTimeout,
		Transport: newTransport(),
	}

	threadID, err := hnLatestHiringID(client)
	if err != nil {
		return fmt.Errorf("finding thread: %w", err)
	}
	log.Printf("[hn] thread id=%s", threadID)

	thread, err := hnFetchItem(client, threadID)
	if err != nil {
		return fmt.Errorf("fetching thread: %w", err)
	}
	log.Printf("[hn] %d top-level comments to scan", len(thread.Kids))

	sem := make(chan struct{}, e.cfg.parallelism())
	var wg sync.WaitGroup
	for _, kid := range thread.Kids {
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

// runWeb crawls job boards and company pages for exposed emails.
func (e *Engine) runWeb() {
	blocker := newDomainBlocker(3)

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

	// Reset the failure counter for a host whenever a response arrives cleanly.
	c.OnResponse(func(r *colly.Response) {
		blocker.recordSuccess(r.Request.URL.Hostname())
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

	// Follow links that look like contact/team/careers pages, skipping blocked FQDNs.
	c.OnHTML("a[href]", func(el *colly.HTMLElement) {
		abs := el.Request.AbsoluteURL(el.Attr("href"))
		if !isRelevantURL(abs) {
			return
		}
		u, err := url.Parse(abs)
		if err != nil || blocker.isBlocked(u.Hostname()) {
			return
		}
		_ = c.Visit(abs)
	})

	c.OnError(func(r *colly.Response, err error) {
		host := r.Request.URL.Hostname()
		log.Printf("[web] %s: %v", r.Request.URL, err)
		blocker.recordFailure(host)
	})

	for _, seed := range webSeeds {
		u, err := url.Parse(seed)
		if err != nil || blocker.isBlocked(u.Hostname()) {
			continue
		}
		_ = c.Visit(seed)
	}
	c.Wait()
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

// hnDecode reads at most hnBodyLimit bytes from resp.Body into dst via a
// buffered reader, then drains and closes the body regardless of outcome.
func hnDecode(resp *http.Response, dst any) error {
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
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
