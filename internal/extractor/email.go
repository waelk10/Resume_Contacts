package extractor

import (
	"html"
	"regexp"
	"strings"
)

var emailRe = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)

// skipPrefixes are local-parts that belong to system/role addresses, not people.
var skipPrefixes = []string{
	"noreply", "no-reply", "donotreply", "do-not-reply",
	"admin", "webmaster", "postmaster", "abuse",
	"privacy", "legal", "security", "bounce",
	"mailer-daemon", "unsubscribe", "notifications",
}

// ExtractEmails pulls unique, valid email addresses from raw text or HTML.
// HTML entities (e.g. &#64;) are decoded before matching.
func ExtractEmails(text string) []string {
	text = html.UnescapeString(text)
	raw := emailRe.FindAllString(text, -1)
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		e = strings.ToLower(strings.TrimSpace(e))
		if seen[e] || !isValid(e) {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out
}

// OrgFromEmail derives an organization name from the email domain.
func OrgFromEmail(email string) string {
	parts := strings.SplitN(email, "@", 2)
	if len(parts) != 2 {
		return ""
	}
	seg := strings.Split(parts[1], ".")
	if len(seg) >= 2 {
		name := seg[len(seg)-2]
		if len(name) > 0 {
			return strings.ToUpper(name[:1]) + name[1:]
		}
	}
	return parts[1]
}

func isValid(email string) bool {
	at := strings.Index(email, "@")
	if at < 1 {
		return false
	}
	local := email[:at]
	for _, prefix := range skipPrefixes {
		if local == prefix || strings.HasPrefix(local, prefix) {
			return false
		}
	}
	// Reject bare TLD-only domains (e.g. x@com)
	domain := email[at+1:]
	if !strings.Contains(domain, ".") {
		return false
	}
	return true
}
