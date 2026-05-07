package extractor

import (
	"html"
	"regexp"
	"strings"
	"sync"
	"time"

	emailverifier "github.com/AfterShip/email-verifier"
)

var emailRe = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)

// skipPrefixes are local-parts that belong to system/role addresses, not people.
// They are checked before any network call as a fast pre-filter.
var skipPrefixes = []string{
	"noreply", "no-reply", "donotreply", "do-not-reply",
	"admin", "webmaster", "postmaster", "abuse",
	"privacy", "legal", "security", "bounce",
	"mailer-daemon", "unsubscribe", "notifications",
}

var (
	gVerifier    *emailverifier.Verifier
	gVerifierMu  sync.RWMutex
	gSMTPEnabled bool
)

func init() {
	gVerifier = newVerifier(false)
}

func newVerifier(smtp bool) *emailverifier.Verifier {
	v := emailverifier.NewVerifier().
		DisableGravatarCheck().
		DisableDomainSuggest().
		DisableAutoUpdateDisposable().
		ConnectTimeout(5 * time.Second).
		OperationTimeout(7 * time.Second)
	if smtp {
		return v.EnableSMTPCheck()
	}
	return v.DisableSMTPCheck()
}

// EnableSMTP switches on SMTP verification for subsequent ExtractEmails calls.
// Call once at startup, before scraper goroutines begin.
func EnableSMTP() {
	gVerifierMu.Lock()
	defer gVerifierMu.Unlock()
	gVerifier = newVerifier(true)
	gSMTPEnabled = true
}

// ExtractEmails pulls unique, valid email addresses from raw text or HTML.
// HTML entities (e.g. &#64;) are decoded before matching.
// Each address is validated for syntax, MX records, disposability, and role
// account status; when --smtp-verify is active, SMTP deliverability is checked too.
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
	// Fast pre-filter: skip known system/role local-parts without any network call.
	local := email[:at]
	for _, prefix := range skipPrefixes {
		if local == prefix || strings.HasPrefix(local, prefix) {
			return false
		}
	}
	// Reject bare TLD-only domains (e.g. x@com).
	if !strings.Contains(email[at+1:], ".") {
		return false
	}

	gVerifierMu.RLock()
	v := gVerifier
	smtpOn := gSMTPEnabled
	gVerifierMu.RUnlock()

	result, err := v.Verify(email)
	if err != nil {
		return false
	}
	if !result.Syntax.Valid || !result.HasMxRecords || result.Disposable || result.RoleAccount {
		return false
	}
	// When SMTP is active, reject addresses the server explicitly rejects
	// (deliverable=false AND not catch-all).  Treat timeouts/unknowns as passing.
	if smtpOn && result.SMTP != nil && !result.SMTP.Deliverable && !result.SMTP.CatchAll {
		return false
	}
	return true
}
