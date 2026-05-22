package scraper

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"Resume_Contacts_Scraper/internal/contact"
	"Resume_Contacts_Scraper/internal/extractor"
)

const (
	redditRecheckInterval = 1 * time.Hour
	redditBodyLimit       = 512 * 1024
	// Reddit requires a non-generic User-Agent; requests with the default Go
	// UA are rejected with 429.
	redditUserAgent = "Resume_Contacts_Scraper/1.0"
)

// redditSubreddits lists the subreddits scanned for job-posting emails.
// All three are dedicated hiring boards where post bodies routinely carry
// company contact addresses.
var redditSubreddits = []string{
	"forhire",
	"remotework",
	"hiring",
	"devops_jobs",
	"remotejs",
	"cscareerquestionseu",
}

type redditListing struct {
	Data struct {
		Children []struct {
			Data struct {
				ID       string `json:"id"`
				Title    string `json:"title"`
				Selftext string `json:"selftext"`
			} `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

// runReddit polls each subreddit in redditSubreddits for new posts and emits
// any emails found in post titles and self-text.  It loops forever until ctx
// is cancelled, sleeping redditRecheckInterval between full sweeps.
// redditFetchListing fetches the newest 100 posts from a subreddit and returns
// the decoded listing.  Package-level so both Engine and AppScanner can use it.
func redditFetchListing(ctx context.Context, client *http.Client, sub string) (*redditListing, error) {
	apiURL := fmt.Sprintf("https://www.reddit.com/r/%s/new.json?limit=100", sub)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", redditUserAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("rate-limited (429)")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var listing redditListing
	if err := json.NewDecoder(
		bufio.NewReaderSize(io.LimitReader(resp.Body, redditBodyLimit), 32*1024),
	).Decode(&listing); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &listing, nil
}

func (e *Engine) runReddit(ctx context.Context) {
	client := &http.Client{
		Timeout:   e.cfg.RequestTimeout,
		Transport: newTransport(),
	}
	seen := make(map[string]bool)

	for {
		for _, sub := range redditSubreddits {
			if ctx.Err() != nil {
				return
			}
			if err := e.processRedditSubreddit(ctx, client, sub, seen); err != nil {
				log.Printf("[reddit] r/%s: %v", sub, err)
			}
			// Reddit recommends no more than 1 request/second for crawlers.
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(redditRecheckInterval):
		}
	}
}

func (e *Engine) processRedditSubreddit(ctx context.Context, client *http.Client, sub string, seen map[string]bool) error {
	listing, err := redditFetchListing(ctx, client, sub)
	if err != nil {
		return err
	}

	newCount := 0
	for _, child := range listing.Data.Children {
		p := child.Data
		if seen[p.ID] {
			continue
		}
		seen[p.ID] = true
		newCount++
		src := fmt.Sprintf("https://www.reddit.com/r/%s/comments/%s/", sub, p.ID)
		for _, em := range extractor.ExtractEmails(p.Title + "\n" + p.Selftext) {
			e.on(contact.Contact{
				Email:  em,
				Org:    extractor.OrgFromEmail(em),
				Source: src,
			})
		}
	}
	if newCount > 0 {
		log.Printf("[reddit] r/%s: %d new posts scanned", sub, newCount)
	}
	return nil
}
