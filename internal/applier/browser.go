package applier

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tebeka/selenium"
	"github.com/tebeka/selenium/firefox"
)

// FillFlags controls form-fill behaviour for a single application.
type FillFlags struct {
	DryRun     bool // fill fields but do not click Submit
	Screenshot bool // save a PNG after filling
	Headful    bool // browser is visible — pause on dry-run so user can inspect form
	Hold       bool // keep window open until user closes it, then move to next URL
}

// Browser manages a single geckodriver process and spawns one Firefox
// WebDriver session per application.  Multiple goroutines may call
// FillApplication concurrently; each gets its own independent session.
type Browser struct {
	service   *selenium.Service
	baseURL   string
	caps      selenium.Capabilities
	sessionMu sync.Mutex // serialises session creation; geckodriver queues internally but concurrent POSTs can race
}

// NewBrowser locates geckodriver in PATH, starts it on port 4444, and
// configures Firefox capabilities.  Returns a clear error message with
// install instructions when geckodriver is not found.
func NewBrowser(headful bool) (*Browser, error) {
	geckodriverPath, err := exec.LookPath("geckodriver")
	if err != nil {
		return nil, fmt.Errorf(
			"geckodriver not found in PATH\n" +
				"Install it for your platform:\n" +
				"  Ubuntu/Debian : sudo apt install firefox-geckodriver\n" +
				"  Fedora/RHEL   : sudo dnf install geckodriver\n" +
				"  Arch Linux    : sudo pacman -S geckodriver\n" +
				"  macOS         : brew install geckodriver\n" +
				"  Manual        : https://github.com/mozilla/geckodriver/releases",
		)
	}

	const port = 4444
	svc, err := selenium.NewGeckoDriverService(geckodriverPath, port)
	if err != nil {
		return nil, fmt.Errorf("start geckodriver on :%d: %w\n(is port %d already in use?)", port, err, port)
	}

	ffCaps := firefox.Capabilities{
		// Hide the webdriver flag and automation extension from JavaScript so
		// Cloudflare's fingerprint checks are less likely to flag the session.
		Prefs: map[string]interface{}{
			"dom.webdriver.enabled":  false,
			"useAutomationExtension": false,
		},
	}
	if !headful {
		ffCaps.Args = []string{"-headless"}
	}
	caps := selenium.Capabilities{"browserName": "firefox"}
	caps.AddFirefox(ffCaps)

	// geckodriver speaks W3C WebDriver directly on /  — NOT the old /wd/hub path
	// used by Selenium standalone server.
	return &Browser{
		service: svc,
		baseURL: fmt.Sprintf("http://localhost:%d", port),
		caps:    caps,
	}, nil
}

// Close stops the geckodriver process.
func (b *Browser) Close() {
	_ = b.service.Stop()
}

// FillApplication opens a fresh Firefox session, navigates to job.URL,
// fills in the applicant details and uploads the resume PDF, then
// optionally clicks Submit.  The session is always closed when done.
func (b *Browser) FillApplication(
	ctx context.Context,
	job *JobInfo,
	info ApplicantInfo,
	resumePath string,
	flags FillFlags,
) (retErr error) {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	b.sessionMu.Lock()
	wd, err := selenium.NewRemote(b.caps, b.baseURL)
	b.sessionMu.Unlock()
	if err != nil {
		return fmt.Errorf("open Firefox session: %w", err)
	}
	// In headful/hold mode keep the browser visible long enough to be useful
	// when an error occurs — otherwise defer wd.Quit() closes it instantly.
	defer func() {
		if retErr != nil {
			if flags.Hold {
				log.Printf("[hold] error — close the Firefox window to continue: %v", retErr)
				waitForWindowClose(ctx, wd)
				retErr = nil
			} else if flags.Headful {
				log.Printf("[apply] headful error — keeping browser open 10 s: %v", retErr)
				time.Sleep(10 * time.Second)
			}
		}
		_ = wd.Quit()
	}()

	_ = wd.SetPageLoadTimeout(30 * time.Second)
	// Zero implicit wait so FindElements returns immediately for absent fields
	// instead of blocking for several seconds on every miss.
	_ = wd.SetImplicitWaitTimeout(0)

	if err := wd.Get(job.URL); err != nil {
		return fmt.Errorf("navigate to %s: %w", job.URL, err)
	}
	// Give JavaScript-heavy ATS pages time to finish rendering.
	time.Sleep(2 * time.Second)

	// Dismiss any bot-protection challenge before trying to interact with the form.
	// In headful mode this blocks until the human solves it if auto-resolution fails.
	if err := waitForChallenge(wd, flags.Headful); err != nil {
		log.Printf("[captcha] %v — proceeding anyway", err)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Dismiss any cookie consent banner before interacting with the page.
	// Cookie overlays block clicks on forms and Apply buttons; we accept them
	// unconditionally since we do not need to customise cookie preferences.
	dismissCookieBanner(wd)

	// Many job-description pages show the posting first and only reveal the
	// application form after the visitor clicks "Apply to this Job" or similar.
	// Detect this pattern and click through before attempting to fill.
	clickPreApplyIfNeeded(ctx, wd)

	// Verify a real application form is present.  Primary check uses
	// appFormSelector (email / name inputs) because formReadySelector is too
	// broad: a search bar or cookie banner already in the DOM satisfies it even
	// when the actual form modal hasn't rendered yet.
	// Fall back to formReadySelector only for unusual forms without email/name
	// inputs (e.g. resume-only uploads, cover-letter-only pages).
	var formEls []selenium.WebElement
	if formEls, _ = wd.FindElements(selenium.ByCSSSelector, appFormSelector); len(formEls) == 0 {
		waitForElement(ctx, wd, appFormSelector, 8*time.Second)
		formEls, _ = wd.FindElements(selenium.ByCSSSelector, appFormSelector)
	}
	if len(formEls) == 0 {
		// appFormSelector didn't match — last resort: check for any text input
		// so unusual ATS forms (no email/name field) are not incorrectly rejected.
		formEls, _ = wd.FindElements(selenium.ByCSSSelector, formReadySelector)
	}
	if len(formEls) == 0 {
		// Only run phrase/status detection when there is no form — this avoids
		// false positives on pages that have a "no longer available" notice in
		// a footer or sidebar while still showing a live application form.
		if err := detectDeadPage(wd); err != nil {
			return err
		}
		return fmt.Errorf("no application form found on page (job may be closed or removed)")
	}

	switch job.ATSPlatform {
	case "greenhouse":
		fillGreenhouse(ctx, wd, info, resumePath)
	case "lever":
		fillLever(ctx, wd, info, resumePath)
	case "ashby":
		fillAshby(ctx, wd, info, resumePath)
	case "bamboohr":
		fillBambooHR(ctx, wd, info, resumePath)
	default:
		fillGeneric(ctx, wd, info, resumePath)
	}

	if flags.Screenshot {
		if data, serr := wd.Screenshot(); serr == nil {
			name := "screenshot_" + safeFilename(job.Company+"_"+job.Title) + ".png"
			_ = os.WriteFile(name, data, 0o644)
		}
	}

	// Hold mode: keep the window open until the user closes it.
	// Takes priority over everything else — no auto-submit.
	if flags.Hold {
		waitForWindowClose(ctx, wd)
		return nil
	}

	if flags.DryRun {
		// In headful dry-run mode keep the window open long enough to inspect.
		if flags.Headful {
			log.Printf("[apply] dry-run: form filled — window stays open for 10 s")
			time.Sleep(10 * time.Second)
		}
		return nil
	}
	if err := clickSubmit(wd); err != nil {
		return err
	}
	return verifySubmission(ctx, wd, flags.Headful || flags.Hold)
}

// waitForWindowClose blocks until the user closes the Firefox window or ctx is
// cancelled.  It polls window handles every 500 ms; when geckodriver reports no
// open windows (or returns an error because the session ended), the function
// returns.
func waitForWindowClose(ctx context.Context, wd selenium.WebDriver) {
	fmt.Println("[hold] Form filled — close the Firefox window to move to the next URL")
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		handles, err := wd.WindowHandles()
		if err != nil || len(handles) == 0 {
			log.Printf("[hold] window closed — proceeding to next URL")
			return
		}
	}
}

