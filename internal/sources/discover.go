package sources

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultMetaSources are pages that enumerate job boards and hiring sources.
// All are plain text / HTML served without JavaScript, making them reliable
// even without a headless browser.
var DefaultMetaSources = []string{
	"https://raw.githubusercontent.com/tramcar/awesome-job-boards/master/README.md",
	"https://raw.githubusercontent.com/lukasz-madon/awesome-remote-job/master/README.md",
	"https://raw.githubusercontent.com/remoteintech/remote-jobs/main/README.md",
	"https://raw.githubusercontent.com/nickytonline/remote-jobs-boards/main/README.md",
	"https://raw.githubusercontent.com/engineerapart/theremotepath/master/README.md",
	"https://raw.githubusercontent.com/reHackable/awesome-reMarkable/master/README.md",
	"https://github.com/topics/job-board",
	"https://github.com/topics/remote-jobs",
}

// Config controls a discovery run.
type Config struct {
	Concurrency    int
	RequestTimeout time.Duration
	MetaSources    []string
	// Jitter is the upper bound of a random pre-request delay added to each
	// liveness probe. Zero disables jitter.
	Jitter time.Duration
}

// DefaultConfig is a ready-to-use configuration.
var DefaultConfig = Config{
	Concurrency:    6,
	RequestTimeout: 20 * time.Second,
	MetaSources:    DefaultMetaSources,
	Jitter:         400 * time.Millisecond,
}

// Result is a single discovered source URL together with the meta-page it came from.
type Result struct {
	URL    string
	Source string
}

// Discoverer fetches meta-source pages, extracts candidate job-board URLs,
// validates each with a lightweight HEAD request, and returns those that are
// both reachable and not already present in the caller's existing seed list.
type Discoverer struct {
	cfg     Config
	client  *http.Client
	blocker *tempBlocker
}

// New returns a Discoverer ready to use.
func New(cfg Config) *Discoverer {
	return &Discoverer{
		cfg:     cfg,
		blocker: newTempBlocker(),
		client: &http.Client{
			Timeout: cfg.RequestTimeout,
			Transport: &http.Transport{
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 15 * time.Second,
				IdleConnTimeout:       60 * time.Second,
				MaxIdleConns:          50,
			},
		},
	}
}

// Run fetches all configured meta-sources, extracts candidate URLs, validates
// them, and returns de-duplicated results that are absent from existing.
func (d *Discoverer) Run(existing []string) ([]Result, error) {
	existingHosts := make(map[string]bool, len(existing))
	for _, u := range existing {
		if p, err := url.Parse(u); err == nil {
			existingHosts[p.Hostname()] = true
		}
	}

	type candidate struct {
		rawURL string
		source string
	}

	var (
		mu         sync.Mutex
		candidates []candidate
	)

	// Shuffle meta-sources so every run fetches them in a different order,
	// which spreads load and avoids always deduping in favour of the same source.
	metaSrcs := make([]string, len(d.cfg.MetaSources))
	copy(metaSrcs, d.cfg.MetaSources)
	rand.Shuffle(len(metaSrcs), func(i, j int) { metaSrcs[i], metaSrcs[j] = metaSrcs[j], metaSrcs[i] })

	sem := make(chan struct{}, d.cfg.Concurrency)
	var wg sync.WaitGroup
	for _, metaSrc := range metaSrcs {
		wg.Add(1)
		sem <- struct{}{}
		go func(src string) {
			defer wg.Done()
			defer func() { <-sem }()
			urls, err := d.extractURLs(src)
			if err != nil {
				log.Printf("[discover] %s: %v", src, err)
				return
			}
			log.Printf("[discover] %s: %d candidates", src, len(urls))
			mu.Lock()
			for _, u := range urls {
				candidates = append(candidates, candidate{rawURL: u, source: src})
			}
			mu.Unlock()
		}(metaSrc)
	}
	wg.Wait()

	// Shuffle candidates before dedup so which hostname "wins" when two meta-sources
	// both reference the same host varies across runs.
	rand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })

	// Deduplicate across all candidates and against the existing seed list.
	seen := make(map[string]bool, len(existingHosts)+len(candidates))
	for h := range existingHosts {
		seen[h] = true
	}
	type workItem struct {
		rawURL string
		source string
	}
	var unique []workItem
	for _, c := range candidates {
		p, err := url.Parse(c.rawURL)
		if err != nil {
			continue
		}
		h := p.Hostname()
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		unique = append(unique, workItem{rawURL: c.rawURL, source: c.source})
	}

	log.Printf("[discover] %d unique candidates after dedup (%d already known)", len(unique), len(existingHosts))

	// Shuffle unique items so validation probes hit different hosts in different
	// orders on each run, reducing predictable burst patterns per domain.
	rand.Shuffle(len(unique), func(i, j int) { unique[i], unique[j] = unique[j], unique[i] })

	// Validate each candidate — only keep reachable hosts.
	resultCh := make(chan Result, len(unique))
	sem2 := make(chan struct{}, d.cfg.Concurrency)
	var wg2 sync.WaitGroup
	for _, w := range unique {
		wg2.Add(1)
		sem2 <- struct{}{}
		go func(w workItem) {
			defer wg2.Done()
			defer func() { <-sem2 }()
			if d.cfg.Jitter > 0 {
				time.Sleep(time.Duration(rand.Int63n(int64(d.cfg.Jitter))))
			}
			if d.isLive(w.rawURL) {
				resultCh <- Result{URL: w.rawURL, Source: w.source}
			}
		}(w)
	}
	wg2.Wait()
	close(resultCh)

	var results []Result
	for r := range resultCh {
		results = append(results, r)
	}
	return results, nil
}

