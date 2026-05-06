package sources

import (
	"bufio"
	"context"
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
	Countries      []string // ISO 3166-1 alpha-2 codes / region aliases; nil = all
	MaxHops        int      // extra BFS hops beyond the initial meta-source fetch (0 = single-pass)
	// Jitter is the upper bound of a random pre-request delay added to each
	// liveness probe. Zero disables jitter.
	Jitter time.Duration
}

// ResolvedMetaSources returns the union of the default meta-sources and any
// country-specific ones implied by cfg.Countries.  Use this instead of
// cfg.MetaSources when you need the full list the discoverer will fetch.
func (cfg Config) ResolvedMetaSources() []string {
	if len(cfg.Countries) == 0 {
		return cfg.MetaSources
	}
	filter := expandFilterCodes(cfg.Countries)
	seen := make(map[string]bool, len(cfg.MetaSources)+16)
	out := make([]string, 0, len(cfg.MetaSources)+16)
	for _, s := range cfg.MetaSources {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for code := range filter {
		for _, s := range countryMetaSources[code] {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}

// euCountryCodes are all European country codes that the "eu" alias expands to.
var euCountryCodes = []string{
	"de", "at", "ch", "nl", "be", "lu", "fr",
	"es", "pt", "it", "gr", "mt",
	"dk", "se", "no", "fi", "is",
	"pl", "cz", "hu", "ro", "bg", "hr", "si", "sk",
	"gb", "ie", "eu",
}

// expandFilterCodes converts user-supplied country codes (which may include
// region aliases) into a flat set of canonical codes.
func expandFilterCodes(codes []string) map[string]bool {
	out := make(map[string]bool, len(codes)*4)
	for _, code := range codes {
		switch code {
		case "eu":
			for _, c := range euCountryCodes {
				out[c] = true
			}
		case "dach":
			out["de"], out["at"], out["ch"] = true, true, true
		case "benelux":
			out["nl"], out["be"], out["lu"] = true, true, true
		case "nordics":
			out["dk"], out["se"], out["no"], out["fi"], out["is"] = true, true, true, true, true
		case "cee":
			out["pl"], out["cz"], out["hu"], out["ro"] = true, true, true, true
			out["bg"], out["hr"], out["si"], out["sk"] = true, true, true, true
		case "southern":
			out["es"], out["pt"], out["it"], out["gr"], out["mt"] = true, true, true, true, true
		default:
			out[code] = true
		}
	}
	return out
}

// countryTLDs maps each country code to the ccTLDs that identify URLs as belonging
// to that country.  Used for filtering discovered results when Countries is set.
var countryTLDs = map[string][]string{
	"de": {".de"},
	"at": {".at"},
	"ch": {".ch"},
	"nl": {".nl"},
	"be": {".be"},
	"lu": {".lu"},
	"fr": {".fr"},
	"es": {".es"},
	"pt": {".pt"},
	"it": {".it"},
	"gr": {".gr"},
	"mt": {".mt"},
	"gb": {".co.uk", ".org.uk", ".me.uk", ".net.uk", ".uk"},
	"ie": {".ie"},
	"dk": {".dk"},
	"se": {".se"},
	"no": {".no"},
	"fi": {".fi"},
	"is": {".is"},
	"pl": {".pl"},
	"cz": {".cz"},
	"hu": {".hu"},
	"ro": {".ro"},
	"bg": {".bg"},
	"hr": {".hr"},
	"si": {".si"},
	"sk": {".sk"},
	"eu": {".eu"},
}

// urlMatchesCountryFilter reports whether rawURL should be included given filter.
// A URL is included when its ccTLD belongs to a requested country code, or when
// its domain has no recognisable ccTLD and "global" is in the filter.
func urlMatchesCountryFilter(rawURL string, filter map[string]bool) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())

	for country, tlds := range countryTLDs {
		if !filter[country] {
			continue
		}
		for _, tld := range tlds {
			if strings.HasSuffix(host, tld) {
				return true
			}
		}
	}

	// Domain has no recognised ccTLD — treat as global.
	if filter["global"] {
		for _, tlds := range countryTLDs {
			for _, tld := range tlds {
				if strings.HasSuffix(host, tld) {
					return false // it IS a ccTLD, just not one we want
				}
			}
		}
		return true
	}
	return false
}

// countryMetaSources maps each country code to pages that enumerate job boards
// focused on that country.  Appended to DefaultMetaSources when Countries is set.
// GitHub topic pages are used because they surface repos tagged with the topic,
// some of which are country-specific job boards or curated lists of them.
var countryMetaSources = map[string][]string{
	"de": {
		"https://github.com/topics/job-board-germany",
		"https://github.com/topics/jobs-germany",
		"https://github.com/topics/german-jobs",
		"https://github.com/topics/german-tech-jobs",
	},
	"at": {
		"https://github.com/topics/jobs-austria",
		"https://github.com/topics/austrian-tech",
	},
	"ch": {
		"https://github.com/topics/swiss-tech-jobs",
		"https://github.com/topics/jobs-switzerland",
	},
	"gb": {
		"https://github.com/topics/uk-tech-jobs",
		"https://github.com/topics/uk-jobs",
		"https://github.com/topics/london-tech-jobs",
	},
	"ie": {
		"https://github.com/topics/ireland-tech",
		"https://github.com/topics/jobs-ireland",
	},
	"fr": {
		"https://github.com/topics/french-tech",
		"https://github.com/topics/french-jobs",
		"https://github.com/topics/emploi-tech",
	},
	"nl": {
		"https://github.com/topics/dutch-tech",
		"https://github.com/topics/jobs-netherlands",
		"https://github.com/topics/amsterdam-tech",
	},
	"be": {
		"https://github.com/topics/belgium-tech",
		"https://github.com/topics/jobs-belgium",
	},
	"lu": {
		"https://github.com/topics/luxembourg-jobs",
	},
	"se": {
		"https://github.com/topics/sweden-tech",
		"https://github.com/topics/swedish-jobs",
		"https://github.com/topics/stockholm-tech",
	},
	"no": {
		"https://github.com/topics/norway-tech",
		"https://github.com/topics/norwegian-jobs",
	},
	"dk": {
		"https://github.com/topics/denmark-tech",
		"https://github.com/topics/danish-jobs",
		"https://github.com/topics/copenhagen-tech",
	},
	"fi": {
		"https://github.com/topics/finland-tech",
		"https://github.com/topics/finnish-jobs",
	},
	"is": {
		"https://github.com/topics/iceland-tech",
	},
	"es": {
		"https://github.com/topics/spain-tech",
		"https://github.com/topics/spanish-jobs",
		"https://github.com/topics/barcelona-tech",
		"https://github.com/topics/madrid-tech",
	},
	"pt": {
		"https://github.com/topics/portugal-tech",
		"https://github.com/topics/lisbon-tech",
	},
	"it": {
		"https://github.com/topics/italy-tech",
		"https://github.com/topics/italian-jobs",
		"https://github.com/topics/milan-tech",
	},
	"gr": {
		"https://github.com/topics/greece-tech",
	},
	"mt": {
		"https://github.com/topics/malta-tech",
	},
	"pl": {
		"https://github.com/topics/polish-developer-jobs",
		"https://github.com/topics/praca-programista",
		"https://github.com/topics/poland-tech",
	},
	"cz": {
		"https://github.com/topics/czech-tech",
		"https://github.com/topics/prague-tech",
	},
	"hu": {
		"https://github.com/topics/hungary-tech",
		"https://github.com/topics/budapest-tech",
	},
	"ro": {
		"https://github.com/topics/romania-tech",
	},
	"bg": {
		"https://github.com/topics/bulgaria-tech",
		"https://github.com/topics/sofia-tech",
	},
	"hr": {
		"https://github.com/topics/croatia-tech",
	},
	"si": {
		"https://github.com/topics/slovenia-tech",
	},
	"sk": {
		"https://github.com/topics/slovakia-tech",
		"https://github.com/topics/bratislava-tech",
	},
	"eu": {
		"https://github.com/topics/european-tech",
		"https://github.com/topics/eu-tech-jobs",
		"https://github.com/topics/europe-jobs",
		"https://github.com/topics/european-startups",
	},
}

// DefaultConfig is a ready-to-use configuration.
var DefaultConfig = Config{
	Concurrency:    6,
	RequestTimeout: 20 * time.Second,
	MetaSources:    DefaultMetaSources,
	MaxHops:        2,
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

// Run performs a multi-hop BFS over meta-source pages to discover job-board URLs.
//
// Hop 0  – fetch ResolvedMetaSources(), collect board candidates and newly
//           discovered curated-list pages (GitHub repos, awesome-* pages, …).
// Hop 1…N – fetch those newly discovered pages, repeat.
//
// After all hops the full candidate pool is deduplicated, liveness-validated,
// and country-filtered before being returned.
//
// If ctx is cancelled before the run completes, Run returns whatever results
// were collected up to that point along with ctx.Err(), allowing callers to
// save a partial result set rather than discarding all progress.
func (d *Discoverer) Run(ctx context.Context, existing []string) ([]Result, error) {
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

	// visitedMeta prevents fetching the same meta-source page twice across hops.
	visitedMeta := make(map[string]bool)

	// Seed the first frontier from the resolved meta-source list.
	frontier := d.cfg.ResolvedMetaSources()
	rand.Shuffle(len(frontier), func(i, j int) { frontier[i], frontier[j] = frontier[j], frontier[i] })
	for _, s := range frontier {
		visitedMeta[s] = true
	}

	maxHops := d.cfg.MaxHops
	if maxHops < 0 {
		maxHops = 0
	}

	var (
		mu         sync.Mutex
		candidates []candidate
	)

	for hop := 0; len(frontier) > 0; hop++ {
		var nextFrontier []string

		sem := make(chan struct{}, d.cfg.Concurrency)
		var wg sync.WaitGroup
	hopLoop:
		for _, src := range frontier {
			if ctx.Err() != nil {
				break
			}
			wg.Add(1)
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				wg.Done()
				break hopLoop
			}
			go func(src string) {
				defer wg.Done()
				defer func() { <-sem }()
				boards, metas, err := d.extractURLs(ctx, src)
				if err != nil {
					log.Printf("[discover] hop=%d %s: %v", hop, src, err)
					return
				}
				log.Printf("[discover] hop=%d %s: %d boards, %d meta-sources", hop, src, len(boards), len(metas))
				mu.Lock()
				for _, u := range boards {
					candidates = append(candidates, candidate{rawURL: u, source: src})
				}
				// Only enqueue new meta-sources when we have hops left.
				if hop < maxHops {
					for _, m := range metas {
						if !visitedMeta[m] {
							visitedMeta[m] = true
							nextFrontier = append(nextFrontier, m)
						}
					}
				}
				mu.Unlock()
			}(src)
		}
		wg.Wait()

		if ctx.Err() != nil || hop >= maxHops {
			break
		}
		rand.Shuffle(len(nextFrontier), func(i, j int) { nextFrontier[i], nextFrontier[j] = nextFrontier[j], nextFrontier[i] })
		frontier = nextFrontier
	}

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

	log.Printf("[discover] %d unique candidates after dedup (%d already known, %d meta-source pages visited)",
		len(unique), len(existingHosts), len(visitedMeta))

	// Shuffle unique items so validation probes hit different hosts in different
	// orders on each run, reducing predictable burst patterns per domain.
	rand.Shuffle(len(unique), func(i, j int) { unique[i], unique[j] = unique[j], unique[i] })

	filter := expandFilterCodes(d.cfg.Countries)

	// partialResults returns the deduplicated (but unvalidated) unique set with
	// the country filter applied. Called on cancellation so the caller always
	// gets the candidates found so far rather than an empty slice.
	partialResults := func() []Result {
		var out []Result
		for _, u := range unique {
			if len(filter) == 0 || urlMatchesCountryFilter(u.rawURL, filter) {
				out = append(out, Result{URL: u.rawURL, Source: u.source})
			}
		}
		return out
	}

	// If ctx was cancelled during the BFS phase, skip liveness probing entirely:
	// every HTTP probe would fail immediately with context.Canceled, giving an
	// empty result set. Return the unvalidated candidates instead.
	if ctx.Err() != nil {
		return partialResults(), ctx.Err()
	}

	// Validate each candidate — only keep reachable hosts.
	resultCh := make(chan Result, len(unique))
	sem2 := make(chan struct{}, d.cfg.Concurrency)
	var wg2 sync.WaitGroup
validLoop:
	for _, w := range unique {
		if ctx.Err() != nil {
			break
		}
		wg2.Add(1)
		select {
		case sem2 <- struct{}{}:
		case <-ctx.Done():
			wg2.Done()
			break validLoop
		}
		go func(w workItem) {
			defer wg2.Done()
			defer func() { <-sem2 }()
			if d.cfg.Jitter > 0 {
				select {
				case <-time.After(time.Duration(rand.Int63n(int64(d.cfg.Jitter)))):
				case <-ctx.Done():
					return
				}
			}
			if d.isLive(ctx, w.rawURL) {
				resultCh <- Result{URL: w.rawURL, Source: w.source}
			}
		}(w)
	}
	wg2.Wait()
	close(resultCh)

	var results []Result
	for r := range resultCh {
		if len(filter) == 0 || urlMatchesCountryFilter(r.URL, filter) {
			results = append(results, r)
		}
	}

	// If cancellation happened mid-validation, all in-flight probes were aborted
	// by the cancelled context and returned false, leaving results empty or sparse.
	// Fall back to the full unvalidated unique set so the caller gets useful output.
	if ctx.Err() != nil {
		return partialResults(), ctx.Err()
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

// extractURLs fetches src and returns:
//   - boards: candidate job-board URLs found in the page
//   - metas:  curated-list pages found in the page that may themselves enumerate
//             more job boards (GitHub repos, awesome-* pages, etc.)
func (d *Discoverer) extractURLs(ctx context.Context, src string) (boards, metas []string, err error) {
	parsed, err := url.Parse(src)
	if err != nil {
		return nil, nil, err
	}
	host := parsed.Hostname()
	if d.blocker.isBlocked(host) {
		return nil, nil, fmt.Errorf("temporarily blocked")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		io.Copy(io.Discard, io.LimitReader(resp.Body, extractDrainLimit)) //nolint:errcheck
		resp.Body.Close()
	}()

	switch resp.StatusCode {
	case http.StatusForbidden:
		d.blocker.block(host, time.Now().Add(block403Duration))
		return nil, nil, fmt.Errorf("HTTP 403")
	case http.StatusTooManyRequests:
		dur := retryAfterDuration(resp, block429Duration)
		d.blocker.block(host, time.Now().Add(dur))
		return nil, nil, fmt.Errorf("HTTP 429")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	lr := io.LimitReader(resp.Body, 4*1024*1024)
	scanner := bufio.NewScanner(lr)
	seenBoards := make(map[string]bool)
	seenMetas := make(map[string]bool)
	for scanner.Scan() {
		for _, raw := range urlRe.FindAllString(scanner.Text(), -1) {
			raw = strings.TrimRight(raw, ".,;:!?)\"'#")
			p, err := url.Parse(raw)
			if err != nil || p.Host == "" {
				continue
			}
			// Strip query/fragment to normalise and avoid session noise.
			p.RawQuery = ""
			p.Fragment = ""
			normalised := p.String()

			if looksLikeJobBoard(p) && !seenBoards[p.Host] {
				seenBoards[p.Host] = true
				boards = append(boards, normalised)
			}
			if looksLikeCuratedList(p) && !seenMetas[normalised] {
				seenMetas[normalised] = true
				metas = append(metas, normalised)
			}
		}
	}
	return boards, metas, scanner.Err()
}

// curatedListKeywords are terms that appear in repo names or page paths that
// suggest the page is a curated index of job boards or remote-work resources.
var curatedListKeywords = []string{
	"awesome", "curated", "job-board", "jobboard", "remote-job", "remotejob",
	"hiring", "job-list", "career-resource", "work-resource",
}

// looksLikeCuratedList returns true for pages that are likely to enumerate job
// boards, making them good candidates for recursive BFS meta-source following.
// It deliberately does NOT overlap with looksLikeJobBoard — its job is to find
// pages that LINK TO job boards, not boards themselves.
func looksLikeCuratedList(u *url.URL) bool {
	host := strings.ToLower(u.Host)
	path := strings.ToLower(u.Path)
	parts := strings.Split(strings.Trim(path, "/"), "/")

	switch host {
	case "raw.githubusercontent.com":
		// Raw GitHub file — always worth following (likely a README of a list repo).
		return true

	case "github.com":
		switch {
		case len(parts) >= 2 && parts[0] == "topics":
			// github.com/topics/XXX — topic index page.
			return true
		case len(parts) == 2:
			// github.com/user/repo — follow only if repo name suggests curation.
			repoName := parts[1]
			for _, kw := range curatedListKeywords {
				if strings.Contains(repoName, kw) {
					return true
				}
			}
			// Also accept repos whose name contains job-related words even without
			// explicit curation keywords (e.g. "remote-jobs", "tech-jobs-europe").
			for _, kw := range []string{"job", "remote", "career", "work", "hire", "employ"} {
				if strings.Contains(repoName, kw) {
					return true
				}
			}
		case len(parts) >= 4 && parts[2] == "blob":
			// github.com/user/repo/blob/branch/file — rendered file view.
			return true
		}

	case "gitlab.com":
		if len(parts) == 2 {
			repoName := parts[1]
			for _, kw := range curatedListKeywords {
				if strings.Contains(repoName, kw) {
					return true
				}
			}
		}
	}

	// Generic fallback: page path contains a strong curation keyword.
	combined := host + path
	for _, kw := range curatedListKeywords {
		if strings.Contains(combined, kw) {
			return true
		}
	}
	return false
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
func (d *Discoverer) isLive(ctx context.Context, rawURL string) bool {
	ua := "Mozilla/5.0 (compatible; ResumeContactsScraper/0.1)"

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	if d.blocker.isBlocked(host) {
		return false
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
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
		req2, err2 := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
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