// ── ATS-specific form fillers ─────────────────────────────────────────────────

func fillGreenhouse(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string) {
	waitForElement(ctx, wd, "#first_name", 8*time.Second)
	first, last := splitName(info.Name)
	if !trySetInput(wd, "#first_name", first) {
		tryFillByLabel(wd, "first name", first)
	}
	if !trySetInput(wd, "#last_name", last) {
		tryFillByLabel(wd, "last name", last)
	}
	if !trySetInput(wd, "#email", info.Email) {
		tryFillByLabel(wd, "email", info.Email)
	}
	if !trySetInput(wd, "#phone", info.Phone) {
		tryFillByLabel(wd, "phone", info.Phone)
	}
	if !trySetInput(wd, `input[name*="linkedin" i]`, info.LinkedInURL) {
		tryFillByLabel(wd, "linkedin", info.LinkedInURL)
	}
	uploadResume(wd, resumePath)
}

func fillLever(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string) {
	waitForElement(ctx, wd, `input[name="name"]`, 8*time.Second)
	first, last := splitName(info.Name)
	if !trySetInput(wd, `input[name="name"]`, info.Name) {
		tryFillByLabel(wd, "full name", info.Name)
	}
	if !trySetInput(wd, `input[placeholder*="First" i]`, first) {
		tryFillByLabel(wd, "first name", first)
	}
	if !trySetInput(wd, `input[placeholder*="Last" i]`, last) {
		tryFillByLabel(wd, "last name", last)
	}
	if !trySetInput(wd, `input[name="email"]`, info.Email) {
		tryFillByLabel(wd, "email", info.Email)
	}
	if !trySetInput(wd, `input[name="phone"]`, info.Phone) {
		tryFillByLabel(wd, "phone", info.Phone)
	}
	if !trySetInput(wd, `input[name="urls[LinkedIn]"]`, info.LinkedInURL) {
		tryFillByLabel(wd, "linkedin", info.LinkedInURL)
	}
	uploadResume(wd, resumePath)
}

func fillAshby(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string) {
	// Wait for the name or email field — whichever appears first.
	// Using a compound selector so we're not blocked on a single ID/name variant.
	waitForElement(ctx, wd,
		`input[name="name"], input[autocomplete="name"], input[type="email"], input[autocomplete="email"]`,
		12*time.Second)

	// Name — try every known Ashby variant before falling back to label text.
	if !trySetInput(wd, `input[name="name"]`, info.Name) &&
		!trySetInput(wd, `input[autocomplete="name"]`, info.Name) &&
		!trySetInput(wd, `input[placeholder*="name" i]`, info.Name) {
		if !tryFillByLabel(wd, "full name", info.Name) {
			tryFillByLabel(wd, "name", info.Name)
		}
	}
	// Email
	if !trySetInput(wd, `input[name="email"]`, info.Email) &&
		!trySetInput(wd, `input[type="email"]`, info.Email) &&
		!trySetInput(wd, `input[autocomplete="email"]`, info.Email) {
		tryFillByLabel(wd, "email", info.Email)
	}
	// Phone
	if !trySetInput(wd, `input[name="phone"]`, info.Phone) &&
		!trySetInput(wd, `input[type="tel"]`, info.Phone) &&
		!trySetInput(wd, `input[autocomplete="tel"]`, info.Phone) {
		tryFillByLabel(wd, "phone", info.Phone)
	}
	// LinkedIn
	if !trySetInput(wd, `input[placeholder*="LinkedIn" i]`, info.LinkedInURL) &&
		!trySetInput(wd, `input[name*="linkedin" i]`, info.LinkedInURL) {
		tryFillByLabel(wd, "linkedin", info.LinkedInURL)
	}
	uploadResume(wd, resumePath)
}