// urlRe matches http(s) URLs in plain text and Markdown link syntax.
var urlRe = regexp.MustCompile(`https?://[^\s\)\]>"'<]+`)

// infraHosts are domains that host meta-source content rather than job boards.
var infraHosts = []string{
	"github.com", "raw.githubusercontent.com", "gist.githubusercontent.com",
	"gitlab.com", "bitbucket.org",
	"twitter.com", "x.com", "facebook.com", "instagram.com",
	"youtube.com", "youtu.be",
	"shields.io", "img.shields.io",
	"opensource.org", "creativecommons.org",
	"choosealicense.com",
}

// jobBoardKeywords are terms present in job-board hostnames or paths.
var jobBoardKeywords = []string{
	"job", "career", "hire", "hiring", "recruit", "talent", "work",
	"employ", "vacancy", "position", "opening", "remote", "startup",
	"tech", "dev", "engineer", "freelance", "gig", "talent",
}

// extractURLs fetches src and returns candidate job-board URLs found in the page.
// It normalises trailing punctuation stripped from Markdown and keeps the full
// URL (not just the root) so scrapers get the right sub-path if needed.
func (d *Discoverer) extractURLs(src string) ([]string, error) {
	parsed, err := url.Parse(src)
	if err != nil {
		return nil, err
	}
	host := parsed.Hostname()
	if d.blocker.isBlocked(host) {
		return nil, fmt.Errorf("temporarily blocked")
	}

	resp, err := d.client.Get(src)
	if err != nil {
		return nil, err
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, extractDrainLimit)) //nolint:errcheck
		resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusForbidden:
		d.blocker.block(host, time.Now().Add(block403Duration))
		return nil, fmt.Errorf("HTTP 403")
	case http.StatusTooManyRequests:
		dur := retryAfterDuration(resp, block429Duration)
		d.blocker.block(host, time.Now().Add(dur))
		return nil, fmt.Errorf("HTTP 429")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	lr := io.LimitReader(resp.Body, 4*1024*1024)
	scanner := bufio.NewScanner(lr)
	seen := make(map[string]bool)
	var out []string
	for scanner.Scan() {
		for _, raw := range urlRe.FindAllString(scanner.Text(), -1) {
			raw = strings.TrimRight(raw, ".,;:!?)\"'#")
			p, err := url.Parse(raw)
			if err != nil || p.Host == "" {
				continue
			}
			if seen[p.Host] {
				continue
			}
			if !looksLikeJobBoard(p) {
				continue
			}
			seen[p.Host] = true
			// Keep scheme+host+path, strip query/fragment to avoid session noise.
			p.RawQuery = ""
			p.Fragment = ""
			out = append(out, p.String())
		}
	}
	return out, scanner.Err()
}

// looksLikeJobBoard returns true when the URL host or path suggests a job-related site.
func looksLikeJobBoard(u *url.URL) bool {
	host := strings.ToLower(u.Host)
	path := strings.ToLower(u.Path)
	for _, infra := range infraHosts {
		if strings.HasSuffix(host, infra) {
			return false
		}
	}
	combined := host + path
	for _, kw := range jobBoardKeywords {
		if strings.Contains(combined, kw) {
			return true
		}
	}
	return false
}

// headDrainLimit is the maximum bytes we read from a HEAD response body.
// RFC 7231 §4.3.2 forbids servers from sending one, but some do anyway.
// Reading beyond this would waste bandwidth without gaining any information.
const headDrainLimit = 4 * 1024

