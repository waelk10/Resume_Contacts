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
	lobstersRecheckInterval = 1 * time.Hour
	lobstersBodyLimit       = 512 * 1024
)

// lobstersStub is the minimal story shape returned by the tag-listing endpoint.
// The full comment tree is absent; fetch the story endpoint separately.
type lobstersStub struct {
	ShortID string `json:"short_id"`
	Title   string `json:"title"`
}

type lobstersStory struct {
	ShortID     string            `json:"short_id"`
	Title       string            `json:"title"`
	Description string            `json:"description"`
	Comments    []lobstersComment `json:"comments"`
}

// lobstersComment mirrors Lobste.rs' nested comment structure: each comment
// may contain its own reply tree in the Comments field.
type lobstersComment struct {
	ShortID  string            `json:"short_id"`
	Comment  string            `json:"comment"`
	Comments []lobstersComment `json:"comments"`
}

// runLobsters polls stories tagged "hiring" on lobste.rs and emits emails
// found in story descriptions and all comments.  Mirrors the HN runner:
// one thread per poll cycle, full comment tree scanned, seen IDs tracked.
func (e *Engine) runLobsters(ctx context.Context) {
	client := &http.Client{
		Timeout:   e.cfg.RequestTimeout,
		Transport: newTransport(),
	}
	seen := make(map[string]bool)

	for {
		if err := e.processLobstersHiring(ctx, client, seen); err != nil {
			log.Printf("[lobsters] %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(lobstersRecheckInterval):
		}
	}
}

func (e *Engine) processLobstersHiring(ctx context.Context, client *http.Client, seen map[string]bool) error {
	stubs, err := lobstersFetchTag(ctx, client, "hiring")
	if err != nil {
		return err
	}

	newCount := 0
	for _, stub := range stubs {
		if seen[stub.ShortID] {
			continue
		}
		seen[stub.ShortID] = true
		newCount++

		story, err := lobstersFetchStory(ctx, client, stub.ShortID)
		if err != nil {
			log.Printf("[lobsters] %s: %v", stub.ShortID, err)
			continue
		}

		storyURL := "https://lobste.rs/s/" + stub.ShortID
		for _, em := range extractor.ExtractEmails(story.Description) {
			e.on(contact.Contact{
				Email:  em,
				Org:    extractor.OrgFromEmail(em),
				Source: storyURL,
			})
		}
		e.emitLobstersComments(story.Comments, storyURL)

		// Polite delay between story fetches.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(e.cfg.Delay):
		}
	}
	if newCount > 0 {
		log.Printf("[lobsters] processed %d new hiring stories", newCount)
	}
	return nil
}

// emitLobstersComments recursively walks the nested comment tree and emits
// any emails found in each comment body.
func (e *Engine) emitLobstersComments(comments []lobstersComment, src string) {
	for _, c := range comments {
		for _, em := range extractor.ExtractEmails(c.Comment) {
			e.on(contact.Contact{
				Email:  em,
				Org:    extractor.OrgFromEmail(em),
				Source: src,
			})
		}
		if len(c.Comments) > 0 {
			e.emitLobstersComments(c.Comments, src)
		}
	}
}

func lobstersFetchTag(ctx context.Context, client *http.Client, tag string) ([]lobstersStub, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://lobste.rs/t/"+tag+".json", nil)
	if err != nil {
		return nil, err
	}
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
	var stubs []lobstersStub
	if err := json.NewDecoder(
		bufio.NewReaderSize(io.LimitReader(resp.Body, lobstersBodyLimit), 32*1024),
	).Decode(&stubs); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return stubs, nil
}

func lobstersFetchStory(ctx context.Context, client *http.Client, shortID string) (*lobstersStory, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://lobste.rs/s/"+shortID+".json", nil)
	if err != nil {
		return nil, err
	}
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
	var story lobstersStory
	if err := json.NewDecoder(
		bufio.NewReaderSize(io.LimitReader(resp.Body, lobstersBodyLimit), 32*1024),
	).Decode(&story); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &story, nil
}