func fillBambooHR(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string) {
	waitForElement(ctx, wd,
		`input[id*="firstName" i], input[name*="first" i], input[type="email"]`,
		8*time.Second)
	first, last := splitName(info.Name)
	if !trySetInput(wd, `input[id*="firstName" i]`, first) &&
		!trySetInput(wd, `input[name*="first" i]`, first) &&
		!trySetInput(wd, `input[placeholder*="first" i]`, first) {
		tryFillByLabel(wd, "first name", first)
	}
	if !trySetInput(wd, `input[id*="lastName" i]`, last) &&
		!trySetInput(wd, `input[name*="last" i]`, last) &&
		!trySetInput(wd, `input[placeholder*="last" i]`, last) {
		tryFillByLabel(wd, "last name", last)
	}
	if !trySetInput(wd, `input[id*="email" i]`, info.Email) &&
		!trySetInput(wd, `input[type="email"]`, info.Email) &&
		!trySetInput(wd, `input[name*="email" i]`, info.Email) {
		tryFillByLabel(wd, "email", info.Email)
	}
	if !trySetInput(wd, `input[id*="phone" i]`, info.Phone) &&
		!trySetInput(wd, `input[type="tel"]`, info.Phone) &&
		!trySetInput(wd, `input[name*="phone" i]`, info.Phone) {
		tryFillByLabel(wd, "phone", info.Phone)
	}
	uploadResume(wd, resumePath)
}

