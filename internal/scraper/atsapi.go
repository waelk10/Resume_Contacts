package scraper

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
)

// ── Greenhouse ───────────────────────────────────────────────────────────────

type ghJobsResponse struct {
	Jobs []struct {
		AbsoluteURL string `json:"absolute_url"`
	} `json:"jobs"`
}

// fetchGreenhouseJobs queries the public Greenhouse Boards API and returns
// every active job-posting URL for the given board token.
// The API is unauthenticated and rate-limit tolerant for reasonable traffic.
func fetchGreenhouseJobs(ctx context.Context, client *http.Client, token string) []string {
	apiURL := "https://boards-api.greenhouse.io/v1/boards/" + url.PathEscape(token) + "/jobs"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var r ghJobsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*1024*1024)).Decode(&r); err != nil {
		return nil
	}
	out := make([]string, 0, len(r.Jobs))
	for _, j := range r.Jobs {
		if j.AbsoluteURL != "" {
			out = append(out, j.AbsoluteURL)
		}
	}
	return out
}

// extractGreenhouseToken returns the board token from any greenhouse.io URL,
// or "" when the URL is not a Greenhouse board URL.
// Works for boards.greenhouse.io, job-boards.greenhouse.io, and boards.eu.greenhouse.io.
// The token is the first non-empty path segment.
func extractGreenhouseToken(u *url.URL) string {
	if !strings.HasSuffix(strings.ToLower(u.Hostname()), ".greenhouse.io") {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" || strings.ContainsAny(parts[0], ".?#") {
		return ""
	}
	return parts[0]
}

// ── Lever ────────────────────────────────────────────────────────────────────

type leverPosting struct {
	ApplyURL  string `json:"applyUrl"`
	HostedURL string `json:"hostedUrl"`
}

// fetchLeverJobs queries the public Lever Postings API (v0) and returns
// apply-form URLs for every active posting.  When no /apply URL is available
// the hosted job-detail URL is returned instead so the BFS crawler can still
// reach the apply form through its normal HTML crawl.
func fetchLeverJobs(ctx context.Context, client *http.Client, company string) []string {
	apiURL := "https://api.lever.co/v0/postings/" + url.PathEscape(company) + "?mode=json"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var postings []leverPosting
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*1024*1024)).Decode(&postings); err != nil {
		return nil
	}
	out := make([]string, 0, len(postings))
	for _, p := range postings {
		switch {
		case p.ApplyURL != "":
			out = append(out, p.ApplyURL)
		case p.HostedURL != "":
			out = append(out, p.HostedURL)
		}
	}
	return out
}

// extractLeverCompany returns the company slug from a jobs.lever.co URL, or "".
func extractLeverCompany(u *url.URL) string {
	if strings.ToLower(u.Hostname()) != "jobs.lever.co" {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" || strings.ContainsAny(parts[0], ".?#") {
		return ""
	}
	return parts[0]
}

// ── Remotive public API ───────────────────────────────────────────────────────

type remotiveJob struct {
	Description string `json:"description"`
}

// seedFromRemotiveAPI fetches software-dev jobs from the Remotive public API,
// parses each job description for embedded ATS application-page URLs, and
// calls emit for every URL that matches isAppPageURL.
// Remotive's "url" field points to their own listing page; the actual apply
// URL is embedded as a hyperlink inside the HTML description field.
func (s *AppScanner) seedFromRemotiveAPI(ctx context.Context, emit func(string)) {
	client := &http.Client{Timeout: s.cfg.RequestTimeout, Transport: newTransport()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://remotive.com/api/remote-jobs?category=software-dev", nil)
	if err != nil {
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[app/remotive] API error: %v", err)
		return
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[app/remotive] API HTTP %d", resp.StatusCode)
		return
	}
	var result struct {
		Jobs []remotiveJob `json:"jobs"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8*1024*1024)).Decode(&result); err != nil {
		log.Printf("[app/remotive] JSON decode: %v", err)
		return
	}
	count := 0
	for _, job := range result.Jobs {
		for _, u := range extractAppURLsFromText(job.Description) {
			emit(u)
			count++
		}
	}
	if count > 0 {
		log.Printf("[app/remotive] emitted %d application-page URL(s)", count)
	}
}

// ── RemoteOK public API ───────────────────────────────────────────────────────

// seedFromRemoteOKAPI fetches the RemoteOK JSON job feed and calls emit for
// every entry whose apply_url is a recognised application-page URL.
// RemoteOK's feed is an array where the first element is a legal-notice string
// and subsequent elements are job objects.
func (s *AppScanner) seedFromRemoteOKAPI(ctx context.Context, emit func(string)) {
	client := &http.Client{Timeout: s.cfg.RequestTimeout, Transport: newTransport()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://remoteok.com/remote-jobs.json", nil)
	if err != nil {
		return
	}
	// RemoteOK blocks default Go user-agent; use a browser-like string.
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:120.0) Gecko/20100101 Firefox/120.0")
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("[app/remoteok] API error: %v", err)
		return
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[app/remoteok] API HTTP %d", resp.StatusCode)
		return
	}
	// The feed is a heterogeneous JSON array: first element is {"legal":…}, rest are jobs.
	var raw []json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8*1024*1024)).Decode(&raw); err != nil {
		log.Printf("[app/remoteok] JSON decode: %v", err)
		return
	}
	type rokJob struct {
		ApplyURL string `json:"apply_url"`
	}
	count := 0
	for _, msg := range raw {
		var j rokJob
		if err := json.Unmarshal(msg, &j); err != nil || j.ApplyURL == "" {
			continue
		}
		if isAppPageURL(j.ApplyURL) {
			emit(j.ApplyURL)
			count++
		}
	}
	if count > 0 {
		log.Printf("[app/remoteok] emitted %d application-page URL(s)", count)
	}
}
