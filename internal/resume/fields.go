package resume

import (
	"regexp"
	"strings"
)

// ParsedFields holds address, link, and education data auto-extracted from a resume.
type ParsedFields struct {
	City    string
	State   string // 2-letter code or full name, as it appears in the CV
	ZipCode string
	Country string
	Website string // personal site / portfolio (not LinkedIn/GitHub/Twitter)
	GitHub  string // full github.com profile URL

	// Education — from the first/highest degree entry in the CV.
	School       string // e.g. "Massachusetts Institute of Technology"
	Degree       string // normalised: "bachelor", "master", "phd", "associate"
	FieldOfStudy string // e.g. "Computer Science"
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

	// Education: degree-level keyword anywhere on a line.
	reDegreeKeyword = regexp.MustCompile(
		`(?i)\b(ph\.?d\.?|d\.?phil\.?|doctoral|doctor\s+of\b|` +
			`m\.?b\.?a\.?|master(?:'s)?(?:\s+of\s+|\s+in\s+)?|m\.s\.?|m\.a\.?|m\.sc\.?|m\.eng\.?|` +
			`bachelor(?:'s)?(?:\s+of\s+|\s+in\s+)?|b\.s\.?|b\.a\.?|b\.sc\.?|b\.eng\.?|` +
			`associate(?:'s)?|a\.a\.?|a\.s\.?)\b`,
	)
	// "in Computer Science" / "of Computer Engineering" — field of study after degree.
	// Character class includes "/" so "Computer Science / Mathematics" is captured whole.
	reFieldOfStudy = regexp.MustCompile(
		`(?i)\b(?:in|of)\s+([A-Z][A-Za-z\s&,()/-]{2,50?})(?:\s*[,;|(]|\s*[-–]\s*\d|\s*$)`,
	)
	// Institution name: a run of title-case words ending in a school-type noun.
	reInstitution = regexp.MustCompile(
		`(?i)\b((?:[A-Z][A-Za-z'-]+\s+){0,6}(?:University|College|` +
			`Institute(?:\s+of\s+Technology)?|Polytechnic|` +
			`Technion|Technische\s+Universit[äa]t|` +
			`School(?:\s+of\s+[A-Za-z\s]+)?))\b`,
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

	parseEducation(text, &f)

	return f
}

// parseEducation extracts the highest / first degree entry from the CV text
// and populates f.School, f.Degree, and f.FieldOfStudy.  Heuristics prefer
// the education section header when found; fall back to full-text scan.
func parseEducation(text string, f *ParsedFields) {
	lines := strings.Split(text, "\n")

	// Try to find the EDUCATION section; if found, search only that block.
	start := 0
	end := len(lines)
	for i, line := range lines {
		up := strings.ToUpper(strings.TrimSpace(line))
		if up == "EDUCATION" || up == "EDUCATION:" ||
			up == "ACADEMIC BACKGROUND" || up == "ACADEMIC HISTORY" ||
			strings.HasPrefix(up, "EDUCATION ") {
			start = i + 1
			// Find the next section header (all-caps short line after a gap).
			for j := start + 1; j < len(lines); j++ {
				t := strings.TrimSpace(lines[j])
				if len(t) < 4 || len(t) > 50 {
					continue
				}
				if t == strings.ToUpper(t) && strings.ToUpper(t) != strings.ToLower(t) {
					end = j
					break
				}
			}
			break
		}
	}
	searchLines := lines[start:end]

	normDegree := func(s string) string {
		low := strings.ToLower(s)
		switch {
		case strings.Contains(low, "ph.d") || strings.Contains(low, "phd") ||
			strings.Contains(low, "d.phil") || strings.Contains(low, "doctoral") ||
			strings.HasPrefix(low, "doctor of"):
			return "phd"
		case strings.Contains(low, "mba") || strings.Contains(low, "m.b.a"):
			return "master" // MBA maps to Master's in most ATS degree lists
		case strings.Contains(low, "master") || strings.Contains(low, "m.s") ||
			strings.Contains(low, "m.a") || strings.Contains(low, "m.sc") || strings.Contains(low, "m.eng"):
			return "master"
		case strings.Contains(low, "bachelor") || strings.Contains(low, "b.s") ||
			strings.Contains(low, "b.a") || strings.Contains(low, "b.sc") || strings.Contains(low, "b.eng"):
			return "bachelor"
		case strings.Contains(low, "associate") || strings.Contains(low, "a.a") || strings.Contains(low, "a.s."):
			return "associate"
		}
		return ""
	}

	for i, line := range searchLines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		// Degree detection on this line.
		if f.Degree == "" && reDegreeKeyword.MatchString(trimmed) {
			f.Degree = normDegree(strings.ToLower(trimmed))

			// Field of study: look for "in X" or "of X" on the same line.
			if f.FieldOfStudy == "" {
				if m := reFieldOfStudy.FindStringSubmatch(trimmed); m != nil {
					raw := strings.TrimSpace(m[1])
					// Trim trailing year or noise.
					raw = strings.TrimRight(raw, " ,;|")
					if len(raw) > 2 && len(raw) < 60 {
						f.FieldOfStudy = raw
					}
				}
			}
		}

		// Institution detection on this line or the next few.
		if f.School == "" {
			if m := reInstitution.FindString(trimmed); m != "" {
				f.School = strings.TrimSpace(m)
			} else if i+1 < len(searchLines) {
				// Try the next line (some CVs put institution on its own line).
				if m2 := reInstitution.FindString(strings.TrimSpace(searchLines[i+1])); m2 != "" {
					f.School = strings.TrimSpace(m2)
				}
			}
		}

		// Stop once we have all three fields.
		if f.Degree != "" && f.School != "" && f.FieldOfStudy != "" {
			break
		}
	}
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