func fillGeneric(ctx context.Context, wd selenium.WebDriver, info ApplicantInfo, resumePath string) {
	waitForElement(ctx, wd, `input[type="text"], input[type="email"], input[name]`, 8*time.Second)
	first, last := splitName(info.Name)

	// For each field: try attribute-based selectors first, then label-based
	// fallback for React/Vue forms that expose no standard name/id attributes.
	filled := false
	for _, sel := range []string{
		`input[name*="first" i]`, `input[placeholder*="First name" i]`,
		`input[id*="first" i]`, `input[aria-label*="First" i]`,
	} {
		if trySetInput(wd, sel, first) {
			filled = true
			break
		}
	}
	if !filled {
		if !tryFillByLabel(wd, "first name", first) {
			tryFillByLabel(wd, "given name", first)
		}
	}

	filled = false
	for _, sel := range []string{
		`input[name*="last" i]`, `input[placeholder*="Last name" i]`,
		`input[id*="last" i]`, `input[aria-label*="Last" i]`,
	} {
		if trySetInput(wd, sel, last) {
			filled = true
			break
		}
	}
	if !filled {
		if !tryFillByLabel(wd, "last name", last) {
			tryFillByLabel(wd, "family name", last)
		}
	}

	filled = false
	for _, sel := range []string{
		`input[name="name"]`, `input[placeholder*="Full name" i]`,
		`input[placeholder*="Your name" i]`,
	} {
		if trySetInput(wd, sel, info.Name) {
			filled = true
			break
		}
	}
	if !filled {
		if !tryFillByLabel(wd, "full name", info.Name) {
			tryFillByLabel(wd, "your name", info.Name)
		}
	}

	filled = false
	for _, sel := range []string{
		`input[type="email"]`, `input[name*="email" i]`,
		`input[placeholder*="email" i]`, `input[id*="email" i]`,
	} {
		if trySetInput(wd, sel, info.Email) {
			filled = true
			break
		}
	}
	if !filled {
		tryFillByLabel(wd, "email", info.Email)
	}

	filled = false
	for _, sel := range []string{
		`input[type="tel"]`, `input[name*="phone" i]`,
		`input[placeholder*="phone" i]`, `input[id*="phone" i]`,
	} {
		if trySetInput(wd, sel, info.Phone) {
			filled = true
			break
		}
	}
	if !filled {
		if !tryFillByLabel(wd, "phone", info.Phone) {
			tryFillByLabel(wd, "mobile", info.Phone)
		}
	}

	filled = false
	for _, sel := range []string{
		`input[placeholder*="LinkedIn" i]`, `input[name*="linkedin" i]`,
		`input[id*="linkedin" i]`, `input[aria-label*="LinkedIn" i]`,
	} {
		if trySetInput(wd, sel, info.LinkedInURL) {
			filled = true
			break
		}
	}
	if !filled {
		tryFillByLabel(wd, "linkedin", info.LinkedInURL)
	}

	uploadResume(wd, resumePath)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// jsAcceptCookies dismisses cookie consent banners in two passes:
//  1. Exact CSS selectors for well-known consent libraries (Cookiebot, OneTrust,
//     Osano, Didomi, TrustArc, Cookie Script, Usercentrics, Funding Choices, …).
//  2. Text-based matching inside any element whose class/id contains "cookie",
//     "consent", "gdpr", "banner", "notice", or "cc-".  Accept-words cover
//     English, German, Dutch, Finnish, Norwegian, Swedish, French, and Danish.
//     Buttons whose text also contains a reject-word are skipped.
//
// Returns true when a button was clicked so the Go side can wait briefly for
// the overlay animation to finish before continuing.
const jsAcceptCookies = `
(function() {
    // Pass 1: library-specific selectors.
    var exact = [
        '#CybotCookiebotDialogBodyLevelButtonLevelOptinAllowAll',
        '#CybotCookiebotDialogBodyButtonAccept',
        '#onetrust-accept-btn-handler',
        '.onetrust-accept-btn-handler',
        '.osano-cm-accept-all', '.osano-cm-accept',
        '#didomi-notice-agree-button',
        '#truste-consent-button',
        '#cookiescript_accept_all', '#cookiescript_accept',
        'button[data-testid="uc-accept-all-button"]',
        '.fc-button.fc-cta-consent',
        '#coiConsentBannerFooterButton',
        '[data-cookiebanner="accept_button"]',
        '#accept-cookies', '#cookie-accept', '#acceptCookies',
        '#accept-all-cookies', '#acceptAllCookies',
        '.js-cookie-accept', '.js-accept-cookies',
        'button[class*="cookie-consent-accept" i]',
        'button[class*="accept-cookie" i]',
        'button[class*="accept-all-cookie" i]',
        'button[aria-label*="accept all" i]',
        'button[aria-label*="alle akzeptieren" i]',
        'button[aria-label*="acceptera alla" i]',
    ];
    for (var s = 0; s < exact.length; s++) {
        try {
            var el = document.querySelector(exact[s]);
            if (el && !el.disabled) { el.click(); return true; }
        } catch(e) {}
    }

    // Pass 2: multilingual text matching inside consent containers.
    var ACCEPT = [
        // English
        'accept', 'allow all', 'allow cookies', 'agree', 'i agree',
        'i accept', 'ok', 'got it',
        // German
        'akzeptieren', 'zustimmen', 'einverstanden', 'annehmen', 'ich stimme',
        // Dutch
        'accepteren', 'akkoord', 'toestaan', 'instemmen',
        // Finnish
        'hyväksy',
        // Norwegian
        'godta', 'aksepter',
        // Swedish
        'acceptera', 'godkänn',
        // French
        "j'accepte", 'accepter',
        // Danish
        'godkend',
    ];
    var REJECT = [
        'decline', 'reject', 'refuse', 'deny',
        'necessary only', 'only necessary', 'essential only',
        'ablehnen', 'nein', 'nur notwendige', 'nur erforderliche',
        'weigeren', 'alleen noodzakelijke',
        'avvisa', 'avvis', 'neka',
        'hylkää',
    ];
    function textOf(el) {
        return (el.innerText || el.value || el.getAttribute('aria-label') || el.title || '').trim().toLowerCase();
    }
    function hasWord(t, list) {
        for (var i = 0; i < list.length; i++) if (t.indexOf(list[i]) !== -1) return true;
        return false;
    }
    var containerSel = [
        '[class*="cookie" i]', '[id*="cookie" i]',
        '[class*="consent" i]', '[id*="consent" i]',
        '[class*="gdpr" i]', '[id*="gdpr" i]',
        '[class*="banner" i]', '[class*="notice" i]',
        '[class*="cc-"]', '[id*="cc-"]',
    ].join(',');
    var containers = document.querySelectorAll(containerSel);
    for (var c = 0; c < containers.length; c++) {
        var btns = containers[c].querySelectorAll('button, [role="button"], a');
        for (var b = 0; b < btns.length; b++) {
            var btn = btns[b];
            if (btn.disabled) continue;
            var t = textOf(btn);
            if (t.length === 0 || t.length > 60) continue;
            if (hasWord(t, REJECT)) continue;
            if (hasWord(t, ACCEPT)) { btn.click(); return true; }
        }
    }
    return false;
})();
`

// dismissCookieBanner attempts to accept any cookie consent banner on the
// current page.  It loops up to three times with a short pause between
// iterations to handle two-step banners ("Manage preferences" → "Accept all").
// The loop stops as soon as no banner button is found.
func dismissCookieBanner(wd selenium.WebDriver) {
	for i := 0; i < 3; i++ {
		res, err := wd.ExecuteScript(jsAcceptCookies, nil)
		if err != nil || res != true {
			return
		}
		time.Sleep(400 * time.Millisecond) // wait for the overlay animation
	}
}

// deadPagePhrases is a normalised list of substrings that appear in page
// titles, headings, or body text when a job posting is closed, filled,
// expired, or the URL 404s.  Ordered longest-first so the most specific
// phrase surfaces in error messages and shorter sub-strings don't shadow them.
var deadPagePhrases = []string{
	// Ashby-specific
	"this role is not currently open for applications",
	"this position is not currently open for applications",
	"not currently open for applications",
	"role is not currently open",
	"position is not currently open",
	"not currently accepting applications",
	"currently not accepting applications",
	"not open for applications",
	"applications are currently closed",
	"applications are closed",
	// Lever / Greenhouse
	"this job is no longer available",
	"this position is no longer available",
	"this role is no longer available",
	"this listing is no longer available",
	"this requisition is no longer available",
	"position has been filled",
	"this job has been filled",
	"this role has been filled",
	"this position has been filled",
	"job is no longer accepting",
	"no longer accepting applications",
	"this listing is no longer active",
	"this requisition is no longer active",
	"this posting is no longer active",
	// Expiry / removal
	"job posting has expired",
	"this job has expired",
	"this posting has expired",
	"this position has been removed",
	"this job has been removed",
	// Generic closed
	"this job has been closed",
	"this position is closed",
	"this job is closed",
	"job closed",
	"no longer available",
	// Generic 404 / error
	"page not found",
	"404 not found",
	"page doesn't exist",
	"page does not exist",
	"this page no longer exists",
	"we couldn't find that page",
}

// jsDeadPage collects signals used by detectDeadPage in a single round-trip:
//   - HTTP response status (Navigation Timing API, Firefox 125+; 0 on older browsers)
//   - Lowercased page title
//   - Text from headings, prominent status/alert elements, paragraphs inside
//     <main> or <article>, and the first 2 000 chars of body text
//
// The 2 000-char window (up from 600) is important for React SPAs like Ashby
// where the "job closed" message is rendered deeper in the component tree.
const jsDeadPage = `
try {
    var status = 0;
    try {
        var nav = performance.getEntriesByType('navigation')[0];
        if (nav && nav.responseStatus) status = nav.responseStatus;
    } catch(e) {}

    var title = (document.title || '').toLowerCase();

    var parts = [];
    var selectors = [
        'h1','h2','h3','h4',
        '[role="alert"]','[role="status"]','[aria-live]',
        'main p','article p',
        '[class*="expired" i]','[class*="closed" i]',
        '[class*="not-found" i]','[class*="unavailable" i]',
        '[class*="empty-state" i]','[class*="empty_state" i]',
        '[id*="expired" i]','[id*="not-found" i]'
    ];
    selectors.forEach(function(s) {
        try {
            document.querySelectorAll(s).forEach(function(el) {
                var t = el.innerText || '';
                if (t.trim()) parts.push(t);
            });
        } catch(e) {}
    });
    parts.push((document.body && document.body.innerText || '').slice(0, 2000));
    var content = parts.join(' ').toLowerCase();

    return {status: status, title: title, content: content};
} catch(e) { return {status: 0, title: '', content: ''}; }
`

// detectDeadPage checks the current page for signs that a job posting no
// longer exists (HTTP 4xx/5xx, URL redirect to an error path, "job closed"
// headings, or known phrase patterns in the visible text).  Returns a
// descriptive error so the caller can skip this URL without wasting time on
// form-filling.
func detectDeadPage(wd selenium.WebDriver) error {
	// Fast URL check — some platforms redirect to /404, /not-found, /gone, etc.
	if u, err := wd.CurrentURL(); err == nil {
		uLow := strings.ToLower(u)
		for _, seg := range []string{"/404", "/not-found", "/error", "/gone", "/expired", "/closed"} {
			if strings.Contains(uLow, seg) {
				return fmt.Errorf("page redirected to error URL: %s", u)
			}
		}
	}

	// JS check: status + title + prominent-heading/body text in one round-trip.
	raw, err := wd.ExecuteScript(jsDeadPage, nil)
	if err != nil {
		return nil // can't tell — let the fill attempt proceed
	}
	data, ok := raw.(map[string]interface{})
	if !ok {
		return nil
	}

	// HTTP status (available on Firefox 125+; 0 on older browsers).
	if code, ok := data["status"].(float64); ok && code >= 400 {
		return fmt.Errorf("HTTP %.0f response", code)
	}

	title, _ := data["title"].(string)
	content, _ := data["content"].(string)
	combined := title + " " + content

	for _, phrase := range deadPagePhrases {
		if strings.Contains(combined, phrase) {
			return fmt.Errorf("job posting unavailable (%q detected on page)", phrase)
		}
	}

	return nil
}

// formReadySelector matches any visible text/email/tel input or textarea.
// Used as the post-fill-click wait target (broad enough to catch any activity).
const formReadySelector = `input[type="text"], input[type="email"], input[type="tel"], textarea`

// appFormSelector is tighter than formReadySelector: it only matches inputs
// that are characteristic of job application forms (email, first/last name).
// Used to decide whether an application form is present before trying to click
// an "Apply" button, so that search bars and cookie-consent inputs don't
// trigger a false "form already loaded" early exit.
// Also used as the post-click wait target so that a search bar already in the
// DOM (when Apply opens a modal overlay on the same page) doesn't cause the
// wait to return before the actual form fields have rendered.
const appFormSelector = `input[type="email"],` +
	`input[name*="email" i],` +
	`input[autocomplete="email"],` +
	`input[name*="first" i],` +
	`input[name*="last" i],` +
	`input[name="name"],` +
	`input[autocomplete="name"],` +
	`input[id*="email" i],` +
	`input[id*="firstname" i],` +
	`input[id*="first_name" i]`

// clickPreApplyIfNeeded detects the "job description first, form second"
// pattern: if the page has no form inputs yet it looks for an "Apply" trigger
// button, clicks it, then waits up to 10 s for the form to appear.
// Returns true when it performed a click (regardless of whether the form
// appeared — the caller's waitForElement inside the fill function will handle
// the remaining wait).
func clickPreApplyIfNeeded(ctx context.Context, wd selenium.WebDriver) bool {
	// Fast exit only when application-form inputs are already VISIBLE in the
	// viewport — not merely present in the DOM.  Many ATS platforms (BambooHR,
	// Greenhouse) pre-render the application form below the job description or
	// inside a hidden div; a plain FindElements call would match those hidden
	// inputs and cause us to skip the "Apply Now" button entirely.
	res, _ := wd.ExecuteScript(`
var els = document.querySelectorAll(arguments[0]);
for (var i = 0; i < els.length; i++) {
    var e = els[i];
    if (e.disabled) continue;
    if (e.offsetParent !== null || window.getComputedStyle(e).position === 'fixed') return true;
}
return false;`, []interface{}{appFormSelector})
	if res == true {
		return false
	}

	clicked := false

	// 1. Attribute-based CSS — highest precision, platform-specific IDs/classes.
	// Ordered: ATS-specific first, then generic data attributes.
	for _, sel := range []string{
		// BambooHR
		`a[class*="BambooHR-ATS-Jobs-Apply"]`,
		`[class*="BambooHR-ATS-Jobs-Apply"]`,
		// Greenhouse, Lever, Ashby, generic ATS
		`button[data-qa*="apply" i]`, `a[data-qa*="apply" i]`,
		`button[id*="apply-btn" i]`, `button[id*="btn-apply" i]`,
		`a[id*="apply-btn" i]`, `a[id*="btn-apply" i]`,
		`button[class*="apply-btn" i]`, `a[class*="apply-btn" i]`,
		`a[class*="jobs-apply" i]`, `button[class*="jobs-apply" i]`,
		`[data-automation*="apply" i]`,
		`button[data-testid*="apply" i]`, `a[data-testid*="apply" i]`,
	} {
		els, err := wd.FindElements(selenium.ByCSSSelector, sel)
		if err != nil || len(els) == 0 {
			continue
		}
		// Try every match — the first one in DOM order may be off-screen.
		for _, el := range els {
			if el.Click() == nil {
				clicked = true
				break
			}
		}
		if clicked {
			break
		}
	}

	// 2. XPath text — longest phrases first to avoid matching sub-strings in
	// unrelated elements (e.g. "Browse and apply" matching "apply").
	if !clicked {
		const lc = `translate(normalize-space(.),'ABCDEFGHIJKLMNOPQRSTUVWXYZ','abcdefghijklmnopqrstuvwxyz')`
		for _, phrase := range []string{
			"apply to this job",
			"apply for this position",
			"apply for this role",
			"apply for this job",
			"start your application",
			"start application",
			"begin application",
			"apply with linkedin",
			"apply with resume",
			"quick apply",
			"1-click apply",
			"apply now",
			"easy apply",
		} {
			xpath := fmt.Sprintf(`(//button|//a)[contains(%s,'%s')]`, lc, phrase)
			els, err := wd.FindElements(selenium.ByXPATH, xpath)
			if err != nil || len(els) == 0 {
				continue
			}
			for _, el := range els {
				if el.Click() == nil {
					clicked = true
					break
				}
			}
			if clicked {
				break
			}
		}
		// Bare "apply" only when the entire label is exactly that word, to
		// prevent matching navigation links like "Jobs / Apply" breadcrumbs.
		if !clicked {
			xpath := fmt.Sprintf(`(//button|//a)[%s='apply']`, lc)
			els, err := wd.FindElements(selenium.ByXPATH, xpath)
			if err == nil {
				for _, el := range els {
					if el.Click() == nil {
						clicked = true
						break
					}
				}
			}
		}
	}

	// 3. JS fallback — scrolls each candidate into view before clicking so
	// off-screen buttons (sticky headers, bottom CTAs) are reachable.
	// Excludes nav/footer/[role="navigation"] but NOT <header> — legitimate
	// ATS apply buttons are frequently placed inside sticky page headers.
	if !clicked {
		const jsApply = `
var MULTI = /\b(apply\s+(?:to\s+this\s+(?:job|position|role)|for\s+this\s+(?:job|position|role)|now|with\s+\S+)|easy\s+apply|quick\s+apply|(?:start|begin)\s+(?:your\s+)?application|1[\s-]click\s+apply)\b/i;
var BARE  = /^\s*apply\s*$/i;
var all = Array.from(document.querySelectorAll('button, a[href], [role="button"]'));
for (var i = 0; i < all.length; i++) {
    var el = all[i];
    if (el.disabled || el.offsetParent === null) continue;
    if (el.closest('nav, footer, [role="navigation"]')) continue;
    var t = (el.innerText || el.value || el.getAttribute('aria-label') || '').trim();
    if (MULTI.test(t) || BARE.test(t)) {
        el.scrollIntoView({block: 'center'});
        el.click();
        return true;
    }
}
return false;`
		res, err := wd.ExecuteScript(jsApply, nil)
		if err == nil && res == true {
			clicked = true
		}
	}

	if clicked {
		// Wait for application-specific inputs (email / name fields) rather than
		// any text input.  When Apply opens a modal overlay the underlying page
		// may already have a search bar that would satisfy formReadySelector
		// immediately, before the modal's own fields have rendered.
		waitForElement(ctx, wd, appFormSelector, 12*time.Second)
	}
	return clicked
}

// jsFill scrolls the first element matching a CSS selector into view, then
// sets its value via the native HTMLInputElement/HTMLTextAreaElement prototype
// setter and fires input/change/blur so React, Vue, and Angular frameworks
// pick up the new value — plain SendKeys does not reliably trigger these.
// Returns true when an element was found and filled, false otherwise so the
// Go caller can distinguish "filled" from "no visible element found".
const jsFill = `
var sel = arguments[0], val = arguments[1];
var els = document.querySelectorAll(sel), el = null;
for (var i = 0; i < els.length; i++) {
    var e = els[i];
    if (e.disabled) continue;
    // offsetParent is null for:
    //   a) elements whose ancestor has display:none  — truly hidden, skip
    //   b) elements with position:fixed              — visible, keep
    // Check computed position to distinguish the two.
    if (e.offsetParent !== null || window.getComputedStyle(e).position === 'fixed') {
        el = e; break;
    }
}
if (!el) return false;
el.focus();
el.scrollIntoView({block: 'center'});
try {
    var proto = el.tagName === 'TEXTAREA'
        ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
    var setter = Object.getOwnPropertyDescriptor(proto, 'value').set;
    setter.call(el, val);
} catch (e) { el.value = val; }
el.dispatchEvent(new Event('input',  {bubbles: true}));
el.dispatchEvent(new Event('change', {bubbles: true}));
el.dispatchEvent(new Event('blur',   {bubbles: true}));
return true;
`

// jsFillByLabel finds the first visible input associated with a <label> whose
// text contains the given needle (case-insensitive), fills it with val using
// the same native-setter approach as jsFill, and returns true on success.
// This is the most reliable fallback for React/Vue forms that expose no
// standard name/id/placeholder attributes but do have visible labels.
const jsFillByLabel = `
var needle = arguments[0].toLowerCase(), val = arguments[1];
var labels = document.querySelectorAll('label');
for (var i = 0; i < labels.length; i++) {
    var l = labels[i];
    if (l.textContent.trim().toLowerCase().indexOf(needle) === -1) continue;
    var inp = null;
    // 1. Standard HTML: label[for] → getElementById
    var fid = l.getAttribute('for');
    if (fid) inp = document.getElementById(fid);
    // 2. Input nested inside the label element
    if (!inp) inp = l.querySelector('input:not([type="hidden"]):not([type="checkbox"]):not([type="radio"]), textarea');
    // 3. Sibling pattern (BambooHR, many modern forms): label and input share a
    //    parent container but are not linked via for/id.  Walk following siblings
    //    until we find an input or hit the next label (= next field boundary).
    if (!inp) {
        var sib = l.nextElementSibling;
        while (sib) {
            if (sib.tagName === 'LABEL') break;
            if ((sib.tagName === 'INPUT' || sib.tagName === 'TEXTAREA')
                    && sib.type !== 'hidden' && sib.type !== 'checkbox' && sib.type !== 'radio') {
                inp = sib; break;
            }
            var inner = sib.querySelector('input:not([type="hidden"]):not([type="checkbox"]):not([type="radio"]), textarea');
            if (inner) { inp = inner; break; }
            sib = sib.nextElementSibling;
        }
    }
    if (!inp || inp.disabled) continue;
    // Same visibility logic as jsFill: accept position:fixed elements.
    if (inp.offsetParent === null && window.getComputedStyle(inp).position !== 'fixed') continue;
    inp.focus();
    inp.scrollIntoView({block: 'center'});
    try {
        var proto = inp.tagName === 'TEXTAREA'
            ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
        var setter = Object.getOwnPropertyDescriptor(proto, 'value').set;
        setter.call(inp, val);
    } catch(e) { inp.value = val; }
    inp.dispatchEvent(new Event('input',  {bubbles: true}));
    inp.dispatchEvent(new Event('change', {bubbles: true}));
    inp.dispatchEvent(new Event('blur',   {bubbles: true}));
    return true;
}
return false;
`

// tryFillByLabel fills the input whose label text contains labelText.
// Returns true when an element was found and filled.
func tryFillByLabel(wd selenium.WebDriver, labelText, value string) bool {
	if value == "" {
		return false
	}
	res, err := wd.ExecuteScript(jsFillByLabel, []interface{}{labelText, value})
	return err == nil && res == true
}

// jsSubmit finds and clicks the most likely submit button via JS heuristics,
// returning true when it clicked something.  Used as a last resort after all
// CSS/XPath attempts fail.
const jsSubmit = `
var WORDS = /\b(submit|apply|send|complete|finish|confirm)\b/i;
var all = Array.from(document.querySelectorAll(
    'button[type="submit"], input[type="submit"], button, [role="button"]'
));
for (var i = 0; i < all.length; i++) {
    var el = all[i];
    if (el.disabled || el.offsetParent === null) continue;
    var label = (el.innerText || el.value || el.getAttribute('aria-label') || '').trim();
    if (el.type === 'submit' || WORDS.test(label)) {
        el.scrollIntoView({block: 'center'});
        el.click();
        return true;
    }
}
return false;
`

// waitForElement polls for selector to match at least one element, returning
// the first match or nil after timeout.  Requires implicit wait to be 0 so
// each FindElements call returns immediately.
func waitForElement(ctx context.Context, wd selenium.WebDriver, selector string, timeout time.Duration) selenium.WebElement {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		els, err := wd.FindElements(selenium.ByCSSSelector, selector)
		if err == nil && len(els) > 0 {
			return els[0]
		}
		time.Sleep(300 * time.Millisecond)
	}
	return nil
}

