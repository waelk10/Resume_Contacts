package resume

import (
	"regexp"
	"strings"
)

// ParsedFields holds address and link data auto-extracted from a resume.
type ParsedFields struct {
	City    string
	State   string // 2-letter code or full name, as it appears in the CV
	ZipCode string
	Country string
	Website string // personal site / portfolio (not LinkedIn/GitHub/Twitter)
	GitHub  string // full github.com profile URL
}

var (
	// "Austin, TX 78701" or "San Francisco, CA 94105"
	reCityStateZip = regexp.MustCompile(
		`\b([A-Z][A-Za-z .'\-]{1,24}),\s*([A-Z]{2,3})\s+(\d{4,10})\b`,
	)
	// "Austin, TX" without zip
	reCityState = regexp.MustCompile(
		`\b([A-Z][A-Za-z .'\-]{1,24}),\s*([A-Z]{2,3})\b`,
	)
	reGitHub = regexp.MustCompile(
		`(?i)(?:https?://)?github\.com/([A-Za-z0-9][A-Za-z0-9\-]{0,38})`,
	)
	reURL     = regexp.MustCompile(`https?://[^\s<>"'\)\]]+`)
	reCountry = regexp.MustCompile(
		`(?i)\b(United\s+States(?:\s+of\s+America)?|U\.?S\.?A?\.?|United\s+Kingdom|U\.?K\.?|Canada|Australia|Germany|France|Netherlands|India|Singapore|Ireland|Sweden|Denmark|Norway|Finland|New\s+Zealand)\b`,
	)
)

var socialDomains = []string{
	"linkedin.com", "github.com", "twitter.com", "x.com",
	"facebook.com", "instagram.com", "youtube.com",
}

// ParseFields extracts commonly needed form fields from plain-text resume
// content.  Extraction is heuristic; callers must always prefer explicit
// user-supplied values over anything returned here.
func ParseFields(text string) ParsedFields {
	var f ParsedFields
	lines := strings.Split(text, "\n")

	// Address: scan only the first ~35 lines (header/contact block).
	for i, line := range lines {
		if i >= 35 {
			break
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if m := reCityStateZip.FindStringSubmatch(trimmed); m != nil {
			f.City = strings.TrimSpace(m[1])
			f.State = strings.TrimSpace(m[2])
			f.ZipCode = strings.TrimSpace(m[3])
			break
		}
		if f.City == "" {
			if m := reCityState.FindStringSubmatch(trimmed); m != nil {
				f.City = strings.TrimSpace(m[1])
				f.State = strings.TrimSpace(m[2])
			}
		}
	}

	// Country: look in the first 35 lines.
	for i, line := range lines {
		if i >= 35 {
			break
		}
		if m := reCountry.FindString(line); m != "" {
			f.Country = normaliseCountry(m)
			break
		}
	}

	// GitHub: scan the whole text, strip to profile root URL.
	if m := reGitHub.FindStringSubmatch(text); m != nil {
		f.GitHub = "https://github.com/" + m[1]
	}

	// Personal website: first URL that is not a known social/professional domain.
	for _, u := range reURL.FindAllString(text, -1) {
		lower := strings.ToLower(u)
		isSocial := false
		for _, d := range socialDomains {
			if strings.Contains(lower, d) {
				isSocial = true
				break
			}
		}
		if !isSocial {
			// Strip trailing punctuation that may have been captured.
			f.Website = strings.TrimRight(u, ".,;)")
			break
		}
	}

	return f
}

func normaliseCountry(s string) string {
	up := strings.ToUpper(strings.TrimSpace(
		strings.ReplaceAll(strings.ReplaceAll(s, ".", ""), " ", " "),
	))
	switch {
	case strings.Contains(up, "UNITED STATES") || up == "US" || up == "USA" || up == "U S A":
		return "United States"
	case strings.Contains(up, "UNITED KINGDOM") || up == "UK" || up == "U K":
		return "United Kingdom"
	case up == "CANADA" || up == "CA":
		return "Canada"
	case up == "AUSTRALIA" || up == "AU":
		return "Australia"
	default:
		// Title-case the raw value.
		words := strings.Fields(s)
		for i, w := range words {
			if len(w) > 0 {
				words[i] = strings.ToUpper(w[:1]) + strings.ToLower(w[1:])
			}
		}
		return strings.Join(words, " ")
	}
}
