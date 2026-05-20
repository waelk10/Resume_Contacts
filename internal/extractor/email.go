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
	// Non-hiring departments — unlikely to be useful contacts.
	"sales", "marketing", "press", "media", "billing",
	"invoice", "orders", "shipping", "newsletter", "social",
	"blog", "events", "conference", "investor", "ir",
	"support", "help", "helpdesk", "feedback", "service",
}

// personalDomains is an explicit blocklist of consumer / free-email provider
// domains.  This supplements the email-verifier library's result.Free flag,
// which may not cover all regional providers.  Checked before any network call.
var personalDomains = map[string]struct{}{
	// Google
	"gmail.com": {}, "googlemail.com": {},
	// Yahoo
	"yahoo.com": {}, "yahoo.co.uk": {}, "yahoo.co.in": {}, "yahoo.co.jp": {},
	"yahoo.ca": {}, "yahoo.com.au": {}, "yahoo.fr": {}, "yahoo.de": {},
	"yahoo.es": {}, "yahoo.it": {}, "yahoo.com.br": {}, "ymail.com": {},
	// Microsoft / Outlook / Hotmail
	"hotmail.com": {}, "hotmail.co.uk": {}, "hotmail.fr": {}, "hotmail.de": {},
	"hotmail.it": {}, "hotmail.es": {}, "hotmail.com.au": {}, "hotmail.com.br": {},
	"outlook.com": {}, "live.com": {}, "live.co.uk": {}, "live.fr": {},
	"live.de": {}, "live.com.au": {}, "live.ca": {}, "msn.com": {},
	// Apple
	"icloud.com": {}, "me.com": {}, "mac.com": {},
	// AOL / Verizon
	"aol.com": {}, "aim.com": {}, "verizon.net": {}, "att.net": {},
	"comcast.net": {}, "sbcglobal.net": {}, "cox.net": {}, "charter.net": {},
	"earthlink.net": {}, "bellsouth.net": {}, "optonline.net": {},
	// Privacy / encrypted
	"protonmail.com": {}, "protonmail.ch": {}, "pm.me": {}, "proton.me": {},
	"tutanota.com": {}, "tuta.io": {}, "tutamail.com": {},
	// European
	"gmx.com": {}, "gmx.de": {}, "gmx.at": {}, "gmx.net": {}, "gmx.ch": {},
	"gmx.co.uk": {}, "web.de": {}, "t-online.de": {}, "freenet.de": {},
	"mail.de": {}, "arcor.de": {}, "o2online.de": {},
	"laposte.net": {}, "orange.fr": {}, "wanadoo.fr": {}, "free.fr": {},
	"sfr.fr": {}, "bbox.fr": {}, "numericable.fr": {},
	"tiscali.it": {}, "libero.it": {}, "virgilio.it": {},
	"terra.es": {}, "euskalnet.net": {},
	// Russian / Eastern European
	"yandex.com": {}, "yandex.ru": {}, "ya.ru": {}, "yandex.ua": {},
	"mail.ru": {}, "bk.ru": {}, "list.ru": {}, "inbox.ru": {},
	"rambler.ru": {}, "ukr.net": {}, "i.ua": {},
	// Asian
	"qq.com": {}, "163.com": {}, "126.com": {}, "sina.com": {},
	"sina.cn": {}, "sohu.com": {}, "139.com": {},
	"naver.com": {}, "daum.net": {}, "hanmail.net": {},
	// Other common free providers
	"mail.com": {}, "inbox.com": {}, "fastmail.com": {}, "fastmail.fm": {},
	"hushmail.com": {}, "guerrillamail.com": {}, "mailinator.com": {},
	"throwam.com": {}, "sharklasers.com": {}, "guerrillamailblock.com": {},
	"rediffmail.com": {}, "indiatimes.com": {},
}

// hiringKeywords are terms that indicate an email on a page is professionally
// relevant to hiring or recruitment.  Used by ExtractEmailsFromBodyText to
// decide whether a body-text email is worth keeping.
var hiringKeywords = []string{
	"hire", "hiring", "recruiter", "recruitment", "recruiting",
	"talent", "talent acquisition", "talent team",
	" hr ", "hr@", "human resources", "people ops", "people team", "people &",
	"career", "careers", "job", "jobs", "position", "role", "opening",
	"vacancy", "vacancies", "opportunity", "opportunities",
	"apply", "application", "applicant", "candidate",
	"staffing", "headhunt", "interview", "onboard",
	"join our team", "join us", "work with us", "we're hiring",
	"we are hiring", "now hiring", "open role", "open position",
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

// ExtractEmailsFromBodyText is like ExtractEmails but additionally requires
// each candidate address to appear within contextWindow characters of at least
// one hiring-related keyword.  Use this for unstructured page-body text where
// emails unrelated to hiring (team bios, investor lists, support contacts, etc.)
// are common — it dramatically reduces false positives.
//
// Emails found via explicit mailto: links should use ExtractEmails instead,
// since a deliberate link already implies intent.
func ExtractEmailsFromBodyText(text string) []string {
	const contextWindow = 400
	text = html.UnescapeString(text)
	textLow := strings.ToLower(text)
	raw := emailRe.FindAllString(text, -1)
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0)
	for _, e := range raw {
		e = strings.ToLower(strings.TrimSpace(e))
		if seen[e] || !isValid(e) {
			continue
		}
		if !nearHiringKeyword(textLow, e, contextWindow) {
			continue
		}
		seen[e] = true
		out = append(out, e)
	}
	return out
}

// nearHiringKeyword returns true when emailLow appears in textLow and at least
// one hiringKeyword is found within window characters on either side of it.
func nearHiringKeyword(textLow, emailLow string, window int) bool {
	idx := strings.Index(textLow, emailLow)
	if idx < 0 {
		return false
	}
	start := idx - window
	if start < 0 {
		start = 0
	}
	end := idx + len(emailLow) + window
	if end > len(textLow) {
		end = len(textLow)
	}
	ctx := textLow[start:end]
	for _, kw := range hiringKeywords {
		if strings.Contains(ctx, kw) {
			return true
		}
	}
	return false
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
	domain := strings.ToLower(email[at+1:])
	// Reject bare TLD-only domains (e.g. x@com).
	if !strings.Contains(domain, ".") {
		return false
	}
	// Reject consumer / free-email provider domains before any network call.
	if _, blocked := personalDomains[domain]; blocked {
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
	if !result.Syntax.Valid || !result.HasMxRecords || result.Disposable || result.RoleAccount || result.Free {
		return false
	}
	// When SMTP is active, reject addresses the server explicitly rejects
	// (deliverable=false AND not catch-all).  Treat timeouts/unknowns as passing.
	if smtpOn && result.SMTP != nil && !result.SMTP.Deliverable && !result.SMTP.CatchAll {
		return false
	}
	return true
}