// trySetInput fills the first visible element matching selector using the JS
// native-setter approach (required for React/Vue controlled inputs) and returns
// true only when an element was found AND the value was actually written.
// If jsFill cannot find a usable visible element it returns false so callers
// can continue down the fallback chain rather than silently stopping.
// As a last resort it tries WebDriver SendKeys on every matched element.
func trySetInput(wd selenium.WebDriver, selector, value string) bool {
	if value == "" {
		return false
	}
	els, err := wd.FindElements(selenium.ByCSSSelector, selector)
	if err != nil || len(els) == 0 {
		return false
	}
	res, err := wd.ExecuteScript(jsFill, []interface{}{selector, value})
	if err == nil && res == true {
		return true
	}
	// jsFill found no usable visible element (returned false) or JS errored.
	// Fall back to WebDriver SendKeys on every matched element — works for
	// plain-HTML forms that don't need synthetic events.
	for _, el := range els {
		_ = el.Click()
		_ = el.Clear()
		if err2 := el.SendKeys(value); err2 == nil {
			return true
		}
	}
	return false
}

// uploadSelectors are tried in order by uploadResume.
// Ordered from most specific (known attribute values) to most generic.
var uploadSelectors = []string{
	`input[name="resume"]`,
	`input[name="cv"]`,
	`input[name*="resume" i]`,
	`input[name*="cv" i]`,
	`input[name*="attachment" i]`,
	`input[accept*=".pdf" i]`,
	`input[accept*="pdf" i]`,
	`input[type="file"]`,
}