// getFallbackDrainLimit caps body reads for the GET fallback. We only need
// the status code (already in the headers), so discarding the body early is safe.
const getFallbackDrainLimit = 64 * 1024

// extractDrainLimit caps leftover body draining in extractURLs (post-scan or
// early-return on 403/429 — we never need more than an error page's worth).
const extractDrainLimit = 512 * 1024

// block403Duration is how long a host is skipped after returning HTTP 403.
const block403Duration = 2 * time.Minute

// block429Duration is the fallback backoff for HTTP 429 when no Retry-After
// header is present.
const block429Duration = 60 * time.Second

// maxRetryAfter caps how far into the future a Retry-After header can push the
// block expiry, preventing a single hostile server from blocking us for hours.
const maxRetryAfter = 10 * time.Minute

// tempBlocker tracks per-FQDN temporary blocks with expiry timestamps.
// Unlike the scraper's domainBlocker (which blocks permanently after N failures),
// blocks here expire automatically once the wall-clock deadline passes.
type tempBlocker struct {
	mu      sync.Mutex
	blocked map[string]time.Time // host → unblock time
}

func newTempBlocker() *tempBlocker {
	return &tempBlocker{blocked: make(map[string]time.Time)}
}

// isBlocked returns true if host is still within its block window.
// Expired entries are pruned on access.
func (tb *tempBlocker) isBlocked(host string) bool {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	until, ok := tb.blocked[host]
	if !ok {
		return false
	}
	if time.Now().After(until) {
		delete(tb.blocked, host)
		return false
	}
	return true
}

// block sets or extends a temporary block for host until until.
// An existing block is only replaced if the new deadline is later.
func (tb *tempBlocker) block(host string, until time.Time) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if existing, ok := tb.blocked[host]; ok && existing.After(until) {
		return
	}
	tb.blocked[host] = until
	log.Printf("[discover] skipping %s until %s", host, until.Format("15:04:05"))
}

// retryAfterDuration parses the Retry-After header from resp and returns the
// indicated wait duration, capped at maxRetryAfter. Returns fallback when the
// header is absent or malformed.
func retryAfterDuration(resp *http.Response, fallback time.Duration) time.Duration {
	h := resp.Header.Get("Retry-After")
	if h == "" {
		return fallback
	}
	if n, err := strconv.Atoi(strings.TrimSpace(h)); err == nil && n > 0 {
		d := time.Duration(n) * time.Second
		if d > maxRetryAfter {
			return maxRetryAfter
		}
		return d
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			if d > maxRetryAfter {
				return maxRetryAfter
			}
			return d
		}
	}
	return fallback
}

// isLive sends a HEAD request (falling back to GET on 405) and returns true
// for any sub-500 response, treating redirects as a sign of a live host.
// Body reads are capped: HEAD responses must not carry a body per RFC 7231, but
// some servers send one anyway; reading it without a limit would stall the goroutine.
// 403 and 429 responses record a temporary block and return false immediately.
func (d *Discoverer) isLive(rawURL string) bool {
	ua := "Mozilla/5.0 (compatible; ResumeContactsScraper/0.1)"

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if d.blocker.isBlocked(host) {
		return false
	}

	req, err := http.NewRequest(http.MethodHead, rawURL, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", ua)

	resp, err := d.client.Do(req)
	if err != nil {
		return false
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, headDrainLimit)) //nolint:errcheck
	resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusForbidden:
		d.blocker.block(host, time.Now().Add(block403Duration))
		return false
	case http.StatusTooManyRequests:
		d.blocker.block(host, time.Now().Add(retryAfterDuration(resp, block429Duration)))
		return false
	case http.StatusMethodNotAllowed:
		if d.blocker.isBlocked(host) {
			return false
		}
		req2, err2 := http.NewRequest(http.MethodGet, rawURL, nil)
		if err2 != nil {
			return false
		}
		req2.Header.Set("User-Agent", ua)
		resp2, err2 := d.client.Do(req2)
		if err2 != nil {
			return false
		}
		io.Copy(io.Discard, io.LimitReader(resp2.Body, getFallbackDrainLimit)) //nolint:errcheck
		resp2.Body.Close()
		switch resp2.StatusCode {
		case http.StatusForbidden:
			d.blocker.block(host, time.Now().Add(block403Duration))
			return false
		case http.StatusTooManyRequests:
			d.blocker.block(host, time.Now().Add(retryAfterDuration(resp2, block429Duration)))
			return false
		}
		return resp2.StatusCode < 500
	}

	return resp.StatusCode < 500
}
