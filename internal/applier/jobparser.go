package applier

import (
	"net/url"
	"strings"
	"time"

	"github.com/gocolly/colly/v2"
	"github.com/gocolly/colly/v2/extensions"
)

// JobInfo holds metadata scraped from a single job-posting URL.
type JobInfo struct {
	URL         string
	Title       string
	Company     string
	Description string // full job description text, used for resume tailoring
	// ATSPlatform is one of: greenhouse, lever, ashby, workday, bamboohr, generic.
	ATSPlatform string
}

// ParseJobPage fetches rawURL and extracts the job title and company name.
// The description is not needed here since we are not tailoring; we only need
// the metadata required to drive the form filler and produce readable output.
func ParseJobPage(rawURL string) (*JobInfo, error) {
	info := &JobInfo{URL: rawURL, ATSPlatform: detectPlatform(rawURL)}

	c := colly.NewCollector(colly.MaxDepth(1))
	c.SetRequestTimeout(25 * time.Second)
	extensions.RandomUserAgent(c)

	// Title — ordered from most-specific to most-generic
	c.OnHTML(strings.Join([]string{
		`[data-qa="posting-title"]`,
		".posting-headline h2",
		".job-title",
		".jv-job-detail-header h1",
		"h1",
	}, ", "), func(el *colly.HTMLElement) {
		if info.Title == "" {
			info.Title = strings.TrimSpace(el.Text)
		}
	})

	// Company
	c.OnHTML(strings.Join([]string{
		".company-name",
		`[data-qa="posting-team"]`,
		".posting-categories .sort-by-team",
		"[itemprop=hiringOrganization]",
		".employer-name",
	}, ", "), func(el *colly.HTMLElement) {
		if info.Company == "" {
			info.Company = strings.TrimSpace(el.Text)
		}
	})

	// Description — ordered from most-specific (ATS containers) to full body fallback
	c.OnHTML(strings.Join([]string{
		"#content",                        // Greenhouse
		".posting-description",            // Lever
		".ashby-job-posting-description",  // Ashby
		".job-description",                // Generic ATS
		`[data-qa="job-description"]`,     // Greenhouse (alternate)
		".jv-job-detail-description",      // Jobvite
		".section-wrapper",                // BambooHR
		"article",
		"main",
	}, ", "), func(el *colly.HTMLElement) {
		if info.Description == "" {
			info.Description = strings.TrimSpace(el.Text)
		}
	})

	// Body fallback: grab everything and truncate to avoid token overload
	c.OnHTML("body", func(el *colly.HTMLElement) {
		if info.Description == "" {
			info.Description = truncate(strings.TrimSpace(el.Text), 6000)
		}
	})

	_ = c.Visit(rawURL)

	if info.Company == "" {
		info.Company = companyFromURL(rawURL)
	}
	return info, nil
}

func detectPlatform(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "generic"
	}
	h := strings.ToLower(u.Host)
	switch {
	case strings.Contains(h, "greenhouse.io"):
		return "greenhouse"
	case strings.Contains(h, "lever.co"):
		return "lever"
	case strings.Contains(h, "ashbyhq.com"):
		return "ashby"
	case strings.Contains(h, "myworkdayjobs.com"):
		return "workday"
	case strings.Contains(h, "bamboohr.com"):
		return "bamboohr"
	case strings.Contains(h, "personio.com") || strings.Contains(h, "personio.de"):
		return "personio"
	case strings.Contains(h, "workable.com"):
		return "workable"
	case strings.Contains(h, "recruitee.com"):
		return "recruitee"
	case strings.Contains(h, "icims.com"):
		return "icims"
	case strings.Contains(h, "smartrecruiters.com"):
		return "smartrecruiters"
	case strings.Contains(h, "breezy.hr"):
		return "breezy"
	default:
		return "generic"
	}
}

func truncate(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) > maxRunes {
		return string(r[:maxRunes])
	}
	return s
}

func companyFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) > 0 && parts[0] != "" {
		return parts[0]
	}
	return u.Hostname()
}