// jsRevealFileInputs makes every file input in the document reachable via
// SendKeys.  Many ATS platforms hide the real <input type="file"> behind a
// styled drag-drop zone or a custom button — the CSS hiding (display:none,
// opacity:0, pointer-events:none, etc.) prevents geckodriver from injecting
// the file path.  We force all of them into a minimal 1×1 px visible rect
// before attempting the upload, then rely on the browser to handle the rest.
const jsRevealFileInputs = `
var inputs = document.querySelectorAll('input[type="file"]');
for (var i = 0; i < inputs.length; i++) {
    inputs[i].style.cssText = [
        'display:block!important',
        'opacity:1!important',
        'visibility:visible!important',
        'position:fixed!important',
        'left:0px!important',
        'top:0px!important',
        'width:1px!important',
        'height:1px!important',
        'overflow:visible!important',
        'pointer-events:auto!important'
    ].join(';');
}
return inputs.length;
`

// uploadResume tries to upload path to any file input on the current page.
// It first reveals all hidden file inputs (a common ATS pattern where the real
// <input type="file"> is hidden behind a drag-drop zone), then walks through
// uploadSelectors from most specific to most generic, attempting SendKeys on
// every matching element until one succeeds.  Logs the outcome either way.
func uploadResume(wd selenium.WebDriver, path string) bool {
	if path == "" {
		return false
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		log.Printf("[upload] cannot resolve path %q: %v", path, err)
		return false
	}
	if _, err := os.Stat(absPath); err != nil {
		log.Printf("[upload] resume file not found: %q", absPath)
		return false
	}

	// Reveal all hidden file inputs before FindElements so geckodriver can
	// reach them.  Ignore errors — reveal is best-effort.
	if n, err2 := wd.ExecuteScript(jsRevealFileInputs, nil); err2 == nil {
		if cnt, ok := n.(float64); ok && cnt > 0 {
			time.Sleep(150 * time.Millisecond) // let CSS transitions settle
		}
	}

	base := filepath.Base(absPath)
	for _, sel := range uploadSelectors {
		els, err := wd.FindElements(selenium.ByCSSSelector, sel)
		if err != nil || len(els) == 0 {
			continue
		}
		for _, el := range els {
			if err := el.SendKeys(absPath); err != nil {
				log.Printf("[upload] SendKeys failed on %q: %v", sel, err)
				continue
			}
			log.Printf("[upload] %q uploaded via selector %q", base, sel)
			return true
		}
	}

	log.Printf("[upload] WARNING: no file input found on page — resume not uploaded")
	return false
}

// jsVerifySubmission checks whether the page shows signs of a successful
// submission or a validation error, collecting both in one round-trip.
const jsVerifySubmission = `
try {
    var txt   = ((document.body && document.body.innerText) || '').toLowerCase();
    var title = (document.title || '').toLowerCase();
    var combined = title + ' ' + txt.slice(0, 3000);

    var ok = [
        'thank you for applying','thanks for applying',
        'application received','application submitted',
        'successfully submitted','successfully applied',
        'we will review','we’ll be in touch',"we'll be in touch",
        'you have applied','application complete',
        'your application has been','application was submitted',
        'submission confirmed','we received your application',
        'your application is complete','application is under review'
    ];
    for (var i = 0; i < ok.length; i++) {
        if (combined.indexOf(ok[i]) !== -1) return {success: true, phrase: ok[i]};
    }

    // Visible validation-error elements — indicates the form is still open with errors.
    var errSels = '[class*="error" i]:not([class*="error-page" i]),[class*="invalid" i],[aria-invalid="true"],[data-error],.field-error,.form-error';
    var errEls  = document.querySelectorAll(errSels);
    for (var j = 0; j < errEls.length; j++) {
        var t = (errEls[j].innerText || '').trim();
        if (t && t.length < 300) return {success: false, error: t};
    }
    return {success: false};
} catch(e) { return {success: false}; }
`

// successURLSegs are path / query segments that indicate a thank-you or
// confirmation page after an ATS form submission.
var successURLSegs = []string{
	"thank", "thanks", "success", "confirm", "submitted",
	"complete", "done", "received", "applied",
}

// verifySubmission waits up to 10 s after submit for a confirmation signal:
// a URL redirect to a thank-you page, a success phrase in the page text, or
// absence of validation errors.  On headful mode it keeps the window open an
// extra 15 s when a form error is detected so the user can see what went wrong.
// Returns nil when the submission appears successful or when no signal can be
// detected (some ATS platforms give no visible feedback).
func verifySubmission(ctx context.Context, wd selenium.WebDriver, headful bool) error {
	originalURL, _ := wd.CurrentURL()
	deadline := time.Now().Add(10 * time.Second)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}

		// URL change is the strongest signal — most ATS platforms redirect to a
		// thank-you or confirmation page after successful submission.
		if cur, _ := wd.CurrentURL(); cur != originalURL {
			low := strings.ToLower(cur)
			for _, seg := range successURLSegs {
				if strings.Contains(low, seg) {
					log.Printf("[apply] submission confirmed via redirect: %s", cur)
					return nil
				}
			}
			// Any navigation away from the form page counts as acceptance.
			log.Printf("[apply] form navigated → %s (treating as submitted)", cur)
			return nil
		}

		// Page-content check: success phrase or validation error.
		raw, err := wd.ExecuteScript(jsVerifySubmission, nil)
		if err != nil {
			continue
		}
		data, _ := raw.(map[string]interface{})
		if data["success"] == true {
			phrase, _ := data["phrase"].(string)
			log.Printf("[apply] submission confirmed (%q)", phrase)
			return nil
		}
		if errMsg, _ := data["error"].(string); errMsg != "" {
			if headful {
				log.Printf("[apply] form validation error — keeping window open 15 s: %s", errMsg)
				time.Sleep(15 * time.Second)
			}
			return fmt.Errorf("form validation failed: %s", errMsg)
		}
	}

	// No signal in 10 s — warn and proceed; many ATS platforms are silent.
	log.Printf("[apply] warning: no submission confirmation signal detected")
	return nil
}

// submitButtonTexts covers the button labels used across ATS platforms.
// Ordered from most specific to most generic to avoid false positives.
var submitButtonTexts = []string{
	"submit application", "submit my application",
	"complete application", "complete my application",
	"send application", "send my application",
	"apply now", "apply for this job", "apply for this position",
	"submit", "apply", "send", "complete", "finish", "confirm",
}

// clickSubmit tries CSS attribute selectors, XPath text search across all
// common ATS button labels, and finally a JS heuristic as a last resort.
func clickSubmit(wd selenium.WebDriver) error {
	// 1. Attribute-based CSS — most reliable; not text-dependent.
	for _, sel := range []string{
		`button[type="submit"]`,
		`input[type="submit"]`,
		`button[data-qa*="submit" i]`,
		`button[data-testid*="submit" i]`,
		`button[data-testid*="apply" i]`,
		`button[aria-label*="submit" i]`,
		`button[aria-label*="apply" i]`,
		`[role="button"][aria-label*="submit" i]`,
		`[role="button"][aria-label*="apply" i]`,
	} {
		els, err := wd.FindElements(selenium.ByCSSSelector, sel)
		if err == nil && len(els) > 0 {
			if cerr := els[0].Click(); cerr == nil {
				time.Sleep(2 * time.Second)
				return nil
			}
		}
	}

	// 2. XPath text search — covers buttons labelled with human-readable text.
	const lowerXPath = `translate(normalize-space(.),'ABCDEFGHIJKLMNOPQRSTUVWXYZ','abcdefghijklmnopqrstuvwxyz')`
	for _, phrase := range submitButtonTexts {
		xpath := fmt.Sprintf(`//button[contains(%s,'%s')] | //a[contains(%s,'%s')]`,
			lowerXPath, phrase, lowerXPath, phrase)
		els, err := wd.FindElements(selenium.ByXPATH, xpath)
		if err == nil && len(els) > 0 {
			if cerr := els[0].Click(); cerr == nil {
				time.Sleep(2 * time.Second)
				return nil
			}
		}
	}

	// 3. JS heuristic — last resort for non-standard markup.
	res, err := wd.ExecuteScript(jsSubmit, nil)
	if err == nil && res == true {
		time.Sleep(2 * time.Second)
		return nil
	}

	return fmt.Errorf("no submit button found on page")
}

func splitName(full string) (first, last string) {
	parts := strings.SplitN(strings.TrimSpace(full), " ", 2)
	first = parts[0]
	if len(parts) > 1 {
		last = parts[1]
	}
	return
}

func safeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}
